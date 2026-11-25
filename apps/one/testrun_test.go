package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"lab/apps/one/repo"
)

// Tests of the test runs (SPEC 16, 30): what an automated test makes under a
// run name is marked, and removing a run takes every record of it and nothing
// else. Everything here is invented; nothing is real mail, a real chat or a
// real person.

const (
	runA = "e2e-runaaaa001"
	runB = "e2e-runbbbb002"
)

type runEnv struct {
	*chatEnv
	db *sql.DB // a second connection to the same file, to look at the tables as they are
}

func newRunEnv(t *testing.T) *runEnv {
	t.Helper()
	e := &runEnv{chatEnv: newChatEnv(t)}
	db, err := sql.Open("sqlite3", "file:"+filepath.Join(e.a.dataDir, "one.db")+"?_busy_timeout=5000")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	e.db = db
	return e
}

// apiRun is an agent's call, naming a run in the header when run is not "".
func (e *runEnv) apiRun(run, method, path string, body any) (int, map[string]any) {
	e.t.Helper()
	var rd *bytes.Reader
	if body == nil {
		rd = bytes.NewReader(nil)
	} else {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req := httptest.NewRequest(method, path, rd)
	req.Header.Set("Authorization", "Bearer "+e.a.agentTokenValue())
	req.Header.Set("Content-Type", "application/json")
	if run != "" {
		req.Header.Set("X-Test-Run", run)
	}
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	var m map[string]any
	json.Unmarshal(rec.Body.Bytes(), &m)
	if m == nil {
		m = map[string]any{}
	}
	return rec.Code, m
}

// batchRun is the bridge's batch with a run name in the field.
func (e *runEnv) batchRun(run string, chats ...bridgeChat) (int, map[string]any) {
	e.t.Helper()
	rec := e.asBridge("POST", "/api/bridge/batch", bridgeBatch{Run: run, Chats: chats})
	return rec.Code, answerOf(e.t, rec)
}

func (e *runEnv) mustBatchRun(run string, chats ...bridgeChat) map[string]any {
	e.t.Helper()
	code, m := e.batchRun(run, chats...)
	if code != 200 {
		e.t.Fatalf("batch under %s: %d %v", run, code, m)
	}
	return m
}

func (e *runEnv) upload(network, chatID, msgID, att, content string) {
	e.t.Helper()
	u := fmt.Sprintf("/api/bridge/files?network=%s&chat_id=%s&message_id=%s&attachment=%s", network, chatID, msgID, att)
	req := httptest.NewRequest("PUT", u, strings.NewReader(content))
	req.Header.Set("Authorization", "Bearer "+e.a.bridgeTokenValue())
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		e.t.Fatalf("upload: %d %s", rec.Code, rec.Body.String())
	}
}

// --- looking at the tables ---

var neverMarked = map[string]bool{"skip_rules": true, "same_mail": true, "review_jobs": true, "claude_usage": true, "night_checks": true, "night_items": true, "settings": true, "source_pictures": true, "tests": true, "sync_runs": true, "finder": true, "finder_content": true, "finder_segments": true,
	"finder_segdir": true, "finder_docsize": true, "finder_stat": true, "sqlite_sequence": true}

// dump is every row of one table that carries a mark, as sorted lines.
func (e *runEnv) dump(table, run string) []string {
	e.t.Helper()
	rows, err := e.db.Query("SELECT * FROM "+table+" WHERE test_run = ?", run)
	if err != nil {
		e.t.Fatal(table, err)
	}
	defer rows.Close()
	cols, _ := rows.Columns()
	var out []string
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			e.t.Fatal(err)
		}
		var b strings.Builder
		for i, v := range vals {
			if bs, ok := v.([]byte); ok {
				v = string(bs)
			}
			fmt.Fprintf(&b, "%s=%v|", cols[i], v)
		}
		out = append(out, b.String())
	}
	sort.Strings(out)
	return out
}

// finderRows are the search index rows of the records that carry a mark.
func (e *runEnv) finderRows(run string) []string {
	e.t.Helper()
	q := `SELECT f.rowid, f.title, f.body, f.kind, f.item, f.ref, f.at, f.who FROM finder f
		WHERE (f.kind IN ('mail','chat') AND EXISTS (SELECT 1 FROM messages m WHERE m.id = f.ref AND m.test_run = ?1))
		   OR (f.kind = 'comment' AND EXISTS (SELECT 1 FROM events v WHERE v.id = f.ref AND v.test_run = ?1))
		   OR (f.kind = 'note' AND EXISTS (SELECT 1 FROM notes n WHERE n.id = f.ref AND n.test_run = ?1))`
	rows, err := e.db.Query(q, run)
	if err != nil {
		e.t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id, item, ref, at int64
		var title, body, kind, who string
		if err := rows.Scan(&id, &title, &body, &kind, &item, &ref, &at, &who); err != nil {
			e.t.Fatal(err)
		}
		out = append(out, fmt.Sprint(id, "|", title, "|", body, "|", kind, "|", item, "|", ref, "|", at, "|", who))
	}
	sort.Strings(out)
	return out
}

func (e *runEnv) finderTotal() int {
	var n int
	e.db.QueryRow("SELECT COUNT(*) FROM finder").Scan(&n)
	return n
}

func hashOf(lines []string) string {
	h := sha256.Sum256([]byte(strings.Join(lines, "\n")))
	return hex.EncodeToString(h[:6]) + fmt.Sprintf("/%d", len(lines))
}

// state is what the tables hold for a mark: every table, as a hash of its rows.
func (e *runEnv) state(run string) map[string]string {
	e.t.Helper()
	out := map[string]string{}
	for _, tbl := range repo.RunTables {
		if tbl == "pool_marks" {
			// path and mark only
		}
		out[tbl] = hashOf(e.dump(tbl, run))
	}
	out["finder"] = hashOf(e.finderRows(run))
	return out
}

func (e *runEnv) count(table, run string) int {
	var n int
	e.db.QueryRow("SELECT COUNT(*) FROM "+table+" WHERE test_run = ?", run).Scan(&n)
	return n
}

func (e *runEnv) poolFiles() map[string]bool {
	out := map[string]bool{}
	filepath.WalkDir(e.a.imp.pool.root, func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() && poolPathRE.MatchString(strings.TrimPrefix(p, e.a.imp.pool.root+"/")) {
			out[strings.TrimPrefix(p, e.a.imp.pool.root+"/")] = true
		}
		return nil
	})
	return out
}

func poolPathOf(content string) string {
	h := sha256.Sum256([]byte(content))
	return relPath(hex.EncodeToString(h[:]))
}

func sameState(t *testing.T, what string, before, after map[string]string) {
	t.Helper()
	for tbl, b := range before {
		if after[tbl] != b {
			t.Errorf("%s: the rows of %s changed: %s became %s", what, tbl, b, after[tbl])
		}
	}
}

// --- a world: real data of every kind, and what two runs make ---

const (
	realShared = "REAL FILE BYTES, INVENTED"
	abShared   = "FILE BOTH RUNS USE, INVENTED"
	onlyA      = "FILE ONLY RUN A HAS, INVENTED"
	onlyB      = "FILE ONLY RUN B HAS, INVENTED"
	ownerFileA = "FILE OWNER ATTACHES IN A TEST CHAT, INVENTED"
)

type world struct {
	realChat, realMail repo.Item
	realPeople         []string
}

