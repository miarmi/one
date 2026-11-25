package main

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"testing"
	"time"

	"lab/apps/one/repo"
)

// Tests of search and notes (SPEC 26). All data is invented.

func (e *agentEnv) searchPage(q string, extra ...string) string {
	e.t.Helper()
	v := url.Values{"q": {q}}
	for i := 0; i+1 < len(extra); i += 2 {
		v.Set(extra[i], extra[i+1])
	}
	rec := get(e.h, "/search?"+v.Encode())
	if rec.Code != 200 {
		e.t.Fatalf("search: %d", rec.Code)
	}
	return rec.Body.String()
}

func (e *agentEnv) searchAPI(q string) []map[string]any {
	e.t.Helper()
	m := e.getJSON("/api/search?q=" + url.QueryEscape(q))
	if m["ok"] != true {
		e.t.Fatalf("api search: %v", m)
	}
	var out []map[string]any
	for _, r := range m["results"].([]any) {
		out = append(out, r.(map[string]any))
	}
	return out
}

func TestSearchFindsAWordInAMessageOlderThanTheThreadWindow(t *testing.T) {
	e := newAgentEnv(t)
	old := e.mail("Quote for the roof", 200)
	old.text = "The tinsmith will use zincoplate sheets for the flashing."
	recent := e.mail("Quote for the roof", 1)
	recent.text = "Please confirm the date."
	it := e.seed(stageInProgress, old, recent)
	// The thread shows only the last month, and says one older message exists.
	if row := e.openRow(it.ID); strings.Contains(row, "zincoplate") || !strings.Contains(row, "1 older message") {
		t.Fatal("the old message is inside the thread window")
	}
	page := e.searchPage("zincoplate")
	if !strings.Contains(page, "Quote for the roof") || !strings.Contains(page, fmt.Sprintf(`href="/items/%d"`, it.ID)) || !strings.Contains(page, "<mark>zincoplate</mark>") {
		t.Errorf("not found:\n%s", page)
	}
	if !strings.Contains(page, ">Mail<") {
		t.Error("a result does not say what it is")
	}
	// A start of a word finds it too, and so does a word without its accents.
	if !strings.Contains(e.searchPage("zincop"), "zincoplate") {
		t.Error("the start of a word does not find it")
	}
	// The result opens the row, with its older messages one press away.
	if rec := get(e.h, fmt.Sprintf("/items/%d", it.ID)); rec.Code != 200 || !strings.Contains(rec.Body.String(), "Quote for the roof") {
		t.Errorf("the item page: %d", rec.Code)
	}
}

func TestSearchFindsAWordInANoteAndInAThreadEntry(t *testing.T) {
	e := newAgentEnv(t)
	it := e.newItem(stageInProgress, false, "Plumber visit", "")
	// The owner's note on the item and an agent's comment are both found as comments.
	if rec := e.ajaxPost(fmt.Sprintf("/items/%d/notes", it.ID), url.Values{"text": {"Ask about the gaskets on Tuesday."}}); rec.Code != 200 {
		t.Fatal(rec.Code)
	}
	e.post("/api/items/"+ref(it)+"/comments", map[string]any{"text": "The washer is the usual culprit."})
	for word, what := range map[string]string{"gaskets": "Comment", "washer": "Comment"} {
		page := e.searchPage(word)
		if !strings.Contains(page, ">"+what+"<") || !strings.Contains(page, "<mark>"+word+"</mark>") || !strings.Contains(page, "the owner") && word == "x" {
			t.Errorf("%s not found as %s:\n%s", word, what, page)
		}
	}
	// A saved note is found, and opens the note.
	rec := post(e.h, "/notes", url.Values{"title": {"Tea at the cottage"}, "body": {"The owner drinks rooibos, never black tea."}})
	if rec.Code != 303 {
		t.Fatalf("note: %d %s", rec.Code, rec.Body.String())
	}
	page := e.searchPage("rooibos")
	if !strings.Contains(page, ">Note<") || !strings.Contains(page, "Tea at the cottage") || !strings.Contains(page, `href="/notes/1"`) || !strings.Contains(page, "<mark>rooibos</mark>") {
		t.Errorf("the note is not found:\n%s", page)
	}
	// Changing a note changes what search finds.
	post(e.h, "/notes/1", url.Values{"title": {"Tea at the cottage"}, "body": {"The owner drinks mate."}})
	if strings.Contains(e.searchPage("rooibos"), "Tea at the cottage") || !strings.Contains(e.searchPage("mate"), "Tea at the cottage") {
		t.Error("the index does not follow a changed note")
	}
}

