package main

import (
	"fmt"
	"strings"
	"testing"
)

// isRefusal says whether v is the refusal of the guard: its name, then one sentence.
func isRefusal(v any, guard string) bool {
	return strings.HasPrefix(fmt.Sprint(v), "Refused by guard: "+guard+". ")
}

// SPEC 22: every refusal names its guard and says why in one plain sentence.
func TestEveryRefusalSaysWhyInOneSentence(t *testing.T) {
	for _, why := range []string{whyReasonRequired, whyProtectedClose, whyProtectedRule, whyApprovalOnly, whyBlockedSkip,
		whyBlockedReply, whySameOwn, whySameNoID, whySameNoRef, whySameNoOne, whySameRecipients, whySendOnceAnswered,
		whySendOnceSending, whyActorAgent, whyActorOwner, whyOnlyOwner, whyApprovalRequired} {
		if !strings.HasSuffix(why, ".") || strings.Contains(strings.TrimSuffix(why, "."), ". ") || why[0] < 'A' || why[0] > 'Z' {
			t.Errorf("not one plain sentence: %q", why)
		}
		if strings.ContainsAny(why, "@<>{}") {
			t.Errorf("a sentence must not hold an address or a placeholder: %q", why)
		}
	}
	err := refuse("Reason required", whyReasonRequired)
	if err.Error() != "Refused by guard: Reason required. "+whyReasonRequired {
		t.Errorf("the words: %q", err.Error())
	}
}

func TestARefusalGoesToTheAgentAndTheRecordKeepsTheReason(t *testing.T) {
	e := newAgentEnv(t)
	it := e.newItem(stageNew, true, "Weekly deals", "")
	before := e.a.store.RefusedSince(e.ctx, 0)["Reason required"]
	m := e.post("/api/items/"+ref(it)+"/skip", map[string]any{"kind": "noise", "reason": ""})
	if !isRefusal(m["error"], "Reason required") || !strings.HasSuffix(fmt.Sprint(m["error"]), whyReasonRequired) {
		t.Fatalf("the agent's error: %v", m)
	}
	if got := e.a.store.RefusedSince(e.ctx, 0)["Reason required"]; got != before+1 {
		t.Errorf("Refused recently counts one more: %d -> %d", before, got)
	}
	evs, err := e.a.store.RawEvents(e.ctx, "refused")
	if err != nil || len(evs) == 0 {
		t.Fatalf("the record: %v %v", evs, err)
	}
	if last := evs[len(evs)-1]; !strings.Contains(last, whyReasonRequired) || !strings.Contains(last, "Reason required") {
		t.Errorf("the record keeps the guard and the reason: %s", last)
	}
	// the same words through the connector
	text, isErr := e.tool("resolve_item", map[string]any{"id": ref(it)})
	if !isErr || !strings.Contains(text, "Refused by guard: Reason required. "+whyReasonRequired) {
		t.Errorf("the tool error: %s", text)
	}
}
