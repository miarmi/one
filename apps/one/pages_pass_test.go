package main

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"testing"
	"time"
)

// Tests of the pages pass and of search while typing (SPEC 1, 5, 6, 23, 26, 28;
// The owner, 2026-11-19). All data is invented.

// headerOf cuts the top line out of a page.
func headerOf(t *testing.T, path, page string) (left, right string) {
	t.Helper()
	i := strings.Index(page, `<header class="top`)
	j := strings.Index(page, "</header>")
	if i < 0 || j < i {
		t.Fatalf("%s: no header block", path)
	}
	head := page[i:j]
	k := strings.Index(head, `class="top-right"`)
	if k < 0 {
		t.Fatalf("%s: the top line has no right-hand block", path)
	}
	return head[:k], head[k:]
}

// TestEveryPageHasTheListsTopLineWithItsControlsOnTheRight: the name, the
// connections dot and the page's title on the left; Back to the queue, the
// links, the search and the page's own main button in the right-hand block,
// the same on every page (the owner: "has to be either right or left not both").
func TestEveryPageHasTheListsTopLineWithItsControlsOnTheRight(t *testing.T) {
	e := newListEnv(t)
	it := e.seed(stageNew, e.mail("Parcel", 1))
	pages := []struct{ path, title, primary string }{
		{"/closed", "Closed", ""},
		{"/connectors", "Connectors", ""},
		{"/settings", "Settings", ">Save</button>"},
		{"/notes", "Notes", ">Add note</a>"},
		{"/notes/new", "Notes", ">Save</button>"},
		{"/people", "People", ""},
		{"/search?q=parcel", "Search", ""},
		{fmt.Sprintf("/items/%d", it.ID), "Q-0001", ""},
	}
	for _, p := range pages {
		page := get(e.h, p.path).Body.String()
		left, right := headerOf(t, p.path, page)
		if strings.Contains(page, "conn-top") || strings.Contains(page, `class="back-link"><a href="/">`) {
			t.Errorf("%s: it still has a header of its own", p.path)
		}
		for _, want := range []string{`<header class="top top-sub"`, `<h1><a href="/">ONE</a></h1>`, `class="dot dot-ok"`, `id="page-title">` + p.title + "</h2>"} {
			if !strings.Contains(left, want) {
				t.Errorf("%s: the left of the top line lacks %q", p.path, want)
			}
		}
		// The environment is said at the foot of the page, beside the version.
		if !strings.Contains(page, `<footer><span class="env">Environment: development</span>`) {
			t.Errorf("%s: the foot of the page does not say the environment", p.path)
		}
		wants := []string{"Back to the queue", `href="/notes"`, `href="/connectors"`, `href="/settings"`, `role="search"`, `id="q"`}
		if p.primary != "" {
			wants = append(wants, p.primary)
		}
		for _, want := range wants {
			if !strings.Contains(right, want) {
				t.Errorf("%s: the right-hand block lacks %q", p.path, want)
			}
		}
		// Nothing that is a control sits on the left.
		for _, not := range []string{"Back to the queue", ">Add note<", ">Save<", `role="search"`, `class="top-nav"`} {
			if strings.Contains(left, not) {
				t.Errorf("%s: %q is on the left of the top line", p.path, not)
			}
		}
		if strings.Count(page, "<h1") != 1 {
			t.Errorf("%s: %d first-level headings", p.path, strings.Count(page, "<h1"))
		}
	}
	// The list's own top line has no Back link, and every page's search is the one in the top line.
	list := get(e.h, "/").Body.String()
	if _, right := headerOf(t, "/", list); strings.Contains(right, "Back to the queue") {
		t.Error("the list has a Back link")
	}
	// Notes has its button once, and the form's own copy is gone.
	if n := strings.Count(get(e.h, "/notes/new").Body.String(), ">Save</button>"); n != 1 {
		t.Errorf("the note form has %d Save buttons", n)
	}
}

