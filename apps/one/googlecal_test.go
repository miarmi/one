package main

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// Tests of the Google calendars, read through each calendar's secret address
// in iCal format (SPEC 15, 18, 20, 23, 24). Every address, name and event
// here is invented; Google is a fake that sits in front of the app's client,
// so nothing leaves this machine, and the clock is fixed.

const (
	gToken  = "0123456789abcdef0123456789abcdef" // the secret part of the invented address
	gTokenB = "fedcba9876543210fedcba9876543210"
	gPathA  = "/calendar/ical/klara%40example.test/private-" + gToken + "/basic.ics"
	gPathB  = "/calendar/ical/team%40example.test/private-" + gTokenB + "/basic.ics"
	gAddrA  = "https://calendar.google.com" + gPathA
	gAddrB  = "https://calendar.google.com" + gPathB
)

// --- a fake Google that records every request it gets ---

type gReq struct {
	Method, Host, Path, IfNoneMatch, IfModifiedSince string
}

type fakeGoogle struct {
	mu       sync.Mutex
	reqs     []gReq
	feeds    map[string]string // path -> iCalendar text
	etags    map[string]string
	status   map[string]int    // path -> a status to answer instead of the feed
	cut      map[string]bool   // the body ends in an error half way
	redirect map[string]string // path -> Location
	down     bool              // the connection fails; the client's error names the address
}

func newFakeGoogle() *fakeGoogle {
	return &fakeGoogle{feeds: map[string]string{}, etags: map[string]string{}, status: map[string]int{}, cut: map[string]bool{}, redirect: map[string]string{}}
}

func (f *fakeGoogle) set(path, ics string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.feeds[path] = ics
}

func (f *fakeGoogle) requests() []gReq {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]gReq(nil), f.reqs...)
}

type cutBody struct {
	data []byte
	read bool
}

func (c *cutBody) Read(p []byte) (int, error) {
	if c.read {
		return 0, io.ErrUnexpectedEOF
	}
	c.read = true
	return copy(p, c.data), nil
}
func (c *cutBody) Close() error { return nil }

func (f *fakeGoogle) RoundTrip(r *http.Request) (*http.Response, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reqs = append(f.reqs, gReq{r.Method, r.URL.Host, r.URL.EscapedPath(), r.Header.Get("If-None-Match"), r.Header.Get("If-Modified-Since")})
	if f.down {
		return nil, &url.Error{Op: "Get", URL: r.URL.String(), Err: errors.New("connection refused")}
	}
	path := r.URL.EscapedPath()
	h := http.Header{}
	reply := func(code int, body io.ReadCloser) (*http.Response, error) {
		return &http.Response{StatusCode: code, Status: fmt.Sprint(code), Header: h, Body: body, Request: r, ContentLength: -1, Proto: "HTTP/1.1", ProtoMajor: 1, ProtoMinor: 1}, nil
	}
	empty := io.NopCloser(strings.NewReader(""))
	if loc, ok := f.redirect[path]; ok {
		h.Set("Location", loc)
		return reply(302, empty)
	}
	if st, ok := f.status[path]; ok {
		return reply(st, empty)
	}
	body, ok := f.feeds[path]
	if !ok {
		return reply(404, empty)
	}
	if tag := f.etags[path]; tag != "" {
		h.Set("ETag", tag)
		if r.Header.Get("If-None-Match") == tag {
			return reply(304, empty)
		}
	}
	if f.cut[path] {
		return reply(200, &cutBody{data: []byte(body[:len(body)/2])})
	}
	return reply(200, io.NopCloser(strings.NewReader(body)))
}

// --- the setting ---

type gEnv struct {
	*calEnv
	g *fakeGoogle
}

func newGoogleEnv(t *testing.T) *gEnv {
	t.Helper()
	e := &gEnv{calEnv: newCalEnv(t), g: newFakeGoogle()}
	e.a.googleHTTP = &http.Client{Transport: e.g}
	return e
}

func (e *gEnv) connect(name, addr string) {
	e.t.Helper()
	rec := post(e.h, "/connectors/google", url.Values{"name": {name}, "address": {addr}})
	if rec.Code != http.StatusSeeOther {
		e.t.Fatalf("connect %q: %d %s", name, rec.Code, rec.Body.String())
	}
}

func (e *gEnv) read() error { return e.a.syncGoogleCal(e.ctx, false) }

func (e *gEnv) mustRead() {
	e.t.Helper()
	if err := e.read(); err != nil {
		e.t.Fatalf("read Google: %v", err)
	}
}

func (e *gEnv) connectorsPage() string { return get(e.h, "/connectors").Body.String() }

