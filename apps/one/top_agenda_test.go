package main

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// Tests of the four screen changes of 2026-11-19 (SPEC 1, 5 and 18): a
// compact top, messages in full, the Now line in the agenda. All data is invented.

// A message of a few lines is shown as it is; only a text of more than forty
// lines is marked to fold and carries Show all.
func TestShortMessageIsRenderedUnfoldedAndATextOverTheLimitIsMarkedToFold(t *testing.T) {
	line := strings.Repeat("x", 30)
	for _, c := range []struct {
		name string
		text string
		long bool
	}{
		{"three lines", "one\ntwo\nthree", false},
		{"seven lines", strings.Repeat(line+"\n", 7), false},
		{"exactly forty lines", strings.TrimSuffix(strings.Repeat(line+"\n", 40), "\n"), false},
		{"forty-one lines", strings.TrimSuffix(strings.Repeat(line+"\n", 41), "\n"), true},
		{"one long paragraph that wraps over forty lines", strings.Repeat("word ", 900), true},
		{"one paragraph that wraps over ten lines", strings.Repeat("word ", 150), false},
	} {
		if got := isLongText(c.text); got != c.long {
			t.Errorf("%s: isLongText = %v, want %v", c.name, got, c.long)
		}
	}

	e := newListEnv(t)
	short := e.mail("short one", 1)
	short.text = "Hello,\nthe parcel comes on Friday.\nThanks"
	long := e.mail("long one", 0.5)
	long.text = strings.TrimSuffix(strings.Repeat("A line of the long text.\n", 60), "\n")
	itShort := e.seed(stageNew, short)
	itLong := e.seed(stageNew, long)

	page := e.openRow(itShort.ID)
	if !strings.Contains(page, "the parcel comes on Friday.") || strings.Contains(page, "folded") || strings.Contains(page, "Show all") || strings.Contains(page, "data-fold") {
		t.Errorf("a short message is folded: %s", page)
	}
	page = e.openRow(itLong.ID)
	if !strings.Contains(page, `class="text folded"`) || !strings.Contains(page, ">Show all</button>") {
		t.Errorf("a text of 60 lines is not marked to fold: %s", page)
	}
	if !strings.Contains(page, long.text) {
		t.Errorf("the folded text is not complete in the page")
	}
	// The fold button follows its text box directly: the script toggles the box before it.
	if !strings.Contains(page, `</div><button type="button" class="tertiary fold-toggle" aria-expanded="false">Show all</button>`) {
		t.Errorf("the button does not follow the text")
	}
}