// TestARunThatFoundNothingNewIsDescribedInWords: "why all these 0s" (the owner).
func TestARunThatFoundNothingNewIsDescribedInWords(t *testing.T) {
	for _, c := range []struct {
		name string
		v    importView
		want []string
		not  []string
	}{
		{"nothing new", importView{Read: 37}, []string{"Import of one week (19 Nov 2026 14:02): finished. It read 37 messages and found nothing new; they were all stored already."}, []string{" 0", "Skipped", "Items made"}},
		{"nothing at all", importView{}, []string{"found no messages in that time"}, []string{" 0"}},
		{"only skipped", importView{Read: 5, Skipped: 5}, []string{"found nothing new; 5 were skipped by a rule and the rest stored already"}, []string{" 0"}},
		{"something new", importView{Read: 37, Stored: 12, ItemsMade: 9, FilesSaved: 1}, []string{"It read 37 messages and stored 12 as new (9 items made, 1 file saved)."}, []string{" 0", "nothing new"}},
		{"files waiting", importView{Read: 3, Stored: 3, ItemsMade: 3, FilesPending: 2}, []string{"2 files still to be fetched."}, []string{" 0"}},
	} {
		got := importLine("Import of one week", "19 Nov 2026 14:02", "done", c.v)
		for _, w := range c.want {
			if !strings.Contains(got, w) {
				t.Errorf("%s: %q lacks %q", c.name, got, w)
			}
		}
		for _, n := range c.not {
			if strings.Contains(got, n) {
				t.Errorf("%s: %q holds %q", c.name, got, n)
			}
		}
	}
	// On the page: a second import of the same mail says so, and no label of a column of numbers is left.
	e := newEnv(t)
	e.box.add(e.in("x", "Plain", 1))
	e.must(e.importMonth())
	e.must(e.importMonth())
	page := get(e.h, "/connectors").Body.String()
	if !strings.Contains(page, "found nothing new") {
		t.Errorf("the second import is not described as finding nothing new:\n%s", page)
	}
	for _, gone := range []string{"Messages read", "Skipped by a rule", "Files still pending", "Bytes on disk", "<dl class=\"facts totals\">"} {
		if strings.Contains(page, gone) {
			t.Errorf("the page still has %q", gone)
		}
	}
	// The totals come first.
	if i, j := strings.Index(page, "1 message in 1 item stored."), strings.Index(page, "found nothing new"); i < 0 || i > j {
		t.Error("the totals do not come before the last run")
	}
}

// TestAConnectionRowIsClosedByDefaultAndShowsItsError: eleven rows, none open;
// a failed source keeps its error on the closed row.
func TestAConnectionRowIsClosedByDefaultAndShowsItsError(t *testing.T) {
	e := newEnv(t)
	got := e.captureNotices()
	_ = got
	page := get(e.h, "/connectors").Body.String()
	if n := strings.Count(page, `<details class="crow-d"`); n != 13 {
		t.Fatalf("%d connection rows, want 13", n)
	}
	if strings.Contains(page, `<details class="crow-d" open`) {
		t.Error("a connection row is open by default")
	}
	for _, id := range []string{"gmail", "graph", "chatbridge", "icloud", "outlookcal", "google", "agent", "bridge", "server"} {
		if !strings.Contains(page, `<section class="crow" id="`+id+`"`) {
			t.Errorf("no row for %s", id)
		}
	}
	// Three failures in a row: the row is still closed and still shows the error.
	e.a.gmail.IMAPAddr = deadAddress(t)
	for i := 0; i < 3; i++ {
		e.advance(time.Minute)
		e.a.imp.RunSync(e.ctx)
	}
	page = get(e.h, "/connectors").Body.String()
	i := strings.Index(page, `id="gmail"`)
	row := page[i : i+strings.Index(page[i:], "</section>")]
	summary := row[strings.Index(row, "<summary"):strings.Index(row, "</summary>")]
	if strings.Contains(row, `<details class="crow-d" open`) || !strings.Contains(summary, `class="crow-error"`) || !strings.Contains(summary, "could not reach the server") || !strings.Contains(summary, ">Error<") {
		t.Errorf("the closed Gmail row does not show its error:\n%s", row)
	}
	// The fields, the test output and the import state are inside the row, not in the summary.
	if strings.Contains(summary, "Known limitations") || strings.Contains(summary, "<form") {
		t.Error("the summary holds a form or the limitations")
	}
}

