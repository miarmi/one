package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"lab/apps/one/repo"
)

// Tests of Work tool's private list and its token (SPEC 32). All data is
// invented.

const marker = "ZQXMARKER7731"

const wrongBody = `{"ok": false, "error": "wrong token"}`

func (e *agentEnv) proToken() string {
	e.t.Helper()
	if rec := post(e.h, "/connectors/worktool-token/new", nil); rec.Code != http.StatusSeeOther {
		e.t.Fatalf("New token: %d", rec.Code)
	}
	return e.a.proTokenValue()
}

func (e *agentEnv) privateWith(token string) *httptest.ResponseRecorder {
	return e.call("GET", privateListPath, token, nil)
}

func (e *agentEnv) privateList() (map[string]any, []map[string]any) {
	e.t.Helper()
	rec := e.privateWith(e.a.agentTokenValue())
	if rec.Code != 200 {
		e.t.Fatalf("the list: %d %s", rec.Code, rec.Body.String())
	}
	var m map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		e.t.Fatal(err)
	}
	var entries []map[string]any
	for _, x := range m["entries"].([]any) {
		entries = append(entries, x.(map[string]any))
	}
	return m, entries
}

// --- the token only opens the list ---

func TestWorkToolTokenOpensOnlyThePrivateListOnEveryAddress(t *testing.T) {
	e := newAgentEnv(t)
	tok := e.proToken()
	if tok == e.a.agentTokenValue() || tok == e.a.bridgeTokenValue() {
		t.Fatal("the Work tool token is not its own")
	}
	if rec := e.privateWith(tok); rec.Code != 200 {
		t.Fatalf("the Work tool token is refused on the list: %d %s", rec.Code, rec.Body.String())
	}
	if len(e.a.apiPatterns) < 40 || len(e.a.bridgePatterns) < 5 {
		t.Fatalf("the test sees only %d and %d addresses, so it proves nothing", len(e.a.apiPatterns), len(e.a.bridgePatterns))
	}
	subst := strings.NewReplacer("{id}", "Q-0001", "{key}", "k1", "{run}", "e2e-abcdefgh")
	var checked int
	for _, pat := range append(append([]string{}, e.a.apiPatterns...), e.a.bridgePatterns...) {
		method, path := "GET", pat
		if i := strings.Index(pat, " "); i > 0 {
			method, path = pat[:i], pat[i+1:]
		}
		path = subst.Replace(path)
		if strings.HasSuffix(path, "/") {
			path += "x"
		}
		if method == "GET" && path == privateListPath {
			continue
		}
		checked++
		for _, body := range []any{nil, map[string]any{"text": "hello", "action": "close"}} {
			rec := e.call(method, path, tok, body)
			if rec.Code != http.StatusUnauthorized || strings.TrimSpace(rec.Body.String()) != wrongBody {
				t.Errorf("%s %s with the Work tool token: %d %q", method, path, rec.Code, rec.Body.String())
			}
			wrong := e.call(method, path, "not-the-token", body)
			if wrong.Code != rec.Code || wrong.Body.String() != rec.Body.String() {
				t.Errorf("%s %s answers the Work tool token differently from a wrong token", method, path)
			}
		}
	}
	if checked < 45 {
		t.Fatalf("only %d addresses were tried", checked)
	}
	// Other methods on the list's own address, and addresses no route has.
	for _, c := range [][2]string{{"POST", privateListPath}, {"PUT", privateListPath}, {"DELETE", privateListPath}, {"GET", privateListPath + "/x"}, {"GET", "/api/approve"}, {"GET", "/api/anything"}, {"GET", "/api/token"}, {"GET", "/api/mcp/x"}} {
		rec := e.call(c[0], c[1], tok, nil)
		if rec.Code != http.StatusUnauthorized || strings.TrimSpace(rec.Body.String()) != wrongBody {
			t.Errorf("%s %s with the Work tool token: %d %q", c[0], c[1], rec.Code, rec.Body.String())
		}
	}
	// The agents' token may read the list too.
	if rec := e.privateWith(e.a.agentTokenValue()); rec.Code != 200 {
		t.Errorf("the agents' token is refused on the list: %d", rec.Code)
	}
	// And the other tokens are still what they were.
	if rec := e.call("GET", "/api/queue", e.a.agentTokenValue(), nil); rec.Code != 200 {
		t.Errorf("the agents' token: %d", rec.Code)
	}
}