func logText(t *testing.T) string {
	t.Helper()
	b, _ := os.ReadFile(logFile)
	return string(b)
}

func gevent(uid, title, start, end string, extra ...string) string {
	return veventText(append([]string{"UID:" + uid, "SUMMARY:" + title, "DTSTART:" + start, "DTEND:" + end}, extra...)...)
}

// --- the address ---

func TestGoogleAddressOnlyTheAllowedShapeAndHostIsAccepted(t *testing.T) {
	good := []string{
		gAddrA,
		"  " + gAddrA + "\n",
		"https://CALENDAR.google.com/calendar/ical/x%40example.test/public/basic.ics",
	}
	for _, s := range good {
		if _, _, ok := parseGoogleAddress(s); !ok {
			t.Errorf("a good address was refused: shape %q", strings.ReplaceAll(s, gToken, "…"))
		}
	}
	bad := map[string]string{
		"http":              "http://calendar.google.com" + gPathA,
		"another host":      "https://calendar.google.com.evil.example.test" + gPathA,
		"a look-alike host": "https://evil.example.test/calendar.google.com" + gPathA,
		"a sub host":        "https://www.calendar.google.com" + gPathA,
		"sign-in data":      "https://calendar.google.com@evil.example.test" + gPathA,
		"a port":            "https://calendar.google.com:8443" + gPathA,
		"a query":           gAddrA + "?x=1",
		"a fragment":        gAddrA + "#x",
		"not basic.ics":     "https://calendar.google.com/calendar/ical/klara%40example.test/private-" + gToken + "/full.ics",
		"another path":      "https://calendar.google.com/calendar/u/0/r",
		"too deep":          "https://calendar.google.com/calendar/ical/a/b/c/basic.ics",
		"a dot dot":         "https://calendar.google.com/calendar/ical/../x/basic.ics",
		"empty":             "",
		"two addresses":     gAddrA + " " + gAddrB,
		"ftp":               "ftp://calendar.google.com" + gPathA,
	}
	for what, s := range bad {
		if _, _, ok := parseGoogleAddress(s); ok {
			t.Errorf("%s: accepted", what)
		}
	}
	// Through the page: a wrong host is refused with the plain sentence, nothing is asked of anyone,
	// nothing is kept, and what was typed is not shown back.
	e := newGoogleEnv(t)
	typed := "https://evil.example.test/calendar/ical/x%40example.test/private-" + gToken + "/basic.ics"
	rec := post(e.h, "/connectors/google", url.Values{"name": {"Mine"}, "address": {typed}})
	body := rec.Body.String()
	if rec.Code != 400 || !strings.Contains(body, "That is not the secret address in iCal format.") || strings.Contains(body, gToken) {
		t.Errorf("a wrong host: %d", rec.Code)
	}
	if len(e.g.requests()) != 0 {
		t.Error("a refused address was still asked for")
	}
	if _, err := os.Stat(filepath.Join(e.a.secretsDir, googleSecretFile)); err == nil || len(e.a.googleCals(e.ctx)) != 0 {
		t.Error("a refused address was kept")
	}
	// A name is needed.
	rec = post(e.h, "/connectors/google", url.Values{"name": {"  "}, "address": {gAddrA}})
	if rec.Code != 400 || !strings.Contains(rec.Body.String(), "Give the calendar a name") || len(e.g.requests()) != 0 {
		t.Errorf("no name: %d", rec.Code)
	}
}