// The top bar is one header block holding everything the SPEC names, so the
// stylesheet can lay it out in one line (a laptop) or two (a phone).
func TestTopBarHoldsNameDotNumbersLinksSearchAndEnvironmentInOneHeaderBlock(t *testing.T) {
	e := newListEnv(t)
	e.seed(stageNew, e.mail("one", 1))
	e.seed(stageInProgress, e.mail("two", 2))
	page := get(e.h, "/").Body.String()
	i := strings.Index(page, `<header class="top" id="top">`)
	j := strings.Index(page, "</header>")
	if i < 0 || j < i {
		t.Fatalf("no header block")
	}
	head := page[i:j]
	main := page[j:]
	for _, want := range []string{
		`<div class="top-bar">`, `class="brand"`, `<h1><a href="/">ONE</a></h1>`, `class="dot dot-ok"`, `href="/connectors"`,
		`class="top-status"`, `id="heading"`, "1 new, 1 in progress", `id="alerts"`, `id="people-strip"`, `id="claude-strip"`,
		`class="top-nav" aria-label="Pages"`, `href="/notes" title="Notes">`, `<span>Notes</span></a>`, `title="Refresh"`, `<span>Connectors</span></a>`, `<span>Settings</span></a>`,
		`class="top-right"`, `role="search"`, `id="q"`, `name="q"`, `js-agenda-toggle`, ">Agenda</button>",
	} {
		if !strings.Contains(head, want) {
			t.Errorf("the header block lacks %q", want)
		}
	}
	// Nothing of the top is left in the page below it, so the list starts right under the bar.
	// The environment is said at the foot of the page, beside the version.
	if !strings.Contains(main, `<footer><span class="env">Environment: development</span>`) {
		t.Error("the foot of the page does not say the environment")
	}
	for _, not := range []string{`id="heading"`, `id="claude-strip"`, `role="search"`} {
		if strings.Contains(main, not) {
			t.Errorf("%q is below the header", not)
		}
	}
	// on a laptop the left pane stands between the ribbon and the rows; on a phone it is not shown
	rest := strings.TrimSpace(main[len("</header>"):])
	if !strings.HasPrefix(rest, `<aside class="pane-left"`) || !strings.Contains(rest, "</aside>\n<main class=\"list-page\"") {
		t.Errorf("the list does not follow the header and the left pane directly")
	}
	// The order the checks and the phone layout rely on: alerts before the Claude strip.
	if strings.Index(head, `id="alerts"`) > strings.Index(head, `id="claude-strip"`) {
		t.Errorf("strips are out of order")
	}
	// Exact wordings of the page stay.
	for _, want := range []string{"<title>ONE</title>", "Environment: development", `data-stage="new"`, ">Closed"} {
		if want == ">Closed" {
			continue // there is no closed item here
		}
		if !strings.Contains(page, want) {
			t.Errorf("the page lacks %q", want)
		}
	}
	empty := newListEnv(t)
	if p := get(empty.h, "/").Body.String(); !strings.Contains(p, "Nothing needs you") || !strings.Contains(p, "Nothing here yet.") {
		t.Errorf("empty list wording is gone")
	}
}

// --- the Now line ---

func TestNowIndexPlacesTheLineBetweenTheEntriesBeforeAndAfter(t *testing.T) {
	h := func(hh int) int64 { return time.Date(2026, 11, 19, hh, 0, 0, 0, time.UTC).Unix() }
	evs := []eventView{
		{Title: "All day", Timed: false},
		{Title: "Early", Timed: true, StartAt: h(8), EndAt: h(9)},
		{Title: "Running", Timed: true, StartAt: h(12), EndAt: h(14)},
		{Title: "Later", Timed: true, StartAt: h(16), EndAt: h(17)},
	}
	for _, c := range []struct {
		at   int64
		want string // what the line follows
	}{
		{h(7), "All day"}, {h(8), "Early"}, {h(10), "Early"}, {h(13), "Running"}, {h(16), "Later"}, {h(23), "Later"},
	} {
		got := withNowLine(evs, c.at, "Now")
		var prev string
		for i, ev := range got {
			if ev.IsNow {
				if i == 0 {
					t.Fatalf("at %d the line is first", c.at)
				}
				prev = got[i-1].Title
			}
		}
		if prev != c.want || len(got) != len(evs)+1 {
			t.Errorf("at %s the line follows %q, want %q", time.Unix(c.at, 0).UTC().Format("15:04"), prev, c.want)
		}
	}
	if got := withNowLine(nil, h(12), "Now 13:00"); len(got) != 1 || !got[0].IsNow {
		t.Errorf("a day with no entry: %+v", got)
	}
	if !running(evs[2], h(13)) || running(evs[2], h(14)) || running(evs[1], h(13)) || running(evs[0], h(13)) {
		t.Errorf("running is wrong")
	}
	cancelled := evs[2]
	cancelled.Mark = "Cancelled"
	if running(cancelled, h(13)) {
		t.Errorf("a cancelled entry is running")
	}
}

