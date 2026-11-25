package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"lab/platform/appkit"
)

// SPEC 24: a failed source is never silent. The state of each mail source is
// worked out from its runs and from the sign-in flags, never kept as a
// second truth; what is kept is only which failure the owner's phone was already
// told about, so one failure sends one notice and not one per timer tick.
//
//   - Error: the third failed fetch in a row. Red, with the error, one notice.
//   - Needs login: the sign-in ran out. The source pauses, one notice, and
//     fetching resumes by itself after the sign-in.
//   - Stale: connected, no fetch has finished for three intervals (at least
//     15 minutes). The timer is not doing its work; a failed source too.
//   - Previews never notify: appkit.Notify writes the notice and the platform
//     drops it; nothing here tries to avoid that.

const (
	statusOK        = "OK"
	statusStale     = "Stale"
	statusError     = "Error"
	statusLogin     = "Needs login"
	statusNotConf   = "Not configured"
	failuresToStop  = 3
	minStale        = 15 * time.Minute
	keyGmailLogin   = "gmail_needs_login" // "1" while Gmail refuses the password
	healthKeyPrefix = "health."
)

// gmailLoginErr marks a refused Gmail sign-in, so it is told apart from a
// network failure.
type gmailLoginErr struct{ inner error }

func (e *gmailLoginErr) Error() string { return e.inner.Error() }
func (e *gmailLoginErr) Unwrap() error { return e.inner }

const gmailLoginText = "Gmail refused the app password. Connect Gmail again with a new app password; fetching continues by itself."

func (a *app) gmailNeedsLogin(ctx context.Context) bool {
	return a.store.Setting(ctx, keyGmailLogin) == "1"
}

func (a *app) setGmailNeedsLogin(ctx context.Context, on bool) {
	v := ""
	if on {
		v = "1"
	}
	if a.store.Setting(ctx, keyGmailLogin) != v {
		a.store.SetSettingBy(context.WithoutCancel(ctx), keyGmailLogin, v, "system", a.now())
	}
}

// sourceState is how one mail source stands, in words for the page.
type sourceState struct {
	Key        string
	Label      string
	Configured bool
	Status     string
	Cls        string // st-ok, st-stale or st-error
	Alert      bool   // named in the strip
	Stopped    bool   // turns the dot to "a source has stopped"
	Error      string
	ErrorAt    string
	LastRead   string
	Silent     time.Duration // on Stale: how long since the last fetch finished
	Quiet      bool          // on Stale: no attempt either, not even a failed one: the runs have stopped coming
	Networks   []string      // on the chat bridge: the networks that need a new login
	episode    string        // identifies one failure, for the notice
	kind       string        // "stopped", "login" or ""
}

// runsStreak counts the fetches that failed in a row, newest first, and says
// where the streak began and what the latest error was.
func (a *app) runsStreak(ctx context.Context, src string) (n int, firstID int64, lastErr string, lastErrAt int64, anyRun bool) {
	rows, err := a.store.RecentRuns(ctx, src, 50)
	if err != nil {
		return
	}
	anyRun = len(rows) > 0
	for _, r := range rows {
		if r.Status == "failed" {
			n++
			firstID = r.ID
			if lastErr == "" {
				lastErr, lastErrAt = r.Error, r.FinishedAt
			}
			continue
		}
		break
	}
	return
}

func (a *app) staleAfter(ctx context.Context) time.Duration {
	return max(3*a.syncSettings(ctx).Interval, minStale)
}

// sourceStateOf works out one source: Gmail or the Office mailbox.
func (a *app) sourceStateOf(ctx context.Context, src mailSource) sourceState {
	s := a.sourceStateRaw(ctx, src)
	if s.Error != "" {
		// What the run said stays in the log; every place that shows the
		// error shows the one plain fragment for its cause.
		s.Error = plainCause(s.Error)
	}
	return s
}