func TestSearchFindsAWordInAClosedItemAndAHistoryItem(t *testing.T) {
	e := newAgentEnv(t)
	m := e.mail("Newsletter of a hardware shop", 2)
	m.text = "Discount on the chainsaw lubricants."
	closed := e.seed(stageClosed, m)
	h := e.mail("Old invoice", 400)
	h.text = "Invoice for the scaffolding rental."
	hist := e.seed(stageHistory, h)
	if page := e.searchPage("lubricants"); !strings.Contains(page, fmt.Sprintf(`href="/items/%d"`, closed.ID)) || !strings.Contains(page, "Closed") {
		t.Errorf("a closed item is not found:\n%s", page)
	}
	if page := e.searchPage("scaffolding"); !strings.Contains(page, fmt.Sprintf(`href="/items/%d"`, hist.ID)) {
		t.Errorf("a history item is not found:\n%s", page)
	}
	// An item Claude has not looked at is not on the owner's page, and is for an agent.
	n := e.newItem(stageNew, true, "Secret parcel", "A parcel with a pelargonium inside.")
	if strings.Contains(e.searchPage("pelargonium"), "Secret parcel") {
		t.Error("The owner's search shows an item Claude has not looked at")
	}
	found := e.searchAPI("pelargonium")
	if len(found) != 1 || found[0]["item"] != ref(n) {
		t.Errorf("the agent's search: %v", found)
	}
}

func TestSearchRendersHitsEscaped(t *testing.T) {
	e := newAgentEnv(t)
	m := e.mail("<img src=x onerror=alert(1)> offer", 1)
	m.text = "Click <script>alert('x')</script> for the marmalade offer &amp; more."
	m.name = "<b>Mallory</b>"
	e.seed(stageNew, m)
	page := e.searchPage("marmalade")
	if !strings.Contains(page, "<mark>marmalade</mark>") {
		t.Fatalf("not found:\n%s", page)
	}
	for _, bad := range []string{"<script>alert", "<img src=x", "<b>Mallory</b>", "onerror=alert(1)>"} {
		if strings.Contains(page, bad) {
			t.Errorf("the page holds %q unescaped", bad)
		}
	}
	if !strings.Contains(page, "&lt;script&gt;") {
		t.Error("the text is not shown escaped")
	}
	// The marks the index uses are never shown, even when the mail holds them.
	m2 := e.mail("Odd", 1)
	m2.text = "A mail with \x01control\x02 characters and a quince inside."
	e.seed(stageNew, m2)
	if page := e.searchPage("quince"); strings.ContainsAny(page, "\x01\x02") || !strings.Contains(page, "<mark>quince</mark>") {
		t.Error("control characters reach the page")
	}
	if strings.Contains(e.searchPage("quince"), "Content-Security") {
		t.Error("a page carries no policy in its body")
	}
}

func TestSearchNeedsTheTokenAndOnlyReads(t *testing.T) {
	e := newAgentEnv(t)
	it := e.newItem(stageNew, false, "Parcel", "A parcel with a hyacinth inside.")
	for _, tok := range []string{"", "wrong"} {
		rec := e.call("GET", "/api/search?q=hyacinth", tok, nil)
		if rec.Code != 401 || strings.TrimSpace(rec.Body.String()) != wrongTokenMsg {
			t.Errorf("token %q: %d %s", tok, rec.Code, rec.Body.String())
		}
	}
	events := e.countEvents()
	got := e.searchAPI("hyacinth")
	if len(got) != 1 || got[0]["kind"] != "mail" || got[0]["item"] != ref(it) || !strings.Contains(got[0]["snippet"].(string), "hyacinth") {
		t.Errorf("results %v", got)
	}
	if strings.ContainsAny(got[0]["snippet"].(string), "\x01\x02") {
		t.Error("marks in the snippet")
	}
	if e.countEvents() != events {
		t.Error("a search wrote something")
	}
	// A search cannot be made to write: other methods and a missing word are refused.
	if rec := e.api("POST", "/api/search?q=hyacinth", map[string]any{}); rec.Code == 200 {
		t.Errorf("POST search: %d", rec.Code)
	}
	if rec := e.api("GET", "/api/search?q=", nil); rec.Code != 400 {
		t.Errorf("an empty search: %d", rec.Code)
	}
	// Through the connector.
	call := func(tok string) map[string]any {
		return e.rpc(tok, "tools/call", map[string]any{"name": "search_items", "arguments": map[string]any{"query": "hyacinth"}})["result"].(map[string]any)
	}
	res := call("")
	if res["isError"] != true || res["content"].([]any)[0].(map[string]any)["text"] != wrongTokenMsg {
		t.Errorf("search_items without a token: %v", res)
	}
	res = call(e.a.agentTokenValue())
	text := res["content"].([]any)[0].(map[string]any)["text"].(string)
	var m map[string]any
	if res["isError"] == true || json.Unmarshal([]byte(text), &m) != nil || len(m["results"].([]any)) != 1 {
		t.Errorf("search_items: %v", res)
	}
}

