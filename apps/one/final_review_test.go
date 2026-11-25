package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"lab/apps/one/repo"
)

// The fixes of the final review of ONE. Every test is named for what must
// hold and fails without its fix. All names, addresses and texts are invented.

// ownerEvent writes an event as the owner on an item, the way his actions do.
func (e *claudeEnv) ownerEvent(it repo.Item, typ string) {
	e.t.Helper()
	err := e.a.store.Update(e.ctx, func(tx *repo.Tx) error {
		return tx.InsertEvent(e.ctx, e.now.Unix(), it.ID, "owner", typ, map[string]any{"text": "An invented line."})
	})
	if err != nil {
		e.t.Fatal(err)
	}
}

func (e *claudeEnv) modelCall(it repo.Item, st *modelState, name string, args map[string]any) (string, bool) {
	b, _ := json.Marshal(args)
	return e.a.modelTool(e.ctx, st, it, name, b)
}

// --- 1 (High): a failed review never hides an item for good ---

func TestFinal1_AnItemTheModelFailedOnThreeTimesIsShownInNewWithANoticeAndStillWaitsForAnAgent(t *testing.T) {
	e := newClaudeEnv(t)
	e.saveKey()
	bad := e.seedMail("gmail", "erin@shop.test", "Odd", "<h1@shop.test>", "An invented parcel text.", 2, true)
	e.fake.script = func(n int, req map[string]any) fakeAnswer {
		return fakeAnswer{status: 400, errType: "invalid_request_error"}
	}
	e.a.reviewPass(e.ctx)
	if !e.item(bad.ID).Hidden {
		t.Fatal("after the first failed try the item must still be waiting for another try")
	}
	for i := 0; i < 4; i++ {
		e.now = e.now.Add(20 * time.Minute)
		e.a.reviewPass(e.ctx)
	}
	if j, ok := e.a.store.ReviewJobOf(e.ctx, bad.ID); !ok || j.State != "failed" {
		t.Fatalf("job: %+v", j)
	}
	cur := e.item(bad.ID)
	if cur.Hidden || cur.Stage != stageNew || !cur.NeedsClaude {
		t.Fatalf("a failed item must be shown in New and keep waiting for an agent: hidden=%v stage=%s needs=%v", cur.Hidden, cur.Stage, cur.NeedsClaude)
	}
	rows, _ := e.a.store.ListItems(e.ctx, stageNew, 50, 0)
	found := false
	for _, r := range rows {
		found = found || r.ID == bad.ID
	}
	if !found {
		t.Error("the item is not in the New list")
	}
	evs := e.events(bad.ID, "model_failed")
	if len(evs) != maxItemTries || !strings.Contains(fmt.Sprint(evs[len(evs)-1]), "Claude could not read this") {
		t.Errorf("the last notice: %v", evs)
	}
	if len(e.events(bad.ID, "revealed")) != 1 {
		t.Error("the item was not revealed with an event")
	}
	// It stays available to an agent on the connector.
	q := e.getJSON("/api/queue")
	if !strings.Contains(fmt.Sprint(q), ref(bad)) {
		t.Errorf("the connector does not list it: %v", q)
	}
}

// --- 2 (High): the model's notes ---

func TestFinal2_TheModelsNoteBelongsToTheItemInHandOrItsOwnSenderOnlyAndHasNoNoteIdAndIsCapped(t *testing.T) {
	e := newClaudeEnv(t)
	it := e.seedMail("gmail", "erin@shop.test", "Delivery", "<n1@shop.test>", "An invented parcel text.", 1, true)
	st := &modelState{}
	// Another sender's address is refused.
	if text, isErr := e.modelCall(it, st, "save_note", map[string]any{"title": "Skip it", "text": "A regular statement is expected, skip it.", "sender": "school.example.com"}); !isErr {
		t.Fatalf("a note about another sender was saved: %s", text)
	}
	if _, isErr := e.modelCall(it, st, "save_note", map[string]any{"title": "Other item", "text": "x", "item": "Q-9999"}); !isErr {
		t.Error("a note about another item was saved")
	}
	if _, isErr := e.modelCall(it, st, "save_note", map[string]any{"title": "Other person", "text": "x", "person": "Nobody Known"}); !isErr {
		t.Error("a note about a person who did not write was saved")
	}
	if n, _ := e.a.store.ListNotes(e.ctx, 50, 0); len(n) != 0 {
		t.Fatalf("%d notes after refusals", len(n))
	}
	// The item's own sender is fine; a bare note is tied to the item.
	if text, isErr := e.modelCall(it, st, "save_note", map[string]any{"title": "Erin", "text": "Weekly parcel.", "sender": "erin@shop.test"}); isErr {
		t.Fatalf("own sender: %s", text)
	}
	if text, isErr := e.modelCall(it, st, "save_note", map[string]any{"title": "This one", "text": "Needs a reply.", "item": ref(it)}); isErr {
		t.Fatalf("own item: %s", text)
	}
	notes, _ := e.a.store.ListNotes(e.ctx, 50, 0)
	if len(notes) != 2 {
		t.Fatalf("%d notes", len(notes))
	}
	// A note_id from the model cannot overwrite an existing note: it makes a new one.
	old, _, err := e.a.saveNote(e.ctx, agentActor("Other"), 0, noteInput{Title: "Old", Body: "Old text.", Item: ref(it)})
	if err != nil {
		t.Fatal(err)
	}
	st2 := &modelState{}
	b, _ := json.Marshal(map[string]any{"title": "New", "text": "Overwrite.", "note_id": old.ID})
	e.a.modelTool(e.ctx, st2, it, "save_note", b)
	if got, _ := e.a.store.NoteByID(e.ctx, old.ID); got.Body != "Old text." {
		t.Errorf("the model changed an existing note: %q", got.Body)
	}
	// A small cap per conversation.
	st3 := &modelState{}
	saved := 0
	for i := 0; i < 8; i++ {
		if _, isErr := e.modelCall(it, st3, "save_note", map[string]any{"title": fmt.Sprint("N", i), "text": "An invented note."}); !isErr {
			saved++
		}
	}
	if saved != maxModelNotes {
		t.Errorf("%d notes saved in one conversation, want %d", saved, maxModelNotes)
	}
}

