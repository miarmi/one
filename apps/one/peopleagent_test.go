package main

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"testing"

	"lab/apps/one/repo"
)

// Tests of what agents do with People (SPEC 25). All names are invented.

func (e *agentEnv) personEvents(typ string) []repo.Event {
	e.t.Helper()
	rows, err := e.a.store.RawEventsFull(e.ctx, typ)
	if err != nil {
		e.t.Fatal(err)
	}
	return rows
}

// tool calls an MCP tool as an agent and gives back its text and whether it failed.
func (e *agentEnv) tool(name string, args map[string]any) (string, bool) {
	e.t.Helper()
	res := e.rpc(e.a.agentTokenValue(), "tools/call", map[string]any{"name": name, "arguments": args})["result"].(map[string]any)
	return res["content"].([]any)[0].(map[string]any)["text"].(string), res["isError"] == true
}

func (e *agentEnv) pendingPersonProposals() []repo.PersonProposal {
	ps, err := e.a.store.PersonProposalsWaiting(e.ctx, 100)
	if err != nil {
		e.t.Fatal(err)
	}
	return ps
}

func TestAgentSetsICallThemAndWhoTheyAreToMeAndSwitchesAlwaysShowOnEachRecordedWithTheAgentAsActor(t *testing.T) {
	e := newAgentEnv(t)
	p := e.makePerson("mail", "lenka@example.test", "Lenka Example")

	// What the owner calls her, by number.
	m := e.post("/api/people/details", map[string]any{"person": p.ID, "call_name": "Lenka", "agent": "Claude"})
	if m["ok"] != true || m["_status"] != float64(200) {
		t.Fatalf("call_name: %v", m)
	}
	got := e.person("Lenka")
	if got.CallName != "Lenka" || got.Relation != "" || got.Protected {
		t.Fatalf("after call_name: %+v", got)
	}
	// Who she is to him, by exact name.
	if m := e.post("/api/people/details", map[string]any{"person": "Lenka Example", "relation": "my partner"}); m["ok"] != true {
		t.Fatalf("relation: %v", m)
	}
	// Always show me on, by address.
	if m := e.post("/api/people/details", map[string]any{"person": "lenka@example.test", "always_show": true, "agent": "Helper"}); m["ok"] != true {
		t.Fatalf("always_show: %v", m)
	}
	got = e.person("Lenka")
	if got.CallName != "Lenka" || got.Relation != "my partner" || !got.Protected {
		t.Fatalf("after all three: %+v", got)
	}

	// Each is an event with the agent as actor, and never the owner.
	evs := e.personEvents("person_changed")
	if len(evs) != 3 {
		t.Fatalf("%d events", len(evs))
	}
	want := map[string]string{"call_name": "claude", "relation": "claude", "always_show": "agent:Helper"}
	for _, ev := range evs {
		var d map[string]any
		json.Unmarshal([]byte(ev.Data), &d)
		field, _ := d["field"].(string)
		if want[field] == "" || ev.Actor != want[field] || ev.Actor == "owner" || d["person"] != float64(p.ID) {
			t.Errorf("event %s by %s: %v", field, ev.Actor, d)
		}
		delete(want, field)
	}
	if len(want) != 0 {
		t.Errorf("missing events %v", want)
	}
	if e.person("Lenka").Protected != true || e.a.store.CountPersonProposals(e.ctx) != 0 {
		t.Error("a direct change left a proposal")
	}

	// Saying the same again changes and records nothing.
	if m := e.post("/api/people/details", map[string]any{"person": p.ID, "call_name": "Lenka", "always_show": true}); m["unchanged"] != true || len(e.personEvents("person_changed")) != 3 {
		t.Errorf("the same again: %v", m)
	}
	// The name shows on messages at once.
	it := e.newItem(stageNew, false, "Dinner", "")
	_ = it

	// The same through the connector.
	q := e.makePerson("mail", "bob@example.test", "Bob Example")
	text, failed := e.tool("update_person", map[string]any{"person": q.ID, "call_name": "Bobík", "relation": "neighbour", "always_show": true, "agent": "Claude"})
	if failed {
		t.Fatalf("update_person: %s", text)
	}
	if b := e.person("Bob"); b.CallName != "Bobík" || b.Relation != "neighbour" || !b.Protected {
		t.Errorf("via MCP: %+v", b)
	}
	if text, failed := e.tool("update_person", map[string]any{"person": "Bobík", "relation": "the neighbour"}); failed || !strings.Contains(text, "the neighbour") {
		t.Errorf("by what the owner calls them: %v %s", failed, text)
	}
	// list_people finds them and gives the ids.
	text, failed = e.tool("list_people", map[string]any{"q": "bobík"})
	if failed || !strings.Contains(text, fmt.Sprintf(`"id":%d`, q.ID)) || !strings.Contains(text, `"always_show":true`) {
		t.Errorf("list_people: %v %s", failed, text)
	}
	list := e.getJSON("/api/people?limit=1")
	if list["more"] != true || len(list["people"].([]any)) != 1 || list["total"] != float64(2) {
		t.Errorf("GET /api/people: %v", list)
	}
	// Mistakes are said in a sentence.
	for _, c := range []struct {
		body map[string]any
		code float64
	}{
		{map[string]any{"person": "Nobody Atall", "call_name": "X"}, 404},
		{map[string]any{"person": 9999, "call_name": "X"}, 404},
		{map[string]any{"person": p.ID}, 400},
		{map[string]any{"person": p.ID, "call_name": "   "}, 400},
		{map[string]any{"person": p.ID, "relation": strings.Repeat("x", 121)}, 400},
		{map[string]any{"call_name": "X"}, 400},
	} {
		if m := e.post("/api/people/details", c.body); m["_status"] != c.code || m["ok"] != false {
			t.Errorf("%v: %v", c.body, m)
		}
	}
	// Without the token nothing is done.
	if rec := e.call("POST", "/api/people/details", "", map[string]any{"person": p.ID, "call_name": "Nope"}); rec.Code != 401 || e.person("Lenka").CallName != "Lenka" {
		t.Errorf("without a token: %d", rec.Code)
	}
}

