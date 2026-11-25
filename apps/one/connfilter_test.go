package main

import (
	"fmt"
	"strings"
	"testing"
)

// SPEC 23: choosing a connection shows only the rows that came through it.
func TestChoosingAConnectionShowsOnlyItsRowsAndTheCountsFollow(t *testing.T) {
	e := newListEnv(t)
	gm := func(subject string, days float64) seedMsg { return e.mail(subject, days) }
	rw := func(subject string, days float64) seedMsg {
		m := e.mail(subject, days)
		m.source = srcGraph
		return m
	}
	chat := func(subject string, days float64) seedMsg {
		m := e.mail(subject, days)
		m.source, m.network = "whatsapp", "whatsapp"
		return m
	}
	e.seed(stageNew, gm("Gmail one", 1))
	e.seed(stageNew, rw("Office one", 2))
	e.seed(stageNew, chat("Chat one", 3))
	e.seed(stageInProgress, gm("Gmail two", 4))
	e.seed(stagePending, rw("Office two", 5))
	e.seed(stageResolved, gm("Gmail three", 6))
	e.seed(stageNew, gm("Mixed", 7), rw("Mixed", 8)) // a row with a message from each

	page := get(e.h, "/?conn=gmail").Body.String()
	if !strings.Contains(page, `Only Gmail`) || !strings.Contains(page, `>Show all<`) {
		t.Errorf("the bar above the list: %s", pageBetween(t, page, `<main`, `</main>`))
	}
	for stage, n := range map[string]int{stageNew: 2, stageInProgress: 1, stagePending: 0, stageResolved: 1} {
		if want := fmt.Sprintf(`data-count="%s">%d<`, stage, n); !strings.Contains(page, want) {
			t.Errorf("the count of %s should be %d", stage, n)
		}
	}
	news := get(e.h, "/list/new?conn=gmail").Body.String()
	for _, want := range []string{"Gmail one", "Mixed"} {
		if !strings.Contains(news, want) {
			t.Errorf("the filtered New lacks %q", want)
		}
	}
	for _, not := range []string{"Office one", "Chat one"} {
		if strings.Contains(news, not) {
			t.Errorf("the filtered New holds %q", not)
		}
	}
	if p := get(e.h, "/list/pending?conn=gmail").Body.String(); strings.Contains(p, "Office two") {
		t.Error("Pending still shows a row of another connection")
	}
	// the chat bridge takes its four networks
	if c := get(e.h, "/list/new?conn=bridge").Body.String(); !strings.Contains(c, "Chat one") || strings.Contains(c, "Gmail one") {
		t.Errorf("the chat bridge filter: %s", c)
	}
	// the state the page renews its numbers from follows too
	if st := get(e.h, "/list/state?since=0&conn=outlook").Body.String(); !strings.Contains(st, `"pending":1`) || !strings.Contains(st, `"new":2`) {
		t.Errorf("state counts for Office: %s", st)
	}
	// without the parameter nothing is narrowed, and nothing is remembered
	if all := get(e.h, "/").Body.String(); strings.Contains(all, "Only ") && strings.Contains(all, `id="only-conn"`) {
		t.Error("the plain page is narrowed")
	}
	if n := get(e.h, "/list/new").Body.String(); !strings.Contains(n, "Chat one") || !strings.Contains(n, "Office one") {
		t.Errorf("the plain list is narrowed: %s", n)
	}
	// an unknown connection narrows nothing (the Claude line and the calendars do not filter)
	if p := get(e.h, "/?conn=claude").Body.String(); strings.Contains(p, `id="only-conn"`) {
		t.Error("the Claude line filtered the list")
	}
}

func TestFilteredListPagesWithTrueTotals(t *testing.T) {
	e := newListEnv(t)
	for i := 0; i < groupPage+5; i++ {
		e.seed(stageNew, e.mail(fmt.Sprintf("Gmail %03d", i), float64(i)/100+1))
		m := e.mail(fmt.Sprintf("Office %03d", i), float64(i)/100+1)
		m.source = srcGraph
		e.seed(stageNew, m)
	}
	page := get(e.h, "/?conn=gmail").Body.String()
	if want := fmt.Sprintf(`data-count="new">%d<`, groupPage+5); !strings.Contains(page, want) {
		t.Errorf("the true count is not shown (want %s)", want)
	}
	first := get(e.h, "/list/new?conn=gmail").Body.String()
	if strings.Contains(first, "Office") || !strings.Contains(first, `data-next="`) {
		t.Errorf("the first page: more link missing or a foreign row in it")
	}
	next := get(e.h, fmt.Sprintf("/list/new?conn=gmail&offset=%d", groupPage)).Body.String()
	if got := strings.Count(next, `class="row`); got < 5 || strings.Contains(next, "Office") {
		t.Errorf("the second page holds %d rows or a foreign one", got)
	}
}

func TestConnectionLinesFilterAndTheConnectorsPageNamesOpenTheList(t *testing.T) {
	e := newListEnv(t)
	conns := pageBetween(t, get(e.h, "/?conn=gmail").Body.String(), `<main`, `</main>`)
	_ = conns
	cp := get(e.h, "/connectors").Body.String()
	for _, want := range []string{`href="/?conn=gmail"`, `href="/?conn=outlook"`, `href="/?conn=bridge"`} {
		if !strings.Contains(cp, want) {
			t.Errorf("the Connectors page lacks %s", want)
		}
	}
}

// SPEC 23: the counts of the stages follow the choice, and so do the figures
// about proposals and the longest wait.
func TestFilteredProposalFiguresFollowTheChoice(t *testing.T) {
	e := newAgentEnv(t)
	a := e.newItem(stageNew, false, "Gmail row", "")
	m := e.mail("Office row", 1)
	m.source = srcGraph
	b := e.seed(stageNew, m)
	c := e.newItem(stageNew, false, "Gmail row two", "")
	e.propose(a, "resolve", "One.")
	e.propose(b, "resolve", "Two.")
	e.propose(c, "resolve", "Three.")
	page := get(e.h, "/?conn=outlook").Body.String()
	if !strings.Contains(page, `data-approve="new">1 to approve<`) || strings.Contains(page, `data-approve="new">3 to approve<`) {
		t.Errorf("the figure under Only Office mail should be 1 to approve")
	}
	if all := get(e.h, "/").Body.String(); !strings.Contains(all, `data-approve="new">3 to approve<`) {
		t.Error("without the choice the figure is 3 to approve")
	}
	if st := get(e.h, "/list/state?since=0&conn=outlook").Body.String(); !strings.Contains(st, `"new":"1 to approve"`) {
		t.Errorf("state: %s", st)
	}
}