func TestGoogleRedirectToAnotherHostIsNotFollowed(t *testing.T) {
	e := newGoogleEnv(t)
	e.g.redirect[gPathA] = "https://evil.example.test/steal/basic.ics"
	rec := post(e.h, "/connectors/google", url.Values{"name": {"Mine"}, "address": {gAddrA}})
	if rec.Code != 400 || !strings.Contains(rec.Body.String(), "which ONE does not follow") {
		t.Errorf("connect through a redirect: %d %s", rec.Code, rec.Body.String())
	}
	for _, r := range e.g.requests() {
		if r.Host != googleHost {
			t.Errorf("a request went to %s", r.Host)
		}
	}
	if len(e.g.requests()) != 1 {
		t.Errorf("%d requests, want the one", len(e.g.requests()))
	}
	// The same on a read of a calendar that is kept: a redirect to another host, and one to plain http.
	e.g.redirect = map[string]string{}
	e.g.set(gPathA, vcal(gevent("u1", "Dentist", "20261120T080000Z", "20261120T090000Z")))
	e.connect("Mine", gAddrA)
	for _, loc := range []string{"https://evil.example.test/x/basic.ics", "http://calendar.google.com/x/basic.ics", "https://calendar.google.com:8443/x/basic.ics"} {
		e.g.redirect[gPathA] = loc
		err := e.read()
		if err == nil || !strings.Contains(plainGoogle(err), "which ONE does not follow") {
			t.Errorf("a redirect to %s: %v", loc, err)
		}
	}
	for _, r := range e.g.requests() {
		if r.Host != googleHost || r.Method != "GET" {
			t.Errorf("a request %s to %s", r.Method, r.Host)
		}
	}
	if len(e.byTitle("Dentist")) != 1 || e.byTitle("Dentist")[0].Status != "confirmed" {
		t.Error("a refused redirect changed the calendar")
	}
	// A redirect inside calendar.google.com is followed.
	e.g.redirect = map[string]string{gPathA: "https://calendar.google.com/moved/basic.ics"}
	e.g.set("/moved/basic.ics", vcal(gevent("u1", "Dentist", "20261120T080000Z", "20261120T090000Z"), gevent("u2", "Checkup", "20261121T080000Z", "20261121T090000Z")))
	e.mustRead()
	if len(e.byTitle("Checkup")) != 1 {
		t.Error("a redirect inside Google was not followed")
	}
}

func TestGoogleAddressNeverAppearsInALogAnErrorTheStoredRunsOrAnyPage(t *testing.T) {
	e := newGoogleEnv(t)
	told := e.captureNotices()
	e.g.set(gPathA, vcal(gevent("u1", "Dentist", "20261120T080000Z", "20261120T090000Z")))
	e.connect("Klara private", gAddrA)

	// The file on disk is the one place it lives.
	st, err := os.Stat(filepath.Join(e.a.secretsDir, googleSecretFile))
	if err != nil || st.Mode().Perm()&0o077 != 0 {
		t.Fatalf("secret file: %v %v", err, st)
	}
	if b, _ := os.ReadFile(filepath.Join(e.a.secretsDir, googleSecretFile)); !strings.Contains(string(b), gToken) {
		t.Error("the address is not in the secrets file")
	}
	page := e.connectorsPage()
	if !strings.Contains(page, "Klara private") || !strings.Contains(page, "…"+gToken[len(gToken)-4:]) {
		t.Error("the page lacks the name or the last characters")
	}
	// Every way a read can fail, three times each so the notice goes out.
	fail := []func(){
		func() { e.g.status[gPathA] = 500 },
		func() { delete(e.g.status, gPathA); e.g.down = true },
		func() { e.g.down = false; e.g.status[gPathA] = 404 },
		func() { e.g.status[gPathA] = 403 },
		func() {
			delete(e.g.status, gPathA)
			e.g.redirect[gPathA] = "https://evil.example.test/" + gToken + "/basic.ics"
		},
		func() { delete(e.g.redirect, gPathA); e.g.cut[gPathA] = true },
	}
	var errs []string
	for _, f := range fail {
		f()
		for i := 0; i < 3; i++ {
			if err := e.read(); err != nil {
				errs = append(errs, err.Error(), plainGoogle(err))
			}
		}
	}
	if len(errs) == 0 {
		t.Fatal("no read failed")
	}
	// And a connect that fails.
	rec := post(e.h, "/connectors/google", url.Values{"name": {"Second"}, "address": {gAddrB}})
	errs = append(errs, rec.Body.String())
	e.a.record(e.ctx, googleConn, false, "Could not read: "+plainGoogle(errors.New("x")))
	post(e.h, "/connectors/google/test", nil)

	runs, _ := e.a.store.RecentRuns(e.ctx, srcGoogleCal, 100)
	for _, r := range runs {
		errs = append(errs, r.Error)
	}
	for _, n := range told.all() {
		errs = append(errs, n[0], n[1], n[2])
	}
	for name, body := range map[string]string{
		"connectors": e.connectorsPage(), "list": e.indexPage(), "agenda": e.agenda(), "settings": get(e.h, "/settings").Body.String(),
		"api": e.api("GET", "/api/agenda", nil).Body.String(), "log": logText(t), "errors": strings.Join(errs, "\n"),
	} {
		for _, secret := range []string{gToken, gTokenB, gAddrA, "klara%40example.test/private"} {
			if strings.Contains(body, secret) {
				t.Errorf("the %s holds the address", name)
			}
		}
	}
	// A preview is never given the secrets folder: the address is only under secrets/.
	if filepath.Base(e.a.secretsDir) != "secrets" {
		t.Error("the secrets are not in the secrets folder")
	}
}

// --- reading ---

