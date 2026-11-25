package main

import (
	"fmt"
	"strings"
	"testing"

	"lab/apps/one/repo"
)

// Tests of Closed (SPEC 27) and of the app closing quiet Resolved items
// (SPEC 3). All data is invented.

// resolvedAt makes a Resolved item whose last message is lastMsgDays old and
// that the owner resolved resolvedDays ago.
func (e *agentEnv) resolvedAt(subject string, lastMsgDays, resolvedDays float64) repo.Item {
	e.t.Helper()
	it := e.seed(stageResolved, e.mail(subject, lastMsgDays))
	err := e.a.store.Update(e.ctx, func(tx *repo.Tx) error {
		return tx.SetItemStage(e.ctx, it.ID, stageResolved, e.ago(resolvedDays))
	})
	if err != nil {
		e.t.Fatal(err)
	}
	return e.item(it.ID)
}

func TestClosedLinkIsHiddenWithNoneAndShowsTheCount(t *testing.T) {
	e := newAgentEnv(t)
	e.newItem(stageNew, false, "Parcel", "")
	page := get(e.h, "/").Body.String()
	// the phone's link is hidden; on a laptop Closed is a stage in the left pane, with its count
	if strings.Contains(page, "Closed (") || !strings.Contains(page, `<p class="closed-link" id="closed-link" hidden>`) {
		t.Error("the Closed link shows with nothing closed")
	}
	if !strings.Contains(page, `<span class="st-name">Closed</span><span class="st-count" id="stage-closed-n">0</span>`) {
		t.Error("the left pane does not show Closed with none")
	}
	a := e.newItem(stageNew, true, "Newsletter", "")
	b := e.newItem(stageNew, true, "Offer", "")
	for _, it := range []repo.Item{a, b} {
		if m := e.post("/api/items/"+ref(it)+"/skip", map[string]any{"reason": "Noise."}); m["ok"] != true {
			t.Fatal(m)
		}
	}
	page = get(e.h, "/").Body.String()
	if !strings.Contains(page, `<a href="/closed">Closed (2, 2 not seen)</a>`) {
		t.Errorf("the link does not show the count:\n%s", page)
	}
	state := get(e.h, "/list/state").Body.String()
	if !strings.Contains(state, `"closed":2`) {
		t.Errorf("the state lacks the count: %s", state)
	}
}

func TestResolvedItemClosesAfterTheSetDaysWithoutNewsAndNotBefore(t *testing.T) {
	e := newAgentEnv(t)
	quiet := e.resolvedAt("Old matter", 30, 15)   // no news and resolved 15 days ago
	fresh := e.resolvedAt("Fresh matter", 30, 10) // resolved only 10 days ago
	late := e.resolvedAt("Late mail", 3, 20)      // a message 3 days ago
	if n, err := e.a.closeQuiet(e.ctx); err != nil || n != 1 {
		t.Fatalf("closeQuiet: %d, %v", n, err)
	}
	if e.item(quiet.ID).Stage != stageClosed {
		t.Error("an item quiet for 15 days stays Resolved with the default of 14")
	}
	if e.item(fresh.ID).Stage != stageResolved || e.item(late.ID).Stage != stageResolved {
		t.Error("an item with news inside the quiet time was closed")
	}
	// The closing is an event by the system, and the thread says so in the spec's words.
	evs := e.events(quiet.ID, "closed")
	if len(evs) != 1 || evs[0].Actor != "system" {
		t.Fatalf("events %+v", evs)
	}
	if body := e.openRow(quiet.ID); !strings.Contains(body, "Closed after 14 days without news.") {
		t.Errorf("the thread lacks the notice:\n%s", body)
	}
	// A second pass does nothing more.
	if n, _ := e.a.closeQuiet(e.ctx); n != 0 {
		t.Errorf("second pass closed %d", n)
	}
	// The setting decides the days.
	e.a.store.SetSetting(e.ctx, keyQuietDays, "7")
	if n, _ := e.a.closeQuiet(e.ctx); n != 1 || e.item(fresh.ID).Stage != stageClosed {
		t.Errorf("with 7 days the item resolved 10 days ago stays: %d", n)
	}
	if e.item(late.ID).Stage != stageResolved {
		t.Error("the item with a message 3 days ago was closed")
	}
	if body := e.openRow(fresh.ID); !strings.Contains(body, "Closed after 7 days without news.") {
		t.Error("the notice does not name the days that were set")
	}
}

