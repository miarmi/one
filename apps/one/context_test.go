package main

import (
	"fmt"
	"strings"
	"testing"

	"lab/apps/one/repo"
)

func part(m map[string]any, path ...string) map[string]any {
	for _, k := range path {
		m = m[k].(map[string]any)
	}
	return m
}

func ctxIDs(p map[string]any) []string {
	var out []string
	for _, x := range p["items"].([]any) {
		out = append(out, x.(map[string]any)["id"].(string))
	}
	return out
}

// SPEC 29: get_context gives the whole background of an item in one call.
func TestGetContextGivesTheBackgroundAndSaysPlainlyWhatIsEmpty(t *testing.T) {
	e := newAgentEnv(t)
	cur := e.seedFrom(stageNew, "Parcel 7", "shop@example.test", "Your parcel 7 is coming.")
	old := e.seedFrom(stageClosed, "Parcel 6", "shop@example.test", "Your parcel 6 came.")
	e.seedFrom(stageNew, "Other", "friend@example.test", "Hello.")
	e.addEvent(old, "agent:Claude", "skipped", map[string]any{"kind": "noise", "reason": "Parcel notice."})
	e.addEvent(old, "owner", "proposal_decided", map[string]any{"decision": "dismissed", "text": strings.Repeat("long ", 200)})
	if text, bad := e.tool("save_note", map[string]any{"title": "Shop", "text": "Parcels from the shop are expected.", "sender": "shop@example.test"}); bad {
		t.Fatal(text)
	}
	if text, bad := e.tool("record_feedback", map[string]any{"id": ref(old), "text": "Skipping these was right."}); bad {
		t.Fatal(text)
	}
	m := e.toolJSON("get_context", map[string]any{"id": ref(cur)})
	if got := ctxIDs(part(m, "related_rows", "same_people")); len(got) != 1 || got[0] != ref(old) {
		t.Errorf("same people: %v", got)
	}
	if empty := part(m, "related_rows", "joined_and_split")["empty"]; empty == nil || !strings.Contains(empty.(string), "No row was joined") {
		t.Errorf("an empty part says so: %v", part(m, "related_rows", "joined_and_split"))
	}
	if part(m, "related_rows", "same_matter")["empty"] == nil {
		t.Error("same_matter says it is empty")
	}
	dec := part(m, "decided_before", "decisions")["items"].([]any)
	if len(dec) != 2 {
		t.Fatalf("decisions: %v", dec)
	}
	first := dec[0].(map[string]any)
	if first["item"] != ref(old) || first["id"] == nil || first["type"] != "proposal_decided" {
		t.Errorf("a decision carries its ids: %v", first)
	}
	if s := fmt.Sprint(first["data"]); len(s) > 600 {
		t.Errorf("a long text is cut: %d", len(s))
	}
	if sd := part(m, "decided_before", "senders")["items"].([]any); len(sd) != 1 || sd[0].(map[string]any)["sender"] != "shop@example.test" {
		t.Errorf("the numbers per sender: %v", sd)
	}
	notes := part(m, "notes_that_apply")["items"].([]any)
	if len(notes) != 1 || notes[0].(map[string]any)["note_id"] == nil {
		t.Errorf("notes: %v", notes)
	}
	fb := part(m, "feedback_that_applies")["items"].([]any)
	if len(fb) != 1 {
		t.Fatalf("feedback: %v", fb)
	}
	f := fb[0].(map[string]any)
	if f["text"] != "Skipping these was right." || f["item"] != ref(old) || f["feedback_id"] == nil || !strings.Contains(f["applies_because"].(string), "shop@example.test") {
		t.Errorf("feedback applies to the same sender: %v", f)
	}
	// an item with nothing around it says so in every part
	lone := e.seedFrom(stageNew, "Lone", "nobody@example.test", "Alone.")
	m = e.toolJSON("get_context", map[string]any{"id": ref(lone)})
	for _, path := range [][]string{{"related_rows", "same_people"}, {"decided_before", "decisions"}, {"notes_that_apply"}, {"feedback_that_applies"}} {
		if p := part(m, path...); p["empty"] == nil || len(p["items"].([]any)) != 0 {
			t.Errorf("%v should be empty and say so: %v", path, p)
		}
	}
	// read only
	for _, tl := range mcpTools {
		if tl.Name == "get_context" && tl.Annotations["readOnlyHint"] != true {
			t.Error("get_context reads only")
		}
	}
}