func TestGoogleRepeatReadWritesNothingAndOnlyEverGetsTheSavedAddress(t *testing.T) {
	e := newGoogleEnv(t)
	e.g.set(gPathA, vcal(
		gevent("u1", "Dentist", "20261120T080000Z", "20261120T090000Z", "LOCATION:Main Street 1"),
		veventText("UID:u2", "SUMMARY:Standup", "DTSTART:20261123T080000Z", "DTEND:20261123T081500Z", "RRULE:FREQ=DAILY;COUNT=3")))
	e.connect("Mine", gAddrA)
	before := e.counts()
	if before[0] != 4 {
		t.Fatalf("expected 4 entries (1 + 3), got %d", before[0])
	}
	e.mustRead()
	e.mustRead()
	if after := e.counts(); after != before {
		t.Errorf("a repeat read wrote: %v -> %v", before, after)
	}
	// With a validator from the server the next read asks "unchanged?" and nothing is written either.
	e.g.etags[gPathA] = `"v1"`
	e.mustRead() // learns the validator
	n := len(e.g.requests())
	e.mustRead()
	reqs := e.g.requests()
	if len(reqs) != n+1 || reqs[n].IfNoneMatch != `"v1"` {
		t.Errorf("the read did not send its validator: %+v", reqs[n:])
	}
	if after := e.counts(); after != before {
		t.Errorf("an unchanged answer wrote: %v -> %v", before, after)
	}
	// When the window has moved to another day, the read is whole again.
	e.now = e.now.Add(24 * time.Hour)
	n = len(e.g.requests())
	e.mustRead()
	if r := e.g.requests()[n]; r.IfNoneMatch != "" {
		t.Error("a moved window was read with a validator")
	}
	// The one request there ever is: a GET of the saved address.
	for _, r := range e.g.requests() {
		if r.Method != "GET" || r.Host != googleHost || r.Path != gPathA {
			t.Errorf("request %s %s%s", r.Method, r.Host, "…")
		}
	}
}

func TestGoogleChangedEventIsOneEntryWithANewVersion(t *testing.T) {
	e := newGoogleEnv(t)
	e.g.set(gPathA, vcal(gevent("u1", "Dentist", "20261120T080000Z", "20261120T090000Z")))
	e.connect("Mine", gAddrA)
	e.g.set(gPathA, vcal(gevent("u1", "Dentist at the new clinic", "20261120T100000Z", "20261120T110000Z", "LOCATION:Elm Street 2")))
	e.mustRead()
	if len(e.byTitle("Dentist")) != 0 {
		t.Error("the old title is still an entry of its own")
	}
	rows := e.byTitle("Dentist at the new clinic")
	if len(rows) != 1 || rows[0].Version != 2 || rows[0].Place != "Elm Street 2" || e.a.store.CountEvents(e.ctx, srcGoogleCal) != 1 {
		t.Fatalf("after a change: %+v", rows)
	}
	if n := e.a.store.CountRows(e.ctx, "agenda_versions"); n != 2 {
		t.Errorf("%d versions, want 2", n)
	}
	// It comes back as it was: version 3, still one entry.
	e.g.set(gPathA, vcal(gevent("u1", "Dentist", "20261120T080000Z", "20261120T090000Z")))
	e.mustRead()
	if rows := e.byTitle("Dentist"); len(rows) != 1 || rows[0].Version != 3 {
		t.Errorf("after the change back: %+v", rows)
	}
	// A cancelled event is kept and marked.
	e.g.set(gPathA, vcal(gevent("u1", "Dentist", "20261120T080000Z", "20261120T090000Z", "STATUS:CANCELLED")))
	e.mustRead()
	if rows := e.byTitle("Dentist"); len(rows) != 1 || rows[0].Status != "cancelled" || rows[0].Version != 4 {
		t.Errorf("after a cancellation: %+v", rows)
	}
}

