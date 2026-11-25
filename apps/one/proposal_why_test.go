package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"lab/apps/one/repo"
)

// SPEC 12: the closed row says why, in the words the agent gave.

func (e *agentEnv) proposeNoReason(it repo.Item, action string) int64 {
	e.t.Helper()
	var id int64
	err := e.a.store.Update(e.ctx, func(tx *repo.Tx) error {
		var err error
		id, err = tx.InsertProposal(e.ctx, repo.Proposal{ItemID: it.ID, Action: action, Reason: "", By: "claude", At: 1,
			BaseStage: it.Stage, BaseMsgAt: tx.NewestMessageAt(e.ctx, it.ID)})
		if err != nil {
			return err
		}
		_, err = tx.AnswerReview(e.ctx, it.ID, 1)
		return err
	})
	if err != nil {
		e.t.Fatal(err)
	}
	return id
}

func TestClosedRowShowsTheReasonAndKeepsTheButtonsInOneGroup(t *testing.T) {
	e := newAgentEnv(t)
	it := e.newItem(stageNew, true, "Invoice", "")
	e.propose(it, "resolve", "We went through this one together on Friday.")
	row := get(e.h, "/list/new").Body.String()
	if !strings.Contains(row, `<span class="prop-why" title="We went through this one together on Friday.">We went through this one together on Friday.</span>`) {
		t.Errorf("the reason is not on the closed row: %s", row)
	}
	if !strings.Contains(row, "prop-chip has-why") {
		t.Error("the chip is not marked as having a reason")
	}
	i := strings.Index(row, `class="prop-btns"`)
	j := strings.Index(row[i:], "</span>")
	grp := row[i : i+j]
	if !strings.Contains(grp, ">Approve<") || !strings.Contains(grp, ">Dismiss<") {
		t.Errorf("Approve and Dismiss are not in one group: %s", grp)
	}
	// opened, the thread shows the whole reason
	open := get(e.h, fmt.Sprintf("/items/%d/row?open=1", it.ID)).Body.String()
	if !strings.Contains(open, `<p class="reason">We went through this one together on Friday.</p>`) {
		t.Error("the opened row lacks the full reason")
	}
}

func TestClosedRowEscapesTheReasonAndShortensVeryLongOnes(t *testing.T) {
	e := newAgentEnv(t)
	it := e.newItem(stageNew, true, "Invoice", "")
	e.propose(it, "resolve", `<b>bold</b> & "quoted" `+strings.Repeat("long ", 95))
	row := get(e.h, "/list/new").Body.String()
	if strings.Contains(row, "<b>bold</b>") {
		t.Error("the reason was not escaped")
	}
	if !strings.Contains(row, "&lt;b&gt;bold&lt;/b&gt; &amp;") {
		t.Errorf("the escaped reason is missing: %s", row)
	}
	if !strings.Contains(row, "…") {
		t.Error("a very long reason is not cut for the closed row")
	}
}

func TestClosedRowWithoutReasonHasNoEmptyReasonBox(t *testing.T) {
	e := newAgentEnv(t)
	it := e.newItem(stageNew, true, "Invoice", "")
	e.proposeNoReason(it, "resolve")
	row := get(e.h, "/list/new").Body.String()
	if strings.Contains(row, "prop-why") || strings.Contains(row, "has-why") {
		t.Errorf("a proposal without a reason shows a reason box: %s", row)
	}
	if !strings.Contains(row, ">Approve<") {
		t.Error("the buttons are gone")
	}
}

// SPEC 5: on a phone the open row's actions are icons in the one bar with the note box; each keeps its word as its name.
func TestOpenRowBarHoldsActionsAttachAndSendWithTheirNames(t *testing.T) {
	e := newChatEnv(t)
	it := e.seed(stageInProgress, seedMsg{subject: "Faktura", name: "Alice", sender: "alice@example.test", text: "Prosím.", at: e.ago(1)})
	page := e.rowPage(it.ID)
	i := strings.Index(page, `class="say-bar"`)
	if i < 0 {
		t.Fatal("no bar")
	}
	bar := page[i:]
	for _, name := range []string{"Mark pending", "Resolve", "To do", "Attach", "Send"} {
		if !strings.Contains(bar, `aria-label="`+name+`" title="`+name+`"`) {
			t.Errorf("the bar has no %q button with a name and a title", name)
		}
	}
	if strings.Index(bar, "bar-acts") > strings.Index(bar, "notebox") {
		t.Error("the actions do not come before the note box")
	}
}

// SPEC 19: on a phone the to-dos are a group of the list after In progress, folded, with Done and the box to add one.
func TestListPageHasAFoldedToDoGroupAfterInProgress(t *testing.T) {
	e := newListEnv(t)
	e.seed(stageNew, e.mail("one", 1))
	page := get(e.h, "/").Body.String()
	if !strings.Contains(page, "Nothing to do.") || !strings.Contains(page, ">Add a to-do<") {
		t.Error("the empty To do group lacks its sentence or its box")
	}
	e.ajaxPost("/agenda/todos", url.Values{"text": {"Call <b>the</b> bank"}})
	page = get(e.h, "/").Body.String()
	ip, td, pe := strings.Index(page, `class="group" data-stage="in_progress"`), strings.Index(page, `class="group group-todo"`), strings.Index(page, `class="group" data-stage="pending"`)
	if !(ip < td && td < pe) {
		t.Errorf("the To do group is not between In progress and Pending: %d %d %d", ip, td, pe)
	}
	if !strings.Contains(page, `data-count="todo">1<`) || !strings.Contains(page, "Call &lt;b&gt;the&lt;/b&gt; bank") || !strings.Contains(page, ">Done<") {
		t.Error("the group lacks its count, its escaped words or Done")
	}
}