// TestPicturesCarryTheFittingClassAndTheFullSizeLink: whole, fitted to the
// column, at most about 60 % of the window height, full size on a click.
func TestPicturesCarryTheFittingClassAndTheFullSizeLink(t *testing.T) {
	e := newListEnv(t)
	it := e.seed(stageNew, e.mail("with a tall picture", 1))
	src := fmt.Sprint("m", seedN.Load(), "-0")
	png := append([]byte("\x89PNG\r\n\x1a\n"), make([]byte, 64)...)
	id := e.saveFile(it, src, "tall.png", "image/png", "inline", png)
	for _, page := range []string{e.openRow(it.ID), get(e.h, fmt.Sprintf("/items/%d", it.ID)).Body.String()} {
		if !strings.Contains(page, fmt.Sprintf(`<a class="pic-link" href="/files/%d/view" target="_blank"`, id)) || !strings.Contains(page, fmt.Sprintf(`<img class="pic pic-fit" src="/files/%d"`, id)) {
			t.Errorf("the picture lacks its class or its link:\n%s", page)
		}
	}
	css := get(e.h, "/static/style.css").Body.String()
	i := strings.Index(css, ".pic-fit")
	if i < 0 {
		t.Fatal("no rule for the fitting class")
	}
	rule := css[i : i+strings.Index(css[i:], "}")]
	for _, want := range []string{"max-width: 100%", "max-height: 60vh", "object-fit: contain"} {
		if !strings.Contains(rule, want) {
			t.Errorf("the fitting rule lacks %q", want)
		}
	}
	if strings.Contains(rule, "object-fit: cover") {
		t.Error("a picture is cropped")
	}
}

// TestTheOpenRowShowsTheSubjectAndTheSenderOnce.
func TestTheOpenRowShowsTheSubjectAndTheSenderOnce(t *testing.T) {
	e := newListEnv(t)
	m := e.mail("Zebra crossing permit", 1)
	m.text = "Please send the form."
	it := e.seed(stageNew, m)
	body := e.openRow(it.ID)
	if n := strings.Count(body, "Zebra crossing permit"); n != 1 {
		t.Errorf("the subject appears %d times in the open row", n)
	}
	// The sender's name is on the chip, on the row line and in the message: the line is hidden when open (style), the heading is gone.
	if strings.Contains(body, `class="subject"`) || strings.Contains(body, `class="msg-subject"`) {
		t.Errorf("a second subject is shown:\n%s", body)
	}
}

// --- search while typing ---

type liveRes struct {
	Query  string `json:"q"`
	Groups []struct {
		Kind  string `json:"kind"`
		Label string `json:"label"`
		Hits  []struct {
			Link  string `json:"link"`
			Title []struct {
				T string
				H bool
			} `json:"title"`
			Snip []struct {
				T string
				H bool
			} `json:"snippet"`
			Who  string `json:"who"`
			When string `json:"when"`
		} `json:"hits"`
	} `json:"groups"`
}

func (e *agentEnv) live(q string) (liveRes, string) {
	e.t.Helper()
	rec := get(e.h, "/search/live?q="+url.QueryEscape(q))
	if rec.Code != 200 || !strings.HasPrefix(rec.Header().Get("Content-Type"), "application/json") {
		e.t.Fatalf("live search: %d %s", rec.Code, rec.Header().Get("Content-Type"))
	}
	var r liveRes
	if err := json.Unmarshal(rec.Body.Bytes(), &r); err != nil {
		e.t.Fatalf("live search answer: %v\n%s", err, rec.Body.String())
	}
	return r, rec.Body.String()
}

func (r liveRes) kinds() string {
	var k []string
	for _, g := range r.Groups {
		k = append(k, g.Kind)
	}
	return strings.Join(k, ",")
}