func TestWorkToolTokenOpensOnlyThePrivateListToolOnEveryTool(t *testing.T) {
	e := newAgentEnv(t)
	tok := e.proToken()
	tools := mcpToolList()
	if len(tools) < 30 {
		t.Fatalf("the test sees only %d tools", len(tools))
	}
	var found bool
	for _, tl := range tools {
		args := map[string]any{"id": "Q-0001", "text": "x"}
		res := e.rpc(tok, "tools/call", map[string]any{"name": tl.Name, "arguments": args})["result"].(map[string]any)
		text := res["content"].([]any)[0].(map[string]any)["text"].(string)
		if tl.Name == proToolName {
			found = true
			if res["isError"] == true || !strings.Contains(text, `"private":true`) {
				t.Errorf("the Work tool token is refused on %s: %s", tl.Name, text)
			}
			if ro, _ := tl.Annotations["readOnlyHint"].(bool); !ro {
				t.Errorf("%s is not marked read only", tl.Name)
			}
			continue
		}
		if res["isError"] != true || text != wrongBody {
			t.Errorf("tool %s with the Work tool token: %v %q", tl.Name, res["isError"], text)
		}
	}
	if !found {
		t.Fatal("there is no private_list tool")
	}
	for _, name := range []string{"no_such_tool", "approve", "get_token"} {
		res := e.rpc(tok, "tools/call", map[string]any{"name": name, "arguments": map[string]any{}})
		if res["error"] != nil || res["result"].(map[string]any)["content"].([]any)[0].(map[string]any)["text"] != wrongBody {
			t.Errorf("unknown tool %s with the Work tool token: %v", name, res)
		}
	}
	// The agents' token reads the list through the tool too.
	text, isErr := e.tool(proToolName, nil)
	if isErr || !strings.Contains(text, `"private":true`) {
		t.Errorf("the agents' token on private_list: %v %s", isErr, text)
	}
	// Nothing in the list of tools shows a token.
	rec := httptest.NewRecorder()
	body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/list"})
	req := httptest.NewRequest("POST", "/api/mcp", strings.NewReader(string(body)))
	e.h.ServeHTTP(rec, req)
	if strings.Contains(rec.Body.String(), tok) {
		t.Error("the list of tools shows the token")
	}
}

// --- making and revoking the token ---

