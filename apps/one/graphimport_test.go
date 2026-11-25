package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"lab/apps/one/repo"
)

const graphSelf = "owner@example.test"

// genv is one app wired to the fake Microsoft and its mailbox, with a clock the test controls.
type genv struct {
	t    *testing.T
	a    *app
	h    http.Handler
	ms   *fakeMS
	box  *fakeGraphBox
	ctx  context.Context
	base time.Time
	skew atomic.Int64
}

func newGraphEnv(t *testing.T) *genv {
	t.Helper()
	a, h := newTestApp(t)
	e := &genv{t: t, a: a, h: h, ctx: context.Background(), base: time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)}
	a.now = func() time.Time { return e.base.Add(time.Duration(e.skew.Load()) * time.Second) }
	e.ms = useFakeMS(t, a)
	e.ms.refreshErr = ""
	e.box = e.ms.box
	e.box.standard()
	a.graph.RetryWait = 5 * time.Millisecond
	e.must(a.store.SetSetting(e.ctx, keyGraphTenant, testTenant))
	e.must(a.store.SetSetting(e.ctx, keyGraphClient, testClient))
	e.signIn()
	a.gimp.pageSize = 3
	a.imp.seedRules(e.ctx)
	return e
}

// signIn writes the sign-in the page would have kept: an access token with an hour left.
func (e *genv) signIn() {
	e.must(writeSecret(e.a.secretsDir, graphSecretFile, graphSecret{Address: graphSelf, AccessToken: secretTok, RefreshToken: secretRef, ExpiresAt: time.Now().Add(time.Hour).Unix()}))
}

func (e *genv) must(err error) {
	e.t.Helper()
	if err != nil {
		e.t.Fatal(err)
	}
}

func (e *genv) ago(days float64) time.Time {
	return e.a.now().Add(-time.Duration(days * 24 * float64(time.Hour)))
}

// in is a mail from someone else received days ago, in the Inbox.
func (e *genv) in(conv, subject string, days float64) testMail {
	return testMail{thrid: conv, from: "Jana Example <jana@example.test>", to: graphSelf, subject: subject, date: e.ago(days),
		text: "Hello, this is " + subject + "."}
}

// out is a mail the owner sent himself.
func (e *genv) out(conv, subject string, days float64) testMail {
	m := e.in(conv, subject, days)
	m.from, m.to = graphSelf, "jana@example.test"
	m.flags = []string{`\Seen`}
	return m
}

func (e *genv) importMonth() error { return e.a.gimp.RunImport(e.ctx, "month", 30*24*time.Hour) }
func (e *genv) importYear() error  { return e.a.gimp.RunImport(e.ctx, "first", 365*24*time.Hour) }
func (e *genv) sync() error        { return e.a.gimp.RunSync(e.ctx) }

func (e *genv) counts() map[string]int {
	out := map[string]int{"pool": e.poolFiles()}
	for _, t := range countedTables {
		out[t] = e.a.store.CountRows(e.ctx, t)
	}
	return out
}

func (e *genv) poolFiles() int {
	n := 0
	root := filepath.Join(e.a.dataDir, "files")
	filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		rel, _ := filepath.Rel(root, p)
		if err == nil && !d.IsDir() && !strings.HasPrefix(rel, "tmp"+string(filepath.Separator)) {
			n++
		}
		return nil
	})
	return n
}

func (e *genv) item(conv string) repo.Item {
	e.t.Helper()
	it, ok := e.a.store.ItemByLink(e.ctx, graphSrc.LinkKind, conv)
	if !ok {
		e.t.Fatalf("no item for conversation %s", conv)
	}
	return it
}

func (e *genv) setItem(conv string, fn func(*repo.Item)) {
	it := e.item(conv)
	fn(&it)
	e.must(e.a.store.Update(e.ctx, func(tx *repo.Tx) error { return tx.SaveItem(e.ctx, it) }))
}

func (e *genv) message(id string) repo.Message {
	e.t.Helper()
	m, ok := e.a.store.MessageBySource(e.ctx, srcGraph, id)
	if !ok {
		e.t.Fatalf("no message %s", id)
	}
	return m
}

func (e *genv) lastRun(modes ...string) repo.Run {
	e.t.Helper()
	r, ok := e.a.store.LatestRun(e.ctx, srcGraph, modes...)
	if !ok {
		e.t.Fatalf("no run of %v", modes)
	}
	return r
}

func (e *genv) event(it repo.Item, typ string) bool {
	return slicesContains(e.a.store.EventTypes(e.ctx, it.ID), typ)
}

func slicesContains(l []string, s string) bool {
	for _, x := range l {
		if x == s {
			return true
		}
	}
	return false
}

// TestGraphRepeatWritesNothingTheSecondTime: the same unchanged mailbox
// imported again, and synced again, writes no raw row, no message, no event
// and no file, and downloads no original a second time.
func TestGraphRepeatWritesNothingTheSecondTime(t *testing.T) {
	e := newGraphEnv(t)
	logo := []byte("\x89PNG-not-really-a-picture-but-bytes")
	m1 := e.in("A", "First of A", 3)
	m1.attach = []attachment{{name: "terms.pdf", ctype: "application/pdf", content: []byte("%PDF-1.4 terms")}, {name: "logo.png", ctype: "image/png", content: logo, inline: true, cid: "logo1"}}
	e.box.add("F-inbox", m1)
	e.box.add("F-inbox", e.in("A", "Re: First of A", 2))
	e.box.add("F-proj", e.in("B", "Second matter", 1))
	e.must(e.importMonth())
	first := e.counts()
	if first["raw"] != 3 || first["messages"] != 3 || first["items"] != 2 || first["files"] != 2 || first["pool"] != 5 {
		t.Fatalf("first import wrote %v", first)
	}
	bodies := len(e.box.mimeFetched())

	e.must(e.importMonth())
	e.must(e.sync())
	e.must(e.sync())
	if again := e.counts(); !reflect.DeepEqual(first, again) {
		t.Fatalf("a repeat changed what is stored:\nfirst %v\nagain %v", first, again)
	}
	if got := len(e.box.mimeFetched()); got != bodies {
		t.Fatalf("a repeat downloaded %d more originals", got-bodies)
	}
	if r := e.lastRun("month"); r.Stored != 0 || r.Skipped != 0 || r.Status != "done" || r.Fetched != 3 {
		t.Fatalf("second import: %+v", r)
	}
	if r := e.lastRun("sync"); r.Stored != 0 || r.Fetched != 0 || r.Status != "done" {
		t.Fatalf("a sync with nothing new read something: %+v", r)
	}
}