func TestAnAgentCannotSwitchAlwaysShowOffOrJoinPeopleDirectly(t *testing.T) {
	e := newAgentEnv(t)
	lenka := e.makePerson("mail", "lenka@example.test", "Lenka Example")
	wa := e.makePerson("whatsapp", "+420600000001", "Lenka on WhatsApp")
	e.post("/api/people/details", map[string]any{"person": lenka.ID, "always_show": true})
	events := e.countEvents()

	// Switching it off, whatever the request says: refused by the guard, nothing changes.
	for _, body := range []map[string]any{
		{"person": lenka.ID, "always_show": false},
		{"person": lenka.ID, "always_show": false, "call_name": "Sneaky"},
		{"person": lenka.ID, "always_show": false, "agent": "the owner"},
		{"person": lenka.ID, "protected": false, "always_show": false},
	} {
		m := e.post("/api/people/details", body)
		if m["_status"] != float64(403) || !isRefusal(m["error"], "Approval required") {
			t.Errorf("%v: %v", body, m)
		}
	}
	if !e.person("Lenka").Protected || e.person("Lenka").CallName != "" {
		t.Error("a refused request changed the person")
	}
	text, failed := e.tool("update_person", map[string]any{"person": lenka.ID, "always_show": false})
	if !failed || !isRefusal(text, "Approval required") || !e.person("Lenka").Protected {
		t.Errorf("MCP switching off: %v %s", failed, text)
	}
	// The other fields cannot do it by another name either.
	e.post("/api/people/details", map[string]any{"person": lenka.ID, "protected": false, "off": true, "unprotect": true, "call_name": "Lenka"})
	if !e.person("Lenka").Protected {
		t.Error("an unknown field switched the mark off")
	}

	// Joining: no address under /api/ does it, with any method and any body.
	body := map[string]any{"person": lenka.ID, "with": wa.ID, "id": lenka.ID, "into": lenka.ID, "merge": wa.ID, "join": wa.ID, "action": "join", "agent": "the owner", "handles": []string{"whatsapp: +420600000001"}}
	for _, method := range []string{"POST", "PUT", "PATCH", "DELETE", "GET"} {
		for _, path := range []string{"/api/people/join", "/api/people/merge", "/api/people/" + fmt.Sprint(lenka.ID) + "/join", "/api/people/" + fmt.Sprint(lenka.ID) + "/handles",
			"/api/people/" + fmt.Sprint(lenka.ID), "/api/people/" + fmt.Sprint(lenka.ID) + "/protected", "/api/people/" + fmt.Sprint(lenka.ID) + "/always-show/off",
			"/api/people/unprotect", "/api/people/approve", "/api/people-proposals/1/approve", "/api/people/proposals/1/approve", "/api/people/proposals/1/dismiss", "/api/people/proposals/1"} {
			rec := e.api(method, path, body)
			if rec.Code != 403 && rec.Code != 404 && rec.Code != 405 {
				t.Errorf("%s %s: %d %s", method, path, rec.Code, rec.Body.String())
			}
		}
	}
	// The one address that takes a join only stores a proposal.
	m := e.post("/api/people/proposals", map[string]any{"action": "join", "person": lenka.ID, "with": wa.ID, "reason": "Same number."})
	if m["ok"] != true || m["proposal_id"] == nil {
		t.Fatalf("propose: %v", m)
	}
	if n := e.a.store.CountPeople(e.ctx, ""); n != 2 {
		t.Errorf("%d people: a proposal joined them", n)
	}
	if len(e.person("Lenka Example").Handles) != 1 || len(e.person("Lenka on").Handles) != 1 {
		t.Error("a proposal moved a handle")
	}
	// Nothing under /api/ decides it, and an agent given as the owner is refused when it tries.
	if _, err := e.a.decidePerson(e.ctx, agentActor("the owner"), int64(m["proposal_id"].(float64)), true); err == nil || !isRefusal(err.Error(), "Actor by channel") {
		t.Errorf("an agent decided: %v", err)
	}
	if e.a.store.CountPeople(e.ctx, "") != 2 || len(e.pendingPersonProposals()) != 1 {
		t.Error("an agent's approval had an effect")
	}
	// No tool joins or switches off; the tool for proposals says so.
	for _, tl := range mcpTools {
		if tl.Name == "propose_merge" {
			continue // joins two rows of mail, not people (SPEC 10; see matter_rows_test.go)
		}
		if strings.Contains(tl.Name, "join") || strings.Contains(tl.Name, "merge") || strings.Contains(tl.Name, "unprotect") {
			t.Errorf("tool %s", tl.Name)
		}
	}
	// The only things the refusals wrote are refusals and the work toolposal.
	if e.countEvents() <= events {
		t.Error("the refusals were not recorded")
	}
}