func TestNewMailOnAResolvedItemBringsItBackAndItIsNotClosed(t *testing.T) {
	e := newAgentEnv(t)
	it := e.resolvedAt("Quiet matter", 30, 20)
	now := e.now.Unix()
	err := e.a.store.Update(e.ctx, func(tx *repo.Tx) error {
		cur, _ := tx.ItemByID(e.ctx, it.ID)
		cur, _ = applyArrival(cur, arrival{At: now - 60, Subject: "Quiet matter"}, now-30*86400, now)
		return tx.SaveItem(e.ctx, cur)
	})
	if err != nil {
		t.Fatal(err)
	}
	if e.item(it.ID).Stage != stageNew {
		t.Fatal("new mail did not bring the item back to New")
	}
	if n, _ := e.a.closeQuiet(e.ctx); n != 0 || e.item(it.ID).Stage != stageNew {
		t.Error("an item that came back was closed")
	}
}

func TestClosedPageWordingSectionsReasonsAndBringBack(t *testing.T) {
	e := newAgentEnv(t)
	skipped := e.newItem(stageNew, true, "Newsletter of a shop", "")
	if m := e.post("/api/items/"+ref(skipped)+"/skip", map[string]any{"reason": "A shop newsletter.", "kind": "noise"}); m["ok"] != true {
		t.Fatal(m)
	}
	byOwner := e.newItem(stageInProgress, false, "Plumber visit", "")
	pid := e.propose(byOwner, "close", "The visit is over.")
	if rec := e.decideAsOwner(pid, "approve"); rec.Code != 200 {
		t.Fatal(rec.Code)
	}
	e.resolvedAt("Insurance renewal", 40, 30)
	if _, err := e.a.closeQuiet(e.ctx); err != nil {
		t.Fatal(err)
	}
	rec := get(e.h, "/closed")
	page := rec.Body.String()
	for _, want := range []string{"<title>Closed</title>", `id="page-title">Closed</h2>`, "Closed by Claude", "Everything else", "Back to the queue",
		"Closed by Claude</p>", "Reason: A shop newsletter.", "Closed by you", "Reason: The visit is over.", "Closed by ONE", "Reason: Resolved, and no news for 14 days.", "Bring back"} {
		if !strings.Contains(page, want) {
			t.Errorf("the page lacks %q", want)
		}
	}
	// What Claude closed by itself is listed first.
	if strings.Index(page, "Newsletter of a shop") > strings.Index(page, "Plumber visit") || strings.Index(page, "Closed by Claude</h2>") > strings.Index(page, "Everything else") {
		t.Error("the section of Claude comes after the rest")
	}
	// Bring back: the item goes to New, with the notice, and nothing is deleted.
	back := post(e.h, fmt.Sprintf("/items/%d/bring-back", byOwner.ID), nil)
	if back.Code != 303 || back.Header().Get("Location") != "/closed?n=back" {
		t.Fatalf("bring back: %d %s", back.Code, back.Header().Get("Location"))
	}
	if e.item(byOwner.ID).Stage != stageNew {
		t.Error("the item did not return to New")
	}
	if page := get(e.h, "/closed?n=back").Body.String(); !strings.Contains(page, "You brought this back.") || strings.Contains(page, "Plumber visit") {
		t.Error("the notice is missing or the item is still listed as closed")
	}
	if body := e.openRow(byOwner.ID); !strings.Contains(body, "You brought this back.") {
		t.Error("the thread does not say it")
	}
	// A second press changes nothing.
	n := len(e.events(byOwner.ID, "brought_back"))
	post(e.h, fmt.Sprintf("/items/%d/bring-back", byOwner.ID), nil)
	if len(e.events(byOwner.ID, "brought_back")) != n {
		t.Error("a second press wrote another event")
	}
}

func TestBringBackIsNotUnderAPI(t *testing.T) {
	e := newAgentEnv(t)
	it := e.newItem(stageNew, true, "Newsletter", "")
	e.post("/api/items/"+ref(it)+"/skip", map[string]any{"reason": "Noise."})
	rec := e.api("POST", "/api/items/"+ref(it)+"/bring-back", nil)
	if rec.Code == 200 || e.stageOf(it) != stageClosed {
		t.Errorf("an agent brought an item back: %d", rec.Code)
	}
}

// closedAs seeds an item closed `days` ago by an actor with the closing event
// of the given type ("skipped" or "closed").
func (e *agentEnv) closedAs(subject, actor, typ string, days float64) repo.Item {
	e.t.Helper()
	it := e.seed(stageNew, e.mail(subject, days+1))
	at := e.ago(days)
	err := e.a.store.Update(e.ctx, func(tx *repo.Tx) error {
		if err := tx.SetItemStage(e.ctx, it.ID, stageClosed, at); err != nil {
			return err
		}
		return tx.InsertEvent(e.ctx, at, it.ID, actor, typ, map[string]any{"reason": "Reason of " + subject, "item": repo.Number(it.Number)})
	})
	if err != nil {
		e.t.Fatal(err)
	}
	return e.item(it.ID)
}

