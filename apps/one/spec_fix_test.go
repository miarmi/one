package main

import (
	"bytes"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"lab/apps/one/repo"
)

// Tests of the fixes that follow SPEC 2, 15 and 23. All names are invented.

// SPEC 15: the top bar has Refresh; pressing it starts one fetch of everything,
// pressing it while one runs starts nothing new, and the answer says when it
// finished.
func TestSpec15RefreshButtonFetchesOnceAtATime(t *testing.T) {
	e := newAgentEnv(t)
	page := get(e.h, "/").Body.String()
	if !strings.Contains(page, `id="refresh"`) || !strings.Contains(page, `<span class="rb-label">Refresh</span></button>`) {
		t.Fatalf("the top bar has no Refresh button")
	}
	if !strings.Contains(get(e.h, "/settings").Body.String(), `<span class="rb-label">Refresh</span></button>`) {
		t.Error("Refresh is missing from the top line of the other pages")
	}
	js := get(e.h, "/static/app.js").Body.String()
	for _, want := range []string{"Refreshing\\u2026", `"Refreshed "`} {
		if !strings.Contains(js, want) {
			t.Errorf("the script lacks %s", want)
		}
	}
	// While a round runs, a second press claims nothing.
	if !e.a.refr.begin() {
		t.Fatal("the first claim was refused")
	}
	rec := post(e.h, "/refresh", url.Values{})
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"running":true`) {
		t.Errorf("a press while a fetch runs: %d %s", rec.Code, rec.Body.String())
	}
	if e.a.fetchAll(e.ctx) {
		t.Error("the timer started a second round while one was running")
	}
	e.a.refr.end(e.now)
	// Once free, a press runs a round and the status says when it finished.
	rec = post(e.h, "/refresh", url.Values{})
	if rec.Code != 200 {
		t.Fatalf("press: %d", rec.Code)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if running, fin := e.a.refr.state(); !running && !fin.IsZero() {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	rec = get(e.h, "/refresh")
	if !strings.Contains(rec.Body.String(), `"running":false`) || !strings.Contains(rec.Body.String(), `"finished":"`) {
		t.Errorf("after the round: %s", rec.Body.String())
	}
}

// SPEC 2: c is for the note box under the thread, not the chat box.
func TestSpec2CKeyGoesToTheNoteBox(t *testing.T) {
	js := get(newAgentEnv(t).h, "/static/app.js").Body.String()
	if !strings.Contains(js, `open.querySelector(".thread .say textarea")`) {
		t.Error("the c key does not look for the note box in the thread part")
	}
}

// SPEC 23: Notify and Claude rows, the sections Recent fetches and Last 14 days,
// the known limitations once each, Set picture on a source and Remove for a
// stale one.
func TestSpec23ConnectionsPageHasItsRowsAndSections(t *testing.T) {
	e := newAgentEnv(t)
	page := get(e.h, "/connectors").Body.String()
	for _, want := range []string{">Notify</h2>", ">Claude</h2>", ">Recent fetches</h2>", ">Last 14 days</h2>", ">Known limitations</h2>",
		"Set picture", "Last 24 hours: 0 items",
		"Chats arrive only while the Mac is on and Beeper is running.",
		"A network that has logged itself out delivers nothing until the owner logs in on the Mac.",
		"Phone calls are not covered.",
		"Mail deleted before the first import cannot be imported.",
		"Only the notification mails are seen. ONE never signs in to the data box, and a notification is not the delivery itself."} {
		if strings.Count(page, want) != 1 && !(want == "Set picture" || want == "Last 24 hours: 0 items") {
			t.Errorf("%q appears %d times, want once", want, strings.Count(page, want))
		}
		if !strings.Contains(page, want) {
			t.Errorf("the page lacks %q", want)
		}
	}
	// Claude: never called, then called by an agent.
	if !strings.Contains(page, "No agent has called yet") {
		t.Error("the Claude row does not say no agent called")
	}
	rec := httptest.NewRequest("GET", "/api/queue", nil)
	rec.Header.Set("Authorization", "Bearer "+e.a.agentTokenValue())
	w := httptest.NewRecorder()
	e.h.ServeHTTP(w, rec)
	if w.Code != 200 {
		t.Fatalf("agent call: %d", w.Code)
	}
	if strings.Contains(get(e.h, "/connectors").Body.String(), "No agent has called yet") {
		t.Error("the Claude row does not show the call")
	}
	// Notify: Test now hands a notice over and the row says when.
	var got []string
	e.a.notify = func(title, body, link string) error { got = append(got, title); return nil }
	if rec := post(e.h, "/connectors/notify/test", url.Values{}); rec.Code != 303 || len(got) != 1 {
		t.Fatalf("test notice: %d, sent %v", rec.Code, got)
	}
	if !strings.Contains(get(e.h, "/connectors").Body.String(), "Last 24 hours: 1 notice sent") {
		t.Error("the Notify row does not count the notice")
	}
}

