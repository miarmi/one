package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"lab/apps/one/repo"
)

// Tests of the four eyes on what Claude closed (SPEC 27): the seen marks, the
// link, and the one summary row with its to-do.

func (e *claudeEnv) evening(hour int) {
	e.a.now = func() time.Time { return time.Date(2026, 11, 21, hour, 5, 0, 0, e.a.loc) }
}

// closeAs closes a new item the way an agent's clear skip does.
func (e *claudeEnv) closeAs(subject, kind, reason string) repo.Item {
	e.t.Helper()
	it := e.newItem(stageNew, true, subject, "")
	out := e.post("/api/items/"+ref(it)+"/skip", map[string]any{"reason": reason, "kind": kind, "agent": "Claude"})
	if out["_status"] != float64(200) {
		e.t.Fatalf("skip %s: %v", subject, out)
	}
	return e.item(it.ID)
}

func (e *claudeEnv) summaryRow(run string) (repo.Item, bool) {
	ctx := e.ctx
	if run != "" {
		ctx = repo.WithRun(ctx, run)
	}
	return e.a.store.ClosedSummaryRow(ctx, run)
}

func (e *claudeEnv) sync(evening bool) bool {
	e.t.Helper()
	told, err := e.a.closedSummarySync(e.ctx, "", evening)
	if err != nil {
		e.t.Fatal(err)
	}
	return told
}

func (e *claudeEnv) closedPage() string { return get(e.h, "/closed").Body.String() }

func TestSeenMarksLinkTextAndOnlyOwnerSetsThem(t *testing.T) {
	e := newClaudeEnv(t)
	a := e.closeAs("Newsletter one", "noise", "A newsletter that needs nothing from the owner.")
	b := e.closeAs("Newsletter two", "noise", "A newsletter that needs nothing from the owner.")
	e.closeAs("Newsletter three", "noise", "A newsletter that needs nothing from the owner.")
	if !strings.Contains(get(e.h, "/").Body.String(), `<a href="/closed">Closed (3, 3 not seen)</a>`) {
		t.Error("the link does not say how many are not seen")
	}
	page := e.closedPage()
	if strings.Count(page, "Not seen") != 3 || strings.Count(page, ">Seen<") != 3 || !strings.Contains(page, "Mark all seen") {
		t.Errorf("the page lacks the marks or the buttons: %d marks", strings.Count(page, "Not seen"))
	}
	// No agent can set it: the agent's token reaches nothing outside /api/, and no tool sets a seen mark.
	for _, p := range []string{fmt.Sprintf("/api/items/%d/closed-seen", a.ID), fmt.Sprintf("/api/items/%s/seen-closed", ref(a)), "/api/closed/seen-all"} {
		if rec := e.call("POST", p, e.a.agentTokenValue(), nil); rec.Code == 200 || rec.Code == 303 || e.a.store.CountUnseenClosed(e.ctx) != 3 {
			t.Errorf("an agent set a seen mark through %s: %d", p, rec.Code)
		}
	}
	for _, tl := range mcpToolList() {
		if strings.Contains(tl.Name, "seen") && tl.Name != "mark_seen" {
			t.Errorf("a tool could set seen marks: %s", tl.Name)
		}
	}
	if rec := post(e.h, fmt.Sprintf("/items/%d/closed-seen", a.ID), nil); rec.Code != 303 {
		t.Fatalf("Seen: %d", rec.Code)
	}
	if !strings.Contains(get(e.h, "/").Body.String(), "Closed (3, 2 not seen)") || strings.Count(e.closedPage(), "Not seen") != 2 {
		t.Error("Seen did not mark one entry")
	}
	// Bringing an item back clears nothing else.
	if rec := post(e.h, fmt.Sprintf("/items/%d/bring-back", b.ID), nil); rec.Code != 303 {
		t.Fatal(rec.Code)
	}
	if e.a.store.CountUnseenClosed(e.ctx) != 1 {
		t.Errorf("bringing back changed other marks: %d unseen", e.a.store.CountUnseenClosed(e.ctx))
	}
	if rec := post(e.h, "/closed/seen-all", nil); rec.Code != 303 {
		t.Fatal(rec.Code)
	}
	if !strings.Contains(get(e.h, "/").Body.String(), `<a href="/closed">Closed (2)</a>`) || strings.Contains(e.closedPage(), "Not seen") {
		t.Error("Mark all seen left a mark, or the link still counts")
	}
	// Closed again later: a new closing, not seen.
	e.closeAs("Newsletter four", "noise", "A newsletter that needs nothing from the owner.")
	if e.a.store.CountUnseenClosed(e.ctx) != 1 {
		t.Error("a new closing is not marked")
	}
}

func TestWhatWasClosedBeforeTheFeatureCountsAsSeen(t *testing.T) {
	e := newClaudeEnv(t)
	e.closeAs("Old one", "noise", "A newsletter that needs nothing from the owner.")
	e.closeAs("Old two", "noise", "A newsletter that needs nothing from the owner.")
	// what the migration does on the first start: the floor is the newest event
	e.a.store.Update(e.ctx, func(tx *repo.Tx) error {
		return tx.SetSetting(e.ctx, repo.SeenFloorKey, fmt.Sprint(tx.LastEventID(e.ctx)))
	})
	if e.a.store.CountUnseenClosed(e.ctx) != 0 || strings.Contains(e.closedPage(), "Not seen") {
		t.Error("what was closed before the floor is marked")
	}
	if e.sync(true) {
		t.Error("a summary was made for what was closed before")
	}
	if _, has := e.summaryRow(""); has {
		t.Error("a row was made for what was closed before")
	}
	e.closeAs("New one", "noise", "A newsletter that needs nothing from the owner.")
	if e.a.store.CountUnseenClosed(e.ctx) != 1 {
		t.Error("what is closed after the floor is not marked")
	}
}