// closedWithManyByClaude makes 150 items closed by Claude (item i closed i+1
// hours ago) and 3 closed by the owner.
func (e *agentEnv) closedWithManyByClaude() {
	for i := 0; i < 150; i++ {
		e.closedAs(fmt.Sprintf("Claude item %03d", i), "claude", "skipped", float64(i+1)/24)
	}
	for i := 0; i < 3; i++ {
		e.closedAs(fmt.Sprintf("The owner item %d", i), "owner", "closed", float64(i+1)/24)
	}
}

func TestClosedFirstPageShowsBothSectionsWith150ByClaudeAnd3ByOwner(t *testing.T) {
	e := newAgentEnv(t)
	e.closedWithManyByClaude()
	page := get(e.h, "/closed").Body.String()
	for _, want := range []string{"Closed by Claude</h2>", "Everything else</h2>", "The owner item 0", "The owner item 1", "The owner item 2", "Claude item 000", "Claude item 024"} {
		if !strings.Contains(page, want) {
			t.Errorf("the first page lacks %q", want)
		}
	}
	if strings.Contains(page, "Claude item 025") {
		t.Error("the Claude section shows more than its first page")
	}
	if n := strings.Count(page, `class="closed-entry"`); n != closedPage+3 {
		t.Errorf("the page has %d entries, want %d", n, closedPage+3)
	}
	if !strings.Contains(page, "Show more") || strings.Count(page, "Show more") != 1 {
		t.Errorf("only the Claude section has more to show; got %d links", strings.Count(page, "Show more"))
	}
	if !strings.Contains(get(e.h, "/").Body.String(), "Closed (153, 150 not seen)") {
		t.Error("the count of the Closed link is no longer the number of all closed items")
	}
}

func TestClosedShowMoreOfOneSectionDoesNotChangeTheOther(t *testing.T) {
	e := newAgentEnv(t)
	e.closedWithManyByClaude()
	for i := 3; i < 40; i++ { // the owner's section now also has more than a page
		e.closedAs(fmt.Sprintf("The owner item %d", i), "owner", "closed", float64(i+1)/24)
	}
	first := get(e.h, "/closed").Body.String()
	if strings.Count(first, "Show more") != 2 {
		t.Fatalf("both sections should offer more, got %d", strings.Count(first, "Show more"))
	}
	// More of Claude's section: The owner's stays as it was.
	more := get(e.h, fmt.Sprintf("/closed?claude=%d", 2*closedPage)).Body.String()
	if !strings.Contains(more, "Claude item 049") || strings.Contains(more, "Claude item 050") {
		t.Error("the Claude section did not grow by one page")
	}
	// What a section shows is its entries; its "Show more" link may change.
	entries := func(s string) string { return s[:strings.LastIndex(s, "</ul>")] }
	cut := func(page string) string { return entries(page[strings.Index(page, "Everything else</h2>"):]) }
	if cut(more) != cut(first) {
		t.Error("showing more of Claude's section changed the other section")
	}
	// More of the owner's section: Claude's stays as it was.
	more = get(e.h, fmt.Sprintf("/closed?else=%d", 2*closedPage)).Body.String()
	if !strings.Contains(more, "The owner item 30") {
		t.Error("The owner's section did not grow")
	}
	end := func(page string) string { return entries(page[:strings.Index(page, "Everything else</h2>")]) }
	if end(more) != end(first) {
		t.Error("showing more of the owner's section changed Claude's section")
	}
	// The link of a section keeps what the other shows.
	if !strings.Contains(more, "/closed?claude=") && !strings.Contains(more, "else=") {
		t.Error("no link to show more")
	}
}

func TestClosedNewestFirstInsideEachSection(t *testing.T) {
	e := newAgentEnv(t)
	// Made in a mixed order on purpose.
	e.closedAs("Claude middle", "claude", "skipped", 2)
	e.closedAs("The owner oldest", "owner", "closed", 9)
	e.closedAs("Claude newest", "claude", "skipped", 1)
	e.closedAs("The owner newest", "owner", "closed", 3)
	e.closedAs("Claude oldest", "claude", "skipped", 5)
	e.closedAs("Rule newest", "rule", "skipped", 4)
	page := get(e.h, "/closed").Body.String()
	order := []string{"Closed by Claude</h2>", "Claude newest", "Claude middle", "Claude oldest", "Closed by a rule</h2>", "Rule newest", "Everything else</h2>", "The owner newest", "The owner oldest"}
	last := -1
	for _, s := range order {
		i := strings.Index(page, s)
		if i < 0 || i < last {
			t.Fatalf("%q is missing or out of order in:\n%s", s, page)
		}
		last = i
	}
}