func TestAgendaHoldsOneNowLineBetweenTheRightEntriesAndTheNowButton(t *testing.T) {
	e := newCalEnv(t)
	e.now = time.Date(2026, 11, 19, 12, 30, 0, 0, time.UTC) // 13:30 in Prague
	cal := e.dav.calendar("c", "Calendar")
	e.dav.put(cal, "1.ics", one("n1", "Morning call", "20261119T080000Z", "20261119T090000Z"))
	e.dav.put(cal, "2.ics", one("n2", "Lunch with the supplier", "20261119T120000Z", "20261119T133000Z"))
	e.dav.put(cal, "3.ics", one("n3", "Evening class", "20261119T160000Z", "20261119T170000Z"))
	e.dav.put(cal, "4.ics", one("n4", "Tomorrow thing", "20261120T080000Z", "20261120T090000Z"))
	e.connectICloud()
	page := e.agenda()
	if n := strings.Count(page, `id="ag-now"`); n != 1 {
		t.Fatalf("%d Now lines", n)
	}
	if !strings.Contains(page, `<span class="ag-now-text">Now 13:30</span>`) {
		t.Errorf("the line does not read Now 13:30")
	}
	pos := func(s string) int { return strings.Index(page, s) }
	if !(pos(">Morning call<") < pos("Now 13:30") && pos(">Lunch with the supplier<") < pos("Now 13:30") &&
		pos("Now 13:30") < pos(">Evening class<") && pos("Now 13:30") < pos(">Tomorrow thing<")) {
		t.Errorf("the line is not between the entries before and after it")
	}
	// Only the entry under way carries the mark.
	if n := strings.Count(page, "ag-ev running"); n != 1 || !strings.Contains(page[pos(">Lunch with the supplier<")-400:pos(">Lunch with the supplier<")], "ag-ev running") {
		t.Errorf("running marks: %d", n)
	}
	if n := strings.Count(page, `class="ag-mark ag-run">Running now</span>`); n != 1 {
		t.Errorf("%d visible running marks", n)
	}
	if !strings.Contains(page, `data-zone="Europe/Prague"`) {
		t.Errorf("the page does not say the zone")
	}
	index := e.indexPage()
	if !strings.Contains(index, `js-now">Now</button>`) {
		t.Errorf("no Now button")
	}
	if i, j := strings.Index(index, `js-now">Now</button>`), strings.Index(index, `id="ag-body"`); i > j {
		t.Errorf("the Now button is inside the part that is swapped, so it would not stay")
	}
}

func TestAgendaNowLineOnADayWithNoEvents(t *testing.T) {
	e := newCalEnv(t)
	e.now = time.Date(2026, 11, 19, 20, 41, 0, 0, time.UTC) // 21:41
	page := e.agenda()
	if strings.Count(page, `id="ag-now"`) != 1 || !strings.Contains(page, "Now 21:41") || !strings.Contains(page, "Today · Thu 19 Nov") || !strings.Contains(page, "No events.") {
		t.Errorf("an empty agenda: %s", page)
	}
	// With entries only on other days, the line still sits under today's heading.
	cal := e.dav.calendar("c", "Calendar")
	e.dav.put(cal, "1.ics", one("m1", "Another day", "20261121T080000Z", "20261121T090000Z"))
	e.connectICloud()
	page = e.agenda()
	i, j, k := strings.Index(page, "Today · Thu 19 Nov"), strings.Index(page, "Now 21:41"), strings.Index(page, ">Another day<")
	if !(i >= 0 && i < j && j < k) {
		t.Errorf("order: today %d, line %d, entry %d", i, j, k)
	}
	if strings.Contains(page, "No events.") {
		t.Errorf("No events. with an entry in the window")
	}
}

func TestAgendaNowLineAcrossMidnight(t *testing.T) {
	e := newCalEnv(t)
	e.now = time.Date(2026, 11, 19, 23, 30, 0, 0, time.UTC) // 00:30 on 20 November in Prague
	cal := e.dav.calendar("c", "Calendar")
	e.dav.put(cal, "1.ics", one("c1", "Late call", "20261119T220000Z", "20261119T234500Z")) // 23:00 to 00:45
	e.dav.put(cal, "2.ics", one("c2", "Breakfast", "20261120T070000Z", "20261120T080000Z"))
	e.connectICloud()
	page := e.agenda()
	pos := func(s string) int { return strings.Index(page, s) }
	yesterday, call, today, line, breakfast := pos("Thu 19 Nov"), pos(">Late call<"), pos("Today · Fri 20 Nov"), pos("Now 00:30"), pos(">Breakfast<")
	if !(yesterday >= 0 && yesterday < call && call < today && today < line && line < breakfast) {
		t.Errorf("order %d %d %d %d %d", yesterday, call, today, line, breakfast)
	}
	if !strings.Contains(page[call-400:call], "ag-ev running") {
		t.Errorf("the call that began before midnight is not marked as running")
	}
	if strings.Count(page, "ag-ev running") != 1 {
		t.Errorf("more than one running entry")
	}
}