// TestGraphIsReadOnlyTowardsTheMailbox: every call to the mailbox is a read,
// ids are asked for as immutable ones, and the only thing the app can send
// is still the test mail to the connected address.
func TestGraphIsReadOnlyTowardsTheMailbox(t *testing.T) {
	e := newGraphEnv(t)
	e.box.add("F-inbox", e.in("A", "One", 2))
	e.box.add("F-sent", e.out("A", "Re: One", 1))
	e.must(e.importMonth())
	e.must(e.sync())
	calls := e.box.callList()
	if len(calls) < 5 {
		t.Fatalf("only %d calls: %v", len(calls), calls)
	}
	for i, c := range calls {
		if !strings.HasPrefix(c, "GET ") {
			t.Fatalf("a call that is not a read: %q", c)
		}
		if strings.Contains(c, "/me/messages/") || strings.Contains(c, "mailFolders") {
			if !strings.Contains(e.box.prefers[i], `IdType="ImmutableId"`) {
				t.Fatalf("call %q did not ask for immutable ids: %q", c, e.box.prefers[i])
			}
		}
	}
	if len(e.ms.sent) != 0 {
		t.Fatal("the import sent a mail")
	}
}

// TestGraphVersionsChangedReadStateCategoryFlagOrFolderIsOneNewRawVersion:
// a changed read state, flag, category or folder gives one new raw version,
// the same one message, and the item does not wait for Claude again.
func TestGraphVersionsChangedReadStateCategoryFlagOrFolderIsOneNewRawVersion(t *testing.T) {
	e := newGraphEnv(t)
	gm := e.box.add("F-inbox", e.in("A", "Offer", 2))
	e.must(e.importMonth())
	e.setItem("A", func(it *repo.Item) { it.NeedsClaude = false; it.Stage = stageInProgress })
	before := e.counts()
	it := e.item("A")
	evBefore := len(e.a.store.EventTypes(e.ctx, it.ID))

	e.box.set(gm.id, func(m *gmMsg) { m.read, m.flagged, m.cats = true, true, []string{"Blue category"} })
	e.must(e.sync())
	after := e.counts()
	if after["raw"] != before["raw"]+1 {
		t.Fatalf("raw rows %d -> %d, want one new version", before["raw"], after["raw"])
	}
	for _, k := range []string{"messages", "items", "events", "files", "pool", "people"} {
		if after[k] != before[k] {
			t.Errorf("%s changed from %d to %d", k, before[k], after[k])
		}
	}
	m := e.message(gm.id)
	if !m.Seen || !m.Starred || !reflect.DeepEqual(m.Labels, []string{"Inbox", "Blue category"}) {
		t.Fatalf("message not updated: %+v", m)
	}
	got := e.item("A")
	if got.NeedsClaude || got.Stage != stageInProgress || len(e.a.store.EventTypes(e.ctx, got.ID)) != evBefore {
		t.Fatalf("the item was touched: %+v", got)
	}

	// Only the category: one more version.
	e.box.set(gm.id, func(m *gmMsg) { m.cats = []string{"Green category"} })
	e.must(e.sync())
	if e.a.store.CountRows(e.ctx, "raw") != before["raw"]+2 {
		t.Fatal("a changed category was not kept as a version")
	}
	// Moved to another folder: one more version, the folder follows.
	e.box.move(gm.id, "F-archive")
	e.must(e.sync())
	if e.a.store.CountRows(e.ctx, "raw") != before["raw"]+3 {
		t.Fatalf("a move was not one version: raw %d", e.a.store.CountRows(e.ctx, "raw"))
	}
	if m := e.message(gm.id); !reflect.DeepEqual(m.Labels, []string{"Archive", "Green category"}) {
		t.Fatalf("folder not followed: %+v", m.Labels)
	}
	// Back to unread and unflagged: again a new version, never "not inserted".
	e.box.set(gm.id, func(m *gmMsg) { m.read, m.flagged = false, false })
	e.must(e.sync())
	if e.a.store.CountRows(e.ctx, "raw") != before["raw"]+4 {
		t.Fatal("going back to an earlier state was not kept as a version")
	}
	if m := e.message(gm.id); m.Seen || m.Starred {
		t.Fatalf("message did not follow the change back: %+v", m)
	}
	// The same state again changes nothing.
	e.must(e.sync())
	if e.a.store.CountRows(e.ctx, "raw") != before["raw"]+4 || e.a.store.CountRows(e.ctx, "messages") != 1 {
		t.Fatal("an unchanged state wrote a row")
	}
	if len(e.box.mimeFetched()) != 1 {
		t.Fatal("a changed state downloaded the original again")
	}
}

// TestGraphOwnMessageNeverWaitsForClaude: a message the owner sent himself does
// not make the item wait for Claude and moves In progress to Pending; a
// message from someone else brings Pending, Resolved and Closed back to New.
func TestGraphOwnMessageNeverWaitsForClaude(t *testing.T) {
	e := newGraphEnv(t)
	for _, c := range []string{"mine", "stays", "p", "r", "c"} {
		e.box.add("F-inbox", e.in(c, "Matter "+c, 5))
	}
	e.must(e.importMonth())
	e.must(e.sync()) // takes the position every folder needs from here on

	e.setItem("mine", func(it *repo.Item) { it.Stage = stageInProgress; it.NeedsClaude = false })
	own := e.box.add("F-sent", e.out("mine", "Re: Matter mine", 1))
	e.setItem("stays", func(it *repo.Item) { it.Stage = stageResolved; it.NeedsClaude = false })
	e.box.add("F-sent", e.out("stays", "Re: Matter stays", 1))
	for c, st := range map[string]string{"p": stagePending, "r": stageResolved, "c": stageClosed} {
		e.setItem(c, func(it *repo.Item) { it.Stage = st; it.NeedsClaude = false })
		e.box.add("F-inbox", e.in(c, "Re: Matter "+c, 0.5))
	}
	e.must(e.sync())

	mine := e.item("mine")
	if mine.Stage != stagePending || mine.NeedsClaude || mine.PendingSince == 0 || !e.event(mine, "pending_set") {
		t.Fatalf("own reply on In progress: %+v", mine)
	}
	if e.event(mine, "reopened") {
		t.Fatal("own reply reopened the item")
	}
	if stays := e.item("stays"); stays.Stage != stageResolved || stays.NeedsClaude {
		t.Fatalf("own reply on Resolved: %+v", stays)
	}
	for _, c := range []string{"p", "r", "c"} {
		it := e.item(c)
		if it.Stage != stageNew || !it.NeedsClaude || !e.event(it, "reopened") {
			t.Fatalf("new mail from someone else on %s: %+v", c, it)
		}
	}
	if !e.message(own.id).SentByMe {
		t.Fatal("a message from the owner is not marked as his")
	}
}

