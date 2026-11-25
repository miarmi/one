package main

import (
	"bytes"
	"encoding/json"
	"html"
	"lab/apps/one/repo"
	"strings"
	"testing"
)

// The top line shows every strip, in the order of importance, and holds a link
// for the page's script to say how many do not fit (UX review B1).
func TestTheTopLineHoldsEveryStripInOrderAndHasTheMoreLink(t *testing.T) {
	e := newListEnv(t)
	data := pageData{EnvText: "development", Version: "t", ShowAgenda: true, Dot: dotFor(nil),
		Alerts: []string{"Gmail has stopped. See Connectors.", "iCloud Calendar needs login. See Connectors.", "Review paused: today's limit is reached"},
		List:   listView{Heading: "1 new, 2 in progress", Proposals: "2 to approve", Night: "Night check: 5 messages, 1 brought back", Waiting: "3 waiting for Claude"}}
	var buf bytes.Buffer
	if err := e.a.pages.ExecuteTemplate(&buf, "topbar", data); err != nil {
		t.Fatal(err)
	}
	page := html.UnescapeString(buf.String())
	last := -1
	for _, want := range []string{"Gmail has stopped", "iCloud Calendar needs login", "Review paused: today's limit is reached", "2 to approve", "Night check: 5 messages", "Dismiss", "3 waiting for Claude", `id="strip-more"`} {
		i := strings.Index(page, want)
		if i < 0 {
			t.Fatalf("the top line lacks %q", want)
		}
		if i < last {
			t.Errorf("%q is out of order", want)
		}
		last = i
	}
	if !strings.Contains(page, `id="strips"`) || !strings.Contains(page, `id="strip-more" href="/connectors" hidden`) {
		t.Error("the strips are not in one container with a hidden more link")
	}
}

// A paused review, items waiting for Claude and a night line all show at once;
// the page used to keep two strips and the others vanished silently.
func TestTheListShowsReviewPausedWaitingAndNightTogetherAndTheClaudeRowIsNotOKWhilePaused(t *testing.T) {
	e := newClaudeEnv(t)
	e.saveKey()
	e.a.store.SetSettingBy(e.ctx, keyDailyLimit, "0.01", "owner", e.now)
	e.seedMail("gmail", "erin@shop.test", "First", "<s1@shop.test>", "An invented first mail.", 1, true)
	e.seedMail("gmail", "erin@shop.test", "Second", "<s2@shop.test>", "An invented second mail.", 0.5, true)
	e.fake.script = func(n int, req map[string]any) fakeAnswer {
		r := say("Nothing is needed.")
		r.in = 100000
		return r
	}
	e.a.reviewPass(e.ctx) // the limit is used up; one item still waits
	d := e.nightDay()
	e.fake.script = func(n int, req map[string]any) fakeAnswer {
		return use("bring_back", map[string]any{"ref": ref(d.agent), "reason": "Tea offers end this week."})
	}
	e.a.nightCheck(e.ctx)
	page := html.UnescapeString(get(e.h, "/").Body.String())
	for _, want := range []string{"Review paused: today's limit is reached", "Night check:", "waiting for Claude"} {
		if !strings.Contains(page, want) {
			t.Errorf("the list lacks %q while the others show", want)
		}
	}
	for _, id := range []string{`id="night-strip" role="status">`, `id="claude-strip" role="status">`} {
		if !strings.Contains(page, id) {
			t.Errorf("a strip is hidden: %s", id)
		}
	}
	var st struct {
		Alerts []string `json:"alerts"`
		Night  string   `json:"night"`
		Claude string   `json:"claude"`
	}
	json.Unmarshal(get(e.h, "/list/state").Body.Bytes(), &st)
	if len(st.Alerts) != 1 || st.Night == "" || st.Claude == "" {
		t.Errorf("the state the page polls drops a strip: %+v", st)
	}
	// the Claude row does not say OK while the review is paused
	v := e.a.claudeViewFor(e.ctx)
	if v.Status == statusOK || !strings.Contains(v.Mode, "paused for today") {
		t.Errorf("the row says %q (%s) while paused", v.Status, v.Mode)
	}
}

func TestSettingsShowMoneyWithItsUnitAndOfferTheKnownModels(t *testing.T) {
	e := newClaudeEnv(t)
	page := get(e.h, "/settings").Body.String()
	for _, want := range []string{`<span class="unit" aria-hidden="true">$</span>`, `inputmode="decimal"`, `class="model-pick"`, `data-model="` + modelSonnet + `"`} {
		if !strings.Contains(page, want) {
			t.Errorf("the settings lack %s", want)
		}
	}
	if strings.Count(page, `class="unit"`) != 2 {
		t.Errorf("both money limits need a unit: %d", strings.Count(page, `class="unit"`))
	}
	if i := strings.Index(page, `name="`+keySkipMin+`"`); i < 0 || !strings.Contains(page[i:i+200], `inputmode="numeric"`) {
		t.Error("a whole number gets the numeric keyboard")
	}
}

func TestTheNoteFormOffersItemsAndPeopleToChooseFrom(t *testing.T) {
	e := newClaudeEnv(t)
	it := e.seedMail("gmail", "erin@shop.test", "Delivery of tea", "<n1@shop.test>", "An invented parcel text.", 1, false)
	page := get(e.h, "/notes/new").Body.String()
	for _, want := range []string{`list="n-items"`, `<datalist id="n-items">`, `value="` + repo.Number(it.Number) + `"`, `list="n-people"`, `<datalist id="n-people">`} {
		if !strings.Contains(page, want) {
			t.Errorf("the note form lacks %s", want)
		}
	}
}

func TestTheAgendaAddBoxIsNotInsideTheToDoSection(t *testing.T) {
	e := newClaudeEnv(t)
	page := get(e.h, "/agenda").Body.String()
	add := strings.Index(page, `class="ag-add"`)
	end := strings.Index(page, "</section>\n<form")
	if add < 0 || end < 0 || end > add {
		t.Errorf("the box to add a to-do must come after the section so it can stay at the bottom (add %d, end %d)", add, end)
	}
	if !strings.Contains(page, ">Add a to-do<") {
		t.Error("the label changed")
	}
}
