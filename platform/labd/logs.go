package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// What the apps log and what the lab does with it (CONTRACT.md, "Logging").
// Apps write one JSON object per line to stdout (platform/appkit does it for
// them: time, level, msg, app, env, version, and for requests method, path,
// status, ms, bytes, rid); journald keeps the lines for 30 days. labd reads
// them back for two things: the Errors page (GET /_lab/errors) that shows
// each app's recent errors without anyone running a workflow, and the
// error-rate alert that tells the owner's phone once when an app has clearly
// more errors than usual.

const (
	journalMax     = 4 << 20 // bytes of journal read per instance
	errorsWindow   = 24 * time.Hour
	errorsKeep     = 30 // error lines shown per app
	errorsCacheFor = time.Minute
	alertMinErrors = 10 // errors in the last hour before an alert is considered
	alertEvery     = 6 * time.Hour
)

// logLine is one line of an app's journal, as far as it could be read.
type logLine struct {
	Time  time.Time // zero when the line had no usable time
	Level string    // ERROR, WARN, INFO, DEBUG
	Msg   string
	RID   string // request id, for request lines
	Err   string // the "err" attribute, when there is one
}

// parseJournalLine reads one journald line (short-iso: "<time> <host>
// <unit>[pid]: <message>") or a bare line of app output. JSON lines give
// their own level, time and message; anything else counts as INFO unless it
// smells like trouble (a Go panic, an error printed by hand).
func parseJournalLine(raw string) (logLine, bool) {
	raw = strings.TrimRight(raw, "\r")
	if raw == "" || strings.HasPrefix(raw, "-- ") {
		return logLine{}, false
	}
	var l logLine
	msg := raw
	if len(raw) > 25 && raw[4] == '-' && raw[10] == 'T' {
		if ts, rest, ok := strings.Cut(raw, " "); ok {
			if t, err := time.Parse("2006-01-02T15:04:05-0700", ts); err == nil {
				l.Time = t
				// "host unit[pid]: message" or "host unit: message"
				if _, after, ok := strings.Cut(rest, "]: "); ok {
					msg = after
				} else if _, after, ok := strings.Cut(rest, ": "); ok {
					msg = after
				} else {
					msg = rest
				}
			}
		}
	}
	msg = strings.TrimSpace(msg)
	if strings.HasPrefix(msg, "{") && strings.HasSuffix(msg, "}") {
		var m map[string]any
		if json.Unmarshal([]byte(msg), &m) == nil {
			l.Level = strings.ToUpper(stringOf(m["level"]))
			l.Msg = stringOf(m["msg"])
			l.RID = stringOf(m["rid"])
			l.Err = stringOf(m["err"])
			if ts := stringOf(m["time"]); ts != "" {
				if t, err := time.Parse(time.RFC3339Nano, ts); err == nil {
					l.Time = t
				}
			}
			if l.Msg == "request" {
				l.Msg = fmt.Sprintf("%s %s → %s", stringOf(m["method"]), stringOf(m["path"]), numberOf(m["status"]))
			}
			switch l.Level {
			case "ERROR", "WARN", "INFO", "DEBUG":
			case "WARNING":
				l.Level = "WARN"
			case "":
				l.Level = "INFO"
			default:
				if strings.HasPrefix(l.Level, "ERROR") {
					l.Level = "ERROR"
				} else {
					l.Level = "INFO"
				}
			}
			return l, true
		}
	}
	l.Msg = msg
	lower := strings.ToLower(msg)
	switch {
	case strings.HasPrefix(msg, "panic:"), strings.HasPrefix(msg, "fatal error:"), strings.Contains(lower, "error"):
		l.Level = "ERROR"
	default:
		l.Level = "INFO"
	}
	return l, true
}

func stringOf(v any) string {
	s, _ := v.(string)
	return s
}

func numberOf(v any) string {
	switch n := v.(type) {
	case float64:
		return strconv.FormatInt(int64(n), 10)
	case string:
		return n
	}
	return ""
}

// parseJournal reads every line of a journal dump.
func parseJournal(text string) []logLine {
	var out []logLine
	for _, raw := range strings.Split(text, "\n") {
		if l, ok := parseJournalLine(raw); ok {
			out = append(out, l)
		}
	}
	return out
}

// appErrors is one instance's last 24 hours as the Errors page shows them.
type appErrors struct {
	Instance Instance
	Errors   int
	Warnings int
	Infos    int
	Lines    []logLine // the newest errors, newest first, at most errorsKeep
	Failed   string    // why the journal could not be read, if it couldn't
}

// summarize counts the lines since the window started and keeps the newest
// errors. Lines without a time count as inside the window (a bare app
// output in local runs).
func summarize(lines []logLine, since time.Time) (errs, warns, infos int, recent []logLine) {
	for _, l := range lines {
		if !l.Time.IsZero() && l.Time.Before(since) {
			continue
		}
		switch l.Level {
		case "ERROR":
			errs++
			recent = append(recent, l)
		case "WARN":
			warns++
		default:
			infos++
		}
	}
	// Newest first; journald prints oldest first.
	for i, j := 0, len(recent)-1; i < j; i, j = i+1, j-1 {
		recent[i], recent[j] = recent[j], recent[i]
	}
	if len(recent) > errorsKeep {
		recent = recent[:errorsKeep]
	}
	return
}

// errorsCache keeps the page's data for a minute: reading every journal is
// not free and the owner may reload.
type errorsCache struct {
	mu   sync.Mutex
	at   time.Time
	data []appErrors
}

