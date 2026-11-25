package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseJournalLine(t *testing.T) {
	// A JSON line as appkit writes it, wrapped the way journalctl prints it.
	l, ok := parseJournalLine(`2026-11-09T10:15:30+0100 lab app[1234]: {"time":"2026-11-09T09:15:30.123Z","level":"ERROR","msg":"saving note","app":"hello","err":"database is locked","rid":"abc123"}`)
	if !ok || l.Level != "ERROR" || l.Msg != "saving note" || l.Err != "database is locked" || l.RID != "abc123" {
		t.Fatalf("json line: %+v", l)
	}
	if want := time.Date(2026, 11, 9, 9, 15, 30, 123000000, time.UTC); !l.Time.Equal(want) {
		t.Errorf("time from the line itself: %v", l.Time)
	}
	// A request line gets a readable message.
	l, _ = parseJournalLine(`{"time":"2026-11-09T09:15:30Z","level":"WARN","msg":"request","method":"GET","path":"/x","status":404}`)
	if l.Level != "WARN" || l.Msg != "GET /x → 404" {
		t.Errorf("request line: %+v", l)
	}
	// A bare line: journald's time, level guessed from the words.
	l, ok = parseJournalLine(`2026-11-09T10:15:30+0100 lab app[1234]: panic: runtime error: index out of range`)
	if !ok || l.Level != "ERROR" || !strings.HasPrefix(l.Msg, "panic:") || l.Time.IsZero() {
		t.Errorf("panic line: %+v", l)
	}
	l, _ = parseJournalLine(`2026-11-09T10:15:30+0100 lab app[1234]: hello listening on 127.0.0.1:20001`)
	if l.Level != "INFO" || l.Msg != "hello listening on 127.0.0.1:20001" {
		t.Errorf("plain line: %+v", l)
	}
	l, _ = parseJournalLine(`template: parsing error at line 3`)
	if l.Level != "ERROR" || !l.Time.IsZero() {
		t.Errorf("hand-written error without a time: %+v", l)
	}
	for _, skip := range []string{"", "-- No entries --", "-- Boot 1234 --"} {
		if _, ok := parseJournalLine(skip); ok {
			t.Errorf("%q should be skipped", skip)
		}
	}
	// Odd levels.
	l, _ = parseJournalLine(`{"level":"warning","msg":"x"}`)
	if l.Level != "WARN" {
		t.Errorf("warning → WARN: %+v", l)
	}
	l, _ = parseJournalLine(`{"msg":"no level"}`)
	if l.Level != "INFO" {
		t.Errorf("no level → INFO: %+v", l)
	}
}

// journalWith builds a journal dump: n error lines per hour for the given
// hours ago (0 = the last hour), plus some info lines.
func journalWith(now time.Time, perHour map[int]int) string {
	var b strings.Builder
	for ago := 24; ago >= 0; ago-- {
		at := now.Add(-time.Duration(ago)*time.Hour - 30*time.Minute)
		fmt.Fprintf(&b, "%s lab app[1]: {\"time\":%q,\"level\":\"INFO\",\"msg\":\"request\",\"method\":\"GET\",\"path\":\"/\",\"status\":200}\n",
			at.Format("2006-01-02T15:04:05-0700"), at.UTC().Format(time.RFC3339Nano))
		for i := 0; i < perHour[ago]; i++ {
			at := at.Add(time.Duration(i) * time.Second)
			fmt.Fprintf(&b, "%s lab app[1]: {\"time\":%q,\"level\":\"ERROR\",\"msg\":\"saving note %d\",\"err\":\"database is locked\",\"rid\":\"r%d\"}\n",
				at.Format("2006-01-02T15:04:05-0700"), at.UTC().Format(time.RFC3339Nano), ago, i)
		}
	}
	return b.String()
}

