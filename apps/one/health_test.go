package main

import (
	"net"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

// Tests of SPEC 24: a failed source is never silent. The notices are
// captured in place of the phone. All data is invented.

type noticeLog struct {
	mu   sync.Mutex
	list [][3]string
}

func (n *noticeLog) add(title, body, link string) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.list = append(n.list, [3]string{title, body, link})
	return nil
}

func (n *noticeLog) all() [][3]string {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([][3]string(nil), n.list...)
}

func (e *env) captureNotices() *noticeLog {
	n := &noticeLog{}
	e.a.notify = n.add
	return n
}

// deadAddress is a mail server that is not there.
func deadAddress(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close()
	return addr
}

func (e *env) advance(d time.Duration) { e.skew.Add(int64(d / time.Second)) }

func (e *env) strips() []string { return alertsFor(e.a.sourceStates(e.ctx)) }

func (e *env) indexPage() string { return get(e.h, "/").Body.String() }

func TestFailedImportShowsTheStripAndSendsOneNoticeAndRecoveryClearsIt(t *testing.T) {
	e := newEnv(t)
	got := e.captureNotices()
	good := e.a.gmail.IMAPAddr
	e.a.gmail.IMAPAddr = deadAddress(t)

	if page := e.indexPage(); strings.Contains(page, "has stopped") || !strings.Contains(page, "Connectors: all fine") {
		t.Fatal("a source that never failed shows a failure")
	}
	for i := 1; i <= 2; i++ {
		e.advance(time.Minute)
		if err := e.a.imp.RunSync(e.ctx); err == nil {
			t.Fatal("a sync against nothing worked")
		}
		if len(e.strips()) != 0 || len(got.all()) != 0 {
			t.Fatalf("after %d failures: strip %v, %d noticeLog", i, e.strips(), len(got.all()))
		}
	}
	// The third failed fetch in a row turns it red and sends one notice.
	e.advance(time.Minute)
	e.a.imp.RunSync(e.ctx)
	if s := e.strips(); len(s) != 1 || s[0] != "Gmail has stopped. See Connectors." {
		t.Fatalf("strip %v", s)
	}
	page := e.indexPage()
	if !strings.Contains(page, "Gmail has stopped. See Connectors.") || !strings.Contains(page, "Connectors: a source has stopped") {
		t.Errorf("the list page does not show the failure:\n%s", page)
	}
	if n := got.all(); len(n) != 1 || n[0][0] != "Gmail has stopped" {
		t.Fatalf("noticeLog %v", n)
	}
	// A fourth and a fifth failure of the same kind send none.
	for i := 0; i < 2; i++ {
		e.advance(time.Minute)
		e.a.imp.RunSync(e.ctx)
	}
	if n := got.all(); len(n) != 1 {
		t.Errorf("%d noticeLog for one failure", len(n))
	}
	// The Connectors page says it, with the error and when.
	conn := get(e.h, "/connectors").Body.String()
	if !strings.Contains(conn, ">Error<") || !strings.Contains(conn, "could not be reached") {
		t.Errorf("the Connectors page lacks the state and the error:\n%s", conn)
	}
	// The list state the page polls carries the strip.
	if st := get(e.h, "/list/state").Body.String(); !strings.Contains(st, `"alerts":["Gmail has stopped. See Connectors."]`) {
		t.Errorf("state: %s", st)
	}
	// Recovery: one good fetch clears the strip, the dot and the state.
	e.a.gmail.IMAPAddr = good
	e.advance(time.Minute)
	if err := e.a.imp.RunSync(e.ctx); err != nil {
		t.Fatal(err)
	}
	if len(e.strips()) != 0 {
		t.Errorf("the strip stays after recovery: %v", e.strips())
	}
	if page := e.indexPage(); strings.Contains(page, "has stopped") || !strings.Contains(page, "Connectors: all fine") {
		t.Error("the page still shows the failure")
	}
	if conn := get(e.h, "/connectors").Body.String(); strings.Contains(conn, ">Error<") || !strings.Contains(conn, ">OK<") || !strings.Contains(conn, "Last read:") {
		t.Error("the Connectors page does not show OK after recovery")
	}
	// A new failure after recovery is a new failure: one more notice, after three.
	e.a.gmail.IMAPAddr = deadAddress(t)
	for i := 0; i < 3; i++ {
		e.advance(time.Minute)
		e.a.imp.RunSync(e.ctx)
	}
	if n := got.all(); len(n) != 2 || n[1][0] != "Gmail has stopped" {
		t.Errorf("noticeLog after a second failure: %v", n)
	}
}

