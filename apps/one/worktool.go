package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"lab/apps/one/repo"
	"lab/platform/appkit"
)

// Work tool sees ONE (SPEC 32). Work tool, the owner's work tool, reads one ready
// list of his private things with a token of its own. The token opens that
// list and nothing else: every other address and tool answers it as it
// answers a wrong token. The list is made from what ONE and its own Claude
// already recorded; it holds no message text, no file names and nothing from
// history or from test runs.

const (
	proTokenFile    = "worktool-token"
	privateListPath = "/api/private-list"
	proToolName     = "private_list"
	privateListMax  = 50
	privateDays     = 7
	privateWhyMax   = 240
	privateTitleMax = 90
	privateNote     = "Private. This list comes from ONE, the owner's own private app, made by ONE and its own Claude. It holds no message texts and no files; open an entry in ONE to see more."
)

// recMux is a ServeMux that remembers the patterns it was given, so a test can
// go through every address the app has.
type recMux struct {
	*http.ServeMux
	patterns []string
}

func newRecMux() *recMux { return &recMux{ServeMux: http.NewServeMux()} }

func (m *recMux) HandleFunc(pattern string, h func(http.ResponseWriter, *http.Request)) {
	m.patterns = append(m.patterns, pattern)
	m.ServeMux.HandleFunc(pattern, h)
}

// --- the token ---

// loadProToken reads the Work tool token from $DATA_DIR/secrets. Unlike the
// agents' token none is made by itself: without one nothing is accepted.
func (a *app) loadProToken() {
	b, err := os.ReadFile(filepath.Join(a.secretsDir, proTokenFile))
	if err != nil {
		return
	}
	if t := strings.TrimSpace(string(b)); len(t) >= 32 {
		a.proMu.Lock()
		a.proTok = t
		a.proMu.Unlock()
	}
}

// newProToken makes a fresh Work tool token; the old one stops working at once.
func (a *app) newProToken() error {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return err
	}
	t := base64.RawURLEncoding.EncodeToString(raw)
	if err := os.MkdirAll(a.secretsDir, 0o700); err != nil {
		return err
	}
	path := filepath.Join(a.secretsDir, proTokenFile)
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
	a.proMu.Lock()
	a.proTok = t
	a.proMu.Unlock()
	return nil
}

