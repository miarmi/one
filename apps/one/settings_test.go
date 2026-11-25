package main

import (
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Tests of Settings (SPEC 28). All data is invented.

func TestSettingsPageWordingAndDefaults(t *testing.T) {
	e := newAgentEnv(t)
	page := get(e.h, "/settings").Body.String()
	for _, want := range []string{"<title>Settings</title>", `id="page-title">Settings</h2>`, "Sync interval", `<option value="5" selected>5 minutes</option>`,
		"First import depth", `<option value="365" selected>1 year</option>`, "Review window", `<option value="30" selected>1 month</option>`,
		"Chat quiet time", `<option value="30" selected>30 minutes</option>`, "Bridge silence before a notice", `<option value="15" selected>15 minutes</option>`,
		"Quiet time before closing", `<option value="14" selected>14 days</option>`, "Proposals from Claude", ">Save</button>", "Guards", "Always on", "Back to the queue"} {
		if !strings.Contains(page, want) {
			t.Errorf("the page lacks %q", want)
		}
	}
}

// SPEC 28: the two agenda settings carry the spec's wording and a month as default.
func TestSpec28AgendaWindowSettingsUseTheSpecsWords(t *testing.T) {
	e := newAgentEnv(t)
	page := get(e.h, "/settings").Body.String()
	for _, want := range []string{"Agenda window back", "Agenda window ahead"} {
		if strings.Count(page, want) != 1 {
			t.Errorf("the page has %q %d times, want once", want, strings.Count(page, want))
		}
	}
	if strings.Contains(page, "Agenda: days") || strings.Count(page, `<option value="30" selected>1 month</option>`) < 3 {
		t.Error("the agenda settings do not default to 1 month")
	}
}

func (e *agentEnv) saveSettings(v url.Values) (code int, body string) {
	rec := post(e.h, "/settings", v)
	return rec.Code, rec.Body.String()
}

func TestEachSettingIsValidatedTakesEffectAndIsRecorded(t *testing.T) {
	e := newAgentEnv(t)
	ctx := e.ctx
	// Each setting: a value that is refused, one that is taken, and where it is read.
	cases := []struct {
		def      settingDef
		bad, ok  string
		effect   func() string
		wantText string
	}{
		{settingDefs[0], "3", "10", func() string { return e.a.syncSettings(ctx).Interval.String() }, "10m0s"},
		{settingDefs[1], "400", "180", func() string { return e.a.syncSettings(ctx).FirstDepth.String() }, (180 * 24 * time.Hour).String()},
		{settingDefs[2], "31", "14", func() string { return e.a.syncSettings(ctx).ReviewWindow.String() }, (14 * 24 * time.Hour).String()},
		{settingDefs[3], "7", "10", func() string { return e.a.chatQuiet(ctx).String() }, "10m0s"},
		{settingDefs[4], "7", "30", func() string { return e.a.bridgeSilence(ctx).String() }, "30m0s"},
		{settingDefs[5], "x", "30", func() string { return strings.Repeat("d", e.a.quietDays(ctx)) }, strings.Repeat("d", 30)},
		{settingDefs[6], "31", "60", func() string { return strconv.Itoa(e.a.agendaDays(ctx, keyAgendaBack)) }, "60"},
		{settingDefs[7], "x", "14", func() string { return strconv.Itoa(e.a.agendaDays(ctx, keyAgendaAhead)) }, "14"},
		{settingDefs[8], "weekly", "off", func() string { return strconv.FormatBool(e.a.summaryOn(ctx)) }, "false"},
	}
	if len(cases) != len(settingDefs) {
		t.Fatalf("a setting has no test: %d of %d", len(cases), len(settingDefs))
	}
	for _, c := range cases {
		before := c.effect()
		code, body := e.saveSettings(url.Values{c.def.Key: {c.bad}})
		if code != 400 || !strings.Contains(body, c.def.Label) || !strings.Contains(body, "Nothing was saved") {
			t.Errorf("%s: a bad value %q gave %d", c.def.Label, c.bad, code)
		}
		if c.effect() != before {
			t.Errorf("%s: a refused value changed the setting", c.def.Label)
		}
		for _, ev := range e.eventsOfType("setting_changed") {
			if strings.Contains(ev, c.def.Key) {
				t.Errorf("%s: a refused value was recorded", c.def.Label)
			}
		}
		code, _ = e.saveSettings(url.Values{c.def.Key: {c.ok}})
		if code != 303 {
			t.Errorf("%s: %q gave %d", c.def.Label, c.ok, code)
		}
		if got := c.effect(); got != c.wantText {
			t.Errorf("%s: the code reads %s, want %s", c.def.Label, got, c.wantText)
		}
		recorded := 0
		for _, ev := range e.eventsOfType("setting_changed") {
			if strings.Contains(ev, `"setting":"`+c.def.Key+`"`) && strings.Contains(ev, `"to":"`+c.ok+`"`) {
				recorded++
			}
		}
		if recorded != 1 {
			t.Errorf("%s: recorded %d times", c.def.Label, recorded)
		}
		if by := e.settingBy(c.def.Key); by != "owner" {
			t.Errorf("%s: written by %q", c.def.Label, by)
		}
		// Saving the same value again records nothing more.
		e.saveSettings(url.Values{c.def.Key: {c.ok}})
	}
	if n := len(e.eventsOfType("setting_changed")); n != len(settingDefs) {
		t.Errorf("%d events for %d changes", n, len(settingDefs))
	}
	// One bad value among good ones saves nothing.
	code, _ := e.saveSettings(url.Values{keyInterval: {"15"}, keyQuietDays: {"-1"}})
	if code != 400 || e.a.syncSettings(ctx).Interval != 10*time.Minute {
		t.Errorf("a form with one bad value saved part of it: %d", code)
	}
	// The page shows what is in force.
	if page := get(e.h, "/settings").Body.String(); !strings.Contains(page, `<option value="30" selected>30 days</option>`) {
		t.Error("the page does not show the new quiet time")
	}
}

// A new review window changes what a new mail does: an item whose last
// message is older arrives as history.
func TestReviewWindowSettingDecidesHistory(t *testing.T) {
	e := newAgentEnv(t)
	cutoff := func() int64 { return e.now.Add(-e.a.syncSettings(e.ctx).ReviewWindow).Unix() }
	at := e.ago(20)
	if newItem(arrival{At: at, Subject: "x"}, cutoff()).Stage != stageNew {
		t.Fatal("a 20 day old mail with the default window is not new")
	}
	e.saveSettings(url.Values{keyReviewDays: {"14"}})
	if newItem(arrival{At: at, Subject: "x"}, cutoff()).Stage != stageHistory {
		t.Error("with a 2 week window a 20 day old mail is not history")
	}
}

func (e *agentEnv) eventsOfType(typ string) []string {
	r, err := e.a.store.RawEvents(e.ctx, typ)
	if err != nil {
		e.t.Fatal(err)
	}
	return r
}

func (e *agentEnv) settingBy(key string) string { return e.a.store.SettingBy(e.ctx, key) }

func TestSettingCannotBeChangedThroughAPI(t *testing.T) {
	e := newAgentEnv(t)
	for _, c := range []struct{ method, path string }{
		{"POST", "/api/settings"},
		{"PUT", "/api/settings/sync_interval_minutes"},
		{"POST", "/api/settings/quiet_close_days"},
		{"POST", "/api/guards/protected_people/off"},
		{"POST", "/api/settings/guards/protected_people/off"},
		{"POST", "/api/people/1"},
	} {
		body := map[string]any{"key": keyInterval, "value": "1", "sync_interval_minutes": "1", "agent": "the owner"}
		rec := e.api(c.method, c.path, body)
		got := strings.TrimSpace(rec.Body.String())
		ownerOnlyPath := strings.Contains(c.path, "setting") || strings.Contains(c.path, "guard")
		switch {
		case ownerOnlyPath && (rec.Code != 403 || got != onlyOwnerMsg):
			t.Errorf("%s %s: %d %s", c.method, c.path, rec.Code, got)
		case !ownerOnlyPath && rec.Code != 404:
			t.Errorf("%s %s: %d %s", c.method, c.path, rec.Code, got)
		}
		// Without the token it is the wrong-token answer, never a change.
		if rec := e.call(c.method, c.path, "", body); rec.Code != 401 {
			t.Errorf("%s %s without a token: %d", c.method, c.path, rec.Code)
		}
	}
	if e.a.syncSettings(e.ctx).Interval != 5*time.Minute || e.a.guardOn(e.ctx, e.a.store.Q, "protected_people") == false {
		t.Error("a setting or a guard changed through /api/")
	}
	// The connector has no tool that changes a setting; list_settings (SPEC 29)
	// only reads them.
	for _, tl := range mcpTools {
		if ro, _ := tl.Annotations["readOnlyHint"].(bool); ro && tl.Name == "list_settings" {
			continue
		}
		if tl.Name == "propose_setting" {
			continue // a proposal only: nothing changes before the owner's Approve (settingproposals_test.go)
		}
		if strings.Contains(tl.Name, "setting") || strings.Contains(tl.Name, "guard") || strings.Contains(tl.Name, "approve") {
			t.Errorf("tool %s", tl.Name)
		}
	}
}

func TestGuardsOnSettingsAreSwitchedOnlyHereAndSomeNeverOff(t *testing.T) {
	e := newAgentEnv(t)
	page := get(e.h, "/settings").Body.String()
	for _, want := range []string{"Reason required", "Protected people", "Approval only", "Exact text", "Same thread", "Blocked targets", "Send once", "Approval required", "Actor by channel", "Refused recently: 0"} {
		if !strings.Contains(page, want) {
			t.Errorf("the page lacks %q", want)
		}
	}
	if strings.Count(page, ">Always on<") != 6 {
		t.Errorf("Always on shown %d times, want 6", strings.Count(page, ">Always on<"))
	}
	// A guard that can be switched off.
	if rec := post(e.h, "/settings/guards/protected_people/off", nil); rec.Code != 303 || e.a.guardOn(e.ctx, e.a.store.Q, "protected_people") {
		t.Fatalf("switch off: %d", rec.Code)
	}
	if page := get(e.h, "/settings").Body.String(); !strings.Contains(page, ">Off<") || !strings.Contains(page, "Switch on") {
		t.Error("the page does not show the guard as Off")
	}
	if n := len(e.eventsOfType("setting_changed")); n != 1 {
		t.Errorf("recorded %d times", n)
	}
	post(e.h, "/settings/guards/protected_people/on", nil)
	if !e.a.guardOn(e.ctx, e.a.store.Q, "protected_people") {
		t.Error("switch on did not work")
	}
	// A guard that can never be switched off is refused, and so is a name that is none.
	for _, key := range []string{"approval_only", "exact_text", "same_thread", "send_once", "approval_required", "actor_by_channel"} {
		if rec := post(e.h, "/settings/guards/"+key+"/off", nil); rec.Code != 400 || !strings.Contains(rec.Body.String(), "always on") {
			t.Errorf("%s: %d", key, rec.Code)
		}
		if !e.a.guardOn(e.ctx, e.a.store.Q, key) {
			t.Errorf("%s is off", key)
		}
	}
	if rec := post(e.h, "/settings/guards/nothing/off", nil); rec.Code != 404 {
		t.Errorf("unknown guard: %d", rec.Code)
	}
	// What a guard refused shows on the page.
	it := e.newItem(stageNew, true, "Parcel", "")
	e.post("/api/items/"+ref(it)+"/skip", map[string]any{"kind": "noise"})
	if page := get(e.h, "/settings").Body.String(); !strings.Contains(page, "Refused recently: 1") {
		t.Error("the refusal is not counted")
	}
}

func TestSettingsListsWhatWaitsForApprove(t *testing.T) {
	e := newAgentEnv(t)
	if page := get(e.h, "/settings").Body.String(); !strings.Contains(page, "Nothing waits for Approve.") {
		t.Error("the empty state is missing")
	}
	it := e.newItem(stageInProgress, false, "Plumber visit", "")
	e.propose(it, "close", "The visit is over.")
	page := get(e.h, "/settings").Body.String()
	if !strings.Contains(page, "Plumber visit: Close") || !strings.Contains(page, "The visit is over.") {
		t.Errorf("the proposal is not listed:\n%s", page)
	}
}
