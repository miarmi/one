package main

import (
	"bytes"
	"fmt"
	"mime/multipart"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"lab/apps/one/repo"
)

// Tests of People (SPEC 25). All names and addresses are invented.

func (e *agentEnv) person(name string) repo.Person {
	e.t.Helper()
	list, err := e.a.store.ListPeople(e.ctx, name, 5, 0)
	if err != nil || len(list) == 0 {
		e.t.Fatalf("no person %q: %v", name, err)
	}
	return list[0]
}

// makePerson makes a person the way the importers do: on first sight, with
// one handle.
func (e *agentEnv) makePerson(network, value, name string) repo.Person {
	e.t.Helper()
	err := e.a.store.Update(e.ctx, func(tx *repo.Tx) error {
		_, err := tx.EnsurePerson(e.ctx, network, value, name, 1)
		return err
	})
	if err != nil {
		e.t.Fatal(err)
	}
	return e.person(name)
}

func (e *agentEnv) personByID(id int64) (repo.Person, bool) { return e.a.store.PersonByID(e.ctx, id) }

func TestPeoplePageWordingAndEmptyState(t *testing.T) {
	e := newAgentEnv(t)
	page := get(e.h, "/people").Body.String()
	for _, want := range []string{"<title>People</title>", `id="page-title">People</h2>`, "No people yet.", "Back to the queue"} {
		if !strings.Contains(page, want) {
			t.Errorf("the page lacks %q", want)
		}
	}
	// Nobody adds people by hand any more.
	for _, off := range []string{"Add person", "New person", "Proposal", "Approve"} {
		if strings.Contains(page, off) {
			t.Errorf("the page shows %q", off)
		}
	}
	if rec := post(e.h, "/people", url.Values{"name": {"Bob Example"}}); rec.Code == 303 || rec.Code == 200 || e.a.store.CountPeople(e.ctx, "") != 0 {
		t.Errorf("a person was added by hand: %d", rec.Code)
	}
}

func TestPhoneTopBarHasNoPeopleLinkAndSettingsHasOne(t *testing.T) {
	e := newAgentEnv(t)
	e.makePerson("mail", "alice@example.test", "Alice Example")
	for _, path := range []string{"/", "/notes", "/connectors", "/settings", "/closed", "/search?q=alice"} {
		page := get(e.h, path).Body.String()
		// People is a page of the laptop's ribbon only (SPEC 1): the link is there for a laptop and a phone does not show it
		if strings.Contains(page, `class="top-nav" aria-label="Pages"><a href="/people"`) {
			t.Errorf("%s: the top bar links to People first", path)
		}
		if !strings.Contains(page, `<a href="/people" class="nav-people"`) {
			t.Errorf("%s: the ribbon has no link to People for a laptop", path)
		}
	}
	top := get(e.h, "/").Body.String()
	nav := top[strings.Index(top, `class="top-nav"`):]
	nav = nav[:strings.Index(nav, "</nav>")]
	if !strings.Contains(nav, "<span>Settings</span>") || !strings.Contains(nav, `class="nav-people"`) {
		t.Errorf("the top bar: %s", nav)
	}
	settings := get(e.h, "/settings").Body.String()
	if !strings.Contains(settings, `<a href="/people">People</a>`) {
		t.Error("Settings has no link People")
	}
}