// SPEC 17: a phone lists a chat only when a thread grew from it; the laptop lists every chat.
func TestPhoneListLeavesOutBareChatsInRowsCountsAndPaging(t *testing.T) {
	e := newChatEnv(t)
	bare := e.chatItemForReply("messenger", "c1", "Ondra", "Jsi tam?")
	e.seed(stageInProgress, e.mail("A mail", 2))
	list := func(phone bool, path string) string {
		req := httptest.NewRequest("GET", path, nil)
		if phone {
			req.AddCookie(&http.Cookie{Name: "one-phone", Value: "1"})
		}
		rec := httptest.NewRecorder()
		e.h.ServeHTTP(rec, req)
		return rec.Body.String()
	}
	if !strings.Contains(list(false, "/list/in_progress"), "Ondra") {
		t.Fatal("the laptop does not list the chat")
	}
	if strings.Contains(list(true, "/list/in_progress"), "Ondra") {
		t.Error("the phone lists a bare chat")
	}
	if !strings.Contains(list(true, "/list/in_progress"), "A mail") {
		t.Error("the phone lost the mail")
	}
	count := func(phone bool) string {
		return list(phone, "/list/state")
	}
	if !strings.Contains(count(false), `"in_progress":2`) || !strings.Contains(count(true), `"in_progress":1`) {
		t.Errorf("counts: laptop %s phone %s", count(false), count(true))
	}
	if !strings.Contains(list(true, "/"), `data-count="in_progress">1<`) || !strings.Contains(list(false, "/"), `data-count="in_progress">2<`) {
		t.Error("the first paint counts do not follow the layout")
	}
	// a note makes the chat listed
	e.agentEnv.ajaxPost(fmt.Sprintf("/items/%d/notes", bare.ID), url.Values{"text": {"Remind me"}})
	if !strings.Contains(list(true, "/list/in_progress"), "Ondra") || !strings.Contains(count(true), `"in_progress":2`) {
		t.Error("a chat with a note of the owner's is not listed on the phone")
	}
}

// SPEC 5: a closed row on a phone carries the resolve icon itself (Reopen in Resolved), outside the button that opens the row.
func TestClosedRowCarriesOneTapResolveOrReopen(t *testing.T) {
	e := newListEnv(t)
	e.seed(stageInProgress, e.mail("working", 1))
	e.seed(stageResolved, e.mail("done", 2))
	prog := get(e.h, "/list/in_progress").Body.String()
	if !strings.Contains(prog, `class="state-act quick-act"`) || !strings.Contains(prog, `aria-label="Resolve" title="Resolve"`) || strings.Contains(prog, `quick-act" data-ajax="row"><button type="submit" class="tertiary quick-btn" aria-label="Reopen"`) {
		t.Error("a closed row in progress lacks the Resolve icon")
	}
	i := strings.Index(prog, "quick-act")
	if strings.LastIndex(prog[:i], "</button>") < strings.LastIndex(prog[:i], "js-row-toggle") {
		t.Error("the quick button is inside the button that opens the row")
	}
	res := get(e.h, "/list/resolved").Body.String()
	if !strings.Contains(res, `class="tertiary quick-btn" aria-label="Reopen" title="Reopen"`) {
		t.Error("a row in Resolved lacks the Reopen icon")
	}
}

// SPEC 3: Claude can bring a Pending item back to New with one sentence, without Approve.
func TestBringBackWorksOnPendingIsRefusedElsewhereAndNeedsAReason(t *testing.T) {
	e := newAgentEnv(t)
	pend := e.newItem(stagePending, false, "Offer", "")
	m := e.post("/api/items/"+ref(pend)+"/bring-back", map[string]any{"reason": "The supplier wrote on another thread that the date moved."})
	if m["ok"] != true {
		t.Fatalf("bring back: %v", m)
	}
	if e.stageOf(pend) != stageNew {
		t.Fatalf("stage %s", e.stageOf(pend))
	}
	row := get(e.h, fmt.Sprintf("/items/%d/row?open=1", pend.ID)).Body.String()
	if !strings.Contains(row, "Claude: The supplier wrote on another thread that the date moved.") {
		t.Errorf("the item does not say Claude: <reason>: %s", row)
	}
	// not Pending: refused, nothing changes
	prog := e.newItem(stageInProgress, false, "Other", "")
	if m := e.post("/api/items/"+ref(prog)+"/bring-back", map[string]any{"reason": "x"}); m["ok"] == true || e.stageOf(prog) != stageInProgress {
		t.Errorf("an item that is not Pending was brought back: %v", m)
	}
	// a reason is required
	pend2 := e.newItem(stagePending, false, "Third", "")
	if m := e.post("/api/items/"+ref(pend2)+"/bring-back", map[string]any{"reason": "  "}); m["ok"] == true || e.stageOf(pend2) != stagePending {
		t.Errorf("a bring back without a reason worked: %v", m)
	}
	// the tool is in the connector
	found := false
	for _, tl := range mcpToolList() {
		found = found || tl.Name == "bring_back"
	}
	if !found {
		t.Error("the connector has no bring_back")
	}
}