func TestGoogleEventGoneFromACompleteFeedIsRemovedButAFailedOrCutFetchRemovesNothing(t *testing.T) {
	e := newGoogleEnv(t)
	both := vcal(gevent("u1", "Dentist", "20261120T080000Z", "20261120T090000Z"), gevent("u2", "Checkup", "20261121T080000Z", "20261121T090000Z"))
	e.g.set(gPathA, both)
	e.connect("Mine", gAddrA)
	if e.a.store.CountEvents(e.ctx, srcGoogleCal) != 2 {
		t.Fatal("setup")
	}
	still := func(what string) {
		t.Helper()
		for _, title := range []string{"Dentist", "Checkup"} {
			if rows := e.byTitle(title); len(rows) != 1 || rows[0].Status != "confirmed" || rows[0].Version != 1 {
				t.Errorf("%s: %s is %+v", what, title, rows)
			}
		}
	}
	// Failures, one after the other: nothing changes.
	for _, step := range []struct {
		what string
		set  func()
	}{
		{"a server error", func() { e.g.status[gPathA] = 503 }},
		{"an unknown feed", func() { delete(e.g.status, gPathA); e.g.status[gPathA] = 404 }},
		{"a refusal", func() { e.g.status[gPathA] = 403 }},
		{"no connection", func() { delete(e.g.status, gPathA); e.g.down = true }},
		{"a cut body", func() {
			e.g.down = false
			e.g.cut[gPathA] = true
		}},
		{"a feed that ends too soon", func() {
			e.g.cut[gPathA] = false
			e.g.set(gPathA, strings.TrimSuffix(vcal(gevent("u1", "Dentist", "20261120T080000Z", "20261120T090000Z")), "END:VCALENDAR\r\n"))
		}},
		{"an answer that is not a calendar", func() { e.g.set(gPathA, "<html>Sign in to continue</html>") }},
	} {
		set, what := step.set, step.what
		set()
		if err := e.read(); err == nil {
			t.Errorf("%s was not an error", what)
		}
		still(what)
	}
	// A complete feed without the checkup: it is removed, kept and marked, with a new version.
	delete(e.g.status, gPathA)
	e.g.set(gPathA, vcal(gevent("u1", "Dentist", "20261120T080000Z", "20261120T090000Z")))
	e.mustRead()
	rows := e.byTitle("Checkup")
	if len(rows) != 1 || rows[0].Status != "removed" || rows[0].Version != 2 {
		t.Fatalf("a vanished event: %+v", rows)
	}
	if rows := e.byTitle("Dentist"); len(rows) != 1 || rows[0].Status != "confirmed" || rows[0].Version != 1 {
		t.Errorf("the other event: %+v", rows)
	}
	if !strings.Contains(e.agenda(), "Checkup") || !strings.Contains(e.agenda(), "Removed") {
		t.Error("the agenda does not show the removed event as removed")
	}
	// A repeat read writes nothing more.
	c := e.counts()
	e.mustRead()
	if e.counts() != c {
		t.Error("a repeat read wrote after the removal")
	}
}

func TestGoogleRecurringAndAllDayAndLateEventsLandOnTheRightPragueDays(t *testing.T) {
	e := newGoogleEnv(t)
	e.g.set(gPathA, vcal(
		veventText("UID:weekly", "SUMMARY:Team sync", "DTSTART;TZID=Europe/Prague:20261120T090000", "DTEND;TZID=Europe/Prague:20261120T100000", "RRULE:FREQ=WEEKLY;COUNT=3"),
		veventText("UID:holiday", "SUMMARY:Day off", "DTSTART;VALUE=DATE:20261124", "DTEND;VALUE=DATE:20261125"),
		veventText("UID:trip", "SUMMARY:Trip", "DTSTART;VALUE=DATE:20261126", "DTEND;VALUE=DATE:20261129"),
		veventText("UID:late", "SUMMARY:Late show", "DTSTART;TZID=Europe/Prague:20261121T233000", "DTEND;TZID=Europe/Prague:20261122T003000"),
		veventText("UID:utc", "SUMMARY:Call", "DTSTART:20261130T233000Z", "DTEND:20261201T000000Z"),
	))
	e.connect("Mine", gAddrA)
	day := func(unix int64) string { return time.Unix(unix, 0).In(e.a.loc).Format("2006-01-02 15:04") }
	var got []string
	for _, r := range e.byTitle("Team sync") {
		got = append(got, day(r.StartAt))
	}
	if strings.Join(got, ",") != "2026-11-20 09:00,2026-11-27 09:00,2026-12-04 09:00" {
		t.Errorf("the repeating event: %v", got)
	}
	if rows := e.byTitle("Day off"); len(rows) != 1 || !rows[0].AllDay || rows[0].StartDate != "2026-11-24" || rows[0].EndDate != "2026-11-25" {
		t.Errorf("the all-day event: %+v", rows)
	}
	if rows := e.byTitle("Trip"); len(rows) != 1 || rows[0].StartDate != "2026-11-26" || rows[0].EndDate != "2026-11-29" {
		t.Errorf("the three-day event: %+v", rows)
	}
	if rows := e.byTitle("Late show"); len(rows) != 1 || day(rows[0].StartAt) != "2026-11-21 23:30" {
		t.Errorf("the late event: %+v", rows)
	}
	if rows := e.byTitle("Call"); len(rows) != 1 || day(rows[0].StartAt) != "2026-12-01 00:30" {
		t.Errorf("the event in UTC just before midnight: %+v", rows)
	}
	// The agenda puts them under the right days.
	page := e.agenda()
	for _, want := range []string{"Fri 20 Nov", "Sat 21 Nov", "Tue 24 Nov", "Thu 26 Nov", "Fri 27 Nov", "Tue 1 Dec"} {
		if !strings.Contains(page, want) {
			t.Errorf("the agenda lacks the day %s", want)
		}
	}
}

