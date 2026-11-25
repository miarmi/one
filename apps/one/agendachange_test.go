package main

import (
	"fmt"
	"net/url"
	"strings"
	"testing"

	"lab/apps/one/repo"
)

// Tests of the to-dos as a work list for agents (SPEC 19, 29): post_agenda
// with no item and a due date, and update_agenda. All of it is a proposal.

func (e *agentEnv) openTodos() []repo.Agenda {
	e.t.Helper()
	ts, err := e.a.store.OpenTodos(e.ctx)
	if err != nil {
		e.t.Fatal(err)
	}
	return ts
}

func (e *agentEnv) addTodo(text string) repo.Agenda {
	e.t.Helper()
	if _, err := e.a.addTodo(e.ctx, text); err != nil {
		e.t.Fatal(err)
	}
	e.now = e.now.Add(0) // the clock is the test's own
	ts := e.openTodos()
	for _, t := range ts {
		if t.Title == text {
			return t
		}
	}
	e.t.Fatalf("no to-do %q", text)
	return repo.Agenda{}
}

func (e *agentEnv) decide(id any, what string) int {
	return e.ajaxPost(fmt.Sprintf("/agenda/proposals/%v/%s", id, what), url.Values{}).Code
}

func (e *agentEnv) agendaPage() string { return get(e.h, "/agenda").Body.String() }

func TestPostAgendaWithNoItemIsAProposalInTheAgendaAndAddsNothingBeforeApprove(t *testing.T) {
	e := newAgentEnv(t)
	m := e.toolJSON("post_agenda", map[string]any{"text": "Renew the car insurance", "due": "2026-12-01", "reason": "The policy ends on 1 December.", "agent": "Claude"})
	id := m["proposal_id"]
	if m["ok"] != true || id == nil {
		t.Fatalf("post_agenda: %v", m)
	}
	if len(e.openTodos()) != 0 {
		t.Fatal("a to-do was added before Approve")
	}
	page := e.agendaPage()
	for _, want := range []string{"Proposal", "Claude", "To do:", "Renew the car insurance", "Tue 1 Dec", "The policy ends on 1 December.", "Approve", "Dismiss"} {
		if !strings.Contains(page, want) {
			t.Errorf("the agenda lacks %q:\n%s", want, page)
		}
	}
	// The same proposal twice returns the waiting one.
	again := e.toolJSON("post_agenda", map[string]any{"text": "Renew the car insurance", "due": "2026-12-01", "reason": "Again.", "agent": "Claude"})
	if again["unchanged"] != true || again["proposal_id"] != id {
		t.Errorf("twice: %v", again)
	}
	// Approve adds it, made by the agent, with its due date.
	if code := e.decide(int(id.(float64)), "approve"); code != 200 {
		t.Fatalf("approve: %d", code)
	}
	ts := e.openTodos()
	if len(ts) != 1 || ts[0].Title != "Renew the car insurance" || ts[0].CreatedBy != "claude" || ts[0].ItemID != 0 || e.a.dueOf(ts[0]) != "2026-12-01" {
		t.Fatalf("todos: %+v", ts)
	}
	if page := e.agendaPage(); strings.Contains(page, "Approve") || !strings.Contains(page, "Tue 1 Dec") {
		t.Errorf("after Approve:\n%s", page)
	}
	// A second Approve does nothing.
	e.decide(int(id.(float64)), "approve")
	if len(e.openTodos()) != 1 {
		t.Error("a second Approve added another")
	}
	// The agenda tool lists it with its due date.
	la := e.toolJSON("list_agenda", map[string]any{})
	td := la["todos"].([]any)[0].(map[string]any)
	if td["due"] != "2026-12-01" || td["made_by"] != "Claude" {
		t.Errorf("list_agenda: %v", td)
	}
}

func TestPostAgendaWithATimeAndDismiss(t *testing.T) {
	e := newAgentEnv(t)
	m := e.toolJSON("post_agenda", map[string]any{"text": "Call the carrier", "due": "2026-11-25T14:30", "reason": "Parcel is late."})
	if page := e.agendaPage(); !strings.Contains(page, "Wed 25 Nov, 14:30") {
		t.Errorf("a time: %s", page)
	}
	if code := e.decide(int(m["proposal_id"].(float64)), "dismiss"); code != 200 {
		t.Fatal(code)
	}
	if len(e.openTodos()) != 0 || strings.Contains(e.agendaPage(), "Call the carrier") {
		t.Error("Dismiss added it or left it showing")
	}
}