// TestGraphThreadingByConversationAndNeverAcrossSources: one conversation is
// one item; a similar subject never joins two conversations, and a Gmail
// thread and a Office conversation with the same subject, or even the
// same id, stay two items.
func TestGraphThreadingByConversationAndNeverAcrossSources(t *testing.T) {
	e := newGraphEnv(t)
	e.box.add("F-inbox", e.in("100", "Invoice 2026-14", 4))
	e.box.add("F-sent", e.out("100", "Re: Invoice 2026-14", 3))
	e.box.add("F-proj", e.in("100", "Re: Re: Invoice 2026-14", 2))
	e.box.add("F-inbox", e.in("200", "Invoice 2026-14", 1))
	e.box.add("F-inbox", e.in("300", "Invoice 2026-14!", 1))
	e.must(e.importMonth())
	claudeLooksAtAll(t, e.a) // new items wait for Claude's look before they are listed (SPEC 11)

	// The same subject and the same thread id on the Gmail side.
	e.must(e.a.store.Update(e.ctx, func(tx *repo.Tx) error {
		_, err := tx.CreateItem(e.ctx, repo.Item{Kind: "thread", Subject: "Invoice 2026-14", Stage: stageNew, FirstAt: 1, LastAt: 1}, "gmail_thread", "100", "system", 1)
		return err
	}))
	e.box.add("F-inbox", e.in("100", "Re: Invoice 2026-14 again", 0.2))
	e.must(e.sync())
	claudeLooksAtAll(t, e.a)

	c := e.counts()
	if c["items"] != 4 || c["messages"] != 6 || c["item_links"] != 4 {
		t.Fatalf("counts %v", c)
	}
	one := e.item("100")
	gm, ok := e.a.store.ItemByLink(e.ctx, "gmail_thread", "100")
	if !ok || gm.ID == one.ID {
		t.Fatal("the Gmail thread and the Office conversation joined")
	}
	var n int
	for _, st := range []string{stageNew, stageInProgress, stagePending, stageResolved, stageClosed, stageHistory} {
		rows, err := e.a.store.ListItems(e.ctx, st, 20, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range rows {
			if r.ID == one.ID && r.Count != 4 {
				t.Fatalf("conversation 100 has %d messages, want 4", r.Count)
			}
			if r.ID == one.ID && r.Source != srcGraph {
				t.Fatalf("the row says source %q", r.Source)
			}
			n++
		}
	}
	if n != 4 {
		t.Fatalf("%d rows", n)
	}
	if got := sourceLabel(srcGraph); got != "Office mail" {
		t.Fatalf("source label %q", got)
	}
}

// TestGraphResumeAfterAnInterruptedImport: the connection drops in the middle
// of an import; the run records its error and its position (the link to the
// next page), and the next run continues without storing anything twice.
func TestGraphResumeAfterAnInterruptedImport(t *testing.T) {
	e := newGraphEnv(t)
	for i := 0; i < 12; i++ {
		e.box.add("F-inbox", e.in("t"+string(rune('a'+i)), "Matter "+string(rune('A'+i)), 12-float64(i)))
	}
	e.box.dropMimeAfter = 5
	if err := e.importMonth(); err == nil {
		t.Fatal("the dropped connection was not an error")
	}
	r := e.lastRun("month")
	if r.Status != "failed" || r.Error == "" || r.Cursor == "" || strings.Contains(r.Error, secretTok) || strings.Contains(r.Error, secretRef) {
		t.Fatalf("failed run: %+v", r)
	}
	c := e.counts()
	if c["raw"] != 5 || c["messages"] != 5 {
		t.Fatalf("after the stop: %v", c)
	}
	st := e.a.gimp.loadState(e.ctx)
	cur := st.Manual.Folders["F-inbox"]
	if st.Manual == nil || st.Manual.Done || cur == nil || cur.Next == "" || cur.Done {
		t.Fatalf("the position was not kept: %+v", st.Manual)
	}
	saved := cur.Next
	before := len(e.box.callList())
	e.box.dropMimeAfter = 0 // the connection is back

	e.must(e.importMonth())
	// The first call of the resumed run reads the saved page, not the start of the folder.
	if !strings.Contains(saved, "skiptoken") {
		t.Fatalf("saved position %q", saved)
	}
	var firstInbox string
	for i, c := range e.box.callList()[before:] {
		if strings.Contains(c, "F-inbox/messages/delta") {
			firstInbox = e.box.queryList()[before+i]
			break
		}
	}
	if !strings.Contains(firstInbox, "skiptoken") || strings.Contains(firstInbox, "filter") {
		t.Fatalf("the resumed run did not start at the saved page: %q", firstInbox)
	}
	r = e.lastRun("month")
	if r.Status != "done" {
		t.Fatalf("resumed run: %+v", r)
	}
	c = e.counts()
	if c["raw"] != 12 || c["messages"] != 12 || c["items"] != 12 || c["events"] != 12 || c["pool"] != 12 {
		t.Fatalf("after the resume: %v", c)
	}
	if got := len(e.box.mimeFetched()); got != 12 {
		t.Fatalf("%d originals were downloaded for 12 messages", got)
	}
	if st := e.a.gimp.loadState(e.ctx); !st.Manual.Done || st.Folders["F-inbox"].Delta == "" {
		t.Fatalf("the import did not finish with a position: %+v", st)
	}
}

// TestGraphSyncContinuesFromTheSavedDeltaLink: the timer's sync reads only
// what changed since the folder's saved delta link, and a link Graph no
// longer knows starts that folder again without storing anything twice.
func TestGraphSyncContinuesFromTheSavedDeltaLink(t *testing.T) {
	e := newGraphEnv(t)
	e.box.add("F-inbox", e.in("a", "Old one", 3))
	e.box.add("F-inbox", e.in("b", "Old two", 2))
	e.must(e.importMonth())
	e.box.add("F-inbox", e.in("c", "New one", 0.1))
	n := len(e.box.callList())
	e.must(e.sync())
	r := e.lastRun("sync")
	if r.Stored != 1 || r.Fetched != 1 {
		t.Fatalf("sync: %+v", r)
	}
	var inboxCalls int
	for _, c := range e.box.callList()[n:] {
		if strings.Contains(c, "F-inbox/messages/delta") {
			inboxCalls++
		}
	}
	if inboxCalls != 1 {
		t.Fatalf("%d delta calls for the inbox, want 1", inboxCalls)
	}
	// Graph forgets the position of the inbox.
	e.box.resync["F-inbox"] = true
	e.box.add("F-inbox", e.in("d", "Newer", 0.05))
	e.must(e.sync())
	if r := e.lastRun("sync"); r.Status != "done" || r.Stored != 1 {
		t.Fatalf("sync after a lost position: %+v", r)
	}
	if c := e.counts(); c["messages"] != 4 || c["raw"] != 4 {
		t.Fatalf("counts %v", c)
	}
}

// TestGraphRemovedMessageIsKept: a message that is deleted in the mailbox is
// kept in the app as one more raw version, never deleted; a message that was
// only moved is not recorded as removed; a removal repeated writes nothing.
func TestGraphRemovedMessageIsKept(t *testing.T) {
	e := newGraphEnv(t)
	gone := e.box.add("F-inbox", e.in("A", "To be deleted", 2))
	moved := e.box.add("F-inbox", e.in("B", "To be moved", 2))
	e.must(e.importMonth())
	before := e.counts()

	e.box.remove(gone.id)
	e.box.move(moved.id, "F-archive")
	e.must(e.sync())
	e.must(e.sync())
	after := e.counts()
	if after["messages"] != before["messages"] || after["items"] != before["items"] || after["pool"] != before["pool"] {
		t.Fatalf("a removal deleted something: %v -> %v", before, after)
	}
	if after["raw"] != before["raw"]+2 {
		t.Fatalf("raw %d -> %d: want one version for the removal and one for the move", before["raw"], after["raw"])
	}
	m := e.message(gone.id)
	raw, _ := e.a.store.LatestRaw(e.ctx, srcGraph, gone.id)
	if !strings.HasPrefix(raw.VersionKey, "removed@") || !strings.Contains(m.Flags, "removed") || m.Text == "" {
		t.Fatalf("removed message: raw %q flags %q", raw.VersionKey, m.Flags)
	}
	rawMoved, _ := e.a.store.LatestRaw(e.ctx, srcGraph, moved.id)
	if strings.HasPrefix(rawMoved.VersionKey, "removed@") || strings.Contains(e.message(moved.id).Flags, "removed") {
		t.Fatal("a moved message was recorded as removed")
	}
	// The removal reached the item's record only as a version: no event, no reopening.
	if e.event(e.item("A"), "reopened") {
		t.Fatal("a removal reopened the item")
	}
}

// TestGraphTokenRefreshDuringARun: the access token runs out in the middle of
// a run; it is renewed, the new refresh token is kept, the run goes on, and a
// token Graph refuses is renewed once.
func TestGraphTokenRefreshDuringARun(t *testing.T) {
	e := newGraphEnv(t)
	for i := 0; i < 6; i++ {
		e.box.add("F-inbox", e.in("t"+string(rune('a'+i)), "Matter", float64(6-i)))
	}
	// After the third original the access token has run out.
	e.box.onMime = func() {
		if len(e.box.mimeFetched()) == 3 {
			e.must(writeSecret(e.a.secretsDir, graphSecretFile, graphSecret{Address: graphSelf, AccessToken: secretTok, RefreshToken: secretRef, ExpiresAt: time.Now().Add(-time.Minute).Unix()}))
		}
	}
	calls := e.ms.tokenCalls
	e.must(e.importMonth())
	if r := e.lastRun("month"); r.Status != "done" || r.Stored != 6 {
		t.Fatalf("run: %+v", r)
	}
	if e.ms.tokenCalls != calls+1 {
		t.Fatalf("the token endpoint was called %d times, want one refresh", e.ms.tokenCalls-calls)
	}
	var sec graphSecret
	readSecret(e.a.secretsDir, graphSecretFile, &sec)
	if sec.AccessToken != secretTok+"-2" || sec.RefreshToken != secretRef || sec.ExpiresAt < time.Now().Unix()+3000 {
		t.Fatal("the renewed sign-in was not kept")
	}
	// Graph refuses a token that still looks good: renewed once, the call repeated.
	e.box.onMime = nil
	e.box.add("F-inbox", e.in("late", "Late", 0.1))
	e.box.badTokens = 1
	calls = e.ms.tokenCalls
	e.must(e.sync())
	if r := e.lastRun("sync"); r.Status != "done" || r.Stored != 1 || e.ms.tokenCalls != calls+1 {
		t.Fatalf("sync after a refused token: %+v, token calls %d", r, e.ms.tokenCalls-calls)
	}
}

// TestGraphExpiredSignInIsShownInPlainWords: a refresh token Microsoft no
// longer accepts pauses the source, says so on the Connectors page without
// a token or a code, keeps the position, and a new sign-in resumes it.
func TestGraphExpiredSignInIsShownInPlainWords(t *testing.T) {
	e := newGraphEnv(t)
	for i := 0; i < 5; i++ {
		e.box.add("F-inbox", e.in("t"+string(rune('a'+i)), "Matter", float64(5-i)))
	}
	e.must(e.importMonth())
	e.must(e.sync())
	e.box.add("F-inbox", e.in("new", "New", 0.1))

	var told [][2]string
	e.a.notify = func(title, body, link string) error { told = append(told, [2]string{title, body}); return nil }
	e.must(writeSecret(e.a.secretsDir, graphSecretFile, graphSecret{Address: graphSelf, AccessToken: secretTok, RefreshToken: secretRef, ExpiresAt: time.Now().Add(-time.Minute).Unix()}))
	e.ms.refreshErr = "invalid_grant"
	err := e.sync()
	if err == nil || !strings.Contains(err.Error(), "sign-in needed") {
		t.Fatalf("sync with an expired sign-in: %v", err)
	}
	// SPEC 24: the strip, and one notice for the owner's phone, naming the source.
	if s := alertsFor(e.a.sourceStates(e.ctx)); len(s) != 1 || s[0] != "Office mail needs login. See Connectors." {
		t.Errorf("strip %v", s)
	}
	if len(told) != 1 || told[0][0] != "Office mail needs login" {
		t.Errorf("notices %v", told)
	}
	r := e.lastRun("sync")
	if r.Status != "failed" || !strings.Contains(r.Error, "Press Sign in") || strings.Contains(r.Error, secretRef) {
		t.Fatalf("run: %+v", r)
	}
	page := get(e.h, "/connectors").Body.String()
	for _, want := range []string{"Needs login", "The Microsoft sign-in has run out. Press Sign in on this page"} {
		if !strings.Contains(page, want) {
			t.Errorf("the page lacks %q", want)
		}
	}
	for _, bad := range []string{secretTok, secretRef, "AADSTS"} {
		if strings.Contains(page, bad) {
			t.Errorf("the page shows %q", bad)
		}
	}
	e.sync() // a second failure of the same kind sends no second notice
	if len(told) != 1 {
		t.Errorf("notices %v", told)
	}
	// The timer pauses: no call is made while the sign-in is missing.
	tc := e.ms.tokenCalls
	if err := e.a.gimp.Tick(e.ctx); err == nil || e.ms.tokenCalls != tc {
		t.Fatalf("the timer tried again (%v, %d calls)", err, e.ms.tokenCalls-tc)
	}
	// After the sign-in fetching resumes by itself, where it stopped.
	e.ms.refreshErr = ""
	e.signIn()
	e.a.setGraphNeedsLogin(e.ctx, false) // what finishing the sign-in does
	e.must(e.a.gimp.Tick(e.ctx))         // the month import has finished, so this starts the first import
	if r := e.lastRun("first"); r.Status != "done" || r.Stored != 1 || r.Fetched != 6 {
		t.Fatalf("after the sign-in: %+v", r)
	}
	page = get(e.h, "/connectors").Body.String()
	if i := strings.Index(page, `id="fetches"`); i > 0 {
		page = page[:i] // the rows say where the sign-in stands; the history below keeps what happened
	}
	if strings.Contains(page, "has run out") {
		t.Fatal("the page still says the sign-in has run out")
	}
}

// TestGraphRulesApply: a matching rule writes a skipped row and nothing
// else, the original is never downloaded, the first enabled rule wins, a rule
// on a header reads the headers, and Gmail's rules do not touch this mailbox.
func TestGraphRulesApply(t *testing.T) {
	e := newGraphEnv(t)
	gh := e.in("gh1", "Run failed: pipeline - main (abc1234)", 1)
	gh.from = "GitHub <notifications@github.com>"
	gh.extra = []string{"X-GitHub-Reason: ci_activity"}
	e.box.add("F-inbox", gh)
	pr := e.in("gh2", "[repo] Pull request opened", 1)
	pr.from = "GitHub <notifications@github.com>"
	e.box.add("F-inbox", pr)
	news := e.in("n1", "Weekly news", 1)
	cat := e.box.add("F-inbox", news)
	e.box.set(cat.id, func(m *gmMsg) { m.cats = []string{"Newsletters"} })
	e.box.add("F-inbox", e.in("keep", "Keep me", 1))
	e.must(e.a.store.Update(e.ctx, func(tx *repo.Tx) error {
		for _, r := range []repo.Rule{
			{Name: "pipeline (outlook)", Enabled: true, Source: "outlook", Field: "header:x-github-reason", Op: "equals", Value: "ci_activity", CreatedBy: "test"},
			{Name: "all GitHub", Enabled: true, Source: "outlook", Field: "sender_domain", Op: "equals", Value: "github.com", CreatedBy: "test"},
			{Name: "newsletters", Enabled: true, Source: "outlook", Field: "label", Op: "equals", Value: "Newsletters", CreatedBy: "test"},
			{Name: "off", Enabled: false, Source: "outlook", Field: "subject", Op: "contains", Value: "Keep", CreatedBy: "test"},
			{Name: "gmail only", Enabled: true, Source: "gmail", Field: "subject", Op: "contains", Value: "Keep", CreatedBy: "test"},
		} {
			if err := tx.EnsureRule(e.ctx, r, 1); err != nil {
				return err
			}
		}
		return nil
	}))
	e.must(e.importMonth())

	c := e.counts()
	if c["skipped"] != 3 || c["raw"] != 1 || c["messages"] != 1 || c["items"] != 1 {
		t.Fatalf("counts %v", c)
	}
	if len(e.box.mimeFetched()) != 1 {
		t.Fatalf("a dropped message was downloaded: %v", e.box.mimeFetched())
	}
	first, _ := e.a.store.RuleByName(e.ctx, "GitHub pipeline mails") // the starting rule now covers this mailbox too, and comes first
	sk, ok := e.a.store.SkippedBySource(e.ctx, srcGraph, e.box.msgs[0].id)
	if !ok || sk.RuleID != first.ID || sk.Sender != "notifications@github.com" || !strings.HasPrefix(sk.Subject, "Run failed") {
		t.Fatalf("skipped row: %+v", sk)
	}
	if r := e.lastRun("month"); r.Skipped != 3 {
		t.Fatalf("run: %+v", r)
	}
	e.must(e.importMonth())
	if e.counts()["skipped"] != 3 {
		t.Fatal("a repeat wrote another skipped row")
	}
}

// TestGraphDraftsDeletedAndJunkAreLeftOut: nothing in Drafts, Deleted Items,
// Junk or their subfolders is read; the other folders, subfolders included, are.
func TestGraphDraftsDeletedAndJunkAreLeftOut(t *testing.T) {
	e := newGraphEnv(t)
	e.box.add("F-drafts", e.out("d1", "Unsent", 1))
	e.box.add("F-deleted", e.in("x1", "Deleted", 1))
	e.box.add("F-trashsub", e.in("x2", "Deleted deeper", 1))
	e.box.add("F-junk", e.in("j1", "Junk", 1))
	e.box.add("F-inbox", e.in("ok1", "Inbox", 1))
	e.box.add("F-proj", e.in("ok2", "Subfolder", 1))
	e.box.add("F-archive", e.in("ok3", "Archive", 1))
	e.box.add("F-sent", e.out("ok4", "Sent", 1))
	flagged := e.in("dr", "Marked as a draft elsewhere", 1)
	dm := e.box.add("F-inbox", flagged)
	e.box.set(dm.id, func(m *gmMsg) { m.draft = true })
	e.must(e.importMonth())
	if c := e.counts(); c["raw"] != 4 || c["skipped"] != 0 {
		t.Fatalf("counts %v", c)
	}
	for _, id := range e.box.mimeFetched() {
		for _, m := range e.box.msgs {
			if m.id == id && (m.folder == "F-drafts" || m.folder == "F-deleted" || m.folder == "F-junk" || m.folder == "F-trashsub" || m.draft) {
				t.Fatalf("an original from %s was downloaded", m.folder)
			}
		}
	}
	// A mailbox without a Junk folder is fine.
	e2 := newGraphEnv(t)
	e2.box.folders = e2.box.folders[:0]
	e2.box.addFolder("F-inbox", "Inbox", "inbox", "")
	e2.box.add("F-inbox", e2.in("a", "Only inbox", 1))
	e2.must(e2.importMonth())
	if e2.counts()["raw"] != 1 {
		t.Fatal("a mailbox without Drafts, Deleted Items and Junk was not read")
	}
}

type graphCounting struct{ n atomic.Int64 }

func (c *graphCounting) RoundTrip(*http.Request) (*http.Response, error) {
	c.n.Add(1)
	return nil, io.ErrClosedPipe
}

// TestGraphContentIsTextAndNothingIsFetched: an HTML-only mail is stored as
// text, a script tag in a subject shows as text on the page, an attachment
// and an inline picture are stored once by hash, and importing and showing a
// mail with remote image addresses makes no outbound request.
func TestGraphContentIsTextAndNothingIsFetched(t *testing.T) {
	e := newGraphEnv(t)
	var hits atomic.Int64
	pixel := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1) }))
	defer pixel.Close()

	h := e.in("html", `<script>alert(1)</script> Offer`, 1)
	h.text = ""
	h.html = `<html><head><style>p{color:red}</style><title>t</title></head><body><script>evil()</script><p>Hello <b>world</b></p>` +
		`<ul><li>one</li><li>two</li></ul><img src="` + pixel.URL + `/p.gif" alt="logo"><a href="` + pixel.URL + `/track">link</a></body></html>`
	hm := e.box.add("F-inbox", h)

	logo := []byte("PNGDATA-the-logo-bytes-0123456789")
	m1 := e.in("a1", "With files", 2)
	m1.attach = []attachment{
		{name: "terms.pdf", ctype: "application/pdf", content: []byte("%PDF terms body")},
		{name: "logo.png", ctype: "image/png", content: logo, inline: true, cid: "logo@x"},
	}
	m1.html, m1.text = `<img src="cid:logo@x"><img src="`+pixel.URL+`/remote.png">`, ""
	e.box.add("F-inbox", m1)
	m2 := e.in("a2", "Same logo", 1)
	m2.attach = []attachment{{name: "logo.png", ctype: "image/png", content: logo, inline: true, cid: "logo@x"}}
	e.box.add("F-inbox", m2)

	// Count what leaves the process on any transport but the fake Microsoft's.
	rt := &graphCounting{}
	old := http.DefaultTransport
	http.DefaultTransport = rt
	defer func() { http.DefaultTransport = old }()

	e.must(e.importMonth())
	claudeLooksAtAll(t, e.a)

	got := e.message(hm.id)
	for _, want := range []string{"Hello world", "- one", "- two", "[image: logo]"} {
		if !strings.Contains(got.Text, want) {
			t.Errorf("text lacks %q:\n%s", want, got.Text)
		}
	}
	for _, bad := range []string{"<", "evil", "color:red", "track"} {
		if strings.Contains(got.Text, bad) {
			t.Errorf("text contains %q:\n%s", bad, got.Text)
		}
	}
	page := get(e.h, "/list/new").Body.String()
	if !strings.Contains(page, "&lt;script&gt;alert(1)&lt;/script&gt; Offer") || strings.Contains(page, "<script>alert(1)") {
		t.Fatalf("the subject is not shown as text:\n%s", page)
	}

	var rows []repo.File
	for _, x := range e.box.msgs {
		fs, err := e.a.store.FilesOfMessage(e.ctx, e.message(x.id).ID)
		if err != nil {
			t.Fatal(err)
		}
		rows = append(rows, fs...)
	}
	if len(rows) != 3 {
		t.Fatalf("%d file rows, want 3", len(rows))
	}
	byPath := map[string]int{}
	for _, f := range rows {
		if f.Status != "saved" || f.SHA == "" || f.Path != relPath(f.SHA) || f.Source != srcGraph {
			t.Fatalf("file row not saved: %+v", f)
		}
		byPath[f.Path]++
	}
	if len(byPath) != 2 {
		t.Fatalf("distinct files %d, want 2 (the logo is stored once): %v", len(byPath), byPath)
	}
	for _, f := range rows {
		if f.Filename == "logo.png" {
			if f.Disposition != "inline" || f.ContentID != "logo@x" || f.ContentType != "image/png" {
				t.Fatalf("inline picture row: %+v", f)
			}
			b, err := os.ReadFile(filepath.Join(e.a.dataDir, "files", filepath.FromSlash(f.Path)))
			if err != nil || string(b) != string(logo) {
				t.Fatalf("stored picture differs: %v", err)
			}
		}
	}
	if got := e.poolFiles(); got != 3+2 {
		t.Fatalf("pool holds %d files, want 5", got)
	}
	if hits.Load() != 0 || rt.n.Load() != 0 {
		t.Fatalf("outbound requests were made: server %d, transport %d", hits.Load(), rt.n.Load())
	}
}

