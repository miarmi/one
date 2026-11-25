package main

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"lab/apps/one/repo"
)

// SPEC 10 (invented data): an agent proposes a pattern, proposes to join two
// rows or to split one; each waits for Approve on the signed-in page.

func (e *agentEnv) pendingPatternProposals() []repo.MatterProposal {
	ps, err := e.a.store.WaitingMatterProposals(e.ctx)
	if err != nil {
		e.t.Fatal(err)
	}
	return ps
}

func TestSpec10AnAgentProposesAPatternAndNothingJoinsUntilOwnerApproves(t *testing.T) {
	e := newAgentEnv(t)
	m := e.post("/api/pattern-proposals", map[string]any{"pattern": `ORD-([0-9]+)`, "name": "Order", "reason": "Order numbers like ORD-1234 in his shop mail.", "agent": "Claude"})
	if m["ok"] != true || m["waits_for_owner"] != true {
		t.Fatalf("proposal: %v", m)
	}
	again := e.post("/api/pattern-proposals", map[string]any{"pattern": `ORD-([0-9]+)`, "reason": "Same."})
	if again["unchanged"] != true || again["pattern_proposal_id"] != m["pattern_proposal_id"] {
		t.Errorf("the same proposal twice: %v", again)
	}
	if len(e.pendingPatternProposals()) != 1 || len(e.a.matterPatterns(e.ctx, e.a.store.Q)) != 0 {
		t.Fatal("a proposal changed the patterns")
	}
	page := get(e.h, "/settings").Body.String()
	for _, want := range []string{"Proposal", "ORD-([0-9]&#43;)", ">Approve</button>", ">Dismiss</button>", "1 proposed pattern waits"} {
		if !strings.Contains(page, want) {
			t.Errorf("Settings lacks %q", want)
		}
	}
	// Nothing under /api/ approves; an agent that reaches the decision in code is refused.
	id := int64(m["pattern_proposal_id"].(float64))
	for _, p := range []string{fmt.Sprintf("/api/pattern-proposals/%d/decision/approve", id), "/api/matter-patterns", "/api/settings/matter-proposals/1/decision/approve"} {
		if rec := e.api("POST", p, map[string]any{"status": "approved"}); rec.Code != 403 && rec.Code != 404 && rec.Code != 405 {
			t.Errorf("%s answered %d", p, rec.Code)
		}
	}
	if err := e.a.decideMatterProposal(e.ctx, agentActor("the owner"), id, true); err == nil || !isRefusal(err.Error(), "Approval only") {
		t.Errorf("an agent decided: %v", err)
	}
	if len(e.a.matterPatterns(e.ctx, e.a.store.Q)) != 0 {
		t.Fatal("an agent added a pattern")
	}
	for _, tl := range mcpTools {
		if tl.Name == "propose_pattern" && tl.Annotations["readOnlyHint"] != false {
			t.Error("propose_pattern must say that it changes something")
		}
		if strings.Contains(tl.Name, "pattern") && tl.Name != "propose_pattern" {
			t.Errorf("tool %s", tl.Name)
		}
	}
	// The owner approves on the page: the pattern is his now.
	if rec := post(e.h, fmt.Sprintf("/settings/matter-proposals/%d/decision/approve", id), nil); rec.Code != 303 {
		t.Fatalf("approve: %d", rec.Code)
	}
	if ps := e.a.matterPatterns(e.ctx, e.a.store.Q); len(ps) != 1 || ps[0].Re != `ORD-([0-9]+)` || ps[0].Name != "Order" {
		t.Errorf("patterns after Approve: %v", ps)
	}
	if len(e.pendingPatternProposals()) != 0 {
		t.Error("the proposal still waits")
	}
	// A second tap changes nothing.
	post(e.h, fmt.Sprintf("/settings/matter-proposals/%d/decision/approve", id), nil)
	if len(e.a.matterPatterns(e.ctx, e.a.store.Q)) != 1 {
		t.Error("a second Approve added the pattern twice")
	}
}

