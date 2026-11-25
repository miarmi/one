package main

import (
	"fmt"
	"strings"
	"testing"

	"lab/apps/one/repo"
)

func (e *agentEnv) seedFrom(stage, subject, sender, text string) repo.Item {
	e.t.Helper()
	m := e.mail(subject, 1)
	m.sender, m.name, m.text = sender, "Sender "+sender, text
	return e.seed(stage, m)
}

func (e *agentEnv) addEvent(it repo.Item, actor, typ string, data any) {
	e.t.Helper()
	if err := e.a.store.Update(e.ctx, func(tx *repo.Tx) error {
		return tx.InsertEvent(e.ctx, e.now.Unix(), it.ID, actor, typ, data)
	}); err != nil {
		e.t.Fatal(err)
	}
}

// SPEC 29: feedback is recorded in the owner's words, kept on the item, shown in
// its thread, given to later reviews, and changes nothing else.
func TestRecordFeedbackIsKeptShownGivenToLaterReviewsAndChangesNothingElse(t *testing.T) {
	e := newAgentEnv(t)
	first := e.seedFrom(stageNew, "Invoice A", "billing@example.test", "Invoice A.")
	later := e.seedFrom(stageNew, "Invoice B", "billing@example.test", "Invoice B.")
	dec := e.post("/api/items/"+ref(first)+"/skip", map[string]any{"kind": "noise", "reason": "Invoice notice."})
	_ = dec
	var decisionID int64
	for _, ev := range e.events(first.ID, "skipped") {
		decisionID = ev.ID
	}
	stageBefore := e.stageOf(first)
	rulesBefore := e.a.store.CountRows(e.ctx, "rules")
	text, bad := e.tool("record_feedback", map[string]any{"id": ref(first), "text": "That was wrong: I want to see invoices.", "about": decisionID, "agent": "Claude"})
	if bad {
		t.Fatalf("record_feedback: %s", text)
	}
	if evs := e.events(first.ID, "feedback"); len(evs) != 1 || evs[0].Actor != "claude" || !strings.Contains(evs[0].Data, "I want to see invoices") {
		t.Fatalf("the event: %+v", evs)
	}
	if e.stageOf(first) != stageBefore || e.a.store.CountRows(e.ctx, "rules") != rulesBefore {
		t.Error("feedback changed a stage or a rule")
	}
	// shown in its thread, on the page
	row := e.openRow(first.ID)
	if !strings.Contains(row, "That was wrong: I want to see invoices.") {
		t.Error("the thread does not show the feedback")
	}
	if !strings.Contains(row, "Feedback from the owner") || !strings.Contains(row, "mark-feedback") {
		t.Error("the thread does not say it is feedback from the owner")
	}
	if i := strings.Index(row, "mark-feedback"); i < 0 || strings.Contains(row[max(0, i-300):i], `entry agent`) {
		t.Error("feedback is drawn like an agent's comment")
	}
	if th, _ := e.toolJSON("get_item", map[string]any{"id": ref(first)})["thread"].([]any); !strings.Contains(fmt.Sprint(th), "feedback_from_owner:true") {
		t.Error("get_item does not mark the feedback entry")
	}
	// given to a later review of the same sender, by get_item (next to the notes) and in the review prompt
	m := e.toolJSON("get_item", map[string]any{"id": ref(later)})
	fb := m["feedback_that_applies"].([]any)
	if len(fb) != 1 || fb[0].(map[string]any)["text"] != "That was wrong: I want to see invoices." || m["feedback_rule"] == nil {
		t.Errorf("get_item feedback: %v", fb)
	}
	prompt, err := e.a.reviewInput(e.ctx, e.item(later.ID))
	if err != nil || !strings.Contains(prompt, "That was wrong: I want to see invoices.") {
		t.Errorf("the review prompt lacks the feedback: %v", err)
	}
	// not given to another sender
	other := e.seedFrom(stageNew, "Hi", "friend@example.test", "Hi.")
	m = e.toolJSON("get_item", map[string]any{"id": ref(other)})
	if len(m["feedback_that_applies"].([]any)) != 0 {
		t.Error("feedback reached another sender")
	}
	// refusals: no text, a decision that is not on this item, the wrong item
	for name, args := range map[string]map[string]any{
		"no text":       {"id": ref(first), "text": "  "},
		"foreign about": {"id": ref(later), "text": "x", "about": decisionID},
		"no such about": {"id": ref(first), "text": "x", "about": 999999},
		"too long":      {"id": ref(first), "text": strings.Repeat("a", feedbackMaxChars+1)},
	} {
		if text, bad := e.tool("record_feedback", args); !bad {
			t.Errorf("%s was accepted: %s", name, text)
		}
	}
	// the tool says to record only what the owner said
	for _, tl := range mcpTools {
		if tl.Name == "record_feedback" {
			if !strings.Contains(tl.Description, "Record only what the owner said, in his words; never the agent's own judgement.") {
				t.Error("the description lacks the rule")
			}
			if tl.Annotations["readOnlyHint"] != false {
				t.Error("record_feedback writes")
			}
		}
	}
	// the thread of the agent's reading carries it too
	if !strings.Contains(fmt.Sprint(e.toolJSON("get_item", map[string]any{"id": ref(first)})["thread"]), "feedback") {
		t.Error("get_item's thread lacks the feedback event")
	}
}

// A test run's feedback stays inside the run.
func TestFeedbackHonoursTheTestRunMark(t *testing.T) {
	e := newAgentEnv(t)
	real := e.seedFrom(stageNew, "Real", "same@example.test", "Real mail.")
	outer := e.ctx
	e.ctx = repo.WithRun(outer, runA)
	inRun := e.seedFrom(stageNew, "Test mail", "same@example.test", "Test mail.")
	inRun2 := e.seedFrom(stageNew, "Test mail 2", "same@example.test", "Test mail 2.")
	e.ctx = outer
	if text, bad := e.tool("record_feedback", map[string]any{"id": ref(inRun), "text": "Test words.", "run": runA}); bad {
		t.Fatalf("under its run: %s", text)
	}
	if _, bad := e.tool("record_feedback", map[string]any{"id": ref(inRun), "text": "x", "run": runB}); !bad {
		t.Error("another run wrote feedback on this run's item")
	}
	if _, bad := e.tool("record_feedback", map[string]any{"id": ref(real), "text": "x", "run": runA}); !bad {
		t.Error("a run wrote feedback on a real item")
	}
	// the run's other item gets it; the real item does not
	if fb := e.toolJSON("get_item", map[string]any{"id": ref(inRun2)})["feedback_that_applies"].([]any); len(fb) != 1 {
		t.Errorf("run feedback: %v", fb)
	}
	if fb := e.toolJSON("get_item", map[string]any{"id": ref(real)})["feedback_that_applies"].([]any); len(fb) != 0 {
		t.Errorf("a real item got a test run's feedback: %v", fb)
	}
}