func TestPeopleListShowsNamesOnlyAndAPersonOpensWithTheForm(t *testing.T) {
	e := newAgentEnv(t)
	p := e.makePerson("mail", "lenka@example.test", "Lenka Example")
	post(e.h, fmt.Sprintf("/people/%d", p.ID), url.Values{"name": {"Lenka Example"}, "call_name": {"Lenka"}, "relation": {"my partner"},
		"handles": {"lenka@example.test\nwhatsapp: +420600000001"}, "protected": {"1"}})
	e.makePerson("mail", "bob@example.test", "Bob Example")
	list := get(e.h, "/people").Body.String()
	if !strings.Contains(list, "Lenka") || !strings.Contains(list, "Bob Example") || !strings.Contains(list, "Always show me") {
		t.Errorf("the list lacks the names or the mark:\n%s", list)
	}
	if strings.Count(list, "Always show me") != 1 {
		t.Error("the mark is shown for someone who has none")
	}
	if !strings.Contains(list, fmt.Sprintf(`href="/people/%d"`, p.ID)) || !strings.Contains(list, `class="avatar badge`) {
		t.Error("a name does not open the person, or has no initials")
	}
	// Names only: nothing else that is kept, and no form.
	for _, off := range []string{"my partner", "lenka@example.test", "420600000001", "Lenka Example", "<form method=\"post\" action=\"/people/", "Who they are to me", "Addresses and handles", ">Save<", "Set picture", "<textarea", "<input type=\"text\""} {
		if strings.Contains(list, off) {
			t.Errorf("the list shows %q", off)
		}
	}
	open := get(e.h, fmt.Sprintf("/people/%d", p.ID))
	if open.Code != 200 {
		t.Fatalf("open: %d", open.Code)
	}
	page := open.Body.String()
	for _, want := range []string{`id="page-title">People</h2>`, ">Name<", ">I call them<", ">Who they are to me<", ">Addresses and handles<", "Always show me", ">Save<", ">Set picture<", "my partner", "lenka@example.test", "&#43;420600000001", "checked"} {
		if !strings.Contains(page, want) {
			t.Errorf("the opened person lacks %q", want)
		}
	}
	if strings.Contains(page, "Bob Example") || strings.Contains(page, "Remove picture") {
		t.Error("the opened person shows someone else, or Remove picture with no picture")
	}
	if get(e.h, "/people/9999").Code != 404 {
		t.Error("a missing person opens")
	}
}

func TestPeopleListStaysShortWithSeveralHundredPeople(t *testing.T) {
	e := newAgentEnv(t)
	e.a.store.Update(e.ctx, func(tx *repo.Tx) error {
		for i := 0; i < 400; i++ {
			if _, err := tx.EnsurePerson(e.ctx, "mail", fmt.Sprintf("person%03d@example.test", i), fmt.Sprintf("Person %03d", i), 1); err != nil {
				return err
			}
		}
		return nil
	})
	page := get(e.h, "/people").Body.String()
	if n := strings.Count(page, `class="person-link"`); n != peoplePage {
		t.Errorf("%d people on the first page", n)
	}
	if !strings.Contains(page, "1–50 of 400") || !strings.Contains(page, "Show more") || strings.Contains(page, "Previous") {
		t.Error("the paging of the first page")
	}
	last := get(e.h, fmt.Sprintf("/people?offset=%d", 350)).Body.String()
	if n := strings.Count(last, `class="person-link"`); n != 50 || strings.Contains(last, "Show more") || !strings.Contains(last, "Previous") {
		t.Errorf("the last page: %d", n)
	}
	found := get(e.h, "/people?q=Person+123").Body.String()
	if n := strings.Count(found, `class="person-link"`); n != 1 || !strings.Contains(found, "Person 123") {
		t.Errorf("the filter found %d", n)
	}
	if none := get(e.h, "/people?q=zzzz").Body.String(); !strings.Contains(none, "Nobody matches that.") {
		t.Error("the filter with no match")
	}
}

func TestPeopleListsHandlesOnce(t *testing.T) {
	e := newAgentEnv(t)
	// The importers see the same address in two spellings: still one person, one handle.
	e.a.store.Update(e.ctx, func(tx *repo.Tx) error {
		for _, v := range []string{"lenka@example.test", "Lenka@Example.test", "lenka@example.test"} {
			if _, err := tx.EnsurePerson(e.ctx, "mail", v, "Lenka Example", 1); err != nil {
				return err
			}
		}
		return nil
	})
	if n := e.a.store.CountPeople(e.ctx, ""); n != 1 {
		t.Fatalf("%d people for one address", n)
	}
	p := e.person("Lenka")
	if len(p.Handles) != 1 {
		t.Fatalf("handles %+v", p.Handles)
	}
	// The owner writes the same handle twice, in two spellings, plus a WhatsApp number: kept once each.
	rec := post(e.h, fmt.Sprintf("/people/%d", p.ID), url.Values{"name": {"Lenka Example"}, "call_name": {"Lenka"}, "relation": {"my partner"},
		"handles": {"lenka@example.test\nLENKA@example.test, whatsapp: +420 600 000 001\nwhatsapp: +420600000001"}})
	if rec.Code != 303 || rec.Header().Get("Location") != fmt.Sprintf("/people/%d?n=saved", p.ID) {
		t.Fatalf("save: %d %s", rec.Code, rec.Body.String())
	}
	p = e.person("Lenka")
	if len(p.Handles) != 2 || p.Handles[0] != (repo.Handle{Network: "mail", Value: "lenka@example.test"}) || p.Handles[1] != (repo.Handle{Network: "whatsapp", Value: "+420600000001"}) {
		t.Fatalf("handles %+v", p.Handles)
	}
	page := get(e.h, fmt.Sprintf("/people/%d", p.ID)).Body.String()
	if strings.Count(page, "lenka@example.test") != 1 || strings.Count(page, "&#43;420600000001") != 1 {
		t.Errorf("a handle is listed more than once:\n%s", page)
	}
	if !strings.Contains(page, "my partner") || !strings.Contains(page, `value="Lenka"`) {
		t.Error("what the owner changed is not shown")
	}
	// A save is recorded without the handles' text.
	evs := e.eventsOfType("setting_changed")
	if len(evs) == 0 || strings.Contains(strings.Join(evs, " "), "lenka@") {
		t.Errorf("events %v", evs)
	}
}