func TestSpec10APatternProposalCanBeDismissedAndBadOnesAreRefused(t *testing.T) {
	e := newAgentEnv(t)
	m := e.post("/api/pattern-proposals", map[string]any{"pattern": `PAR-[0-9]+`, "reason": "Parcel numbers like PAR-77."})
	id := int64(m["pattern_proposal_id"].(float64))
	if rec := post(e.h, fmt.Sprintf("/settings/matter-proposals/%d/decision/dismiss", id), nil); rec.Code != 303 {
		t.Fatalf("dismiss: %d", rec.Code)
	}
	if len(e.a.matterPatterns(e.ctx, e.a.store.Q)) != 0 || len(e.pendingPatternProposals()) != 0 {
		t.Error("Dismiss left something behind")
	}
	post(e.h, "/settings/matter-patterns", url.Values{"pattern": {`INV-[0-9]+`}})
	for name, body := range map[string]map[string]any{
		"no reason":        {"pattern": `X-[0-9]+`},
		"bad expression":   {"pattern": `([`, "reason": "r"},
		"empty match":      {"pattern": `x*`, "reason": "r"},
		"The owner has it": {"pattern": `INV-[0-9]+`, "reason": "r"},
	} {
		if rec := e.api("POST", "/api/pattern-proposals", body); rec.Code < 400 {
			t.Errorf("%s: answered %d", name, rec.Code)
		}
	}
	res := e.rpc(e.a.agentTokenValue(), "tools/call", map[string]any{"name": "propose_pattern", "arguments": map[string]any{"pattern": `TKT-([0-9]+)`, "reason": "Ticket numbers like TKT-5."}})["result"].(map[string]any)
	if res["isError"] == true || len(e.pendingPatternProposals()) != 1 {
		t.Errorf("the tool: %v", res)
	}
}