func TestNothingClosedGivesNoSummaryAndNoNotice(t *testing.T) {
	e := newClaudeEnv(t)
	e.saveKey()
	e.evening(21)
	e.closeAs("By the owner", "noise", "x") // closed by Claude, then seen
	post(e.h, "/closed/seen-all", nil)
	e.a.closedSummaryRun(e.ctx, "", true)
	if _, has := e.summaryRow(""); has || len(e.told.all()) != 0 || e.fake.count() != 0 {
		t.Error("a summary, a notice or a model call with nothing to review")
	}
}

func TestTheSummaryIsOneRowWithOneTodoMadeAtOnceAsAPlainGroupedList(t *testing.T) {
	e := newClaudeEnv(t) // no key: the plain list, said in its first line
	e.evening(10)
	e.closeAs("Invoice due Friday", "old", "old: the date has passed, it was Friday the 14th")
	e.closeAs("Weekly newsletter", "noise", "A newsletter that needs nothing from the owner at all.")
	e.closeAs("Weekly newsletter two", "noise", "A newsletter that needs nothing from the owner at all.")
	e.closeAs("Chat about lunch", "finished", "The chat about lunch has finished.")
	e.a.closedSummaryRun(e.ctx, "", false)
	row, has := e.summaryRow("")
	if !has || row.Subject != "Closed by Claude: 4 to review" || row.NeedsClaude || row.Hidden || row.Stage != stageNew {
		t.Fatalf("the row: %+v %v", row, has)
	}
	todo, ok := e.a.store.OpenTodoOfItem(e.ctx, row.ID)
	if !ok || todo.Title != "Review what Claude closed" {
		t.Fatalf("the to-do: %+v %v", todo, ok)
	}
	n := e.told.all()
	if len(n) != 1 || n[0][0] != "Closed by Claude" || !strings.Contains(n[0][1], "4 to review") {
		t.Errorf("the notice: %v", n)
	}
	// the list: grouped by kind, the least sure first (the old invoice), each with its number as a link
	v := e.a.closedSummaryViewFor(e.ctx, row.ID)
	if v == nil || v.Count != 4 || len(v.Groups) != 3 {
		t.Fatalf("the view: %+v", v)
	}
	if v.Groups[0].Items[0].What != "Invoice due Friday" || v.Groups[0].Items[0].Doubt == "" {
		t.Errorf("the least sure item is not first with its doubt: %+v", v.Groups[0])
	}
	// With the absence "no note" gone, the newsletters and the finished chat each hold one
	// reason ("first message"): only the old invoice, with two, stands before them.
	if v.Groups[0].Items[0].What == "Chat about lunch" {
		t.Errorf("the finished chat stands first: %+v", v.Groups[0])
	}
	for _, g := range v.Groups {
		for _, it := range g.Items {
			if !strings.HasPrefix(it.Open, "/items/") || !strings.HasPrefix(it.Number, "Q-") || it.Who == "" {
				t.Errorf("an item lacks its number as a link or who it is from: %+v", it)
			}
		}
	}
	// no second row, no second notice, the same day
	e.a.closedSummaryRun(e.ctx, "", false)
	e.closeAs("Later one", "noise", "A newsletter that needs nothing from the owner at all.")
	e.a.closedSummaryRun(e.ctx, "", false)
	rows := 0
	e.a.store.Update(e.ctx, func(tx *repo.Tx) error {
		rows = tx.Q.CountKind(e.ctx, repo.ClosedSummaryKind)
		return nil
	})
	if rows != 1 || len(e.told.all()) != 1 {
		t.Errorf("rows %d, notices %d", rows, len(e.told.all()))
	}
	// the row is open in the list and says what it is from
	page := get(e.h, "/list/new").Body.String()
	if !strings.Contains(page, "Closed by Claude: 4 to review") {
		t.Error("the row is not in the list")
	}
	body := get(e.h, fmt.Sprintf("/items/%d", row.ID)).Body.String()
	if !strings.Contains(body, "Claude API: Not configured") || !strings.Contains(body, "Q-") {
		t.Errorf("the first line does not say why the model was not used")
	}
}

func TestResolvingTheRowOrTickingItsTodoMarksWhatItListsAsSeen(t *testing.T) {
	for _, how := range []string{"resolve", "todo"} {
		e := newClaudeEnv(t)
		e.evening(10)
		e.closeAs("One", "noise", "A newsletter that needs nothing from the owner at all.")
		e.closeAs("Two", "noise", "A newsletter that needs nothing from the owner at all.")
		e.a.closedSummaryRun(e.ctx, "", false)
		row, _ := e.summaryRow("")
		late := e.closeAs("Three", "noise", "A newsletter that needs nothing from the owner at all.") // closed after the row was written: not listed
		if how == "resolve" {
			if rec := post(e.h, fmt.Sprintf("/items/%d/resolve", row.ID), nil); rec.Code != 303 {
				t.Fatalf("resolve: %d", rec.Code)
			}
		} else {
			td, _ := e.a.store.OpenTodoOfItem(e.ctx, row.ID)
			if did, err := e.a.tickOff(e.ctx, td.ID); err != nil || !did {
				t.Fatal(did, err)
			}
		}
		if got := e.item(row.ID).Stage; got != stageResolved {
			t.Errorf("%s: the row is %s", how, got)
		}
		if _, has := e.a.store.OpenTodoOfItem(e.ctx, row.ID); has {
			t.Errorf("%s: the to-do is still open", how)
		}
		if ids, _ := e.a.store.UnseenClosedIDs(e.ctx, ""); len(ids) != 1 || ids[0] != late.ID {
			t.Errorf("%s: what is still not seen: %v", how, ids)
		}
		// the next thing Claude closed starts a new row and a new to-do
		e.a.closedSummaryRun(e.ctx, "", false)
		next, has := e.summaryRow("")
		if !has || next.ID == row.ID || next.Subject != "Closed by Claude: 1 to review" {
			t.Errorf("%s: no new row: %+v", how, next)
		}
	}
}