// TestGraphMemoryALargeMessageIsStreamed: a message with a 30 MB attachment is
// downloaded, read and saved through small buffers, never held whole.
func TestGraphMemoryALargeMessageIsStreamed(t *testing.T) {
	e := newGraphEnv(t)
	const lines = 540_000 // 57 bytes each: about 30.8 MB of attachment, 41 MB on the wire
	m := e.in("big", "Big file", 1)
	m.raw = "From: Jana Example <jana@example.test>\r\nTo: " + graphSelf + "\r\nSubject: Big file\r\nDate: " + e.ago(1).Format(time.RFC1123Z) +
		"\r\nMIME-Version: 1.0\r\nContent-Type: multipart/mixed; boundary=\"BND\"\r\n\r\n"
	gm := e.box.addGenerated("F-inbox", m, func(w io.Writer) {
		io.WriteString(w, "--BND\r\nContent-Type: text/plain; charset=utf-8\r\n\r\nThe big one.\r\n--BND\r\n"+
			"Content-Type: application/octet-stream; name=\"big.bin\"\r\nContent-Disposition: attachment; filename=\"big.bin\"\r\nContent-Transfer-Encoding: base64\r\n\r\n")
		raw := make([]byte, 57)
		enc := make([]byte, 76+2)
		x := uint64(88172645463325252)
		for i := 0; i < lines; i++ {
			for j := range raw {
				x ^= x << 13
				x ^= x >> 7
				x ^= x << 17
				raw[j] = byte(x)
			}
			base64Encode(enc[:76], raw)
			enc[76], enc[77] = '\r', '\n'
			w.Write(enc)
		}
		io.WriteString(w, "--BND--\r\n")
	})
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	e.must(e.importMonth())
	runtime.ReadMemStats(&after)

	alloc := after.TotalAlloc - before.TotalAlloc
	st := e.a.imp.pool.stats
	st.mu.Lock()
	maxWrite, written := st.maxWrite, st.written
	st.mu.Unlock()
	t.Logf("allocated %d MB while storing %d MB; largest single write %d bytes", alloc>>20, written>>20, maxWrite)
	if written < 65<<20 {
		t.Fatalf("only %d bytes reached the pool; the message was not stored in full", written)
	}
	if maxWrite > 64<<10 {
		t.Fatalf("the pool was written in a chunk of %d bytes", maxWrite)
	}
	if alloc > 24<<20 {
		t.Fatalf("storing the message allocated %d MB, which says it was held in memory", alloc>>20)
	}
	mm := e.message(gm.id)
	fs, _ := e.a.store.FilesOfMessage(e.ctx, mm.ID)
	if len(fs) != 1 || fs[0].Status != "saved" || fs[0].Size != int64(lines)*57 || mm.Text != "The big one." {
		t.Fatalf("message %q files %+v", mm.Text, fs)
	}
}

