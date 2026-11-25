package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"lab/apps/one/repo"
	"lab/platform/appkit"
)

// The way in for the chat bridge (SPEC 16): a few addresses under /api/bridge/
// with a token of their own. The bridge connects out from the owner's Mac; the
// server never calls it. The bridge token opens these addresses and nothing
// else: it cannot read mail, items, notes or anything else, and it cannot
// approve. The agents' token does not open these addresses either.

const (
	bridgeTokenFile = "bridge-token"
	bridgePrefix    = "/api/bridge/"
	maxBatchBody    = 8 << 20
	maxUpload       = maxChatFile // a file of a chat has no limit worth the name; this only stops a runaway
	outboxPage      = 5
	outPage         = 20

	keyBridgeHeard = "bridge.heard_at"
	keyBridgeNets  = "bridge.networks"
	keyBridgeVer   = "bridge.version"
	keyBridgeFiles = "bridge.files"
	keyBridgeErr   = "bridge.error"
	keyBridgeErrAt = "bridge.error_at"
	keyBridgeLogin = "bridge.logins" // per network: the times it needed a new login

	// What the server tells the bridge: how far back to read the first time,
	// and the biggest file it takes (SPEC 7, 16).
	maxChatFile = 1 << 30
)

// --- the token ---

func (a *app) loadBridgeToken() error {
	b, err := os.ReadFile(filepath.Join(a.secretsDir, bridgeTokenFile))
	if err == nil {
		if t := strings.TrimSpace(string(b)); len(t) >= 32 {
			a.bridgeMu.Lock()
			a.bridgeTok = t
			a.bridgeMu.Unlock()
			return nil
		}
	}
	return a.newBridgeToken()
}

// newBridgeToken makes a fresh bridge token; the old one stops working at once.
func (a *app) newBridgeToken() error {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return err
	}
	t := base64.RawURLEncoding.EncodeToString(raw)
	if err := os.MkdirAll(a.secretsDir, 0o700); err != nil {
		return err
	}
	path := filepath.Join(a.secretsDir, bridgeTokenFile)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(t+"\n"), 0o600); err != nil {
		return err
	}
	if err := os.Chmod(tmp, 0o600); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	a.bridgeMu.Lock()
	a.bridgeTok = t
	a.bridgeMu.Unlock()
	return nil
}

func (a *app) bridgeTokenValue() string {
	a.bridgeMu.RLock()
	defer a.bridgeMu.RUnlock()
	return a.bridgeTok
}

// bearerMatches compares an Authorization header with a token in constant time.
// With no token on file nothing is accepted.
func bearerMatches(header, want string) bool {
	if want == "" {
		return false
	}
	const prefix = "bearer "
	if len(header) <= len(prefix) || !strings.EqualFold(header[:len(prefix)], prefix) {
		return false
	}
	got := sha256.Sum256([]byte(strings.TrimSpace(header[len(prefix):])))
	exp := sha256.Sum256([]byte(want))
	return subtle.ConstantTimeCompare(got[:], exp[:]) == 1
}