func TestPostAgendaRefusals(t *testing.T) {
	e := newAgentEnv(t)
	for name, args := range map[string]map[string]any{
		"no reason without an item": {"text": "x"},
		"empty text":                {"text": " ", "reason": "r"},
		"bad due":                   {"text": "x", "due": "next week", "reason": "r"},
		"bad item":                  {"text": "x", "item": "nope", "reason": "r"},
	} {
		if text, bad := e.tool("post_agenda", args); !bad {
			t.Errorf("%s: %q", name, text)
		}
	}
	if len(e.openTodos()) != 0 || e.a.store.CountAgendaProposals(e.ctx) != 0 {
		t.Error("a refused call stored something")
	}
	// With an item the proposal stays on the item, and a due date rides along.
	it := e.newItem(stageNew, false, "Parcel", "")
	m := e.toolJSON("post_agenda", map[string]any{"item": ref(it), "text": "Chase the parcel", "due": "2026-11-30"})
	if m["ok"] != true || e.a.store.CountAgendaProposals(e.ctx) != 0 {
		t.Fatalf("item: %v", m)
	}
	ps, _ := e.a.store.ProposalsOfItem(e.ctx, it.ID)
	var pr repo.Proposal
	for _, x := range ps {
		pr = x
	}
	if len(ps) != 1 || pr.Due != "2026-11-30" {
		t.Fatalf("proposals: %+v", ps)
	}
	if !strings.Contains(e.openRow(it.ID), "Mon 30 Nov") {
		t.Error("the row does not say the due date")
	}
	e.ajaxPost(fmt.Sprintf("/proposals/%d/approve", pr.ID), nil)
	if ts := e.openTodos(); len(ts) != 1 || e.a.dueOf(ts[0]) != "2026-11-30" || ts[0].ItemID != it.ID {
		t.Errorf("approved: %+v", ts)
	}
}

func TestUpdateAgendaEveryChangeIsAProposalThatChangesNothingBeforeApprove(t *testing.T) {
	e := newAgentEnv(t)
	a := e.addTodo("Call bank about the loan")
	b := e.addTodo("Call the bank about loan")
	c := e.addTodo("Buy stamps and pay the invoice")
	d := e.addTodo("Old thing")
	f := e.addTodo("Finished already")
	g := e.addTodo("Call bank loan again")
	call := func(args map[string]any) map[string]any {
		args["agent"], args["reason"] = "Claude", "Because."
		return e.toolJSON("update_agenda", args)
	}
	edit := call(map[string]any{"id": a.ID, "change": "edit", "text": "Call Acme Bank about the loan", "due": "2026-12-03T09:00"})
	join := call(map[string]any{"id": b.ID, "change": "join", "with": []int{int(g.ID)}})
	split := call(map[string]any{"id": c.ID, "change": "split", "parts": []map[string]any{{"text": "Buy stamps"}, {"text": "Pay the invoice", "due": "2026-12-05"}}})
	drop := call(map[string]any{"id": d.ID, "change": "drop"})
	done := call(map[string]any{"id": f.ID, "change": "done"})
	for _, m := range []map[string]any{edit, join, split, drop, done} {
		if m["ok"] != true || m["proposal_id"] == nil {
			t.Fatalf("a proposal: %v", m)
		}
	}
	if n := len(e.openTodos()); n != 6 {
		t.Fatalf("something changed before Approve: %d to-dos", n)
	}
	if again := call(map[string]any{"id": d.ID, "change": "drop"}); again["unchanged"] != true {
		t.Errorf("twice: %v", again)
	}
	page := e.agendaPage()
	for _, want := range []string{"Call Acme Bank about the loan", "<s>Call bank about the loan</s>", "<s>Call bank loan again</s>", "Pay the invoice", "Done:", "<s>Old thing</s>", "Because."} {
		if !strings.Contains(page, want) {
			t.Errorf("the agenda lacks %q", want)
		}
	}
	if strings.Count(page, ">Approve<") != 5 || strings.Count(page, ">Dismiss<") != 5 {
		t.Errorf("Approve and Dismiss: %d %d", strings.Count(page, ">Approve<"), strings.Count(page, ">Dismiss<"))
	}
	// edit
	if e.decide(int(edit["proposal_id"].(float64)), "approve") != 200 {
		t.Fatal("approve edit")
	}
	got, _ := e.a.store.AgendaByID(e.ctx, a.ID)
	if got.Title != "Call Acme Bank about the loan" || e.a.dueOf(got) != "2026-12-03T09:00" || got.State != "open" {
		t.Errorf("edit: %+v", got)
	}
	// join: g goes into b.
	if e.decide(int(join["proposal_id"].(float64)), "approve") != 200 {
		t.Fatal("approve join")
	}
	if gb, _ := e.a.store.AgendaByID(e.ctx, g.ID); gb.State != "dropped" {
		t.Errorf("join left the duplicate: %+v", gb)
	}
	// split: two new to-dos, the original gone.
	e.decide(int(split["proposal_id"].(float64)), "approve")
	if gc, _ := e.a.store.AgendaByID(e.ctx, c.ID); gc.State != "dropped" {
		t.Errorf("split kept the original: %+v", gc)
	}
	titles := map[string]string{}
	for _, t := range e.openTodos() {
		titles[t.Title] = e.a.dueOf(t)
	}
	if titles["Buy stamps"] != "" || titles["Pay the invoice"] != "2026-12-05" {
		t.Errorf("split parts: %v", titles)
	}
	// drop and done
	e.decide(int(drop["proposal_id"].(float64)), "approve")
	e.decide(int(done["proposal_id"].(float64)), "approve")
	if gd, _ := e.a.store.AgendaByID(e.ctx, d.ID); gd.State != "dropped" {
		t.Errorf("drop: %+v", gd)
	}
	if gf, _ := e.a.store.AgendaByID(e.ctx, f.ID); gf.State != "done" {
		t.Errorf("done: %+v", gf)
	}
	open := e.openTodos()
	var names []string
	for _, t := range open {
		names = append(names, t.Title)
	}
	if len(open) != 4 {
		t.Errorf("what is left: %v", names)
	}
	// Dropped to-dos stay in the record but are not listed, even with include_done.
	la := e.toolJSON("list_agenda", map[string]any{"include_done": true})
	for _, x := range la["todos"].([]any) {
		if s := x.(map[string]any)["text"]; s == "Old thing" || s == "Call bank loan again" {
			t.Errorf("a dropped to-do is listed: %v", s)
		}
	}
	if evs, _ := e.a.store.RawEventsFull(e.ctx, "agenda_dropped"); len(evs) != 3 {
		t.Errorf("dropped events: %d", len(evs))
	}
}