// TestGraphSlowDownIsObeyedAndFollowedLinksStayOnGraph: a 429 with
// Retry-After is waited out and repeated; a link to another address is never
// followed with the token.
func TestGraphSlowDownIsObeyedAndFollowedLinksStayOnGraph(t *testing.T) {
	e := newGraphEnv(t)
	for i := 0; i < 5; i++ {
		e.box.add("F-inbox", e.in("t"+string(rune('a'+i)), "Matter", float64(5-i)))
	}
	e.box.throttle = 2
	e.must(e.importMonth())
	if e.box.statuses[429] != 2 || e.counts()["raw"] != 5 {
		t.Fatalf("429s %d, raw %d", e.box.statuses[429], e.counts()["raw"])
	}

	e2 := newGraphEnv(t)
	for i := 0; i < 5; i++ {
		e2.box.add("F-inbox", e2.in("t"+string(rune('a'+i)), "Matter", float64(5-i)))
	}
	e2.box.wrongLink = true
	err := e2.importMonth()
	if err == nil || !strings.Contains(err.Error(), "another address") {
		t.Fatalf("a link to another address: %v", err)
	}
	if r := e2.lastRun("month"); r.Status != "failed" || r.Error == "" || strings.Contains(r.Error, "elsewhere") {
		t.Fatalf("run %+v", r)
	}
}

