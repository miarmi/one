package main

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"lab/apps/one/repo"
)

// Tests of the laptop layout (SPEC 1, "On a laptop the page has three panes and
// a ribbon"): the markup the stylesheet and the script work on, the stage
// counts, the lines of the block Connections and the wording they use.

func pageBetween(t *testing.T, s, from, to string) string {
	t.Helper()
	i := strings.Index(s, from)
	if i < 0 {
		t.Fatalf("no %q in the page", from)
	}
	s = s[i:]
	j := strings.Index(s, to)
	if j < 0 {
		t.Fatalf("no %q after %q", to, from)
	}
	return s[:j]
}

func TestRibbonHasTheWordsOfTheSpecInOrderAndStartsGreyedOut(t *testing.T) {
	e := newListEnv(t)
	e.seed(stageNew, e.mail("one", 0.1))
	page := get(e.h, "/").Body.String()
	head := pageBetween(t, page, `<header class="top"`, "</header>")
	if !strings.Contains(head, `role="toolbar"`) || !strings.Contains(head, `id="ribbon-tools"`) {
		t.Error("the ribbon has no toolbar for the chosen row")
	}
	last := -1
	for _, w := range []string{"<span>Open</span>", "<span>Reply</span>", "<span>Pending</span>", "<span>Resolve</span>", "<span>Reopen</span>", "<span>To do</span>",
		"<span>Notes</span>", "<span>People</span>", "<span>Connectors</span>", "<span>Settings</span>", ">Refresh</span>"} {
		i := strings.Index(head, w)
		if i < 0 {
			t.Errorf("the ribbon lacks %q", w)
			continue
		}
		if i < last {
			t.Errorf("%q is out of order", w)
		}
		last = i
	}
	// every action of the toolbar is greyed out until the script has chosen a row, and Reopen is not shown before a stage offers it
	tools := pageBetween(t, head, `id="ribbon-tools"`, "</div>")
	for _, act := range []string{"open", "reply", "pending", "resolve", "reopen", "todo"} {
		if !strings.Contains(tools, `data-act="`+act+`" aria-disabled="true"`) {
			t.Errorf("the action %s does not start greyed out", act)
		}
	}
	if !strings.Contains(tools, `data-act="reopen" aria-disabled="true" hidden`) {
		t.Error("Reopen is shown before a stage offers it")
	}
	// icons: inline, hidden from screen readers, no font and no file
	if n := strings.Count(head, `<svg class="ic" width="16" height="16"`); n < 11 {
		t.Errorf("%d icons in the ribbon, want one for each of the 11 words", n)
	}
	for _, part := range strings.Split(head, "<svg")[1:] {
		if tag := part[:strings.Index(part, ">")]; !strings.Contains(tag, `aria-hidden="true"`) && !strings.Contains(tag, `class="search-icon"`) {
			t.Errorf("an icon is not hidden from screen readers: <svg%.80s", part)
		}
	}
	if strings.Contains(head, "stroke-width=\"1.5\"") == false {
		t.Error("the icons are not drawn with a 1.5 stroke")
	}
	// the strict policy: no inline style, no inline handler anywhere on the page
	for _, bad := range []string{` style="`, ` onclick=`, ` onchange=`, ` onsubmit=`, "<script>"} {
		if strings.Contains(page, bad) {
			t.Errorf("the page holds %q, which the content policy forbids", bad)
		}
	}
}

func TestEveryPageHasTheRibbonInPlaceOfTheOldTopLine(t *testing.T) {
	e := newAgentEnv(t)
	for path, title := range map[string]string{"/notes": "Notes", "/settings": "Settings", "/connectors": "Connectors", "/people": "People", "/closed": "Closed", "/search?q=x": "Search"} {
		page := get(e.h, path).Body.String()
		head := pageBetween(t, page, `<header class="top`, "</header>")
		if strings.Contains(head, `id="ribbon-tools"`) {
			t.Errorf("%s: the toolbar of a row is on a page that has no rows", path)
		}
		if !strings.Contains(head, `class="heading page-title" id="page-title">`+title+`</h2>`) {
			t.Errorf("%s: the page's title is not in the ribbon", path)
		}
		for _, w := range []string{"<span>Notes</span>", "<span>People</span>", "<span>Connectors</span>", "<span>Settings</span>", ">Refresh</span>"} {
			if !strings.Contains(head, w) {
				t.Errorf("%s: the ribbon lacks %q", path, w)
			}
		}
		if !strings.Contains(head, `<h1><a href="/">ONE</a></h1>`) {
			t.Errorf("%s: ONE is not a link to the list", path)
		}
	}
}