func TestSpec23ASourcePictureIsSetShownOnRowsAndRemoved(t *testing.T) {
	e := newListEnv(t)
	it := e.seed(stageNew, e.mail("pic", 1))
	if strings.Contains(e.openRow(it.ID), `class="badge badge-img"`) {
		t.Fatal("a row shows a picture nobody set")
	}
	png := "\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR\x00\x00\x00\x01\x00\x00\x00\x01\x08\x06\x00\x00\x00\x1f\x15\xc4\x89\x00\x00\x00\rIDATx\x9cc\xf8\xff\xff?\x00\x05\xfe\x02\xfe\xa7\x9a\xa0\xa0\x00\x00\x00\x00IEND\xaeB`\x82"
	rec := upload(e.h, "/sources/gmail/picture", "picture", "g.png", []byte(png))
	if rec.Code != 303 {
		t.Fatalf("set picture: %d %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(e.openRow(it.ID), `class="badge badge-img"`) || get(e.h, "/sources/gmail/picture").Code != 200 {
		t.Error("the source's picture is not shown on its rows")
	}
	bad := upload(e.h, "/sources/gmail/picture", "picture", "x.txt", []byte("not a picture"))
	if bad.Code != 400 || !strings.Contains(bad.Body.String(), "Use a PNG, JPEG or WebP picture under 300 KB.") {
		t.Errorf("a text file: %d", bad.Code)
	}
	big := upload(e.h, "/sources/gmail/picture", "picture", "big.png", append([]byte(png), make([]byte, 301<<10)...))
	if big.Code != 400 {
		t.Errorf("a picture over 300 KB: %d", big.Code)
	}
	if rec := upload(e.h, "/sources/nonsense/picture", "picture", "g.png", []byte(png)); rec.Code != 404 {
		t.Errorf("an unknown source: %d", rec.Code)
	}
	if rec := post(e.h, "/sources/gmail/picture/remove", url.Values{}); rec.Code != 303 {
		t.Fatalf("remove picture: %d", rec.Code)
	}
	if strings.Contains(e.openRow(it.ID), `class="badge badge-img"`) {
		t.Error("the picture is still on the row after Remove picture")
	}
}

func TestSpec23AStaleSourceCanBeRemovedAndComesBackWhenReadAgain(t *testing.T) {
	e := newListEnv(t)
	writeSecret(e.a.secretsDir, gmailSecretFile, gmailSecret{Address: testAddr, Password: testPass})
	old := e.now.Add(-2 * time.Hour).Unix()
	e.a.store.Update(e.ctx, func(tx *repo.Tx) error {
		_, err := tx.Exec(`INSERT INTO sync_runs (source, mode, status, started_at, finished_at) VALUES ('gmail', 'sync', 'done', ?, ?)`, old, old)
		return err
	})
	page := get(e.h, "/connectors").Body.String()
	if !strings.Contains(page, `form="f-gmail-remove">Remove</button>`) {
		t.Fatalf("a stale source has no Remove button")
	}
	if rec := post(e.h, "/connectors/gmail/remove", url.Values{}); rec.Code != 303 {
		t.Fatalf("remove: %d", rec.Code)
	}
	if strings.Contains(get(e.h, "/connectors").Body.String(), `id="h-gmail"`) {
		t.Error("the removed source is still on the page")
	}
	e.now = e.now.Add(time.Minute)
	fresh := e.now.Unix()
	e.a.store.Update(e.ctx, func(tx *repo.Tx) error {
		_, err := tx.Exec(`INSERT INTO sync_runs (source, mode, status, started_at, finished_at) VALUES ('gmail', 'sync', 'done', ?, ?)`, fresh, fresh)
		return err
	})
	if !strings.Contains(get(e.h, "/connectors").Body.String(), `id="h-gmail"`) {
		t.Error("the source did not come back when it was read again")
	}
	if rec := post(e.h, "/connectors/gmail/remove", url.Values{}); rec.Code != 409 {
		t.Errorf("a source that works was removable: %d", rec.Code)
	}
}

func TestSpec23RecentFetchesAndTheLast14DaysShowWhatWasRead(t *testing.T) {
	e := newListEnv(t)
	e.seed(stageNew, e.mail("today", 1))
	at := e.now.Unix() - 60
	e.a.store.Update(e.ctx, func(tx *repo.Tx) error {
		_, err := tx.Exec(`INSERT INTO sync_runs (source, mode, status, started_at, finished_at, fetched, stored, items_made) VALUES ('gmail', 'sync', 'done', ?, ?, 4, 3, 2)`, at, at)
		return err
	})
	page := get(e.h, "/connectors").Body.String()
	i := strings.Index(page, `id="h-fetches"`)
	if i < 0 || !strings.Contains(page[i:], "4 read, 3 new") {
		t.Errorf("Recent fetches does not list the run")
	}
	j := strings.Index(page, `id="h-days"`)
	if j < 0 || strings.Count(page[j:], "<tr>") < 14 {
		t.Errorf("Last 14 days does not list 14 days")
	}
	_ = fmt.Sprint
}

// upload posts one file as a form, the way the page does.
func upload(h http.Handler, path, field, name string, data []byte) *httptest.ResponseRecorder {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	w, _ := mw.CreateFormFile(field, name)
	w.Write(data)
	mw.Close()
	req := httptest.NewRequest("POST", path, &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// The one-tap icon on a phone ignores a second tap: busy until the row is
// swapped, and a short hold on the icon that takes its place.
func TestQuickTapGuardIsInThePage(t *testing.T) {
	e := newListEnv(t)
	js := get(e.h, "/static/app.js").Body.String()
	for _, want := range []string{`form.dataset.busy`, `holdUntil`, `QUICK_HOLD_MS = 600`} {
		if !strings.Contains(js, want) {
			t.Errorf("the page script lacks %q", want)
		}
	}
}