func TestSeenOnThePageKeepsTheRowInStepAndTheLastOneClosesIt(t *testing.T) {
	e := newClaudeEnv(t)
	e.evening(10)
	a := e.closeAs("One", "noise", "A newsletter that needs nothing from the owner at all.")
	b := e.closeAs("Two", "noise", "A newsletter that needs nothing from the owner at all.")
	e.a.closedSummaryRun(e.ctx, "", false)
	row, _ := e.summaryRow("")
	post(e.h, fmt.Sprintf("/items/%d/closed-seen", a.ID), nil)
	if r := e.item(row.ID); r.Subject != "Closed by Claude: 1 to review" || r.Stage != stageNew {
		t.Errorf("the count did not follow: %+v", r)
	}
	if v := e.a.closedSummaryViewFor(e.ctx, row.ID); v.Count != 1 || strings.Contains(fmt.Sprint(v.Groups), "One") && !strings.Contains(fmt.Sprint(v.Groups), "Two") {
		t.Errorf("the list did not follow: %+v", v)
	}
	post(e.h, fmt.Sprintf("/items/%d/closed-seen", b.ID), nil)
	if r := e.item(row.ID); r.Stage != stageResolved {
		t.Errorf("the last seen did not close the row: %s", r.Stage)
	}
	if _, has := e.a.store.OpenTodoOfItem(e.ctx, row.ID); has {
		t.Error("the to-do stays open")
	}
}

func TestAnItemBroughtBackFromTheSummaryWorksLikeBringBackAndLeavesTheList(t *testing.T) {
	e := newClaudeEnv(t)
	e.evening(10)
	a := e.closeAs("One", "noise", "A newsletter that needs nothing from the owner at all.")
	e.closeAs("Two", "noise", "A newsletter that needs nothing from the owner at all.")
	e.a.closedSummaryRun(e.ctx, "", false)
	row, _ := e.summaryRow("")
	rec := post(e.h, fmt.Sprintf("/items/%d/bring-back", a.ID), map[string][]string{"back": {fmt.Sprint(row.ID)}})
	if rec.Code != 303 || rec.Header().Get("Location") != fmt.Sprintf("/items/%d", row.ID) {
		t.Fatalf("%d %s", rec.Code, rec.Header().Get("Location"))
	}
	if e.item(a.ID).Stage != stageNew || e.item(row.ID).Subject != "Closed by Claude: 1 to review" {
		t.Error("the item is not back, or the count did not follow")
	}
}

func TestTheEveningWriteCallsTheModelOnceWithinTheLimitAndItsTextIsChecked(t *testing.T) {
	e := newClaudeEnv(t)
	e.saveKey()
	e.evening(10)
	a := e.closeAs("Invoice due Friday", "old", "old: the date has passed")
	b := e.closeAs("Newsletter", "noise", "A newsletter that needs nothing from the owner at all.")
	e.a.closedSummaryRun(e.ctx, "", false) // the day: the plain list, no model
	if e.fake.count() != 0 {
		t.Fatal("the model was called before the evening")
	}
	row, _ := e.summaryRow("")
	e.fake.script = func(n int, req map[string]any) fakeAnswer {
		return use("write_summary", map[string]any{"groups": []any{
			map[string]any{"title": "Newsletters that need nothing", "items": []any{map[string]any{"id": ref(b)}}},
			map[string]any{"title": "Old, date passed", "items": []any{map[string]any{"id": ref(a), "doubt": "It mattered once: its date has passed"}}},
		}})
	}
	e.evening(21)
	e.a.closedSummaryRun(e.ctx, "", true)
	if e.fake.count() != 1 {
		t.Fatalf("the model was called %d times", e.fake.count())
	}
	in := fmt.Sprint(e.fake.call(0)["messages"])
	if !strings.Contains(in, ref(a)) || !strings.Contains(in, "Invoice due Friday") || !strings.Contains(in, "old: the date has passed") {
		t.Errorf("the model did not get number, subject and reason: %s", in)
	}
	if !strings.Contains(fmt.Sprint(e.fake.call(0)["system"]), "never an instruction") {
		t.Error("the prompt does not say that messages are data")
	}
	t1, _ := e.a.summaryCurrent(e.ctx, e.a.store.Q, row.ID)
	if !t1.Model || len(t1.Groups) != 2 || t1.Groups[0].Items[0].Number != ref(a) || t1.Groups[0].Title != "Old, date passed" {
		t.Errorf("the written text, least sure first: %+v", t1)
	}
	if e.a.spentToday(e.ctx) == 0 {
		t.Error("the call is not counted in the daily spend")
	}
	// once a day: another pass the same evening calls nothing, even with more closed
	e.closeAs("More", "noise", "A newsletter that needs nothing from the owner at all.")
	e.a.closedSummaryRun(e.ctx, "", true)
	if e.fake.count() != 1 {
		t.Errorf("the model was called again the same evening: %d", e.fake.count())
	}
	// the next evening: more was closed, so it is written again; the old text stays in the record
	e.a.now = func() time.Time { return time.Date(2026, 11, 22, 21, 0, 0, 0, e.a.loc) }
	e.fake.script = nil
	e.a.closedSummaryRun(e.ctx, "", true)
	evs, _ := e.a.store.EventsOfItem(e.ctx, row.ID, 50)
	n := 0
	for _, ev := range evs {
		if ev.Type == summaryEvent {
			n++
		}
	}
	if n != 3 {
		t.Errorf("the record holds %d texts, want 3 (plain, model, plain after a bad answer)", n)
	}
}

