package main

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"lab/apps/one/repo"
)

// Tests of history in batches (SPEC 34). All data is invented.

type histEnv struct{ *claudeEnv }

func newHistEnv(t *testing.T) *histEnv {
	t.Helper()
	e := &histEnv{newClaudeEnv(t)}
	t.Cleanup(func() { historyReviewOn = historyReviewEnabled })
	return e
}

// old makes an old mail thread in the stage history: the first message is from
// someone else, and with mine a message of the owner's follows an hour later.
func (e *histEnv) old(subject, sender string, days float64, mine bool) repo.Item {
	e.t.Helper()
	m := e.mail(subject, days)
	m.sender, m.name = sender, "Sender of "+subject
	msgs := []seedMsg{m}
	if mine {
		r := e.mail("Re: "+subject, days)
		r.at, r.sender, r.name, r.mine = m.at+3600, "owner@example.test", "the owner", true
		msgs = append(msgs, r)
	}
	return e.seed(stageHistory, msgs...)
}

// tool calls a tool through the connector; the answer is its JSON text.
func (e *histEnv) tool(name string, args map[string]any, run string) (map[string]any, bool) {
	e.t.Helper()
	b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": name, "arguments": args}})
	req := httptest.NewRequest("POST", "/api/mcp", strings.NewReader(string(b)))
	req.Header.Set("Authorization", "Bearer "+e.a.agentTokenValue())
	if run != "" {
		req.Header.Set("X-Test-Run", run)
	}
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	var rpc struct {
		Result struct {
			Content []struct{ Text string }
			IsError bool `json:"isError"`
		}
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &rpc); err != nil || len(rpc.Result.Content) == 0 {
		e.t.Fatalf("%s: %s", name, rec.Body.String())
	}
	var out map[string]any
	if json.Unmarshal([]byte(rpc.Result.Content[0].Text), &out) != nil {
		out = map[string]any{"text": rpc.Result.Content[0].Text}
	}
	return out, rpc.Result.IsError
}

func (e *histEnv) batch(args map[string]any) map[string]any {
	e.t.Helper()
	out, bad := e.tool("history_batch", args, "")
	if bad {
		e.t.Fatalf("history_batch %v: %v", args, out)
	}
	return out
}

func (e *histEnv) record(it repo.Item, outcome string, attention string) (map[string]any, bool) {
	args := map[string]any{"id": ref(it), "summary": "Short summary of " + it.Subject + ".", "outcome": outcome, "agent": "Claude"}
	if attention != "" {
		args["needs_attention"], args["reason"] = true, attention
	}
	return e.tool("review_history_thread", args, "")
}

func subjects(out map[string]any) []string {
	var s []string
	for _, x := range out["items"].([]any) {
		s = append(s, x.(map[string]any)["subject"].(string))
	}
	return s
}

func (e *histEnv) count() int { return e.countEvents() }