func TestSearchPersonFilterAndWording(t *testing.T) {
	e := newAgentEnv(t)
	a := e.mail("Fence", 1)
	a.text = "The fence needs paint, says Alice."
	b := e.mail("Fence again", 1)
	b.text = "The fence needs a new gate, says Bob."
	b.sender, b.name = "bob@example.test", "Bob Example"
	e.seed(stageNew, a)
	e.seed(stageNew, b)
	e.a.store.Update(e.ctx, func(tx *repo.Tx) error {
		tx.EnsurePerson(e.ctx, "mail", "alice@example.test", "Alice Example", 1)
		_, err := tx.EnsurePerson(e.ctx, "mail", "bob@example.test", "Bob Example", 1)
		return err
	})
	page := e.searchPage("fence")
	if !strings.Contains(page, "paint") || !strings.Contains(page, "gate") {
		t.Fatal("both are expected without a filter")
	}
	page = e.searchPage("fence", "person", "alice")
	if !strings.Contains(page, "paint") || strings.Contains(page, "gate") {
		t.Errorf("the person filter:\n%s", page)
	}
	if page := e.searchPage("fence", "person", "nobody at all"); !strings.Contains(page, "No results.") || !strings.Contains(page, "Nobody on People matches that person.") {
		t.Error("a filter that matches nobody")
	}
	if page := e.searchPage("zzzyyyxxx"); !strings.Contains(page, "No results.") {
		t.Error("no results wording")
	}
	empty := get(e.h, "/search").Body.String()
	if strings.Contains(empty, "No results.") || !strings.Contains(empty, `placeholder="Search mail, chats, events and notes"`) {
		t.Error("an empty search page")
	}
	// The top bar of the list has the box with the exact placeholder, and the keys focus it.
	if index := get(e.h, "/").Body.String(); !strings.Contains(index, `placeholder="Search mail, chats, events and notes"`) || !strings.Contains(index, `action="/search"`) {
		t.Error("no search box on the list page")
	}
}