func TestGoogleOversizedFeedIsRefusedWithTheStripAndOneNotice(t *testing.T) {
	old := googleFeedMax
	googleFeedMax = 3000
	t.Cleanup(func() { googleFeedMax = old })
	e := newGoogleEnv(t)
	told := e.captureNotices()
	e.g.set(gPathA, vcal(gevent("u1", "Dentist", "20261120T080000Z", "20261120T090000Z")))
	e.connect("Big one", gAddrA)
	var many []string
	for i := 0; i < 60; i++ {
		many = append(many, gevent(fmt.Sprintf("big%d", i), fmt.Sprintf("Event number %d", i), "20261120T080000Z", "20261120T090000Z"))
	}
	e.g.set(gPathA, vcal(many...))
	// A new calendar that is too large is refused when it is added.
	e.g.set(gPathB, vcal(many...))
	rec := post(e.h, "/connectors/google", url.Values{"name": {"Another"}, "address": {gAddrB}})
	if rec.Code != 400 || !strings.Contains(rec.Body.String(), "is larger than the") || len(e.a.googleCals(e.ctx)) != 1 {
		t.Errorf("a too large calendar was added: %d", rec.Code)
	}
	for i := 0; i < 4; i++ {
		if err := e.read(); err == nil || !errors.Is(err, errGoogleTooBig) {
			t.Fatalf("read %d: %v", i, err)
		}
	}
	strips := e.strips()
	if len(strips) != 1 || strips[0] != "Google Calendar has stopped. See Connectors." {
		t.Errorf("strip %v", strips)
	}
	if n := told.all(); len(n) != 1 || n[0][0] != "Google Calendar has stopped" {
		t.Errorf("notices %v", n)
	}
	if dot := dotFor(e.a.sourceStates(e.ctx)); dot.Cls != "dot-stopped" {
		t.Errorf("dot %v", dot)
	}
	if !strings.Contains(e.indexPage(), "Google Calendar has stopped. See Connectors.") {
		t.Error("the list page lacks the strip")
	}
	page := e.connectorsPage()
	if !strings.Contains(page, "Big one") || !strings.Contains(page, "is larger than the") || !strings.Contains(page, "Disconnect it here, or make a smaller calendar in Google.") || !strings.Contains(page, ">Error<") {
		t.Error("the Connectors page does not say it plainly")
	}
	if len(e.byTitle("Dentist")) != 1 || e.byTitle("Dentist")[0].Status != "confirmed" {
		t.Error("a refused feed changed what was read")
	}
	// It reads again by itself when it fits: the strip goes.
	e.g.set(gPathA, vcal(gevent("u1", "Dentist", "20261120T080000Z", "20261120T090000Z")))
	e.mustRead()
	if len(e.strips()) != 0 {
		t.Errorf("strips after a good read: %v", e.strips())
	}
}

func TestGoogleOtherCalendarsGoOnWhenOneFailsAndTheNoticeNamesIt(t *testing.T) {
	e := newGoogleEnv(t)
	e.g.set(gPathA, vcal(gevent("u1", "Dentist", "20261120T080000Z", "20261120T090000Z")))
	e.g.set(gPathB, vcal(gevent("t1", "Planning", "20261121T080000Z", "20261121T090000Z")))
	e.connect("Mine", gAddrA)
	e.connect("Team", gAddrB)
	e.g.set(gPathA, vcal(gevent("u1", "Dentist", "20261120T080000Z", "20261120T090000Z"), gevent("u3", "Surgery", "20261122T080000Z", "20261122T090000Z")))
	delete(e.g.feeds, gPathB) // the address was reset in Google
	err := e.read()
	if err == nil || !errors.Is(err, errGoogleGone) {
		t.Fatalf("read: %v", err)
	}
	if len(e.byTitle("Surgery")) != 1 {
		t.Error("the good calendar was not read because another failed")
	}
	if rows := e.byTitle("Planning"); len(rows) != 1 || rows[0].Status != "confirmed" {
		t.Error("the failing calendar lost its events")
	}
	text := plainGoogle(err)
	if !strings.Contains(text, "“Team”") || !strings.Contains(text, "reset") || !strings.Contains(text, "connect it again") {
		t.Errorf("the sentence: %s", text)
	}
}