func TestSizingHandsOutNothingAndMovesNothing(t *testing.T) {
	e := newHistEnv(t)
	e.old("Oak 40", "a@shop.test", 40, false)
	e.old("Oak 50", "a@shop.test", 50, false)
	e.old("Oak 150", "a@shop.test", 150, false)
	e.seed(stageHistory, e.mail("Inside the window", 10)) // newest message is inside the review window
	before := e.count()
	out := e.batch(map[string]any{"months": 3})
	if out["threads"] != float64(2) || out["handed_out"] != float64(0) || out["items"] != nil || out["mode"] != "size" {
		t.Fatalf("sizing: %v", out)
	}
	want := 2 * len("Body of Oak 40.")
	if out["characters"] != float64(want) {
		t.Errorf("characters %v, want %d", out["characters"], want)
	}
	est := out["estimate"].(map[string]any)
	if est["model"] != e.a.reviewModel(e.ctx) || est["usd"].(float64) <= 0 || !strings.Contains(est["is_an_estimate"].(string), "estimate") {
		t.Errorf("estimate: %v", est)
	}
	if out["point_reached"] != nil || e.count() != before {
		t.Errorf("sizing moved something: point %v, events %d -> %d", out["point_reached"], before, e.count())
	}
	// Asking again gives the same; a bigger period holds the third thread.
	if again := e.batch(map[string]any{"months": 3}); again["threads"] != float64(2) {
		t.Errorf("again: %v", again)
	}
	if big := e.batch(map[string]any{"months": 5}); big["threads"] != float64(3) {
		t.Errorf("five months: %v", big)
	}
	// A named period works and a bad one says why.
	d := func(days float64) string { return time.Unix(e.ago(days), 0).In(e.a.loc).Format("2006-01-02") }
	if named := e.batch(map[string]any{"from": d(60), "to": d(45)}); named["threads"] != float64(1) {
		t.Errorf("named: %v", named)
	}
	if _, bad := e.tool("history_batch", map[string]any{}, ""); !bad {
		t.Error("a call with no period is refused")
	}
	if _, bad := e.tool("history_batch", map[string]any{"months": 2, "from": d(60), "to": d(45)}, ""); !bad {
		t.Error("months and from/to together are refused")
	}
	if _, bad := e.tool("history_batch", map[string]any{"from": d(5), "to": d(1)}, ""); !bad {
		t.Error("a period inside the review window is refused")
	}
}

func TestBatchesGoBackwardAndThePointMovesOnlyWithRecordedThreads(t *testing.T) {
	e := newHistEnv(t)
	t40 := e.old("Oak 40", "a@shop.test", 40, false)
	t50 := e.old("Oak 50", "a@shop.test", 50, false)
	t90 := e.old("Oak 90", "a@shop.test", 90, false)
	out := e.batch(map[string]any{"months": 3, "confirm": true})
	if got := strings.Join(subjects(out), ","); got != "Oak 40,Oak 50,Oak 90" || out["handed_out"] != float64(3) {
		t.Fatalf("order: %v", out)
	}
	first := out["items"].([]any)[0].(map[string]any)
	for _, k := range []string{"id", "subject", "people", "first_at", "last_at", "messages", "characters", "preview"} {
		if _, ok := first[k]; !ok {
			t.Errorf("hand-out lacks %s: %v", k, first)
		}
	}
	if out["point_reached"] != nil {
		t.Error("handing out moved the point")
	}
	// Recording the newest moves the point to it; the next batch starts there.
	if res, bad := e.record(t40, "answered", ""); bad || res["recorded"] != true {
		t.Fatalf("record: %v", res)
	}
	next := e.batch(map[string]any{"months": 1, "confirm": true})
	if got := strings.Join(subjects(next), ","); got != "Oak 50" {
		t.Errorf("next batch: %v", next)
	}
	if next["point_reached"] != time.Unix(t40.LastAt, 0).In(e.a.loc).Format("2006-01-02") {
		t.Errorf("point: %v", next["point_reached"])
	}
	e.record(t50, "ignored", "")
	if got := strings.Join(subjects(e.batch(map[string]any{"months": 2, "confirm": true})), ","); got != "Oak 90" {
		t.Errorf("after two: %s", got)
	}
	_ = t90
}

func TestOnlyMineKeepsThreadsOwnerTookPartIn(t *testing.T) {
	e := newHistEnv(t)
	e.old("Quote from joiner", "joiner@shop.test", 40, true)
	e.old("Newsletter", "news@shop.test", 45, false)
	out := e.batch(map[string]any{"months": 2, "only_mine": true, "confirm": true})
	if got := strings.Join(subjects(out), ","); got != "Re: Quote from joiner" && got != "Quote from joiner" || out["threads"] != float64(1) {
		t.Errorf("only_mine: %v", out)
	}
	if all := e.batch(map[string]any{"months": 2}); all["threads"] != float64(2) {
		t.Errorf("all: %v", all)
	}
}