func TestLeftPaneListsTheFiveStagesWithCountsAndProposals(t *testing.T) {
	e := newAgentEnv(t)
	a := e.newItem(stageNew, false, "First", "")
	e.newItem(stageNew, false, "Second", "")
	e.newItem(stageInProgress, false, "Third", "")
	e.newItem(stagePending, false, "Fourth", "")
	e.propose(a, "resolve", "Nothing more is needed.")
	closed := e.newItem(stageNew, true, "Newsletter", "")
	if m := e.post("/api/items/"+ref(closed)+"/skip", map[string]any{"reason": "Noise."}); m["ok"] != true {
		t.Fatal(m)
	}
	page := get(e.h, "/").Body.String()
	left := pageBetween(t, page, `<aside class="pane-left"`, "</aside>")
	for stage, want := range map[string]string{"new": "New", "in_progress": "In progress", "pending": "Pending", "resolved": "Resolved"} {
		if !strings.Contains(left, `data-stage="`+stage+`" aria-pressed="false"><span class="st-name">`+want+`</span>`) {
			t.Errorf("the left pane has no button for %s", want)
		}
	}
	if !strings.Contains(left, `<span class="st-approve" data-approve="new">1 to approve</span><span class="st-count" data-count="new">2</span>`) {
		t.Errorf("New does not show its count and its proposal:\n%s", left)
	}
	if !strings.Contains(left, `data-count="in_progress">1</span>`) || !strings.Contains(left, `data-count="pending">1</span>`) || !strings.Contains(left, `data-count="resolved">0</span>`) {
		t.Errorf("the counts are wrong:\n%s", left)
	}
	if !strings.Contains(left, `<a class="stage-btn" id="stage-closed" href="/closed"><span class="st-name">Closed</span><span class="st-count" id="stage-closed-n">1</span></a>`) {
		t.Errorf("Closed does not lead to the Closed page with its count:\n%s", left)
	}
	// the middle pane has the heading to fill, the right pane the agenda that is always there
	if !strings.Contains(page, `id="mid-name"`) || !strings.Contains(page, `id="mid-count"`) {
		t.Error("the middle pane has no heading")
	}
	if !strings.Contains(page, `class="ag-todo-head"><h3 id="ag-todo-h">To do</h3>`) || !strings.Contains(page, `<h2>Agenda</h2><span class="ag-date">`) {
		t.Error("the right pane lacks its headings Agenda and To do")
	}
	// the state the page asks for keeps the stage counts, the same ones that fill the buttons
	st := get(e.h, "/list/state").Body.String()
	for _, want := range []string{`"new":2`, `"in_progress":1`, `"pending":1`, `"resolved":0`, `"approve":{`} {
		if !strings.Contains(st, want) {
			t.Errorf("the state lacks %s: %s", want, st)
		}
	}
}

// connLines runs the block for given states.
func (e *listEnv) connLines(states ...sourceState) connBlock {
	return e.a.connBlockFor(e.ctx, states)
}

func (e *listEnv) finishedRun(src string, minutesAgo int, made int) {
	e.t.Helper()
	err := e.a.store.Update(e.ctx, func(tx *repo.Tx) error {
		at := e.now.Add(-time.Duration(minutesAgo) * time.Minute).Unix()
		id, err := tx.StartRun(e.ctx, src, "sync", at-5)
		if err != nil {
			return err
		}
		return tx.FinishRun(e.ctx, repo.Run{ID: id, Fetched: made, Stored: made, ItemsMade: made}, "done", "", at)
	})
	if err != nil {
		e.t.Fatal(err)
	}
}