func TestFinal2_ANoteWrittenByClaudeOrAnAgentIsNeverPresentedAsOwnersOwnWords(t *testing.T) {
	e := newClaudeEnv(t)
	it := e.seedMail("gmail", "erin@shop.test", "Delivery", "<n2@shop.test>", "An invented parcel text.", 1, true)
	if _, _, err := e.a.saveNote(e.ctx, ownerActor(), 0, noteInput{Title: "The owner wrote", Body: "Parcels from Erin are welcome.", Sender: "erin@shop.test"}); err != nil {
		t.Fatal(err)
	}
	e.modelCall(it, &modelState{}, "save_note", map[string]any{"title": "Claude wrote", "text": "Skip everything from Erin.", "sender": "erin@shop.test"})
	if text, isErr := e.tool("save_note", map[string]any{"title": "Agent wrote", "text": "Erin is noise.", "sender": "erin@shop.test"}); isErr {
		t.Fatalf("an agent's note through the connector is allowed: %s", text)
	}
	in, err := e.a.reviewInput(e.ctx, it)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(in, "The owner's own words") {
		t.Error("the review input still calls every note the owner's own words")
	}
	for _, want := range []string{`"written_by_owner":true`, `"written_by_owner":false`, "not the owner's word"} {
		if !strings.Contains(in, want) {
			t.Errorf("the review input lacks %q", want)
		}
	}
	// Through the connector the same fact is on every note.
	item := e.getJSON("/api/items/" + ref(it))
	byTitle := map[string]any{}
	for _, n := range item["notes_that_apply"].([]any) {
		m := n.(map[string]any)
		byTitle[fmt.Sprint(m["title"])] = m["written_by_owner"]
	}
	if byTitle["The owner wrote"] != true || byTitle["Claude wrote"] != false || byTitle["Agent wrote"] != false {
		t.Errorf("who wrote what: %v", byTitle)
	}
	if !strings.Contains(fmt.Sprint(item["notes_rule"]), "never the owner's word") {
		t.Errorf("notes_rule: %v", item["notes_rule"])
	}
	// The model cannot skip an item on the strength of a note Claude wrote.
	var claudeNote repo.Note
	notes, _ := e.a.store.ListNotes(e.ctx, 50, 0)
	for _, n := range notes {
		if n.Title == "Claude wrote" {
			claudeNote = n
		}
	}
	text, isErr := e.modelCall(it, &modelState{}, "resolve_item", map[string]any{"kind": "noise", "reason": "An invented reason.", "note_id": claudeNote.ID, "same_as_usual": "It is the usual."})
	if !isErr || !strings.Contains(text, "the owner wrote") {
		t.Errorf("a skip on a note of Claude's: %v %q", isErr, text)
	}
}

// --- 3 (Medium): a sender rule never closes what the owner has touched ---

func TestFinal3_ASenderRuleNeverClosesAnItemOwnerHasTouchedOrThatHasAProposalWaiting(t *testing.T) {
	e := newClaudeEnv(t)
	e.sendersItems("shop@offers.test", 6)
	if out := e.proposeSender("shop@offers.test"); out["_status"] != float64(200) {
		t.Fatalf("proposal: %v", out)
	}
	mk := func(n string) repo.Item {
		return e.seedMail("gmail", "shop@offers.test", "Fresh "+n, "<"+n+"@x.test>", "An invented offer text.", 1, true)
	}
	plain, noted, pending, resolved, proposed := mk("plain"), mk("noted"), mk("pending"), mk("resolved"), mk("proposed")
	e.ownerEvent(noted, "noted")
	e.ownerEvent(pending, "pending_set")
	e.ownerEvent(resolved, "resolved")
	e.propose(proposed, "close", "An invented reason to close it.")
	e.approveSkipRule(e.ruleOfKind(skipSender).ID)
	if e.stage(plain) != stageClosed {
		t.Error("an item that is new and untouched must be closed")
	}
	for name, it := range map[string]repo.Item{"noted": noted, "pending": pending, "resolved": resolved, "proposed": proposed} {
		if e.stage(it) != stageNew {
			t.Errorf("the rule closed the item the owner touched (%s): %s", name, e.stage(it))
		}
	}
	// A message that arrives between the look and the close stops the close.
	late := mk("late")
	rule := e.ruleOfKind(skipSender)
	if err := e.a.closeByRule(e.ctx, rule, late.ID, "", late.LastAt-10); err != nil {
		t.Fatal(err)
	}
	if e.stage(late) != stageNew {
		t.Error("closeByRule ignored a message that came after the look")
	}
}