func TestAStoppedBatchContinuesAfterTheDoneThreads(t *testing.T) {
	e := newHistEnv(t)
	var its []repo.Item
	for i := 0; i < 5; i++ {
		its = append(its, e.old("Thread "+string(rune('A'+i)), "a@shop.test", float64(40+i*3), false))
	}
	p1 := e.batch(map[string]any{"months": 3, "confirm": true, "limit": 2})
	if p1["more"] != true || p1["handed_out"] != float64(2) || p1["next_after"] != ref(its[1]) {
		t.Fatalf("page 1: %v", p1)
	}
	// The owner stops here: only the first one was recorded.
	e.record(its[0], "answered", "")
	again := e.batch(map[string]any{"months": 3, "confirm": true, "limit": 10})
	if got := strings.Join(subjects(again), ","); got != "Thread B,Thread C,Thread D,Thread E" {
		t.Errorf("resume: %s", got)
	}
	if res, bad := e.record(its[0], "answered", ""); !bad {
		t.Errorf("a thread is recorded once: %v", res)
	}
	// Paging by after goes on behind the page before.
	p2 := e.batch(map[string]any{"months": 3, "confirm": true, "limit": 2, "after": ref(its[1])})
	if got := strings.Join(subjects(p2), ","); got != "Thread C,Thread D" {
		t.Errorf("page 2: %s", got)
	}
}

func TestNeedsAttentionIsTheOnlyWayIntoNew(t *testing.T) {
	e := newHistEnv(t)
	quiet := e.old("Invoice 2025", "bill@shop.test", 40, false)
	open := e.old("Question never answered", "boss@firm.test", 50, false)
	e.addUnrelatedNew()
	if res, bad := e.record(quiet, "still_open", ""); bad || res["appeared_in_new"] != false {
		t.Fatalf("quiet: %v", res)
	}
	if _, bad := e.tool("review_history_thread", map[string]any{"id": ref(open), "summary": "s", "outcome": "answered", "reason": "why"}, ""); !bad {
		t.Error("a reason without needs_attention is refused")
	}
	if _, bad := e.tool("review_history_thread", map[string]any{"id": ref(open), "summary": "s", "outcome": "answered", "needs_attention": true}, ""); !bad {
		t.Error("needs_attention without a reason is refused")
	}
	if _, bad := e.tool("review_history_thread", map[string]any{"id": ref(open), "summary": "s", "outcome": "maybe"}, ""); !bad {
		t.Error("an unknown outcome is refused")
	}
	if res, bad := e.record(open, "still_open", "The owner's question to the firm never got an answer"); bad || res["appeared_in_new"] != true {
		t.Fatalf("flagged: %v", res)
	}
	q, o := e.item(quiet.ID), e.item(open.ID)
	if q.Stage != stageHistory || q.Hidden || q.NeedsClaude {
		t.Errorf("quiet thread: %+v", q)
	}
	if o.Stage != stageNew || o.Hidden || o.NeedsClaude {
		t.Errorf("flagged thread: %+v", o)
	}
	// Nothing is in In progress, Pending or Resolved, and New holds only the flagged one and the unrelated one.
	for _, st := range []string{"in_progress", "pending", "resolved"} {
		if n := e.getJSON("/api/stage-items?stage=" + st)["total"]; n != float64(0) {
			t.Errorf("%s holds %v", st, n)
		}
	}
	if n := e.getJSON("/api/stage-items?stage=new")["total"]; n != float64(2) {
		t.Errorf("New holds %v", n)
	}
	// The owner sees the notice in the exact words and the summary on the thread, also for the quiet one.
	page := get(e.h, "/items/"+itoa(open.ID)).Body.String()
	if !strings.Contains(page, "History: the owner&#39;s question to the firm never got an answer") && !strings.Contains(page, "History: The owner&#39;s question to the firm never got an answer") {
		t.Errorf("notice missing on the item page")
	}
	if !strings.Contains(get(e.h, "/items/"+itoa(quiet.ID)).Body.String(), "Short summary of Invoice 2025.") {
		t.Error("the summary is not on the item")
	}
	if len(e.events(quiet.ID, "commented")) != 0 {
		t.Error("a history summary must not be a comment (it would count as the sender having mattered)")
	}
}

