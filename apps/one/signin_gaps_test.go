package main

import (
	"testing"
	"time"
)

// SPEC 24: Beeper refusing the bridge's token is a sign-in that stopped, not
// a fine bridge: the Mac is heard but nothing arrives. One notice, and the
// source is fine again by itself once the Mac stops reporting the refusal.
func TestBeeperRefusingTheTokenIsNeedsLoginWithOneNotice(t *testing.T) {
	e := newChatEnv(t)
	alive := func(lastErr string) {
		e.asBridge("POST", "/api/bridge/alive", map[string]any{"networks": []bridgeNet{{Network: "whatsapp", State: "connected"}}, "last_error": lastErr})
	}
	alive("")
	if st := e.a.bridgeState(e.ctx, bridgeSrc); st.Status != statusOK {
		t.Fatalf("a quiet bridge is fine: %q", st.Status)
	}
	before := len(e.notes.all())
	alive("Beeper does not accept the token the bridge uses.")
	alive("Beeper does not accept the token the bridge uses.")
	e.now = e.now.Add(time.Minute)
	alive("Beeper does not accept the token the bridge uses.")
	st := e.a.bridgeState(e.ctx, bridgeSrc)
	if st.Status != statusLogin || !st.Alert {
		t.Fatalf("a refused Beeper token: %q alert %v", st.Status, st.Alert)
	}
	if got := e.notes.all(); len(got) != before+1 || got[before][0] != "Chat bridge needs login" {
		t.Errorf("one notice for the refusal: %v", got)
	}
	e.a.checkSources(e.ctx)
	if got := e.notes.all(); len(got) != before+1 {
		t.Errorf("the same failure told again: %v", got)
	}
	alive("") // the token was renewed on the Mac
	if st := e.a.bridgeState(e.ctx, bridgeSrc); st.Status != statusOK || st.Error != "" {
		t.Errorf("fine again by itself: %q %q", st.Status, st.Error)
	}
}

// A Beeper that does not answer for a long while stops the source; a short
// stumble does not.
func TestBeeperNotAnsweringForALongTimeStopsTheBridge(t *testing.T) {
	e := newChatEnv(t)
	alive := func() {
		e.asBridge("POST", "/api/bridge/alive", map[string]any{"networks": []bridgeNet{{Network: "whatsapp", State: "connected"}}, "last_error": "Beeper is not answering on the Mac."})
	}
	alive()
	if st := e.a.bridgeState(e.ctx, bridgeSrc); st.Status != statusOK {
		t.Fatalf("a short stumble is not a failure: %q", st.Status)
	}
	e.now = e.now.Add(minStale + time.Minute)
	alive()
	if st := e.a.bridgeState(e.ctx, bridgeSrc); st.Status != statusError || !st.Stopped {
		t.Errorf("a lasting silence of Beeper: %q", st.Status)
	}
}

// A busy or unwell Gmail is not a refused password.
func TestBusyGmailIsNotALoginProblem(t *testing.T) {
	for text, want := range map[string]bool{
		"[AUTHENTICATIONFAILED] Invalid credentials (Failure)": false,
		"[ALERT] Application-specific password required":       false,
		"[UNAVAILABLE] Temporary system error":                 true,
		"Too many simultaneous connections. (Failure)":         true,
		"[THROTTLED] slow down":                                true,
	} {
		if got := imapTransient(text); got != want {
			t.Errorf("%q: transient %v, want %v", text, got, want)
		}
	}
}