// --- 4 (Medium): the same-mail rule needs the sender too ---

func TestFinal4_TheSameMailRuleNeedsTheSameSenderAndSubjectNotTheMessageIdAlone(t *testing.T) {
	e := newClaudeEnv(t)
	first := e.seedMail("gmail", "attacker@evil.test", "Hello", "<dup-1@x.test>", "An invented mail text.", 1.0, false)
	e.a.afterMailStored(e.ctx, first.ID)
	lookalike := e.seedMail("outlook", "bank-lookalike@other.test", "Invoice overdue, pay today", "<DUP-1@x.test>", "An invented, different mail.", 0.9, true)
	e.a.afterMailStored(e.ctx, lookalike.ID)
	if _, _, ok := e.a.store.SameMailOf(e.ctx, lookalike.ID); ok {
		t.Fatal("two different senders with one message id were taken for one mail")
	}
	sameSender := e.seedMail("outlook", "attacker@evil.test", "Something else entirely", "<dup-1@x.test>", "An invented, different mail.", 0.8, true)
	e.a.afterMailStored(e.ctx, sameSender.ID)
	if _, _, ok := e.a.store.SameMailOf(e.ctx, sameSender.ID); ok {
		t.Fatal("one sender, one message id, another subject: taken for one mail")
	}
	twin := e.seedMail("outlook", "Attacker@Evil.test", "RE: hello", "<dup-1@x.test>", "An invented mail text.", 0.7, true)
	e.a.afterMailStored(e.ctx, twin.ID)
	if n, second, ok := e.a.store.SameMailOf(e.ctx, twin.ID); !ok || !second || n != first.Number {
		t.Fatalf("the real second copy was not found: %d %v %v", n, second, ok)
	}
	e.approveSkipRule(e.ruleOfKind(skipSameMail).ID)
	if e.stage(lookalike) != stageNew || e.stage(sameSender) != stageNew {
		t.Error("a mail that is not a copy was closed")
	}
	// Months apart is not the same mail either.
	old := e.seedMail("gmail", "bob@shop.test", "Order", "<o-9@shop.test>", "An invented order.", 40, false)
	late := e.seedMail("outlook", "bob@shop.test", "Order", "<o-9@shop.test>", "An invented order.", 1, true)
	_ = old
	if _, ok := e.a.store.FindTwin(e.ctx, repo.TwinOf{HeaderID: "<o-9@shop.test>", Source: "outlook", Sender: "bob@shop.test", Subject: "Order", ReceivedAt: late.LastAt}, late.ID); ok {
		t.Error("a mail 40 days earlier was taken for the twin")
	}
}

// --- 5 (Medium): what is brought back is never closed again by a rule ---

func TestFinal5_AnItemOwnerBroughtBackStaysOpenEvenWhenItsRuleIsSwitchedOnAgain(t *testing.T) {
	e := newClaudeEnv(t)
	it := e.seedMail("gmail", calendarNotice.Sender, calendarNotice.SubjectPrefix+" Choir", "<c-20@cal.test>", "Invented: an entry was added.", 1, true)
	e.a.afterMailStored(e.ctx, it.ID)
	rule := e.ruleOfKind(skipCalendar)
	e.approveSkipRule(rule.ID)
	if e.stage(it) != stageClosed {
		t.Fatal("not closed")
	}
	e.ajaxPost(fmt.Sprintf("/items/%d/bring-back", it.ID), url.Values{})
	post(e.h, fmt.Sprintf("/settings/skip-rules/%d/enabled/on", rule.ID), url.Values{})
	e.a.skipRulesPass(e.ctx)
	if e.stage(it) != stageNew {
		t.Fatal("the rule closed again what the owner brought back")
	}
	if got := len(e.events(it.ID, "brought_back")); got != 1 {
		t.Errorf("the trail has %d brought_back events, want 1", got)
	}
}