func TestConnectionLinesSayWhenLastReadAndHowMuchCameToday(t *testing.T) {
	e := newListEnv(t)
	e.seed(stageNew, e.mail("today one", 0.05))
	e.seed(stageNew, e.mail("today two", 0.06))
	e.seed(stageNew, e.mail("yesterday", 1.5))
	e.finishedRun(srcGmail, 2, 2)
	e.finishedRun(srcICloud, 12, 3)
	e.finishedRun(srcGraph, 180, 0)
	ok := func(key, label string) sourceState {
		return sourceState{Key: key, Label: label, Configured: true, Status: statusOK, Cls: "st-ok"}
	}
	b := e.connLines(ok(srcGmail, "Gmail"), ok(srcGraph, "Office mail"), ok(srcICloud, "iCloud Calendar"), sourceState{Key: srcGoogleCal, Label: "Google Calendar"})
	if len(b.Lines) != 4 { // three sources that are connected, then Claude: a source that is not connected has no line
		t.Fatalf("%d lines: %+v", len(b.Lines), b.Lines)
	}
	g := b.Lines[0]
	if g.Name != "Gmail" || g.Main != "2 new today" || g.Sub != "read 2 min ago" || g.Cls != "ok" {
		t.Errorf("Gmail: %+v", g)
	}
	if r := b.Lines[1]; r.Main != "0 new today" || r.Sub != "read at "+e.a.clockShort(e.now.Add(-180*time.Minute).Unix(), e.now) {
		t.Errorf("a mailbox read three hours ago: %+v", r)
	}
	if c := b.Lines[2]; c.Name != "iCloud Calendar" || c.Main != "3 changes" || c.Sub != "read 12 min ago" {
		t.Errorf("a calendar says changes, not new: %+v", c)
	}
	if !strings.Contains(g.Title, "Gmail: 2 new today, read 2 min ago") {
		t.Errorf("the title of the line: %q", g.Title)
	}
}

func TestStoppedLoginAndStaleSourcesStandOutAndKeepTheTextsOfTheStrips(t *testing.T) {
	e := newListEnv(t)
	e.finishedRun(srcGmail, 90, 0)
	e.finishedRun(srcICloud, 40, 0)
	stopped := sourceState{Key: srcGmail, Label: "Gmail", Configured: true, Status: statusError, Cls: "st-error", Alert: true, Stopped: true}
	login := sourceState{Key: srcGraph, Label: "Office mail", Configured: true, Status: statusLogin, Cls: "st-error", Alert: true, Stopped: true}
	stale := sourceState{Key: srcICloud, Label: "iCloud Calendar", Configured: true, Status: statusStale, Cls: "st-stale", Alert: true, Silent: 40 * time.Minute}
	b := e.connLines(stopped, login, stale)
	// Error on Connectors says `error` with `read <time>`, in the colour of a stopped source.
	if l := b.Lines[0]; l.Main != "error" || l.Cls != "stopped" || !strings.HasPrefix(l.Sub, "read ") || strings.Contains(l.Sub, "min ago") || l.Alert != "Gmail has stopped. See Connectors." {
		t.Errorf("a source with an error: %+v", l)
	}
	if l := b.Lines[1]; l.Main != "needs login" || l.Cls != "stopped" || l.Alert != "Office mail needs login. See Connectors." {
		t.Errorf("a source that needs login: %+v", l)
	}
	// Stale on Connectors says `late` with `read <time>`, in the colour of a stopped source.
	if l := b.Lines[2]; l.Cls != "stopped" || l.Main != "late" || !strings.HasPrefix(l.Sub, "read ") || !strings.Contains(l.Alert, "has not been read for 40 minutes") {
		t.Errorf("a late source: %+v", l)
	}
	// the page puts the sentence of the strip into the line, for screen readers and as its title
	var buf strings.Builder
	if err := e.a.pages.ExecuteTemplate(&buf, "connections", b); err != nil {
		t.Fatal(err)
	}
	html := buf.String()
	for _, want := range []string{`<h2 class="conns-h" id="conns-h">Connections</h2>`, `conn-li conn-stopped`, `<span class="conn-main">error</span>`,
		`<span class="visually-hidden">Gmail has stopped. See Connectors.</span>`, `href="/connectors"`, `title="Gmail has stopped. See Connectors. Gmail: error, read `} {
		if !strings.Contains(html, want) {
			t.Errorf("the block lacks %q:\n%s", want, html)
		}
	}
}