func (e *histEnv) addUnrelatedNew() {
	e.seed(stageNew, e.mail("Something new", 1))
}

func TestTheAppsOwnModelAndTheNightCheckNeverRunOverHistory(t *testing.T) {
	e := newHistEnv(t)
	e.saveKey()
	var its []repo.Item
	for i := 0; i < 4; i++ {
		its = append(its, e.old("Old "+string(rune('A'+i)), "a@shop.test", float64(40+i), false))
	}
	e.batch(map[string]any{"months": 3})
	e.batch(map[string]any{"months": 3, "confirm": true})
	for i, it := range its {
		e.record(it, "answered", map[bool]string{true: "needs a look"}[i == 3])
	}
	if got := e.a.store.CountWaiting(e.ctx); got != 0 {
		t.Errorf("%d items wait for Claude", got)
	}
	e.a.reviewPass(e.ctx)
	e.a.nightCheck(e.ctx)
	lines, _, err := e.a.nightLines(e.ctx, e.now.Add(-24*time.Hour).Unix())
	if err != nil || len(lines) != 0 {
		t.Errorf("the night check sees %d lines of history (%v)", len(lines), err)
	}
	if n := e.fake.count(); n != 0 {
		t.Errorf("the app called its model %d times", n)
	}
	if n := e.a.store.LastUsageAt(e.ctx, "review") + e.a.store.LastUsageAt(e.ctx, "night"); n != 0 {
		t.Error("the app spent money")
	}
	// And no notice reached the owner's phone.
	if n := len(e.told.all()); n != 0 {
		t.Errorf("%d notices sent", n)
	}
}

func TestTheSwitchMakesBothToolsRefuseAndHidesTheSectionButSummariesStayReadable(t *testing.T) {
	e := newHistEnv(t)
	it := e.old("Old one", "a@shop.test", 40, false)
	other := e.old("Old two", "a@shop.test", 41, false)
	if _, bad := e.record(it, "answered", ""); bad {
		t.Fatal("record")
	}
	if !strings.Contains(get(e.h, "/settings").Body.String(), "History review") {
		t.Error("the section is missing while on")
	}
	historyReviewOn = false
	for name, args := range map[string]map[string]any{"history_batch": {"months": 2}, "review_history_thread": {"id": ref(other), "summary": "s", "outcome": "answered"}} {
		out, bad := e.tool(name, args, "")
		if !bad || !strings.Contains(out["text"].(string), "switched off") {
			t.Errorf("%s while off: %v", name, out)
		}
	}
	if e.a.store.HistoryReviewed(e.ctx, other.ID) {
		t.Error("a refused tool recorded something")
	}
	if strings.Contains(get(e.h, "/settings").Body.String(), "History review") {
		t.Error("the section shows while off")
	}
	if !strings.Contains(get(e.h, "/items/"+itoa(it.ID)).Body.String(), "Short summary of Old one.") {
		t.Error("a recorded summary is not readable while off")
	}
}