func TestSearchIsFastOnTensOfThousandsOfMessages(t *testing.T) {
	if testing.Short() {
		t.Skip("fills a database")
	}
	e := newAgentEnv(t)
	words := []string{"alpha", "bravo", "charlie", "delta", "echo", "foxtrot", "golf", "hotel", "india", "juliet"}
	const items, per = 4000, 6
	err := e.a.store.Update(e.ctx, func(tx *repo.Tx) error {
		for i := 0; i < items; i++ {
			it, err := tx.CreateItem(e.ctx, repo.Item{Kind: "thread", Subject: fmt.Sprintf("Matter %d", i), Stage: stageHistory, FirstAt: 1000 + int64(i), LastAt: 1000 + int64(i), MovedAt: 1000 + int64(i)},
				"bulk", fmt.Sprint(i), "system", 1000)
			if err != nil {
				return err
			}
			for j := 0; j < per; j++ {
				sid := fmt.Sprintf("b%d-%d", i, j)
				raw, _, err := tx.InsertRaw(e.ctx, repo.Raw{Source: srcGmail, SourceID: sid, VersionKey: "a@0", ThreadKey: "x", ReceivedAt: 1000, IngestedAt: 1000, BlobSHA: "ab", Payload: "{}"})
				if err != nil {
					return err
				}
				text := fmt.Sprintf("%s %s %s message number %d of matter %d", words[(i+j)%10], words[(i*3+j)%10], words[(i*7+j)%10], j, i)
				if i == 3777 && j == 2 {
					text += " with the rare word xylophonist"
				}
				if _, _, err := tx.InsertMessage(e.ctx, repo.Message{ItemID: it.ID, RawID: raw, Source: srcGmail, SourceID: sid, ThreadKey: "x", Network: "mail",
					Sender: "bulk@example.test", Subject: fmt.Sprintf("Matter %d", i), Text: text, ReceivedAt: 1000 + int64(i*10+j)}); err != nil {
					return err
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if n := e.a.store.CountRows(e.ctx, "messages"); n != items*per {
		t.Fatalf("%d messages", n)
	}
	start := time.Now()
	rec := e.searchPage("xylophonist")
	rare := time.Since(start)
	if !strings.Contains(rec, "Matter 3777") {
		t.Error("the rare word is not found")
	}
	start = time.Now()
	common := e.searchPage("alpha charlie")
	if !strings.Contains(common, "<mark>") {
		t.Errorf("a common search found nothing: %.600s", common[strings.Index(common, "<main"):])
	}
	slow := time.Since(start)
	t.Logf("%d messages: rare word %s, two common words %s", items*per, rare, slow)
	if rare > 2*time.Second || slow > 2*time.Second {
		t.Errorf("search is slow: %s and %s", rare, slow)
	}
}

func TestNotesPageWordingAndSaving(t *testing.T) {
	e := newAgentEnv(t)
	page := get(e.h, "/notes").Body.String()
	for _, want := range []string{"<title>Notes</title>", `id="page-title">Notes</h2>`, "Add note", "No saved notes yet.", "Back to the queue"} {
		if !strings.Contains(page, want) {
			t.Errorf("the page lacks %q", want)
		}
	}
	form := get(e.h, "/notes/new").Body.String()
	for _, want := range []string{">Title<", ">Note<", ">Save</button>"} {
		if !strings.Contains(form, want) {
			t.Errorf("the form lacks %q", want)
		}
	}
	if rec := post(e.h, "/notes", url.Values{"title": {""}, "body": {"x"}}); rec.Code != 400 {
		t.Errorf("no title: %d", rec.Code)
	}
	it := e.newItem(stageNew, false, "Plumber", "")
	e.a.store.Update(e.ctx, func(tx *repo.Tx) error {
		_, err := tx.EnsurePerson(e.ctx, "mail", "alice@example.test", "Alice Example", 1)
		return err
	})
	rec := post(e.h, "/notes", url.Values{"title": {"How Alice likes things"}, "body": {"Short mails, no phone calls."}, "item": {ref(it)}, "person": {"Alice Example"}})
	if rec.Code != 303 {
		t.Fatalf("save: %d %s", rec.Code, rec.Body.String())
	}
	page = get(e.h, "/notes").Body.String()
	if strings.Contains(page, "No saved notes yet.") || !strings.Contains(page, "How Alice likes things") || !strings.Contains(page, "About Alice Example") || !strings.Contains(page, "About <a href=") {
		t.Errorf("the saved note is not listed:\n%s", page)
	}
	for _, bad := range []url.Values{
		{"title": {"x"}, "body": {"y"}, "item": {"Q-9999"}},
		{"title": {"x"}, "body": {"y"}, "item": {"nonsense"}},
		{"title": {"x"}, "body": {"y"}, "person": {"Nobody Known"}},
	} {
		if rec := post(e.h, "/notes", bad); rec.Code != 400 {
			t.Errorf("%v: %d", bad, rec.Code)
		}
	}
	// Every save is kept as an event, so an earlier text can be read.
	post(e.h, "/notes/1", url.Values{"title": {"How Alice likes things"}, "body": {"Long mails are fine."}})
	evs := e.eventsOfType("note_saved")
	if len(evs) != 2 || !strings.Contains(evs[1], "Short mails") || !strings.Contains(evs[0], "Long mails") {
		t.Errorf("events %v", evs)
	}
}

func TestAgentsSaveNotesButChangeOnlyTheirOwn(t *testing.T) {
	e := newAgentEnv(t)
	m := e.post("/api/notes", map[string]any{"title": "Parcels", "text": "Parcels go to the neighbour.", "agent": "Claude"})
	if m["ok"] != true || m["created"] != true {
		t.Fatalf("save: %v", m)
	}
	id := int64(m["note_id"].(float64))
	if m := e.post("/api/notes", map[string]any{"title": "Parcels", "text": "Parcels go to the neighbour at number 4.", "note_id": id}); m["ok"] != true || m["created"] != false {
		t.Errorf("an agent changes its own note: %v", m)
	}
	if got := e.searchAPI("neighbour"); len(got) != 1 || got[0]["kind"] != "note" || got[0]["note_id"] != float64(id) {
		t.Errorf("results %v", got)
	}
	post(e.h, "/notes", url.Values{"title": {"The owner's rule"}, "body": {"Never close mail from the bank."}})
	if m := e.post("/api/notes", map[string]any{"title": "The owner's rule", "text": "Close mail from the bank.", "note_id": 2}); m["_status"] != float64(403) {
		t.Errorf("an agent changed the owner's note: %v", m)
	}
	if n, _ := e.a.store.NoteByID(e.ctx, 2); n.Body != "Never close mail from the bank." {
		t.Error("The owner's note was changed")
	}
	// Through the connector, and an agent's name never makes it the owner's.
	res := e.rpc(e.a.agentTokenValue(), "tools/call", map[string]any{"name": "save_note", "arguments": map[string]any{"title": "Tools", "text": "The drill is in the shed.", "agent": "the owner"}})["result"].(map[string]any)
	if res["isError"] == true {
		t.Fatalf("save_note: %v", res)
	}
	if n, ok := e.a.store.NoteByID(e.ctx, 3); !ok || n.CreatedBy != "agent:the owner" {
		t.Errorf("created by %q", n.CreatedBy)
	}
}
