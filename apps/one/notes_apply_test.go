package main

import (
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"
)

// Tests of SPEC 11 (notes that apply, reasons) and the top line `<n> to do`.
// All names, addresses and texts are invented.

func (e *agentEnv) saveNoteAs(args map[string]any) float64 {
	e.t.Helper()
	m := e.post("/api/notes", args)
	if m["ok"] != true {
		e.t.Fatalf("save_note %v: %v", args, m)
	}
	return m["note_id"].(float64)
}

func idsOf(list any) map[float64]map[string]any {
	out := map[float64]map[string]any{}
	arr, _ := list.([]any)
	for _, x := range arr {
		m := x.(map[string]any)
		out[m["note_id"].(float64)] = m
	}
	return out
}

func TestNotesThatApplyComeWithGetItemAndTheQueueAndOthersDoNot(t *testing.T) {
	e := newAgentEnv(t)
	e.makePerson("mail", "alice@example.test", "Alice Example")
	it := e.newItem(stageNew, true, "Your standing order", "")
	other := e.newItem(stageNew, true, "Something else", "")
	// Notes that apply, each for its own reason.
	byItem := e.saveNoteAs(map[string]any{"title": "About this item", "text": "The owner looked at this one.", "item": ref(it)})
	byPerson := e.saveNoteAs(map[string]any{"title": "About Alice", "text": "Alice is a friend.", "person": "Alice Example"})
	byAddr := e.saveNoteAs(map[string]any{"title": "Standing order", "text": "Alice's monthly mail is expected.", "sender": "Alice@Example.test"})
	byDomain := e.saveNoteAs(map[string]any{"title": "Everything at example.test", "text": "Fine to read late.", "sender": "@example.test"})
	byMentionAddr := e.saveNoteAs(map[string]any{"title": "Mentions the address", "text": "Mail from alice@example.test is the rent."})
	byMentionDomain := e.saveNoteAs(map[string]any{"title": "Mentions the domain", "text": "Whatever comes from example.test, ignore the ads."})
	byMentionName := e.saveNoteAs(map[string]any{"title": "Mentions the name", "text": "Alice Example sends the club news."})
	// Notes that do not.
	e.saveNoteAs(map[string]any{"title": "Another item", "text": "Not this.", "item": ref(other)})
	otherPerson := e.makePerson("mail", "bob@else.test", "Bob Else")
	_ = otherPerson
	e.saveNoteAs(map[string]any{"title": "About Bob", "text": "Bob is a neighbour.", "person": "Bob Else"})
	e.saveNoteAs(map[string]any{"title": "Other sender", "text": "Expected.", "sender": "carol@else.test"})
	e.saveNoteAs(map[string]any{"title": "Other domain", "text": "Expected.", "sender": "else.test"})
	e.saveNoteAs(map[string]any{"title": "Near miss", "text": "Mail from myexample.test and example.testing is not it."})
	e.saveNoteAs(map[string]any{"title": "Standing alone", "text": "The owner likes short mails."})

	got := e.getJSON("/api/items/" + ref(it))
	applying := idsOf(got["notes_that_apply"])
	want := map[float64]string{byItem: "attached to this item", byPerson: "Alice Example", byAddr: "alice@example.test",
		byDomain: "example.test", byMentionAddr: "mentions alice@example.test", byMentionDomain: "mentions example.test", byMentionName: "mentions alice example"}
	if len(applying) != len(want) {
		t.Fatalf("%d notes apply, want %d: %v", len(applying), len(want), got["notes_that_apply"])
	}
	for id, why := range want {
		n, ok := applying[id]
		if !ok {
			t.Errorf("note %v does not come with get_item", id)
			continue
		}
		if !strings.Contains(strings.ToLower(n["applies_because"].(string)), strings.ToLower(why)) {
			t.Errorf("note %v applies because %q, want %q", id, n["applies_because"], why)
		}
		for _, f := range []string{"title", "text", "saved_by", "saved_at"} {
			if s, _ := n[f].(string); s == "" {
				t.Errorf("note %v has no %s", id, f)
			}
		}
	}
	if applying[byAddr]["saved_by"] != "Claude" || got["notes_rule"] == nil {
		t.Errorf("who saved it, and the rule: %v", applying[byAddr])
	}
	// The queue: titles and ids only.
	q := e.getJSON("/api/queue")
	rows, _ := q["items"].([]any)
	found := false
	for _, r := range rows {
		row := r.(map[string]any)
		if row["id"] != ref(it) {
			continue
		}
		found = true
		qn := idsOf(row["notes_that_apply"])
		if len(qn) != len(want) {
			t.Errorf("the queue row holds %d notes, want %d", len(qn), len(want))
		}
		for id, n := range qn {
			if _, ok := want[id]; !ok {
				t.Errorf("the queue row holds note %v", id)
			}
			if len(n) != 2 || n["title"] == nil {
				t.Errorf("a queue note must be an id and a title only: %v", n)
			}
		}
	}
	if !found {
		t.Fatal("the item is not in the queue")
	}
	// The same through the tool.
	text, isErr := e.tool("get_item", map[string]any{"id": ref(it)})
	if isErr || !strings.Contains(text, "notes_that_apply") || !strings.Contains(text, "Standing order") {
		t.Errorf("get_item tool: %v %.200s", isErr, text)
	}
	// Another item gets the notes attached to it, not those attached to this one.
	oth := idsOf(e.getJSON("/api/items/" + ref(other))["notes_that_apply"])
	if _, ok := oth[byItem]; ok || len(oth) != len(want) {
		t.Errorf("the other item gets %d notes (the first item's own note: %v)", len(oth), ok)
	}
}