// realWorld makes unmarked data of every kind, the way the real app has it.
func (e *runEnv) realWorld() world {
	e.t.Helper()
	var w world
	// A real chat with a file, a person on each side and the owner's own message.
	c := chatOf("messenger", "real-1", "Anna Realova", "direct",
		e.in("r1", "real.anna", "Anna Realova", "Is the invoice for the kitchen ready? Real text.", 200),
		bridgeMessage{ID: "r2", Sender: "real.anna", SenderName: "Anna Realova", At: e.at(190), Text: "Here is the plan.", Attachments: []bridgeAttachment{{ID: "a1", Name: "real-plan.txt", Type: "text/plain", Size: int64(len(realShared))}}},
		e.mine("r3", "Yes, tomorrow.", 250))
	c.Participants = append(c.Participants, repo.Participant{Handle: "real.anna", Name: "Anna Realova"})
	c2 := chatOf("whatsapp", "real-2", "Bob Realny", "direct", e.in("q1", "real.bob", "Bob Realny", "Call me when you are free. Real text.", 100))
	c2.Participants = append(c2.Participants, repo.Participant{Handle: "real.bob", Name: "Bob Realny"})
	e.batch(c, c2)
	e.upload("messenger", "real-1", "r2", "a1", realShared)
	claudeLooksAtAll(e.t, e.a)
	w.realChat = e.itemOfChat("messenger", "real-1")
	w.realPeople = []string{"real.anna", "real.bob"}
	// A real mail with its history lines and a real person.
	w.realMail = e.newItem(stageNew, false, "Real invoice", "")
	err := e.a.store.Update(e.ctx, func(tx *repo.Tx) error {
		for i := 0; i < 3; i++ {
			if _, err := tx.RecordHistory(e.ctx, repo.HistoryRow{Source: "gmail", SourceID: fmt.Sprint("hist-", i), Thread: "th-real", Sender: "shop@real.test", ReceivedAt: e.ago(float64(i + 1)), State: "inbox"}, e.now.Unix()); err != nil {
				return err
			}
		}
		if _, err := tx.EnsurePerson(e.ctx, "mail", "alice@example.test", "Alice Example", e.now.Unix()); err != nil {
			return err
		}
		if err := tx.SetPicture(e.ctx, 1, "image/png", []byte("PNG-INVENTED"), e.now.Unix()); err != nil {
			return err
		}
		// A real rule that drops one chat sender, a real skipped line, a real calendar entry.
		if _, err := tx.InsertRule(e.ctx, repo.Rule{Name: "Real drop", Enabled: true, Source: "chat", Field: "sender", Op: "equals", Value: "drop.me", CreatedBy: "owner"}, e.now.Unix()); err != nil {
			return err
		}
		id, err := tx.InsertAgenda(e.ctx, repo.Agenda{Kind: "event", Source: "icloud", SourceID: "cal1/ev1", CalKey: "cal1", Calendar: "Home", Title: "Real dentist", Status: "confirmed", State: "open", CreatedAt: 1, UpdatedAt: 1, StartAt: e.now.Unix(), EndAt: e.now.Unix() + 3600, Version: 1})
		if err != nil {
			return err
		}
		return tx.InsertAgendaVersion(e.ctx, id, 1, "h", "{}", 1, 0)
	})
	if err != nil {
		e.t.Fatal(err)
	}
	// Agents without a run, on the real chat and the real mail.
	tok := func(m map[string]any) {
		if m["ok"] != true {
			e.t.Fatalf("a real agent call failed: %v", m)
		}
	}
	code, m := e.apiRun("", "POST", "/api/items/"+ref(w.realMail)+"/comments", map[string]any{"text": "A real comment by Claude."})
	if code != 200 {
		e.t.Fatal(code, m)
	}
	tok(m)
	_, m = e.apiRun("", "POST", "/api/items/"+ref(w.realMail)+"/proposals", map[string]any{"action": "pending", "reason": "Waits for the shop."})
	tok(m)
	_, m = e.apiRun("", "POST", "/api/agenda/todos", map[string]any{"item": ref(w.realMail), "text": "Pay the real invoice", "reason": "Due soon."})
	tok(m)
	_, m = e.apiRun("", "POST", "/api/notes", map[string]any{"title": "Real note", "text": "Anna Realova pays late. Real text.", "item": ref(w.realMail)})
	tok(m)
	_, m = e.apiRun("", "POST", "/api/notes", map[string]any{"title": "Real standalone note", "text": "Something real about invoices.", "sender": "shop@real.test"})
	tok(m)
	_, m = e.apiRun("", "POST", "/api/people/details", map[string]any{"person": "Anna Realova", "call_name": "Anča", "relation": "customer"})
	tok(m)
	_, m = e.apiRun("", "POST", "/api/people/proposals", map[string]any{"action": "join", "person": "Anna Realova", "with": "Bob Realny", "reason": "Same person, maybe."})
	tok(m)
	_, m = e.apiRun("", "POST", "/api/rule-proposals", map[string]any{"field": "sender", "value": "bulk@real-shop.test", "reason": "214 mails in 3 years, none opened."})
	tok(m)
	e.setInProgress(w.realChat)
	_, m = e.apiRun("", "POST", "/api/items/"+ref(w.realChat)+"/reply-proposals", map[string]any{"text": "Tomorrow works. the owner", "reason": "She asked for a time."})
	tok(m)
	// The owner's own doing on real data: approves the to-do, writes in the real chat with a file.
	if rec := e.decideAsOwner(int64(1), "approve"); rec.Code != 200 {
		e.t.Fatalf("real approve: %d %s", rec.Code, rec.Body.String())
	}
	e.macIsHere()
	if rec := e.writeAs(w.realChat, "A real message from the owner.", upFile{"real-note.txt", []byte(realShared)}); rec.Code != 200 {
		e.t.Fatalf("real write: %d %s", rec.Code, rec.Body.String())
	}
	if rec := e.formPost(fmt.Sprintf("/items/%d/notes", w.realMail.ID), url.Values{"text": {"The owner's real note on the real mail."}}); rec.Code != 200 && rec.Code != 303 {
		e.t.Fatalf("real note: %d %s", rec.Code, rec.Body.String())
	}
	return w
}

