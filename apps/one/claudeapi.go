package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"lab/apps/one/repo"
	"lab/platform/appkit"
)

// The Claude API (SPEC 31): the one place the app calls out to a model. The
// address is fixed here, https, and a redirect is never followed, so a call
// can only ever reach api.anthropic.com. The key is kept in
// $DATA_DIR/secrets/claude.json with the other secrets; it is never shown
// again, never logged and never part of an error text. Nothing here reads a
// message: callers hand over what the model should see.

const (
	claudeHost       = "api.anthropic.com"
	claudeURL        = "https://" + claudeHost + "/v1/messages"
	claudeVersion    = "2023-06-01"
	claudeSecretFile = "claude.json"
	maxClaudeAnswer  = 4 << 20

	modelHaiku  = "claude-haiku-5-5"
	modelSonnet = "claude-sonnet-5-5"
	modelOpus   = "claude-opus-5-5"

	keyReviewModel = "claude.review_model"
	keyDailyLimit  = "claude.daily_limit"
	keyNightAt     = "claude.night_at"
	keyNightModel  = "claude.night_model"
	keyNightLimit  = "claude.night_limit"
	keyPrices      = "claude.prices"

	defDailyLimit = 2.0
	defNightLimit = 1.0
	defNightAt    = "03:00"
)

var knownModels = []string{modelHaiku, modelSonnet, modelOpus}

// --- the key ---

type claudeSecret struct {
	Key string `json:"key"`
}

func (a *app) claudeKey() (string, bool) {
	var s claudeSecret
	if !readSecret(a.secretsDir, claudeSecretFile, &s) || s.Key == "" {
		return "", false
	}
	return s.Key, true
}

func (a *app) claudeConfigured() bool {
	_, ok := a.claudeKey()
	return ok
}

// --- the request and the answer, as the Messages API takes and gives them ---

type claudeMsg struct {
	Role    string `json:"role"`
	Content any    `json:"content"`
}

type claudeToolDef struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"input_schema"`
}

type claudeRequest struct {
	Model      string          `json:"model"`
	MaxTokens  int             `json:"max_tokens"`
	System     string          `json:"system,omitempty"`
	Messages   []claudeMsg     `json:"messages"`
	Tools      []claudeToolDef `json:"tools,omitempty"`
	ToolChoice map[string]any  `json:"tool_choice,omitempty"`
}

// claudeBlock is one block of an answer: text, or a tool use.
type claudeBlock struct {
	Type  string          `json:"type"`
	Text  string          `json:"text,omitempty"`
	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`
}

// claudeResult is a tool's answer going back to the model.
type claudeResult struct {
	Type      string `json:"type"`
	ToolUseID string `json:"tool_use_id"`
	Content   string `json:"content"`
	IsError   bool   `json:"is_error,omitempty"`
}

type claudeAnswer struct {
	Content    []claudeBlock `json:"content"`
	StopReason string        `json:"stop_reason"`
	Usage      struct {
		In  int `json:"input_tokens"`
		Out int `json:"output_tokens"`
	} `json:"usage"`
}

// claudeErr says why a call did not give an answer. Down covers every case in
// which nothing the app does to the item would help: the API cannot be
// reached, is overloaded, or refuses the key. Bad is a request the API did not
// accept for this item.
type claudeErr struct {
	limit     bool // the day's limit was used up in the middle of a conversation
	Down      bool
	Refused   bool // the key was refused
	Stopped   bool // the app is shutting down: the call was cut off, which is no outage
	Unreached bool // the connection itself failed: nothing came back from the API
	Status    int  // the HTTP status of an answer the API did not accept
	Kind      string
	APIText   string // the API's own words for a 400 invalid_request_error, for the server log only
	Text      string
}

func (e *claudeErr) Error() string { return e.Text }