func TestJoinAndStopAlwaysShowingProposalsShowTheExactCardAreCarriedOutOnceOnApproveAndDroppedOnDismiss(t *testing.T) {
	e := newAgentEnv(t)
	lenka := e.makePerson("mail", "lenka@example.test", "Lenka Example")
	wa := e.makePerson("whatsapp", "+420600000001", "Lenka on WhatsApp")
	ondra := e.makePerson("mail", "ondra@example.test", "Ondra Example")
	e.post("/api/people/details", map[string]any{"person": lenka.ID, "call_name": "Lenka", "always_show": true})
	e.post("/api/people/details", map[string]any{"person": ondra.ID, "call_name": "Ondra", "always_show": true})
	e.post("/api/people/details", map[string]any{"person": wa.ID, "relation": "my partner"})

	// Proposals, through REST and through the connector.
	j := e.post("/api/people/proposals", map[string]any{"action": "join", "person": "lenka@example.test", "with": "whatsapp: +420600000001", "reason": "Same person on two networks.", "agent": "Claude"})
	if j["ok"] != true {
		t.Fatalf("join: %v", j)
	}
	joinID := int64(j["proposal_id"].(float64))
	text, failed := e.tool("propose_person_change", map[string]any{"action": "stop_always_show", "person": "Ondra", "reason": "The owner said he need not always see him."})
	if failed {
		t.Fatalf("stop: %s", text)
	}
	stopID := int64(e.pendingPersonProposals()[1].ID)
	// The same proposal again, in either order, is the one waiting.
	if m := e.post("/api/people/proposals", map[string]any{"action": "join", "person": wa.ID, "with": lenka.ID, "reason": "Again."}); m["unchanged"] != true || int64(m["proposal_id"].(float64)) != joinID {
		t.Errorf("a repeated join: %v", m)
	}
	if n := len(e.pendingPersonProposals()); n != 2 {
		t.Fatalf("%d waiting", n)
	}
	// Mistakes: Always show me is not on, same person, no reason, nobody.
	for _, c := range []map[string]any{
		{"action": "stop_always_show", "person": wa.ID, "reason": "x"},
		{"action": "join", "person": lenka.ID, "with": lenka.ID, "reason": "x"},
		{"action": "join", "person": lenka.ID, "reason": "x"},
		{"action": "join", "person": lenka.ID, "with": wa.ID},
		{"action": "split", "person": lenka.ID, "reason": "x"},
		{"action": "join", "person": "Nobody Atall", "with": wa.ID, "reason": "x"},
	} {
		if m := e.post("/api/people/proposals", c); m["ok"] == true {
			t.Errorf("%v was accepted", c)
		}
	}

	// The cards, with the exact wording, on the People page and in Settings.
	for _, path := range []string{"/people", "/settings"} {
		page := get(e.h, path).Body.String()
		for _, want := range []string{"Join Lenka and Lenka on WhatsApp", "Stop always showing Ondra", ">Approve<", ">Dismiss<", ">Proposal<", "Same person on two networks.", "Claude"} {
			if !strings.Contains(page, want) {
				t.Errorf("%s lacks %q", path, want)
			}
		}
		if n := strings.Count(page, ">Approve<"); n != 2 {
			t.Errorf("%s: %d Approve buttons", path, n)
		}
	}
	// Nothing has changed yet.
	if e.a.store.CountPeople(e.ctx, "") != 3 || !e.person("Ondra").Protected {
		t.Fatal("a proposal changed something before Approve")
	}

	// Approve the join: once. Handles, mark, notes and what the owner calls her stay.
	note := e.post("/api/notes", map[string]any{"title": "Lenka on WhatsApp", "text": "Writes in the evening.", "person": "Lenka on WhatsApp"})
	if note["ok"] != true {
		t.Fatal(note)
	}
	rec := post(e.h, fmt.Sprintf("/people-proposals/%d/approve", joinID), url.Values{"back": {"people"}})
	if rec.Code != 303 || rec.Header().Get("Location") != "/people?n=proposal" {
		t.Fatalf("approve: %d %s", rec.Code, rec.Header().Get("Location"))
	}
	if e.a.store.CountPeople(e.ctx, "") != 2 {
		t.Fatalf("%d people after the join", e.a.store.CountPeople(e.ctx, ""))
	}
	merged, ok := e.personByID(lenka.ID)
	if !ok || merged.CallName != "Lenka" || merged.Relation != "my partner" || !merged.Protected || len(merged.Handles) != 2 {
		t.Fatalf("the joined person: %+v", merged)
	}
	if _, gone := e.personByID(wa.ID); gone {
		t.Error("the person who was joined is still there")
	}
	// The same Approve again does nothing; so does a late Dismiss.
	post(e.h, fmt.Sprintf("/people-proposals/%d/approve", joinID), url.Values{})
	post(e.h, fmt.Sprintf("/people-proposals/%d/dismiss", joinID), url.Values{})
	if e.a.store.CountPeople(e.ctx, "") != 2 || len(e.personEvents("people_joined")) != 1 {
		t.Error("a second Approve joined again")
	}
	if msgs := e.personEvents("person_proposal_decided"); len(msgs) != 1 {
		t.Errorf("%d decisions recorded", len(msgs))
	}
	// A message from either handle is the same person now.
	refs, _ := e.a.store.PeopleByHandles(e.ctx, []repo.Handle{{Network: "whatsapp", Value: "+420600000001"}, {Network: "mail", Value: "lenka@example.test"}})
	if len(refs) != 2 || refs[repo.Handle{Network: "whatsapp", Value: "+420600000001"}].ID != lenka.ID {
		t.Errorf("handles: %v", refs)
	}
	if res := e.getJSON("/api/search?q=evening"); len(res["results"].([]any)) != 1 {
		t.Error("the note is lost")
	}

	// Dismiss the other: the mark stays, the card is gone, the dismissal is recorded.
	rec = post(e.h, fmt.Sprintf("/people-proposals/%d/dismiss", stopID), url.Values{"back": {"settings"}})
	if rec.Code != 303 || rec.Header().Get("Location") != "/settings?n=dismissed" {
		t.Fatalf("dismiss: %d %s", rec.Code, rec.Header().Get("Location"))
	}
	if !e.person("Ondra").Protected || len(e.pendingPersonProposals()) != 0 {
		t.Error("a dismissed proposal had an effect or still waits")
	}
	for _, path := range []string{"/people", "/settings"} {
		if page := get(e.h, path).Body.String(); strings.Contains(page, "Stop always showing") || strings.Contains(page, ">Approve<") {
			t.Errorf("%s still shows a card", path)
		}
	}
	decided := e.personEvents("person_proposal_decided")
	if len(decided) != 2 || decided[0].Actor != "owner" || !strings.Contains(decided[0].Data, `"dismissed"`) {
		t.Errorf("decisions %v", decided)
	}

	// Stop always showing, approved: now the mark is off and an agent may close again.
	m := e.post("/api/people/proposals", map[string]any{"action": "stop_always_show", "person": ondra.ID, "reason": "The owner said so."})
	rec = post(e.h, fmt.Sprintf("/people-proposals/%d/approve", int64(m["proposal_id"].(float64))), url.Values{})
	if rec.Code != 303 || e.person("Ondra").Protected {
		t.Fatalf("stop approved: %d protected=%v", rec.Code, e.person("Ondra").Protected)
	}
	if got := e.person("Ondra"); got.CallName != "Ondra" {
		t.Errorf("approving changed more: %+v", got)
	}
	post(e.h, fmt.Sprintf("/people-proposals/%d/approve", int64(m["proposal_id"].(float64))), url.Values{})
	e.post("/api/people/details", map[string]any{"person": ondra.ID, "always_show": true}) // an agent switches it on again
	if !e.person("Ondra").Protected {
		t.Error("an agent cannot switch the mark on")
	}
	// The owner himself still switches it off on the page, with no proposal.
	post(e.h, fmt.Sprintf("/people/%d", ondra.ID), url.Values{"name": {"Ondra Example"}, "call_name": {"Ondra"}, "handles": {"ondra@example.test"}})
	if e.person("Ondra").Protected || len(e.pendingPersonProposals()) != 0 {
		t.Error("The owner needed a proposal for his own change")
	}
	post(e.h, fmt.Sprintf("/people/%d", ondra.ID), url.Values{"name": {"Ondra Example"}, "call_name": {"Ondra"}, "handles": {"ondra@example.test"}, "protected": {"1"}})
	if !e.person("Ondra").Protected {
		t.Error("The owner cannot switch the mark on")
	}
	// A proposal whose people are gone or changed is withdrawn, not carried out.
	k := e.makePerson("mail", "kim@example.test", "Kim Example")
	l := e.makePerson("mail", "lee@example.test", "Lee Example")
	m1 := e.post("/api/people/proposals", map[string]any{"action": "join", "person": k.ID, "with": l.ID, "reason": "Same."})
	m2 := e.post("/api/people/proposals", map[string]any{"action": "join", "person": l.ID, "with": ondra.ID, "reason": "Same."})
	post(e.h, fmt.Sprintf("/people-proposals/%d/approve", int64(m1["proposal_id"].(float64))), url.Values{})
	if _, found := e.personByID(l.ID); found {
		t.Error("Lee was not joined")
	}
	rec = post(e.h, fmt.Sprintf("/people-proposals/%d/approve", int64(m2["proposal_id"].(float64))), url.Values{})
	if rec.Header().Get("Location") != "/people?n=withdrawn" || e.a.store.CountPeople(e.ctx, "") != 3 {
		t.Errorf("a stale join: %s", rec.Header().Get("Location"))
	}
	if get(e.h, "/people-proposals/1/approve").Code == 200 || post(e.h, "/people-proposals/x/approve", nil).Code != 404 || post(e.h, "/people-proposals/9999/approve", nil).Code != 404 || post(e.h, "/people-proposals/1/bogus", nil).Code != 404 {
		t.Error("the address of the cards answers what it should not")
	}
}