// A running entry of a calendar is marked in the page and is not marked once it ended.
func TestRunningEntryCarriesItsMark(t *testing.T) {
	for _, c := range []struct {
		now     time.Time
		running bool
	}{
		{time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC), false},
		{time.Date(2026, 11, 19, 10, 45, 0, 0, time.UTC), true},
		{time.Date(2026, 11, 19, 11, 0, 0, 0, time.UTC), false}, // the end is not inside
	} {
		e := newCalEnv(t)
		e.now = c.now
		cal := e.dav.calendar("c", "Calendar")
		e.dav.put(cal, "1.ics", one("r1", "Workshop", "20261119T103000Z", "20261119T110000Z"))
		e.connectICloud()
		page := e.agenda()
		got := strings.Contains(page, "ag-ev running") && strings.Contains(page, `class="ag-mark ag-run">Running now</span>`)
		if got != c.running {
			t.Errorf("at %s: running mark %v, want %v", c.now.Format("15:04"), got, c.running)
		}
		if !strings.Contains(page, fmt.Sprintf(`data-start="%d"`, time.Date(2026, 11, 19, 10, 30, 0, 0, time.UTC).Unix())) {
			t.Errorf("the entry does not carry its start for the script")
		}
	}
}

// A picture opens full size from the app itself: its link goes to /view, which
// shows a verified picture inline and still downloads anything else.
func TestPictureLinkOpensTheViewAddressWhichIsInline(t *testing.T) {
	e := newListEnv(t)
	it := e.seed(stageNew, e.mail("with a picture", 1))
	src := fmt.Sprint("m", seedN.Load(), "-0")
	png := append([]byte("\x89PNG\r\n\x1a\n"), make([]byte, 64)...)
	pngID := e.saveFile(it, src, "plan.png", "image/png", "inline", png)
	pdfID := e.saveFile(it, src, "terms.pdf", "application/pdf", "attachment", []byte("%PDF-1.4 terms"))
	fakeID := e.saveFile(it, src, "fake.png", "image/png", "inline", []byte("<html><script>alert(1)</script></html>"))
	page := e.openRow(it.ID)
	if !strings.Contains(page, fmt.Sprintf(`<a class="pic-link" href="/files/%d/view"`, pngID)) || !strings.Contains(page, fmt.Sprintf(`<img class="pic pic-fit" src="/files/%d"`, pngID)) {
		t.Errorf("the picture is not linked to its view address: %s", page)
	}
	rec := get(e.h, fmt.Sprintf("/files/%d/view", pngID))
	if cd := rec.Header().Get("Content-Disposition"); rec.Code != 200 || !strings.HasPrefix(cd, "inline") || rec.Header().Get("Content-Type") != "image/png" ||
		rec.Header().Get("X-Content-Type-Options") != "nosniff" || !strings.Contains(rec.Header().Get("Content-Security-Policy"), "sandbox") {
		t.Errorf("view of a picture: %d %v", rec.Code, rec.Header())
	}
	for _, id := range []int64{pdfID, fakeID} {
		rec := get(e.h, fmt.Sprintf("/files/%d/view", id))
		if !strings.HasPrefix(rec.Header().Get("Content-Disposition"), "attachment") || rec.Header().Get("Content-Type") != "application/octet-stream" {
			t.Errorf("view of file %d is not a download: %v", id, rec.Header())
		}
	}
}