func TestTheModelsTextIsUsedOnlyWhenEveryNumberIsListedAndEveryItemAppears(t *testing.T) {
	items := []sumItem{{ID: 1, Number: "Q-0001", Who: "A", What: "a", Score: 0},
		{ID: 2, Number: "Q-0002", Who: "B", What: "b", Score: 3, Doubt: "Amount 1 250 Kč", Facts: []string{"amount 1 250 Kč"}}}
	good := `{"groups":[{"title":"Why","items":[{"id":"Q-0001"},{"id":"Q-0002","doubt":"Names an amount of 1 250 Kč"}]}]}`
	gs, p := checkModelSummary(json.RawMessage(good), items)
	if p != "" || gs[0].Items[0].Number != "Q-0002" || gs[0].Items[0].What != "b" {
		t.Errorf("a good answer: %q %+v", p, gs)
	}
	for name, c := range map[string][2]string{
		"an unlisted number as an item": {`{"groups":[{"title":"Why","items":[{"id":"Q-0001"},{"id":"Q-0002"},{"id":"Q-0003"}]}]}`, "Q-0003"},
		"an unlisted number in a text":  {`{"groups":[{"title":"Why Q-0099","items":[{"id":"Q-0001"},{"id":"Q-0002"}]}]}`, "Q-0099"},
		"an item left out":              {`{"groups":[{"title":"Why","items":[{"id":"Q-0001"}]}]}`, "left out: Q-0002"},
		"an item twice":                 {`{"groups":[{"title":"Why","items":[{"id":"Q-0001"},{"id":"Q-0001"},{"id":"Q-0002"}]}]}`, "Q-0001 appears twice"},
		"a doubt without a fact":        {`{"groups":[{"title":"Why","items":[{"id":"Q-0001","doubt":"may matter"},{"id":"Q-0002"}]}]}`, "Q-0001 has no fact"},
		"an invented amount":            {`{"groups":[{"title":"Why","items":[{"id":"Q-0001"},{"id":"Q-0002","doubt":"Names an amount: 9 900 Kč"}]}]}`, "number that is not in its facts"},
		"no groups":                     {`{"groups":[]}`, "no groups"},
	} {
		if _, p := checkModelSummary(json.RawMessage(c[0]), items); p == "" || !strings.Contains(p, c[1]) {
			t.Errorf("%s: accepted or wrong reason: %q", name, p)
		}
	}
	// the app's own doubt stays for an item the model wrote none for
	if gs, _ := checkModelSummary(json.RawMessage(`{"groups":[{"title":"Why","items":[{"id":"Q-0001"},{"id":"Q-0002"}]}]}`), items); gs[0].Items[0].Doubt != "Amount 1 250 Kč" {
		t.Errorf("the doubt: %+v", gs[0].Items[0])
	}
}

func TestAnInventedNumberFallsBackToThePlainList(t *testing.T) {
	e := newClaudeEnv(t)
	e.saveKey()
	e.evening(21)
	e.closeAs("One", "noise", "A newsletter that needs nothing from the owner at all.")
	e.fake.script = func(n int, req map[string]any) fakeAnswer {
		return use("write_summary", map[string]any{"groups": []any{map[string]any{"title": "Why", "items": []any{map[string]any{"id": "Q-9999", "what": "x"}}}}})
	}
	e.a.closedSummaryRun(e.ctx, "", true)
	row, _ := e.summaryRow("")
	cur, _ := e.a.summaryCurrent(e.ctx, e.a.store.Q, row.ID)
	if cur.Model || len(cur.Groups) != 1 || cur.Groups[0].Items[0].What != "One" {
		t.Errorf("the plain list was not used: %+v", cur)
	}
}

func TestWithoutAKeyOverTheLimitOrWhenClaudeIsDownTheListIsPlainAndSaysWhy(t *testing.T) {
	cases := map[string]struct {
		setup func(e *claudeEnv)
		first string
	}{
		"no key": {func(e *claudeEnv) {}, "Claude API: Not configured"},
		"limit":  {func(e *claudeEnv) { e.saveKey(); e.a.store.SetSetting(e.ctx, keyDailyLimit, "0") }, "Review paused: today's limit is reached"},
		"down": {func(e *claudeEnv) {
			e.saveKey()
			e.fake.script = func(int, map[string]any) fakeAnswer { return fakeAnswer{netFail: true} }
		}, "Review paused: Claude cannot be reached"},
	}
	for name, c := range cases {
		e := newClaudeEnv(t)
		e.evening(21)
		c.setup(e)
		e.closeAs("One", "noise", "A newsletter that needs nothing from the owner at all.")
		e.a.closedSummaryRun(e.ctx, "", true)
		row, _ := e.summaryRow("")
		cur, _ := e.a.summaryCurrent(e.ctx, e.a.store.Q, row.ID)
		if cur.Model || cur.Intro != c.first || len(cur.Groups) != 1 {
			t.Errorf("%s: %+v", name, cur)
		}
		if name == "limit" && e.fake.count() != 0 {
			t.Error("the model was called over the limit")
		}
	}
}

func TestTheScheduleAndTheSetting(t *testing.T) {
	e := newClaudeEnv(t)
	e.saveKey()
	e.closeAs("One", "noise", "A newsletter that needs nothing from the owner at all.")
	e.evening(19)
	e.a.closedSummaryTick(e.ctx)
	if _, has := e.summaryRow(""); !has || e.fake.count() != 0 {
		t.Error("before 20:00 the row exists at once and the model is not called")
	}
	e.fake.script = func(int, map[string]any) fakeAnswer {
		return use("write_summary", map[string]any{"groups": []any{map[string]any{"title": "Why", "items": []any{map[string]any{"id": "Q-0001", "what": "x"}}}}})
	}
	e.evening(20)
	e.a.closedSummaryTick(e.ctx)
	if e.fake.count() != 1 {
		t.Errorf("at 20:00 the model was called %d times", e.fake.count())
	}
	// the setting off: no row for what is closed, no call
	e2 := newClaudeEnv(t)
	e2.saveKey()
	e2.evening(21)
	post(e2.h, "/settings", map[string][]string{keyClosedSummary: {"off"}})
	e2.closeAs("One", "noise", "A newsletter that needs nothing from the owner at all.")
	e2.a.closedSummaryTick(e2.ctx)
	if _, has := e2.summaryRow(""); has || e2.fake.count() != 0 {
		t.Error("the summary was written with the setting off")
	}
	if page := get(e2.h, "/settings").Body.String(); !strings.Contains(page, "Summary of what Claude closed") || !strings.Contains(page, ">On<") {
		t.Error("the setting is not on the page with On and Off")
	}
}