func TestSummarizeAndErrorRate(t *testing.T) {
	now := time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)
	lines := parseJournal(journalWith(now, map[int]int{0: 12, 3: 2, 20: 1, 24: 40}))

	errs, warns, infos, recent := summarize(lines, now.Add(-24*time.Hour))
	// The hour 24 ago is before the window; 23 info lines inside it... one per
	// hour for hours 0..23 is 24 info lines.
	if errs != 15 || warns != 0 || infos != 24 {
		t.Errorf("summarize: %d errors, %d warnings, %d infos", errs, warns, infos)
	}
	if len(recent) != 15 || recent[0].Msg != "saving note 0" || !recent[0].Time.After(recent[len(recent)-1].Time) {
		t.Errorf("recent errors should be newest first: %+v", recent)
	}

	// The last hour has 12; the 24 hours before it include the hour 24 ago
	// (the alert looks 25 hours back): 2 + 1 + 40.
	lastHour, before, newest := errorRate(lines, now)
	if lastHour != 12 || before != 43 || len(newest) != 12 || newest[0].RID != "r11" {
		t.Errorf("errorRate: %d in the last hour, %d before; newest %+v", lastHour, before, newest)
	}

	// The threshold: 10 or more, and three times the usual hourly count
	// plus five, so a steady stream never alerts and a burst does.
	for _, c := range []struct {
		lastHour, before int
		want             bool
	}{
		{0, 0, false}, {9, 0, false}, {10, 0, true}, {12, 3, true},
		{12, 48, true},   // usual 2/h: 3*2+5 = 11 ≤ 12
		{10, 48, false},  // usual 2/h: 10 < 11
		{10, 240, false}, // usual 10/h: a steady stream
		{40, 240, true},
	} {
		if got := alertWorthy(c.lastHour, c.before); got != c.want {
			t.Errorf("alertWorthy(%d, %d) = %v, want %v", c.lastHour, c.before, got, c.want)
		}
	}
}