func TestPeopleFormsAreValidatedAndHandlesAreNotTakenOver(t *testing.T) {
	e := newAgentEnv(t)
	bob := e.makePerson("mail", "bob@example.test", "Bob Example")
	rob := e.makePerson("mail", "rob@example.test", "Rob Example")
	save := func(id int64, v url.Values) *httptest.ResponseRecorder {
		return post(e.h, fmt.Sprintf("/people/%d", id), v)
	}
	if rec := save(bob.ID, url.Values{"name": {"  "}}); rec.Code != 400 || !strings.Contains(rec.Body.String(), "Write a name first.") {
		t.Errorf("no name: %d", rec.Code)
	}
	if rec := save(bob.ID, url.Values{"name": {"Bob Example"}, "handles": {"bob at example"}}); rec.Code != 400 || !strings.Contains(rec.Body.String(), "network: value") {
		t.Errorf("a handle without a network: %d", rec.Code)
	}
	if rec := save(bob.ID, url.Values{"name": {"Bob Example"}, "handles": {"mail: not an address"}}); rec.Code != 400 {
		t.Errorf("a bad address: %d", rec.Code)
	}
	if rec := save(bob.ID, url.Values{"name": {"Bob Example"}, "handles": {"fax: 123"}}); rec.Code != 400 {
		t.Errorf("an unknown network: %d", rec.Code)
	}
	// A refused form shows what the owner typed, on the person's page.
	rec := save(bob.ID, url.Values{"name": {"Bob Example"}, "call_name": {"Bobík"}, "relation": {"neighbour"}, "handles": {"fax: 123"}})
	if !strings.Contains(rec.Body.String(), `value="Bobík"`) || !strings.Contains(rec.Body.String(), "neighbour") {
		t.Error("a refused save lost what the owner typed")
	}
	if rec := save(bob.ID, url.Values{"name": {"Bob Example"}, "call_name": {"Bobík"}, "relation": {"neighbour"}, "handles": {"bob@example.test"}}); rec.Code != 303 {
		t.Fatalf("save: %d %s", rec.Code, rec.Body.String())
	}
	// Another person cannot take Bob's address.
	rec = save(rob.ID, url.Values{"name": {"Rob Example"}, "handles": {"rob@example.test\nBOB@example.test"}})
	if rec.Code != 409 || !strings.Contains(rec.Body.String(), "already belongs to Bobík") {
		t.Errorf("a taken handle: %d %s", rec.Code, rec.Body.String())
	}
	if len(e.person("Rob").Handles) != 1 {
		t.Error("a refused save changed the handles")
	}
	if rec := post(e.h, "/people/9999", url.Values{"name": {"x"}}); rec.Code != 404 {
		t.Errorf("a missing person: %d", rec.Code)
	}
}

func TestPeopleNamesRenderEscaped(t *testing.T) {
	e := newAgentEnv(t)
	evil := "<script>alert(1)</script>"
	p := e.makePerson("mail", "mallory@example.test", evil)
	q := e.makePerson("mail", "oscar@example.test", "Oscar Example")
	post(e.h, fmt.Sprintf("/people/%d", p.ID), url.Values{"name": {evil}, "call_name": {"<b onclick=x>Mal</b>"}, "relation": {"<img src=x onerror=y>"}, "handles": {"mallory@example.test"}})
	e.post("/api/people/proposals", map[string]any{"action": "join", "person": p.ID, "with": q.ID, "reason": "<script>alert(2)</script>"})
	for _, path := range []string{"/people", fmt.Sprintf("/people/%d", p.ID), "/settings"} {
		page := get(e.h, path).Body.String()
		for _, raw := range []string{"<script>alert", "<b onclick", "<img src=x"} {
			if strings.Contains(page, raw) {
				t.Errorf("%s inserts %q as markup", path, raw)
			}
		}
	}
	if page := get(e.h, "/people").Body.String(); !strings.Contains(page, "&lt;b onclick=x&gt;Mal&lt;/b&gt;") || !strings.Contains(page, "Join &lt;b onclick=x&gt;Mal&lt;/b&gt; and Oscar Example") {
		t.Error("the escaped names are not shown")
	}
}