func TestATestRunsClosedItemsNeverEnterARealSummary(t *testing.T) {
	e := newClaudeEnv(t)
	e.evening(21)
	run := "e2e-summary01"
	rctx := repo.WithRun(e.ctx, run)
	if err := e.a.store.Update(rctx, func(tx *repo.Tx) error {
		it, err := tx.CreateItem(rctx, repo.Item{Kind: "thread", Subject: "A test item", Stage: stageNew, FirstAt: 5, LastAt: 5, MovedAt: 5}, "test_thread", "run-item", "system", 5)
		if err != nil {
			return err
		}
		if err := tx.SetItemStage(rctx, it.ID, stageClosed, 6); err != nil {
			return err
		}
		return tx.InsertEvent(rctx, 6, it.ID, "claude", "skipped", map[string]any{"kind": "noise", "reason": "A newsletter that needs nothing from the owner."})
	}); err != nil {
		t.Fatal(err)
	}
	e.a.closedSummaryRun(e.ctx, "", true)
	if _, has := e.summaryRow(""); has || len(e.told.all()) != 0 {
		t.Fatal("a test run's item entered a real summary")
	}
	rec := e.call("POST", "/api/test-runs/"+run+"/closed-summary", e.a.agentTokenValue(), nil)
	var out map[string]any
	json.Unmarshal(rec.Body.Bytes(), &out)
	if rec.Code != 200 || out["subject"] != "Closed by Claude: 1 to review" {
		t.Fatalf("the test path: %d %v", rec.Code, out)
	}
	if _, has := e.summaryRow(""); has {
		t.Error("the run's row is a real row")
	}
	row, has := e.summaryRow(run)
	if !has {
		t.Fatal("no row for the run")
	}
	if len(e.told.all()) != 0 {
		t.Error("the phone was told about a test run")
	}
	if counts, err := e.a.removeRun(context.Background(), run); err != nil || counts["items"] < 1 {
		t.Errorf("the run's rows are not removable with it: %v %v", counts, err)
	}
	if _, ok := e.a.store.ItemByID(e.ctx, row.ID); ok {
		t.Error("the summary row of the run stays after the run is removed")
	}
	// a bad run name is refused
	if rec := e.call("POST", "/api/test-runs/real/closed-summary", e.a.agentTokenValue(), nil); rec.Code != 400 {
		t.Errorf("a bad run name: %d", rec.Code)
	}
}

func TestNoAgentCanSkipOrReplyOnTheSummaryRowAndItDoesNotWaitForClaude(t *testing.T) {
	e := newClaudeEnv(t)
	e.evening(10)
	e.closeAs("One", "noise", "A newsletter that needs nothing from the owner at all.")
	e.a.closedSummaryRun(e.ctx, "", false)
	row, _ := e.summaryRow("")
	if row.NeedsClaude || e.a.store.CountWaiting(e.ctx) != 0 {
		t.Error("the row waits for Claude by itself")
	}
	if out := e.post("/api/items/"+ref(row)+"/skip", map[string]any{"reason": "an invented reason", "kind": "noise", "agent": "Claude"}); out["_status"] == float64(200) {
		t.Error("an agent closed the summary row")
	}
	if out := e.post("/api/items/"+ref(row)+"/reply-proposals", map[string]any{"text": "hello", "reason": "x", "agent": "Claude"}); out["_status"] == float64(200) {
		t.Error("a reply was proposed on the summary row")
	}
	// an agent reading the row sees what it lists
	got := e.getJSON("/api/items/" + ref(row))
	if fmt.Sprint(got["item"]) == "" || !strings.Contains(fmt.Sprint(got), "closed_summary") {
		t.Errorf("an agent does not see the list: %v", got)
	}
	// a note of the owner's on it makes it wait, as on any row
	rec := post(e.h, fmt.Sprintf("/items/%d/notes", row.ID), map[string][]string{"text": {"Why was One closed?"}})
	if rec.Code >= 400 || !e.item(row.ID).NeedsClaude {
		t.Errorf("a note does not make it wait for Claude: %d", rec.Code)
	}
}

// --- the first real summary was not good enough (2026-11-21) ---

// realShape is what the Messages API answers to a forced or free use of the
// tool, as the review client parses it: a message with a tool_use block whose
// input is the tool's arguments. A recorded shape, so a mismatch cannot pass.
const realShape = `{"id":"msg_01Abc","type":"message","role":"assistant","model":"claude-sonnet-5-5",
 "content":[{"type":"text","text":"Writing it."},{"type":"tool_use","id":"toolu_01Xyz","name":"write_summary",
 "input":{"groups":[{"title":"Newsletters that need nothing","items":[{"id":"%s","doubt":""}]}]}}],
 "stop_reason":"tool_use","stop_sequence":null,"usage":{"input_tokens":812,"output_tokens":96}}`

func TestARealShapedAnswerIsParsedAndUsed(t *testing.T) {
	var ans claudeAnswer
	if err := json.Unmarshal([]byte(fmt.Sprintf(realShape, "Q-0001")), &ans); err != nil || len(ans.Content) != 2 || ans.Content[1].Name != "write_summary" {
		t.Fatalf("the recorded shape is not parsed: %v %+v", err, ans)
	}
	items := []sumItem{{ID: 1, Number: "Q-0001", Who: "N", What: "A newsletter"}}
	if gs, p := checkModelSummary(ans.Content[1].Input, items); p != "" || gs[0].Items[0].What != "A newsletter" {
		t.Errorf("the recorded input does not pass the check: %q", p)
	}
}