func TestErrorsPage(t *testing.T) {
	h := newHarness(t)
	code, _, raw := h.call("POST", "/api/settings", []byte(`{"domain":"","public_ip":"127.0.0.1","allow_ips":["`+homeIP+`"]}`), refMain)
	mustStatus(t, "bootstrap settings", code, 200, raw)
	now := time.Now()
	for _, inst := range []Instance{
		{App: "hello", Env: prodEnv, Title: "Hello", Port: 20001, Status: "running", Health: "/healthz", DeployedAt: now},
		{App: "hello", Env: "dark-mode", Title: "Hello", Port: 20002, Status: "running", Health: "/healthz", DeployedAt: now},
		{App: "quiet", Env: prodEnv, Title: "Quiet", Port: 20003, Status: "running", Health: "/healthz", DeployedAt: now},
	} {
		if err := h.srv.store.Put(inst); err != nil {
			t.Fatal(err)
		}
	}
	os.MkdirAll(filepath.Dir(h.runner.logPath("x")), 0o755)
	os.WriteFile(h.runner.logPath(instanceID("hello", prodEnv)), []byte(journalWith(now, map[int]int{0: 2, 5: 1})), 0o644)
	os.WriteFile(h.runner.logPath(instanceID("hello", "dark-mode")), []byte("2026-01-01T00:00:00+0000 lab app[1]: old error line\n"+journalWith(now, map[int]int{0: 1})), 0o644)
	code, _, raw = h.call("POST", "/api/settings", []byte(`{"domain":"`+testDomain+`","public_ip":"127.0.0.1","allow_ips":["`+homeIP+`"]}`), refMain)
	mustStatus(t, "domain settings", code, 200, raw)

	// Away and signed out: the sign-in page, and back here afterwards.
	resp, _ := h.visit(visit{method: "GET", path: "/_lab/errors"})
	if resp.StatusCode != 303 || resp.Header.Get("Location") != "/_lab/login?next=%2F_lab%2Ferrors" {
		t.Fatalf("errors away: %d %v", resp.StatusCode, resp.Header)
	}
	// At home: the page, production first, with the errors and the counts.
	resp, body := h.visit(visit{method: "GET", path: "/_lab/errors", ip: homeIP})
	if resp.StatusCode != 200 {
		t.Fatalf("errors at home: %d %s", resp.StatusCode, body)
	}
	for _, want := range []string{esc(textsEN.ErrorsTitle), "saving note 0", "database is locked", "<code>r1</code>", `3 errors`, `1 errors`, `0 errors`, "Quiet", esc(textsEN.NoErrors), "dark-mode", "hello." + testDomain} {
		if !strings.Contains(body, want) {
			t.Errorf("page lacks %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "old error line") {
		t.Error("an error from January is not from the last 24 hours")
	}
	if strings.Index(body, "Quiet") > strings.Index(body, "dark-mode") {
		t.Error("production apps should come before previews")
	}
	// Czech when the browser prefers it.
	_, body = h.visit(visit{method: "GET", path: "/_lab/errors", ip: homeIP, header: map[string]string{"Accept-Language": "cs"}})
	if !strings.Contains(body, esc(textsCS.ErrorsTitle)) || !strings.Contains(body, "chyb") {
		t.Errorf("czech page: %s", body)
	}
	// The home page links here.
	_, body = h.visit(visit{method: "GET", path: "/", ip: homeIP})
	if !strings.Contains(body, `href="/_lab/errors"`) {
		t.Errorf("home page has no link to the errors: %s", body)
	}
	// The page is cached for a minute: a new error does not show at once.
	os.WriteFile(h.runner.logPath(instanceID("quiet", prodEnv)), []byte(journalWith(now, map[int]int{0: 1})), 0o644)
	_, body = h.visit(visit{method: "GET", path: "/_lab/errors", ip: homeIP})
	if !strings.Contains(body, esc(textsEN.NoErrors)) {
		t.Error("the cache should still show Quiet without errors")
	}
	h.srv.errors.at = time.Time{}
	_, body = h.visit(visit{method: "GET", path: "/_lab/errors", ip: homeIP})
	if strings.Contains(body, esc(textsEN.NoErrors)) {
		t.Error("after the cache expired, Quiet's error should show")
	}
}

func TestWatchErrorsAlertsOnce(t *testing.T) {
	h := newHarness(t)
	f := installFakeNotify(t, h)
	n := h.srv.notifier
	now := time.Now()
	inst := Instance{App: "hello", Env: prodEnv, Port: 20001, Status: "running", Health: "/healthz", DeployedAt: now}
	preview := Instance{App: "hello", Env: "dark-mode", Port: 20002, Status: "running", Health: "/healthz", DeployedAt: now}
	for _, i := range []Instance{inst, preview} {
		if err := h.srv.store.Put(i); err != nil {
			t.Fatal(err)
		}
	}
	os.MkdirAll(filepath.Dir(h.runner.logPath("x")), 0o755)
	ctx := context.Background()

	// A steady trickle: nothing.
	steady := map[int]int{}
	for ago := 0; ago <= 24; ago++ {
		steady[ago] = 10
	}
	os.WriteFile(h.runner.logPath(inst.ID()), []byte(journalWith(now, steady)), 0o644)
	n.watchErrors(ctx)
	if len(f.messages()) != 0 {
		t.Fatalf("a steady error rate alerted: %s", f.titles())
	}

	// A burst in the last hour, in production and in a preview: one alert,
	// for production, with a sample and the usual rate.
	burst := map[int]int{0: 25, 2: 1, 7: 1}
	os.WriteFile(h.runner.logPath(inst.ID()), []byte(journalWith(now, burst)), 0o644)
	os.WriteFile(h.runner.logPath(preview.ID()), []byte(journalWith(now, burst)), 0o644)
	n.watchErrors(ctx)
	got := f.messages()
	if len(got) != 1 {
		t.Fatalf("want one alert, got %s", f.titles())
	}
	m := got[0]
	if m.Topic != "lab" || m.Title != "hello: 25 errors in the last hour" || !strings.Contains(m.Body, "saving note 0: database is locked") ||
		!strings.Contains(m.Body, "usual: ~0.1/h") || strings.Count(m.Body, "\n") != 3 || m.URL != "" {
		t.Errorf("alert: %+v", m)
	}
	// The same burst ten minutes later: no second alert within 6 hours.
	n.watchErrors(ctx)
	if len(f.messages()) != 1 {
		t.Fatalf("alerted again: %s", f.titles())
	}
	// With the lab's domain set, the alert links to the Errors page.
	n.forget("errors:" + inst.ID())
	set := h.srv.store.Settings()
	set.Domain = testDomain
	if err := h.srv.store.SetSettings(set); err != nil {
		t.Fatal(err)
	}
	n.watchErrors(ctx)
	if got = f.messages(); len(got) != 2 || got[1].URL != "https://"+testDomain+"/_lab/errors" {
		t.Fatalf("alert url: %s", f.titles())
	}
}