func TestWorkToolTokenNewReplacesAndRevokeEndsItAtOnce(t *testing.T) {
	e := newAgentEnv(t)
	if e.a.proTokenValue() != "" {
		t.Fatal("a Work tool token exists before the owner made one")
	}
	if rec := e.privateWith("anything"); rec.Code != 401 {
		t.Errorf("no token on file, yet %d", rec.Code)
	}
	if rec := e.privateWith(""); rec.Code != 401 {
		t.Errorf("no header: %d", rec.Code)
	}
	first := e.proToken()
	if len(first) < 32 {
		t.Fatal("no token was made")
	}
	if b, err := os.ReadFile(e.a.secretsDir + "/" + proTokenFile); err != nil || strings.TrimSpace(string(b)) != first {
		t.Errorf("the token is not kept in the secrets folder: %v", err)
	}
	if st, err := os.Stat(e.a.secretsDir + "/" + proTokenFile); err != nil || st.Mode().Perm() != 0o600 {
		t.Errorf("the token file is not private: %v %v", st, err)
	}
	// It is kept over a restart.
	e.a.proMu.Lock()
	e.a.proTok = ""
	e.a.proMu.Unlock()
	e.a.loadProToken()
	if e.a.proTokenValue() != first {
		t.Error("the token is not read back")
	}

	second := e.proToken()
	if second == first {
		t.Fatal("New token made the same token")
	}
	if rec := e.privateWith(first); rec.Code != 401 || strings.TrimSpace(rec.Body.String()) != wrongBody {
		t.Errorf("the old token still works: %d", rec.Code)
	}
	if res := e.rpc(first, "tools/call", map[string]any{"name": proToolName, "arguments": map[string]any{}})["result"].(map[string]any); res["isError"] != true {
		t.Error("the old token still works on the tool")
	}
	if rec := e.privateWith(second); rec.Code != 200 {
		t.Errorf("the new token is refused: %d", rec.Code)
	}

	if rec := post(e.h, "/connectors/worktool-token/revoke", nil); rec.Code != http.StatusSeeOther {
		t.Fatalf("Revoke: %d", rec.Code)
	}
	if rec := e.privateWith(second); rec.Code != 401 || strings.TrimSpace(rec.Body.String()) != wrongBody {
		t.Errorf("a revoked token still works: %d", rec.Code)
	}
	if res := e.rpc(second, "tools/call", map[string]any{"name": proToolName, "arguments": map[string]any{}})["result"].(map[string]any); res["isError"] != true {
		t.Error("a revoked token still works on the tool")
	}
	if _, err := os.Stat(e.a.secretsDir + "/" + proTokenFile); !os.IsNotExist(err) {
		t.Error("the token file is still there after Revoke")
	}
	e.a.loadProToken()
	if e.a.proTokenValue() != "" {
		t.Error("a revoked token came back")
	}
	// The agents' token was never touched.
	if rec := e.call("GET", "/api/queue", e.a.agentTokenValue(), nil); rec.Code != 200 {
		t.Errorf("the agents' token after Revoke: %d", rec.Code)
	}
	// Revoking again is harmless.
	if rec := post(e.h, "/connectors/worktool-token/revoke", nil); rec.Code != http.StatusSeeOther {
		t.Errorf("Revoke twice: %d", rec.Code)
	}
}

func TestWorkToolTokenIsOnlyOnTheConnectionsPageAndNeverInTheLog(t *testing.T) {
	e := newAgentEnv(t)
	page := get(e.h, "/connectors").Body.String()
	i := strings.Index(page, `id="worktool"`)
	if i < 0 {
		t.Fatal("no Work tool section")
	}
	section := page[i:]
	section = section[:strings.Index(section, "</section>")]
	for _, want := range []string{"Work tool token", ">Copy<", ">New token<", ">Revoke<"} {
		if !strings.Contains(section, want) {
			t.Errorf("the section lacks %q", want)
		}
	}
	tok := e.proToken()
	page = get(e.h, "/connectors").Body.String()
	if strings.Count(page, tok) != 1 {
		t.Fatalf("the page shows the Work tool token %d times", strings.Count(page, tok))
	}
	i = strings.Index(page, `id="worktool"`)
	if !strings.Contains(page[i:i+strings.Index(page[i:], "</section>")], tok) {
		t.Error("the token is not in its own section")
	}
	if strings.Contains(page, e.a.agentTokenValue()) && strings.Count(page, e.a.agentTokenValue()) != 1 {
		t.Error("the agents' token shows more than once")
	}
	// Use it every way, rightly and wrongly, then look at the log and at the answers.
	e.privateWith(tok)
	e.call("GET", "/api/queue", tok, nil)
	e.call("POST", "/api/items/Q-0001/comments", tok, map[string]any{"text": "x"})
	e.call("GET", "/api/bridge/alive", tok, nil)
	e.rpc(tok, "tools/call", map[string]any{"name": "get_item", "arguments": map[string]any{"id": 1}})
	for _, p := range []string{"/", "/agenda", "/healthz", "/manifest.webmanifest"} {
		if strings.Contains(get(e.h, p).Body.String(), tok) {
			t.Errorf("page %s shows the token", p)
		}
	}
	for _, p := range []string{"/api/queue", "/api/set-up", "/api/service-status", "/api/log", privateListPath} {
		if strings.Contains(e.api("GET", p, nil).Body.String(), tok) {
			t.Errorf("%s shows the token", p)
		}
	}
	if text, _ := e.tool("get_log", map[string]any{}); strings.Contains(text, tok) {
		t.Error("get_log shows the token")
	}
	log, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(log), tok) {
		t.Error("a log line holds the Work tool token")
	}
}