// collectErrors reads every instance's journal for the last 24 hours,
// production first, then previews by name.
func (s *Server) collectErrors(ctx context.Context) []appErrors {
	s.errors.mu.Lock()
	defer s.errors.mu.Unlock()
	if time.Since(s.errors.at) < errorsCacheFor && s.errors.data != nil {
		return s.errors.data
	}
	insts := s.store.List()
	sort.SliceStable(insts, func(a, b int) bool {
		if (insts[a].Env == prodEnv) != (insts[b].Env == prodEnv) {
			return insts[a].Env == prodEnv
		}
		if insts[a].Env != insts[b].Env {
			return insts[a].Env < insts[b].Env
		}
		return insts[a].App < insts[b].App
	})
	since := time.Now().Add(-errorsWindow)
	out := make([]appErrors, 0, len(insts))
	for _, inst := range insts {
		e := appErrors{Instance: inst}
		text, err := s.runner.Journal(ctx, inst.ID(), errorsWindow)
		if err != nil {
			e.Failed = err.Error()
		} else {
			e.Errors, e.Warnings, e.Infos, e.Lines = summarize(parseJournal(text), since)
		}
		out = append(out, e)
	}
	s.errors.at, s.errors.data = time.Now(), out
	return out
}

// --- the page ---

type errorLineView struct {
	When, Msg, RID, Err string
}

type appErrorsView struct {
	Title, Host, Env string
	Prod, Open       bool
	Errors, Warnings int
	Infos            int
	Lines            []errorLineView
	Failed           string
}

// errorsPage is GET /_lab/errors: every app's errors of the last 24 hours,
// behind the same gate as the home page.
func (s *Server) errorsPage(w http.ResponseWriter, r *http.Request, base string) {
	lang, t := pageLang(r)
	set := s.store.Settings()
	in := s.signedIn(r, base)
	if !in && !allowedIP(clientIP(r), set.AllowIPs) {
		http.Redirect(w, r, "/_lab/login?next=%2F_lab%2Ferrors", http.StatusSeeOther)
		return
	}
	loc, err := time.LoadLocation("Europe/Prague")
	if err != nil {
		loc = time.UTC
	}
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	d := pageData{T: t, Lang: lang, Title: t.ErrorsTitle, SignedIn: in}
	for _, e := range s.collectErrors(ctx) {
		title := e.Instance.Title
		if title == "" {
			title = e.Instance.App
		}
		v := appErrorsView{
			Title: title, Host: hostFor(e.Instance.App, e.Instance.Env, set), Env: e.Instance.Env,
			Prod: e.Instance.Env == prodEnv, Open: e.Errors > 0 || e.Failed != "",
			Errors: e.Errors, Warnings: e.Warnings, Infos: e.Infos, Failed: e.Failed,
		}
		for _, l := range e.Lines {
			when := "—"
			if !l.Time.IsZero() {
				when = l.Time.In(loc).Format("2. 1. 15:04")
			}
			v.Lines = append(v.Lines, errorLineView{When: when, Msg: clip(l.Msg, 300), RID: l.RID, Err: clip(l.Err, 300)})
		}
		d.Errors = append(d.Errors, v)
	}
	render(w, "errors", http.StatusOK, d)
}

// --- the alert ---

// errorRate counts an instance's errors in the last hour and in the 24 hours
// before that, and returns the newest error lines.
func errorRate(lines []logLine, now time.Time) (lastHour, before int, recent []logLine) {
	hourAgo := now.Add(-time.Hour)
	dayAgo := hourAgo.Add(-24 * time.Hour)
	for _, l := range lines {
		if l.Level != "ERROR" {
			continue
		}
		switch {
		case l.Time.IsZero() || !l.Time.Before(hourAgo):
			lastHour++
			recent = append(recent, l)
		case !l.Time.Before(dayAgo):
			before++
		}
	}
	for i, j := 0, len(recent)-1; i < j; i, j = i+1, j-1 {
		recent[i], recent[j] = recent[j], recent[i]
	}
	return
}

// alertWorthy says whether an hour with lastHour errors stands out against
// the day before it: at least 10, and at least three times the usual hourly
// count plus five, so a steady trickle never alerts and a burst does.
func alertWorthy(lastHour, before int) bool {
	usual := float64(before) / 24
	return lastHour >= alertMinErrors && float64(lastHour) >= 3*usual+5
}

// watchErrors runs with the 10-minute tick: for every production app it reads
// the last 25 hours of its journal and alerts once per 6 hours when the last
// hour stands out.
func (n *notifier) watchErrors(ctx context.Context) {
	set := n.store.Settings()
	now := n.now()
	for _, inst := range n.store.List() {
		if inst.Env != prodEnv || inst.Status != "running" {
			continue
		}
		text, err := n.runner.Journal(ctx, inst.ID(), 25*time.Hour)
		if err != nil {
			log.Printf("errors: reading the journal of %s: %v", inst.ID(), err)
			continue
		}
		lastHour, before, recent := errorRate(parseJournal(text), now)
		if !alertWorthy(lastHour, before) {
			continue
		}
		var sample []string
		for i, l := range recent {
			if i == 3 {
				break
			}
			line := l.Msg
			if l.Err != "" {
				line += ": " + l.Err
			}
			sample = append(sample, clip(line, 200))
		}
		body := strings.Join(sample, "\n") + fmt.Sprintf("\nusual: ~%.1f/h", float64(before)/24)
		url := ""
		if set.Domain != "" {
			url = "https://" + set.Domain + "/_lab/errors"
		}
		n.sendOnce(ctx, "errors:"+inst.ID(), alertEvery, message{
			Topic: "lab",
			Title: clip(fmt.Sprintf("%s: %d errors in the last hour", inst.App, lastHour), notifyMaxTitle),
			Body:  clip(body, notifyMaxBody),
			URL:   url,
		})
	}
}