func TestClaudeLineSaysWaitingAndLastReadAndTheNotesHoldTheStrips(t *testing.T) {
	e := newAgentEnv(t)
	b := e.a.connBlockFor(e.ctx, nil)
	if l := b.Lines[len(b.Lines)-1]; l.Name != "Claude" || l.Main != "0 waiting" || l.Sub != "not read yet" || l.Cls != "ok" {
		t.Errorf("Claude with nothing waiting: %+v", l)
	}
	e.newItem(stageNew, true, "One", "")
	e.newItem(stageNew, true, "Two", "")
	read := e.now.Add(-30 * time.Minute)
	e.a.store.SetSettingBy(e.ctx, keyAgentCall, fmt.Sprint(read.Unix()), "system", e.now)
	b = e.a.connBlockFor(e.ctx, nil)
	l := b.Lines[len(b.Lines)-1]
	if l.Main != "2 waiting" || l.Cls != "claude" || l.Sub != "last read "+e.a.clockShort(read.Unix(), e.now) || l.Alert != "2 waiting for Claude" {
		t.Errorf("Claude with two items waiting: %+v", l)
	}
	// the night check's line comes with its Dismiss, and goes away when it is dismissed
	if err := e.a.store.Update(e.ctx, func(tx *repo.Tx) error {
		id, err := tx.InsertNightCheck(e.ctx, "2026-11-19", "m", e.now.Unix())
		if err != nil {
			return err
		}
		return tx.FinishNightCheck(e.ctx, repo.NightCheck{ID: id, FinishedAt: e.now.Unix(), Status: "done", Checked: 123, BroughtBack: 2})
	}); err != nil {
		t.Fatal(err)
	}
	b = e.a.connBlockFor(e.ctx, nil)
	if len(b.Notes) != 1 || b.Notes[0].Text != "Night check: 123 messages, 2 brought back" || !b.Notes[0].Dismiss {
		t.Fatalf("notes: %+v", b.Notes)
	}
	page := get(e.h, "/").Body.String()
	conns := pageBetween(t, page, `<section class="conns"`, "</section>")
	if !strings.Contains(conns, `<form method="post" action="/night/dismiss"><button type="submit" class="conn-x">Dismiss</button></form>`) {
		t.Errorf("the night check has no Dismiss in the block:\n%s", conns)
	}
	if rec := get(e.h, "/list/connections"); rec.Code != 200 || !strings.Contains(rec.Body.String(), `<h2 class="conns-h"`) {
		t.Errorf("the block as a fragment: %d", rec.Code)
	}
}

func TestReadTextUsesMinutesWithinTheHourAndTheTimeOfDayAfter(t *testing.T) {
	e := newListEnv(t)
	for _, c := range []struct {
		ago  time.Duration
		want string
	}{{30 * time.Second, "read 0 min ago"}, {2 * time.Minute, "read 2 min ago"}, {59 * time.Minute, "read 59 min ago"}} {
		if got := e.a.readText(e.now.Add(-c.ago).Unix(), e.now); got != c.want {
			t.Errorf("%v: %q, want %q", c.ago, got, c.want)
		}
	}
	at := e.now.Add(-2 * time.Hour)
	if got, want := e.a.readText(at.Unix(), e.now), "read at "+at.In(e.a.loc).Format("15:04"); got != want {
		t.Errorf("two hours ago: %q, want %q", got, want)
	}
	if got := e.a.readText(0, e.now); got != "not read yet" {
		t.Errorf("never: %q", got)
	}
	if changesText(1) != "1 change" || changesText(2) != "2 changes" || changesText(0) != "0 changes" {
		t.Error("the word for calendar changes")
	}
}

func TestTheScriptHasTheRibbonAndStageLogic(t *testing.T) {
	js := get(newListEnv(t).h, "/static/app.js").Body.String()
	for _, want := range []string{
		`one.stage`,                // the chosen stage is remembered on the device
		`f.requestSubmit()`,        // the ribbon uses the very form of the row
		`.line-acts form[action$=`, // ...found by the endpoint it posts to
		`"Nothing in "`,            // the heading of an empty stage
		`" items, newest first"`,   // and of one that is not
		`"/list/connections"`,      // the block is renewed with the numbers
		`lap.matches ? true`,       // the agenda is always there on a laptop
	} {
		if !strings.Contains(js, want) {
			t.Errorf("the script lacks %s", want)
		}
	}
}