// TestGraphTimerStartsTheFirstImportOnlyAfterTheMonthImport: as for Gmail,
// the timer only syncs until "Import one month" has finished once; then it
// starts the long first import by itself; after that it only syncs.
func TestGraphTimerStartsTheFirstImportOnlyAfterTheMonthImport(t *testing.T) {
	e := newGraphEnv(t)
	for i := 0; i < 4; i++ {
		e.box.add("F-inbox", e.in("t"+string(rune('a'+i)), "Matter", float64(i+1)*20+20))
	}
	e.box.add("F-inbox", e.in("recent", "Recent", 2))
	e.must(e.a.gimp.Tick(e.ctx))
	if e.counts()["raw"] != 0 {
		t.Fatal("the timer imported before the owner ran the one-month import")
	}
	if r, ok := e.a.store.LatestRun(e.ctx, srcGraph, "sync"); !ok || r.Status != "done" {
		t.Fatal("the timer did not sync")
	}
	e.must(e.importMonth())
	if e.counts()["raw"] != 1 {
		t.Fatalf("the month import stored %d messages", e.counts()["raw"])
	}
	e.must(e.a.gimp.Tick(e.ctx))
	if r := e.lastRun("first"); r.Status != "done" || r.Stored != 4 {
		t.Fatalf("first import: %+v", r)
	}
	if e.counts()["raw"] != 5 {
		t.Fatalf("raw %d", e.counts()["raw"])
	}
	n := e.a.store.CountRows(e.ctx, "sync_runs")
	e.must(e.a.gimp.Tick(e.ctx))
	if e.a.store.CountRows(e.ctx, "sync_runs") != n+1 {
		t.Fatal("the timer did not do one plain sync")
	}
	if r, _ := e.a.store.LatestRun(e.ctx, srcGraph, "sync"); r.Mode != "sync" {
		t.Fatal("not a sync")
	}
}