// claudeClient is the one HTTP client for the API: no redirect is followed.
func (a *app) claudeClient() *http.Client {
	if a.claudeHTTP != nil {
		return a.claudeHTTP
	}
	return &http.Client{
		Timeout:       150 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// callClaude makes one call. Whatever it returns, the key is not in it.
func (a *app) callClaude(ctx context.Context, key string, in claudeRequest) (claudeAnswer, *claudeErr) {
	var out claudeAnswer
	body, err := json.Marshal(in)
	if err != nil {
		return out, &claudeErr{Text: "The request could not be made."}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, claudeURL, bytes.NewReader(body))
	if err != nil {
		return out, &claudeErr{Text: "The request could not be made."}
	}
	if req.URL.Scheme != "https" || req.URL.Host != claudeHost {
		return out, &claudeErr{Text: "Only api.anthropic.com is called."}
	}
	req.Header.Set("x-api-key", key)
	req.Header.Set("anthropic-version", claudeVersion)
	req.Header.Set("content-type", "application/json")
	resp, err := a.claudeClient().Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return out, &claudeErr{Stopped: true, Text: "The call was stopped."}
		}
		return out, &claudeErr{Down: true, Unreached: true, Text: "Claude could not be reached."}
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxClaudeAnswer))
	if err != nil {
		return out, &claudeErr{Down: true, Unreached: true, Text: "The answer of Claude could not be read."}
	}
	switch {
	case resp.StatusCode == 401 || resp.StatusCode == 403:
		return out, &claudeErr{Down: true, Refused: true, Text: "Claude refused the key."}
	case resp.StatusCode == 429 || resp.StatusCode >= 500:
		return out, &claudeErr{Down: true, Text: fmt.Sprintf("Claude answered with status %d.", resp.StatusCode)}
	case resp.StatusCode >= 300:
		var e struct {
			Error struct {
				Type    string `json:"type"`
				Message string `json:"message"`
			} `json:"error"`
		}
		json.Unmarshal(raw, &e)
		kind := e.Error.Type
		if kind == "" || len(kind) > 60 {
			kind = "unknown"
		}
		// The text of the error is the API's own words; it is never kept, only its kind.
		if kind == "billing_error" || kind == "permission_error" {
			return out, &claudeErr{Down: true, Refused: true, Text: "Claude refused the call (" + kind + ")."}
		}
		ce := &claudeErr{Status: resp.StatusCode, Kind: kind, Text: fmt.Sprintf("Claude did not accept the request (status %d, %s).", resp.StatusCode, kind)}
		if resp.StatusCode == 400 && kind == "invalid_request_error" {
			ce.APIText = e.Error.Message // never shown, never put on an item: logRefused cleans it for the log
		}
		return out, ce
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return claudeAnswer{}, &claudeErr{Text: "The answer of Claude could not be understood."}
	}
	return out, nil
}

// checkClaudeKey is Save: the cheapest possible call, one token from the
// smallest model. A key the API accepts (even if it then says something about
// the model) is a key; a refused key is not saved.
func (a *app) checkClaudeKey(ctx context.Context, key string) *claudeErr {
	_, err := a.callClaude(ctx, key, claudeRequest{Model: modelHaiku, MaxTokens: 1, Messages: []claudeMsg{{Role: "user", Content: "Hi"}}})
	if err != nil && (err.Down || err.Refused || err.Stopped) {
		return err
	}
	if err != nil && err.Status >= 300 && err.Status < 400 {
		// An answer that sends the call somewhere else is not a key that was accepted.
		return &claudeErr{Refused: true, Text: "Claude did not accept the key."}
	}
	return nil
}

// --- what a call cost ---

type modelPrice struct {
	In  float64 `json:"in"`  // US dollars per million tokens
	Out float64 `json:"out"` // US dollars per million tokens
}

type priceTable struct {
	Prices    map[string]modelPrice `json:"prices"`
	Confirmed bool                  `json:"confirmed"` // the owner has looked at them and saved them
}