func (a *app) sourceStateRaw(ctx context.Context, src mailSource) sourceState {
	if src.Name == srcBridge {
		return a.bridgeState(ctx, src)
	}
	s := sourceState{Key: src.Name, Label: src.Label, Status: statusNotConf, Cls: "st-stale"}
	loggedOut := false
	switch src.Name {
	case srcGmail:
		_, s.Configured = a.gmailSecret()
		loggedOut = s.Configured && a.gmailNeedsLogin(ctx)
	case srcICloud:
		_, s.Configured = a.icloudSecret()
		loggedOut = s.Configured && a.icloudNeedsLogin(ctx)
	case srcGoogleCal:
		s.Configured = len(a.googleCals(ctx)) > 0 // no sign-in: a refused or reset address is an error, not a login
	case srcOutlookCal:
		s.Configured = a.graphSigned() // the same Microsoft sign-in as the mail
		loggedOut = s.Configured && a.graphNeedsLogin(ctx)
	default:
		s.Configured = a.graphSigned()
		loggedOut = s.Configured && a.graphNeedsLogin(ctx)
	}
	if !s.Configured {
		return s
	}
	n, firstID, lastErr, lastErrAt, anyRun := a.runsStreak(ctx, src.Name)
	if r, ok := a.store.LastFinishedRun(ctx, src.Name, "sync"); ok {
		s.LastRead = a.clock(r.FinishedAt)
	}
	if lastErr != "" {
		s.Error, s.ErrorAt = lastErr, a.clock(lastErrAt)
	}
	switch {
	case loggedOut:
		s.Status, s.Cls, s.Alert, s.Stopped = statusLogin, "st-error", true, true
		s.kind, s.episode = "login", fmt.Sprintf("login:%d", a.store.SettingAt(ctx, loginKey(src.Name)))
		if s.Error == "" {
			s.Error = map[string]string{srcGmail: gmailLoginText, srcICloud: msgICloudAuth, srcOutlookCal: plainOutlookCal(errNeedsLogin)}[src.Name]
			if s.Error == "" {
				s.Error = needsLoginText
			}
		}
		if src.Name == srcOutlookCal {
			// One sign-in, one story: the mail's strip and notice already say it.
			s.Alert, s.kind, s.episode = false, "", ""
		}
	case n >= failuresToStop:
		s.Status, s.Cls, s.Alert, s.Stopped = statusError, "st-error", true, true
		s.kind, s.episode = "stopped", fmt.Sprintf("failed:%d", firstID)
	case anyRun:
		last := a.store.LastRunTime(ctx, src.Name)
		if since := a.now().Sub(time.Unix(last, 0)); last > 0 && since > a.staleAfter(ctx) {
			s.Status, s.Cls, s.Alert, s.Silent = statusStale, "st-stale", true, since
			// Still trying (a failed fetch lately) is late; no attempt at all is a source that stopped.
			if rows, err := a.store.RecentRuns(ctx, src.Name, 1); err != nil || len(rows) == 0 || a.now().Sub(time.Unix(rows[0].FinishedAt, 0)) > a.staleAfter(ctx) {
				s.Quiet = true
			}
			s.kind, s.episode = "stale", fmt.Sprintf("stale:%d", last)
		} else {
			s.Status, s.Cls = statusOK, "st-ok"
		}
	default:
		s.Status, s.Cls = "Not tested yet", "st-stale"
	}
	return s
}

func loginKey(src string) string {
	switch src {
	case srcGmail:
		return keyGmailLogin
	case srcICloud:
		return keyICloudLogin
	}
	return keyGraphLogin // the mail and the calendar of Office share one sign-in
}

// healthSources are the sources the app reads itself and watches: the two
// mailboxes, then the calendars (SPEC 24).
var healthSources = []mailSource{gmailSrc, graphSrc, bridgeSrc, outlookCalSrc, icloudSrc, googleCalSrc}

func (a *app) sourceStates(ctx context.Context) []sourceState {
	out := make([]sourceState, 0, len(healthSources))
	for _, src := range healthSources {
		if a.sourceRemoved(ctx, src.Name) {
			continue // the owner took it off the page; a new read brings it back
		}
		out = append(out, a.sourceStateOf(ctx, src))
	}
	return out
}

// sourceStatesAll is every source, removed ones too.
func (a *app) sourceStatesAll(ctx context.Context) []sourceState {
	out := make([]sourceState, 0, len(healthSources))
	for _, src := range healthSources {
		out = append(out, a.sourceStateOf(ctx, src))
	}
	return out
}

