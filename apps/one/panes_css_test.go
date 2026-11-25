package main

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

func panesCSS(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("web/style.css")
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// SPEC 5: the three parts of an open row sit side by side in the middle pane once it is 700 px wide.
func TestOpenRowGoesSideBySideInAWideMiddlePane(t *testing.T) {
	css := panesCSS(t)
	re := regexp.MustCompile(`@container page \(min-width: 43\.75rem\) \{[^}]*body:is\(\.page-panes, \.page-item\) \.row-body \{ grid-template-columns: 7\.5rem minmax\(0, 1fr\) minmax\(14rem, 18rem\)`)
	if !re.MatchString(css) {
		t.Error("no container query that puts who, what and the thread side by side from 700 px")
	}
}

// SPEC 7: the Attachments heading is inside an open row, so a rule that hides the stage headers must not reach it.
func TestPanesHideOnlyTheStageHeadersNotTheAttachmentsHeading(t *testing.T) {
	css := panesCSS(t)
	if strings.Contains(css, "body:is(.page-panes, .page-item) .group h3,") {
		t.Error("every h3 under a group is hidden in the panes, the Attachments heading too")
	}
	if !strings.Contains(css, "body:is(.page-panes, .page-item) .group > h3,") {
		t.Error("the stage headers are no longer hidden on a laptop")
	}
}

// SPEC 23: Back to the queue shows in the ribbon of a page of its own.
func TestBackToTheQueueIsNotHiddenOnALaptop(t *testing.T) {
	css := panesCSS(t)
	if regexp.MustCompile(`\.top \.top-back[^{]*\{ display: none`).MatchString(css) ||
		regexp.MustCompile(`\.top \.top-back,[^{]*\{ display: none`).MatchString(css) {
		t.Error("the back link is hidden on a laptop")
	}
}

// SPEC 1: one type, 13 px for text and 12 px for times: nothing in the ribbon is set to another size.
func TestRibbonNameIsInTheTextSize(t *testing.T) {
	if strings.Contains(panesCSS(t), "h1 { margin: 0; font-size: 15px") {
		t.Error("the name ONE is 15 px in the ribbon")
	}
}

// SPEC 2: a row that the owner has moved on stays put, so the sentence about an empty stage must not appear above it.
func TestEmptyStageSentenceWaitsForTheRowsThatStay(t *testing.T) {
	b, err := os.ReadFile("web/app.js")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "var empty = n === 0 && held === 0;") {
		t.Error("the heading says a stage is empty while a moved row still stands in it")
	}
}

// SPEC 1: no text sits at font-size 0 (the one-type-family check counts it as a third size);
// words are hidden with the clip pattern, and a decorative mark carries no text at all.
func TestNoTextIsHiddenWithFontSizeZero(t *testing.T) {
	css := panesCSS(t)
	if re := regexp.MustCompile(`font-size:\s*0\s*(!important)?\s*[;}]`); re.MatchString(css) {
		t.Errorf("style.css hides text with font-size 0: %s", re.FindString(css))
	}
	for _, f := range []string{"web/topbar.html", "web/agenda.html"} {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		s := string(b)
		if strings.Contains(s, `class="dot-mark" aria-hidden="true">{{`) {
			t.Errorf("%s: the dot mark carries text", f)
		}
		if strings.Contains(s, `ag-done">Done</button>`) && strings.Contains(s, `class="ag-td"`) && !strings.Contains(s, `ag-done-t`) {
			t.Errorf("%s: the to-do tick box has its word as a bare text node", f)
		}
	}
	if !strings.Contains(css, ".ag-done .ag-done-t { position: absolute; width: 1px; height: 1px; overflow: hidden; clip:") {
		t.Error("the tick box word is not hidden with the clip pattern")
	}
}

// SPEC 5: on a phone the note box of the open thread is fixed at the bottom of the screen however many rows follow it.
// A container is the containing block of a fixed box, so the list may be one only where container queries can match.
func TestPhoneNoteBoxIsFixedAndTheListIsNoContainerOnAPhone(t *testing.T) {
	css := panesCSS(t)
	if !strings.Contains(css, ".row.pinned .thread .say-bar { position: fixed; left: 0; right: 0; bottom: 0;") {
		t.Error("no rule that fixes the note box of the pinned row at the bottom of the screen")
	}
	for _, sel := range []string{".list-page { container-type", ".rows { container-type"} {
		i := strings.Index(css, sel)
		if i < 0 {
			t.Fatalf("%s: rule missing", sel)
		}
		line := css[strings.LastIndex(css[:i], "\n")+1 : i]
		if !strings.HasPrefix(line, "@media (min-width: 56rem) {") {
			t.Errorf("%s is not inside @media (min-width: 56rem); on a phone it would hold the fixed note box inside the list", sel)
		}
	}
	js, err := os.ReadFile("web/app.js")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(js), "function pinNote()") || !strings.Contains(string(js), "pinNote();\n    return fresh;") {
		t.Error("app.js does not pin the note box after a row is swapped")
	}
}