// defaultPrices are the prices of the three models offered in Settings, in
// US dollars per million tokens, from Anthropic's price list as read on
// 2026-11-20. The small model costs less for prompts up to 100,000 tokens; it
// is counted at its price for longer ones, so a limit is never passed.
func defaultPrices() priceTable {
	return priceTable{Prices: map[string]modelPrice{
		modelHaiku:  {In: 0.5, Out: 2.5},
		modelSonnet: {In: 2, Out: 10},
		modelOpus:   {In: 4, Out: 20},
	}}
}

func (a *app) priceTable(ctx context.Context) priceTable {
	t := defaultPrices()
	var saved priceTable
	if json.Unmarshal([]byte(a.store.Setting(ctx, keyPrices)), &saved) == nil && len(saved.Prices) > 0 {
		t = saved
	}
	return t
}

// costMicro is what a call cost in millionths of a US dollar: tokens times
// dollars per million tokens. A model with no price is counted at the highest
// price in the table, so a limit is never passed by a missing line.
func (t priceTable) costMicro(model string, in, out int) int64 {
	p, ok := t.Prices[model]
	if !ok {
		for _, q := range t.Prices {
			p.In, p.Out = math.Max(p.In, q.In), math.Max(p.Out, q.Out)
		}
	}
	return int64(math.Round(float64(in)*p.In + float64(out)*p.Out))
}

func usd(micro int64) string { return fmt.Sprintf("$%.2f", float64(micro)/1e6) }

// recordUsage keeps a call and what it cost.
func (a *app) recordUsage(ctx context.Context, purpose, ref, model string, ans claudeAnswer) int64 {
	micro := a.priceTable(ctx).costMicro(model, ans.Usage.In, ans.Usage.Out)
	now := a.now()
	uctx := context.WithoutCancel(ctx) // a call that was paid for is recorded even while the app stops
	if err := a.store.Update(uctx, func(tx *repo.Tx) error {
		return tx.AddUsage(uctx, now.Unix(), a.dayKey(now), purpose, ref, model, ans.Usage.In, ans.Usage.Out, micro)
	}); err != nil {
		appkit.Error("recording what a call to Claude cost", err)
	}
	return micro
}

// --- the settings of the review and the night check ---

func (a *app) dayKey(t time.Time) string { return t.In(a.loc).Format("2006-01-02") }

func (a *app) modelSetting(ctx context.Context, key, def string) string {
	if v := strings.TrimSpace(a.store.Setting(ctx, key)); v != "" {
		return v
	}
	return def
}

func (a *app) reviewModel(ctx context.Context) string {
	return a.modelSetting(ctx, keyReviewModel, modelSonnet)
}
func (a *app) nightModel(ctx context.Context) string {
	return a.modelSetting(ctx, keyNightModel, modelOpus)
}

func (a *app) usdSetting(ctx context.Context, key string, def float64) float64 {
	if v, err := strconv.ParseFloat(strings.TrimSpace(a.store.Setting(ctx, key)), 64); err == nil && v >= 0 {
		return v
	}
	return def
}

func (a *app) dailyLimit(ctx context.Context) float64 {
	return a.usdSetting(ctx, keyDailyLimit, defDailyLimit)
}
func (a *app) nightLimit(ctx context.Context) float64 {
	return a.usdSetting(ctx, keyNightLimit, defNightLimit)
}

// nightAt is the minute of the day (Europe/Prague) the night check starts.
func (a *app) nightAt(ctx context.Context) (h, m int, text string) {
	text = a.modelSetting(ctx, keyNightAt, defNightAt)
	t, err := time.Parse("15:04", text)
	if err != nil {
		t, _ = time.Parse("15:04", defNightAt)
		text = defNightAt
	}
	return t.Hour(), t.Minute(), text
}

func limitMicro(limit float64) int64 { return int64(math.Round(limit * 1e6)) }

// spentToday is what today's review calls cost (the day turns at midnight Prague time).
func (a *app) spentToday(ctx context.Context) int64 {
	return a.store.SpentMicro(ctx, "review", a.dayKey(a.now()), "")
}

func (a *app) limitReached(ctx context.Context) bool {
	return a.spentToday(ctx) >= limitMicro(a.dailyLimit(ctx))
}