// TestGraphFirstImportYieldsToTheTimer: while a long import runs, the regular
// sync still happens every interval.
func TestGraphFirstImportYieldsToTheTimer(t *testing.T) {
	e := newGraphEnv(t)
	e.a.gimp.holdFirstImport = false
	e.a.gimp.pageSize = 2
	for i := 0; i < 8; i++ {
		e.box.add("F-inbox", e.in("t"+string(rune('a'+i)), "Matter", float64(8-i)))
	}
	// A month import first gives the folder its position, so a sync has something to follow.
	e.must(e.importMonth())
	e.box.onMime = func() { e.skew.Add(180) }
	for i := 0; i < 8; i++ {
		e.box.add("F-archive", e.in("o"+string(rune('a'+i)), "Old matter", float64(40+i)))
	}
	e.must(e.a.gimp.Tick(e.ctx))
	imp := e.lastRun("first")
	if imp.Status != "done" || imp.Stored != 8 {
		t.Fatalf("import: %+v", imp)
	}
	sync, ok := e.a.store.LatestRun(e.ctx, srcGraph, "sync")
	if !ok || sync.ID <= imp.ID || sync.Status != "done" {
		t.Fatalf("the import did not yield to the timer: %+v", sync)
	}
}

// TestGraphBothSourcesAreSyncedOneAfterTheOther: the timer's round syncs Gmail
// and then the Office mailbox, each into its own records.
func TestGraphBothSourcesAreSyncedOneAfterTheOther(t *testing.T) {
	e := newEnv(t)
	ms := useFakeMS(t, e.a)
	ms.box.standard()
	ge := &genv{t: t, a: e.a, h: e.h, ms: ms, box: ms.box, ctx: e.ctx, base: e.base}
	e.must(e.a.store.SetSetting(e.ctx, keyGraphTenant, testTenant))
	e.must(e.a.store.SetSetting(e.ctx, keyGraphClient, testClient))
	ge.signIn()
	e.box.add(e.in("G1", "Gmail matter", 2))
	ms.box.add("F-inbox", ge.in("G1", "Gmail matter", 2)) // the same thread id and subject in the other mailbox
	e.must(e.importMonth())
	ge.must(ge.importMonth())
	e.box.add(e.in("G2", "Later in Gmail", 0.1))
	ms.box.add("F-inbox", ge.in("R2", "Later in Office", 0.1))
	e.must(e.a.imp.Tick(e.ctx))
	ge.must(e.a.gimp.Tick(e.ctx))
	if e.a.store.CountMessages(e.ctx, srcGmail) != 2 || e.a.store.CountMessages(e.ctx, srcGraph) != 2 {
		t.Fatal("each source should hold two messages")
	}
	if e.a.store.CountRows(e.ctx, "items") != 4 {
		t.Fatalf("items %d: the two mailboxes joined", e.a.store.CountRows(e.ctx, "items"))
	}
}