func TestSaveNoteAboutASenderIsCheckedAndKeptWhenOwnerEditsIt(t *testing.T) {
	e := newAgentEnv(t)
	for _, bad := range []string{"two words", "a@b@c.test", "nodot", "@", "x@nodot"} {
		if m := e.post("/api/notes", map[string]any{"title": "T", "text": "x", "sender": bad}); m["_status"] != float64(400) {
			t.Errorf("sender %q: %v", bad, m)
		}
	}
	id := int64(e.saveNoteAs(map[string]any{"title": "Club", "text": "Fine.", "sender": "News@Club.test"}))
	n, _ := e.a.store.NoteByID(e.ctx, id)
	if n.Sender != "news@club.test" {
		t.Errorf("sender kept as %q", n.Sender)
	}
	page := get(e.h, fmt.Sprintf("/notes/%d", id)).Body.String()
	if !strings.Contains(page, `name="sender"`) || !strings.Contains(page, `value="news@club.test"`) {
		t.Error("the edit page does not show the sender")
	}
	rec := e.ajaxPost(fmt.Sprintf("/notes/%d", id), url.Values{"title": {"Club"}, "body": {"Changed."}, "sender": {"news@club.test"}})
	if rec.Code != 303 {
		t.Fatalf("edit: %d", rec.Code)
	}
	if n, _ := e.a.store.NoteByID(e.ctx, id); n.Sender != "news@club.test" || n.Body != "Changed." {
		t.Errorf("after the owner's edit: %+v", n)
	}
}