func TestGetContextFindsRowsOfTheSameMatterAndTheJoinedHistory(t *testing.T) {
	e := newAgentEnv(t)
	e.a.store.SetSettingBy(e.ctx, keyMatterPatterns, "Order\tOrder (\\d+)", "test", e.now)
	cur := e.seedFrom(stageNew, "Order 4711 shipped", "a@one.example.test", "Shipped.")
	same := e.seedFrom(stageResolved, "Re: Order 4711", "b@two.example.test", "Question.")
	e.seedFrom(stageNew, "Order 9999", "c@three.example.test", "Different.")
	e.addEvent(cur, "owner", "rows_joined", map[string]any{"number": "Q-0099"})
	m := e.toolJSON("get_context", map[string]any{"id": ref(cur)})
	if got := ctxIDs(part(m, "related_rows", "same_matter")); len(got) != 1 || got[0] != ref(same) {
		t.Errorf("same matter: %v", got)
	}
	jr := part(m, "related_rows", "joined_and_split")["items"].([]any)
	if len(jr) != 1 || jr[0].(map[string]any)["item"] != "Q-0099" {
		t.Errorf("joined history: %v", jr)
	}
}

// A test run's feedback and context stay inside the run.
func TestFeedbackAndContextHonourTheTestRunMark(t *testing.T) {
	e := newAgentEnv(t)
	real := e.seedFrom(stageNew, "Real", "same@example.test", "Real mail.")
	outer := e.ctx
	e.ctx = repo.WithRun(outer, runA)
	inRun := e.seedFrom(stageNew, "Test mail", "same@example.test", "Test mail.")
	inRun2 := e.seedFrom(stageNew, "Test mail 2", "same@example.test", "Test mail 2.")
	e.ctx = outer
	// feedback on a test item under its run
	if text, bad := e.tool("record_feedback", map[string]any{"id": ref(inRun), "text": "Test words.", "run": runA}); bad {
		t.Fatalf("under its run: %s", text)
	}
	// not under no run on the real item's neighbour, and not under another run
	if _, bad := e.tool("record_feedback", map[string]any{"id": ref(inRun), "text": "x", "run": runB}); !bad {
		t.Error("another run wrote feedback on this run's item")
	}
	if _, bad := e.tool("record_feedback", map[string]any{"id": ref(real), "text": "x", "run": runA}); !bad {
		t.Error("a run wrote feedback on a real item")
	}
	// the run's item sees its own feedback and its own rows only
	m := e.toolJSON("get_context", map[string]any{"id": ref(inRun2), "run": runA})
	if got := ctxIDs(part(m, "related_rows", "same_people")); len(got) != 1 || got[0] != ref(inRun) {
		t.Errorf("run rows: %v", got)
	}
	if fb := part(m, "feedback_that_applies")["items"].([]any); len(fb) != 1 {
		t.Errorf("run feedback: %v", fb)
	}
	// the real item sees none of it
	m = e.toolJSON("get_context", map[string]any{"id": ref(real)})
	if got := ctxIDs(part(m, "related_rows", "same_people")); len(got) != 0 {
		t.Errorf("a real item sees a test run's rows: %v", got)
	}
	if fb := part(m, "feedback_that_applies")["items"].([]any); len(fb) != 0 {
		t.Errorf("a real item sees a test run's feedback: %v", fb)
	}
	// reading a real item under a run is refused
	if _, bad := e.tool("get_context", map[string]any{"id": ref(real), "run": runA}); !bad {
		t.Error("a run read a real item's background")
	}
}