// A source no longer heard from (the Mac bridge silent past its setting) stays `stopped` with `last heard <time>`;
// only failing reads say `error` (SPEC 1, 2026-11-24).
func TestASilentBridgeStaysStoppedWhileFailingReadsSayError(t *testing.T) {
	e := newListEnv(t)
	e.finishedRun(srcGmail, 90, 0)
	silent := sourceState{Key: srcGmail, Label: "Chat bridge", Configured: true, Status: statusError, Cls: "st-error", Alert: true, Stopped: true, kind: "silent"}
	failing := sourceState{Key: srcGmail, Label: "Gmail", Configured: true, Status: statusError, Cls: "st-error", Alert: true, Stopped: true, kind: "stopped"}
	b := e.connLines(silent, failing)
	if l := b.Lines[0]; l.Main != "stopped" || l.Cls != "stopped" || !strings.HasPrefix(l.Sub, "last heard ") {
		t.Errorf("a silent bridge: %+v", l)
	}
	if l := b.Lines[1]; l.Main != "error" || l.Cls != "stopped" || !strings.HasPrefix(l.Sub, "read ") {
		t.Errorf("a source whose reads fail: %+v", l)
	}
}

func (e *listEnv) failedRun(src string, minutesAgo int) {
	e.t.Helper()
	if err := e.a.store.Update(e.ctx, func(tx *repo.Tx) error {
		at := e.now.Add(-time.Duration(minutesAgo) * time.Minute).Unix()
		id, err := tx.StartRun(e.ctx, src, "sync", at-5)
		if err != nil {
			return err
		}
		return tx.FinishRun(e.ctx, repo.Run{ID: id}, "failed", "authentication failed", at)
	}); err != nil {
		e.t.Fatal(err)
	}
}

// The pane's words follow the real cause, worked out from the runs: reads that
// are tried and fail say `error`; a source still trying but behind says `late`;
// one with no read and no attempt for longer than allowed has stopped.
func TestThePaneWordsFollowTheRealCauseOfASourcesState(t *testing.T) {
	line := func(e *listEnv) connLine {
		e.t.Helper()
		for _, s := range e.a.sourceStates(e.ctx) {
			if s.Key == srcGmail {
				for _, l := range e.a.connBlockFor(e.ctx, []sourceState{s}).Lines {
					if l.Name == s.Label {
						return l
					}
				}
			}
		}
		e.t.Fatal("no Gmail line")
		return connLine{}
	}
	setup := func() *listEnv {
		e := newListEnv(t)
		if err := writeSecret(e.a.secretsDir, gmailSecretFile, gmailSecret{Address: "p@example.test", Password: "x"}); err != nil {
			t.Fatal(err)
		}
		return e
	}
	// failing reads: three failed attempts in a row after an old good read
	e := setup()
	e.finishedRun(srcGmail, 300, 1)
	for _, m := range []int{30, 20, 10} {
		e.failedRun(srcGmail, m)
	}
	if l := line(e); l.Main != "error" || l.Cls != "stopped" || !strings.HasPrefix(l.Sub, "read ") {
		t.Errorf("failing reads: %+v", l)
	}
	// behind its interval but still trying: one failed attempt lately
	e = setup()
	e.finishedRun(srcGmail, 100, 1)
	e.failedRun(srcGmail, 5)
	if l := line(e); l.Main != "late" || l.Cls != "stopped" || !strings.HasPrefix(l.Sub, "read ") {
		t.Errorf("behind its interval: %+v", l)
	}
	// no read and no attempt for longer than allowed: stopped
	e = setup()
	e.finishedRun(srcGmail, 100, 1)
	if l := line(e); l.Main != "stopped" || l.Cls != "stopped" || !strings.HasPrefix(l.Sub, "last heard ") {
		t.Errorf("runs stopped coming: %+v", l)
	}
	// needs login stays as it was
	e = setup()
	e.finishedRun(srcGmail, 100, 1)
	e.a.store.SetSettingBy(e.ctx, keyGmailLogin, "1", "system", e.now)
	if l := line(e); l.Main != "needs login" || l.Cls != "stopped" || !strings.HasPrefix(l.Sub, "last heard ") {
		t.Errorf("needs login: %+v", l)
	}
}