// revokeProToken takes the token away: the file goes and nothing is accepted
// until a new one is made.
func (a *app) revokeProToken() error {
	a.proMu.Lock()
	a.proTok = ""
	a.proMu.Unlock()
	if err := os.Remove(filepath.Join(a.secretsDir, proTokenFile)); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func (a *app) proTokenValue() string {
	a.proMu.RLock()
	defer a.proMu.RUnlock()
	return a.proTok
}

// proOK says whether the header carries the Work tool token.
func (a *app) proOK(header string) bool { return bearerMatches(header, a.proTokenValue()) }

// proView is the Work tool token section of the Connections page.
type proView struct {
	Token  string
	Has    bool
	Notice string
}

func (a *app) proViewFor(notice string) proView {
	t := a.proTokenValue()
	return proView{Token: t, Has: t != "", Notice: notice}
}

// proTokenNew and proTokenRevoke are the only ways to change the token: the
// signed-in page. Nothing under /api/ can.
func (a *app) proTokenNew(w http.ResponseWriter, r *http.Request) {
	if err := a.newProToken(); err != nil {
		appkit.Error("making a new Work tool token", err)
		http.Error(w, "The token could not be made. Try again.", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/connectors?n=pronew#worktool", http.StatusSeeOther)
}

func (a *app) proTokenRevoke(w http.ResponseWriter, r *http.Request) {
	if err := a.revokeProToken(); err != nil {
		appkit.Error("revoking the Work tool token", err)
		http.Error(w, "The token could not be revoked. Try again.", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/connectors?n=prorevoked#worktool", http.StatusSeeOther)
}

// --- the list ---

type privateEntry struct {
	Kind  string `json:"kind"` // waiting, todo or agenda
	Title string `json:"title"`
	Who   string `json:"who"`
	Why   string `json:"why"`
	Date  string `json:"date"`
	URL   string `json:"url"`

	at    int64
	group int
}

func (a *app) apiPrivateList(w http.ResponseWriter, r *http.Request) {
	run, err := readRun(r)
	if err != nil {
		answerErr(w, err)
		return
	}
	l, err := a.privateList(r.Context(), run)
	if err != nil {
		appkit.Error("making the private list", err)
		apiError(w, http.StatusInternalServerError, "the list could not be made")
		return
	}
	writeJSON(w, http.StatusOK, l)
}

func (a *app) baseURL() string { return strings.TrimRight(os.Getenv("BASE_URL"), "/") }

func (a *app) stamp(ts int64) string { return time.Unix(ts, 0).In(a.loc).Format(time.RFC3339) }

// cut shortens a line at a word, with an ellipsis.
func cut(s string, max int) string {
	s = oneLine(s, 100000)
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	r = r[:max]
	if i := strings.LastIndex(string(r), " "); i > max/2 {
		return strings.TrimRight(string(r[:i]), " ,;:.") + "…"
	}
	return strings.TrimRight(string(r), " ,;:.") + "…"
}

var proposalWords = map[string]string{
	"pending": "Claude proposes to put it in Pending",
	"resolve": "Claude proposes to skip it",
	"close":   "Claude proposes to close it",
	"todo":    "Claude proposes a to-do",
	"reply":   "Claude wrote a reply for you to read and send",
}

// privateList is the one ready list (SPEC 32): what waits for the owner first
// (oldest first), then his open to-dos, then his agenda of the next days.
func (a *app) privateList(ctx context.Context, run string) (map[string]any, error) {
	now := a.now()
	base := a.baseURL()
	var out []privateEntry
	seen := map[int64]bool{}

	rowWho := func(id int64) string {
		if row, ok := a.store.ListRowByID(ctx, id); ok {
			if row.SenderName != "" {
				return oneLine(row.SenderName, 60)
			}
			return oneLine(row.Sender, 60)
		}
		return ""
	}
	itemURL := func(id int64) string { return base + "/items/" + strconv.FormatInt(id, 10) }

	// Proposals waiting for the owner's Approve.
	ps, items, err := a.store.ProposalsWaiting(ctx, 500)
	if err != nil {
		return nil, err
	}
	for _, p := range ps {
		it, ok := items[p.ItemID]
		if !ok || seen[it.ID] || it.TestRun != run || it.Hidden || it.Stage == stageHistory {
			continue
		}
		seen[it.ID] = true
		words := proposalWords[p.Action]
		if words == "" {
			words = "Claude proposes a change"
		}
		why := words
		if p.Action == "todo" && p.Text != "" {
			why += " (" + cut(p.Text, 80) + ")"
		}
		if p.Reason != "" {
			why += ": " + cut(p.Reason, 140)
		}
		why = strings.TrimRight(why, ".") + ". Open it in ONE and press Approve, or dismiss it."
		out = append(out, privateEntry{Kind: "waiting", Title: cut(it.Subject, privateTitleMax), Who: rowWho(it.ID), Why: why,
			at: p.At, URL: itemURL(it.ID)})
	}
	aps, err := a.store.PrivateAgendaProposals(ctx, 100, run)
	if err != nil {
		return nil, err
	}
	for _, p := range aps {
		title := "A change to your to-dos"
		if p.Text != "" {
			title = "To-do: " + cut(p.Text, 80)
		}
		why := "Claude proposes a change to the agenda"
		if p.Reason != "" {
			why += ": " + cut(p.Reason, 140)
		}
		why = strings.TrimRight(why, ".") + ". Open the agenda in ONE and press Approve, or dismiss it."
		out = append(out, privateEntry{Kind: "waiting", Title: title, Who: "Claude", Why: why, at: p.At, URL: base + "/agenda"})
	}

	// Items that Claude has looked at and said something about, still in New
	// or In progress: what Claude wrote is the line.
	cands, err := a.store.PrivateWaitingItems(ctx, run)
	if err != nil {
		return nil, err
	}
	for _, it := range cands {
		if seen[it.ID] {
			continue
		}
		evs, err := a.store.EntriesOfItem(ctx, it.ID, "commented")
		if err != nil {
			return nil, err
		}
		var best repo.Entry
		var bestText string
		for _, e := range evs {
			if e.Actor != "claude" {
				continue
			}
			var d struct {
				Text string `json:"text"`
				Kind string `json:"kind"`
			}
			if json.Unmarshal([]byte(e.Data), &d) != nil || strings.TrimSpace(d.Text) == "" {
				continue
			}
			if bestText == "" || d.Kind == "summary" || best.Type == "" {
				best, bestText = e, d.Text
			}
		}
		if bestText == "" {
			continue
		}
		seen[it.ID] = true
		out = append(out, privateEntry{Kind: "waiting", Title: cut(it.Subject, privateTitleMax), Who: rowWho(it.ID), Why: cut(bestText, privateWhyMax),
			at: best.At, URL: itemURL(it.ID)})
	}
	for i := range out {
		out[i].group = 0
	}

	// Open to-dos.
	todos, err := a.store.PrivateOpenTodos(ctx, run)
	if err != nil {
		return nil, err
	}
	for _, t := range todos {
		e := privateEntry{Kind: "todo", Title: cut(t.Title, privateTitleMax), Who: "Your to-do", at: t.CreatedAt, group: 1, URL: base + "/agenda"}
		why := "Your open to-do: " + cut(t.Title, 140)
		if t.ItemID != 0 {
			if it, ok := a.store.ItemByID(ctx, t.ItemID); ok {
				why += ". It came from the item \"" + cut(it.Subject, 70) + "\""
				e.URL = itemURL(it.ID)
			}
		}
		if due := a.dueOf(t); due != "" {
			why += ". Due " + due
		}
		e.Why = strings.TrimRight(why, ".") + "."
		out = append(out, e)
	}

	// The agenda from now to the end of the seventh day ahead (Prague time).
	today := now.In(a.loc)
	end := time.Date(today.Year(), today.Month(), today.Day()+privateDays+1, 0, 0, 0, 0, a.loc)
	evs, err := a.store.PrivateEvents(ctx, time.Date(today.Year(), today.Month(), today.Day(), 0, 0, 0, 0, a.loc).Unix(), end.Unix(), run)
	if err != nil {
		return nil, err
	}
	threads, _ := a.store.MeetingThreads(ctx)
	dup := map[string]bool{}
	for _, ev := range evs {
		if ev.AllDay {
			if ev.EndDate != "" && ev.EndDate <= today.Format(dateLayout) {
				continue
			}
		} else if ev.EndAt > ev.StartAt && ev.EndAt <= now.Unix() || ev.EndAt <= ev.StartAt && ev.StartAt < now.Unix() {
			continue
		}
		key := repo.MeetingKey(ev) + "|" + strconv.FormatInt(ev.StartAt, 10) + "|" + ev.Title
		if dup[key] {
			continue
		}
		dup[key] = true
		clock, until := a.eventTime(ev)
		day := time.Unix(ev.StartAt, 0).In(a.loc)
		when := day.Format("Mon 2 Jan") + ", " + clock
		if until != "" {
			when += " " + until
		}
		why := when
		if ev.Place != "" {
			why += ", " + cut(ev.Place, 80)
		}
		e := privateEntry{Kind: "agenda", Title: cut(ev.Title, privateTitleMax), Who: oneLine(ev.Calendar, 60), Why: why + ".", at: ev.StartAt, group: 2, URL: base + "/agenda"}
		if t, ok := threads[repo.MeetingKey(ev)]; ok {
			e.URL = itemURL(t[0])
		}
		out = append(out, e)
	}

	sort.SliceStable(out, func(i, j int) bool {
		if out[i].group != out[j].group {
			return out[i].group < out[j].group
		}
		return out[i].at < out[j].at
	})
	more := 0
	if len(out) > privateListMax {
		more = len(out) - privateListMax
		out = out[:privateListMax]
	}
	entries := make([]privateEntry, len(out))
	for i, e := range out {
		e.Date = a.stamp(e.at)
		entries[i] = e
	}
	res := map[string]any{"private": true, "made_at": a.stamp(now.Unix()), "note": privateNote, "entries": entries}
	if more > 0 {
		res["more"] = more
	}
	return res, nil
}

// --- the tool ---

var proTools = []mcpTool{{
	Name:        proToolName,
	Title:       "The owner's private list",
	Description: "The owner's private list from ONE, read only: what waits for him (proposals waiting for his Approve, items Claude says need him), his open to-dos and his agenda of today and the next 7 days. Each entry has a kind (waiting, todo or agenda), a short title, who or what it is, one line why it matters and what to do, its date and its address in ONE. The list is private (private is true), made by ONE and its own Claude, and holds no message texts and no files. It is the only thing the Work tool token can read.",
	InputSchema: schema(map[string]any{"run": runT}),
	Annotations: hints("The owner's private list", true, false, true),
}}

func init() { mcpTools = append(mcpTools, proTools...) }

func (a *app) callProTool(ctx context.Context, name string) (string, bool, bool) {
	if name != proToolName {
		return "", false, false
	}
	l, err := a.privateList(ctx, repo.RunOf(ctx))
	if err != nil {
		appkit.Error("making the private list", err)
		return "The list could not be made.", true, true
	}
	b, _ := json.Marshal(l)
	return string(b), false, true
}