// --- connecting and removing ---

func TestGoogleSecondCalendarCanBeAddedAndOneRemoved(t *testing.T) {
	e := newGoogleEnv(t)
	e.g.set(gPathA, vcal(gevent("u1", "Dentist", "20261120T080000Z", "20261120T090000Z")))
	e.g.set(gPathB, vcal(gevent("t1", "Planning", "20261121T080000Z", "20261121T090000Z")))
	if p := e.connectorsPage(); !strings.Contains(p, "Not configured") || !strings.Contains(p, "Secret address in iCal format") {
		t.Error("the empty section")
	}
	e.connect("Mine", gAddrA)
	e.connect("Team", gAddrB)
	page := e.connectorsPage()
	for _, want := range []string{"Mine", "Team", "Show in agenda", "Whose", "Test now", "Known limitations", "Disconnect", "Connected: <b>Mine</b>"} {
		if !strings.Contains(page, want) {
			t.Errorf("the page lacks %q", want)
		}
	}
	if len(e.byTitle("Dentist")) != 1 || len(e.byTitle("Planning")) != 1 {
		t.Fatal("the events of both calendars are not in the agenda")
	}
	// The same name or address twice is refused.
	if rec := post(e.h, "/connectors/google", url.Values{"name": {"mine"}, "address": {gAddrB}}); rec.Code != 400 || !strings.Contains(rec.Body.String(), "already a calendar with that name") {
		t.Errorf("the same name: %d", rec.Code)
	}
	if rec := post(e.h, "/connectors/google", url.Values{"name": {"Third"}, "address": {gAddrB}}); rec.Code != 400 || !strings.Contains(rec.Body.String(), "already connected") {
		t.Errorf("the same address: %d", rec.Code)
	}
	cals := e.a.googleCals(e.ctx)
	if len(cals) != 2 {
		t.Fatalf("%d calendars", len(cals))
	}
	// Remove the first: its events and address go, the other stays.
	if rec := post(e.h, "/connectors/google/disconnect", url.Values{"key": {cals[0].Key}}); rec.Code != http.StatusSeeOther {
		t.Fatalf("disconnect: %d", rec.Code)
	}
	if len(e.byTitle("Dentist")) != 0 || len(e.byTitle("Planning")) != 1 {
		t.Error("removing a calendar did not remove exactly its events")
	}
	if b, _ := os.ReadFile(filepath.Join(e.a.secretsDir, googleSecretFile)); strings.Contains(string(b), gToken) || !strings.Contains(string(b), gTokenB) {
		t.Error("the secrets file after the removal")
	}
	if p := e.connectorsPage(); strings.Contains(p, "Connected: <b>Mine</b>") || !strings.Contains(p, "Connected: <b>Team</b>") {
		t.Error("the page after the removal")
	}
	// A key that is not there changes nothing.
	post(e.h, "/connectors/google/disconnect", url.Values{"key": {"googlecal:nothing"}})
	if len(e.a.googleCals(e.ctx)) != 1 {
		t.Error("an unknown key removed a calendar")
	}
	// Remove the last: nothing is left, not even the file.
	post(e.h, "/connectors/google/disconnect", url.Values{"key": {e.a.googleCals(e.ctx)[0].Key}})
	if _, err := os.Stat(filepath.Join(e.a.secretsDir, googleSecretFile)); err == nil {
		t.Error("the secrets file is still there")
	}
	if e.a.store.CountEvents(e.ctx, srcGoogleCal) != 0 || !strings.Contains(e.connectorsPage(), "Not configured") {
		t.Error("the last removal")
	}
	if err := e.read(); !errors.Is(err, errCalOff) {
		t.Errorf("a read with nothing connected: %v", err)
	}
	if len(e.strips()) != 0 {
		t.Error("a strip for a calendar that is not connected")
	}
}