func TestNoticesCarryNoMailContent(t *testing.T) {
	e := newEnv(t)
	got := e.captureNotices()
	e.box.add(e.in("t1", "Secret offer for the kitchen", 1))
	if err := e.importMonth(); err != nil {
		t.Fatal(err)
	}
	e.a.gmail.IMAPAddr = deadAddress(t)
	for i := 0; i < 3; i++ {
		e.advance(time.Minute)
		e.a.imp.RunSync(e.ctx)
	}
	n := got.all()
	if len(n) != 1 {
		t.Fatalf("noticeLog %v", n)
	}
	for _, field := range n[0] {
		for _, bad := range []string{"Secret offer", "kitchen", "alice@", testAddr, "Hello", testPass} {
			if strings.Contains(field, bad) {
				t.Errorf("a notice holds %q: %v", bad, n[0])
			}
		}
	}
}

func TestSignInThatRanOutPausesSendsOneNoticeAndResumesAfterTheSignIn(t *testing.T) {
	e := newEnv(t)
	got := e.captureNotices()
	// The password Gmail holds is no longer accepted.
	if err := writeSecret(e.a.secretsDir, gmailSecretFile, gmailSecret{Address: testAddr, Password: "wrongwrongwrongw"}); err != nil {
		t.Fatal(err)
	}
	e.advance(time.Minute)
	e.a.imp.RunSync(e.ctx)
	if s := e.strips(); len(s) != 1 || s[0] != "Gmail needs login. See Connectors." {
		t.Fatalf("strip %v", s)
	}
	if n := got.all(); len(n) != 1 || n[0][0] != "Gmail needs login" {
		t.Fatalf("noticeLog %v", n)
	}
	conn := get(e.h, "/connectors").Body.String()
	if !strings.Contains(conn, ">Needs login<") || !strings.Contains(conn, "refused the app password") {
		t.Errorf("the Connectors page:\n%s", conn)
	}
	if !strings.Contains(e.indexPage(), "Connectors: a source has stopped") {
		t.Error("the dot does not say it")
	}
	// The timer pauses: it makes no more attempts.
	runs := e.a.store.CountRows(e.ctx, "sync_runs")
	for i := 0; i < 3; i++ {
		if err := e.a.imp.Tick(e.ctx); err != errNeedsLogin {
			t.Fatalf("tick: %v", err)
		}
	}
	if e.a.store.CountRows(e.ctx, "sync_runs") != runs {
		t.Error("a paused source was tried again")
	}
	if len(got.all()) != 1 {
		t.Error("the pause sent more noticeLog")
	}
	// The owner signs in again with the right password: fetching resumes by itself.
	rec := post(e.h, "/connectors/gmail", url.Values{"address": {testAddr}, "password": {testPass}})
	if rec.Code != 303 {
		t.Fatalf("connect: %d %s", rec.Code, rec.Body.String())
	}
	if len(e.strips()) != 0 {
		t.Error("the strip stays after the sign-in")
	}
	e.advance(time.Minute)
	if err := e.a.imp.Tick(e.ctx); err != nil {
		t.Fatalf("tick after the sign-in: %v", err)
	}
	if e.a.store.CountRows(e.ctx, "sync_runs") <= runs {
		t.Error("fetching did not resume")
	}
	if conn := get(e.h, "/connectors").Body.String(); strings.Contains(conn, ">Needs login<") {
		t.Error("still Needs login")
	}
	if len(got.all()) != 1 {
		t.Errorf("noticeLog %v", got.all())
	}
}

