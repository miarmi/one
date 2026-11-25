package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"lab/apps/one/repo"
)

// SPEC 15: after a refresh the page says how many came and each source that
// failed with its error; the button and refresh_now read the same outcome.
func TestRefreshStateSaysWhatHappenedAndAgreesWithTheTool(t *testing.T) {
	e := newAgentEnv(t)
	begin := e.now
	e.a.refr.mu.Lock()
	e.a.refr.startedAt, e.a.refr.finished = begin, begin.Add(5*time.Minute)
	e.a.refr.mu.Unlock()

	read := func() (st refreshState) {
		if err := json.Unmarshal(get(e.h, "/refresh").Body.Bytes(), &st); err != nil {
			t.Fatal(err)
		}
		return st
	}
	if st := read(); st.New != 0 || len(st.Failed) != 0 {
		t.Fatalf("nothing happened yet: %+v", st)
	}

	writeSecret(e.a.secretsDir, gmailSecretFile, gmailSecret{Address: "a@example.test", Password: "x"})
	writeSecret(e.a.secretsDir, icloudSecretFile, icloudSecret{AppleID: "c@example.test", Password: "x"})
	// iCloud read three new entries and two changed ones; Gmail failed.
	e.now = begin.Add(time.Minute)
	if err := e.a.store.Update(e.ctx, func(tx *repo.Tx) error {
		id, err := tx.StartRun(e.ctx, srcICloud, "sync", e.now.Unix())
		if err != nil {
			return err
		}
		return tx.FinishRun(e.ctx, repo.Run{ID: id, Stored: 5, ItemsMade: 3}, "done", "", e.now.Unix())
	}); err != nil {
		t.Fatal(err)
	}
	e.failedRuns(srcGmail, failuresToStop, "The password was refused.")
	st := read()
	if st.New != 3 {
		t.Errorf("new counts new calendar entries only, not the two changed: %d", st.New)
	}
	if len(st.Failed) != 1 || st.Failed[0].Source != "Gmail" || !strings.Contains(st.Failed[0].Error, "refused") {
		t.Errorf("the failed source with its error: %+v", st.Failed)
	}
	m := e.toolJSON("refresh_now", map[string]any{"run": "e2e-refreshnotice"})
	if m["new_messages"] != float64(st.New) {
		t.Errorf("the tool and the button disagree: %v vs %d", m["new_messages"], st.New)
	}
	// while a round runs the page asks again later and shows nothing
	e.a.refr.mu.Lock()
	e.a.refr.running = true
	e.a.refr.mu.Unlock()
	if st := read(); !st.Running || st.New != 0 || len(st.Failed) != 0 {
		t.Errorf("a running round has no outcome yet: %+v", st)
	}
}

func TestRefreshNoticeIsOnThePageAndFilledByTheScript(t *testing.T) {
	e := newAgentEnv(t)
	if !strings.Contains(get(e.h, "/").Body.String(), `id="refresh-note"`) {
		t.Error("the page has no place for the refresh notice")
	}
	js := get(e.h, "/static/app.js").Body.String()
	for _, want := range []string{`"nothing new"`, `" new"`, `" failed: "`} {
		if !strings.Contains(js, want) {
			t.Errorf("the script lacks %s", want)
		}
	}
}

// One failed round is not yet "stopped", but "nothing new" must not read as if
// the source had answered; and no address stays in the error.
func TestOneFailedRoundIsNamedAndTheErrorHoldsNoAddress(t *testing.T) {
	e := newAgentEnv(t)
	writeSecret(e.a.secretsDir, gmailSecretFile, gmailSecret{Address: "a@example.test", Password: "x"})
	begin := e.now
	e.a.refr.mu.Lock()
	e.a.refr.startedAt, e.a.refr.finished = begin, begin.Add(time.Minute)
	e.a.refr.mu.Unlock()
	e.failedRuns(srcGmail, 1, "Gmail could not be reached: dial tcp 142.251.189.108:993: connect: connection refused for a@example.test")
	var st refreshState
	if err := json.Unmarshal(get(e.h, "/refresh").Body.Bytes(), &st); err != nil {
		t.Fatal(err)
	}
	if len(st.Failed) != 1 || st.Failed[0].Source != "Gmail" {
		t.Fatalf("a single failed round is named: %+v", st)
	}
	got := st.Failed[0].Error
	for _, bad := range []string{"142.251", "993", "a@example.test", "dial tcp"} {
		if strings.Contains(got, bad) {
			t.Errorf("the error holds %q: %s", bad, got)
		}
	}
	if got != "could not reach the server" || strings.Contains(got, "Gmail") || strings.Contains(got, ":") {
		t.Errorf("one plain fragment, no name, no colon: %s", got)
	}
}

// SPEC 15: the error in "<source> failed: <error>" is one plain fragment by
// cause: no source name again, no nested colon, no address.
func TestPlainCauseIsOneFragmentFromAFixedSet(t *testing.T) {
	fixed := map[string]bool{causeUnreachable: true, causeRefused: true, causeRunOut: true, causeServerError: true, causeSlow: true, causeSilent: true, causeUnknown: true}
	for raw, want := range map[string]string{
		"Gmail could not be reached or answered something unexpected: could not reach the server: connect: connection refused": causeUnreachable,
		"Gmail did not answer in time.":                     causeSlow,
		"Gmail refused the app password. Connect again.":    causeRefused,
		"The Microsoft sign-in has run out. Sign in again.": causeRunOut,
		"Microsoft answered 503 Service Unavailable":        causeServerError,
		"something nobody has seen":                         causeUnknown,
	} {
		got := plainCause(raw)
		if got != want || !fixed[got] || strings.Contains(got, ":") {
			t.Errorf("%q gives %q, want %q", raw, got, want)
		}
	}
	if plainCause("") != "" {
		t.Error("no error stays no error")
	}
}