func TestProtectedMarkBlocksWhatTheGuardSays(t *testing.T) {
	e := newAgentEnv(t)
	// A direct mail from an address the importer has seen.
	it := e.newItem(stageNew, true, "Dinner", "")
	e.a.store.Update(e.ctx, func(tx *repo.Tx) error {
		_, err := tx.EnsurePerson(e.ctx, "mail", "alice@example.test", "Alice Example", 1)
		return err
	})
	e.a.store.Update(e.ctx, func(tx *repo.Tx) error { // the seeded message is addressed to the owner
		_, err := tx.Exec(`UPDATE messages SET addressed = 1`)
		return err
	})
	p := e.person("Alice")
	// Not protected yet: an agent may close it.
	body := map[string]any{"kind": "noise", "reason": "Looks like noise."}
	other := e.newItem(stageNew, true, "Other", "")
	if m := e.post("/api/items/"+ref(other)+"/skip", body); m["ok"] != true {
		t.Fatalf("skip: %v", m)
	}
	// The owner marks her Always show me on the page.
	if rec := post(e.h, fmt.Sprintf("/people/%d", p.ID), url.Values{"name": {"Alice Example"}, "handles": {"alice@example.test"}, "protected": {"1"}}); rec.Code != 303 {
		t.Fatalf("save: %d", rec.Code)
	}
	if !e.person("Alice").Protected {
		t.Fatal("the mark was not saved")
	}
	if page := get(e.h, fmt.Sprintf("/people/%d", p.ID)).Body.String(); !strings.Contains(page, "checked") || !strings.Contains(get(e.h, "/people").Body.String(), "Always show me") {
		t.Error("the page does not show the mark")
	}
	if m := e.post("/api/items/"+ref(it)+"/skip", body); !isRefusal(m["error"], "Protected people") || e.stageOf(it) != stageNew {
		t.Errorf("a protected person's mail was skipped: %v", m)
	}
	if m := e.post("/api/items/"+ref(it)+"/proposals", map[string]any{"action": "close", "reason": "Done."}); !isRefusal(m["error"], "Protected people") {
		t.Errorf("a close proposal for a protected person: %v", m)
	}
	// Commenting is still allowed; the mark is recorded as the owner's doing.
	if m := e.post("/api/items/"+ref(it)+"/comments", map[string]any{"text": "She asks about dinner."}); m["ok"] != true {
		t.Errorf("comment: %v", m)
	}
	// Taking the mark away lets the agent close again.
	post(e.h, fmt.Sprintf("/people/%d", p.ID), url.Values{"name": {"Alice Example"}, "handles": {"alice@example.test"}})
	if m := e.post("/api/items/"+ref(it)+"/skip", body); m["ok"] != true {
		t.Errorf("after taking the mark away: %v", m)
	}
	// An agent cannot reach the person's address of the page.
	if rec := e.api("POST", fmt.Sprintf("/api/people/%d", p.ID), map[string]any{"protected": true}); rec.Code != 404 || e.person("Alice").Protected {
		t.Errorf("the mark through /api/: %d", rec.Code)
	}
}