func TestTheRequestHasNothingTheRealAPIRefuses(t *testing.T) {
	e := newClaudeEnv(t)
	e.saveKey()
	e.evening(21)
	it := e.closeAs("Weekly newsletter", "noise", "A newsletter that needs nothing from the owner.")
	e.fake.script = func(n int, req map[string]any) fakeAnswer {
		return use("write_summary", map[string]any{"groups": []any{map[string]any{"title": "Newsletters", "items": []any{map[string]any{"id": ref(it)}}}}})
	}
	e.a.closedSummaryRun(e.ctx, "", true)
	req := e.fake.call(0)
	if _, forced := req["tool_choice"]; forced {
		t.Error("the request forces a tool: models that think refuse that")
	}
	if req["model"] != e.a.reviewModel(e.ctx) || req["max_tokens"].(float64) > 8000 || req["system"] == "" {
		t.Errorf("model, max_tokens or system: %v %v", req["model"], req["max_tokens"])
	}
	row, _ := e.summaryRow("")
	if cur, _ := e.a.summaryCurrent(e.ctx, e.a.store.Q, row.ID); !cur.Model {
		t.Errorf("the model's text was not used: %+v", cur)
	}
}

func TestWhateverFailsIsLoggedAtErrorAndTheFallbackLineIsTrue(t *testing.T) {
	cases := map[string]struct {
		ans   fakeAnswer
		intro string
		cause string
	}{
		"a refused request": {fakeAnswer{status: 400, errType: "invalid_request_error"}, "", "did not accept the request (status 400, invalid_request_error)"},
		"overloaded":        {fakeAnswer{status: 529, errType: "overloaded_error"}, "", "status 529"},
		"no connection":     {fakeAnswer{netFail: true}, introDown, "could not be reached"},
		"a refused key":     {fakeAnswer{status: 401}, "", "refused the key"},
	}
	for name, c := range cases {
		e := newClaudeEnv(t)
		lines := capturedLog(e.a)
		e.saveKey()
		e.evening(21)
		e.fake.script = func(int, map[string]any) fakeAnswer { return c.ans }
		e.closeAs("One", "noise", "A newsletter that needs nothing from the owner.")
		e.a.closedSummaryRun(e.ctx, "", true)
		row, _ := e.summaryRow("")
		cur, _ := e.a.summaryCurrent(e.ctx, e.a.store.Q, row.ID)
		if cur.Model || cur.Intro != c.intro || len(cur.Groups) != 1 {
			t.Errorf("%s: %+v", name, cur)
		}
		found := false
		for _, l := range lines() {
			if l["level"] == "ERROR" && strings.Contains(fmt.Sprint(l["msg"]), "the model call failed") && strings.Contains(fmt.Sprint(l["cause"]), c.cause) {
				found = true
			}
			if strings.Contains(fmt.Sprint(l), testClaudeKey) || strings.Contains(fmt.Sprint(l), "One") && strings.Contains(fmt.Sprint(l), "newsletter") {
				t.Errorf("%s: the log holds the key or a text: %v", name, l)
			}
		}
		if !found {
			t.Errorf("%s: nothing logged at error with the cause %q: %v", name, c.cause, lines())
		}
	}
}

func TestARefusedTextIsLoggedAsItsOwnCauseAndTheModelGetsOneRetryWithWhatWasMissing(t *testing.T) {
	e := newClaudeEnv(t)
	lines := capturedLog(e.a)
	e.saveKey()
	e.evening(21)
	a := e.closeAs("Newsletter one", "noise", "A newsletter that needs nothing from the owner.")
	b := e.closeAs("Newsletter two", "noise", "A newsletter that needs nothing from the owner.")
	e.fake.script = func(n int, req map[string]any) fakeAnswer {
		items := []any{map[string]any{"id": ref(a)}}
		if n == 2 {
			items = append(items, map[string]any{"id": ref(b)})
		}
		return use("write_summary", map[string]any{"groups": []any{map[string]any{"title": "Newsletters", "items": items}}})
	}
	e.a.closedSummaryRun(e.ctx, "", true)
	if e.fake.count() != 2 {
		t.Fatalf("the model was called %d times, want one retry", e.fake.count())
	}
	if msg := fmt.Sprint(e.fake.call(1)["messages"]); !strings.Contains(msg, "left out: "+ref(b)) {
		t.Errorf("the retry does not say what was missing: %s", msg)
	}
	row, _ := e.summaryRow("")
	if cur, _ := e.a.summaryCurrent(e.ctx, e.a.store.Q, row.ID); !cur.Model {
		t.Errorf("the second answer was not used: %+v", cur)
	}
	ok := false
	for _, l := range lines() {
		ok = ok || l["level"] == "ERROR" && strings.Contains(fmt.Sprint(l["msg"]), "refused by the check") && strings.Contains(fmt.Sprint(l["cause"]), "left out")
	}
	if !ok {
		t.Errorf("the refusal is not logged as its own cause: %v", lines())
	}
	// refused twice: the plain list, no intro line that would not be true
	e2 := newClaudeEnv(t)
	e2.saveKey()
	e2.evening(21)
	e2.closeAs("Only", "noise", "A newsletter that needs nothing from the owner.")
	e2.fake.script = func(int, map[string]any) fakeAnswer { return say("I cannot do that.") }
	e2.a.closedSummaryRun(e2.ctx, "", true)
	row2, _ := e2.summaryRow("")
	if cur, _ := e2.a.summaryCurrent(e2.ctx, e2.a.store.Q, row2.ID); cur.Model || cur.Intro != "" || e2.fake.count() != 2 {
		t.Errorf("twice refused: %+v after %d calls", cur, e2.fake.count())
	}
}