func TestUpdateAgendaRefusesACalendarEntryAndWhatDoesNotFit(t *testing.T) {
	e := newAgentEnv(t)
	todo := e.addTodo("Water the plants")
	var evID int64
	if err := e.a.store.Update(e.ctx, func(tx *repo.Tx) error {
		var err error
		evID, err = tx.InsertAgenda(e.ctx, repo.Agenda{Kind: "event", Source: "icloud", SourceID: "x", CalKey: "k", Calendar: "Family", Title: "Dentist", StartAt: e.now.Unix() + 3600, EndAt: e.now.Unix() + 7200,
			Status: "confirmed", State: "open", Version: 1, Hash: "h", CreatedAt: e.now.Unix(), UpdatedAt: e.now.Unix()})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	for _, ch := range []string{"edit", "drop", "done"} {
		args := map[string]any{"id": evID, "change": ch, "reason": "r"}
		if ch == "edit" {
			args["text"] = "Moved"
		}
		text, bad := e.tool("update_agenda", args)
		if !bad || !strings.Contains(text, "calendar entry") {
			t.Errorf("%s on an event: %q %v", ch, text, bad)
		}
	}
	text, bad := e.tool("update_agenda", map[string]any{"id": todo.ID, "change": "join", "with": []int{int(evID)}, "reason": "r"})
	if !bad || !strings.Contains(text, "calendar entry") {
		t.Errorf("join with an event: %q", text)
	}
	if ev, _ := e.a.store.AgendaByID(e.ctx, evID); ev.Title != "Dentist" || ev.State != "open" || e.a.store.CountAgendaProposals(e.ctx) != 0 {
		t.Error("the calendar entry or the proposals changed")
	}
	cases := []struct {
		name string
		args map[string]any
		want string
	}{
		{"no such", map[string]any{"id": 9999, "change": "drop", "reason": "r"}, "no such to-do"},
		{"no reason", map[string]any{"id": todo.ID, "change": "drop"}, "reason is missing"},
		{"bad change", map[string]any{"id": todo.ID, "change": "rename", "reason": "r"}, "change must be"},
		{"edit nothing", map[string]any{"id": todo.ID, "change": "edit", "reason": "r"}, "needs new words"},
		{"edit same", map[string]any{"id": todo.ID, "change": "edit", "text": "Water the plants", "reason": "r"}, "already"},
		{"bad due", map[string]any{"id": todo.ID, "change": "edit", "due": "tomorrow", "reason": "r"}, "due must be"},
		{"one part", map[string]any{"id": todo.ID, "change": "split", "parts": []map[string]any{{"text": "a"}}, "reason": "r"}, "2 to 8"},
		{"empty part", map[string]any{"id": todo.ID, "change": "split", "parts": []map[string]any{{"text": "a"}, {"text": " "}}, "reason": "r"}, "Write what"},
		{"join itself", map[string]any{"id": todo.ID, "change": "join", "with": []int{int(todo.ID)}, "reason": "r"}, "other, different"},
		{"join nobody", map[string]any{"id": todo.ID, "change": "join", "reason": "r"}, "join needs"},
		{"drop with text", map[string]any{"id": todo.ID, "change": "drop", "text": "x", "reason": "r"}, "takes no text"},
	}
	for _, c := range cases {
		if text, bad := e.tool("update_agenda", c.args); !bad || !strings.Contains(text, c.want) {
			t.Errorf("%s: %q %v", c.name, text, bad)
		}
	}
	// A to-do ticked off no longer takes a proposal, and a waiting one is withdrawn.
	m := e.toolJSON("update_agenda", map[string]any{"id": todo.ID, "change": "edit", "text": "Water the plants twice", "reason": "r"})
	e.ajaxPost(fmt.Sprintf("/agenda/%d/done", todo.ID), nil)
	if p, _ := e.a.store.AgendaProposalByID(e.ctx, int64(m["proposal_id"].(float64))); p.Status != "withdrawn" {
		t.Errorf("ticked off: the proposal is %s", p.Status)
	}
	if text, bad := e.tool("update_agenda", map[string]any{"id": todo.ID, "change": "drop", "reason": "r"}); !bad || !strings.Contains(text, "not open") {
		t.Errorf("on a ticked-off to-do: %q", text)
	}
	if page := e.agendaPage(); strings.Contains(page, "Water the plants twice") {
		t.Error("a withdrawn proposal still shows")
	}
	// A proposal made against a to-do that was dropped by another approval is withdrawn on Approve.
	x := e.addTodo("Duplicate one")
	y := e.addTodo("Duplicate two")
	m1 := e.toolJSON("update_agenda", map[string]any{"id": x.ID, "change": "join", "with": []int{int(y.ID)}, "reason": "r"})
	m2 := e.toolJSON("update_agenda", map[string]any{"id": y.ID, "change": "edit", "text": "Duplicate 2", "reason": "r"})
	e.decide(int(m1["proposal_id"].(float64)), "approve")
	if p, _ := e.a.store.AgendaProposalByID(e.ctx, int64(m2["proposal_id"].(float64))); p.Status != "withdrawn" {
		t.Errorf("a proposal about a joined to-do: %s", p.Status)
	}
}

func TestAgendaProposalsOnlyOwnerDecidesAndTheTestRunMarkHolds(t *testing.T) {
	e := newAgentEnv(t)
	todo := e.addTodo("Real to-do")
	m := e.toolJSON("post_agenda", map[string]any{"text": "A new one", "reason": "r"})
	id := int64(m["proposal_id"].(float64))
	// An agent has no way to approve: the address does not exist under /api/, and the code refuses any actor but the owner.
	for _, p := range []string{fmt.Sprintf("/api/agenda/proposals/%d/approve", id), fmt.Sprintf("/api/agenda/changes/%d/approve", id)} {
		if rec := e.api("POST", p, map[string]any{}); rec.Code == 200 {
			t.Errorf("%s: %d", p, rec.Code)
		}
	}
	if _, err := e.a.decideAgendaProposal(e.ctx, agentActor("Claude"), id, true); err == nil || len(e.openTodos()) != 1 {
		t.Errorf("an agent approved: %v", err)
	}
	// A call under a run may touch only to-dos of that run.
	run := "e2e-agenda00001"
	if text, bad := e.tool("update_agenda", map[string]any{"id": todo.ID, "change": "drop", "reason": "r", "run": run}); !bad || text != (&repo.RunError{}).Error() {
		t.Errorf("a run changing a real to-do: %q %v", text, bad)
	}
	if text, bad := e.tool("update_agenda", map[string]any{"id": todo.ID, "change": "drop", "reason": "r", "run": "bad"}); !bad || text != badRunMessage {
		t.Errorf("a bad run: %q", text)
	}
	m = e.toolJSON("post_agenda", map[string]any{"text": "Of the run", "reason": "r", "run": run})
	rid := int64(m["proposal_id"].(float64))
	if mark, _ := e.a.store.RowRun(e.ctx, "agenda_proposals", rid); mark != run {
		t.Errorf("the proposal's mark: %q", mark)
	}
	e.decide(rid, "approve")
	var made repo.Agenda
	for _, tdo := range e.openTodos() {
		if tdo.Title == "Of the run" {
			made = tdo
		}
	}
	if mark, _ := e.a.store.RowRun(e.ctx, "agenda", made.ID); made.ID == 0 || mark != run {
		t.Errorf("what Approve made carries the run's mark: %+v %q", made, mark)
	}
	// None of it wrote an event of a calendar.
	if e.a.store.CountEvents(e.ctx, "") != 0 {
		t.Error("a calendar entry appeared")
	}
}