func TestSpec10AnApprovedPatternProposalJoinsMailThatArrivesAfterwards(t *testing.T) {
	e := newEnv(t)
	e.a.imp.seedRules(e.ctx)
	if err := e.a.store.Update(e.ctx, func(tx *repo.Tx) error {
		_, err := tx.InsertMatterProposal(e.ctx, repo.MatterProposal{Name: "Case", Pattern: `CASE-([0-9]+)`, Reason: "Case numbers.", ProposedBy: "claude", ProposedAt: 1})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	e.box.add(e.in("100", "Case CASE-12 opened", 4))
	e.box.add(e.in("200", "Case CASE-12 update", 3))
	e.must(e.importMonth())
	if e.item("100").ID == e.item("200").ID {
		t.Fatal("a proposal that was not approved joined rows")
	}
	if err := e.a.decideMatterProposal(e.ctx, ownerActor(), 1, true); err != nil {
		t.Fatal(err)
	}
	e.box.add(e.in("300", "Case CASE-13 opened", 2))
	e.box.add(e.in("400", "Case CASE-13 closed", 1))
	e.must(e.importMonth())
	if e.item("300").ID != e.item("400").ID {
		t.Error("mail after Approve did not join the row of its reference")
	}
}

func TestSpec10APatternProposedUnderATestRunIsMarkedAppliesToItAndGoesWithIt(t *testing.T) {
	e := newRunEnv(t)
	code, m := e.apiRun(runA, "POST", "/api/pattern-proposals", map[string]any{"pattern": `RUN-([0-9]+)`, "reason": "Numbers like RUN-9."})
	if code != 200 {
		t.Fatalf("proposal: %d %v", code, m)
	}
	if e.count("matter_proposals", runA) != 1 || e.count("matter_proposals", "") != 0 {
		t.Fatal("the proposal does not carry the run's mark")
	}
	// The same pattern as a real proposal is a different one.
	if _, m := e.apiRun("", "POST", "/api/pattern-proposals", map[string]any{"pattern": `RUN-([0-9]+)`, "reason": "Real."}); m["unchanged"] == true {
		t.Error("a real proposal was answered with the run's")
	}
	id := int64(m["pattern_proposal_id"].(float64))
	if err := e.a.decideMatterProposal(e.ctx, ownerActor(), id, true); err != nil {
		t.Fatal(err)
	}
	if ps := e.a.matterPatterns(e.ctx, e.a.store.Q); len(ps) != 0 {
		t.Errorf("a run's pattern became one of the owner's: %v", ps)
	}
	if ps := e.a.matterPatternsFor(repo.WithRun(e.ctx, runA), e.a.store.Q); len(ps) != 1 {
		t.Errorf("the run's own patterns: %v", ps)
	}
	if ps := e.a.matterPatternsFor(repo.WithRun(e.ctx, runB), e.a.store.Q); len(ps) != 0 {
		t.Errorf("another run sees it: %v", ps)
	}
	status, gone := e.removeVia(e.a.agentTokenValue(), runA)
	if status != 200 || removedCount(gone, "matter_proposals") != 1 || e.count("matter_proposals", runA) != 0 {
		t.Errorf("removal: %d %v", status, gone)
	}
	if e.countAll("matter_proposals") != 1 {
		t.Error("the real proposal went with the run")
	}
}

// --- joining and splitting rows ---

func (e *agentEnv) proposeMerge(keep, join repo.Item, reason string) map[string]any {
	e.t.Helper()
	return e.post("/api/items/"+ref(keep)+"/merge-proposals", map[string]any{"join": ref(join), "reason": reason, "agent": "Claude"})
}

func TestSpec10AnAgentProposesToJoinTwoRowsAndNothingMovesUntilOwnerApproves(t *testing.T) {
	e := newAgentEnv(t)
	keep := e.seed(stageNew, e.mail("Order 55 shipped", 3))
	join := e.seed(stageNew, e.mail("Order 55 delay", 1))
	m := e.proposeMerge(keep, join, "Both are about order 55.")
	if m["ok"] != true || m["proposal_id"] == nil {
		t.Fatalf("proposal: %v", m)
	}
	if again := e.proposeMerge(keep, join, "Again."); again["unchanged"] != true {
		t.Errorf("twice: %v", again)
	}
	pid := int64(m["proposal_id"].(float64))
	rows := get(e.h, "/list/new").Body.String()
	if !strings.Contains(rows, "Join "+ref(join)+" into this row") {
		t.Errorf("the chip is missing on the closed row")
	}
	if e.a.store.MergedInto(e.ctx, join.ID) != 0 || len(mustMsgs(e, keep.ID)) != 1 {
		t.Fatal("a proposal moved something")
	}
	if err := func() error { _, err := e.a.decide(e.ctx, agentActor("the owner"), pid, true); return err }(); err == nil {
		t.Error("an agent approved")
	}
	if rec := e.decideAsOwner(pid, "approve"); rec.Code != 200 {
		t.Fatalf("approve: %d %s", rec.Code, rec.Body.String())
	}
	if n := len(mustMsgs(e, keep.ID)); n != 2 {
		t.Errorf("the row that stays holds %d messages, want 2", n)
	}
	if to := e.a.store.MergedInto(e.ctx, join.ID); to != keep.ID {
		t.Errorf("the joined row points at %d", to)
	}
	// The number of the joined row leads to the row that stays; its page too.
	if it, err := e.a.findItem(e.ctx, e.a.store.Q, ref(join)); err != nil || it.ID != keep.ID {
		t.Errorf("the number leads to %v %v", it.ID, err)
	}
	if rec := get(e.h, fmt.Sprintf("/items/%d", join.ID)); rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != fmt.Sprintf("/items/%d", keep.ID) {
		t.Errorf("the page of the joined row: %d %s", rec.Code, rec.Header().Get("Location"))
	}
	open := e.openRow(keep.ID)
	if want := "Claude joined " + ref(join) + " into this row."; !strings.Contains(open, want) {
		t.Errorf("the row lacks the notice %q", want)
	}
	if strings.Contains(get(e.h, "/").Body.String(), "Order 55 delay</") && strings.Count(get(e.h, "/").Body.String(), `class="row`) > 1 {
		t.Error("the joined row is still listed")
	}
	if got := e.item(keep.ID); got.LastAt != join.LastAt || got.FirstAt != keep.FirstAt {
		t.Errorf("the times of the row: %+v", got)
	}
}

func mustMsgs(e *agentEnv, id int64) []repo.Message {
	e.t.Helper()
	ms, _, err := e.a.store.MessagesSince(e.ctx, id, 0)
	if err != nil {
		e.t.Fatal(err)
	}
	return ms
}

func TestSpec10AJoinIsRefusedForTheSameRowAChatAndAnotherRunsRow(t *testing.T) {
	e := newAgentEnv(t)
	a := e.seed(stageNew, e.mail("Same one", 3))
	b := e.seed(stageNew, e.mail("Other", 2))
	if rec := e.api("POST", "/api/items/"+ref(a)+"/merge-proposals", map[string]any{"join": ref(a), "reason": "x"}); rec.Code != 409 {
		t.Errorf("a row with itself: %d", rec.Code)
	}
	if rec := e.api("POST", "/api/items/"+ref(a)+"/merge-proposals", map[string]any{"join": ref(b)}); rec.Code != 400 {
		t.Errorf("no reason: %d", rec.Code)
	}
	if rec := e.api("POST", "/api/items/"+ref(a)+"/merge-proposals", map[string]any{"join": "nonsense", "reason": "x"}); rec.Code != 400 {
		t.Errorf("a bad number: %d", rec.Code)
	}
	if rec := e.call("POST", "/api/items/"+ref(a)+"/merge-proposals", e.a.agentTokenValue(), map[string]any{"join": ref(b), "reason": "x"}); rec.Code != 200 {
		t.Errorf("a good one: %d", rec.Code)
	}
	// Once the owner approved, the rows cannot be joined again.
	if err := e.a.store.Update(e.ctx, func(tx *repo.Tx) error {
		if _, err := tx.JoinItems(e.ctx, a, b); err != nil {
			return err
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if msg := mergeMisfit(e.ctx, e.a.store.Q, a, b); !strings.Contains(msg, "joined into another") {
		t.Errorf("a row that was joined already: %q", msg)
	}
	// With the cross-channel trial switched off, a chat is refused as before.
	crossChannelJoin = false
	defer func() { crossChannelJoin = true }()
	if msg := mergeMisfit(e.ctx, e.a.store.Q, a, repo.Item{ID: 999, Kind: "chat"}); !strings.Contains(msg, "chat") {
		t.Errorf("a chat: %q", msg)
	}
	crossChannelJoin = true
	if msg := mergeMisfit(e.ctx, e.a.store.Q, a, repo.Item{ID: 998, Kind: "thread", TestRun: runA}); msg == "" {
		t.Error("a row of a run was joined with a real one")
	}
}

func TestSpec10AnAgentProposesToSplitAWronglyJoinedRowAndOwnerApprovesIt(t *testing.T) {
	e := newEnv(t)
	post(e.h, "/settings/matter-patterns", url.Values{"name": {"Case"}, "pattern": {`CASE-([0-9]+)`}})
	e.box.add(e.in("100", "Case CASE-7 opened", 4))
	e.box.add(e.in("200", "Case CASE-7 update", 2))
	e.must(e.importMonth())
	row := e.item("100")
	if e.item("200").ID != row.ID {
		t.Fatal("the two threads did not join")
	}
	// Mail with a number the pattern read wrongly: the agent splits the second thread off.
	ms, _, _ := e.a.store.MessagesSince(e.ctx, row.ID, 0)
	var second repo.Message
	for _, m := range ms {
		if m.ThreadKey == "gmail:200" {
			second = m
		}
	}
	if second.ID == 0 {
		t.Fatal("no message of the second thread")
	}
	res, err := e.a.proposeSplit(e.ctx, agentActor("Claude"), ref(row), second.ID, "This is another case.")
	if err != nil || res.Proposal == 0 {
		t.Fatalf("proposal: %v", err)
	}
	if again, _ := e.a.proposeSplit(e.ctx, agentActor("Claude"), ref(row), second.ID, "Again."); !again.Same {
		t.Error("the same split twice")
	}
	if n := e.a.store.ThreadsOfItem(e.ctx, row.ID); n != 2 {
		t.Fatalf("a proposal split the row: %d threads", n)
	}
	if _, err := e.a.decide(e.ctx, agentActor("Claude"), res.Proposal, true); err == nil {
		t.Error("an agent approved a split")
	}
	if _, err := e.a.decide(e.ctx, ownerActor(), res.Proposal, true); err != nil {
		t.Fatal(err)
	}
	a, b := e.item("100"), e.item("200")
	if a.ID != row.ID || b.ID == row.ID {
		t.Fatalf("rows after the split: %d %d", a.ID, b.ID)
	}
	if n := e.a.store.ThreadsOfItem(e.ctx, row.ID); n != 1 || e.a.store.ThreadsOfItem(e.ctx, b.ID) != 1 {
		t.Error("the messages were not shared out")
	}
	if c := e.counts(); c["messages"] != 2 || c["items"] != 2 {
		t.Errorf("counts %v: nothing may be lost", c)
	}
	if b.Subject != "Case CASE-7 update" || a.Subject != "Case CASE-7 opened" {
		t.Errorf("subjects: %q %q", a.Subject, b.Subject)
	}
	notice := "Claude split " + repo.Number(b.Number) + " off this row."
	entries, _ := e.a.threadEntries(e.ctx, row.ID, e.a.now())
	found := false
	for _, en := range entries {
		found = found || en.Text == notice
	}
	if !found {
		t.Errorf("no notice %q", notice)
	}
	// Mail of the second thread goes to its own row from now on.
	e.box.add(e.in("200", "Re: Case CASE-7 update", 1))
	e.must(e.importMonth())
	if e.item("200").ID != b.ID {
		t.Error("the thread went back to the old row")
	}
}

func TestSpec10AMergeUnderATestRunStaysInTheRunAndGoesWithIt(t *testing.T) {
	e := newRunEnv(t)
	ctxA := repo.WithRun(e.ctx, runA)
	var a, b repo.Item
	if err := e.a.store.Update(ctxA, func(tx *repo.Tx) error {
		var err error
		if a, err = tx.CreateItem(ctxA, repo.Item{Kind: "thread", Subject: "A", Stage: stageNew, FirstAt: 100, LastAt: 100}, "test_thread", "ra", "system", 100); err != nil {
			return err
		}
		b, err = tx.CreateItem(ctxA, repo.Item{Kind: "thread", Subject: "B", Stage: stageNew, FirstAt: 200, LastAt: 200}, "test_thread", "rb", "system", 200)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	real := e.freshChat("", "x@shop.test", "real")
	code, m := e.apiRun(runA, "POST", "/api/items/"+repo.Number(a.Number)+"/merge-proposals", map[string]any{"join": repo.Number(b.Number), "reason": "Same."})
	if code != 200 {
		t.Fatalf("a run's own rows: %d %v", code, m)
	}
	if code, _ := e.apiRun(runA, "POST", "/api/items/"+repo.Number(a.Number)+"/merge-proposals", map[string]any{"join": repo.Number(real.Number), "reason": "x"}); code != 403 && code != 409 {
		t.Errorf("a real row in a run's merge: %d", code)
	}
	if code, _ := e.apiRun(runB, "POST", "/api/items/"+repo.Number(a.Number)+"/merge-proposals", map[string]any{"join": repo.Number(b.Number), "reason": "x"}); code != 403 {
		t.Errorf("another run's rows: %d", code)
	}
	if _, err := e.a.decide(e.ctx, ownerActor(), int64(m["proposal_id"].(float64)), true); err != nil {
		t.Fatal(err)
	}
	if e.a.store.MergedInto(e.ctx, b.ID) != a.ID {
		t.Error("the run's rows were not joined")
	}
	if status, gone := e.removeVia(e.a.agentTokenValue(), runA); status != 200 || removedCount(gone, "items") != 2 {
		t.Errorf("removal: %d %v", status, gone)
	}
}
