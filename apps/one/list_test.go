package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"lab/apps/one/repo"
)

// Tests of the list page. All data is invented.

var seedN atomic.Int64

type seedMsg struct {
	subject, name, sender, text string
	at                          int64
	mine                        bool
	network                     string // "mail" when empty
	source                      string // srcGmail when empty
}

type listEnv struct {
	t   *testing.T
	a   *app
	h   http.Handler
	ctx context.Context
	now time.Time
}

func newListEnv(t *testing.T) *listEnv {
	t.Helper()
	a, h := newTestApp(t)
	e := &listEnv{t: t, a: a, h: h, ctx: context.Background(), now: time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)}
	a.now = func() time.Time { return e.now }
	return e
}

func (e *listEnv) ago(days float64) int64 {
	return e.now.Add(-time.Duration(days * 24 * float64(time.Hour))).Unix()
}

// seed makes an item in a stage with messages; the first message sets the item's times.
func (e *listEnv) seed(stage string, msgs ...seedMsg) repo.Item {
	e.t.Helper()
	var it repo.Item
	err := e.a.store.Update(e.ctx, func(tx *repo.Tx) error {
		var err error
		it, err = e.seedIn(tx, stage, msgs...)
		return err
	})
	if err != nil {
		e.t.Fatal(err)
	}
	return it
}

func (e *listEnv) seedIn(tx *repo.Tx, stage string, msgs ...seedMsg) (repo.Item, error) {
	n := seedN.Add(1)
	first, last := msgs[0].at, msgs[0].at
	for _, m := range msgs {
		first, last = min(first, m.at), max(last, m.at)
	}
	it := repo.Item{Kind: "thread", Subject: msgs[len(msgs)-1].subject, Stage: stage, FirstAt: first, LastAt: last, MovedAt: last}
	if stage == stagePending {
		it.PendingSince = last
	}
	it, err := tx.CreateItem(e.ctx, it, "test_thread", fmt.Sprint("t", n), "system", last)
	if err != nil {
		return it, err
	}
	for i, m := range msgs {
		sid := fmt.Sprint("m", n, "-", i)
		src := m.source
		if src == "" {
			src = srcGmail
		}
		rawID, _, err := tx.InsertRaw(e.ctx, repo.Raw{Source: src, SourceID: sid, VersionKey: "a@0", ThreadKey: "gmail:x", ReceivedAt: m.at, IngestedAt: m.at, BlobSHA: "ab", Payload: "{}"})
		if err != nil {
			return it, err
		}
		network := m.network
		if network == "" {
			network = "mail"
		}
		if _, _, err := tx.InsertMessage(e.ctx, repo.Message{ItemID: it.ID, RawID: rawID, Source: src, SourceID: sid, ThreadKey: "gmail:x", Network: network,
			Sender: m.sender, SenderName: m.name, Subject: m.subject, Text: m.text, ReceivedAt: m.at, SentByMe: m.mine}); err != nil {
			return it, err
		}
	}
	return it, nil
}

func (e *listEnv) mail(subject string, days float64) seedMsg {
	return seedMsg{subject: subject, name: "Alice Example", sender: "alice@example.test", text: "Body of " + subject + ".", at: e.ago(days)}
}