func TestDoubtsAreFactsOfTheItemNeverWordsOfClaudesOwnReason(t *testing.T) {
	e := newClaudeEnv(t)
	// the newsletter of the first real summary: Claude's reason denies every word that used to match
	news := e.closeAs("But in the more tender moments", "noise", "A newsletter with no deadline, payment, account change or request for the owner.")
	e.seedMail(srcGmail, "ana@example.test", "Re: Your invoice 2026/14", "h1", "Please pay 1 250 Kč by 15. 12. 2026.", 2, false)
	person := e.closeAs("Re: Lunch on Friday", "noise", "Nothing is asked of the owner.")
	rows := e.a.gatherClosed(e.ctx, []int64{news.ID, person.ID})
	var n, p sumItem
	for _, r := range rows {
		if r.ID == news.ID {
			n = r
		} else {
			p = r
		}
	}
	// the weak fact alone gives no doubt: no doubt line, last
	if n.Doubt != "" && !strings.HasPrefix(n.Doubt, "First message from this sender") && !strings.HasPrefix(n.Doubt, "Addressed to you") {
		t.Errorf("a doubt that is no fact: %q", n.Doubt)
	}
	if strings.Contains(strings.ToLower(n.Doubt), "money") || strings.Contains(n.Doubt, "reason") {
		t.Errorf("the doubt reads Claude's reason: %q", n.Doubt)
	}
	if p.What != "Lunch on Friday" {
		t.Errorf("the what is the cleaned subject: %q", p.What)
	}
}

func TestFactsAreFoundInTheMessageAndStatedPlainly(t *testing.T) {
	if got := firstAmount("Please pay 1 250 Kč by Friday"); got != "1 250 Kč" {
		t.Errorf("amount: %q", got)
	}
	if got := firstAmount("Only $49.90 today"); got != "$49.90" {
		t.Errorf("amount: %q", got)
	}
	if got := firstAmount("Version 3.5.1 is out"); got != "" {
		t.Errorf("a version is no amount: %q", got)
	}
	now, loc := time.Date(2026, 11, 23, 20, 0, 0, 0, time.UTC), time.UTC
	if got := firstDate("due 15. 12. 2026 at the office", now, loc); got != "15. 12. 2026" {
		t.Errorf("date: %q", got)
	}
	if got := firstDate("see 3.5.1 and 45.13.", now, loc); got != "" {
		t.Errorf("not dates: %q", got)
	}
	if got := firstDate("Meeting on 5 December", now, loc); got != "5 December" {
		t.Errorf("date: %q", got)
	}
	score, doubt, _ := doubtOfFacts([]fact{{1, "closed with no note of yours covering it"}})
	if score != 0 || doubt != "" {
		t.Errorf("a weak fact alone: %d %q", score, doubt)
	}
	_, doubt, _ = doubtOfFacts([]fact{{2, "first message from this sender"}, {2, "amount 1 250 Kč"}, {1, "closed with no note of yours covering it"}})
	if doubt != "First message from this sender; amount 1 250 Kč" {
		t.Errorf("doubt: %q", doubt)
	}
	for in, want := range map[string]string{"Re: RE: Fwd:   Hello   world": "Hello world", "": ""} {
		if got := cleanSubject(in); got != want {
			t.Errorf("cleanSubject(%q) = %q", in, got)
		}
	}
	long := strings.Repeat("word ", 40)
	if got := cleanSubject(long); len([]rune(got)) > 81 || !strings.HasSuffix(got, "…") {
		t.Errorf("a long subject is cut at a word: %q", got)
	}
	if got := whatOf(repo.ListRow{Item: repo.Item{Kind: "chat"}, Snippet: "Hi, are you coming tonight? Let me know"}, "Tina"); !strings.HasPrefix(got, "Hi, are you coming") {
		t.Errorf("a chat is told by its last message: %q", got)
	}
}

func TestADirectMessageFromANewPersonWhoWasWrittenToStatesItsFacts(t *testing.T) {
	e := newClaudeEnv(t)
	e.addressed = true
	person := e.seedMail(srcGmail, "ana@example.test", "Re: Your invoice 2026/14", "h2", "Hello, please pay 1 250 Kč by 15. 12. 2026.", 3, false)
	if err := e.a.store.Update(e.ctx, func(tx *repo.Tx) error {
		_, _, err := tx.InsertMessage(e.ctx, repo.Message{ItemID: person.ID, Source: srcGmail, SourceID: "mine1", ThreadKey: "gmail:x", Network: "mail",
			Sender: "owner@example.test", Subject: "Hi", Text: "Hello Ana", To: "ana@example.test", ReceivedAt: e.ago(5), SentByMe: true})
		return err
	}); err != nil {
		t.Skip("cannot seed the owner's own message here: ", err)
	}
	out := e.a.factsOf(e.ctx, repoRow(e, person), "noise", false)
	var texts []string
	for _, f := range out {
		texts = append(texts, f.text)
	}
	all := strings.Join(texts, " | ")
	// At most two reasons, the strongest first; "addressed to you directly" is no reason.
	if all != "you have written to this sender before | amount 1 250 Kč" {
		t.Errorf("facts: %s", all)
	}
}

func repoRow(e *claudeEnv, it repo.Item) repo.ListRow {
	lr, _ := e.a.store.ListRowByID(e.ctx, it.ID)
	return lr
}