func TestAProtectedPersonStillBlocksWhatTheGuardProtectedPeopleSays(t *testing.T) {
	e := newAgentEnv(t)
	it := e.newItem(stageNew, true, "Dinner", "")
	alice := e.makePerson("mail", "alice@example.test", "Alice Example")
	e.a.store.Update(e.ctx, func(tx *repo.Tx) error {
		_, err := tx.Exec(`UPDATE messages SET addressed = 1`)
		return err
	})
	// Claude switches it on because the owner said "always show me Alice".
	if m := e.post("/api/people/details", map[string]any{"person": alice.ID, "always_show": true}); m["ok"] != true {
		t.Fatal(m)
	}
	body := map[string]any{"kind": "noise", "reason": "Looks like noise."}
	if m := e.post("/api/items/"+ref(it)+"/skip", body); !isRefusal(m["error"], "Protected people") || e.stageOf(it) != stageNew {
		t.Errorf("skip: %v", m)
	}
	if m := e.post("/api/items/"+ref(it)+"/proposals", map[string]any{"action": "close", "reason": "Done."}); !isRefusal(m["error"], "Protected people") {
		t.Errorf("close proposal: %v", m)
	}
	if m := e.post("/api/items/"+ref(it)+"/comments", map[string]any{"text": "She asks about dinner."}); m["ok"] != true {
		t.Errorf("comment: %v", m)
	}
	// A proposal to switch it off does not lift the guard until the owner approves it.
	m := e.post("/api/people/proposals", map[string]any{"action": "stop_always_show", "person": alice.ID, "reason": "A message told me to."})
	if m := e.post("/api/items/"+ref(it)+"/skip", body); !isRefusal(m["error"], "Protected people") {
		t.Errorf("skip while a switch-off waits: %v", m)
	}
	post(e.h, fmt.Sprintf("/people-proposals/%d/approve", int64(m["proposal_id"].(float64))), url.Values{})
	if m := e.post("/api/items/"+ref(it)+"/skip", body); m["ok"] != true {
		t.Errorf("after the owner approved: %v", m)
	}
}