func (e *listEnv) ajaxPost(path string, form url.Values) *httptest.ResponseRecorder {
	req := httptest.NewRequest("POST", path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Requested-With", "one")
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	return rec
}

func (e *listEnv) item(id int64) repo.Item {
	e.t.Helper()
	it, ok := e.a.store.ItemByID(e.ctx, id)
	if !ok {
		e.t.Fatal("no such item")
	}
	return it
}

func (e *listEnv) events(id int64, types ...string) []repo.Entry {
	e.t.Helper()
	ev, err := e.a.store.EntriesOfItem(e.ctx, id, types...)
	if err != nil {
		e.t.Fatal(err)
	}
	return ev
}

func (e *listEnv) openRow(id int64) string {
	e.t.Helper()
	rec := get(e.h, fmt.Sprintf("/items/%d/row?open=1", id))
	if rec.Code != 200 {
		e.t.Fatalf("row: %d", rec.Code)
	}
	return rec.Body.String()
}

// TestHeadingCountsAndEmptyStateWording: SPEC 1's heading as two numbers,
// the empty-state wording, and counts on the group headers.
func TestHeadingCountsAndEmptyStateWording(t *testing.T) {
	e := newListEnv(t)
	page := get(e.h, "/").Body.String()
	for _, want := range []string{"<title>ONE</title>", "Nothing needs you", "Nothing here yet.", "Environment: development"} {
		if !strings.Contains(page, want) {
			t.Errorf("empty page lacks %q", want)
		}
	}
	if strings.Contains(page, "g-head") {
		t.Error("an empty list still shows groups")
	}

	e.seed(stageNew, e.mail("one", 1))
	e.seed(stageNew, e.mail("two", 2))
	e.seed(stageInProgress, e.mail("three", 3))
	e.seed(stagePending, e.mail("four", 4))
	e.seed(stageResolved, e.mail("five", 5))
	page = get(e.h, "/").Body.String()
	if !strings.Contains(page, "2 new, 1 in progress") {
		t.Errorf("heading: %s", page)
	}
	for stage, n := range map[string]int{stageNew: 2, stageInProgress: 1, stagePending: 1, stageResolved: 1} {
		want := fmt.Sprintf(`data-count="%s">%d<`, stage, n)
		if !strings.Contains(page, want) {
			t.Errorf("no count %q", want)
		}
	}
	for _, name := range []string{">New<", ">In progress<", ">Pending<", ">Resolved<"} {
		if !strings.Contains(page, name) {
			t.Errorf("no group %q", name)
		}
	}

	// Only pending and resolved items: nothing needs the owner.
	e2 := newListEnv(t)
	e2.seed(stagePending, e2.mail("p", 1))
	if page := get(e2.h, "/").Body.String(); !strings.Contains(page, "Nothing needs you") || strings.Contains(page, "Nothing here yet.") {
		t.Errorf("pending only: %s", page)
	}
}

// TestAllGroupsAreFoldedAndHistoryIsLeftOut: every group is folded and empty
// when the page opens, Closed and history are not groups, and history is
// never served as a list.
func TestAllGroupsAreFoldedAndHistoryIsLeftOut(t *testing.T) {
	e := newListEnv(t)
	e.seed(stageNew, e.mail("visible-new", 1))
	e.seed(stageClosed, e.mail("closed-one", 2))
	e.seed(stageHistory, e.mail("history-one", 400))
	page := get(e.h, "/").Body.String()
	if n := strings.Count(page, `aria-expanded="false"`); n != 5 {
		t.Errorf("%d folded groups, want 5 (the four stages and To do)", n)
	}
	if strings.Contains(page, `aria-expanded="true"`) {
		t.Error("a group is open")
	}
	if n := strings.Count(page, `<ol class="rows"`); n != 4 || strings.Count(page, "hidden></ol>") != 4 {
		t.Errorf("the group bodies are not all empty and hidden")
	}
	for _, bad := range []string{"visible-new", "closed-one", "history-one", ">History<", `data-count="closed"`, `data-count="history"`} {
		if strings.Contains(page, bad) {
			t.Errorf("page holds %q before any group is opened", bad)
		}
	}
	if !strings.Contains(page, "1 new, 0 in progress") {
		t.Error("history and closed items were counted in the heading")
	}
	for _, stage := range []string{"history", "closed", "nonsense"} {
		if rec := get(e.h, "/list/"+stage); rec.Code != 404 {
			t.Errorf("/list/%s: %d", stage, rec.Code)
		}
	}
	rows := get(e.h, "/list/new").Body.String()
	if !strings.Contains(rows, "visible-new") || strings.Contains(rows, "closed-one") || strings.Contains(rows, "history-one") {
		t.Errorf("new group: %s", rows)
	}
	if rec := get(e.h, "/list/resolved"); !strings.Contains(rec.Body.String(), "Nothing in Resolved.") {
		t.Errorf("empty group wording: %s", rec.Body.String())
	}
}

// TestPendingRowsShowHowLongTheyHaveWaited: "waiting <n> days", "waiting
// <n> hours" under a day, and the longest wait on the Pending header.
func TestPendingRowsShowHowLongTheyHaveWaited(t *testing.T) {
	e := newListEnv(t)
	e.seed(stagePending, e.mail("long", 3.2))
	e.seed(stagePending, e.mail("short", 0.25))
	rows := get(e.h, "/list/pending").Body.String()
	if !strings.Contains(rows, "waiting 3 days") || !strings.Contains(rows, "waiting 6 hours") {
		t.Errorf("pending rows: %s", rows)
	}
	page := get(e.h, "/").Body.String()
	if !strings.Contains(page, "longest wait 3 days") {
		t.Errorf("no longest wait: %s", page)
	}
	st := get(e.h, "/list/state").Body.String()
	if !strings.Contains(st, "longest wait 3 days") {
		t.Errorf("state: %s", st)
	}
	if strings.Contains(get(e.h, "/list/new").Body.String(), "waiting ") {
		t.Error("a New row shows a wait")
	}
}

// TestOpeningANewRowMovesItToInProgressAndWritesOneEvent: SPEC 2 on the
// server side: the item is In progress, one event by owner, a second tap
// writes nothing, and the row tops In progress at the next load.
func TestOpeningANewRowMovesItToInProgressAndWritesOneEvent(t *testing.T) {
	e := newListEnv(t)
	older := e.seed(stageInProgress, e.mail("already open", 1))
	it := e.seed(stageNew, e.mail("fresh", 5))
	rec := e.ajaxPost(fmt.Sprintf("/items/%d/open", it.ID), nil)
	if rec.Code != 200 {
		t.Fatalf("open: %d %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, `data-stage="in_progress"`) || !strings.Contains(body, `class="row open"`) || !strings.Contains(body, "Body of fresh.") {
		t.Errorf("the answer is not the open row as it is now: %s", body)
	}
	if got := e.item(it.ID); got.Stage != stageInProgress {
		t.Fatalf("stage %s", got.Stage)
	}
	ev := e.events(it.ID, "opened")
	if len(ev) != 1 || ev[0].Actor != "owner" {
		t.Fatalf("events: %+v", ev)
	}
	e.ajaxPost(fmt.Sprintf("/items/%d/open", it.ID), nil)
	if n := len(e.events(it.ID, "opened")); n != 1 {
		t.Errorf("a second tap wrote %d events", n)
	}
	// Once it is listed again it is at the top of In progress.
	rows := get(e.h, "/list/in_progress").Body.String()
	if strings.Index(rows, "fresh") < 0 || strings.Index(rows, "fresh") > strings.Index(rows, "already open") {
		t.Errorf("order in progress: %s", rows)
	}
	_ = older
	// The same from a page without scripts: a redirect, and the same effect.
	it2 := e.seed(stageNew, e.mail("no script", 6))
	if rec := post(e.h, fmt.Sprintf("/items/%d/open", it2.ID), nil); rec.Code != http.StatusSeeOther {
		t.Errorf("plain post: %d", rec.Code)
	}
	if e.item(it2.ID).Stage != stageInProgress {
		t.Error("plain post did not open the item")
	}
	if rec := e.ajaxPost("/items/999999/open", nil); rec.Code != 404 {
		t.Errorf("unknown item: %d", rec.Code)
	}
	if rec := e.ajaxPost(fmt.Sprintf("/items/%d/frobnicate", it.ID), nil); rec.Code != 404 {
		t.Errorf("unknown action: %d", rec.Code)
	}
}

// TestOnlyANewMessageFromSomeoneElseMovesARow: what the owner does leaves the
// other rows where they are, and in the table of arrivals only a message from
// someone else takes a row's place.
func TestOnlyANewMessageFromSomeoneElseMovesARow(t *testing.T) {
	e := newListEnv(t)
	a := e.seed(stageNew, e.mail("a", 3))
	b := e.seed(stageNew, e.mail("b", 2))
	c := e.seed(stageNew, e.mail("c", 1))
	e.ajaxPost(fmt.Sprintf("/items/%d/open", b.ID), nil)
	rows := get(e.h, "/list/new").Body.String()
	if strings.Index(rows, ">c<") > strings.Index(rows, ">a<") || strings.Contains(rows, ">b<") {
		t.Errorf("The owner opening b moved the others: %s", rows)
	}
	_ = c

	cutoff := e.ago(30)
	now := e.now.Unix()
	it := e.item(a.ID)
	it.MovedAt = 111
	got, _ := applyArrival(it, arrival{At: e.ago(0.5), SentByMe: false}, cutoff, now)
	if got.MovedAt != e.ago(0.5) {
		t.Errorf("a message from someone else: moved_at %d", got.MovedAt)
	}
	got, _ = applyArrival(it, arrival{At: e.ago(0.5), SentByMe: true}, cutoff, now)
	if got.MovedAt != 111 {
		t.Errorf("The owner's own message on a New item moved it: %d", got.MovedAt)
	}
	it.Stage = stageInProgress
	got, ch := applyArrival(it, arrival{At: e.ago(0.5), SentByMe: true}, cutoff, now)
	if !ch.SetPending || got.Stage != stagePending || got.MovedAt != now {
		t.Errorf("The owner's reply on an item in progress: %+v %+v", got, ch)
	}
}

// TestOwnerChangesStageFromTheRow: each stage change of SPEC 3, with its
// event by owner, its notice and its buttons.
func TestOwnerChangesStageFromTheRow(t *testing.T) {
	e := newListEnv(t)
	it := e.seed(stageInProgress, e.mail("work", 1))
	act := func(action string) string {
		rec := e.ajaxPost(fmt.Sprintf("/items/%d/%s", it.ID, action), url.Values{"open": {"1"}})
		if rec.Code != 200 {
			t.Fatalf("%s: %d %s", action, rec.Code, rec.Body.String())
		}
		return rec.Body.String()
	}
	stage := func() string { return e.item(it.ID).Stage }

	body := act("pending")
	if stage() != stagePending || !strings.Contains(body, "You marked this pending.") || !strings.Contains(body, ">Resolve<") || !strings.Contains(body, ">Reopen<") || strings.Contains(body, ">Mark pending<") {
		t.Errorf("mark pending: stage %s\n%s", stage(), body)
	}
	if got := e.item(it.ID); got.PendingSince != e.now.Unix() {
		t.Errorf("pending since %d", got.PendingSince)
	}
	body = act("resolve")
	if stage() != stageResolved || !strings.Contains(body, "You resolved this.") || strings.Contains(body, ">Resolve<") || !strings.Contains(body, ">Reopen<") {
		t.Errorf("resolve: stage %s\n%s", stage(), body)
	}
	if got := e.item(it.ID); got.PendingSince != 0 {
		t.Error("a resolved item still waits")
	}
	body = act("reopen")
	if stage() != stageInProgress || !strings.Contains(body, "You reopened this.") || !strings.Contains(body, ">Mark pending<") {
		t.Errorf("reopen: stage %s\n%s", stage(), body)
	}
	act("pending")
	act("reopen") // out of Pending too
	if stage() != stageInProgress {
		t.Errorf("reopen from pending: %s", stage())
	}

	var types []string
	for _, ev := range e.events(it.ID, "pending_set", "resolved", "unresolved", "pending_cleared") {
		if ev.Actor != "owner" {
			t.Errorf("event %s by %s", ev.Type, ev.Actor)
		}
		types = append(types, ev.Type)
	}
	if got := strings.Join(types, ","); got != "pending_set,resolved,unresolved,pending_set,pending_cleared" {
		t.Errorf("events %s", got)
	}

	// A New item can be resolved or marked pending straight from its row.
	n1 := e.seed(stageNew, e.mail("n1", 1))
	n2 := e.seed(stageNew, e.mail("n2", 1))
	e.ajaxPost(fmt.Sprintf("/items/%d/resolve", n1.ID), nil)
	e.ajaxPost(fmt.Sprintf("/items/%d/pending", n2.ID), nil)
	if e.item(n1.ID).Stage != stageResolved || e.item(n2.ID).Stage != stagePending {
		t.Error("stage changes from a New row")
	}
	// What does not fit writes nothing: Reopen on a New item, Mark pending on a resolved one.
	before := len(e.events(n1.ID, "pending_set", "resolved", "unresolved", "pending_cleared", "opened"))
	e.ajaxPost(fmt.Sprintf("/items/%d/pending", n1.ID), nil)
	n3 := e.seed(stageNew, e.mail("n3", 1))
	e.ajaxPost(fmt.Sprintf("/items/%d/reopen", n3.ID), nil)
	if after := len(e.events(n1.ID, "pending_set", "resolved", "unresolved", "pending_cleared", "opened")); after != before || e.item(n1.ID).Stage != stageResolved || e.item(n3.ID).Stage != stageNew {
		t.Error("an action that does not fit changed something")
	}
}

// TestThreadShowsTheLastMonthAndShowOlder: SPEC 9, newest first, with the
// older messages left out until the button is used.
func TestThreadShowsTheLastMonthAndShowOlder(t *testing.T) {
	e := newListEnv(t)
	it := e.seed(stageNew,
		seedMsg{subject: "Long chat", name: "Bob", sender: "bob@example.test", text: "message-from-two-months-ago", at: e.ago(61)},
		seedMsg{subject: "Long chat", name: "Bob", sender: "bob@example.test", text: "message-from-forty-days-ago", at: e.ago(40)},
		seedMsg{subject: "Long chat", name: "Bob", sender: "bob@example.test", text: "message-from-ten-days-ago", at: e.ago(10)},
		seedMsg{subject: "Long chat", name: "Bob", sender: "bob@example.test", text: "message-from-today", at: e.ago(0.1)},
	)
	body := e.openRow(it.ID)
	for _, want := range []string{"message-from-ten-days-ago", "message-from-today", "2 older messages", "Show older", "4 messages"} {
		if !strings.Contains(body, want) {
			t.Errorf("open row lacks %q", want)
		}
	}
	if strings.Contains(body, "message-from-forty") || strings.Contains(body, "message-from-two-months") {
		t.Error("messages older than a month are shown at once")
	}
	msgs := body[strings.Index(body, `<ol class="msgs"`):]
	if strings.Index(msgs, "message-from-ten") < strings.Index(msgs, "message-from-today") {
		t.Error("the messages are not newest first")
	}
	rec := get(e.h, fmt.Sprintf("/items/%d/messages", it.ID))
	all := rec.Body.String()
	if rec.Code != 200 || strings.Contains(all, "Show older") || strings.Contains(all, "older messages") {
		t.Errorf("show older: %d %s", rec.Code, all)
	}
	i1, i2, i3, i4 := strings.Index(all, "two-months"), strings.Index(all, "forty-days"), strings.Index(all, "ten-days"), strings.Index(all, "from-today")
	if i1 < 0 || !(i1 > i2 && i2 > i3 && i3 > i4) {
		t.Errorf("all messages, newest first: %d %d %d %d", i1, i2, i3, i4)
	}
	// A chat that went quiet long ago still shows its own last month.
	old := e.seed(stageResolved, e.mail("ancient", 500))
	if b := e.openRow(old.ID); !strings.Contains(b, "Body of ancient.") || strings.Contains(b, "Show older") {
		t.Errorf("an item whose newest message is old: %s", b)
	}
	if rec := get(e.h, "/items/999999/messages"); rec.Code != 404 {
		t.Errorf("unknown item: %d", rec.Code)
	}
}

// TestMailTextIsRenderedEscaped: a subject, a sender name, a body and a file
// name with markup show the markup itself and run nothing.
func TestMailTextIsRenderedEscaped(t *testing.T) {
	e := newListEnv(t)
	const evil = `<script>alert(1)</script>`
	it := e.seed(stageNew, seedMsg{subject: evil + " Offer", name: evil + "Eve", sender: "eve@example.test",
		text: evil + ` <img src="https://tracker.example.test/p.gif"> <a href="javascript:x()">link</a>`, at: e.ago(1)})
	var fid int64
	e.a.store.Update(e.ctx, func(tx *repo.Tx) error {
		m, _ := tx.MessageBySource(e.ctx, srcGmail, fmt.Sprint("m", seedN.Load(), "-0"))
		var err error
		fid, err = tx.InsertFile(e.ctx, repo.File{ItemID: it.ID, Source: srcGmail, MessageID: m.ID, Part: "2", Filename: evil + ".txt", ContentType: "text/plain", Disposition: "attachment", Status: "pending", AddedAt: 1})
		return err
	})
	_ = fid
	for name, page := range map[string]string{"list": get(e.h, "/list/new").Body.String(), "open row": e.openRow(it.ID)} {
		if strings.Contains(page, "<script>alert") || strings.Contains(page, `<img src="https://tracker`) || strings.Contains(page, `<a href="javascript`) {
			t.Errorf("%s inserted mail text as markup:\n%s", name, page)
		}
		if !strings.Contains(page, "&lt;script&gt;alert(1)&lt;/script&gt;") {
			t.Errorf("%s does not show the tag as text", name)
		}
	}
	if body := e.openRow(it.ID); strings.Contains(body, "https://tracker.example.test/p.gif\"") && strings.Contains(body, "<img class=\"pic\" src=\"https") {
		t.Error("a picture was loaded from a sender's address")
	}
	// The page holds no inline script or style.
	rec := get(e.h, "/")
	body := rec.Body.String()
	if strings.Contains(body, "<script>") || strings.Contains(body, " style=") || strings.Contains(body, " onclick=") {
		t.Error("inline script or style on the list page")
	}
	if csp := rec.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "script-src 'self'") || !strings.Contains(csp, "style-src 'self'") || strings.Contains(csp, "unsafe") {
		t.Errorf("csp %q", csp)
	}
}

func (e *listEnv) saveFile(it repo.Item, msgSource, name, ctype, disp string, content []byte) int64 {
	e.t.Helper()
	sha, rel, size, _, err := e.a.imp.pool.write(strings.NewReader(string(content)))
	if err != nil {
		e.t.Fatal(err)
	}
	var id int64
	err = e.a.store.Update(e.ctx, func(tx *repo.Tx) error {
		m, ok := tx.MessageBySource(e.ctx, srcGmail, msgSource)
		if !ok {
			e.t.Fatal("no message")
		}
		id, err = tx.InsertFile(e.ctx, repo.File{ItemID: it.ID, Source: srcGmail, MessageID: m.ID, Part: fmt.Sprint(name, len(content)), Filename: name, ContentType: ctype, Disposition: disp,
			Size: size, SHA: sha, Path: rel, Status: "saved", AddedAt: 1})
		return err
	})
	if err != nil {
		e.t.Fatal(err)
	}
	return id
}

// TestFilesAreServedFromThePoolAsDownloads: a saved file goes out as an
// attachment with safe headers, a picture keeps its type only when its bytes
// agree, and an unknown or unsaved file is a 404.
func TestFilesAreServedFromThePoolAsDownloads(t *testing.T) {
	e := newListEnv(t)
	it := e.seed(stageNew, e.mail("with files", 1))
	src := fmt.Sprint("m", seedN.Load(), "-0")
	png := append([]byte("\x89PNG\r\n\x1a\n"), make([]byte, 64)...)
	pngID := e.saveFile(it, src, "logo.png", "image/png", "inline", png)
	pdfID := e.saveFile(it, src, "terms \"final\".pdf", "application/pdf", "attachment", []byte("%PDF-1.4 terms"))
	htmlID := e.saveFile(it, src, "page.html", "text/html", "attachment", []byte("<html><script>alert(1)</script></html>"))
	fakeID := e.saveFile(it, src, "fake.png", "image/png", "inline", []byte("<html><script>alert(1)</script></html>"))
	var pendingID int64
	e.a.store.Update(e.ctx, func(tx *repo.Tx) error {
		m, _ := tx.MessageBySource(e.ctx, srcGmail, src)
		pendingID, _ = tx.InsertFile(e.ctx, repo.File{ItemID: it.ID, Source: srcGmail, MessageID: m.ID, Part: "9", Filename: "later.zip", ContentType: "application/zip", Disposition: "attachment", Status: "pending", AddedAt: 1})
		return nil
	})

	check := func(id int64, wantType, wantBody string) {
		t.Helper()
		rec := get(e.h, fmt.Sprintf("/files/%d", id))
		if rec.Code != 200 {
			t.Fatalf("file %d: %d", id, rec.Code)
		}
		h := rec.Header()
		if !strings.HasPrefix(h.Get("Content-Disposition"), "attachment") {
			t.Errorf("file %d: Content-Disposition %q", id, h.Get("Content-Disposition"))
		}
		if h.Get("Content-Type") != wantType || h.Get("X-Content-Type-Options") != "nosniff" || !strings.Contains(h.Get("Content-Security-Policy"), "sandbox") {
			t.Errorf("file %d: headers %v", id, h)
		}
		if !strings.HasPrefix(rec.Body.String(), wantBody) {
			t.Errorf("file %d: body %q", id, rec.Body.String())
		}
	}
	check(pngID, "image/png", "\x89PNG")
	check(pdfID, "application/octet-stream", "%PDF")
	check(htmlID, "application/octet-stream", "<html>")
	check(fakeID, "application/octet-stream", "<html>")
	if cd := get(e.h, fmt.Sprintf("/files/%d", pdfID)).Header().Get("Content-Disposition"); !strings.Contains(cd, "terms") || strings.Contains(cd, "\n") {
		t.Errorf("file name in %q", cd)
	}
	for _, path := range []string{"/files/999999", "/files/abc", "/files/0", "/files/-1", fmt.Sprintf("/files/%d", pendingID)} {
		if rec := get(e.h, path); rec.Code != 404 {
			t.Errorf("%s: %d", path, rec.Code)
		}
	}

	// In the open row: the real picture is shown from the app's own copy, the
	// other files are named download links, and nothing points elsewhere.
	body := e.openRow(it.ID)
	if !strings.Contains(body, fmt.Sprintf(`<img class="pic pic-fit" src="/files/%d"`, pngID)) {
		t.Errorf("the picture is not shown from the pool:\n%s", body)
	}
	if !strings.Contains(body, fmt.Sprintf(`href="/files/%d" download`, pdfID)) || !strings.Contains(body, "later.zip") || strings.Contains(body, fmt.Sprintf("/files/%d", pendingID)) {
		t.Errorf("file chips:\n%s", body)
	}
	for _, bad := range []string{`src="http`, `href="http`, `src="//`} {
		if strings.Contains(body, bad) {
			t.Errorf("the row points outside the app: %q", bad)
		}
	}
}

// TestANoteIsStoredAsAnEventAndShown: SPEC 26's note part and SPEC 6's thread.
func TestANoteIsStoredAsAnEventAndShown(t *testing.T) {
	e := newListEnv(t)
	it := e.seed(stageInProgress, e.mail("note me", 1))
	if body := e.openRow(it.ID); !strings.Contains(body, "No notes yet.") || !strings.Contains(body, `placeholder="Add a note or ask Claude…"`) || !strings.Contains(body, ">Send<") {
		t.Errorf("an empty thread: %s", body)
	}
	rec := e.ajaxPost(fmt.Sprintf("/items/%d/notes", it.ID), url.Values{"text": {"  Please <b>check</b> the invoice  "}})
	if rec.Code != 200 {
		t.Fatalf("note: %d %s", rec.Code, rec.Body.String())
	}
	out := rec.Body.String()
	if !strings.Contains(out, "Please &lt;b&gt;check&lt;/b&gt; the invoice") || !strings.Contains(out, "<b>you</b>") || strings.Contains(out, "No notes yet.") {
		t.Errorf("the thread after a note: %s", out)
	}
	ev := e.events(it.ID, "noted")
	if len(ev) != 1 || ev[0].Actor != "owner" || !strings.Contains(ev[0].Data, "check") {
		t.Fatalf("events %+v", ev)
	}
	if !e.item(it.ID).NeedsClaude {
		t.Error("the note did not queue the item for Claude")
	}
	if e.item(it.ID).Stage != stageInProgress {
		t.Error("a note moved the item")
	}
	e.ajaxPost(fmt.Sprintf("/items/%d/notes", it.ID), url.Values{"text": {"second note"}})
	body := e.openRow(it.ID)
	if strings.Index(body, "check") > strings.Index(body, "second note") || strings.Index(body, "second note") < 0 {
		t.Error("notes are not in time order")
	}
	// Notices and notes share the thread in time order.
	e.now = e.now.Add(time.Minute)
	e.ajaxPost(fmt.Sprintf("/items/%d/resolve", it.ID), nil)
	body = e.openRow(it.ID)
	if strings.Index(body, "second note") > strings.Index(body, "You resolved this.") {
		t.Error("the notice is before the notes")
	}

	for _, text := range []string{"", "   \n\t "} {
		rec := e.ajaxPost(fmt.Sprintf("/items/%d/notes", it.ID), url.Values{"text": {text}})
		if rec.Code != 400 || !strings.Contains(rec.Body.String(), "Write something first.") {
			t.Errorf("empty note: %d %s", rec.Code, rec.Body.String())
		}
	}
	if n := len(e.events(it.ID, "noted")); n != 2 {
		t.Errorf("%d notes stored after two refused ones", n)
	}
	if rec := e.ajaxPost("/items/999999/notes", url.Values{"text": {"x"}}); rec.Code != 404 {
		t.Errorf("unknown item: %d", rec.Code)
	}
}

// TestACrossSitePostIsRefused: every page that changes something refuses a
// request marked as coming from another site, and writes nothing.
func TestACrossSitePostIsRefused(t *testing.T) {
	e := newListEnv(t)
	it := e.seed(stageNew, e.mail("guarded", 1))
	for _, path := range []string{"open", "pending", "resolve", "reopen", "notes"} {
		req := httptest.NewRequest("POST", fmt.Sprintf("/items/%d/%s", it.ID, path), strings.NewReader("text=x"))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Sec-Fetch-Site", "cross-site")
		rec := httptest.NewRecorder()
		e.h.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s: %d", path, rec.Code)
		}
	}
	if e.item(it.ID).Stage != stageNew || len(e.events(it.ID, "opened", "pending_set", "resolved", "noted")) != 0 {
		t.Error("a refused post changed the item")
	}
}