// A test run can have the model's text written at once for its own items.
func TestATestRunCanHaveTheModelsTextWrittenNowForItsOwnItems(t *testing.T) {
	e := newClaudeEnv(t)
	e.saveKey()
	e.evening(10) // not the evening: the plain row comes first
	run := "e2e-summarynow1"
	it := e.seedRun(run, stageNew, seedMsg{subject: "Weekly newsletter", name: "N", sender: "news@example.test", text: "Hello", at: e.ago(1)})
	rec := e.call("POST", "/api/items/"+ref(it)+"/skip", e.a.agentTokenValue(), map[string]any{"reason": "A newsletter that needs nothing.", "kind": "noise", "agent": "Claude", "run": run})
	if rec.Code != 200 {
		t.Fatalf("skip under the run: %d %s", rec.Code, rec.Body.String())
	}
	e.fake.script = func(int, map[string]any) fakeAnswer {
		return use("write_summary", map[string]any{"groups": []any{map[string]any{"title": "Newsletters", "items": []any{map[string]any{"id": ref(it)}}}}})
	}
	rec = e.call("POST", testRunsPath+"/"+run+"/closed-summary?now=1", e.a.agentTokenValue(), nil)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"model":true`) || e.fake.count() != 1 {
		t.Fatalf("now=1: %d %s (%d calls)", rec.Code, rec.Body.String(), e.fake.count())
	}
	// asked again: it writes again at once (the day does not stop a test)
	rec = e.call("POST", testRunsPath+"/"+run+"/closed-summary?now=1", e.a.agentTokenValue(), nil)
	if rec.Code != 200 || e.fake.count() != 2 {
		t.Errorf("a second now=1: %d, %d calls", rec.Code, e.fake.count())
	}
}

func TestOnlyARealMoneyAmountCountsAndOnlyADateAheadCounts(t *testing.T) {
	for in, want := range map[string]string{
		"Gemini 9kC context window": "", "limit 9kC": "", "Celkem 450 Kč.": "450 Kč", "Celkem 450Kč": "450Kč", "pay €12 now": "€12",
		"12 EUR": "12 EUR", "1 250,50 CZK": "1 250,50 CZK", "ref 12345 abc": "", "5 kc": "5 kc",
	} {
		if got := firstAmount(in); got != want {
			t.Errorf("firstAmount(%q) = %q, want %q", in, got, want)
		}
	}
	now, loc := time.Date(2026, 11, 23, 20, 0, 0, 0, time.UTC), time.UTC
	for in, want := range map[string]string{
		"doručení 22.11.": "", "on 2026-11-22": "", "on 22 Nov": "", "dne 23.11.": "23.11.", "dne 25.11.": "25.11.",
		"22.11. nebo 25.11.": "25.11.", "on 2027-01-05": "2027-01-05", "Nov 30": "Nov 30", "1.2.2026": "",
	} {
		if got := firstDate(in, now, loc); got != want {
			t.Errorf("firstDate(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestTonightsDoubtLinesAreShortAndHoldOnlyRealReasons(t *testing.T) {
	e := newClaudeEnv(t)
	e.addressed = true
	// An Alza order notice: a file, a passed date, an amount, the owner has written there before.
	alza := e.seedMail(srcGmail, "doklady@alza.test", "Objednávka 123", "h-alza", "Děkujeme. Celkem 450 Kč. Doručení 12.11.", 0.2, false)
	if err := e.a.store.Update(e.ctx, func(tx *repo.Tx) error {
		raw1, _, err := tx.InsertRaw(e.ctx, repo.Raw{Source: srcGmail, SourceID: "invoice-alza", VersionKey: "a@0", ThreadKey: "gmail:x", ReceivedAt: e.ago(0.1), IngestedAt: e.ago(0.1), BlobSHA: "ab", Payload: "{}"})
		if err != nil {
			return err
		}
		raw2, _, err := tx.InsertRaw(e.ctx, repo.Raw{Source: srcGmail, SourceID: "mine-alza", VersionKey: "a@0", ThreadKey: "gmail:x", ReceivedAt: e.ago(30), IngestedAt: e.ago(30), BlobSHA: "ab", Payload: "{}"})
		if err != nil {
			return err
		}
		if _, _, err := tx.InsertMessage(e.ctx, repo.Message{ItemID: alza.ID, RawID: raw1, Source: srcGmail, SourceID: "invoice-alza", ThreadKey: "gmail:x", Network: "mail",
			Sender: "doklady@alza.test", Subject: "Faktura", Text: "Faktura v prilone", ReceivedAt: e.ago(0.1), HasAttachments: true, Addressed: true}); err != nil {
			return err
		}
		_, _, err = tx.InsertMessage(e.ctx, repo.Message{ItemID: alza.ID, RawID: raw2, Source: srcGmail, SourceID: "mine-alza", ThreadKey: "gmail:x", Network: "mail",
			Sender: "owner@example.test", Subject: "Dotaz", Text: "Dobrý den", To: "doklady@alza.test", ReceivedAt: e.ago(30), SentByMe: true})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, f := range e.a.factsOf(e.ctx, repoRow(e, alza), "noise", false) {
		got = append(got, f.text)
	}
	if strings.Join(got, "|") != "you have written to this sender before|amount 450 Kč" {
		t.Errorf("Alza: %v", got)
	}
	// A Google announcement with "9kC": no amount, so nothing real speaks for it.
	gem := e.seedMail(srcGmail, "gemini-noreply@example.test", "Gemini news", "h-gem", "Gemini now reads 9kC tokens of context.", 0.2, false)
	gf := e.a.factsOf(e.ctx, repoRow(e, gem), "noise", false)
	for _, f := range gf {
		if strings.Contains(f.text, "amount") || strings.Contains(f.text, "no note") || strings.Contains(f.text, "directly") {
			t.Errorf("Gemini: %v", gf)
		}
	}
	if _, doubt, _ := doubtOfFacts(gf); doubt != "" && len(gf) == 0 {
		t.Errorf("Gemini doubt: %q", doubt)
	}
	// A calendar notice with a date still ahead: only the date.
	e.seedMail(srcGmail, "calendar-notification@google.com", "New event: Earlier", "h-cal0", "Older notice.", 20, false)
	cal := e.seedMail(srcGmail, "calendar-notification@google.com", "New event: Dentist", "h-cal1", "New event: Dentist on 25.11.", 0.2, false)
	var cf []string
	for _, f := range e.a.factsOf(e.ctx, repoRow(e, cal), "noise", false) {
		cf = append(cf, f.text)
	}
	if strings.Join(cf, "|") != "date 25.11." {
		t.Errorf("calendar: %v", cf)
	}
}