func TestGoogleLimitationsAreOnThePageAndTestNowReportsInPlainWords(t *testing.T) {
	e := newGoogleEnv(t)
	e.g.set(gPathA, vcal(gevent("u1", "Dentist", "20261120T080000Z", "20261120T090000Z")))
	e.connect("Mine", gAddrA)
	page := e.connectorsPage()
	for _, want := range []string{
		"ONE only reads.",
		"The address can lag behind the calendar",
		"Anyone who has the secret address can read the whole calendar.",
		"reset it in Google; ONE then stops reading that calendar until you connect it again with the new address.",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("a limitation is missing: %q", want)
		}
	}
	if rec := post(e.h, "/connectors/google/test", nil); rec.Code != http.StatusSeeOther {
		t.Fatalf("test: %d", rec.Code)
	}
	if p := e.connectorsPage(); !strings.Contains(p, "Read 1 calendar shown in the agenda. 1 entries in the agenda now.") || !strings.Contains(p, "Last test:") {
		t.Error("Test now does not report")
	}
	e.g.status[gPathA] = 404
	post(e.h, "/connectors/google/test", nil)
	p := e.connectorsPage()
	if !strings.Contains(p, "Could not read: Google no longer knows the address of the calendar “Mine”") || !strings.Contains(p, ">Error<") {
		t.Error("Test now does not say what failed")
	}
	// A limitation does not turn the dot.
	delete(e.g.status, gPathA)
	e.mustRead()
	if dot := dotFor(e.a.sourceStates(e.ctx)); dot.Cls == "dot-stopped" {
		t.Errorf("dot %v", dot)
	}
	// Nothing to test before a calendar is connected.
	e2 := newGoogleEnv(t)
	if rec := post(e2.h, "/connectors/google/test", nil); rec.Code != 400 || !strings.Contains(rec.Body.String(), "Connect a Google calendar first.") {
		t.Errorf("test with nothing connected: %d", rec.Code)
	}
}

func TestGoogleTimerReadsItAfterTheOtherCalendars(t *testing.T) {
	e := newGoogleEnv(t)
	cal := e.dav.calendar("work", "Work")
	e.dav.put(cal, "a.ics", one("u1", "Dentist", "20261120T080000Z", "20261120T090000Z"))
	e.connectICloud()
	e.g.set(gPathA, vcal(gevent("g1", "Planning", "20261121T080000Z", "20261121T090000Z")))
	e.connect("Mine", gAddrA)
	n := len(e.g.requests())
	e.a.syncCalendars(e.ctx)
	if len(e.g.requests()) != n+1 {
		t.Error("the timer did not read Google")
	}
	if len(e.byTitle("Dentist")) != 1 || len(e.byTitle("Planning")) != 1 {
		t.Error("the timer's read lost an entry")
	}
	// Show in agenda off: the calendar is not read and its entries are not listed, but stay stored.
	cals := e.a.googleCals(e.ctx)
	post(e.h, "/connectors/google/calendars", url.Values{"key0": {cals[0].Key}, "whose0": {"0"}})
	n = len(e.g.requests())
	e.a.syncCalendars(e.ctx)
	if len(e.g.requests()) != n {
		t.Error("a calendar that is not shown was read")
	}
	if strings.Contains(e.agenda(), "Planning") || len(e.byTitle("Planning")) != 1 {
		t.Error("a calendar that is not shown")
	}
}

func TestTheSameEventInGoogleAndICloudIsShownOnce(t *testing.T) {
	e := newGoogleEnv(t)
	cal := e.dav.calendar("work", "Work")
	e.dav.put(cal, "a.ics", one("shared-1", "Planning meeting", "20261121T080000Z", "20261121T090000Z"))
	e.dav.put(cal, "b.ics", one("icloud-only", "Dentist", "20261122T080000Z", "20261122T090000Z"))
	e.connectICloud()
	e.readICloud()
	e.g.set(gPathA, vcal(gevent("shared-1", "Planning meeting", "20261121T080000Z", "20261121T090000Z"), gevent("google-only", "Surgery", "20261123T080000Z", "20261123T090000Z")))
	e.connect("Team", gAddrA)
	if len(e.byTitle("Planning meeting")) != 2 {
		t.Fatal("both copies should stay stored")
	}
	page := e.agenda()
	if n := strings.Count(page, "Planning meeting"); n != 1 {
		t.Errorf("the event is shown %d times", n)
	}
	if !strings.Contains(page, "Google · Team") || !strings.Contains(page, "iCloud · Work") {
		t.Error("the entry does not name both calendars")
	}
	from, to := e.now.AddDate(0, 0, -30), e.now.AddDate(0, 0, 30)
	evs, _, err := e.a.agendaEntries(e.ctx, agendaQuery{From: from, To: to})
	if err != nil {
		t.Fatal(err)
	}
	found := 0
	for _, ev := range evs {
		if ev["title"] == "Planning meeting" {
			found++
			if cs, _ := ev["calendars"].([]string); len(cs) != 2 {
				t.Errorf("calendars %v", ev["calendars"])
			}
		}
	}
	if found != 1 {
		t.Errorf("an agent sees the event %d times", found)
	}
	if strings.Count(page, "Surgery") != 1 || strings.Count(page, "Dentist") != 1 {
		t.Error("the events of one calendar are not shown once each")
	}
}