func (e *runEnv) formPost(path string, form url.Values) *httptest.ResponseRecorder {
	req := httptest.NewRequest("POST", path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Requested-With", "one")
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	return rec
}

func (e *runEnv) setInProgress(it repo.Item) {
	e.t.Helper()
	if err := e.a.store.Update(e.ctx, func(tx *repo.Tx) error { return tx.SetItemStage(e.ctx, it.ID, stageInProgress, e.now.Unix()) }); err != nil {
		e.t.Fatal(err)
	}
}

// proposalIDs lists the ids of the proposals of the item, by action.
func (e *runEnv) proposalOf(it repo.Item, action string) int64 {
	e.t.Helper()
	ps, err := e.a.store.ProposalsOfItem(e.ctx, it.ID)
	if err != nil {
		e.t.Fatal(err)
	}
	var id int64
	for _, p := range ps {
		if p.Action == action || (action == "reply" && isReplyAction(p.Action)) {
			id = max(id, p.ID)
		}
	}
	if id == 0 {
		e.t.Fatalf("no %s proposal on %s", action, ref(it))
	}
	return id
}

// makeRun is what a test of one run does: it puts chats in, makes entries,
// proposals, notes, people changes and a rule proposal under its name, and
// The owner then works on its items on the page. suffix keeps two runs apart.
func (e *runEnv) makeRun(run, suffix string, own string) {
	e.t.Helper()
	chatID := "e2e-chat-" + suffix
	c := chatOf("messenger", chatID, "Test chat "+suffix, "direct",
		e.in("t1", "test.one."+suffix, "Tina "+suffix, "Marker-"+suffix+" please send the test plan. Invented.", 150),
		e.in("t2", "real.anna", "Anna Realova", "Marker-"+suffix+" a line from a person who exists for real.", 140),
		bridgeMessage{ID: "t3", Sender: "test.one." + suffix, SenderName: "Tina " + suffix, At: e.at(130), Text: "Marker-" + suffix + " files",
			Attachments: []bridgeAttachment{{ID: "f1", Name: "shared-real.txt"}, {ID: "f2", Name: "both.txt"}, {ID: "f3", Name: "own.txt"}}},
		e.in("t4", "drop.me", "Dropped Sender", "Marker-"+suffix+" this one a real rule drops", 120),
	)
	c.Participants = append(c.Participants,
		repo.Participant{Handle: "test.one." + suffix, Name: "Tina " + suffix},
		repo.Participant{Handle: "test.two." + suffix, Name: "Tomas " + suffix},
		repo.Participant{Handle: "real.anna", Name: "Anna Realova"})
	c2 := chatOf("whatsapp", chatID+"-b", "Second test chat "+suffix, "group", e.in("u1", "test.three."+suffix, "Thea "+suffix, "Marker-"+suffix+" second chat. Invented.", 100))
	c2.Participants = append(c2.Participants, repo.Participant{Handle: "test.three." + suffix, Name: "Thea " + suffix})
	ans := e.mustBatchRun(run, c, c2)
	if ans["dropped"].(float64) != 1 {
		e.t.Fatalf("the real rule should drop one message of the run: %v", ans)
	}
	e.upload("messenger", chatID, "t3", "f1", realShared)
	e.upload("messenger", chatID, "t3", "f2", abShared)
	e.upload("messenger", chatID, "t3", "f3", own)
	claudeLooksAtAll(e.t, e.a)
	item := e.itemOfChat("messenger", chatID)
	it2 := e.itemOfChat("whatsapp", chatID+"-b")
	if item.TestRun != run || it2.TestRun != run {
		e.t.Fatalf("the items of the run are not marked: %q %q", item.TestRun, it2.TestRun)
	}
	e.setInProgress(item)
	call := func(method, path string, body any) map[string]any {
		code, m := e.apiRun(run, method, path, body)
		if code != 200 {
			e.t.Fatalf("%s %s under %s: %d %v", method, path, run, code, m)
		}
		return m
	}
	long := strings.Repeat("An invented sentence about the test plan that is long enough. ", 8)
	call("POST", "/api/items/"+ref(item)+"/comments", map[string]any{"text": "Comment for " + suffix + ". Marker-" + suffix})
	call("POST", "/api/items/"+ref(item)+"/comments", map[string]any{"text": "A second comment. " + long})
	call("POST", "/api/items/"+ref(item)+"/proposals", map[string]any{"action": "resolve", "reason": "Test says so."})
	call("POST", "/api/items/"+ref(item)+"/reply-proposals", map[string]any{"text": "Sent from a test " + suffix + ". the owner", "reason": "A test."})
	call("POST", "/api/agenda/todos", map[string]any{"item": ref(item), "text": "Todo of " + suffix, "reason": "Test."})
	call("POST", "/api/agenda/todos", map[string]any{"text": "Loose to-do of " + suffix, "due": "2026-12-01", "reason": "A to-do that belongs to no item."})
	call("POST", "/api/notes", map[string]any{"title": "Note " + suffix, "text": "Marker-" + suffix + " attached to the item.", "item": ref(item)})
	call("POST", "/api/notes", map[string]any{"title": "Standalone note " + suffix, "text": "Marker-" + suffix + " standalone."})
	call("POST", "/api/notes", map[string]any{"title": "Person note " + suffix, "text": "Marker-" + suffix + " about a person of the run.", "person": "Tina " + suffix})
	call("POST", "/api/people/details", map[string]any{"person": "Tina " + suffix, "call_name": "T-" + suffix, "relation": "tester"})
	call("POST", "/api/people/proposals", map[string]any{"action": "join", "person": "Tomas " + suffix, "with": "T-" + suffix, "reason": "Test."})
	call("POST", "/api/setting-proposals", map[string]any{"kind": "setting", "name": "Chat quiet time", "value": "10 minutes", "reason": "Chats of " + suffix + " go quiet slowly."})
	call("POST", "/api/pattern-proposals", map[string]any{"pattern": "REF-" + suffix + "-[0-9]+", "reason": "Invoice numbers like REF-" + suffix + "-1."})
	call("POST", "/api/rule-proposals", map[string]any{"field": "sender", "value": "bulk-" + suffix + "@shop.test", "reason": "214 mails in three years, none opened, none answered."})
	// A history line and a picture of the run, and a version of a to-do.
	rctx := repo.WithRun(e.ctx, run)
	if err := e.a.store.Update(rctx, func(tx *repo.Tx) error {
		_, err := tx.RecordHistory(rctx, repo.HistoryRow{Source: "gmail", SourceID: "hist-" + suffix, Thread: "th-" + suffix, Sender: "bulk-" + suffix + "@shop.test", ReceivedAt: e.ago(1), State: "inbox"}, e.now.Unix())
		return err
	}); err != nil {
		e.t.Fatal(err)
	}
	// The owner on the page, on the run's items: his doing carries the item's mark.
	if rec := e.decideAsOwner(e.proposalOf(item, "todo"), "approve"); rec.Code != 200 {
		e.t.Fatalf("approve the to-do: %d %s", rec.Code, rec.Body.String())
	}
	if rec := e.formPost(fmt.Sprintf("/proposals/%d/approve", e.proposalOf(item, "reply")), url.Values{"text": {"Sent from a test " + suffix + ". the owner"}}); rec.Code != 200 {
		e.t.Fatalf("approve the reply: %d %s", rec.Code, rec.Body.String())
	}
	if rec := e.formPost(fmt.Sprintf("/items/%d/notes", item.ID), url.Values{"text": {"The owner notes Marker-" + suffix + " on a test item."}}); rec.Code != 200 && rec.Code != 303 {
		e.t.Fatalf("note on the item: %d", rec.Code)
	}
	e.macIsHere()
	if rec := e.writeAs(it2, "The owner writes Marker-"+suffix+" in a test chat.", upFile{"owner-a.txt", []byte(ownerFileA + suffix)}, upFile{"owner-shared.txt", []byte(realShared)}); rec.Code != 200 {
		e.t.Fatalf("write in the test chat: %d %s", rec.Code, rec.Body.String())
	}
	if rec := e.formPost("/settings/rules/"+fmt.Sprint(e.ruleOf("bulk-"+suffix+"@shop.test"))+"/decision/approve", nil); rec.Code != 303 && rec.Code != 200 {
		e.t.Fatalf("approve the rule: %d %s", rec.Code, rec.Body.String())
	}
	// A skip rule of the run, approved by the owner on the page (SPEC 31).
	var skipID int64
	if err := e.a.store.Update(rctx, func(tx *repo.Tx) error {
		var err error
		skipID, err = tx.InsertSkipRule(rctx, repo.SkipRule{Name: "Skip rule " + suffix, Kind: skipSender, Value: "bulk-" + suffix + "@shop.test",
			Reason: "skipped 5 times, brought back 0 times", ProposedBy: "claude", ProposedAt: e.now.Unix()})
		return err
	}); err != nil {
		e.t.Fatal(err)
	}
	if rec := e.formPost(fmt.Sprintf("/settings/skip-rules/%d/decision/approve", skipID), nil); rec.Code != 303 && rec.Code != 200 {
		e.t.Fatalf("approve the skip rule: %d %s", rec.Code, rec.Body.String())
	}
	// The join of two people of the run, approved by the owner on the page.
	people, _ := e.a.store.FindPeople(e.ctx, "Tomas "+suffix, 5)
	if len(people) != 1 {
		e.t.Fatalf("people: %v", people)
	}
	pid := e.personProposalOf(people[0].ID)
	if rec := e.formPost(fmt.Sprintf("/people-proposals/%d/approve", pid), nil); rec.Code != 303 && rec.Code != 200 {
		e.t.Fatalf("approve the join: %d %s", rec.Code, rec.Body.String())
	}
	var tinaID int64
	e.db.QueryRow("SELECT id FROM people WHERE test_run = ? ORDER BY id LIMIT 1", run).Scan(&tinaID)
	if err := e.a.store.Update(rctx, func(tx *repo.Tx) error {
		return tx.SetPicture(rctx, tinaID, "image/png", []byte("PNG-"+suffix), e.now.Unix())
	}); err != nil {
		e.t.Fatal(err)
	}
	var todoID int64
	e.db.QueryRow("SELECT id FROM agenda WHERE test_run = ? AND kind = 'todo' LIMIT 1", run).Scan(&todoID)
	e.a.store.Update(e.ctx, func(tx *repo.Tx) error { return tx.InsertAgendaVersion(e.ctx, todoID, 2, "h2", "{}", e.now.Unix(), 0) })
}

func (e *runEnv) ruleOf(value string) int64 {
	var id int64
	if err := e.db.QueryRow("SELECT id FROM rules WHERE value = ?", value).Scan(&id); err != nil {
		e.t.Fatal(err)
	}
	return id
}

func (e *runEnv) personProposalOf(personID int64) int64 {
	var id int64
	if err := e.db.QueryRow("SELECT id FROM person_proposals WHERE person_id = ? OR other_id = ? ORDER BY id DESC", personID, personID).Scan(&id); err != nil {
		e.t.Fatal(err)
	}
	return id
}

func (e *runEnv) removeVia(token, run string) (int, map[string]any) {
	rec := e.bridgeCall("DELETE", "/api/test-runs/"+run, token, nil)
	return rec.Code, answerOf(e.t, rec)
}

func removedCount(m map[string]any, kind string) int {
	r, _ := m["removed"].(map[string]any)
	n, _ := r[kind].(float64)
	return int(n)
}

// --- the tests ---

func TestRemovingARunRemovesEveryRecordOfItAndTheFilesOnlyItUsedAndNothingElseChanges(t *testing.T) {
	e := newRunEnv(t)
	e.realWorld()
	e.makeRun(runA, "alpha", onlyA)
	e.makeRun(runB, "beta", onlyB)

	// Every table that can hold a mark holds some of run A (nothing is left out of the proof).
	for _, tbl := range repo.RunTables {
		if e.count(tbl, runA) == 0 {
			t.Errorf("the scenario made no row of run A in %s", tbl)
		}
	}
	if n := e.count("finder", runA); n != 0 {
		_ = n
	}
	realBefore, bBefore := e.state(""), e.state(runB)
	totalFinder := e.finderTotal()
	aFinder := len(e.finderRows(runA))
	if aFinder == 0 {
		t.Fatal("the scenario put nothing of run A into the search index")
	}
	files := e.poolFiles()
	for _, c := range []string{realShared, abShared, onlyA + "", onlyB, ownerFileA + "alpha", ownerFileA + "beta"} {
		if !files[poolPathOf(c)] {
			t.Fatalf("the pool lacks %q before the removal", c)
		}
	}
	// Nothing about the run is a real record: real people, handles and the real file are unmarked.
	if e.count("people", "") == 0 || e.count("person_handles", "") == 0 {
		t.Fatal("no real people")
	}

	code, ans := e.removeVia(e.a.agentTokenValue(), runA)
	if code != 200 || ans["ok"] != true {
		t.Fatalf("remove: %d %v", code, ans)
	}
	// It says how many of each kind went, and every kind the scenario made is more than none.
	for _, k := range []string{"items", "messages", "raw", "events", "files", "people", "handles", "notes", "proposals", "agenda", "agenda_versions", "rules", "outbox", "history", "chats", "links", "skipped", "pictures", "person_proposals", "agenda_proposals", "search_hits", "pool_files"} {
		if removedCount(ans, k) == 0 {
			t.Errorf("the answer says no %s were removed: %v", k, ans["removed"])
		}
	}
	if removedCount(ans, "search_hits") != aFinder {
		t.Errorf("search hits removed: %d, the run had %d", removedCount(ans, "search_hits"), aFinder)
	}
	// Run A has nothing left in any table, and the index.
	for _, tbl := range repo.RunTables {
		if n := e.count(tbl, runA); n != 0 {
			t.Errorf("%d rows of run A are left in %s", n, tbl)
		}
	}
	if left := e.finderRows(runA); len(left) != 0 {
		t.Errorf("search hits of run A are left: %v", left)
	}
	if now := e.finderTotal(); now != totalFinder-aFinder {
		t.Errorf("the search index holds %d rows, want %d", now, totalFinder-aFinder)
	}
	// What is not run A is exactly as it was, table by table, row by row.
	sameState(t, "real data", realBefore, e.state(""))
	sameState(t, "run B", bBefore, e.state(runB))
	// The search finds the real and run B's text, and nothing of run A.
	if rec := e.api("GET", "/api/search?q=Marker-alpha", nil); !strings.Contains(rec.Body.String(), `"results":[]`) {
		t.Errorf("a search still finds run A: %s", rec.Body.String())
	}
	if rec := e.api("GET", "/api/search?q=Marker-beta", nil); !strings.Contains(rec.Body.String(), "Marker-beta") {
		t.Errorf("a search does not find run B: %s", rec.Body.String())
	}
	if rec := e.api("GET", "/api/search?q=invoice", nil); !strings.Contains(rec.Body.String(), "invoice") {
		t.Errorf("a search does not find real data: %s", rec.Body.String())
	}
	// Pool files: the one only run A had is gone; the one real data holds and the one run B holds stay.
	after := e.poolFiles()
	if after[poolPathOf(onlyA)] || after[poolPathOf(ownerFileA+"alpha")] {
		t.Error("a file only run A used is still in the pool")
	}
	for _, c := range []string{realShared, abShared, onlyB, ownerFileA + "beta"} {
		if !after[poolPathOf(c)] {
			t.Errorf("a file that is not only run A's was removed: %q", c)
		}
	}
	// The real file never carried a mark and still has its real rows.
	var realRefs int
	e.db.QueryRow("SELECT COUNT(*) FROM files WHERE test_run = '' AND path = ?", poolPathOf(realShared)).Scan(&realRefs)
	if realRefs < 2 {
		t.Errorf("the real file has %d rows", realRefs)
	}
	// Removing run B then takes the file both runs used, and still not the real one.
	if code, _ := e.removeVia(e.a.agentTokenValue(), runB); code != 200 {
		t.Fatal(code)
	}
	last := e.poolFiles()
	if last[poolPathOf(abShared)] || last[poolPathOf(onlyB)] {
		t.Error("files only the runs used are still in the pool")
	}
	if !last[poolPathOf(realShared)] {
		t.Error("the real file was removed")
	}
	sameState(t, "real data after both", realBefore, e.state(""))
	for _, tbl := range repo.RunTables {
		if n := e.count(tbl, runB); n != 0 {
			t.Errorf("%d rows of run B are left in %s", n, tbl)
		}
	}
}

func TestAChatPersonOrFileThatExistedBeforeIsNeverMarkedOrRemovedWhenARunUsesIt(t *testing.T) {
	e := newRunEnv(t)
	e.realWorld()
	var annaRows, annaHandles int
	e.db.QueryRow("SELECT COUNT(*) FROM person_handles WHERE value = 'real.anna'").Scan(&annaHandles)
	people := e.count("people", "")
	e.makeRun(runA, "alpha", onlyA)
	if e.count("people", "") != people {
		t.Error("a real person was changed by a run")
	}
	e.db.QueryRow("SELECT COUNT(*) FROM person_handles WHERE value = 'real.anna' AND test_run = ''").Scan(&annaRows)
	if annaRows != annaHandles || annaHandles != 1 {
		t.Fatalf("the real handle: %d of %d unmarked", annaRows, annaHandles)
	}
	// The run's own message from the real person refers to it, by handle.
	var n int
	e.db.QueryRow("SELECT COUNT(*) FROM messages WHERE test_run = ? AND sender = 'real.anna'", runA).Scan(&n)
	if n != 1 {
		t.Fatalf("the run has %d messages from the real person", n)
	}
	// The pool file that was there before is not marked for the run.
	var marks int
	e.db.QueryRow("SELECT COUNT(*) FROM pool_marks WHERE path = ?", poolPathOf(realShared)).Scan(&marks)
	if marks != 0 {
		t.Error("a file that existed before the run was marked")
	}
	e.removeVia(e.a.agentTokenValue(), runA)
	e.db.QueryRow("SELECT COUNT(*) FROM person_handles WHERE value = 'real.anna' AND test_run = ''").Scan(&n)
	if n != 1 {
		t.Error("the real handle is gone")
	}
	if !e.poolFiles()[poolPathOf(realShared)] {
		t.Error("the real file is gone")
	}
}

func TestABatchUnderARunCannotCreateOrChangeAChatThatExistsWithoutItsMark(t *testing.T) {
	e := newRunEnv(t)
	e.realWorld()
	e.makeRun(runB, "beta", onlyB)
	real := e.state("")
	b := e.state(runB)
	msgs := e.count("messages", "")
	// The same network and chat id as a real chat: refused, not merged.
	c := chatOf("messenger", "real-1", "Anna Realova", "direct", e.in("x1", "real.anna", "Anna Realova", "Marker-merge a message into a real chat.", 5))
	code, ans := e.batchRun(runA, c)
	if code != 409 || !strings.Contains(fmt.Sprint(ans["error"]), "not part of this test run") {
		t.Fatalf("a batch into a real chat: %d %v", code, ans)
	}
	// One new chat and one real one in the same batch: nothing of it is written.
	fresh := chatOf("messenger", "e2e-fresh-chat", "Fresh", "direct", e.in("y1", "fresh.one", "Fresh One", "Marker-fresh text.", 5))
	if code, _ := e.batchRun(runA, fresh, c); code != 409 {
		t.Fatalf("a mixed batch: %d", code)
	}
	if _, ok := e.a.store.ItemByLink(e.ctx, chatLinkKind, repo.ChatKey("messenger", "e2e-fresh-chat")); ok {
		t.Error("the new chat of a refused batch was written")
	}
	// Another run's chat is just as closed.
	other := chatOf("messenger", "e2e-chat-beta", "Test chat beta", "direct", e.in("z1", "test.one.beta", "Tina", "Marker-other into another run's chat.", 5))
	if code, _ := e.batchRun(runA, other); code != 409 {
		t.Fatalf("a batch into another run's chat: %d", code)
	}
	// Even a message id that exists in a real chat, sent in a chat of its own name, changes nothing real.
	sameState(t, "real data", real, e.state(""))
	sameState(t, "run B", b, e.state(runB))
	if e.count("messages", "") != msgs || e.count("messages", runA) != 0 {
		t.Error("a message was written")
	}
	// Without a run the same batch is what it always was: it goes into the real chat.
	if code, _ := e.batchRun("", c); code != 200 {
		t.Fatalf("the same batch without a run: %d", code)
	}
	if e.count("messages", "") != msgs+1 {
		t.Error("a batch without a run no longer adds to a real chat")
	}
}

func TestAnAgentCallUnderARunCannotWriteToAnUnmarkedItemPersonNoteOrRuleOrToAnotherRun(t *testing.T) {
	e := newRunEnv(t)
	w := e.realWorld()
	e.makeRun(runB, "beta", onlyB)
	// A real note, a real person, a real proposed rule and a real active rule to aim at.
	var realNote int64
	e.db.QueryRow("SELECT id FROM notes WHERE test_run = '' ORDER BY id LIMIT 1").Scan(&realNote)
	before, bBefore := e.state(""), e.state(runB)
	itemB := e.itemOfChat("messenger", "e2e-chat-beta")
	cases := []struct {
		what, method, path string
		body               map[string]any
	}{
		{"a comment on a real item", "POST", "/api/items/" + ref(w.realMail) + "/comments", map[string]any{"text": "Marker-no a comment."}},
		{"a look at a real item", "POST", "/api/items/" + ref(w.realChat) + "/seen", nil},
		{"a skip of a real item", "POST", "/api/items/" + ref(w.realMail) + "/skip", map[string]any{"reason": "noise", "kind": "noise"}},
		{"a proposal on a real item", "POST", "/api/items/" + ref(w.realMail) + "/proposals", map[string]any{"action": "close", "reason": "test"}},
		{"a reply on a real chat", "POST", "/api/items/" + ref(w.realChat) + "/reply-proposals", map[string]any{"text": "Marker-no reply", "reason": "test"}},
		{"a to-do on a real item", "POST", "/api/agenda/todos", map[string]any{"item": ref(w.realMail), "text": "Marker-no todo", "reason": "test"}},
		{"a note on a real item", "POST", "/api/notes", map[string]any{"title": "No", "text": "Marker-no note", "item": ref(w.realMail)}},
		{"a note about a real person", "POST", "/api/notes", map[string]any{"title": "No", "text": "Marker-no note", "person": "Anna Realova"}},
		{"a change of a real note", "POST", "/api/notes", map[string]any{"title": "No", "text": "Marker-no note", "note_id": realNote}},
		{"a change of a real person", "POST", "/api/people/details", map[string]any{"person": "Anna Realova", "call_name": "Marker-no"}},
		{"a proposal about real people", "POST", "/api/people/proposals", map[string]any{"action": "join", "person": "Bob Realny", "with": "Anna Realova", "reason": "test"}},
		{"a join of a real and a run's person", "POST", "/api/people/proposals", map[string]any{"action": "join", "person": "Bob Realny", "with": "T-beta", "reason": "test"}},
		{"a rule that a real one says already", "POST", "/api/rule-proposals", map[string]any{"field": "sender", "value": "bulk@real-shop.test", "reason": "214 mails, none opened."}},
		{"a comment on another run's item", "POST", "/api/items/" + ref(itemB) + "/comments", map[string]any{"text": "Marker-no a comment."}},
	}
	for _, c := range cases {
		var body any
		if c.body != nil {
			body = c.body
		}
		code, m := e.apiRun(runA, c.method, c.path, body)
		if code != 403 || !strings.Contains(fmt.Sprint(m["error"]), "not part of it") {
			t.Errorf("%s under a run: %d %v", c.what, code, m)
		}
	}
	// The same through the connector's tools, with the argument.
	for _, c := range []struct {
		tool string
		args map[string]any
	}{
		{"add_comment", map[string]any{"id": ref(w.realMail), "text": "Marker-no", "run": runA}},
		{"mark_seen", map[string]any{"id": ref(w.realChat), "run": runA}},
		{"propose_change", map[string]any{"id": ref(w.realMail), "action": "close", "reason": "test", "run": runA}},
		{"post_agenda", map[string]any{"item": ref(w.realMail), "text": "Marker-no", "reason": "test", "run": runA}},
		{"save_note", map[string]any{"title": "No", "text": "Marker-no", "item": ref(w.realMail), "run": runA}},
		{"update_person", map[string]any{"person": "Anna Realova", "call_name": "Marker-no", "run": runA}},
		{"propose_person_change", map[string]any{"action": "join", "person": "Bob Realny", "with": "Anna Realova", "reason": "test", "run": runA}},
		{"propose_rule", map[string]any{"field": "sender", "value": "bulk@real-shop.test", "reason": "214 mails, none opened.", "run": runA}},
	} {
		text, isErr, _ := e.a.callTool(repo.WithRun(e.ctx, runA), c.tool, mustJSON(c.args))
		if !isErr || !strings.Contains(text, "not part of it") {
			t.Errorf("tool %s under a run: %v %q", c.tool, isErr, text)
		}
	}
	// Nothing was written, anywhere.
	sameState(t, "real data", before, e.state(""))
	sameState(t, "run B", bBefore, e.state(runB))
	for _, tbl := range repo.RunTables {
		if n := e.count(tbl, runA); n != 0 {
			t.Errorf("a refused call left %d rows of its run in %s", n, tbl)
		}
	}
	// Without a run the same calls are what they always were.
	if code, m := e.apiRun("", "POST", "/api/items/"+ref(w.realMail)+"/comments", map[string]any{"text": "A real comment without a run."}); code != 200 {
		t.Errorf("a call without a run: %d %v", code, m)
	}
}

func TestChildrenMadeLaterOnAMarkedItemByOwnerOnThePageAreMarkedAndRemovedWithTheRun(t *testing.T) {
	e := newRunEnv(t)
	e.realWorld()
	e.makeRun(runA, "alpha", onlyA)
	item := e.itemOfChat("messenger", "e2e-chat-alpha")
	it2 := e.itemOfChat("whatsapp", "e2e-chat-alpha-b")
	// The owner's note, his approvals and his writing in the chat all carry the run's mark.
	for _, q := range []string{
		"SELECT COUNT(*) FROM events WHERE item_id = ? AND type = 'noted' AND actor = 'owner' AND test_run = ?",
		"SELECT COUNT(*) FROM events WHERE item_id = ? AND type = 'proposal_decided' AND actor = 'owner' AND test_run = ?",
		"SELECT COUNT(*) FROM agenda WHERE item_id = ? AND kind = 'todo' AND test_run = ?",
	} {
		var n int
		e.db.QueryRow(q, item.ID, runA).Scan(&n)
		if n == 0 {
			t.Errorf("no row of the run for %q", q)
		}
	}
	for _, q := range []string{
		"SELECT COUNT(*) FROM chat_out WHERE item_id = ? AND test_run = ?",
		"SELECT COUNT(*) FROM files WHERE item_id = ? AND source = 'owner' AND test_run = ?",
		"SELECT COUNT(*) FROM events WHERE item_id = ? AND type = 'write_queued' AND test_run = ?",
	} {
		var n int
		e.db.QueryRow(q, it2.ID, runA).Scan(&n)
		if n == 0 {
			t.Errorf("no row of the run for %q", q)
		}
	}
	// Nothing that belongs to a marked item is unmarked, in any table with an item.
	for _, tbl := range []string{"events", "files", "messages", "proposals", "chat_out", "agenda", "item_links", "notes", "chats"} {
		var n int
		e.db.QueryRow("SELECT COUNT(*) FROM " + tbl + " t WHERE t.test_run = '' AND t.item_id IN (SELECT id FROM items WHERE test_run <> '')").Scan(&n)
		if n != 0 {
			t.Errorf("%d unmarked rows of %s belong to a marked item", n, tbl)
		}
	}
	// A note that Claude, without a run, writes on a marked item is marked too.
	code, m := e.apiRun("", "POST", "/api/notes", map[string]any{"title": "Later", "text": "Marker-later on a test item.", "item": ref(item)})
	if code != 200 {
		t.Fatal(code, m)
	}
	var later string
	e.db.QueryRow("SELECT test_run FROM notes WHERE title = 'Later'").Scan(&later)
	if later != runA {
		t.Errorf("a note on a marked item carries %q", later)
	}
	_, ans := e.removeVia(e.a.agentTokenValue(), runA)
	if removedCount(ans, "items") != 2 {
		t.Errorf("removed: %v", ans)
	}
	for _, tbl := range repo.RunTables {
		if n := e.count(tbl, runA); n != 0 {
			t.Errorf("%d rows of the run are left in %s", n, tbl)
		}
	}
	// Nothing points at a removed item.
	rows, err := e.db.Query("PRAGMA foreign_key_check")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	if rows.Next() {
		t.Error("a row points at something that was removed")
	}
}

func TestAnInvalidRunNameIsRefusedAndNothingIsWritten(t *testing.T) {
	e := newRunEnv(t)
	w := e.realWorld()
	before := e.state("")
	bad := []string{"e2e-short", "E2E-abcdefgh1", "run-abcdefgh1", "e2e-" + strings.Repeat("a", 41), "e2e-abcdefgh/1", "e2e-abcdefg h", "e2e-ABCDEFGH1", "e2e_abcdefgh1", "../e2e-abcdefgh1"}
	for _, name := range bad {
		code, m := e.apiRun(name, "POST", "/api/items/"+ref(w.realMail)+"/comments", map[string]any{"text": "Marker-bad."})
		if code != 400 || !strings.Contains(fmt.Sprint(m["error"]), "e2e-") {
			t.Errorf("header %q: %d %v", name, code, m)
		}
		if code, _ := e.batchRun(name, chatOf("messenger", "bad-run-chat", "x", "direct", e.in("b1", "bad.one", "Bad", "Marker-bad.", 5))); code != 400 {
			t.Errorf("batch field %q: %d", name, code)
		}
		rec := e.bridgeCall("DELETE", "/api/test-runs/"+url.PathEscape(name), e.a.agentTokenValue(), nil)
		if rec.Code != 400 && rec.Code != 404 && rec.Code != 301 {
			t.Errorf("removing %q: %d", name, rec.Code)
		}
	}
	// The connector refuses it too.
	req := httptest.NewRequest("POST", "/api/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"add_comment","arguments":{"id":"`+ref(w.realMail)+`","text":"Marker-bad","run":"nope"}}}`))
	req.Header.Set("Authorization", "Bearer "+e.a.agentTokenValue())
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	if !strings.Contains(rec.Body.String(), `"isError":true`) || !strings.Contains(rec.Body.String(), "e2e-") {
		t.Errorf("the connector took a bad run name: %s", rec.Body.String())
	}
	// Header and field that name two runs are refused.
	req = httptest.NewRequest("POST", "/api/bridge/batch", strings.NewReader(`{"run":"`+runA+`","chats":[]}`))
	req.Header.Set("Authorization", "Bearer "+e.a.bridgeTokenValue())
	req.Header.Set("X-Test-Run", runB)
	rec = httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	if rec.Code != 400 {
		t.Errorf("two names: %d", rec.Code)
	}
	sameState(t, "real data", before, e.state(""))
	if e.count("events", "") != e.countAll("events") {
		t.Error("something was marked")
	}
}

func (e *runEnv) countAll(table string) int {
	var n int
	e.db.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&n)
	return n
}

func TestRemovingARunNeedsATokenAndEitherTokenWorksAndAnUnknownRunOrASecondRemovalAnswersZeros(t *testing.T) {
	e := newRunEnv(t)
	e.realWorld()
	e.makeRun(runA, "alpha", onlyA)
	before := e.state("")
	for _, tok := range []string{"", "not-the-token", "x"} {
		if code, m := e.removeVia(tok, runA); code != 401 || m["error"] != "wrong token" {
			t.Errorf("token %q: %d %v", tok, code, m)
		}
		if rec := e.bridgeCall("GET", "/api/test-runs", tok, nil); rec.Code != 401 {
			t.Errorf("listing with token %q: %d", tok, rec.Code)
		}
	}
	if e.count("items", runA) == 0 {
		t.Fatal("a refused removal removed something")
	}
	// The bridge's token lists, the agents' token removes.
	rec := e.bridgeCall("GET", "/api/test-runs", e.a.bridgeTokenValue(), nil)
	list := answerOf(t, rec)
	runs, _ := list["runs"].([]any)
	if rec.Code != 200 || len(runs) != 1 {
		t.Fatalf("list: %d %v", rec.Code, list)
	}
	first := runs[0].(map[string]any)
	if first["run"] != runA || first["records"].(float64) == 0 || first["first_at"] == nil || first["counts"].(map[string]any)["items"].(float64) != 2 {
		t.Errorf("the run in the list: %v", first)
	}
	code, ans := e.removeVia(e.a.agentTokenValue(), runA)
	if code != 200 || removedCount(ans, "items") != 2 {
		t.Fatalf("remove with the agents' token: %d %v", code, ans)
	}
	// Twice, and a run that never was, answer zeros and change nothing.
	for _, run := range []string{runA, runB, "e2e-neverbefore99"} {
		code, ans := e.removeVia(e.a.bridgeTokenValue(), run)
		if code != 200 || ans["total"].(float64) != 0 {
			t.Errorf("removing %s again: %d %v", run, code, ans)
		}
		for k, v := range ans["removed"].(map[string]any) {
			if v.(float64) != 0 {
				t.Errorf("%s: %s %v", run, k, v)
			}
		}
	}
	sameState(t, "real data", before, e.state(""))
	// The list is empty again; another method on the address is not removal.
	list = answerOf(t, e.bridgeCall("GET", "/api/test-runs", e.a.agentTokenValue(), nil))
	if runs, _ := list["runs"].([]any); len(runs) != 0 {
		t.Errorf("the list after the removal: %v", list)
	}
	if rec := e.bridgeCall("GET", "/api/test-runs/"+runA, e.a.agentTokenValue(), nil); rec.Code == 200 {
		t.Error("a GET removed or listed a run by name")
	}
}

func TestTheAppendOnlyTriggersRefuseDeletingAnUnmarkedRawRowOrEventAndAllowOnlyMarkedOnes(t *testing.T) {
	e := newRunEnv(t)
	e.realWorld()
	e.makeRun(runA, "alpha", onlyA)
	raws, events := e.countAll("raw"), e.countAll("events")
	// Whatever the statement says, a row without a mark cannot go; one statement that reaches both fails whole.
	for _, q := range []string{"DELETE FROM raw", "DELETE FROM events", "DELETE FROM raw WHERE test_run = ''", "DELETE FROM events WHERE test_run = ''",
		"DELETE FROM raw WHERE id > 0", "DELETE FROM events WHERE id > 0", "DELETE FROM raw WHERE test_run <> 'e2e-x'", "DELETE FROM events WHERE test_run <> 'e2e-x'"} {
		if _, err := e.db.Exec(q); err == nil {
			t.Errorf("%q was allowed", q)
		}
	}
	for _, q := range []string{"UPDATE raw SET test_run = 'e2e-aaaaaaaa9' WHERE test_run = ''", "UPDATE events SET test_run = 'e2e-aaaaaaaa9' WHERE test_run = ''"} {
		if _, err := e.db.Exec(q); err == nil {
			t.Errorf("%q was allowed: a real row could be marked", q)
		}
	}
	// A row that carries no run-shaped mark cannot be deleted either.
	if _, err := e.db.Exec("INSERT INTO events (at, actor, type, data, test_run) VALUES (1, 'x', 'x', '', 'not-a-run')"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.db.Exec("DELETE FROM events WHERE test_run = 'not-a-run'"); err == nil {
		t.Error("a mark that is not a run's was deletable")
	}
	if e.countAll("raw") != raws || e.countAll("events") != events+1 {
		t.Error("a refused delete changed the tables")
	}
	// A marked row can go.
	if _, err := e.db.Exec("DELETE FROM events WHERE test_run = ?", runA); err != nil {
		t.Errorf("a marked event could not be deleted: %v", err)
	}
}

func TestTheSettingsPageShowsTestDataOnlyWhenMarkedDataExistsAndRemoveTakesTheRunAway(t *testing.T) {
	e := newRunEnv(t)
	e.realWorld()
	page := get(e.h, "/settings").Body.String()
	if strings.Contains(page, "Test data") || strings.Contains(page, "e2e-") {
		t.Error("the Settings page shows a section for test data when there is none")
	}
	e.makeRun(runA, "alpha", onlyA)
	e.makeRun(runB, "beta", onlyB)
	page = get(e.h, "/settings").Body.String()
	for _, want := range []string{"Test data", runA, runB, ">Remove<", "/settings/test-data/" + runA + "/remove", "items"} {
		if !strings.Contains(page, want) {
			t.Errorf("the section lacks %q", want)
		}
	}
	realBefore, bBefore := e.state(""), e.state(runB)
	// Posted from another site, the button does nothing.
	req := httptest.NewRequest("POST", "/settings/test-data/"+runA+"/remove", nil)
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	if rec.Code != 403 || e.count("items", runA) == 0 {
		t.Fatalf("a cross-site post: %d", rec.Code)
	}
	// A name that is not a run's is not found.
	if rec := post(e.h, "/settings/test-data/real-1/remove", nil); rec.Code != 404 {
		t.Errorf("a bad name: %d", rec.Code)
	}
	rec = post(e.h, "/settings/test-data/"+runA+"/remove", nil)
	if rec.Code != 303 || !strings.Contains(rec.Header().Get("Location"), "n=testremoved") {
		t.Fatalf("Remove: %d %q", rec.Code, rec.Header().Get("Location"))
	}
	page = get(e.h, strings.SplitN(rec.Header().Get("Location"), "#", 2)[0]).Body.String()
	if !strings.Contains(page, "Removed.") || strings.Contains(page, runA) || !strings.Contains(page, runB) {
		t.Error("the page after Remove does not say Removed. and show only the other run")
	}
	for _, tbl := range repo.RunTables {
		if n := e.count(tbl, runA); n != 0 {
			t.Errorf("%d rows of run A are left in %s", n, tbl)
		}
	}
	sameState(t, "real data", realBefore, e.state(""))
	sameState(t, "run B", bBefore, e.state(runB))
	post(e.h, "/settings/test-data/"+runB+"/remove", nil)
	if page := get(e.h, "/settings").Body.String(); strings.Contains(page, "Test data") {
		t.Error("the section stays when nothing is left")
	}
}

func TestNothingAboutRunNamesAppearsInThePagesOwnerSeesOutsideTheSettingsSection(t *testing.T) {
	e := newRunEnv(t)
	w := e.realWorld()
	e.makeRun(runA, "alpha", onlyA)
	item := e.itemOfChat("messenger", "e2e-chat-alpha")
	var someNote, somePerson int64
	e.db.QueryRow("SELECT id FROM notes WHERE test_run = ?", runA).Scan(&someNote)
	e.db.QueryRow("SELECT id FROM people WHERE test_run = ? LIMIT 1", runA).Scan(&somePerson)
	pages := []string{"/", "/list/new", "/list/in_progress", "/list/pending", "/list/resolved", "/closed", "/agenda", "/people", fmt.Sprintf("/people/%d", somePerson),
		"/notes", fmt.Sprintf("/notes/%d", someNote), "/search?q=Marker-alpha", "/connectors", "/settings",
		fmt.Sprintf("/items/%d", item.ID), fmt.Sprintf("/items/%d/row?open=1", item.ID), fmt.Sprintf("/items/%d", w.realChat.ID), fmt.Sprintf("/items/%d/messages", item.ID)}
	for _, p := range pages {
		rec := get(e.h, p)
		if rec.Code != 200 {
			t.Errorf("%s: %d", p, rec.Code)
			continue
		}
		body := rec.Body.String()
		if p == "/settings" {
			i := strings.Index(body, `id="h-testdata"`)
			if i < 0 {
				t.Fatal("no Test data section")
			}
			j := strings.Index(body[i:], "</section>")
			body = body[:i] + body[i+j:]
		}
		for _, bad := range []string{runA, "e2e-run", "test_run", "X-Test-Run"} {
			if strings.Contains(body, bad) {
				t.Errorf("%s shows %q", p, bad)
			}
		}
	}
	// The agents' reading addresses do not carry it either.
	for _, p := range []string{"/api/items/" + ref(item), "/api/items/" + ref(item) + "/events", "/api/items/" + ref(item) + "/thread", "/api/people?q=alpha", "/api/search?q=Marker-alpha", "/api/queue", "/api/decisions", "/api/agenda", "/api/items/" + ref(item) + "/files"} {
		rec := e.api("GET", p, nil)
		if rec.Code != 200 {
			t.Errorf("%s: %d", p, rec.Code)
		}
		for _, bad := range []string{runA, "test_run"} {
			if strings.Contains(rec.Body.String(), bad) {
				t.Errorf("%s shows %q", p, bad)
			}
		}
	}
}

func TestEveryTableWithMarkedRowsIsRemovedAndNoNewTableIsLeftOut(t *testing.T) {
	e := newRunEnv(t)
	e.realWorld()
	in := map[string]bool{}
	for _, tbl := range repo.RunTables {
		in[tbl] = true
	}
	rows, err := e.db.Query("SELECT name FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%'")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		rows.Scan(&name)
		cols, _ := e.db.Query("SELECT name FROM pragma_table_info('" + name + "')")
		has := false
		for cols.Next() {
			var c string
			cols.Scan(&c)
			has = has || c == "test_run"
		}
		cols.Close()
		switch {
		case has && !in[name]:
			t.Errorf("table %s has a mark column but is not removed with the run", name)
		case !has && !in[name] && !neverMarked[name]:
			t.Errorf("table %s is neither marked nor listed as never marked: decide which", name)
		case !has && in[name]:
			t.Errorf("table %s is removed with the run but has no mark column", name)
		}
	}
	// Every table of the removal has its own DELETE naming the mark.
	if got := len(repo.Kinds()); got < len(repo.RunTables)-1 {
		t.Errorf("kinds: %d", got)
	}
	// The things that never carry a mark: settings, guards and connections are written by the owner or the app and stay.
	if err := e.a.store.SetSetting(e.ctx, "guard.blocked_targets", "off"); err != nil {
		t.Fatal(err)
	}
	e.makeRun(runA, "alpha", onlyA)
	e.removeVia(e.a.agentTokenValue(), runA)
	if e.a.store.Setting(e.ctx, "guard.blocked_targets") != "off" {
		t.Error("a setting was removed")
	}
	if e.a.agentTokenValue() == "" || e.a.bridgeTokenValue() == "" {
		t.Error("a token was removed")
	}
	_ = context.Background
	_ = http.StatusOK
}

// SPEC 16: the answer's total is the sum of the per-kind counts it reports.
func TestSpec16RemovedTotalIsTheSumOfTheCountsShown(t *testing.T) {
	e := newRunEnv(t)
	run := "e2e-sumcheck01"
	e.mustBatchRun(run, chatOf("whatsapp", "sum1", "Jana", "direct", e.in("m1", "+420111", "Jana", "Ahoj sumcheck", 5)))
	code, ans := e.removeVia(e.a.agentTokenValue(), run)
	if code != 200 {
		t.Fatalf("remove: %d %v", code, ans)
	}
	sum := 0.0
	for _, v := range ans["removed"].(map[string]any) {
		sum += v.(float64)
	}
	if sum == 0 || ans["total"].(float64) != sum {
		t.Errorf("total %v, the counts shown add up to %v: %v", ans["total"], sum, ans["removed"])
	}
}

// SPEC 16: a to-do typed by hand in the agenda belongs to no item; the page may
// name a test run for it, in the fixed shape of a run name only, and the run's
// removal then takes it with its record. A badly shaped name is ignored.
func TestSpec16ATodoTypedInThePageCanCarryARunsMark(t *testing.T) {
	e := newRunEnv(t)
	run := "e2e-todohand01"
	todo := func(runName, text string) {
		var buf strings.Builder
		buf.WriteString("text=" + url.QueryEscape(text))
		req := httptest.NewRequest("POST", "/agenda/todos", strings.NewReader(buf.String()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("X-Requested-With", "one")
		if runName != "" {
			req.Header.Set("X-Test-Run", runName)
		}
		rec := httptest.NewRecorder()
		e.h.ServeHTTP(rec, req)
		if rec.Code != 200 {
			t.Fatalf("to-do %q: %d %s", text, rec.Code, rec.Body.String())
		}
	}
	todo(run, "Marked by hand")
	todo("not a run name", "Bad shape")
	todo("", "Plain")
	var marked, plain int
	e.db.QueryRow(`SELECT COUNT(*) FROM agenda WHERE test_run = ?`, run).Scan(&marked)
	e.db.QueryRow(`SELECT COUNT(*) FROM agenda WHERE test_run = '' OR test_run IS NULL`).Scan(&plain)
	if marked != 1 || plain != 2 {
		t.Fatalf("marked %d (want 1), unmarked %d (want 2)", marked, plain)
	}
	code, ans := e.removeVia(e.a.agentTokenValue(), run)
	if code != 200 || removedCount(ans, "agenda") != 1 {
		t.Fatalf("removing the run: %d %v", code, ans)
	}
	var left int
	e.db.QueryRow(`SELECT COUNT(*) FROM agenda`).Scan(&left)
	if left != 2 {
		t.Errorf("%d to-dos left, want the 2 that carried no mark", left)
	}
}