// --- the dot and the strip ---

type dotView struct {
	Label string
	Cls   string // dot-ok, dot-look or dot-stopped
	Mark  string // a shape, so colour is not the only sign
	Lines []dotLine
}

type dotLine struct{ Name, State, Cls string }

func dotFor(states []sourceState) dotView {
	d := dotView{Label: "Connectors: all fine", Cls: "dot-ok", Mark: "●"}
	for _, s := range states {
		switch {
		case s.Stopped:
			d = dotView{Label: "Connectors: a source has stopped", Cls: "dot-stopped", Mark: "■"}
		case s.Status == statusStale && d.Cls == "dot-ok":
			d = dotView{Label: "Connectors: needs a look", Cls: "dot-look", Mark: "▲"}
		}
		d.Lines = append(d.Lines, dotLine{s.Label, s.Status, s.Cls})
	}
	return d
}

// alertText is the strip about one source.
func alertText(s sourceState) string {
	switch s.Status {
	case statusLogin:
		return s.Label + " needs login. See Connectors."
	case statusStale:
		return fmt.Sprintf("%s has not been read for %s. See Connectors.", s.Label, silentText(s.Silent))
	}
	return s.Label + " has stopped. See Connectors."
}

func silentText(d time.Duration) string {
	if d >= 2*time.Hour {
		return fmt.Sprintf("%d hours", int(d.Hours()))
	}
	return fmt.Sprintf("%d minutes", int(d.Minutes()))
}

// alerts lists the strips for failed sources: stopped first, then login, then stale.
func alertsFor(states []sourceState) []string {
	var out []string
	for _, status := range []string{statusError, statusLogin, statusStale} {
		for _, s := range states {
			if s.Alert && s.Status == status {
				out = append(out, alertText(s))
			}
		}
	}
	return out
}

// --- notices ---

type healthMark struct {
	Kind    string `json:"kind"`
	Episode string `json:"episode"`
}

func (a *app) notifyTitle(s sourceState) string {
	if s.kind == "silent" {
		return "The Mac bridge is silent"
	}
	if s.kind == "login" {
		return s.Label + " needs login"
	}
	return s.Label + " has stopped"
}

func (a *app) notifyBody(s sourceState) string {
	switch s.kind {
	case "silent":
		return fmt.Sprintf("ONE has not heard from the Mac for %s. Chats arrive only while the Mac is on and Beeper is running.", silentText(s.Silent))
	case "login":
		if s.Key == srcBridge {
			if len(s.Networks) > 0 {
				return strings.Join(s.Networks, ", ") + " needs a new login. Nothing arrives from it until you log in again in Beeper on the Mac."
			}
			return "Beeper does not accept the token the bridge uses. Nothing arrives until the token is renewed on the Mac."
		}
		return "The sign-in has run out. Fetching is paused until you sign in again on the Connectors page."
	case "stale":
		return fmt.Sprintf("ONE has not read it for %s. Open Connectors to see why.", silentText(s.Silent))
	}
	return "ONE could not read it three times in a row. Open Connectors to see why."
}

// checkSource sends the notice for a failure that the owner's phone was not told
// about yet, and forgets the failure once the source is fine again. It is
// called after every run and every minute by the housekeeping loop. A notice
// carries the name of the source and nothing from any mail.
func (a *app) checkSource(ctx context.Context, src mailSource) {
	ctx = context.WithoutCancel(ctx)
	s := a.sourceStateOf(ctx, src)
	key := healthKeyPrefix + src.Name
	var mark healthMark
	json.Unmarshal([]byte(a.store.Setting(ctx, key)), &mark)
	if s.kind == "" {
		if mark.Kind != "" && s.Status == statusOK {
			a.store.SetSettingBy(ctx, key, "", "system", a.now()) // recovered: the next failure is a new one
		}
		return
	}
	if mark.Kind == s.kind && mark.Episode == s.episode {
		return // this failure was told already
	}
	// Remember first: a notice that cannot be sent is not sent again every tick.
	b, _ := json.Marshal(healthMark{Kind: s.kind, Episode: s.episode})
	if err := a.store.SetSettingBy(ctx, key, string(b), "system", a.now()); err != nil {
		return
	}
	link := ""
	if base := os.Getenv("BASE_URL"); base != "" {
		link = strings.TrimRight(base, "/") + "/connectors"
	}
	if err := a.tell(a.notifyTitle(s), a.notifyBody(s), link); err != nil {
		appkit.Error("sending a notice about a source", err)
	}
}