// --- the list ---

// privateData makes the data of the list tests: what must be on the list, and
// what must stay off it.
func (e *agentEnv) privateData(t *testing.T) map[string]repo.Item {
	t.Helper()
	t.Setenv("BASE_URL", "https://one.example.test")
	out := map[string]repo.Item{}
	// A: Claude wrote a summary, the item waits for the owner (Claude says it needs him).
	e.now = time.Date(2026, 11, 19, 8, 0, 0, 0, time.UTC)
	a := e.newItem(stageNew, true, "Parcel held at the depot", strings.Repeat("Your parcel waits. ", 25)+marker+" owner secret")
	if m := e.post("/api/items/"+ref(a)+"/comments", map[string]any{"text": "The depot holds a parcel for the owner until Friday; he must collect it or ask for a new delivery.", "kind": "summary"}); m["ok"] != true {
		t.Fatalf("summary: %v", m)
	}
	out["says"] = e.item(a.ID)
	// B: a proposal waits for Approve; made earlier than A's summary.
	e.now = time.Date(2026, 11, 18, 9, 0, 0, 0, time.UTC)
	b := e.newItem(stageNew, true, "School trip form", strings.Repeat("Please sign the form. ", 12))
	e.propose(b, "close", "The form was already signed last week.")
	out["proposal"] = e.item(b.ID)
	// C: a New item that Claude has not looked at yet: not on the list.
	c := e.newItem(stageNew, true, "Waiting for Claude", "")
	out["unseen"] = c
	// D: a history item with a Claude comment: not on the list.
	e.now = time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC)
	d := e.newItem(stageHistory, false, "Old history thread", "")
	e.commentDirect(d, "claude", "History summary that must stay out.")
	out["history"] = d
	// E: an item made by a test run, with a Claude comment and a proposal: not on the list.
	rctx := repo.WithRun(e.ctx, "e2e-abcdefgh")
	keep := e.ctx
	e.ctx = rctx
	f := e.seed(stageNew, e.mail("Test run subject", 1))
	e.ctx = keep
	e.commentDirect(f, "claude", "Test run comment that must stay out.")
	out["testrun"] = f
	// To-dos: one open, one ticked off, one made from item A.
	e.addTodo("Pay the water bill")
	done := e.addTodo("Already done thing")
	if _, err := e.a.store.SetAgendaDone(e.ctx, done.ID, e.now.Unix()); err != nil {
		t.Fatal(err)
	}
	if _, err := e.a.todoFromItem(e.ctx, a.ID); err != nil {
		t.Fatal(err)
	}
	// The agenda: now is Thursday 19 Nov 2026 10:00 UTC (11:00 in Prague).
	loc := e.a.loc
	ev := func(id, title, place string, start time.Time, status string) {
		err := e.a.store.Update(e.ctx, func(tx *repo.Tx) error {
			_, err := tx.InsertAgenda(e.ctx, repo.Agenda{Kind: "event", Source: srcICloud, SourceID: id, CalKey: "cal1", Calendar: "Family", UID: id, Title: title, Place: place,
				StartAt: start.Unix(), EndAt: start.Add(time.Hour).Unix(), Status: status, State: "open", Version: 1, Hash: id, CreatedAt: 1, UpdatedAt: 1})
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	ev("e1", "Dentist", "Dr Novak, Main Street", time.Date(2026, 11, 20, 14, 30, 0, 0, loc), "confirmed")
	ev("e2", "Last evening of the week", "", time.Date(2026, 11, 26, 23, 0, 0, 0, loc), "confirmed")
	ev("e3", "Too far ahead", "", time.Date(2026, 11, 27, 0, 30, 0, 0, loc), "confirmed")
	ev("e4", "Already over", "", time.Date(2026, 11, 19, 8, 0, 0, 0, loc), "confirmed")
	ev("e5", "Cancelled one", "", time.Date(2026, 11, 21, 9, 0, 0, 0, loc), "cancelled")
	ev("e6", "Later today", "", time.Date(2026, 11, 19, 17, 0, 0, 0, loc), "confirmed")
	return out
}

// commentDirect writes a comment event as the given actor, the way the
// review would have left it.
func (e *agentEnv) commentDirect(it repo.Item, actor, text string) {
	e.t.Helper()
	err := e.a.store.Update(e.ctx, func(tx *repo.Tx) error {
		return tx.InsertEvent(e.ctx, e.now.Unix(), it.ID, actor, "commented", map[string]any{"text": text, "kind": "summary"})
	})
	if err != nil {
		e.t.Fatal(err)
	}
}

func TestPrivateListHoldsWhatWaitsTheTodosAndTheAgendaInOrder(t *testing.T) {
	e := newAgentEnv(t)
	items := e.privateData(t)
	e.now = time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC)
	top, entries := e.privateList()
	if top["private"] != true {
		t.Error("the list is not marked private")
	}
	if _, err := time.Parse(time.RFC3339, top["made_at"].(string)); err != nil {
		t.Errorf("made_at: %v", err)
	}
	if n, _ := top["note"].(string); !strings.Contains(n, "Private") || len(n) > 220 {
		t.Errorf("note: %q", n)
	}
	if _, has := top["more"]; has {
		t.Error("more is given for a short list")
	}
	var kinds, titles []string
	for _, en := range entries {
		if len(en) != 6 {
			t.Errorf("an entry has fields %v", en)
		}
		for _, k := range []string{"kind", "title", "who", "why", "date", "url"} {
			if s, _ := en[k].(string); s == "" && k != "who" {
				t.Errorf("entry %v lacks %s", en["title"], k)
			}
		}
		if _, err := time.Parse(time.RFC3339, en["date"].(string)); err != nil {
			t.Errorf("date %q: %v", en["date"], err)
		}
		if !strings.HasPrefix(en["url"].(string), "https://one.example.test/") {
			t.Errorf("url %q", en["url"])
		}
		kinds = append(kinds, en["kind"].(string))
		titles = append(titles, en["title"].(string))
	}
	want := []string{"School trip form", "Parcel held at the depot", "Pay the water bill", "Parcel held at the depot", "Later today", "Dentist", "Last evening of the week"}
	if strings.Join(titles, "|") != strings.Join(want, "|") {
		t.Fatalf("the list is\n%v\nwant\n%v", titles, want)
	}
	if strings.Join(kinds, ",") != "waiting,waiting,todo,todo,agenda,agenda,agenda" {
		t.Errorf("kinds: %v", kinds)
	}
	// Waiting first, oldest first: the proposal (18 Nov) before the summary (19 Nov).
	if entries[0]["date"].(string) >= entries[1]["date"].(string) {
		t.Errorf("waiting is not oldest first: %v %v", entries[0]["date"], entries[1]["date"])
	}
	prop, says := entries[0], entries[1]
	if !strings.Contains(prop["why"].(string), "already signed") || !strings.Contains(prop["why"].(string), "Approve") {
		t.Errorf("the proposal's why: %q", prop["why"])
	}
	if !strings.Contains(says["why"].(string), "collect it") {
		t.Errorf("the summary's why: %q", says["why"])
	}
	if who := says["who"].(string); who != "Alice Example" {
		t.Errorf("who: %q", who)
	}
	if says["url"] != fmt.Sprintf("https://one.example.test/items/%d", items["says"].ID) || prop["url"] != fmt.Sprintf("https://one.example.test/items/%d", items["proposal"].ID) {
		t.Errorf("urls: %v %v", says["url"], prop["url"])
	}
	// The to-do made from item A names the item it came from.
	fromItem := entries[3]
	if !strings.Contains(fromItem["why"].(string), "came from the item") || fromItem["url"] != says["url"] {
		t.Errorf("the to-do from an item: %v", fromItem)
	}
	// The agenda: time and place, by time; the entry of a calendar goes to the agenda page.
	dentist := entries[5]
	if !strings.Contains(dentist["why"].(string), "14:30") || !strings.Contains(dentist["why"].(string), "Dr Novak") || dentist["url"] != "https://one.example.test/agenda" || dentist["who"] != "Family" {
		t.Errorf("the dentist: %v", dentist)
	}
	if dentist["date"] != "2026-11-20T14:30:00+01:00" {
		t.Errorf("the dentist's date: %v", dentist["date"])
	}
	// Never on the list: unseen, history, test run, ticked off, out of range.
	whole, _ := json.Marshal(top)
	for _, no := range []string{"Waiting for Claude", "Old history", "History summary", "Test run", "Already done", "Too far ahead", "Already over", "Cancelled one"} {
		if strings.Contains(string(whole), no) {
			t.Errorf("the list holds %q", no)
		}
	}
}

func TestPrivateListHoldsNoMessageTextFileNamesOrBodies(t *testing.T) {
	e := newAgentEnv(t)
	items := e.privateData(t)
	e.now = time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC)
	// A file with a name nobody should see, on the item that is on the list.
	msgs, _, err := e.a.store.MessagesSince(e.ctx, items["says"].ID, 0)
	if err != nil || len(msgs) == 0 {
		t.Fatal("no message", err)
	}
	e.saveFile(items["says"], msgs[0].SourceID, "ZQXFILENAME-contract.pdf", "application/pdf", "attachment", []byte("%PDF- "+marker))
	e.propose(items["says"], "close", "Nothing more to do here.")
	for _, body := range []string{
		e.privateWith(e.a.agentTokenValue()).Body.String(),
		e.rpcText(e.a.agentTokenValue()),
	} {
		for _, secret := range []string{marker, "ZQXFILENAME", "owner secret", "Your parcel waits", "alice@example.test"} {
			if strings.Contains(body, secret) {
				t.Errorf("the list holds %q", secret)
			}
		}
	}
	tok := e.proToken()
	if body := e.privateWith(tok).Body.String(); strings.Contains(body, marker) || strings.Contains(body, "ZQXFILENAME") {
		t.Error("the list the Work tool token gets holds message or file text")
	}
}

