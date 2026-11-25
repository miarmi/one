package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"lab/apps/one/repo"
)

// failedRuns records n failed fetches of a source, one a minute.
func (e *agentEnv) failedRuns(src string, n int, why string) {
	e.t.Helper()
	for i := 0; i < n; i++ {
		e.now = e.now.Add(time.Minute)
		if err := e.a.store.Update(e.ctx, func(tx *repo.Tx) error {
			id, err := tx.StartRun(e.ctx, src, "sync", e.now.Unix())
			if err != nil {
				return err
			}
			return tx.FinishRun(e.ctx, repo.Run{ID: id}, "failed", why, e.now.Unix())
		}); err != nil {
			e.t.Fatal(err)
		}
	}
}

func sourceNamed(m map[string]any, name string) map[string]any {
	for _, x := range m["sources"].([]any) {
		if s := x.(map[string]any); s["source"] == name {
			return s
		}
	}
	return nil
}

func TestServiceStatusShowsWhatTheConnectionsPageShowsAndNoSecret(t *testing.T) {
	e := newAgentEnv(t)
	m := e.toolJSON("service_status", map[string]any{})
	if g := sourceNamed(m, "Gmail"); g == nil || g["state"] != "Not configured" {
		t.Fatalf("an unconfigured Gmail: %v", g)
	}
	if c := sourceNamed(m, "Claude"); c == nil || c["claude_api_key"] != "not configured" {
		t.Fatalf("claude: %v", c)
	}
	if n := m["night_check"].(map[string]any); n["last_ran"] != "never" {
		t.Errorf("night: %v", n)
	}
	if m["waiting_for_claude"] == nil || !strings.Contains(m["note"].(string), "data") {
		t.Errorf("answer: %v", m)
	}

	const pw = "hunter2-very-secret"
	writeSecret(e.a.secretsDir, gmailSecretFile, gmailSecret{Address: "owner@example.test", Password: pw})
	writeSecret(e.a.secretsDir, icloudSecretFile, icloudSecret{AppleID: "apple@example.test", Password: pw})
	e.a.store.SetSettingBy(e.ctx, keyICloudCals, `[{"key":"a","name":"Family","show":true}]`, "test", e.now)
	e.a.store.SetSettingBy(e.ctx, keyGoogleCals, `[{"key":"g","name":"Kids school","tail":"ab12","show":true}]`, "test", e.now)
	writeSecret(e.a.secretsDir, "google.json", googleSecret{Addresses: map[string]string{"g": "https://calendar.google.test/secret-" + pw + "/basic.ics"}})
	e.failedRuns(srcGmail, failuresToStop, "The mail server could not be reached.")

	m = e.toolJSON("service_status", map[string]any{})
	g := sourceNamed(m, "Gmail")
	if g["state"] != "Error" || g["last_error"] != "could not reach the server" || g["came_today"] != float64(0) || g["last_read_at"] == nil {
		t.Fatalf("a failed Gmail: %v", g)
	}
	// The Connectors page says the same.
	page := get(e.h, "/connectors").Body.String()
	if !strings.Contains(page, ">Error<") || !strings.Contains(page, "could not reach the server") {
		t.Error("the page and the tool disagree")
	}
	if i := sourceNamed(m, "iCloud Calendar"); i == nil || fmt.Sprint(i["calendars"]) != "[Family]" {
		t.Errorf("icloud: %v", i)
	}
	if gc := sourceNamed(m, "Google Calendar"); gc == nil || fmt.Sprint(gc["calendars"]) != "[Kids school]" {
		t.Errorf("google: %v", gc)
	}
	raw, _ := json.Marshal(m)
	for _, secret := range []string{pw, "secret-", "apple@example.test", "owner@example.test", e.a.agentTokenValue(), e.a.bridgeTokenValue(), "ab12"} {
		if secret != "" && strings.Contains(string(raw), secret) {
			t.Errorf("the answer holds %q", secret)
		}
	}
	// A reading tool writes nothing.
	before := len(e.personEvents(""))
	e.tool("service_status", map[string]any{})
	if after := len(e.personEvents("")); after != before {
		t.Errorf("service_status wrote %d events", after-before)
	}
}