// agentTokenNew's twin: only this signed-in page makes a new bridge token.
func (a *app) bridgeTokenNew(w http.ResponseWriter, r *http.Request) {
	if err := a.newBridgeToken(); err != nil {
		appkit.Error("making a new bridge token", err)
		http.Error(w, "The token could not be made. Try again.", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/connectors?n=bridgenew#bridge", http.StatusSeeOther)
}

// --- the addresses ---

// bridgeAPI answers everything under /api/bridge/. The token is checked
// first; a request with the agents' token (or none) is refused as any other
// wrong token is.
func (a *app) bridgeAPI(w http.ResponseWriter, r *http.Request) {
	if !bearerMatches(r.Header.Get("Authorization"), a.bridgeTokenValue()) {
		writeFixed(w, http.StatusUnauthorized, wrongTokenMsg)
		return
	}
	// A call of an automated test carries the run's mark in the header; what it
	// writes is marked and it never counts as the real bridge being heard.
	run, err := runFrom(r.Header, "")
	if err != nil {
		answerErr(w, err)
		return
	}
	a.bridge.ServeHTTP(w, r.WithContext(repo.WithRun(r.Context(), run)))
}

func (a *app) bridgeRoutes() *http.ServeMux {
	m := newRecMux()
	m.HandleFunc("POST /api/bridge/alive", a.bridgeAlive)
	m.HandleFunc("POST /api/bridge/batch", a.bridgeBatch)
	m.HandleFunc("PUT /api/bridge/files", a.bridgeFile)
	m.HandleFunc("POST /api/bridge/outbox", a.bridgeOutbox)
	m.HandleFunc("POST /api/bridge/outbox/{id}/result", a.bridgeResult)
	m.HandleFunc("POST /api/bridge/posts/{id}/result", a.bridgePostResult)
	m.HandleFunc("GET /api/bridge/posts/{id}/file", a.bridgePostFile)
	m.HandleFunc("/api/bridge/", func(w http.ResponseWriter, r *http.Request) {
		if ownerOnly.MatchString(r.URL.Path) {
			a.store.Update(r.Context(), func(tx *repo.Tx) error {
				return tx.InsertEvent(r.Context(), a.now().Unix(), 0, "agent", "refused", map[string]any{"guard": "Actor by channel", "why": whyOnlyOwner})
			})
			writeFixed(w, http.StatusForbidden, onlyOwnerMsg)
			return
		}
		apiError(w, http.StatusNotFound, "no such address")
	})
	a.bridgePatterns = m.patterns
	return m.ServeMux
}

// heard records that the Mac has just been heard from. A call under a test
// run's mark is not the Mac and records nothing (SPEC 16).
func (a *app) heard(ctx context.Context) {
	if repo.RunOf(ctx) != "" {
		return
	}
	ctx = context.WithoutCancel(ctx)
	v := strconv.FormatInt(a.now().Unix(), 10)
	if a.store.Setting(ctx, keyBridgeHeard) != v {
		if err := a.store.SetSettingBy(ctx, keyBridgeHeard, v, "bridge", a.now()); err != nil {
			appkit.Error("recording that the bridge was heard", err)
		}
	}
}

func extendRead(w http.ResponseWriter, d time.Duration) {
	http.NewResponseController(w).SetReadDeadline(time.Now().Add(d))
}

// bridgeNet is what the bridge says of one network.
type bridgeNet struct {
	Network    string `json:"network"`
	State      string `json:"state"` // connected, needs_login, error or unknown
	Detail     string `json:"detail,omitempty"`
	Status     string `json:"status,omitempty"`      // Beeper's own word for the account (connection_required, ...)
	StatusText string `json:"status_text,omitempty"` // Beeper's own explanation, if any
}

// netWords says in plain words what Beeper's status of an account means.
var netWords = map[string]string{
	"connected":           "is connected",
	"connecting":          "is connecting",
	"backfilling":         "is catching up on older messages",
	"connection_required": "needs a new login in Beeper",
	"reconnect_required":  "has to be logged in again in Beeper",
	"attention_required":  "needs your attention in Beeper",
	"disconnected":        "is disconnected in Beeper",
	"disabled":            "is switched off in Beeper",
}

// needsLoginStatus are the account states of Beeper that mean the owner has to log in again.
func needsLoginStatus(s string) bool {
	switch s {
	case "connection_required", "reconnect_required", "attention_required", "disconnected":
		return true
	}
	return false
}

var netStates = map[string]bool{"connected": true, "needs_login": true, "error": true, "unknown": true}

// bridgeAlive is the bridge saying it is there, with the state of each
// network it reads. It is called about once a minute.
func (a *app) bridgeAlive(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Networks  []bridgeNet `json:"networks"`
		Version   string      `json:"version"`
		LastError string      `json:"last_error"`
		FilesPend int         `json:"files_pending"`
		FilesNote string      `json:"files_note"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	ctx := r.Context()
	if repo.RunOf(ctx) != "" { // a test's call: nothing of it is the real bridge's state
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "time": a.now().UTC().Format(time.RFC3339), "backfill_days": a.backfillDays(ctx), "max_file_bytes": maxChatFile})
		return
	}
	if len(in.Networks) > 20 {
		in.Networks = in.Networks[:20]
	}
	for i := range in.Networks {
		n := &in.Networks[i]
		n.Network = strings.ToLower(oneLine(n.Network, 20))
		if !netStates[n.State] {
			n.State = "unknown"
		}
		n.Detail = oneLine(n.Detail, 200)
		n.Status = strings.ToLower(oneLine(n.Status, 40))
		n.StatusText = oneLine(n.StatusText, 200)
		if _, known := netWords[n.Status]; !known && n.Status != "" {
			n.Status = "" // a word of Beeper's that ONE does not know is not shown as one
		}
		// Beeper's own status decides when it says one: a network that needs a new login is that, whatever else was sent.
		if needsLoginStatus(n.Status) {
			n.State = "needs_login"
		}
	}
	nets, _ := json.Marshal(in.Networks)
	now := a.now()
	err := a.store.Update(ctx, func(tx *repo.Tx) error {
		if err := a.noteLogins(ctx, tx, in.Networks, now); err != nil {
			return err
		}
		if tx.Setting(ctx, keyBridgeNets) != string(nets) {
			if err := tx.SetSettingBy(ctx, keyBridgeNets, string(nets), "bridge", now); err != nil {
				return err
			}
		}
		if v := oneLine(in.Version, 40); tx.Setting(ctx, keyBridgeVer) != v {
			if err := tx.SetSettingBy(ctx, keyBridgeVer, v, "bridge", now); err != nil {
				return err
			}
		}
		fp := ""
		if in.FilesPend > 0 {
			fp = strconv.Itoa(min(in.FilesPend, 100000)) + "|" + oneLine(in.FilesNote, 200)
		}
		if tx.Setting(ctx, keyBridgeFiles) != fp {
			if err := tx.SetSettingBy(ctx, keyBridgeFiles, fp, "bridge", now); err != nil {
				return err
			}
		}
		if e := oneLine(in.LastError, 300); e != tx.Setting(ctx, keyBridgeErr) {
			if err := tx.SetSettingBy(ctx, keyBridgeErr, e, "bridge", now); err != nil {
				return err
			}
			if err := tx.SetSettingBy(ctx, keyBridgeErrAt, strconv.FormatInt(now.Unix(), 10), "bridge", now); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		appkit.Error("recording the bridge's state", err)
		apiError(w, http.StatusInternalServerError, "That did not work. Try again.")
		return
	}
	a.heard(ctx)
	a.checkSource(ctx, bridgeSrc) // a network that logged itself out is told at once
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "time": now.UTC().Format(time.RFC3339), "backfill_days": a.backfillDays(ctx), "max_file_bytes": maxChatFile})
}

// backfillDays is how far back the bridge reads a chat it has never read: the
// review window (SPEC 3, a month by default). The server says it; the bridge
// does not choose.
func (a *app) backfillDays(ctx context.Context) int {
	return max(1, int(a.syncSettings(ctx).ReviewWindow/(24*time.Hour)))
}

// loginLog is the dated count of how often each network needed a new login
// (the owner's own criteria for this project watch it): the times, newest last.
type loginLog map[string][]int64

const keepLogins = 100

func (a *app) loginsLog(ctx context.Context, q repo.Q) loginLog {
	var l loginLog
	json.Unmarshal([]byte(q.Setting(ctx, keyBridgeLogin)), &l)
	if l == nil {
		l = loginLog{}
	}
	return l
}

// noteLogins counts a network that has newly gone into a state that needs a
// new login: one event for each time it goes from fine to not fine, however
// often it is reported meanwhile.
func (a *app) noteLogins(ctx context.Context, tx *repo.Tx, now []bridgeNet, at time.Time) error {
	var before []bridgeNet
	json.Unmarshal([]byte(tx.Setting(ctx, keyBridgeNets)), &before)
	was := map[string]bool{}
	for _, n := range before {
		was[n.Network] = n.State == "needs_login"
	}
	l := a.loginsLog(ctx, tx.Q)
	changed := false
	for _, n := range now {
		if n.State != "needs_login" || was[n.Network] {
			continue
		}
		l[n.Network] = append(l[n.Network], at.Unix())
		if len(l[n.Network]) > keepLogins {
			l[n.Network] = l[n.Network][len(l[n.Network])-keepLogins:]
		}
		changed = true
	}
	if !changed {
		return nil
	}
	b, _ := json.Marshal(l)
	return tx.SetSettingBy(ctx, keyBridgeLogin, string(b), "bridge", at)
}

// bridgeBatch takes a batch of chats and messages. Sending the same batch
// again writes nothing the second time. It answers which attachments the
// bridge still has to upload.
func (a *app) bridgeBatch(w http.ResponseWriter, r *http.Request) {
	extendRead(w, 2*time.Minute)
	r.Body = http.MaxBytesReader(w, r.Body, maxBatchBody)
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		apiError(w, http.StatusRequestEntityTooLarge, "the batch is too large")
		return
	}
	var b bridgeBatch
	if err := json.Unmarshal(raw, &b); err != nil {
		apiError(w, http.StatusBadRequest, "the body must be a JSON batch of chats")
		return
	}
	run, err := runFrom(r.Header, b.Run)
	if err != nil {
		answerErr(w, err)
		return
	}
	ctx := repo.WithRun(r.Context(), run)
	res, err := a.ingestBatch(ctx, b)
	if err != nil {
		answerErr(w, err)
		return
	}
	a.heard(ctx)
	a.logBatch(res)
	if res.Missing == nil {
		res.Missing = []missingFile{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "chats": res.Chats, "messages": res.Messages, "stored": res.Stored,
		"versions": res.Versions, "unchanged": res.Unchanged, "dropped": res.Dropped, "left_out": res.Failed, "missing_files": res.Missing,
		"backfill_days": a.backfillDays(ctx), "max_file_bytes": maxChatFile})
}

// logBatch says how a batch went: numbers only, never a name or a text.
func (a *app) logBatch(res batchResult) {
	if res.Stored+res.Versions+res.Dropped+res.Failed == 0 {
		return
	}
	appkit.Logger().Info("bridge batch", "chats", res.Chats, "messages", res.Messages, "stored", res.Stored, "versions", res.Versions, "dropped", res.Dropped, "left_out", res.Failed)
}

// bridgeFile takes one attachment of a chat message. The file goes into the
// pool by its hash: the same bytes are kept once, however many messages hold
// them. The attachment must be one the batch announced.
func (a *app) bridgeFile(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	network, chatID, msgID, att := q.Get("network"), q.Get("chat_id"), q.Get("message_id"), q.Get("attachment")
	if network == "" || chatID == "" || msgID == "" || att == "" {
		apiError(w, http.StatusBadRequest, "name the network, chat_id, message_id and attachment")
		return
	}
	ctx := r.Context()
	msg, ok := a.store.MessageBySource(ctx, strings.ToLower(network), chatSourceID(chatID, msgID))
	if !ok {
		apiError(w, http.StatusNotFound, "no such message")
		return
	}
	files, err := a.store.FilesOfMessage(ctx, msg.ID)
	if err != nil {
		answerErr(w, err)
		return
	}
	var f repo.File
	found := false
	for _, x := range files {
		if x.Part == att {
			f, found = x, true
		}
	}
	if !found {
		apiError(w, http.StatusNotFound, "no such attachment")
		return
	}
	a.heard(ctx)
	if f.Status == "saved" {
		io.Copy(io.Discard, io.LimitReader(r.Body, 1<<20))
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "unchanged": true})
		return
	}
	extendRead(w, 30*time.Minute)
	r.Body = http.MaxBytesReader(w, r.Body, maxUpload)
	sha, rel, size, created, err := a.imp.pool.write(r.Body)
	if err != nil {
		apiError(w, http.StatusBadRequest, "the file could not be read to its end")
		return
	}
	if err := a.store.Update(ctx, func(tx *repo.Tx) error {
		if err := tx.MarkFileSaved(ctx, f.ID, size, sha, rel); err != nil {
			return err
		}
		return tx.PoolUsed(ctx, rel, created, f.ItemID)
	}); err != nil {
		appkit.Error("saving an attachment of a chat", err)
		apiError(w, http.StatusInternalServerError, "That did not work. Try again.")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "size": size})
}

// --- chat replies waiting for the Mac ---

type outReply struct {
	ID      int64  `json:"id"`
	Network string `json:"network"`
	ChatID  string `json:"chat_id"`
	Text    string `json:"text"`
}

type outResend struct {
	Network   string `json:"network"`
	ChatID    string `json:"chat_id"`
	MessageID string `json:"message_id"`
}

// bridgeOutbox is the bridge asking what waits for it: the chat replies the owner
// approved, and the dropped messages he asked to have imported again. Each
// reply is handed out once. A reply that was handed out and not reported is
// never handed out again; a withdrawn one never is.
func (a *app) bridgeOutbox(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	a.heard(ctx)
	now := a.now().Unix()
	var claimed []repo.Proposal
	var claimedOut []repo.OutPart
	err := a.store.Update(ctx, func(tx *repo.Tx) error {
		var err error
		claimed, err = tx.ClaimQueued(ctx, outboxPage, now)
		if err != nil {
			return err
		}
		claimedOut, err = tx.ClaimOut(ctx, outPage, now)
		return err
	})
	if err != nil {
		appkit.Error("handing out chat replies", err)
		apiError(w, http.StatusInternalServerError, "That did not work. Try again.")
		return
	}
	replies := []outReply{}
	for _, p := range claimed {
		rt := parseRecipients(p.Recipient)
		chat, ok := a.store.ChatByKey(ctx, repo.ChatKey(rt.Network, rt.ChatID))
		if !ok || chat.ItemID != p.ItemID || strings.TrimSpace(p.FinalText) == "" {
			// The chat is not the one the reply was proposed in: nothing is sent.
			a.finishSend(context.WithoutCancel(ctx), p, &sendFailure{Text: "The chat is not the one this reply was written for, so nothing was sent."})
			continue
		}
		replies = append(replies, outReply{ID: p.ID, Network: rt.Network, ChatID: rt.ChatID, Text: p.FinalText})
	}
	resend := []outResend{}
	if again, err := a.store.AgainRequests(ctx, 50); err == nil {
		for _, s := range again {
			if chatID, msgID, ok := splitChatSourceID(s.SourceID); ok {
				resend = append(resend, outResend{Network: s.Source, ChatID: chatID, MessageID: msgID})
			}
		}
	}
	// Messages left out of a batch because each failed on its own are asked for
	// again the same way, with growing waits and a limit (softfail.go); asking
	// counts as a try, so one the Mac can no longer find is not asked for for ever.
	for _, id := range a.failDue(ctx, "chat", 50) {
		network, sid, ok := strings.Cut(id, "\x1f")
		if !ok {
			a.failClear(ctx, "chat", id)
			continue
		}
		if chatID, msgID, ok := splitChatSourceID(sid); ok && len(resend) < 100 {
			resend = append(resend, outResend{Network: network, ChatID: chatID, MessageID: msgID})
			a.failNote(ctx, "chat", id)
		}
	}
	posts := []outPost{}
	for _, p := range claimedOut {
		chat, ok := a.store.ChatByKey(ctx, repo.ChatKey(p.Network, p.ChatID))
		if !ok || chat.ItemID != p.ItemID {
			// The chat is not the one the message was written in: nothing is sent.
			a.finishOut(context.WithoutCancel(ctx), p, "failed", "The chat is not the one this was written in, so nothing was sent.", "")
			continue
		}
		op := outPost{ID: p.ID, Group: p.Group, Seq: p.Seq, Network: p.Network, ChatID: p.ChatID, Kind: p.Kind, Text: p.Text}
		if p.Kind == "file" {
			op.File = &outFile{Name: p.FileName, Type: p.FileType, Size: p.Size}
		}
		posts = append(posts, op)
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "replies": replies, "resend": resend, "posts": posts,
		"backfill_days": a.backfillDays(ctx), "max_file_bytes": maxChatFile})
}

// outPost is a message the owner wrote himself, handed to the bridge once: the
// exact text, or one file that the bridge fetches from ONE with its own token.
type outPost struct {
	ID      int64    `json:"id"`
	Group   string   `json:"group"`
	Seq     int      `json:"seq"`
	Network string   `json:"network"`
	ChatID  string   `json:"chat_id"`
	Kind    string   `json:"kind"` // text or file
	Text    string   `json:"text,omitempty"`
	File    *outFile `json:"file,omitempty"`
}

type outFile struct {
	Name string `json:"name"`
	Type string `json:"type"`
	Size int64  `json:"size"`
}

// bridgePostResult is the bridge reporting how one part of the owner's write
// ended: sent, failed (it knows nothing went) or unknown (it may have gone).
func (a *app) bridgePostResult(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		apiError(w, http.StatusNotFound, "no such message")
		return
	}
	var in struct {
		State     string `json:"state"`
		Error     string `json:"error"`
		MessageID string `json:"message_id"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	if in.State != "sent" && in.State != "failed" && in.State != "unknown" {
		apiError(w, http.StatusBadRequest, `state must be "sent", "failed" or "unknown"`)
		return
	}
	ctx := r.Context()
	a.heard(ctx)
	p, ok := a.store.OutByID(ctx, id)
	if !ok {
		apiError(w, http.StatusNotFound, "no such message")
		return
	}
	switch p.State {
	case "handed":
	case "unknown":
		if in.State == "unknown" {
			writeJSON(w, http.StatusOK, map[string]any{"ok": true, "unchanged": true})
			return
		}
	case "sent", "failed":
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "unchanged": true})
		return
	default:
		apiError(w, http.StatusConflict, "That message was not handed out.")
		return
	}
	reason := oneLine(strings.ToValidUTF8(in.Error, ""), 300)
	state := a.finishOut(context.WithoutCancel(ctx), p, in.State, reason, oneLine(in.MessageID, 200))
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "state": state})
}