func TestResolveItemKindOldNeedsAReasonAndShowsInWords(t *testing.T) {
	e := newAgentEnv(t)
	it := e.newItem(stageNew, true, "Payment due 3 March", "")
	if m := e.post("/api/items/"+ref(it)+"/skip", map[string]any{"kind": "old"}); m["_status"] != float64(400) && m["_status"] != float64(403) {
		t.Errorf("old without a reason: %v", m)
	}
	if e.stageOf(it) != stageNew {
		t.Fatal("the item moved without a reason")
	}
	text, isErr := e.tool("resolve_item", map[string]any{"id": ref(it), "kind": "old", "reason": "The invoice was due on 3 March and that is long past."})
	if isErr {
		t.Fatalf("resolve_item old: %s", text)
	}
	if e.stageOf(it) != stageClosed {
		t.Fatal("not closed")
	}
	evs := e.events(it.ID, "skipped")
	if len(evs) != 1 || !strings.Contains(evs[0].Data, `"reason":"old: The invoice was due on 3 March`) || !strings.Contains(evs[0].Data, `"kind":"old"`) {
		t.Fatalf("event: %+v", evs)
	}
	closed := get(e.h, "/closed").Body.String()
	for _, want := range []string{"Kind: Old: its date has passed", "Reason: old: The invoice was due on 3 March"} {
		if !strings.Contains(closed, want) {
			t.Errorf("the Closed page lacks %q", want)
		}
	}
	// A reason that already begins old: is not doubled.
	it2 := e.newItem(stageNew, true, "Another old one", "")
	e.post("/api/items/"+ref(it2)+"/skip", map[string]any{"kind": "old", "reason": "old: due 1 May"})
	if ev := e.events(it2.ID, "skipped"); len(ev) != 1 || !strings.Contains(ev[0].Data, `"reason":"old: due 1 May"`) {
		t.Errorf("doubled prefix: %+v", ev)
	}
	// Noise and finished are shown in words too.
	it3 := e.newItem(stageNew, true, "Ads", "")
	e.post("/api/items/"+ref(it3)+"/skip", map[string]any{"kind": "noise", "reason": "Nothing in it needs anything."})
	if !strings.Contains(get(e.h, "/closed").Body.String(), "Kind: Noise") {
		t.Error("noise is not shown as noise")
	}
	if m := e.post("/api/items/"+ref(e.newItem(stageNew, true, "x", ""))+"/skip", map[string]any{"kind": "spam", "reason": "x"}); m["_status"] != float64(400) {
		t.Errorf("an unknown kind: %v", m)
	}
}

func TestResolveItemWithANoteIdNamesTheNoteAndNoiseNeedsSameAsUsual(t *testing.T) {
	e := newAgentEnv(t)
	note := int64(e.saveNoteAs(map[string]any{"title": "Standing order", "text": "The monthly mail from Alice is expected.", "sender": "alice@example.test"}))
	unrelated := int64(e.saveNoteAs(map[string]any{"title": "Unrelated", "text": "Nothing to do with this.", "sender": "zed@else.test"}))
	it := e.newItem(stageNew, true, "Monthly statement", "")
	path := "/api/items/" + ref(it) + "/skip"
	// Noise by a note needs the sentence.
	if m := e.post(path, map[string]any{"kind": "noise", "reason": "Expected mail.", "note_id": note}); m["_status"] != float64(400) || !strings.Contains(m["error"].(string), "same_as_usual") {
		t.Errorf("noise by a note without the sentence: %v", m)
	}
	if m := e.post(path, map[string]any{"kind": "noise", "reason": "x", "same_as_usual": "Same as always."}); m["_status"] != float64(400) {
		t.Errorf("a sentence without a note: %v", m)
	}
	if m := e.post(path, map[string]any{"kind": "noise", "reason": "x", "note_id": unrelated, "same_as_usual": "Same."}); m["_status"] != float64(409) {
		t.Errorf("a note that does not apply: %v", m)
	}
	if m := e.post(path, map[string]any{"kind": "noise", "reason": "x", "note_id": 9999, "same_as_usual": "Same."}); m["_status"] != float64(404) {
		t.Errorf("a note that does not exist: %v", m)
	}
	if e.stageOf(it) != stageNew {
		t.Fatal("a refused skip moved the item")
	}
	text, isErr := e.tool("resolve_item", map[string]any{"id": ref(it), "kind": "noise", "reason": "The usual monthly statement; nothing is asked of the owner.",
		"note_id": note, "same_as_usual": "Same sender, same subject and the same amount as every month."})
	if isErr {
		t.Fatalf("resolve_item with a note: %s", text)
	}
	ev := e.events(it.ID, "skipped")
	if len(ev) != 1 || !strings.Contains(ev[0].Data, `"same_as_usual":"Same sender, same subject`) || !strings.Contains(ev[0].Data, `"note_title":"Standing order"`) {
		t.Fatalf("event: %+v", ev)
	}
	page := get(e.h, fmt.Sprintf("/items/%d", it.ID)).Body.String()
	if !strings.Contains(page, "Skipped by your note Standing order.") {
		t.Error("the item does not say it was skipped by the note")
	}
	if !strings.Contains(get(e.h, "/closed").Body.String(), "by your note Standing order") {
		t.Error("the Closed page does not name the note")
	}
	// A skip with no note says nothing in the thread.
	plain := e.newItem(stageNew, true, "Plain skip", "")
	e.post("/api/items/"+ref(plain)+"/skip", map[string]any{"kind": "noise", "reason": "Ads."})
	if strings.Contains(get(e.h, fmt.Sprintf("/items/%d", plain.ID)).Body.String(), "Skipped by your note") {
		t.Error("a plain skip names a note")
	}
	// With kind old a note id needs no sentence.
	old := e.newItem(stageNew, true, "Old reminder", "")
	if m := e.post("/api/items/"+ref(old)+"/skip", map[string]any{"kind": "old", "reason": "The date was 2 May.", "note_id": note}); m["ok"] != true {
		t.Errorf("old by a note: %v", m)
	}
}