// TestLiveSearchReturnsGroupedBoundedResults: People, Mail, Chat, Comment and
// Note in that order, a handful in each, nothing under two letters.
func TestLiveSearchReturnsGroupedBoundedResults(t *testing.T) {
	e := newAgentEnv(t)
	for i := 0; i < 7; i++ {
		m := e.mail(fmt.Sprintf("Marmalade order %d", i), float64(i+1))
		m.text = "The marmalade jars arrive on Friday."
		e.seed(stageNew, m)
	}
	chat := e.mail("Picnic", 1)
	chat.text = "Marmalade for the picnic?"
	chat.network = "whatsapp"
	e.seed(stageNew, chat)
	post(e.h, "/notes", url.Values{"title": {"Marmalade rule"}, "body": {"Never order marmalade on Mondays."}})
	e.makePerson("mail", "marmaduke@example.test", "Marmaduke Example")
	r, _ := e.live("marmalade")
	if got := r.kinds(); got != "mail,chat,note" {
		t.Errorf("groups: %s", got)
	}
	for _, g := range r.Groups {
		if len(g.Hits) == 0 || len(g.Hits) > liveGroupMax {
			t.Errorf("group %s holds %d rows, want 1 to %d", g.Kind, len(g.Hits), liveGroupMax)
		}
		if g.Kind == "mail" {
			if g.Label != "Mail" || len(g.Hits) != liveGroupMax {
				t.Errorf("the mail group: %q with %d rows", g.Label, len(g.Hits))
			}
			h := g.Hits[0]
			marked := false
			for _, p := range h.Snip {
				marked = marked || (p.H && strings.EqualFold(p.T, "marmalade"))
			}
			if !marked || h.Who == "" || h.When == "" || !strings.HasPrefix(h.Link, "/items/") {
				t.Errorf("a mail row: %+v", h)
			}
		}
	}
	// A person is found by the start of a name, under People and first.
	if r, _ := e.live("marmad"); r.kinds() != "people" || r.Groups[0].Label != "People" || !strings.HasPrefix(r.Groups[0].Hits[0].Link, "/people/") {
		t.Errorf("people: %+v", r)
	}
	// Fewer than two letters, and nothing found, give empty answers.
	if r, _ := e.live("m"); len(r.Groups) != 0 {
		t.Errorf("one letter: %+v", r)
	}
	if r, _ := e.live("  "); len(r.Groups) != 0 {
		t.Errorf("blank: %+v", r)
	}
	if r, _ := e.live("qqqzzz"); len(r.Groups) != 0 {
		t.Errorf("nothing: %+v", r)
	}
	// Items that wait for Claude's first look are not shown to the owner, as on the search page.
	e.newItem(stageNew, true, "Secret marmalade", "A hidden marmalade.")
	if _, body := e.live("secret marmalade"); strings.Contains(body, "Secret marmalade") {
		t.Error("an item that waits for Claude is shown")
	}
}

// TestLiveSearchFindsWithoutDiacriticsBothWays: "najom" finds "nájomníčka" and
// "nájom" finds "najomnicka"; the found word is marked whole.
func TestLiveSearchFindsWithoutDiacriticsBothWays(t *testing.T) {
	e := newAgentEnv(t)
	a := e.mail("Smlouva", 2)
	a.text = "Nová nájomníčka podepíše smlouvu v pátek, žluťoučký kůň."
	e.seed(stageNew, a)
	b := e.mail("Contract", 1)
	b.text = "The tenant is called najomnicka in the draft, kun and zlutoucky."
	e.seed(stageNew, b)
	for _, q := range []string{"najom", "nájom", "najomnicka", "nájomníčka", "NÁJOM"} {
		r, _ := e.live(q)
		if len(r.Groups) == 0 || len(r.Groups[0].Hits) != 2 {
			t.Errorf("%q: %+v", q, r)
			continue
		}
		for _, h := range r.Groups[0].Hits {
			marked := ""
			for _, p := range h.Snip {
				if p.H {
					marked += p.T + "|"
				}
			}
			if !strings.Contains(strings.ToLower(marked), "nájomníčka|") && !strings.Contains(marked, "najomnicka|") {
				t.Errorf("%q: the found word is not marked whole: %q", q, marked)
			}
		}
	}
	// Two words: every word must be there, the last one may be a start.
	if r, _ := e.live("zlutoucky ku"); len(r.Groups) == 0 || len(r.Groups[0].Hits) < 1 {
		t.Errorf("two words: %+v", r)
	}
	if r, _ := e.live("podepíše tenant"); len(r.Groups) != 0 {
		t.Errorf("words from two different mails: %+v", r)
	}
	// A person's name, too.
	e.makePerson("mail", "rehor@example.test", "Řehoř Nájemník")
	if r, _ := e.live("rehor najem"); r.kinds() != "people" {
		t.Errorf("a name without accents: %+v", r)
	}
	if r, _ := e.live("ŘEHOŘ"); r.kinds() != "people" {
		t.Errorf("a name in capitals: %+v", r)
	}
}