// --- the key on the Connections page ---

type claudeAPIView struct {
	Status string // Connected or Not configured
	Cls    string
	Notice string
	Error  string
}

func (a *app) claudeAPIViewFor(ctx context.Context, notice, errText string) claudeAPIView {
	v := claudeAPIView{Status: statusNotConf, Cls: "st-stale", Notice: notice, Error: errText}
	if a.claudeConfigured() {
		v.Status, v.Cls = "Connected", "st-ok"
	}
	return v
}

// claudeKeySave is Save. The key is checked first with the cheapest call; a
// refused key is not saved, and nothing said back holds it.
func (a *app) claudeKeySave(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 4<<10)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "The form could not be read.", http.StatusBadRequest)
		return
	}
	key := strings.TrimSpace(r.PostForm.Get("key"))
	if key == "" || len(key) > 300 || strings.ContainsAny(key, " \t\r\n\x00") {
		a.renderConnectors(w, r, http.StatusBadRequest, "claude", "", "Paste the key as Claude gave it, without spaces.")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 40*time.Second)
	defer cancel()
	if e := a.checkClaudeKey(ctx, key); e != nil {
		msg := "Claude cannot be reached right now, so the key was not checked and not saved. Try again."
		code := http.StatusBadGateway
		if e.Refused {
			msg, code = "Claude refused this key. Check it and try again.", http.StatusBadRequest
		}
		a.renderConnectors(w, r, code, "claude", "", msg)
		return
	}
	if err := writeSecret(a.secretsDir, claudeSecretFile, claudeSecret{Key: key}); err != nil {
		http.Error(w, "The key could not be saved. Try again.", http.StatusInternalServerError)
		return
	}
	a.keyEvent(ctx, "claude_key_saved")
	a.clearDown(ctx)
	a.wakeReview()
	http.Redirect(w, r, "/connectors?n=claudekey#claudeapi", http.StatusSeeOther)
}

func (a *app) claudeKeyRemove(w http.ResponseWriter, r *http.Request) {
	if err := removeSecret(a.secretsDir, claudeSecretFile); err != nil {
		http.Error(w, "The key could not be removed. Try again.", http.StatusInternalServerError)
		return
	}
	a.keyEvent(r.Context(), "claude_key_removed")
	a.clearDown(r.Context())
	http.Redirect(w, r, "/connectors?n=claudeoff#claudeapi", http.StatusSeeOther)
}

// keyEvent leaves a line in the event log when the owner saves or removes the key.
// It says only that it happened; the key is never in it.
func (a *app) keyEvent(ctx context.Context, typ string) {
	ctx = context.WithoutCancel(ctx)
	if err := a.store.Update(ctx, func(tx *repo.Tx) error {
		return tx.InsertEvent(ctx, a.now().Unix(), 0, "owner", typ, map[string]any{})
	}); err != nil {
		appkit.Error("recording a change of the key", err)
	}
}

// cleanAPIText makes the API's description of a refused request fit for the
// server log: anything between quotes longer than 40 characters is replaced
// (it could be a piece of a message), an opening quote with no end counts as
// such a piece, and the whole is cut to 300 characters.
func cleanAPIText(s string) string {
	var b strings.Builder
	r := []rune(s)
	for i := 0; i < len(r); i++ {
		c := r[i]
		if c != '"' && c != '\'' && c != '`' {
			b.WriteRune(c)
			continue
		}
		end := -1
		for j := i + 1; j < len(r); j++ {
			if r[j] == c {
				end = j
				break
			}
		}
		if end < 0 {
			if len(r)-i-1 > 40 {
				b.WriteString("[cut]")
				break
			}
			b.WriteRune(c)
			continue
		}
		if end-i-1 > 40 {
			b.WriteString("[cut]")
		} else {
			b.WriteString(string(r[i : end+1]))
		}
		i = end
	}
	out := []rune(b.String())
	if len(out) > 300 {
		out = out[:300]
	}
	return string(out)
}