// TestGraphConnectorsPageShowsTheImport: the buttons, the state of the last
// import, the time of the last sync, the totals and the known limitation,
// with text from mail inserted as text only.
func TestGraphConnectorsPageShowsTheImport(t *testing.T) {
	e := newGraphEnv(t)
	m := e.in("x", "Plain", 1)
	m.from = `"<b>Eve</b>" <eve@example.test>`
	e.box.add("F-inbox", m)
	e.box.add("F-inbox", e.in("y", "Another", 2))
	e.must(e.importMonth())
	e.must(e.sync())

	rec := get(e.h, "/connectors")
	page := rec.Body.String()
	i := strings.Index(page, `id="graph"`)
	section := page[i:]
	if j := strings.Index(section, `id="agent"`); j > 0 {
		section = section[:j]
	}
	for _, want := range []string{"Import one week", "Import one month", "Import of one month (", "finished. It read 2 messages and stored 2 as new (2 items made)", "Last sync by the timer: 19 Nov 2026",
		"2 messages in 2 items stored."} {
		if !strings.Contains(section, want) {
			t.Errorf("the Office mail section lacks %q", want)
		}
	}
	// The limitations are one section of the page, each sentence once (SPEC 23).
	for _, want := range []string{"Known limitations", "Mail deleted before the first import cannot be imported."} {
		if strings.Count(page, want) != 1 {
			t.Errorf("the page has %q %d times, want once", want, strings.Count(page, want))
		}
	}
	if strings.Contains(section, "<b>Eve") {
		t.Error("text from a mail is not inserted as text")
	}

	if rec := post(e.h, "/connectors/graph/import", map[string][]string{"depth": {"week"}}); rec.Code != http.StatusSeeOther || !strings.Contains(rec.Header().Get("Location"), "graphimp") {
		t.Fatalf("import button: %d %s", rec.Code, rec.Header().Get("Location"))
	}
	if rec := post(e.h, "/connectors/graph/import", map[string][]string{"depth": {"month"}}); !strings.Contains(rec.Header().Get("Location"), "graphbusy") {
		t.Fatalf("a second press: %s", rec.Header().Get("Location"))
	}
	req := <-e.a.gimp.kick
	if req.Mode != "week" || req.Depth != 7*24*time.Hour {
		t.Fatalf("request %+v", req)
	}
	if rec := post(e.h, "/connectors/graph/import", map[string][]string{"depth": {"year"}}); rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown depth: %d", rec.Code)
	}

	// A failed import says so in plain words and shows no token.
	e.box.add("F-inbox", e.in("z1", "More", 1))
	e.box.add("F-inbox", e.in("z2", "More", 1))
	e.box.dropMimeAfter = 1
	e.a.gimp.RunImport(e.ctx, "week", 7*24*time.Hour)
	page = get(e.h, "/connectors").Body.String()
	if !strings.Contains(page, "Import of one week (") || !strings.Contains(page, "Microsoft Graph could not be reached") || strings.Contains(page, secretTok) || strings.Contains(page, secretRef) {
		t.Fatalf("failed import page:\n%s", page)
	}

	// Not signed in: no import button.
	removeSecret(e.a.secretsDir, graphSecretFile)
	if rec := post(e.h, "/connectors/graph/import", map[string][]string{"depth": {"week"}}); rec.Code != http.StatusBadRequest {
		t.Fatalf("import without a sign-in: %d", rec.Code)
	}
}