func TestMCPInstructionsAndToolsFollowTheRulesOfSpec11(t *testing.T) {
	for _, want := range []string{"never what kind of mail it is", "notes that apply first", "never noise", "kind old", "same_as_usual", "never hides a problem or a change", "save it as a note"} {
		if !strings.Contains(mcpInstructions, want) {
			t.Errorf("the instructions lack %q", want)
		}
	}
	for _, tl := range mcpTools {
		if tl.Name == "resolve_item" && !strings.Contains(tl.Description, "never noise") {
			t.Error("resolve_item does not say what is never noise")
		}
	}
}

func TestTopLineShowsHowManyToDoAndHidesItWithNone(t *testing.T) {
	e := newCalEnv(t)
	page := get(e.h, "/").Body.String()
	if !strings.Contains(page, `id="todo-link"`) || !strings.Contains(page, `id="todo-link" href="#ag-todo-h" hidden>`) {
		t.Errorf("with no to-do the link must be there but hidden")
	}
	if strings.Contains(page, " to do</a>") {
		t.Error("the top line says to do with none")
	}
	if s := e.getJSON2("/list/state"); !strings.Contains(s, `"todo":""`) || !strings.Contains(s, `"todos":0`) {
		t.Errorf("state with none: %s", s)
	}
	e.ajaxPost("/agenda/todos", url.Values{"text": {"Call the bank"}})
	e.ajaxPost("/agenda/todos", url.Values{"text": {"Buy stamps"}})
	page = get(e.h, "/").Body.String()
	if !strings.Contains(page, `id="todo-link" href="#ag-todo-h">2 to do</a>`) {
		t.Errorf("the top line with two to-dos:\n%s", textOf(page)[:min(len(page), 1500)])
	}
	if s := e.getJSON2("/list/state"); !strings.Contains(s, `"todo":"2 to do"`) {
		t.Errorf("state with two: %s", s)
	}
	id := e.todos()[0].ID
	e.ajaxPost(fmt.Sprintf("/agenda/%d/done", id), nil)
	if !strings.Contains(get(e.h, "/").Body.String(), ">1 to do</a>") {
		t.Error("a ticked-off to-do is still counted")
	}
	if !strings.Contains(string(mustRead(t, "web/app.js")), "ag-todo-h") {
		t.Error("the script does not open the agenda at the to-dos")
	}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