func (e *agentEnv) rpcText(token string) string {
	e.t.Helper()
	res := e.rpc(token, "tools/call", map[string]any{"name": proToolName, "arguments": map[string]any{}})["result"].(map[string]any)
	return res["content"].([]any)[0].(map[string]any)["text"].(string)
}

func TestPrivateListToolGivesTheSameListAsTheAddress(t *testing.T) {
	e := newAgentEnv(t)
	e.privateData(t)
	e.now = time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC)
	tok := e.proToken()
	var viaTool, viaRest map[string]any
	json.Unmarshal([]byte(e.rpcText(tok)), &viaTool)
	json.Unmarshal(e.privateWith(tok).Body.Bytes(), &viaRest)
	a, _ := json.Marshal(viaTool)
	b, _ := json.Marshal(viaRest)
	if string(a) != string(b) || len(viaTool["entries"].([]any)) != 7 {
		t.Errorf("the tool and the address differ:\n%s\n%s", a, b)
	}
}

func TestPrivateListIsCappedWithACountOfTheRest(t *testing.T) {
	e := newAgentEnv(t)
	for i := 0; i < privateListMax+10; i++ {
		e.addTodo(fmt.Sprintf("Thing number %d", i))
	}
	top, entries := e.privateList()
	if len(entries) != privateListMax || top["more"] != float64(10) {
		t.Errorf("%d entries, more %v", len(entries), top["more"])
	}
}