func TestFinal5_AnItemTheNightCheckBroughtBackOrAMessageImportedAgainAfterBeingDroppedIsNeverClosedByARule(t *testing.T) {
	e := newClaudeEnv(t)
	e.saveKey()
	it := e.seedMail("gmail", calendarNotice.Sender, calendarNotice.SubjectPrefix+" Choir", "<c-21@cal.test>", "Invented: an entry was added.", 0.3, true)
	e.a.afterMailStored(e.ctx, it.ID)
	rule := e.ruleOfKind(skipCalendar)
	e.approveSkipRule(rule.ID)
	e.fake.script = func(n int, req map[string]any) fakeAnswer {
		return use("bring_back", map[string]any{"ref": ref(it), "reason": "An invented reason it matters."})
	}
	e.a.nightCheck(e.ctx)
	if e.stage(it) != stageNew {
		t.Fatal("the night check did not bring it back")
	}
	post(e.h, fmt.Sprintf("/settings/skip-rules/%d/enabled/on", rule.ID), url.Values{})
	e.a.skipRulesPass(e.ctx)
	if e.stage(it) != stageNew {
		t.Error("a rule closed again what the night check brought back")
	}
	// A message that was dropped once and is imported again is protected too.
	again := e.seedMail("gmail", calendarNotice.Sender, calendarNotice.SubjectPrefix+" Choir 2", "<c-22@cal.test>", "Invented: another entry.", 0.2, true)
	err := e.a.store.Update(e.ctx, func(tx *repo.Tx) error {
		msgs, _, _ := tx.MessagesSince(e.ctx, again.ID, 0)
		id, err := tx.InsertRule(e.ctx, repo.Rule{Name: "Drop once", Enabled: true, Source: "gmail", Field: "sender", Op: "equals", Value: "x@y.test", CreatedBy: "owner"}, e.now.Unix())
		if err != nil {
			return err
		}
		_, err = tx.InsertSkipped(e.ctx, repo.Skipped{At: e.now.Unix(), Source: msgs[0].Source, SourceID: msgs[0].SourceID, Sender: msgs[0].Sender, Subject: "s", ReceivedAt: msgs[0].ReceivedAt, RuleID: id})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	e.a.skipRulesPass(e.ctx)
	if e.stage(again) != stageNew {
		t.Error("a message imported again after being dropped was closed by a rule")
	}
}

// --- 6 (Medium): matter patterns ---

func TestFinal6_APatternIsCompiledOnceNotForEveryMessage(t *testing.T) {
	ps := []matterPattern{{Name: "orders", Re: "ZZORD-[0-9]+"}}
	before := matterCompiles.Load()
	for i := 0; i < 50; i++ {
		if k := matterKey(ps, "Your order ZZORD-77", "text"); k != "orders|zzord-77" {
			t.Fatalf("key %q", k)
		}
	}
	if got := matterCompiles.Load() - before; got != 1 {
		t.Errorf("the pattern was compiled %d times for 50 messages", got)
	}
}

func TestFinal6_APatternIsRefusedWhenItIsTooBroadOrAsksForHugeRepeatsAndInputIsBounded(t *testing.T) {
	for _, bad := range []string{"[0-9]+", ".", "[a-z]+", "(?i)([a-z]{1000})([a-z]{1000})x", "ab[0-9]+", "(INV-|[0-9])[0-9]+"} {
		if checkMatterRegex(bad) == "" {
			t.Errorf("%q was accepted", bad)
		}
	}
	for _, good := range []string{"INV-[0-9]+", "ORD-[0-9]{4,12}", "(?i)parcel ([a-z0-9]+)", "(INV|FAK)-[0-9]+"} {
		if msg := checkMatterRegex(good); msg != "" {
			t.Errorf("%q refused: %s", good, msg)
		}
	}
	ps := []matterPattern{{Name: "o", Re: "ORD-[0-9]+"}}
	if k := matterKey(ps, "x", strings.Repeat("a", matterTextScanSize+10)+" ORD-5"); k != "" {
		t.Errorf("text past the scan size was searched: %q", k)
	}
	if k := matterKey([]matterPattern{{Name: "o", Re: strings.Repeat("a", maxMatterRegex+1) + "[0-9]+"}}, "aaaa", "aaaa"); k != "" {
		t.Error("an over-long stored pattern was used")
	}
}

func TestFinal6_AProposedPatternShowsHowManyExistingRowsItWouldHaveJoined(t *testing.T) {
	e := newClaudeEnv(t)
	for i, subj := range []string{"Order ZQV-5501 shipped", "Re: ZQV-5501 delay", "Invoice for ZQV-5501", "Order ZQV-9999 shipped", "Unrelated"} {
		e.seedMail("gmail", fmt.Sprintf("shop%d@shop.test", i), subj, fmt.Sprintf("<m%d@x.test>", i), "An invented text.", 2, false)
	}
	out := e.post("/api/pattern-proposals", map[string]any{"pattern": "ZQV-[0-9]+", "name": "zqv", "reason": "An invented example ZQV-5501.", "agent": "Claude"})
	if out["_status"] != float64(200) || out["would_join_rows"] != float64(3) {
		t.Fatalf("proposal: %v", out)
	}
	page := get(e.h, "/settings").Body.String()
	if !strings.Contains(page, "it would have joined 3 rows") {
		t.Errorf("the proposal does not show the number:\n%s", page[strings.Index(page, "h-matter"):][:1500])
	}
	if bad := e.post("/api/pattern-proposals", map[string]any{"pattern": "[0-9]+", "reason": "Everything.", "agent": "Claude"}); bad["_status"] != float64(400) {
		t.Errorf("a pattern that matches almost everything: %v", bad)
	}
}

// --- 7 (Medium): the night check is not lost for a night ---

func TestFinal7_ANightRowLeftRunningByARestartDoesNotBlockTheNightAndAStaleOneIsTakenOver(t *testing.T) {
	e := newClaudeEnv(t)
	e.saveKey()
	e.seedMail("gmail", "erin@shop.test", "Delivery", "<t7@shop.test>", "An invented parcel text.", 0.1, true)
	e.fake.script = func(n int, req map[string]any) fakeAnswer { return say("") }
	e.now = time.Date(2026, 11, 20, 2, 0, 30, 0, time.UTC) // 03:00 in Prague
	var id int64
	e.a.store.Update(e.ctx, func(tx *repo.Tx) (err error) {
		id, err = tx.InsertNightCheck(e.ctx, e.a.dayKey(e.now), modelOpus, e.now.Unix()-60)
		return
	})
	e.a.nightTick(e.ctx) // a fresh row may still be at work
	time.Sleep(50 * time.Millisecond)
	if e.fake.count() != 0 {
		t.Fatal("a row that started a minute ago was taken over")
	}
	e.now = e.now.Add(nightStale + time.Minute)
	e.a.nightTick(e.ctx)
	waitNight(t, e, func(n repo.NightCheck) bool { return n.ID != id && n.Status == "done" })
	if e.fake.count() != 1 {
		t.Errorf("%d calls after the stale row was taken over, want 1", e.fake.count())
	}
	rows := e.a.store.NightChecksOfDay(e.ctx, e.a.dayKey(e.now))
	if rows[0].Status != "failed" || rows[0].Error == "" {
		t.Errorf("the stale row: %+v", rows[0])
	}
}

func TestFinal7_ARestartEndsWhatWasRunningSoTheNightIsRetriedInsteadOfLost(t *testing.T) {
	e := newClaudeEnv(t)
	e.a.store.Update(e.ctx, func(tx *repo.Tx) error {
		_, err := tx.InsertNightCheck(e.ctx, e.a.dayKey(e.now), modelOpus, e.now.Unix())
		return err
	})
	e.a.recoverAfterRestart(e.ctx)
	rows := e.a.store.NightChecksOfDay(e.ctx, e.a.dayKey(e.now))
	if len(rows) != 1 || rows[0].Status != "failed" || !strings.Contains(rows[0].Error, "restarted") {
		t.Errorf("after a restart: %+v", rows)
	}
}

func TestFinal7_AFailedNightIsRetriedOnceLaterTheSameNightWithinTheNightsMoneyLimitAndNeverMoreOften(t *testing.T) {
	e := newClaudeEnv(t)
	e.saveKey()
	e.seedMail("gmail", "erin@shop.test", "Delivery", "<t8@shop.test>", "An invented parcel text.", 0.1, true)
	e.fake.script = func(n int, req map[string]any) fakeAnswer {
		return fakeAnswer{status: 503, errType: "overloaded_error"}
	}
	e.now = time.Date(2026, 11, 20, 2, 0, 30, 0, time.UTC)
	e.a.nightTick(e.ctx)
	waitNight(t, e, func(n repo.NightCheck) bool { return n.Status == "failed" })
	calls := e.fake.count()
	e.now = e.now.Add(5 * time.Minute)
	e.a.nightTick(e.ctx)
	time.Sleep(50 * time.Millisecond)
	if e.fake.count() != calls {
		t.Fatal("the retry came too soon")
	}
	e.fake.script = func(n int, req map[string]any) fakeAnswer { return say("") }
	e.now = e.now.Add(nightRetryWait)
	e.a.nightTick(e.ctx)
	waitNight(t, e, func(n repo.NightCheck) bool { return n.Status == "done" })
	if e.fake.count() != calls+1 {
		t.Errorf("the retry made %d calls", e.fake.count()-calls)
	}
	// One retry only: a second failure is not tried again.
	e2 := newClaudeEnv(t)
	e2.saveKey()
	e2.seedMail("gmail", "erin@shop.test", "Delivery", "<t9@shop.test>", "An invented parcel text.", 0.1, true)
	e2.fake.script = func(n int, req map[string]any) fakeAnswer {
		return fakeAnswer{status: 503, errType: "overloaded_error"}
	}
	e2.now = time.Date(2026, 11, 20, 2, 0, 30, 0, time.UTC)
	for i := 0; i < 2; i++ {
		e2.a.nightTick(e2.ctx)
		waitNightCount(t, e2, i+1)
		e2.now = e2.now.Add(nightRetryWait + time.Minute)
	}
	e2.a.nightTick(e2.ctx)
	time.Sleep(50 * time.Millisecond)
	if got := len(e2.a.store.NightChecksOfDay(e2.ctx, e2.a.dayKey(e2.now))); got != nightTries {
		t.Errorf("%d tries in one night, want %d", got, nightTries)
	}
	if told := e2.told.all(); len(told) != 1 {
		t.Errorf("%d notices for one failed night, want 1", len(told))
	}
}

func waitNight(t *testing.T, e *claudeEnv, ok func(repo.NightCheck) bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		for _, n := range e.a.store.NightChecksOfDay(e.ctx, e.a.dayKey(e.now)) {
			if ok(n) {
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("the night check did not get where the test waits for")
}

func waitNightCount(t *testing.T, e *claudeEnv, n int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		rows := e.a.store.NightChecksOfDay(e.ctx, e.a.dayKey(e.now))
		if len(rows) >= n && rows[len(rows)-1].Status != "running" {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("the night check did not end")
}

// --- 8 (Low): night check data, reach and numbers ---

func TestFinal8_TheNightCheckSendsNoTextForABlockedTargetAndABrokenMailNeverLeaksItsCode(t *testing.T) {
	e := newClaudeEnv(t)
	e.saveKey()
	e.seedMail("gmail", "alerts@csob.cz", "Your sign-in code", "<b1@csob.cz>", "Your invented code is 482913, do not share it.", 0.3, true)
	e.seedMail("gmail", "erin@shop.test", "Delivery", "<b2@shop.test>", "An invented parcel text for Friday.", 0.2, true)
	e.fake.script = func(n int, req map[string]any) fakeAnswer { return say("") }
	e.a.nightCheck(e.ctx)
	sent := e.nightUserText(0)
	if strings.Contains(sent, "482913") || strings.Contains(sent, "sign-in code, do not") {
		t.Errorf("the text of a bank's mail went to the model:\n%s", sent)
	}
	if !strings.Contains(sent, "An invented parcel text for Friday") {
		t.Error("the text of an ordinary unread mail is no longer sent")
	}
}

func TestFinal8_TheNightCheckBringsBackAtMostTenOrAFifthAndNamesTheRestInTheDetailOnConnectors(t *testing.T) {
	e := newClaudeEnv(t)
	e.saveKey()
	var items []repo.Item
	for i := 0; i < 14; i++ {
		it := e.seedMail("gmail", fmt.Sprintf("bulk%d@offers.test", i), fmt.Sprintf("Offer %d", i), fmt.Sprintf("<cap%d@x.test>", i), "An invented offer text.", 0.5, true)
		e.skipAsAgent(it)
		items = append(items, it)
	}
	e.fake.script = func(n int, req map[string]any) fakeAnswer {
		var blocks []map[string]any
		for _, it := range items {
			blocks = append(blocks, map[string]any{"type": "tool_use", "name": "bring_back", "input": map[string]any{"ref": ref(it), "reason": "An invented reason."}})
		}
		return fakeAnswer{content: blocks, stop: "tool_use", in: 1000, out: 100}
	}
	e.a.nightCheck(e.ctx)
	n, _ := e.a.store.LatestNightCheck(e.ctx)
	if n.BroughtBack != maxNightBack(14) || n.BroughtBack != 10 {
		t.Fatalf("brought back %d, want 10", n.BroughtBack)
	}
	for _, it := range items[10:] {
		if !strings.Contains(n.Error, ref(it)) {
			t.Errorf("%s is not named in the detail: %q", ref(it), n.Error)
		}
		if e.stage(it) != stageClosed {
			t.Errorf("%s was brought back over the cap", ref(it))
		}
	}
	page := html0(get(e.h, "/connectors").Body.String())
	if !strings.Contains(page, ref(items[13])) || !strings.Contains(page, "more than the 10 it may") {
		t.Error("the Connectors page does not name the rest")
	}
}

func TestFinal8_AnNightOverTheMessageCapSaysSoPlainlyInTheMorningDetail(t *testing.T) {
	e := newClaudeEnv(t)
	e.saveKey()
	for i := 0; i < 4; i++ {
		e.seedMail("gmail", fmt.Sprintf("s%d@offers.test", i), fmt.Sprintf("Offer %d", i), fmt.Sprintf("<cap-%d@x.test>", i), "An invented offer text.", 0.5, true)
	}
	old := nightMaxMessages
	nightMaxMessages = 3
	defer func() { nightMaxMessages = old }()
	e.fake.script = func(n int, req map[string]any) fakeAnswer { return say("") }
	e.a.nightCheck(e.ctx)
	n, _ := e.a.store.LatestNightCheck(e.ctx)
	if n.Status != "done" || !strings.Contains(n.Error, "more than 3 messages") {
		t.Errorf("record: %+v", n)
	}
	if page := html0(get(e.h, "/connectors").Body.String()); !strings.Contains(page, "more than 3 messages") {
		t.Error("the Connectors page does not say it")
	}
}

func html0(s string) string { return strings.NewReplacer("&#39;", "'", "&amp;", "&").Replace(s) }

func TestFinal8_TestRunDataNeverCountsInTheRealTwinLookupOrTheCounts(t *testing.T) {
	e := newClaudeEnv(t)
	real := e.seedMail("outlook", "bob@shop.test", "Order", "<tr-1@shop.test>", "An invented order.", 1, false)
	realCal := e.seedMail("gmail", calendarNotice.Sender, calendarNotice.SubjectPrefix+" Real", "<tr-2@cal.test>", "Invented.", 1, false)
	_ = realCal
	run := e.ctx
	e.ctx = repo.WithRun(run, "e2e-final8run")
	marked := e.seedMail("gmail", "bob@shop.test", "Order", "<tr-1@shop.test>", "An invented order.", 1, false)
	e.seedMail("gmail", calendarNotice.Sender, calendarNotice.SubjectPrefix+" Test", "<tr-3@cal.test>", "Invented.", 1, false)
	e.ctx = run
	if marked.TestRun != "e2e-final8run" {
		t.Fatalf("the item was not marked: %+v", marked)
	}
	// A real mail's twin is never found among a run's messages, and the other way round.
	tw := repo.TwinOf{HeaderID: "<tr-1@shop.test>", Source: "outlook", Sender: "bob@shop.test", Subject: "Order", ReceivedAt: real.LastAt}
	if id, ok := e.a.store.FindTwin(e.ctx, tw, 0); ok {
		t.Errorf("a test run's mail became the twin of a real item: %d", id)
	}
	since := e.now.Add(-30 * 24 * time.Hour).Unix()
	if n := e.a.store.CountMailLike(e.ctx, calendarNotice.Sender, calendarNotice.SubjectPrefix, since); n != 1 {
		t.Errorf("CountMailLike counted %d, want the 1 real notice", n)
	}
	e.a.store.Update(e.ctx, func(tx *repo.Tx) error { return tx.RecordSameMail(e.ctx, marked.ID, real.ID, e.now.Unix()) })
	if n := e.a.store.CountSameMailSince(e.ctx, since); n != 0 {
		t.Errorf("CountSameMailSince counted a test run's pair: %d", n)
	}
}

func TestFinal8_ARedirectAnswerOnSaveIsNotAValidKeyAndNothingIsStored(t *testing.T) {
	e := newClaudeEnv(t)
	e.a.claudeHTTP = &http.Client{Transport: roundTrip(func(r *http.Request) (*http.Response, error) {
		return respond(302, map[string]any{}), nil
	})}
	rec := post(e.h, "/connectors/claude-api", url.Values{"key": {testClaudeKey}})
	if rec.Code < 400 || e.a.claudeConfigured() {
		t.Errorf("a redirect counted as an accepted key: %d, configured %v", rec.Code, e.a.claudeConfigured())
	}
}

func TestFinal8_SavingAndRemovingTheKeyLeaveAnEventWithoutTheKey(t *testing.T) {
	e := newClaudeEnv(t)
	e.fake.script = func(n int, req map[string]any) fakeAnswer { return say("Hi") }
	if rec := post(e.h, "/connectors/claude-api", url.Values{"key": {testClaudeKey}}); rec.Code != http.StatusSeeOther {
		t.Fatalf("save: %d %s", rec.Code, rec.Body.String())
	}
	if rec := post(e.h, "/connectors/claude-api/remove", url.Values{}); rec.Code != http.StatusSeeOther {
		t.Fatalf("remove: %d", rec.Code)
	}
	if len(e.eventsOfType("claude_key_saved")) != 1 || len(e.eventsOfType("claude_key_removed")) != 1 {
		t.Errorf("events: saved %v removed %v", e.eventsOfType("claude_key_saved"), e.eventsOfType("claude_key_removed"))
	}
	for _, typ := range []string{"claude_key_saved", "claude_key_removed"} {
		if strings.Contains(strings.Join(e.eventsOfType(typ), " "), testClaudeKey) {
			t.Error("the key is in an event")
		}
	}
}

func TestFinal8_TheClosedPageBoundsTheSizeOfASection(t *testing.T) {
	e := newClaudeEnv(t)
	for i := 0; i < 6; i++ {
		it := e.seedMail("gmail", "bulk@offers.test", fmt.Sprintf("Offer %d", i), fmt.Sprintf("<cl%d@x.test>", i), "An invented offer text.", 3, false)
		e.skipAsAgent(it)
	}
	old := closedShownMax
	closedShownMax = 3
	defer func() { closedShownMax = old }()
	for _, g := range closedGroups {
		v, err := e.a.closedViewFor(e.ctx, map[string]int{g.Key: 100000})
		if err != nil {
			t.Fatal(err)
		}
		for _, s := range v.Sections {
			if len(s.Entries) > 3 {
				t.Errorf("section %s shows %d entries, cap is 3", s.Key, len(s.Entries))
			}
			if s.Key == g.Key && len(s.Entries) == 3 && s.MoreURL != "" {
				t.Errorf("a section at its cap offers more: %s", s.MoreURL)
			}
		}
	}
}

func TestFinal8_AShutdownInTheMiddleOfACallIsNoOutageAndTheCallThatWasPaidForIsRecorded(t *testing.T) {
	e := newClaudeEnv(t)
	e.saveKey()
	it := e.seedMail("gmail", "erin@shop.test", "Delivery", "<sd@shop.test>", "An invented parcel text.", 1, true)
	ctx, cancel := context.WithCancel(e.ctx)
	e.a.claudeHTTP = &http.Client{Transport: roundTrip(func(r *http.Request) (*http.Response, error) {
		cancel() // the app is told to stop while the call is on its way
		return nil, r.Context().Err()
	})}
	e.a.reviewPass(ctx)
	if _, down := e.a.downStateOf(e.ctx); down {
		t.Error("a shutdown was recorded as an outage")
	}
	if told := e.told.all(); len(told) != 0 {
		t.Errorf("a shutdown sent a notice: %v", told)
	}
	if j, ok := e.a.store.ReviewJobOf(e.ctx, it.ID); !ok || j.State != "retry" {
		t.Errorf("the job after a shutdown: %+v", j)
	}
	// A call that was answered is recorded even when the context has ended.
	dead, stop := context.WithCancel(e.ctx)
	stop()
	var ans claudeAnswer
	ans.Usage.In, ans.Usage.Out = 1000, 100
	e.a.recordUsage(dead, "review", "", modelSonnet, ans)
	if got := e.a.store.SpentMicro(e.ctx, "review", e.a.dayKey(e.now), ""); got != 1000*2+100*10 {
		t.Errorf("the usage of a call made during a shutdown was lost: %d", got)
	}
}

// Whatever protects an item from the rules is decided again inside the same
// write transaction that closes it, so the order in which goroutines run does
// not matter. Here the bad order is forced: the rule has looked and found the
// item open and untouched, then the protecting fact is written, then the close
// runs. It must refuse, whatever the fact was.
func TestFinal5_TheCloseRereadsTheProtectionUnderTheWriteLock(t *testing.T) {
	facts := map[string]func(e *claudeEnv, it repo.Item){
		"a message imported again after being dropped": func(e *claudeEnv, it repo.Item) {
			e.a.store.Update(e.ctx, func(tx *repo.Tx) error {
				msgs, _, _ := tx.MessagesSince(e.ctx, it.ID, 0)
				id, err := tx.InsertRule(e.ctx, repo.Rule{Name: "Drop once", Enabled: true, Source: "gmail", Field: "sender", Op: "equals", Value: "x@y.test", CreatedBy: "owner"}, e.now.Unix())
				if err != nil {
					return err
				}
				_, err = tx.InsertSkipped(e.ctx, repo.Skipped{At: e.now.Unix(), Source: msgs[0].Source, SourceID: msgs[0].SourceID, Sender: msgs[0].Sender, Subject: "s", ReceivedAt: msgs[0].ReceivedAt, RuleID: id})
				return err
			})
		},
		"brought back by the night check or by the owner": func(e *claudeEnv, it repo.Item) {
			e.a.store.Update(e.ctx, func(tx *repo.Tx) error {
				return tx.InsertEvent(e.ctx, e.now.Unix(), it.ID, "nightcheck", "brought_back", map[string]any{"reason": "x"})
			})
		},
		"something the owner did": func(e *claudeEnv, it repo.Item) {
			e.a.store.Update(e.ctx, func(tx *repo.Tx) error {
				return tx.InsertEvent(e.ctx, e.now.Unix(), it.ID, "owner", "noted", map[string]any{})
			})
		},
	}
	for name, protect := range facts {
		t.Run(name, func(t *testing.T) {
			e := newClaudeEnv(t)
			it := e.seedMail("gmail", calendarNotice.Sender, calendarNotice.SubjectPrefix+" Choir", "<c-30@cal.test>", "Invented: an entry was added.", 0.3, true)
			e.a.afterMailStored(e.ctx, it.ID)
			rule := e.ruleOfKind(skipCalendar)
			e.approveSkipRule(rule.ID)
			// approving closed the first item; use a second, still open one.
			it2 := e.seedMail("gmail", calendarNotice.Sender, calendarNotice.SubjectPrefix+" Choir B", "<c-31@cal.test>", "Invented: another entry.", 0.2, true)
			cur, _ := e.a.store.ItemByID(e.ctx, it2.ID)
			rules, _ := e.a.store.EnabledSkipRules(e.ctx)
			if len(rules) == 0 {
				t.Fatal("the rule is not enabled")
			}
			protect(e, it2) // after the rule looked, before the close
			if err := e.a.closeByRule(e.ctx, rules[0], it2.ID, "", cur.LastAt); err != nil {
				t.Fatal(err)
			}
			if e.stage(it2) != stageNew {
				t.Fatalf("the close ignored the protecting fact: %s", name)
			}
		})
	}
}