func TestATestRunSeesAndRecordsOnlyItsOwnHistoryThreads(t *testing.T) {
	e := newHistEnv(t)
	real := e.old("Real old", "a@shop.test", 40, false)
	run := "e2e-histrun01"
	e.ctx = repo.WithRun(context.Background(), run)
	marked := e.old("Marked old", "a@shop.test", 41, false)
	e.ctx = context.Background()
	if marked.TestRun != run {
		t.Fatalf("the seeded thread is not marked: %q", marked.TestRun)
	}
	out, bad := e.tool("history_batch", map[string]any{"months": 2, "confirm": true}, run)
	if bad || strings.Join(subjects(out), ",") != "Marked old" {
		t.Fatalf("run sees: %v", out)
	}
	if real := e.batch(map[string]any{"months": 2, "confirm": true}); strings.Join(subjects(real), ",") != "Real old" {
		t.Errorf("real sees: %v", real)
	}
	if _, bad := e.tool("review_history_thread", map[string]any{"id": ref(real), "summary": "s", "outcome": "answered"}, run); !bad {
		t.Error("a run recorded a real thread")
	}
	if res, bad := e.tool("review_history_thread", map[string]any{"id": ref(marked), "summary": "s", "outcome": "answered"}, run); bad {
		t.Fatalf("run record: %v", res)
	}
	evs, _ := e.a.store.RawEvents(e.ctx, "history_reviewed")
	if len(evs) != 1 {
		t.Errorf("events %v", evs)
	}
	if got := e.a.historyReviewFor(e.ctx).Line; got != "Not started" {
		t.Errorf("a run moved the owner's point: %s", got)
	}
	// Removing the run takes its record with it and nothing else.
	if err := e.a.store.Update(e.ctx, func(tx *repo.Tx) error { _, _, err := tx.RemoveRun(e.ctx, run); return err }); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(e.batch(map[string]any{"months": 2, "confirm": true})["items"].([]any)[0].(map[string]any)["subject"].(string), "Real old") {
		t.Error("the real thread is gone")
	}
}

func TestSettingsSaysNotStartedThenHowFarBackAndHowManyAreLeft(t *testing.T) {
	e := newHistEnv(t)
	t40 := e.old("Old 40", "a@shop.test", 40, false)
	e.old("Old 60", "a@shop.test", 60, false)
	e.old("Old 80", "a@shop.test", 80, false)
	page := func() string { return get(e.h, "/settings").Body.String() }
	if p := page(); !strings.Contains(p, "History review") || !strings.Contains(p, "Not started") {
		t.Error("before any batch")
	}
	e.batch(map[string]any{"months": 3, "confirm": true})
	if !strings.Contains(page(), "Not started") {
		t.Error("handing out started it")
	}
	e.record(t40, "answered", "")
	want := "Reviewed back to " + time.Unix(t40.LastAt, 0).In(e.a.loc).Format("2 Jan 2006") + ", 2 threads left"
	if !strings.Contains(page(), want) {
		t.Errorf("want %q in\n%s", want, e.a.historyReviewFor(e.ctx).Line)
	}
}

func TestRulesAndNotesComeOutThroughTheExistingToolsForHistorySenders(t *testing.T) {
	e := newHistEnv(t)
	it := e.old("Bulk offer", "bulk@shop.test", 40, false)
	e.record(it, "ignored", "")
	rule, bad := e.tool("propose_rule", map[string]any{"field": "sender", "value": "bulk@shop.test", "reason": "214 mails in three years, none opened, none answered", "agent": "Claude"}, "")
	if bad {
		t.Errorf("propose_rule on a history sender: %v", rule)
	}
	note, bad := e.tool("save_note", map[string]any{"title": "Shop offers", "text": "Offers from this shop are noise.", "sender": "bulk@shop.test", "item": ref(it), "agent": "Claude"}, "")
	if bad {
		t.Errorf("save_note on a history item and sender: %v", note)
	}
	// The gap: a skip rule needs skipped items, and history threads are never skipped.
	sk, bad := e.tool("propose_skip_rule", map[string]any{"field": "sender", "value": "bulk@shop.test", "reason": "skipped 0 times, brought back 0 times", "agent": "Claude"}, "")
	if !bad {
		t.Errorf("a skip rule from history alone was accepted: %v", sk)
	}
	if e.item(it.ID).Stage != stageHistory {
		t.Error("something moved the thread")
	}
}

func itoa(id int64) string { return strconv.FormatInt(id, 10) }