// bridgePostFile gives the bridge the bytes of one file of a message it has
// just been handed. Only that: a file of a message that was not handed out, or
// that has been reported, is not there.
func (a *app) bridgePostFile(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		apiError(w, http.StatusNotFound, "no such file")
		return
	}
	ctx := r.Context()
	p, ok := a.store.OutFileOf(ctx, id)
	if !ok || !poolPathRE.MatchString(p.Path) {
		apiError(w, http.StatusNotFound, "no such file")
		return
	}
	f, err := a.imp.pool.open(p.Path)
	if err != nil {
		apiError(w, http.StatusNotFound, "no such file")
		return
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() {
		apiError(w, http.StatusNotFound, "no such file")
		return
	}
	a.heard(ctx)
	extendRead(w, 5*time.Minute)
	http.NewResponseController(w).SetWriteDeadline(time.Now().Add(10 * time.Minute))
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", strconv.FormatInt(st.Size(), 10))
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	io.Copy(w, f)
}

// bridgeResult is the bridge reporting how a reply it was handed ended: sent,
// failed (it knows nothing went) or unknown (it may have gone).
func (a *app) bridgeResult(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		apiError(w, http.StatusNotFound, "no such reply")
		return
	}
	var in struct {
		State     string `json:"state"`
		Error     string `json:"error"`
		MessageID string `json:"message_id"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	if in.State != "sent" && in.State != "failed" && in.State != "unknown" {
		apiError(w, http.StatusBadRequest, `state must be "sent", "failed" or "unknown"`)
		return
	}
	ctx := r.Context()
	a.heard(ctx)
	p, ok := a.store.ChatSendOf(ctx, id)
	if !ok {
		apiError(w, http.StatusNotFound, "no such reply")
		return
	}
	switch p.SendState {
	case "handed":
	case "unknown":
		if in.State == "unknown" {
			writeJSON(w, http.StatusOK, map[string]any{"ok": true, "unchanged": true})
			return
		}
	case "sent", "failed":
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "unchanged": true})
		return
	default:
		apiError(w, http.StatusConflict, "That reply was not handed out.")
		return
	}
	p.SendRef = oneLine(in.MessageID, 200)
	var fail *sendFailure
	reason := oneLine(strings.ToValidUTF8(in.Error, ""), 300)
	switch in.State {
	case "failed":
		if reason == "" {
			reason = "The Mac could not send it."
		}
		fail = &sendFailure{Text: reason}
	case "unknown":
		if reason == "" {
			reason = "The Mac did not say whether it went."
		}
		fail = &sendFailure{Doubt: true, Text: reason + " It may have gone: check the chat before you try again."}
	}
	state := a.finishSend(context.WithoutCancel(ctx), p, fail)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "state": state})
}

// chatSendsInDoubt marks the chat replies the Mac took and never reported:
// they are not handed out again, and the thread says they may have gone.
func (a *app) chatSendsInDoubt(ctx context.Context) {
	ctx = context.WithoutCancel(ctx)
	before := a.now().Add(-a.bridgeSilence(ctx)).Unix()
	ps, err := a.store.HandedBefore(ctx, before)
	if err != nil {
		appkit.Error("looking for chat replies in doubt", err)
		return
	}
	for _, p := range ps {
		a.finishSend(ctx, p, &sendFailure{Doubt: true, Text: "The Mac took this reply and did not say whether it went. It may have gone: check the chat before you try again."})
	}
}

// withdrawChatReply is Withdraw on a chat reply that waits for the Mac. Only
// the signed-in page does it; a reply the Mac already took cannot be taken back.
func (a *app) withdrawChatReply(w http.ResponseWriter, r *http.Request) {
	pid, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || pid <= 0 {
		http.NotFound(w, r)
		return
	}
	ctx := r.Context()
	now := a.now().Unix()
	var itemID int64
	var withdrew bool
	err = a.store.Update(ctx, func(tx *repo.Tx) error {
		p, ok := tx.ChatSendOf(ctx, pid)
		if !ok {
			return errNoProposal
		}
		itemID = p.ItemID
		var err error
		withdrew, err = tx.WithdrawQueued(ctx, pid, now)
		if err != nil || !withdrew {
			return err
		}
		return tx.InsertEvent(ctx, now, p.ItemID, "owner", "send_withdrawn", map[string]any{"proposal": pid})
	})
	var ue *userErr
	switch {
	case errors.As(err, &ue):
		http.Error(w, ue.Msg, ue.Status)
		return
	case err != nil:
		appkit.Error("withdrawing a chat reply", err)
		http.Error(w, "That did not work. Try again.", http.StatusInternalServerError)
		return
	case !withdrew:
		http.Error(w, "The Mac has already taken this reply, so it cannot be withdrawn.", http.StatusConflict)
		return
	}
	if r.Header.Get("X-Requested-With") != "one" {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	a.rowResponse(w, r, itemID, true, false)
}

// planChatReply works out where a reply to a chat goes: into the chat the
// newest message came from, which is the only place it can go.
func (a *app) planChatReply(ctx context.Context, q repo.Q, it repo.Item, newest repo.Message) (replyPlan, error) {
	p := replyPlan{Action: replyChat, Target: newest}
	chat, ok := q.ChatOfThread(ctx, it.ID, newest.ThreadKey)
	if !ok || chat.Network != newest.Network {
		return p, conflict("This chat is not known well enough to answer it.")
	}
	if a.guardOn(ctx, q, "blocked_targets") {
		blocked := a.blockedTargets(ctx, q)
		senders, err := q.SendersOfThread(ctx, it.ID, newest.ThreadKey)
		if err != nil {
			return p, err
		}
		for _, s := range senders {
			if blockedBy(blocked, s.Address) {
				return p, refuse("Blocked targets", whyBlockedReply)
			}
		}
		for _, part := range chat.Participants {
			if blockedBy(blocked, part.Handle) {
				return p, refuse("Blocked targets", whyBlockedReply)
			}
		}
	}
	name := chat.Name
	if name == "" {
		name = it.Subject
	}
	if chat.Group {
		name += " (group chat)"
	}
	p.Recipients = replyTo{To: []string{oneLine(name, 200)}, Network: chat.Network, ChatID: chat.ChatID, ChatName: chat.Name, Group: chat.Group}
	return p, nil
}

var _ = url.Values{}

// bridgeLimits are the known limitations of the chat bridge, in plain words
// (SPEC 23). A limitation is not an error and does not turn the dot.
var bridgeLimits = []string{
	"Chats arrive only while the Mac is on and Beeper is running.",
	"A network that has logged itself out delivers nothing until the owner logs in on the Mac.",
	"Phone calls are not covered.",
}

var netStateText = map[string]string{"connected": "Connected", "needs_login": "Needs login", "error": "Error", "unknown": "Not known"}

// bridgeViewFor is what the Connectors page says of the chat bridge.
func (a *app) bridgeViewFor(ctx context.Context, notice string) bridgeView {
	v := bridgeView{Token: a.bridgeTokenValue(), Notice: notice, Limits: bridgeLimits}
	if f := a.store.Setting(ctx, keyBridgeFiles); f != "" {
		n, note, _ := strings.Cut(f, "|")
		line := n + " file(s) from chats could not be fetched yet and are listed as pending."
		if note != "" {
			line += " " + note
		}
		v.Limits = append(append([]string{}, bridgeLimits...), line)
	}
	st := a.bridgeState(ctx, bridgeSrc)
	v.Status, v.StatusCls, v.Heard, v.LastRead = st.Status, st.Cls, st.Configured, st.LastRead
	if st.Status != statusOK || st.Error != "" {
		v.LatestError, v.ErrorAt = st.Error, st.ErrorAt
	}
	v.Chats = a.store.CountChats(ctx)
	v.Messages = a.store.CountChatMessages(ctx)
	since := a.now().Add(-24 * time.Hour).Unix()
	v.Items24, v.Messages24 = a.store.ChatItemsSince(ctx, since), a.store.ChatMessagesSince(ctx, since)
	v.Version = a.store.Setting(ctx, keyBridgeVer)
	logins := a.loginsLog(ctx, a.store.Q)
	for _, n := range a.bridgeNetworks(ctx) {
		cls := "st-ok"
		if n.State != "connected" {
			cls = "st-error"
			if n.State == "unknown" {
				cls = "st-stale"
			}
		}
		detail := n.Detail
		if w, ok := netWords[n.Status]; ok && n.Status != "connected" {
			detail = w + "."
			if n.StatusText != "" {
				detail += " Beeper says: " + n.StatusText
			}
		} else if n.State == "needs_login" && detail == "" {
			detail = "needs a new login in Beeper."
		}
		v.Networks = append(v.Networks, bridgeNetView{Name: sourceLabel(n.Network), State: netStateText[n.State], Cls: cls, Detail: detail, Logins: a.loginsText(logins[n.Network])})
	}
	return v
}

// loginsText says how often a network needed a new login, with dates: all
// the times that are kept, and how many of them fall in the last 30 days.
func (a *app) loginsText(times []int64) string {
	if len(times) == 0 {
		return ""
	}
	cut := a.now().Add(-30 * 24 * time.Hour).Unix()
	recent := 0
	for _, t := range times {
		if t >= cut {
			recent++
		}
	}
	times30 := func(n int) string {
		if n == 1 {
			return "once"
		}
		return fmt.Sprintf("%d times", n)
	}
	last := time.Unix(times[len(times)-1], 0).In(a.loc).Format("2 Jan 15:04")
	return fmt.Sprintf("Needed a new login %s in the last 30 days (%s in all counted), last on %s.", times30(recent), times30(len(times)), last)
}