func TestServiceStatusSaysTheBridgeIsSilentAndTheNightCheckRan(t *testing.T) {
	e := newAgentEnv(t)
	e.a.store.SetSettingBy(e.ctx, keyBridgeHeard, fmt.Sprint(e.now.Add(-2*time.Hour).Unix()), "test", e.now)
	m := e.toolJSON("service_status", map[string]any{})
	b := sourceNamed(m, "Chat bridge")
	if b == nil || b["state"] != "Error" || b["last_error"] != "it did not answer" {
		t.Fatalf("a silent bridge: %v", b)
	}
	if err := e.a.store.Update(e.ctx, func(tx *repo.Tx) error {
		id, err := tx.InsertNightCheck(e.ctx, "2026-11-19", "model-x", e.now.Unix())
		if err != nil {
			return err
		}
		return tx.FinishNightCheck(e.ctx, repo.NightCheck{ID: id, Status: "done", Checked: 120, BroughtBack: 3, FinishedAt: e.now.Unix()})
	}); err != nil {
		t.Fatal(err)
	}
	m = e.toolJSON("service_status", map[string]any{})
	if n := m["night_check"].(map[string]any); n["result"] != "done" || n["brought_back"] != float64(3) {
		t.Errorf("night: %v", n)
	}
}

// SPEC 24: every source that stops, the chat bridge and the calendars too, is
// told to the owner once, however many times the timer looks.
func TestEveryStoppedSourceSendsExactlyOneNotice(t *testing.T) {
	e := newAgentEnv(t)
	var sent []string
	e.a.notify = func(title, body, link string) error { sent = append(sent, title); return nil }
	writeSecret(e.a.secretsDir, gmailSecretFile, gmailSecret{Address: "a@example.test", Password: "x"})
	writeSecret(e.a.secretsDir, graphSecretFile, graphSecret{Address: "b@example.test", RefreshToken: "r"})
	writeSecret(e.a.secretsDir, icloudSecretFile, icloudSecret{AppleID: "c@example.test", Password: "x"})
	e.a.store.SetSettingBy(e.ctx, keyGoogleCals, `[{"key":"g","name":"Kids","show":true}]`, "test", e.now)
	for _, src := range []string{srcGmail, srcGraph, srcICloud, srcGoogleCal, srcOutlookCal} {
		e.failedRuns(src, failuresToStop, "It could not be read.")
	}
	e.a.store.SetSettingBy(e.ctx, keyBridgeHeard, fmt.Sprint(e.now.Add(-time.Hour).Unix()), "test", e.now)
	for i := 0; i < 5; i++ { // the timer looks every minute
		e.now = e.now.Add(time.Minute)
		e.a.checkSources(e.ctx)
	}
	count := map[string]int{}
	for _, s := range sent {
		count[s]++
	}
	for _, want := range []string{"Gmail has stopped", "Office mail has stopped", "iCloud Calendar has stopped", "Google Calendar has stopped", "The Mac bridge is silent"} {
		if count[want] != 1 {
			t.Errorf("%q sent %d times; all: %v", want, count[want], count)
		}
	}
}

func TestRefreshNowStartsOneRoundAndAnswersPerSource(t *testing.T) {
	e := newAgentEnv(t)
	m := e.toolJSON("refresh_now", map[string]any{})
	if m["fetch"] != "started" || m["finished"] != true {
		t.Fatalf("a round that starts and finishes: %v", m)
	}
	if _, ok := m["sources"].([]any); !ok {
		t.Fatalf("per source answer: %v", m)
	}
	if _, ok := m["failed"].([]any); !ok {
		t.Fatalf("failed list: %v", m)
	}
	// asked again at once: nothing new starts, and it says when to retry
	m = e.toolJSON("refresh_now", map[string]any{})
	if !strings.HasPrefix(m["fetch"].(string), "not started") || m["retry_after_seconds"] == nil {
		t.Fatalf("a second ask within a minute: %v", m)
	}
	// the tool is marked as changing something, and idempotent
	for _, tl := range mcpTools {
		if tl.Name == "refresh_now" && tl.Annotations["readOnlyHint"] != false {
			t.Errorf("refresh_now must not be read-only: %v", tl.Annotations)
		}
	}
}

func TestRefreshNowJoinsARunningRoundAndNeverStartsTwo(t *testing.T) {
	e := newAgentEnv(t)
	if !e.a.refr.begin() { // the page's button, or the timer, is fetching
		t.Fatal("begin")
	}
	go func() {
		time.Sleep(150 * time.Millisecond)
		e.a.refr.end(time.Now())
	}()
	m := e.toolJSON("refresh_now", map[string]any{})
	if !strings.HasPrefix(m["fetch"].(string), "joined") || m["finished"] != true {
		t.Fatalf("it joins the running round and waits for it: %v", m)
	}
}

func TestRefreshNowUnderATestRunStartsNoFetch(t *testing.T) {
	e := newAgentEnv(t)
	m := e.toolJSON("refresh_now", map[string]any{"run": "e2e-refreshtest1"})
	if !strings.HasPrefix(m["fetch"].(string), "not started: a call under a test run") {
		t.Fatalf("a test-run call: %v", m)
	}
	if running, fin := e.a.refr.state(); running || !fin.IsZero() {
		t.Errorf("a test run started a round: %v %v", running, fin)
	}
}