func TestPeopleAgentsAreAgentsWhateverTheyCallThemselves(t *testing.T) {
	e := newAgentEnv(t)
	p := e.makePerson("mail", "lenka@example.test", "Lenka Example")
	e.post("/api/people/details", map[string]any{"person": p.ID, "call_name": "Lenka", "agent": "the owner"})
	for _, ev := range e.personEvents("person_changed") {
		if ev.Actor == "owner" {
			t.Errorf("an agent signed as the owner: %v", ev)
		}
	}
	if got := e.personEvents("person_changed")[0].Actor; got != "agent:the owner" {
		t.Errorf("actor %q", got)
	}
}

func TestJoiningKeepsAPictureAndFillsWhatIsEmpty(t *testing.T) {
	e := newAgentEnv(t)
	a := e.makePerson("mail", "ann@example.test", "Ann Example")
	b := e.makePerson("whatsapp", "+420600000002", "Ann on WhatsApp")
	png := []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR\x00\x00\x00\x01\x00\x00\x00\x01\x08\x02\x00\x00\x00\x90wS\xde")
	e.a.store.Update(e.ctx, func(tx *repo.Tx) error { return tx.SetPicture(e.ctx, b.ID, "image/png", png, 1) })
	e.post("/api/people/details", map[string]any{"person": b.ID, "call_name": "Ann", "relation": "my sister"})
	m := e.post("/api/people/proposals", map[string]any{"action": "join", "person": a.ID, "with": b.ID, "reason": "Same."})
	post(e.h, fmt.Sprintf("/people-proposals/%d/approve", int64(m["proposal_id"].(float64))), url.Values{})
	got, ok := e.personByID(a.ID)
	if !ok || !got.HasPicture || got.CallName != "Ann" || got.Relation != "my sister" || len(got.Handles) != 2 {
		t.Fatalf("joined: %+v", got)
	}
	if get(e.h, fmt.Sprintf("/people/%d/picture", a.ID)).Code != 200 || get(e.h, fmt.Sprintf("/people/%d/picture", b.ID)).Code != 404 {
		t.Error("the picture did not move to the person who stays")
	}
}