func (a *app) checkSources(ctx context.Context) {
	for _, src := range healthSources {
		a.checkSource(ctx, src)
	}
}

// --- the chat bridge (SPEC 24) ---

const srcBridge = "bridge"

// bridgeTokenRefused is what the bridge reports when Beeper refuses its token
// (the same words as in bridge/bridge.go).
const bridgeTokenRefused = "does not accept the token"

var bridgeSrc = mailSource{Name: srcBridge, Label: "Chat bridge"}

// bridgeHeardAt is when the Mac was last heard from (0: never).
func (a *app) bridgeHeardAt(ctx context.Context) int64 {
	n, _ := strconv.ParseInt(a.store.Setting(ctx, keyBridgeHeard), 10, 64)
	return n
}

// bridgeNetworks are the networks the bridge last said it reads.
func (a *app) bridgeNetworks(ctx context.Context) []bridgeNet {
	var nets []bridgeNet
	json.Unmarshal([]byte(a.store.Setting(ctx, keyBridgeNets)), &nets)
	return nets
}

// bridgeState is how the Mac stands: silent for longer than the limit in
// Settings, a failed source that is named in the strip and in one notice; or
// heard, with a network that has logged itself out; or fine. It is worked
// out from the time it was last heard, never kept as a second truth.
func (a *app) bridgeState(ctx context.Context, src mailSource) sourceState {
	s := sourceState{Key: src.Name, Label: src.Label, Status: statusNotConf, Cls: "st-stale"}
	heard := a.bridgeHeardAt(ctx)
	if heard == 0 {
		return s
	}
	s.Configured = true
	s.LastRead = a.clock(heard)
	silent := a.now().Sub(time.Unix(heard, 0))
	var out []string
	for _, n := range a.bridgeNetworks(ctx) {
		if n.State == "needs_login" {
			out = append(out, n.Network)
			s.Networks = append(s.Networks, sourceLabel(n.Network))
		}
	}
	switch {
	case silent > a.bridgeSilence(ctx):
		s.Status, s.Cls, s.Alert, s.Stopped, s.Silent = statusError, "st-error", true, true, silent
		s.kind, s.episode = "silent", fmt.Sprintf("silent:%d", heard)
		s.Error = fmt.Sprintf("The Mac has not been heard from for %s.", silentText(silent))
		s.ErrorAt = a.clock(a.now().Unix())
	case len(out) > 0:
		s.Status, s.Cls, s.Alert, s.Stopped = statusLogin, "st-error", true, true
		s.kind, s.episode = "login", "login:"+strings.Join(out, ",")
		s.Error = strings.Join(s.Networks, ", ") + " needs a new login. Log in again in Beeper on the Mac."
		s.ErrorAt = a.clock(heard)
	default:
		s.Status, s.Cls = statusOK, "st-ok"
		if e := a.store.Setting(ctx, keyBridgeErr); e != "" {
			at, _ := strconv.ParseInt(a.store.Setting(ctx, keyBridgeErrAt), 10, 64)
			s.Error, s.ErrorAt = e, a.clock(at)
			// The Mac is heard but cannot read the chats: nothing arrives, so the
			// source is not fine. Beeper refusing the bridge's token is a login the
			// Mac cannot repair alone; any other lasting error stops the source.
			// Both tell the phone once (the episode is when the words last changed).
			switch {
			case strings.Contains(e, bridgeTokenRefused):
				s.Status, s.Cls, s.Alert, s.Stopped = statusLogin, "st-error", true, true
				s.kind, s.episode = "login", fmt.Sprintf("login:beeper:%d", at)
			case at > 0 && a.now().Sub(time.Unix(at, 0)) > minStale:
				s.Status, s.Cls, s.Alert, s.Stopped = statusError, "st-error", true, true
				s.kind, s.episode = "stopped", fmt.Sprintf("failed:beeper:%d", at)
			}
		}
	}
	return s
}
