package main

import (
	"fmt"
	"net/url"
	"strings"
	"testing"

	"lab/apps/one/repo"
)

// Patterns and rules are bounded: what a rule compares, how many rules and
// patterns there can be, how long a pattern is.
func TestRuleMatchingAndCountsAreBounded(t *testing.T) {
	long := strings.Repeat("a", 100000)
	if got := boundMatched(long); len(got) > maxMatchedBytes {
		t.Errorf("a long value is cut to %d bytes, not %d", maxMatchedBytes, len(got))
	}
	r := repo.Rule{Field: "subject", Op: "ends_with", Value: "invoice due", Enabled: true}
	if !ruleMatches(r, ruleFacts{Subject: long + " invoice due"}) {
		t.Error("ends_with still sees the end of a long subject")
	}
	r.Op, r.Value = "starts_with", "re: invoice"
	if !ruleMatches(r, ruleFacts{Subject: "Re: invoice " + long}) {
		t.Error("starts_with still sees the start of a long subject")
	}

	e := newAgentEnv(t)
	for i := 0; i < maxRules; i++ {
		if err := e.a.store.Update(e.ctx, func(tx *repo.Tx) error {
			_, err := tx.InsertRule(e.ctx, repo.Rule{Name: "r", Source: "any", Field: "subject", Op: "contains", Value: fmt.Sprintf("word%04d", i), Enabled: true, CreatedBy: "owner"}, e.now.Unix())
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	rec := post(e.h, "/settings/rules", url.Values{"source": {"any"}, "field": {"subject"}, "op": {"contains"}, "value": {"one more"}})
	if rec.Code == 303 || !strings.Contains(rec.Body.String(), "There are already 500 rules") {
		t.Errorf("rule number %d is refused in a plain sentence: %d", maxRules+1, rec.Code)
	}
	if text, bad := e.tool("propose_rule", map[string]any{"source": "any", "field": "sender_domain", "op": "equals", "value": "bounds.example.test", "reason": "Seen 5 times, never answered."}); !bad || !strings.Contains(text, "as many rules") {
		t.Errorf("an agent's rule over the bound is refused: %v %s", bad, text)
	}
}

func TestMatterPatternsAreBounded(t *testing.T) {
	if msg := checkMatterRegex(strings.Repeat("a", maxMatterRegex+1)); msg == "" {
		t.Error("a pattern over the length bound is refused")
	}
	var ps []matterPattern
	for i := 0; i < maxMatterPatterns+5; i++ {
		ps = append(ps, matterPattern{Name: "n", Re: fmt.Sprintf("NOPE%d-[0-9]+", i)})
	}
	ps = append(ps, matterPattern{Name: "late", Re: "LATE-[0-9]+"})
	if k := matterKey(ps, "LATE-1", ""); k != "" {
		t.Errorf("only %d patterns are run: %q", maxMatterPatterns, k)
	}
}
