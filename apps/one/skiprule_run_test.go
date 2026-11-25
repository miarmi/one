package main

import (
	"fmt"
	"net/url"
	"strings"
	"testing"
	"time"

	"lab/apps/one/repo"
)

// SPEC 31 (invented data): skip rules for a sender cover chats; a sender marked
// Always show me is refused with 403; a skip rule proposed under a test run is
// real for that run alone (API.md, "Test runs").

const skipRunA = "e2e-skiprunaaaa"
const skipRunB = "e2e-skiprunbbbb"

// chatsFrom posts n quiet chats from one sender (under the run, "" for real),
// lets Claude look at them and clear-skips each as an agent. It returns the items.
func (e *runEnv) chatsFrom(run, sender, tag string, n int, skipThem bool) []repo.Item {
	e.t.Helper()
	var items []repo.Item
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("%s-%s-%d", tag, run, i)
		c := chatOf("messenger", id, "Offers "+id, "direct", e.in("m1", sender, "Bulk Sender", "An invented offer "+id, 120))
		e.mustBatchRun(run, c)
		items = append(items, e.itemOfChat("messenger", id))
	}
	claudeLooksAtAll(e.t, e.a)
	if skipThem {
		for _, it := range items {
			code, m := e.apiRun(run, "POST", "/api/items/"+ref(it)+"/skip", map[string]any{"reason": "an invented reason, nothing needed", "kind": "noise"})
			if code != 200 {
				e.t.Fatalf("skip: %d %v", code, m)
			}
		}
	}
	return items
}

// proposeSkip proposes a skip rule for the sender (under the run) and returns the status and answer.
func (e *runEnv) proposeSkip(run, sender, name string) (int, map[string]any) {
	return e.apiRun(run, "POST", "/api/skip-rule-proposals", map[string]any{"field": "sender", "value": sender, "name": name,
		"reason": "skipped 5 times, brought back 0 times"})
}

func (e *runEnv) approveSkip(id float64) {
	e.t.Helper()
	if rec := e.formPost(fmt.Sprintf("/settings/skip-rules/%d/decision/approve", int64(id)), url.Values{}); rec.Code != 303 {
		e.t.Fatalf("approve: %d %s", rec.Code, rec.Body.String())
	}
	e.a.skipRulesPass(e.ctx)
}

// a quiet chat from the sender, as the bridge posts it (under the run, "" for real).
func (e *runEnv) freshChat(run, sender, id string) repo.Item {
	e.t.Helper()
	e.mustBatchRun(run, chatOf("messenger", id, "Fresh "+id, "direct", e.in("m1", sender, "Bulk Sender", "A fresh invented offer "+id, 120)))
	return e.itemOfChat("messenger", id)
}

func (e *runEnv) fresh(it repo.Item) repo.Item {
	it2, _ := e.a.store.ItemByID(e.ctx, it.ID)
	return it2
}

// SPEC 31 (test "a new chat item from that sender is closed"): an approved rule
// for a sender closes a new chat of that sender, with the rule as the reason.
func TestSpec31ASenderRuleClosesANewChatItemFromThatSender(t *testing.T) {
	e := newRunEnv(t)
	const sender = "bulk-real@shop.test"
	e.chatsFrom("", sender, "real", 5, true)
	code, ans := e.proposeSkip("", sender, "Skip chats from bulk-real")
	if code != 200 {
		t.Fatalf("propose: %d %v", code, ans)
	}
	e.approveSkip(ans["skip_rule_proposal_id"].(float64))
	it := e.freshChat("", sender, "fresh-real")
	if got := e.fresh(it); got.Stage != stageClosed || got.NeedsClaude {
		t.Errorf("a new chat from the sender is %q, waiting=%v; want closed", got.Stage, got.NeedsClaude)
	}
	if !strings.Contains(fmt.Sprint(e.events(it.ID, "skipped")), "Closed by a rule: Skip chats from bulk-real") {
		t.Error("the item does not say it was closed by the rule")
	}
	// A live chat is closed too once its quiet time has passed.
	live := chatOf("messenger", "live-real", "Live", "direct", e.in("m1", sender, "Bulk Sender", "A live invented offer", 1))
	e.mustBatchRun("", live)
	li := e.itemOfChat("messenger", "live-real")
	if got := e.fresh(li); got.Stage != stageNew {
		t.Fatalf("a live chat was closed before its quiet time: %q", got.Stage)
	}
	e.now = e.now.Add(31 * time.Minute)
	e.queueIDs() // reading the queue releases the chats whose quiet time is over
	if got := e.fresh(li); got.Stage != stageClosed {
		t.Errorf("a chat that went quiet is %q, want closed", got.Stage)
	}
}

// SPEC 31 / API.md: a sender marked Always show me is refused with 403, in a chat too.
func TestSpec31ASkipRuleForAnAlwaysShowMeSenderIsRefusedWith403(t *testing.T) {
	e := newRunEnv(t)
	const sender = "vip-real@shop.test"
	e.chatsFrom("", sender, "vip", 5, true)
	if code, m := e.apiRun("", "POST", "/api/people/details", map[string]any{"person": "Bulk Sender", "always_show": true}); code != 200 {
		t.Fatalf("always show: %d %v", code, m)
	}
	code, ans := e.proposeSkip("", sender, "Skip the VIP")
	if code != 403 || ans["ok"] != false || !strings.Contains(fmt.Sprint(ans["error"]), "Protected people") {
		t.Errorf("proposal for an Always show me sender: %d %v", code, ans)
	}
	if n := e.a.store.CountSkipRuleProposals(e.ctx); n != 0 {
		t.Errorf("%d proposals wait", n)
	}
}