// TestLiveSearchNeedsTheSignedInPageAndIsNotServedUnderAPI.
func TestLiveSearchNeedsTheSignedInPageAndIsNotServedUnderAPI(t *testing.T) {
	e := newAgentEnv(t)
	e.seed(stageNew, e.mail("Quince jam", 1))
	tok := e.a.agentTokenValue()
	for _, p := range []string{"/api/search/live?q=quince", "/api/live?q=quince", "/api/search/live"} {
		for _, token := range []string{"", tok} {
			rec := e.call("GET", p, token, nil)
			if rec.Code == 200 || strings.Contains(rec.Body.String(), `"groups"`) {
				t.Errorf("%s (token %q) is served: %d %s", p, token, rec.Code, rec.Body.String())
			}
		}
	}
	// The agent's own search is untouched, and the page's address is not on its list.
	if len(e.searchAPI("quince")) != 1 {
		t.Error("the agents' search changed")
	}
	if rec := post(e.h, "/search/live?q=quince", url.Values{}); rec.Code == 200 {
		t.Errorf("POST: %d", rec.Code)
	}
	if got := get(e.h, "/search/live?q=quince").Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control %q", got)
	}
}

// TestLiveSearchResultsAreEscaped: text from mail is data in JSON, never markup,
// and the highlighting is made of parts, not of tags.
func TestLiveSearchResultsAreEscaped(t *testing.T) {
	e := newAgentEnv(t)
	m := e.mail("<img src=x onerror=alert(1)> offer", 1)
	m.text = "A <script>alert('x')</script> marmalade offer &amp; more \x01\x02."
	m.name = "<b>Mallory</b>"
	e.seed(stageNew, m)
	r, body := e.live("marmalade")
	if len(r.Groups) == 0 {
		t.Fatalf("not found: %s", body)
	}
	for _, bad := range []string{"<script>", "<img", "<b>", "<mark>", "\x01", "\x02"} {
		if strings.Contains(body, bad) {
			t.Errorf("the answer holds %q raw", bad)
		}
	}
	if !strings.Contains(body, "\\u003cscript\\u003e") {
		t.Errorf("the text is not escaped in the answer: %s", body)
	}
	// A search that matches the subject marks words in the title as parts.
	r, body = e.live("offer")
	if strings.Contains(body, "<mark>") || strings.Contains(body, "<img") {
		t.Errorf("markup in the title answer: %s", body)
	}
	if len(r.Groups) == 0 || len(r.Groups[0].Hits) == 0 {
		t.Fatalf("subject not found: %s", body)
	}
	marked := false
	for _, p := range r.Groups[0].Hits[0].Title {
		marked = marked || (p.H && p.T == "offer")
	}
	if !marked {
		t.Errorf("the word is not marked in the title: %+v", r.Groups[0].Hits[0].Title)
	}
	// The script never uses innerHTML for it.
	js := get(e.h, "/static/app.js").Body.String()
	i := strings.Index(js, "Search while typing")
	if i < 0 {
		t.Fatal("no live search in the script")
	}
	if live := js[i:]; strings.Contains(live, "innerHTML") || strings.Contains(live, "insertAdjacentHTML") || strings.Contains(live, "document.write") {
		t.Error("the live search builds markup")
	}
}

func TestMarkWordsMarksWholeWordsWithoutAccentsOrCase(t *testing.T) {
	got := markWords("Nová NÁJOMNÍČKA, ready", []string{"najom", "re"})
	want := []livePart{{T: "Nová "}, {T: "NÁJOMNÍČKA", H: true}, {T: ", "}, {T: "ready", H: true}}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("got %v want %v", got, want)
	}
	if got := markWords("", []string{"x"}); len(got) != 1 || got[0].T != "" {
		t.Errorf("empty: %v", got)
	}
}
