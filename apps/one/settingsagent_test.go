package main

import (
	"encoding/json"
	"strings"
	"testing"

	"lab/apps/one/repo"
)

func (e *agentEnv) toolJSON(name string, args map[string]any) map[string]any {
	e.t.Helper()
	text, bad := e.tool(name, args)
	if bad {
		e.t.Fatalf("%s: %s", name, text)
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(text), &m); err != nil {
		e.t.Fatalf("%s: not JSON: %v: %s", name, err, text)
	}
	return m
}

func entryByName(list any, name string) map[string]any {
	for _, x := range list.([]any) {
		if m := x.(map[string]any); m["name"] == name {
			return m
		}
	}
	return nil
}

func TestListSettingsShowsWhatIsSetUpAndNeverASecret(t *testing.T) {
	e := newAgentEnv(t)
	now := e.now.Unix()
	if err := e.a.store.Update(e.ctx, func(tx *repo.Tx) error {
		id, err := tx.InsertRule(e.ctx, repo.Rule{Name: "Bulk shop", Enabled: false, Source: "any", Field: "sender_domain", Op: "equals", Value: "shop.test", Status: "proposed", Reason: "214 mails, none answered", ProposedBy: "agent:Claude", ProposedAt: now}, now)
		if err != nil {
			return err
		}
		if _, err := tx.DecideRule(e.ctx, id, true, "owner", now+60); err != nil {
			return err
		}
		if _, err := tx.InsertRule(e.ctx, repo.Rule{Name: "Hand rule", Enabled: true, Source: "chat", Field: "network", Op: "equals", Value: "whatsapp", CreatedBy: "owner"}, now); err != nil {
			return err
		}
		if _, err := tx.InsertRule(e.ctx, repo.Rule{Name: "Pending rule", Source: "any", Field: "sender", Op: "equals", Value: "wait@other.test", Status: "proposed", Reason: "why", ProposedBy: "agent:Claude", ProposedAt: now}, now); err != nil {
			return err
		}
		sid, err := tx.InsertSkipRule(e.ctx, repo.SkipRule{Name: "Skip bulk", Kind: skipSender, Value: "news@shop.test", Reason: "skipped 6 times", ProposedBy: "agent:Claude", ProposedAt: now})
		if err != nil {
			return err
		}
		_, err = tx.DecideSkipRule(e.ctx, sid, true, "owner", now+120)
		if err != nil {
			return err
		}
		_, err = tx.InsertSkipRule(e.ctx, repo.SkipRule{Name: "Skip waiting", Kind: skipSender, Value: "x@elsewhere.test", Reason: "r", ProposedBy: "agent:Claude", ProposedAt: now})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	m := e.toolJSON("list_settings", map[string]any{})
	r := entryByName(m["import_rules"], "Bulk shop")
	if r == nil || r["proposed_by"] != "Claude" || r["approved_by"] != "you" || r["reason"] != "214 mails, none answered" || r["approved_at"] == unknownWord || r["enabled"] != true {
		t.Fatalf("approved import rule: %v", r)
	}
	if g := entryByName(m["import_rules"], "Hand rule"); g == nil || g["approved_at"] != unknownWord || g["proposed_by"] != "you" {
		t.Errorf("a rule the owner made says unknown where nothing is held: %v", g)
	}
	s := entryByName(m["skip_rules"], "Skip bulk")
	if s == nil || s["paused"] != false || s["closed"] != float64(0) || s["approved_by"] != "you" {
		t.Fatalf("skip rule: %v", s)
	}
	if entryByName(m["skip_rules"], "Skip waiting") != nil {
		t.Error("a waiting skip rule is listed as active")
	}
	if len(m["waiting"].([]any)) != 2 {
		t.Errorf("waiting: %v", m["waiting"])
	}
	guards := m["guards"].([]any)
	if len(guards) != len(guardsShown) {
		t.Fatalf("guards: %d", len(guards))
	}
	var blocked map[string]any
	for _, g := range guards {
		if g.(map[string]any)["key"] == "blocked_targets" {
			blocked = g.(map[string]any)
		}
	}
	if blocked == nil || !strings.Contains(strings.Join(anyStrings(blocked["blocked_targets"]), ","), "paypal.com") {
		t.Fatalf("blocked targets: %v", blocked)
	}
	if !strings.Contains(m["note"].(string), "data") {
		t.Error("the answer does not say its text is data")
	}
	// A kind narrows the answer.
	k := e.toolJSON("list_settings", map[string]any{"kind": "guards"})
	if _, there := k["import_rules"]; there || k["guards"] == nil {
		t.Errorf("kind guards: %v", k)
	}
	if text, bad := e.tool("list_settings", map[string]any{"kind": "tokens"}); !bad || !strings.Contains(text, "kind must be") {
		t.Errorf("an unknown kind: %q %v", text, bad)
	}
	// The sender narrows it to the rules that would apply.
	f := e.toolJSON("list_settings", map[string]any{"sender": "News@Shop.test"})
	if entryByName(f["import_rules"], "Bulk shop") == nil || entryByName(f["skip_rules"], "Skip bulk") == nil || entryByName(f["import_rules"], "Hand rule") != nil {
		t.Errorf("sender filter: %v", f)
	}
	if len(f["matter_patterns"].([]any)) != 0 {
		t.Error("matter patterns are listed for a sender")
	}
	o := e.toolJSON("list_settings", map[string]any{"sender": "paypal.com"})
	for _, g := range o["guards"].([]any) {
		if g.(map[string]any)["key"] == "blocked_targets" && g.(map[string]any)["sender_is_blocked"] != true {
			t.Errorf("paypal.com is not blocked: %v", g)
		}
	}
	for _, args := range []map[string]any{{}, {"sender": "shop.test"}} {
		text, _ := e.tool("list_settings", args)
		for _, secret := range []string{e.a.agentTokenValue(), e.a.bridgeTokenValue(), "secret", "password", "token"} {
			if secret != "" && strings.Contains(strings.ToLower(text), strings.ToLower(secret)) {
				t.Errorf("the answer holds %q", secret)
			}
		}
	}
}

func anyStrings(v any) []string {
	var out []string
	for _, x := range v.([]any) {
		out = append(out, x.(string))
	}
	return out
}

func TestListSettingsHonoursTheTestRunMark(t *testing.T) {
	e := newAgentEnv(t)
	now := e.now.Unix()
	run := "e2e-abcdefgh12"
	rctx := repo.WithRun(e.ctx, run)
	if err := e.a.store.Update(rctx, func(tx *repo.Tx) error {
		_, err := tx.InsertSkipRule(rctx, repo.SkipRule{Name: "Run skip", Kind: skipSender, Value: "run@shop.test", Reason: "r", ProposedBy: "agent:Claude", ProposedAt: now})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	real := e.toolJSON("list_settings", map[string]any{})
	if len(real["waiting"].([]any)) != 0 {
		t.Errorf("a real call sees a run's proposal: %v", real["waiting"])
	}
	m := e.toolJSON("list_settings", map[string]any{"run": run})
	if len(m["waiting"].([]any)) != 1 {
		t.Errorf("the run's call: %v", m["waiting"])
	}
	if text, bad := e.tool("list_settings", map[string]any{"run": "bad"}); !bad || text != badRunMessage {
		t.Errorf("a bad run name: %q %v", text, bad)
	}
}
