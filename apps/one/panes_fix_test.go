package main

import (
	"strings"
	"testing"
)

// UX review of the three panes (2026-11-20): what the markup must carry so that the style sheet can keep a row to one line.

func TestRowWithProposalCarriesWhatTheLineNeedsToGiveWay(t *testing.T) {
	e := newAgentEnv(t)
	plain := e.newItem(stageNew, false, "Plain row", "")
	prop := e.newItem(stageNew, false, "Row with a proposal", "")
	e.propose(prop, "resolve", "Looks finished.")
	pend := e.newItem(stagePending, false, "Waiting row", "")
	e.propose(pend, "resolve", "Looks finished.")
	_ = plain

	news := get(e.h, "/list/new").Body.String()
	row := func(page, subject string) string {
		i := strings.Index(page, subject)
		if i < 0 {
			t.Fatalf("no row %q", subject)
		}
		start := strings.LastIndex(page[:i], `<li class="row`)
		return page[start : i+strings.Index(page[i:], "</li>")]
	}
	if r := row(news, "Plain row"); strings.Contains(r, "has-prop") || strings.Contains(r, "has-wait") {
		t.Errorf("a plain row is marked: %.200s", r)
	}
	r := row(news, "Row with a proposal")
	for _, want := range []string{`class="row has-prop"`, `<span class="prop-by">Claude: </span>`, `title="Claude: Resolve"`} {
		if !strings.Contains(r, want) {
			t.Errorf("a row with a proposal lacks %q:\n%s", want, r)
		}
	}
	if strings.Contains(r, "has-wait") {
		t.Error("a new row is marked as waiting")
	}
	// the words of the proposal are still one text, whatever the style sheet shortens
	if !strings.Contains(r, `</span>Resolve</span>`) {
		t.Errorf("the proposal's words are not in one piece:\n%s", r)
	}
	pr := row(get(e.h, "/list/pending").Body.String(), "Waiting row")
	if !strings.Contains(pr, "has-prop") {
		t.Errorf("a pending row with a proposal is not marked:\n%s", pr)
	}
}

func TestNavLinksAndRefreshKeepAnAccessibleNameWhenTheirWordsAreDropped(t *testing.T) {
	e := newAgentEnv(t)
	for _, path := range []string{"/", "/notes", "/settings"} {
		head := pageBetween(t, get(e.h, path).Body.String(), `<header class="top`, "</header>")
		for _, want := range []string{`title="Notes"`, `title="People"`, `title="Connectors"`, `title="Settings"`, `title="Refresh"`,
			"<span>Notes</span>", "<span>Settings</span>", `class="rb-label">Refresh</span>`} {
			if !strings.Contains(head, want) {
				t.Errorf("%s: the ribbon lacks %q", path, want)
			}
		}
	}
}

func TestClaudeLineLeadsToTheClaudeSectionLikeTheNightCheck(t *testing.T) {
	e := newAgentEnv(t)
	page := get(e.h, "/").Body.String()
	conns := pageBetween(t, page, `<section class="conns"`, "</section>")
	if !strings.Contains(conns, `<a class="conn-line" href="/connectors#claude" title="Claude: `) {
		t.Errorf("the Claude line does not lead to /connectors#claude:\n%s", conns)
	}
}

func TestScriptKeepsTheWindowStillAndSaysWhenARowDoesNotOpen(t *testing.T) {
	js := get(newListEnv(t).h, "/static/app.js").Body.String()
	for _, want := range []string{
		`function reveal(`,                        // only the middle pane is scrolled
		`preventScroll: true`,                     // a focus never moves the window
		`This did not open. Try again.`,           // a row that fails to open says so
		`function rove(`,                          // the list is one stop for Tab
		`"ArrowDown"`,                             // and the arrow keys move inside it
		`classList.toggle("is-empty"`,             // an empty stage puts its sentence under the heading
		`window.matchMedia("(min-width: 68rem)")`, // the three panes start at 68rem
	} {
		if !strings.Contains(js, want) {
			t.Errorf("the script lacks %s", want)
		}
	}
	if strings.Contains(js, "62rem") {
		t.Error("the script still uses the old breakpoint")
	}
	css := get(newListEnv(t).h, "/static/style.css").Body.String()
	if strings.Contains(css, "62rem") || strings.Contains(css, "61.99rem") {
		t.Error("the style sheet still uses the old breakpoint")
	}
	for _, want := range []string{`html:has(> body.page-panes)`, `overflow: clip`, `--pane-l`, `--pane-r`, `@media (min-width: 68rem)`} {
		if !strings.Contains(css, want) {
			t.Errorf("the style sheet lacks %s", want)
		}
	}
}