func TestPrivateListIsEmptyWhenNothingIsThere(t *testing.T) {
	e := newAgentEnv(t)
	top, entries := e.privateList()
	if len(entries) != 0 || top["private"] != true {
		t.Errorf("%v", top)
	}
	if !strings.Contains(e.privateWith(e.a.agentTokenValue()).Body.String(), `"entries":[]`) {
		t.Error("an empty list is not []")
	}
}

// --- the list of a test run ---

func TestPrivateListOfATestRunHoldsOnlyThatRunsRecords(t *testing.T) {
	e := newAgentEnv(t)
	e.privateData(t)
	e.now = time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC)
	const run = "e2e-runlist01"
	rctx := repo.WithRun(e.ctx, run)
	keep := e.ctx
	e.ctx = rctx
	a := e.seed(stageNew, e.mail("Run item with comment", 1))
	b := e.seed(stageNew, e.mail("Run item with proposal", 1))
	e.commentDirect(a, "claude", "Run comment line.")
	err := e.a.store.Update(rctx, func(tx *repo.Tx) error {
		if _, err := tx.InsertProposal(rctx, repo.Proposal{ItemID: b.ID, Action: "close", Reason: "Run reason.", By: "claude", At: e.now.Unix(), BaseStage: stageNew}); err != nil {
			return err
		}
		_, err := tx.InsertAgenda(rctx, repo.Agenda{Kind: "event", Source: srcICloud, SourceID: "r1", CalKey: "calr", Calendar: "Run cal", UID: "r1", Title: "Run meeting",
			StartAt: time.Date(2026, 11, 20, 9, 0, 0, 0, e.a.loc).Unix(), EndAt: time.Date(2026, 11, 20, 10, 0, 0, 0, e.a.loc).Unix(), Status: "confirmed", State: "open", Version: 1, Hash: "r1", CreatedAt: 1, UpdatedAt: 1})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.a.addTodo(rctx, "Run to-do"); err != nil {
		t.Fatal(err)
	}
	e.ctx = keep

	titles := func(body []byte) string {
		var m struct {
			Entries []struct{ Title string } `json:"entries"`
		}
		if err := json.Unmarshal(body, &m); err != nil {
			t.Fatalf("%v %s", err, body)
		}
		var out []string
		for _, en := range m.Entries {
			out = append(out, en.Title)
		}
		return strings.Join(out, "|")
	}
	runTitles := "Run item with proposal|Run item with comment|Run to-do|Run meeting"
	plain := e.privateWith(e.a.agentTokenValue())
	if strings.Contains(titles(plain.Body.Bytes()), "Run ") {
		t.Errorf("a run's record is in the plain list: %s", titles(plain.Body.Bytes()))
	}
	tok := e.proToken()
	for name, rec := range map[string]*httptest.ResponseRecorder{
		"query, agents' token":   e.call("GET", privateListPath+"?run="+run, e.a.agentTokenValue(), nil),
		"query, Work tool token": e.call("GET", privateListPath+"?run="+run, tok, nil),
	} {
		if rec.Code != 200 || titles(rec.Body.Bytes()) != runTitles {
			t.Errorf("%s: %d %s", name, rec.Code, titles(rec.Body.Bytes()))
		}
	}
	req := httptest.NewRequest("GET", privateListPath, nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set(runHeader, run)
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	if titles(rec.Body.Bytes()) != runTitles {
		t.Errorf("header: %s", titles(rec.Body.Bytes()))
	}
	for _, token := range []string{e.a.agentTokenValue(), tok} {
		res := e.rpc(token, "tools/call", map[string]any{"name": proToolName, "arguments": map[string]any{"run": run}})["result"].(map[string]any)
		if got := titles([]byte(res["content"].([]any)[0].(map[string]any)["text"].(string))); got != runTitles {
			t.Errorf("tool with run: %s", got)
		}
	}
	// None of the owner's records, and no other run's, in the run's list.
	body := e.call("GET", privateListPath+"?run="+run, tok, nil).Body.String()
	for _, no := range []string{"Parcel", "School trip", "water bill", "Dentist", "Test run subject", "Test run comment"} {
		if strings.Contains(body, no) {
			t.Errorf("the run's list holds %q", no)
		}
	}
	if rec := e.call("GET", privateListPath+"?run=e2e-zzzzzzzz", tok, nil); titles(rec.Body.Bytes()) != "" {
		t.Error("another run's list is not empty")
	}
	if rec := e.call("GET", privateListPath+"?run=nonsense", tok, nil); rec.Code != 400 {
		t.Errorf("a bad run name: %d", rec.Code)
	}
}
