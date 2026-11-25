package main

import (
	"encoding/json"
	"fmt"
	"net/url"
	"slices"
	"sort"
	"strings"
	"testing"

	"lab/apps/one/repo"
)

// Tests of propose_setting (SPEC 22): a proposal changes nothing before the owner's
// Approve, is checked as the Settings page checks, and is applied by the code
// Save uses.

func (e *agentEnv) proposeSet(args map[string]any) (string, bool) {
	e.t.Helper()
	if _, ok := args["reason"]; !ok {
		args["reason"] = "Chats are long here, and an invented reason says so."
	}
	return e.tool("propose_setting", args)
}

func (e *agentEnv) proposeOK(args map[string]any) int64 {
	e.t.Helper()
	text, bad := e.proposeSet(args)
	if bad {
		e.t.Fatalf("propose_setting: %s", text)
	}
	ps, _ := e.a.store.WaitingSettingProposals(e.ctx)
	if len(ps) == 0 {
		e.t.Fatal("no proposal waits")
	}
	return ps[len(ps)-1].ID
}

func TestAProposedSettingChangesNothingUntilApproveAndApproveWritesWhatSaveWrites(t *testing.T) {
	e := newAgentEnv(t)
	id := e.proposeOK(map[string]any{"kind": "setting", "name": "Sync interval", "value": "10 minutes"})
	if e.a.syncSettings(e.ctx).Interval.Minutes() != 5 || len(e.eventsOfType("setting_changed")) != 0 {
		t.Fatal("the proposal changed the setting")
	}
	page := get(e.h, "/settings").Body.String()
	for _, want := range []string{"Proposals from Claude", "Sync interval", "5 minutes", "10 minutes", "an invented reason says so", "Approve", "Dismiss"} {
		if !strings.Contains(page, want) {
			t.Errorf("the Settings page lacks %q", want)
		}
	}
	if !strings.Contains(get(e.h, "/list/state").Body.String(), "1 about settings") {
		t.Error("the strip does not count the proposal")
	}
	rec := post(e.h, fmt.Sprintf("/settings/setting-proposals/%d/approve", id), url.Values{})
	if rec.Code != 303 || rec.Header().Get("Location") != "/settings?n=proposal#h-proposals" {
		t.Fatalf("approve: %d %s", rec.Code, rec.Header().Get("Location"))
	}
	if e.a.syncSettings(e.ctx).Interval.Minutes() != 10 {
		t.Error("Approve did not change the setting")
	}
	viaApprove := e.eventsOfType("setting_changed")
	// the record is the one Save writes
	e.saveSettings(url.Values{keyInterval: {"15"}})
	viaSave := e.eventsOfType("setting_changed")
	if len(viaApprove) != 1 || len(viaSave) != 2 {
		t.Fatalf("records: %v / %v", viaApprove, viaSave)
	}
	shape := func(s string) string {
		var m map[string]any
		json.Unmarshal([]byte(s), &m)
		var keys []string
		for k := range m {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		return strings.Join(keys, ",") + "|" + fmt.Sprint(m["setting"], m["label"])
	}
	if shape(viaApprove[0]) != shape(viaSave[0]) {
		t.Errorf("Approve wrote another record than Save: %s vs %s", viaApprove[0], viaSave[0])
	}
	if p, _ := e.a.store.SettingProposalByID(e.ctx, id); p.Status != "approved" || p.DecidedBy != "owner" {
		t.Errorf("the proposal: %+v", p)
	}
	// a second tap writes nothing
	post(e.h, fmt.Sprintf("/settings/setting-proposals/%d/approve", id), url.Values{})
	if len(e.eventsOfType("setting_changed")) != 2 {
		t.Error("a second Approve wrote again")
	}
}

func TestProposeSettingValidatesAsTheSettingsPageDoesAndNeverClamps(t *testing.T) {
	e := newAgentEnv(t)
	for name, args := range map[string]map[string]any{
		"a value not on the list":       {"kind": "setting", "name": "Sync interval", "value": "3 minutes"},
		"a review window of 31 days":    {"kind": "setting", "name": "Review window", "value": "31"},
		"a bad model name":              {"kind": "setting", "name": "Review model", "value": "not a model!"},
		"a limit beyond the range":      {"kind": "setting", "name": "Daily limit", "value": "5000"},
		"a negative limit":              {"kind": "setting", "name": "Night check limit", "value": "-1"},
		"a time that is not a time":     {"kind": "setting", "name": "Night check at", "value": "25:00"},
		"skips before a rule, zero":     {"kind": "setting", "name": "Skips before a sender rule", "value": "0"},
		"the summary, another word":     {"kind": "setting", "name": "Summary of what Claude closed", "value": "weekly"},
		"a setting that does not exist": {"kind": "setting", "name": "Colour of the sky", "value": "blue"},
		"the value it already has":      {"kind": "setting", "name": "Sync interval", "value": "5 minutes"},
	} {
		if text, bad := e.proposeSet(args); !bad {
			t.Errorf("%s was accepted: %s", name, text)
		}
	}
	if len(mustWaiting(e)) != 0 {
		t.Error("a refused value left a proposal")
	}
	// valid values of the model's settings are taken as given, not rounded
	e.proposeOK(map[string]any{"kind": "setting", "name": "Daily limit", "value": "2.5"})
	e.proposeOK(map[string]any{"kind": "setting", "name": "Night check at", "value": "04:30"})
	e.proposeOK(map[string]any{"kind": "setting", "name": "Summary of what Claude closed", "value": "off"})
	ps := mustWaiting(e)
	if len(ps) != 3 || ps[0].To != "2.5" || ps[1].To != "04:30" || ps[2].To != "off" {
		t.Errorf("proposals: %+v", ps)
	}
	post(e.h, fmt.Sprintf("/settings/setting-proposals/%d/approve", ps[0].ID), url.Values{})
	post(e.h, fmt.Sprintf("/settings/setting-proposals/%d/approve", ps[2].ID), url.Values{})
	if e.a.dailyLimit(e.ctx) != 2.5 || e.a.summaryOn(e.ctx) {
		t.Error("Approve did not apply the model's setting or the summary setting")
	}
}

func mustWaiting(e *agentEnv) []repo.SettingProposal {
	ps, _ := e.a.store.WaitingSettingProposals(e.ctx)
	return ps
}

func TestProposeSettingForGuardsAndBlockedTargets(t *testing.T) {
	e := newAgentEnv(t)
	for _, g := range []string{"Approval only", "Exact text", "Same thread", "Send once", "Approval required", "Actor by channel"} {
		text, bad := e.proposeSet(map[string]any{"kind": "guard", "name": g, "value": "off"})
		if !bad || !strings.Contains(text, "always on") {
			t.Errorf("%s: %v %s", g, bad, text)
		}
	}
	if _, bad := e.proposeSet(map[string]any{"kind": "guard", "name": "Reason required", "value": "on"}); !bad {
		t.Error("a guard that is already on was proposed on")
	}
	id := e.proposeOK(map[string]any{"kind": "guard", "name": "Reason required", "value": "off"})
	if !e.a.guardOn(e.ctx, e.a.store.Q, "reason_required") {
		t.Fatal("the proposal switched the guard")
	}
	page := get(e.h, "/settings").Body.String()
	if !strings.Contains(page, "Reason required") || !strings.Contains(page, "<b>Off</b>") {
		t.Error("the page does not show the guard change")
	}
	post(e.h, fmt.Sprintf("/settings/setting-proposals/%d/approve", id), url.Values{})
	if e.a.guardOn(e.ctx, e.a.store.Q, "reason_required") {
		t.Error("Approve did not switch the guard")
	}
	// blocked targets
	for name, args := range map[string]map[string]any{
		"not an address":         {"kind": "blocked_target", "name": "not an address", "value": "add"},
		"neither add nor remove": {"kind": "blocked_target", "name": "bank.example", "value": "maybe"},
		"remove what is absent":  {"kind": "blocked_target", "name": "absent.example", "value": "remove"},
	} {
		if _, bad := e.proposeSet(args); !bad {
			t.Errorf("accepted: %s", name)
		}
	}
	id = e.proposeOK(map[string]any{"kind": "blocked_target", "name": "Pay.Example", "value": "add"})
	if slices.Contains(e.a.blockedTargets(e.ctx, e.a.store.Q), "pay.example") {
		t.Fatal("the proposal changed the list")
	}
	post(e.h, fmt.Sprintf("/settings/setting-proposals/%d/approve", id), url.Values{})
	if !slices.Contains(e.a.blockedTargets(e.ctx, e.a.store.Q), "pay.example") {
		t.Error("Approve did not add the entry")
	}
	if _, bad := e.proposeSet(map[string]any{"kind": "blocked_target", "name": "pay.example", "value": "add"}); !bad {
		t.Error("an entry already on the list was proposed")
	}
	id = e.proposeOK(map[string]any{"kind": "blocked_target", "name": "pay.example", "value": "remove"})
	post(e.h, fmt.Sprintf("/settings/setting-proposals/%d/approve", id), url.Values{})
	if slices.Contains(e.a.blockedTargets(e.ctx, e.a.store.Q), "pay.example") {
		t.Error("Approve did not remove the entry")
	}
}

func TestProposeSettingRefusesRulesPatternsAndSecretsNamingTheRightTool(t *testing.T) {
	e := newAgentEnv(t)
	for kind, tool := range map[string]string{"import_rule": "propose_rule", "skip_rule": "propose_skip_rule", "matter_pattern": "propose_pattern"} {
		text, bad := e.proposeSet(map[string]any{"kind": kind, "name": "x", "value": "y"})
		if !bad || !strings.Contains(text, tool) {
			t.Errorf("%s: %v %s", kind, bad, text)
		}
	}
	for _, name := range []string{"Claude API key", "api key", "Agent token", "Bridge token", "iCloud calendar address", "password", "claude.json"} {
		if text, bad := e.proposeSet(map[string]any{"kind": "setting", "name": name, "value": "x"}); !bad {
			t.Errorf("a secret was proposed: %s %s", name, text)
		}
	}
	// what list_settings gives holds no secret either
	out, _ := e.tool("list_settings", map[string]any{"kind": "settings"})
	for _, w := range []string{testClaudeKey, "token", "password"} {
		if strings.Contains(strings.ToLower(out), w) {
			t.Errorf("list_settings mentions %q", w)
		}
	}
}

func TestAProposalWhoseFromNoLongerMatchesIsWithdrawnAtApprove(t *testing.T) {
	e := newAgentEnv(t)
	id := e.proposeOK(map[string]any{"kind": "setting", "name": "Sync interval", "value": "10 minutes"})
	e.saveSettings(url.Values{keyInterval: {"15"}}) // the owner changed it himself
	rec := post(e.h, fmt.Sprintf("/settings/setting-proposals/%d/approve", id), url.Values{})
	if rec.Header().Get("Location") != "/settings?n=withdrawn#h-proposals" {
		t.Errorf("the answer: %s", rec.Header().Get("Location"))
	}
	if e.a.syncSettings(e.ctx).Interval.Minutes() != 15 {
		t.Error("a withdrawn proposal changed the setting")
	}
	if p, _ := e.a.store.SettingProposalByID(e.ctx, id); p.Status != "withdrawn" {
		t.Errorf("status %s", p.Status)
	}
	if !strings.Contains(get(e.h, "/settings?n=withdrawn").Body.String(), "This proposal no longer fits and was withdrawn.") {
		t.Error("the page does not say it was withdrawn")
	}
}

func TestDismissingAProposalChangesNothingAndIsRecorded(t *testing.T) {
	e := newAgentEnv(t)
	id := e.proposeOK(map[string]any{"kind": "setting", "name": "Chat quiet time", "value": "10 minutes"})
	post(e.h, fmt.Sprintf("/settings/setting-proposals/%d/dismiss", id), url.Values{})
	if e.a.chatQuiet(e.ctx).Minutes() != 30 || len(e.eventsOfType("setting_changed")) != 0 {
		t.Error("a dismissed proposal changed the setting")
	}
	if len(e.eventsOfType("setting_proposal_dismissed")) != 1 || len(mustWaiting(e)) != 0 {
		t.Error("the dismissal is not recorded")
	}
}

func TestNoAgentCanApproveOrApplyASettingProposal(t *testing.T) {
	e := newAgentEnv(t)
	id := e.proposeOK(map[string]any{"kind": "setting", "name": "Sync interval", "value": "10 minutes"})
	for _, p := range []string{fmt.Sprintf("/api/setting-proposals/%d/approve", id), fmt.Sprintf("/api/setting-proposals/%d/decision/approve", id), "/api/settings"} {
		if rec := e.api("POST", p, map[string]any{}); rec.Code == 200 || rec.Code == 303 {
			t.Errorf("%s: %d", p, rec.Code)
		}
	}
	if _, err := e.a.decideSettingProposal(e.ctx, agentActor("the owner"), id, true); err == nil || !isRefusal(err.Error(), "Actor by channel") && !strings.Contains(err.Error(), "Approval only") {
		t.Errorf("an agent decided in code: %v", err)
	}
	if e.a.syncSettings(e.ctx).Interval.Minutes() != 5 {
		t.Error("an agent changed a setting")
	}
	// the same change twice returns the one that waits
	e.proposeOK(map[string]any{"kind": "setting", "name": "Sync interval", "value": "10"})
	if len(mustWaiting(e)) != 1 {
		t.Error("the same proposal was made twice")
	}
}

func TestListSettingsListsTheSettingsAndTheWaitingChanges(t *testing.T) {
	e := newAgentEnv(t)
	e.proposeOK(map[string]any{"kind": "setting", "name": "Sync interval", "value": "10 minutes"})
	e.proposeOK(map[string]any{"kind": "guard", "name": "Protected people", "value": "off"})
	out := e.toolJSON("list_settings", map[string]any{"kind": "settings"})
	s := entryByName(out["settings"], "Sync interval")
	if s == nil || s["value"] != "5 minutes" || !strings.Contains(fmt.Sprint(s["allowed"]), "10 minutes") {
		t.Fatalf("the setting: %v", s)
	}
	for _, n := range []string{"Summary of what Claude closed", "Review model", "Daily limit"} {
		if entryByName(out["settings"], n) == nil {
			t.Errorf("list_settings lacks %s", n)
		}
	}
	waiting := out["waiting"].([]any)
	if len(waiting) != 1 || waiting[0].(map[string]any)["to"] != "10 minutes" { // kind settings: not the guard
		t.Errorf("waiting for kind settings: %v", waiting)
	}
	all := e.toolJSON("list_settings", map[string]any{})
	if len(all["waiting"].([]any)) != 2 {
		t.Errorf("waiting: %v", all["waiting"])
	}
}

func TestASettingProposalUnderATestRunCanNeverChangeARealSetting(t *testing.T) {
	e := newAgentEnv(t)
	run := "e2e-setprop01"
	text, bad := e.tool("propose_setting", map[string]any{"kind": "setting", "name": "Sync interval", "value": "10 minutes", "reason": "Test.", "run": run})
	if bad {
		t.Fatal(text)
	}
	if len(mustWaiting(e)) != 1 || mustWaiting(e)[0].TestRun != run {
		t.Fatalf("the run's proposal is not marked: %+v", mustWaiting(e))
	}
	if e.a.store.CountSettingProposals(e.ctx) != 0 {
		t.Error("a run's proposal counts for the owner")
	}
	// not in what an agent reads without the run
	if out := e.toolJSON("list_settings", map[string]any{}); len(out["waiting"].([]any)) != 0 {
		t.Error("a real list shows a run's proposal")
	}
	id := mustWaiting(e)[0].ID
	post(e.h, fmt.Sprintf("/settings/setting-proposals/%d/approve", id), url.Values{})
	if e.a.syncSettings(e.ctx).Interval.Minutes() != 5 || len(e.eventsOfType("setting_changed")) != 0 {
		t.Error("a run's proposal changed a real setting")
	}
	if p, _ := e.a.store.SettingProposalByID(e.ctx, id); p.Status != "approved" {
		t.Errorf("the run's proposal: %+v", p)
	}
	if _, err := e.a.removeRun(e.ctx, run); err != nil {
		t.Fatal(err)
	}
	if _, ok := e.a.store.SettingProposalByID(e.ctx, id); ok {
		t.Error("the run's proposal stays after the run is removed")
	}
}