// SPEC 31 and API.md "Test runs": a rule proposed under a run is accepted, marked,
// counted in Test data, approved on the page, applies to that run's items only,
// never counts in real numbers, and goes with the run.
func TestSkipRuleOfATestRunClosesOnlyThatRunsItems(t *testing.T) {
	e := newRunEnv(t)
	const sender = "bulk-shared@shop.test"
	e.chatsFrom(skipRunA, sender, "a", 5, true)
	e.chatsFrom(skipRunB, sender, "b", 5, true)
	realItem := e.freshChat("", sender, "real-waiting") // a real item from the same sender, waiting

	// Accepted, carries the run's mark, shown with the run's counts.
	code, ans := e.proposeSkip(skipRunA, sender, "Run A rule")
	if code != 200 {
		t.Fatalf("propose under a run: %d %v", code, ans)
	}
	id := ans["skip_rule_proposal_id"].(float64)
	if e.count("skip_rules", skipRunA) != 1 || e.count("skip_rules", "") != 0 {
		t.Fatalf("the rule is not marked with the run: run=%d real=%d", e.count("skip_rules", skipRunA), e.count("skip_rules", ""))
	}
	if page := get(e.h, "/settings").Body.String(); !strings.Contains(page, "1 skip rule") || !strings.Contains(page, skipRunA) {
		t.Error("the Test data section does not count the run's skip rule")
	}
	// It never counts in real numbers: no real proposal waits, no real skip is counted.
	if n := e.a.store.CountSkipRuleProposals(e.ctx); n != 0 {
		t.Errorf("a run's proposal counts as a real one: %d", n)
	}
	if sk, _, _ := e.a.store.AgentSkips(e.ctx, "address", sender); sk != 0 {
		t.Errorf("a run's skipped items count in the real numbers: %d", sk)
	}
	// The same proposal twice answers the waiting one; the other run may make its own.
	if _, again := e.proposeSkip(skipRunA, sender, "Run A rule"); again["unchanged"] != true {
		t.Errorf("the same proposal twice: %v", again)
	}
	// Nothing happens before Approve.
	a0 := e.freshChat(skipRunA, sender, "a-before")
	if got := e.fresh(a0); got.Stage == stageClosed {
		t.Fatal("an item was closed by a rule that was only proposed")
	}
	e.approveSkip(id)

	// Run A's new item is closed; run B's, the real one from the same sender, and the real waiting one are not.
	aNew := e.freshChat(skipRunA, sender, "a-after")
	bNew := e.freshChat(skipRunB, sender, "b-after")
	realNew := e.freshChat("", sender, "real-after")
	if got := e.fresh(aNew); got.Stage != stageClosed {
		t.Errorf("the run's own item is %q, want closed", got.Stage)
	}
	if got := e.fresh(a0); got.Stage != stageClosed {
		t.Errorf("the run's item that waited when the rule was approved is %q, want closed", got.Stage)
	}
	for name, it := range map[string]repo.Item{"another run's": bNew, "a real": realNew, "the waiting real": realItem} {
		if got := e.fresh(it); got.Stage != stageNew || !got.NeedsClaude {
			t.Errorf("%s item from the same sender was touched: %q waiting=%v", name, got.Stage, got.NeedsClaude)
		}
	}
	// Removing the run removes the rule, its proposals (the events of its life) and its items; nothing else.
	realBefore, bBefore := e.state(""), e.state(skipRunB)
	status, gone := e.removeVia(e.a.agentTokenValue(), skipRunA)
	if status != 200 || removedCount(gone, "skip_rules") != 1 {
		t.Fatalf("removing the run: %d %v", status, gone)
	}
	if e.count("skip_rules", skipRunA) != 0 || e.countAll("skip_rules") != 0 {
		t.Error("the rule is still there after the run was removed")
	}
	var left int
	e.db.QueryRow(`SELECT COUNT(*) FROM events WHERE type LIKE 'skip_rule_%' AND test_run = ?`, skipRunA).Scan(&left)
	if left != 0 {
		t.Errorf("%d entries about the rule are left", left)
	}
	if fmt.Sprint(e.state("")) != fmt.Sprint(realBefore) || fmt.Sprint(e.state(skipRunB)) != fmt.Sprint(bBefore) {
		t.Error("removing the run changed something that is not the run's")
	}
	if got := e.fresh(realItem); got.Stage != stageNew {
		t.Error("the real item changed")
	}
}

// A real, approved rule never touches the items of a test run.
func TestARealSkipRuleNeverTouchesATestRunsItems(t *testing.T) {
	e := newRunEnv(t)
	const sender = "bulk-both@shop.test"
	e.chatsFrom("", sender, "real", 5, true)
	code, ans := e.proposeSkip("", sender, "Real rule")
	if code != 200 {
		t.Fatalf("propose: %d %v", code, ans)
	}
	e.approveSkip(ans["skip_rule_proposal_id"].(float64))
	inRun := e.freshChat(skipRunA, sender, "a-run")
	realNew := e.freshChat("", sender, "real-new")
	if got := e.fresh(inRun); got.Stage != stageNew || !got.NeedsClaude {
		t.Errorf("a real rule touched a test run's item: %q waiting=%v", got.Stage, got.NeedsClaude)
	}
	if got := e.fresh(realNew); got.Stage != stageClosed {
		t.Errorf("the real rule did not close a real chat: %q", got.Stage)
	}
}