// TestGroupsArePagedAndFastWithManyItems: thousands of items, a group loads
// fifty rows at a time and says whether there are more.
func TestGroupsArePagedAndFastWithManyItems(t *testing.T) {
	e := newListEnv(t)
	err := e.a.store.Update(e.ctx, func(tx *repo.Tx) error {
		for i := 0; i < 3000; i++ {
			if _, err := e.seedIn(tx, stageNew, seedMsg{subject: fmt.Sprint("subject ", i), name: "Alice", sender: "alice@example.test", text: "x", at: e.ago(1) - int64(i)}); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	page := get(e.h, "/").Body.String()
	if !strings.Contains(page, "3000 new, 0 in progress") || strings.Contains(page, "subject 1") {
		t.Error("the first page of 3000 items")
	}
	first := get(e.h, "/list/new").Body.String()
	if n := strings.Count(first, `<li class="row`); n != groupPage || !strings.Contains(first, `data-next="50"`) {
		t.Errorf("first page: %d rows, next? %v", n, strings.Contains(first, "data-next"))
	}
	if !strings.Contains(first, "subject 0") || strings.Contains(first, "subject 50<") {
		t.Error("the newest item is not first")
	}
	last := get(e.h, "/list/new?offset=2950").Body.String()
	if n := strings.Count(last, `<li class="row`); n != 50 || strings.Contains(last, "data-next") {
		t.Errorf("last page: %d rows", n)
	}
	if empty := get(e.h, "/list/new?offset=9000").Body.String(); strings.Contains(empty, "<li") {
		t.Errorf("past the end: %s", empty)
	}
}

// TestNewItemsButtonCountsOnlyArrivals: the state answer offers the button
// for items that arrived or came back after the page was built, and the owner's
// own actions are not arrivals.
func TestNewItemsButtonCountsOnlyArrivals(t *testing.T) {
	e := newListEnv(t)
	it := e.seed(stageNew, e.mail("known", 1))
	since := e.a.store.LastEventID(e.ctx) // the mark the page takes when it is drawn
	state := func() string { return get(e.h, fmt.Sprintf("/list/state?since=%d", since)).Body.String() }
	if s := state(); !strings.Contains(s, `"waiting":0`) {
		t.Errorf("nothing arrived: %s", s)
	}
	e.now = e.now.Add(time.Minute)
	e.ajaxPost(fmt.Sprintf("/items/%d/open", it.ID), nil)
	e.ajaxPost(fmt.Sprintf("/items/%d/notes", it.ID), url.Values{"text": {"x"}})
	if s := state(); !strings.Contains(s, `"waiting":0`) {
		t.Errorf("The owner's own actions counted: %s", s)
	}
	add := func(id int64, typ string) {
		e.a.store.Update(e.ctx, func(tx *repo.Tx) error {
			return tx.InsertEvent(e.ctx, e.now.Unix(), id, "system", typ, nil)
		})
	}
	add(it.ID, "ingested")
	if s := state(); !strings.Contains(s, `"waiting":1`) || !strings.Contains(s, `"button":"1 new item"`) {
		t.Errorf("one arrival: %s", s)
	}
	other := e.seed(stageResolved, e.mail("came back", 2))
	add(other.ID, "reopened")
	add(other.ID, "ingested")
	hist := e.seed(stageHistory, e.mail("history", 400))
	add(hist.ID, "ingested")
	if s := state(); !strings.Contains(s, `"waiting":2`) || !strings.Contains(s, `"button":"2 new items"`) {
		t.Errorf("two arrivals: %s", s)
	}
}

// SPEC 2: an arrival in the very second the page was drawn, and on an app that
// was empty when the page was drawn (mark 0), still offers the button.
func TestSpec2NewItemsButtonAppearsForAnArrivalInTheSameSecondOnAnEmptyApp(t *testing.T) {
	e := newListEnv(t)
	page := get(e.h, "/").Body.String()
	if !strings.Contains(page, `data-since="0"`) {
		t.Fatalf("an empty app should draw the page with mark 0: %.300s", page)
	}
	it := e.seed(stageNew, e.mail("arrived", 0))
	e.a.store.Update(e.ctx, func(tx *repo.Tx) error {
		return tx.InsertEvent(e.ctx, e.now.Unix(), it.ID, "system", "ingested", nil)
	})
	s := get(e.h, "/list/state?since=0").Body.String()
	if !strings.Contains(s, `"waiting":1`) || !strings.Contains(s, `"button":"1 new item"`) {
		t.Errorf("same-second arrival on an empty app: %s", s)
	}
}

// TestConnectorsShowTotalsAndNoTable: the Connectors page says how many
// messages and items are stored in all, and no longer lists items.
func TestConnectorsShowTotalsAndNoTable(t *testing.T) {
	e := newListEnv(t)
	writeSecret(e.a.secretsDir, gmailSecretFile, gmailSecret{Address: testAddr, Password: testPass})
	e.seed(stageNew, e.mail("a", 1), e.mail("a2", 1))
	e.seed(stageHistory, e.mail("b", 400), e.mail("b2", 401), e.mail("b3", 402))
	page := get(e.h, "/connectors").Body.String()
	for _, want := range []string{"5 messages in 2 items stored."} {
		if !strings.Contains(page, want) {
			t.Errorf("page lacks %q", want)
		}
	}
	if strings.Contains(page, "Latest items") {
		t.Error("the temporary table is still there")
	}
}

// The phone's Undo after a one-tap resolve returns the item to the stage it
// held before (New stays New), and only right after the owner's own resolve.
func TestUndoAfterResolveReturnsTheItemToItsStage(t *testing.T) {
	e := newListEnv(t)
	post := func(it repo.Item, action string) { e.ajaxPost(fmt.Sprintf("/items/%d/%s", it.ID, action), nil) }
	for _, from := range []string{stageNew, stageInProgress, stagePending} {
		it := e.seed(from, e.mail("u-"+from, 1))
		post(it, "resolve")
		if e.item(it.ID).Stage != stageResolved {
			t.Fatalf("%s: not resolved", from)
		}
		post(it, "undo")
		if got := e.item(it.ID).Stage; got != from {
			t.Errorf("undo after a resolve from %s left it in %s", from, got)
		}
	}
	// Not right after a resolve of the owner's: nothing moves.
	it := e.seed(stageNew, e.mail("u-late", 1))
	post(it, "resolve")
	post(it, "reopen")
	post(it, "resolve") // now resolved from in progress
	post(it, "undo")
	if got := e.item(it.ID).Stage; got != stageInProgress {
		t.Errorf("undo went to %s, want in_progress (the stage before the latest resolve)", got)
	}
	post(it, "undo") // a second tap: it is not resolved any more
	if got := e.item(it.ID).Stage; got != stageInProgress {
		t.Errorf("second undo moved it to %s", got)
	}
	// An item that was never resolved by the owner, or is resolved some other way, is not moved by undo.
	n := e.seed(stageNew, e.mail("u-new", 1))
	post(n, "undo")
	r := e.seed(stageResolved, e.mail("u-seeded", 1))
	post(r, "undo")
	if e.item(n.ID).Stage != stageNew || e.item(r.ID).Stage != stageResolved {
		t.Error("undo moved an item that the owner had not just resolved")
	}
}

// The phone's Undo after a one-tap reopen puts the item back to Resolved, and
// only right after the owner's own reopen; a second Undo moves nothing.
func TestUndoAfterReopenReturnsTheItemToResolved(t *testing.T) {
	e := newListEnv(t)
	post := func(it repo.Item, action string) { e.ajaxPost(fmt.Sprintf("/items/%d/%s", it.ID, action), nil) }
	it := e.seed(stageResolved, e.mail("r-1", 1))
	post(it, "reopen")
	if got := e.item(it.ID).Stage; got != stageInProgress {
		t.Fatalf("reopen left it in %s", got)
	}
	post(it, "undo")
	if got := e.item(it.ID).Stage; got != stageResolved {
		t.Errorf("undo after a reopen left it in %s, want resolved", got)
	}
	post(it, "undo") // a second tap: the latest move is the undo itself
	if got := e.item(it.ID).Stage; got != stageResolved {
		t.Errorf("a second undo moved it to %s", got)
	}
	// once more: reopen, then the owner moves it on (pending): undo no longer fits
	post(it, "reopen")
	post(it, "pending")
	post(it, "undo")
	if got := e.item(it.ID).Stage; got != stagePending {
		t.Errorf("undo moved an item that had moved on: %s", got)
	}
	// a reopen that is not from Resolved (pending cleared) is not undone to Resolved
	p := e.seed(stagePending, e.mail("r-2", 1))
	post(p, "reopen")
	post(p, "undo")
	if got := e.item(p.ID).Stage; got != stageInProgress {
		t.Errorf("undo after clearing pending went to %s", got)
	}
}