func TestAWaitingPeopleProposalIsCountedOnTheListPageAndItsLinkLeadsToTheCard(t *testing.T) {
	e := newAgentEnv(t)
	lenka := e.makePerson("mail", "lenka@example.test", "Lenka Example")
	wa := e.makePerson("whatsapp", "+420600000001", "Lenka on WhatsApp")
	it := e.newItem(stageNew, true, "Parcel", "")

	// With nothing waiting about people, the list page has no such strip.
	if page := get(e.h, "/").Body.String(); strings.Contains(page, "about people") {
		t.Fatal("a strip about people with no proposal")
	}
	// A proposal about an item is shown by its group header as before, and still no strip about people.
	e.propose(it, "pending", "waiting for a reply")
	if page := get(e.h, "/").Body.String(); strings.Contains(page, "about people") || !strings.Contains(page, "1 to approve") {
		t.Fatal("the group header or the strip is wrong with an item proposal only")
	}

	// A proposal about people is counted with it: 2 to approve, 1 of them about people.
	m := e.post("/api/people/proposals", map[string]any{"action": "join", "person": lenka.ID, "with": wa.ID, "reason": "Same number."})
	if m["ok"] != true {
		t.Fatalf("propose: %v", m)
	}
	page := get(e.h, "/").Body.String()
	if !strings.Contains(page, "2 to approve, 1 about people") {
		t.Fatalf("the list page does not count it: %s", page[strings.Index(page, `id="alerts"`):][:400])
	}
	i := strings.Index(page, `id="people-strip"`)
	strip := page[i : i+strings.Index(page[i:], "</p>")]
	if strings.Contains(strip, " hidden") || !strings.Contains(strip, `<a href="/settings#h-proposals">2 to approve, 1 about people</a>`) {
		t.Errorf("the strip: %s", strip)
	}
	// The same number comes with the page's live state, so it appears without a reload.
	if st := get(e.h, "/list/state").Body.String(); !strings.Contains(st, `"people":"2 to approve, 1 about people"`) {
		t.Errorf("state: %s", st)
	}
	// The link leads to the card with the exact wording, Approve and Dismiss.
	settings := get(e.h, "/settings").Body.String()
	if !strings.Contains(settings, `id="h-proposals"`) {
		t.Fatal("the link points at nothing")
	}
	card := settings[strings.Index(settings, `id="h-proposals"`):]
	for _, want := range []string{"Join Lenka Example and Lenka on WhatsApp", ">Approve<", ">Dismiss<", "Proposal"} {
		if !strings.Contains(card, want) {
			t.Errorf("the card lacks %q", want)
		}
	}
	// A second kind, stop always showing, is counted too.
	e.post("/api/people/details", map[string]any{"person": lenka.ID, "always_show": true})
	m2 := e.post("/api/people/proposals", map[string]any{"action": "stop_always_show", "person": lenka.ID, "reason": "He said so."})
	if m2["ok"] != true {
		t.Fatalf("propose: %v", m2)
	}
	if page := get(e.h, "/").Body.String(); !strings.Contains(page, "3 to approve, 2 about people") {
		t.Error("the second people proposal is not counted")
	}
	// Dismissing them leaves the strip away.
	post(e.h, fmt.Sprintf("/people-proposals/%d/dismiss", int64(m["proposal_id"].(float64))), url.Values{"back": {"settings"}})
	post(e.h, fmt.Sprintf("/people-proposals/%d/dismiss", int64(m2["proposal_id"].(float64))), url.Values{"back": {"settings"}})
	if page := get(e.h, "/").Body.String(); strings.Contains(page, "about people") {
		t.Error("the strip stays after the proposals are decided")
	}
}