func TestMessagesShowTheNameOwnerCallsThemOneColourAndThePicture(t *testing.T) {
	e := newAgentEnv(t)
	it := e.newItem(stageNew, false, "Dinner", "")
	row := e.openRow(it.ID)
	if !strings.Contains(row, "Alice Example") {
		t.Fatal("the sender's own name is not shown")
	}
	e.a.store.Update(e.ctx, func(tx *repo.Tx) error {
		_, err := tx.EnsurePerson(e.ctx, "mail", "alice@example.test", "Alice Example", 1)
		return err
	})
	p := e.person("Alice")
	post(e.h, fmt.Sprintf("/people/%d", p.ID), url.Values{"name": {"Alice Example"}, "call_name": {"Ali from the shop"}, "handles": {"alice@example.test"}})
	row = e.openRow(it.ID)
	if !strings.Contains(row, "Ali from the shop") || strings.Contains(row, "Alice Example") {
		t.Errorf("the name the owner calls her is not shown:\n%s", row)
	}
	color := colorOf("person" + fmt.Sprint(p.ID))
	if !strings.Contains(row, `from-dot `+color) {
		t.Errorf("her colour is not the person's colour %s", color)
	}
	group := get(e.h, "/list/new").Body.String()
	if !strings.Contains(group, "Ali from the shop") {
		t.Error("the row line does not show the name")
	}
	// The owner's own picture, kept and served; refused when it is not a picture or too big.
	png := []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR\x00\x00\x00\x01\x00\x00\x00\x01\x08\x02\x00\x00\x00\x90wS\xde")
	upload := func(name string, data []byte) *httptest.ResponseRecorder {
		var buf bytes.Buffer
		mw := multipart.NewWriter(&buf)
		fw, _ := mw.CreateFormFile("picture", name)
		fw.Write(data)
		mw.Close()
		req := httptest.NewRequest("POST", fmt.Sprintf("/people/%d/picture", p.ID), &buf)
		req.Header.Set("Content-Type", mw.FormDataContentType())
		rec := httptest.NewRecorder()
		e.h.ServeHTTP(rec, req)
		return rec
	}
	for _, bad := range []struct {
		name string
		data []byte
	}{{"a.png", []byte("this is text, not a picture")}, {"a.png", []byte("<svg xmlns='http://www.w3.org/2000/svg'><script>1</script></svg>")}, {"big.png", append(append([]byte{}, png...), make([]byte, 301<<10)...)}} {
		rec := upload(bad.name, bad.data)
		if rec.Code != 400 || !strings.Contains(rec.Body.String(), "Use a PNG, JPEG or WebP picture under 300 KB.") {
			t.Errorf("%s: %d", bad.name, rec.Code)
		}
	}
	if rec := upload("ali.png", png); rec.Code != 303 {
		t.Fatalf("upload: %d %s", rec.Code, rec.Body.String())
	}
	pic := get(e.h, fmt.Sprintf("/people/%d/picture", p.ID))
	if pic.Code != 200 || pic.Header().Get("Content-Type") != "image/png" || pic.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Errorf("serving: %d %v", pic.Code, pic.Header())
	}
	if row = e.openRow(it.ID); !strings.Contains(row, fmt.Sprintf(`src="/people/%d/picture"`, p.ID)) {
		t.Error("the message does not show the picture")
	}
	if page := get(e.h, fmt.Sprintf("/people/%d", p.ID)).Body.String(); !strings.Contains(page, "Remove picture") {
		t.Error("no Remove picture once a picture is set")
	}
	post(e.h, fmt.Sprintf("/people/%d/picture/remove", p.ID), nil)
	if get(e.h, fmt.Sprintf("/people/%d/picture", p.ID)).Code != 404 {
		t.Error("the picture is still there")
	}
}

// SPEC 25: saving a person never changes a handle the owner did not edit, even a
// WhatsApp handle that is not a phone number.
func TestSpec25SavingAPersonKeepsAHandleThatIsNotAPhoneNumber(t *testing.T) {
	e := newAgentEnv(t)
	p := e.makePerson("whatsapp", "Jana Example 7", "Jana Example")
	f := url.Values{"name": {"Jana Example"}, "call_name": {"Jana"}, "handles": {handlesText(p.Handles)}}
	rec := post(e.h, fmt.Sprintf("/people/%d", p.ID), f)
	if rec.Code != 303 {
		t.Fatalf("save: %d %s", rec.Code, rec.Body.String())
	}
	got, _ := e.personByID(p.ID)
	if len(got.Handles) != 1 || got.Handles[0].Value != "Jana Example 7" || got.Handles[0].Network != "whatsapp" {
		t.Errorf("the handle was rewritten: %+v", got.Handles)
	}
	// A new line for a phone number is still tidied; the old line stays as it was.
	f.Set("handles", handlesText(p.Handles)+"\nwhatsapp: +420 600 000 111")
	if rec := post(e.h, fmt.Sprintf("/people/%d", p.ID), f); rec.Code != 303 {
		t.Fatalf("second save: %d", rec.Code)
	}
	got, _ = e.personByID(p.ID)
	var vals []string
	for _, h := range got.Handles {
		vals = append(vals, h.Value)
	}
	if len(vals) != 2 || !(vals[0] == "+420600000111" && vals[1] == "Jana Example 7" || vals[1] == "+420600000111" && vals[0] == "Jana Example 7") {
		t.Errorf("handles after adding a phone number: %v", vals)
	}
}