func TestTimerThatHasNotSyncedForTooLongIsStaleAndToldOnce(t *testing.T) {
	e := newEnv(t)
	got := e.captureNotices()
	if err := e.a.imp.RunSync(e.ctx); err != nil {
		t.Fatal(err)
	}
	e.a.checkSources(e.ctx)
	if len(e.strips()) != 0 {
		t.Fatal("a fresh sync is stale")
	}
	e.advance(14 * time.Minute)
	if len(e.strips()) != 0 {
		t.Error("stale before three intervals")
	}
	e.advance(2 * time.Minute) // 16 minutes: more than three intervals of 5
	s := e.strips()
	if len(s) != 1 || s[0] != "Gmail has not been read for 16 minutes. See Connectors." {
		t.Fatalf("strip %v", s)
	}
	page := e.indexPage()
	if !strings.Contains(page, "Connectors: needs a look") {
		t.Errorf("the dot does not say it:\n%s", page)
	}
	if conn := get(e.h, "/connectors").Body.String(); !strings.Contains(conn, ">Stale<") {
		t.Error("the Connectors page does not say Stale")
	}
	e.a.checkSources(e.ctx)
	e.a.checkSources(e.ctx)
	if n := got.all(); len(n) != 1 || n[0][0] != "Gmail has stopped" {
		t.Errorf("noticeLog %v", n)
	}
	// The interval decides how long is too long.
	post(e.h, "/settings", url.Values{keyInterval: {"15"}})
	if len(e.strips()) != 0 {
		t.Error("with 15 minutes the same silence is fine")
	}
	// A sync ends it.
	post(e.h, "/settings", url.Values{keyInterval: {"5"}})
	if err := e.a.imp.RunSync(e.ctx); err != nil {
		t.Fatal(err)
	}
	e.a.checkSources(e.ctx)
	if len(e.strips()) != 0 {
		t.Error("a sync did not end it")
	}
}

func TestAtMostTwoStripsAndFailedSourcesComeFirst(t *testing.T) {
	e := newEnv(t)
	e.a.notify = func(string, string, string) error { return nil }
	e.box.add(e.in("t1", "Parcel", 1))
	if err := e.importMonth(); err != nil {
		t.Fatal(err)
	}
	// Claude has something waiting, which makes the third strip.
	if page := e.indexPage(); !strings.Contains(page, "waiting for Claude") {
		t.Fatal("no strip for Claude")
	}
	if err := writeSecret(e.a.secretsDir, gmailSecretFile, gmailSecret{Address: testAddr, Password: "wrongwrongwrongw"}); err != nil {
		t.Fatal(err)
	}
	e.advance(time.Minute)
	e.a.imp.RunSync(e.ctx)
	page := e.indexPage()
	if !strings.Contains(page, "Gmail needs login. See Connectors.") {
		t.Fatal("no strip for the source")
	}
	// One failed source and Claude's strip: both show, the failed source first.
	if i, j := strings.Index(page, "Gmail needs login"), strings.Index(page, `id="claude-strip"`); i < 0 || j < 0 || i > j {
		t.Error("the failed source does not come first")
	}
}

func TestConnectionsDotListsEachSourceOnHover(t *testing.T) {
	e := newEnv(t)
	page := e.indexPage()
	for _, want := range []string{`href="/connectors"`, "Connectors: all fine", `id="dot-pop"`, "Gmail", "Office mail"} {
		if !strings.Contains(page, want) {
			t.Errorf("the page lacks %q", want)
		}
	}
	if !strings.Contains(page, ">Not configured<") {
		t.Error("an unconfigured source is not listed as such")
	}
	if !strings.Contains(page, `<h1><a href="/">ONE</a></h1>`) {
		t.Error("the name ONE")
	}
}
