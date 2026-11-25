package main

import (
	"context"
	"crypto/tls"
	"io"
	"io/fs"
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

// env is one app wired to a fake Gmail, with a clock the test controls.
type env struct {
	t    *testing.T
	a    *app
	h    http.Handler
	fake *fakeIMAP
	box  *fakeBox
	ctx  context.Context
	base time.Time
	skew atomic.Int64 // seconds the clock has been moved on
}

func newEnv(t *testing.T) *env {
	t.Helper()
	a, h := newTestApp(t)
	e := &env{t: t, a: a, h: h, ctx: context.Background(), base: time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)}
	a.now = func() time.Time { return e.base.Add(time.Duration(e.skew.Load()) * time.Second) }
	cert, pool := testCert(t, "imap.gmail.com")
	e.fake = startFakeIMAP(t, cert, testAddr, testPass)
	e.box = newFakeBox()
	e.fake.box = e.box
	a.gmail.IMAPAddr = e.fake.addr
	a.gmail.TLS = &tls.Config{ServerName: "imap.gmail.com", RootCAs: pool, MinVersion: tls.VersionTLS12}
	a.gmail.Timeout = 10 * time.Second
	if err := writeSecret(a.secretsDir, gmailSecretFile, gmailSecret{Address: testAddr, Password: testPass}); err != nil {
		t.Fatal(err)
	}
	a.imp.pageSize = 3
	a.imp.seedRules(e.ctx)
	return e
}

func (e *env) ago(days float64) time.Time {
	return e.a.now().Add(-time.Duration(days * 24 * float64(time.Hour)))
}

// in is a mail from someone else received days ago.
func (e *env) in(thrid, subject string, days float64) testMail {
	return testMail{thrid: thrid, from: "Alice Example <alice@example.test>", to: testAddr, subject: subject, date: e.ago(days),
		labels: []string{`\Inbox`}, flags: nil, text: "Hello, this is " + subject + "."}
}

// out is a mail the owner sent himself.
func (e *env) out(thrid, subject string, days float64) testMail {
	m := e.in(thrid, subject, days)
	m.from, m.to = testAddr, "alice@example.test"
	m.labels = []string{`\Sent`}
	m.flags = []string{`\Seen`}
	return m
}

func (e *env) importMonth() error {
	return e.a.imp.RunImport(e.ctx, "month", 30*24*time.Hour)
}

func (e *env) importYear() error {
	return e.a.imp.RunImport(e.ctx, "first", 365*24*time.Hour)
}

func (e *env) sync() error { return e.a.imp.RunSync(e.ctx) }

func (e *env) must(err error) {
	e.t.Helper()
	if err != nil {
		e.t.Fatal(err)
	}
}

var countedTables = []string{"raw", "messages", "items", "events", "files", "skipped", "people", "item_links"}

func (e *env) counts() map[string]int {
	out := map[string]int{"pool": e.poolFiles()}
	for _, t := range countedTables {
		out[t] = e.a.store.CountRows(e.ctx, t)
	}
	return out
}

func (e *env) poolFiles() int {
	n := 0
	root := filepath.Join(e.a.dataDir, "files")
	filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		rel, _ := filepath.Rel(root, p)
		if err == nil && !d.IsDir() && !strings.HasPrefix(rel, "tmp"+string(filepath.Separator)) {
			n++
		}
		return nil
	})
	return n
}

func (e *env) item(thrid string) repo.Item {
	e.t.Helper()
	it, ok := e.a.store.ItemByLink(e.ctx, "gmail_thread", thrid)
	if !ok {
		e.t.Fatalf("no item for thread %s", thrid)
	}
	return it
}

// setItem plays what the owner or Claude would do to an item.
func (e *env) setItem(thrid string, fn func(*repo.Item)) {
	it := e.item(thrid)
	fn(&it)
	e.must(e.a.store.Update(e.ctx, func(tx *repo.Tx) error { return tx.SaveItem(e.ctx, it) }))
}

func (e *env) message(msgid string) repo.Message {
	e.t.Helper()
	m, ok := e.a.store.MessageBySource(e.ctx, srcGmail, msgid)
	if !ok {
		e.t.Fatalf("no message %s", msgid)
	}
	return m
}

func (e *env) lastRun(modes ...string) repo.Run {
	e.t.Helper()
	r, ok := e.a.store.LatestRun(e.ctx, srcGmail, modes...)
	if !ok {
		e.t.Fatalf("no run of %v", modes)
	}
	return r
}

func (e *env) lastRunPtr() *repo.Run {
	r := e.lastRun("month")
	return &r
}

func (e *env) event(it repo.Item, typ string) bool {
	for _, t := range e.a.store.EventTypes(e.ctx, it.ID) {
		if t == typ {
			return true
		}
	}
	return false
}

// TestRepeatWritesNothingTheSecondTime: the same unchanged mailbox imported
// again, and synced again, writes no raw row, no message, no event and no
// file, and the second import does not even download a body.
func TestRepeatWritesNothingTheSecondTime(t *testing.T) {
	e := newEnv(t)
	logo := []byte("\x89PNG-not-really-a-picture-but-bytes")
	m1 := e.in("A", "First of A", 3)
	m1.attach = []attachment{{name: "terms.pdf", ctype: "application/pdf", content: []byte("%PDF-1.4 terms")}, {name: "logo.png", ctype: "image/png", content: logo, inline: true, cid: "logo1"}}
	e.box.add(m1)
	e.box.add(e.in("A", "Re: First of A", 2))
	e.box.add(e.in("B", "Second matter", 1))
	e.must(e.importMonth())
	first := e.counts()
	if first["raw"] != 3 || first["messages"] != 3 || first["items"] != 2 || first["files"] != 2 || first["pool"] != 5 {
		t.Fatalf("first import wrote %v", first)
	}
	bodies := len(e.box.fetchedBodies())

	e.must(e.importMonth())
	e.must(e.sync())
	e.must(e.sync())
	if again := e.counts(); !reflect.DeepEqual(first, again) {
		t.Fatalf("a repeat changed what is stored:\nfirst %v\nagain %v", first, again)
	}
	if got := len(e.box.fetchedBodies()); got != bodies {
		t.Fatalf("a repeat downloaded %d more bodies", got-bodies)
	}
	if r := e.lastRun("month"); r.Stored != 0 || r.Skipped != 0 || r.Status != "done" || r.Fetched != 3 {
		t.Fatalf("second import: %+v", r)
	}
	// Only reading, never changing: no flag set, nothing moved, bodies peeked.
	for _, c := range e.fake.commands() {
		up := strings.ToUpper(c)
		for _, bad := range []string{"STORE", "APPEND", "COPY", "MOVE", "EXPUNGE", "SELECT", "DELETE", "CREATE"} {
			if strings.HasPrefix(up, bad+" ") || strings.HasPrefix(up, "UID "+bad+" ") {
				t.Fatalf("the import sent %q", c)
			}
		}
		if strings.Contains(up, "BODY[") && !strings.Contains(up, "BODY.PEEK[") {
			t.Fatalf("a fetch that would mark mail as read: %q", c)
		}
	}
}

// TestVersionsChangedFlagOrLabelIsOneNewRawVersion: a changed read state or
// label gives one new raw version, the same one message, and the item does
// not wait for Claude again. Going back to an earlier state is a new version too.
func TestVersionsChangedFlagOrLabelIsOneNewRawVersion(t *testing.T) {
	e := newEnv(t)
	fm := e.box.add(e.in("A", "Offer", 2))
	e.must(e.importMonth())
	e.setItem("A", func(it *repo.Item) { it.NeedsClaude = false; it.Stage = stageInProgress })
	before := e.counts()
	it := e.item("A")
	evBefore := len(e.a.store.EventTypes(e.ctx, it.ID))

	e.box.change(fm.msgid, []string{`\Seen`, `\Flagged`}, []string{`\Inbox`, "Work"})
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
	m := e.message(fm.msgid)
	if !m.Seen || !m.Starred || !reflect.DeepEqual(m.Labels, []string{`\Inbox`, "Work"}) {
		t.Fatalf("message not updated: %+v", m)
	}
	got := e.item("A")
	if got.NeedsClaude || got.Stage != stageInProgress || len(e.a.store.EventTypes(e.ctx, got.ID)) != evBefore {
		t.Fatalf("the item was touched: %+v", got)
	}

	// Back to the first state: again a new version, never "not inserted".
	e.box.change(fm.msgid, nil, []string{`\Inbox`})
	e.must(e.sync())
	if e.a.store.CountRows(e.ctx, "raw") != before["raw"]+2 {
		t.Fatal("going back to an earlier state was not kept as a version")
	}
	if m := e.message(fm.msgid); m.Seen || m.Starred {
		t.Fatalf("message did not follow the change back: %+v", m)
	}
	// The same state again changes nothing.
	e.must(e.sync())
	if e.a.store.CountRows(e.ctx, "raw") != before["raw"]+2 {
		t.Fatal("an unchanged state wrote a row")
	}
}

// TestOwnMessageNeverWaitsForClaude: a message the owner sent himself does not
// make the item wait for Claude and moves In progress to Pending; a message
// from someone else brings Pending, Resolved and Closed back to New, waiting.
func TestOwnMessageNeverWaitsForClaude(t *testing.T) {
	e := newEnv(t)
	for _, th := range []string{"mine", "stays", "p", "r", "c"} {
		e.box.add(e.in(th, "Matter "+th, 5))
	}
	e.must(e.importMonth())

	// The owner's own reply on an item In progress.
	e.setItem("mine", func(it *repo.Item) { it.Stage = stageInProgress; it.NeedsClaude = false })
	e.box.add(e.out("mine", "Re: Matter mine", 1))
	// His own reply on a Resolved item: nothing moves.
	e.setItem("stays", func(it *repo.Item) { it.Stage = stageResolved; it.NeedsClaude = false })
	e.box.add(e.out("stays", "Re: Matter stays", 1))
	// Someone else writes on Pending, Resolved and Closed items.
	for th, st := range map[string]string{"p": stagePending, "r": stageResolved, "c": stageClosed} {
		e.setItem(th, func(it *repo.Item) { it.Stage = st; it.NeedsClaude = false })
		e.box.add(e.in(th, "Re: Matter "+th, 0.5))
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
	for _, th := range []string{"p", "r", "c"} {
		it := e.item(th)
		if it.Stage != stageNew || !it.NeedsClaude || !e.event(it, "reopened") {
			t.Fatalf("new mail from someone else on %s: %+v", th, it)
		}
	}
	var sent int
	for _, th := range []string{"mine", "stays"} {
		for _, x := range e.box.msgs {
			if x.thrid == th && strings.HasPrefix(x.header, "From: "+testAddr) {
				if !e.message(x.msgid).SentByMe {
					t.Fatalf("a message from the owner is not marked as his")
				}
				sent++
			}
		}
	}
	if sent != 2 {
		t.Fatalf("found %d own messages", sent)
	}
}

// TestThreadingFollowsGmailThreadsOnly: one Gmail thread is one item; a
// similar subject never joins two threads.
func TestThreadingFollowsGmailThreadsOnly(t *testing.T) {
	e := newEnv(t)
	e.box.add(e.in("100", "Invoice 2026-14", 4))
	e.box.add(e.in("100", "Re: Invoice 2026-14", 3))
	e.box.add(e.in("100", "Re: Re: Invoice 2026-14", 2))
	e.box.add(e.in("200", "Invoice 2026-14", 1))
	e.box.add(e.in("300", "Invoice 2026-14!", 1))
	e.must(e.importMonth())
	claudeLooksAtAll(t, e.a) // new items wait for Claude's look before they are listed (SPEC 11)
	c := e.counts()
	if c["items"] != 3 || c["messages"] != 5 || c["item_links"] != 3 {
		t.Fatalf("counts %v", c)
	}
	var rows []repo.ListRow
	for _, st := range []string{stageNew, stageInProgress, stagePending, stageResolved, stageClosed, stageHistory} {
		got, err := e.a.store.ListItems(e.ctx, st, 10, 0)
		if err != nil {
			t.Fatal(err)
		}
		rows = append(rows, got...)
	}
	sizes := map[int]int{}
	for _, r := range rows {
		sizes[r.Count]++
	}
	if sizes[3] != 1 || sizes[1] != 2 {
		t.Fatalf("thread sizes %v", sizes)
	}
	for _, r := range rows {
		if r.Number < 1 || repo.Number(r.Number) == "" {
			t.Fatal("bad item number")
		}
	}
}

// TestResumeAfterAnInterruptedImport: the connection drops in the middle of
// an import; the run records its error and its position, and the next run
// continues without storing anything twice.
func TestResumeAfterAnInterruptedImport(t *testing.T) {
	e := newEnv(t)
	for i := 0; i < 12; i++ {
		e.box.add(e.in("t"+string(rune('a'+i)), "Matter "+string(rune('A'+i)), 12-float64(i)))
	}
	e.box.failAfter = 5
	if err := e.importMonth(); err == nil {
		t.Fatal("the dropped connection was not an error")
	}
	r := e.lastRun("month")
	if r.Status != "failed" || r.Error == "" || r.Cursor == "" || strings.Contains(r.Error, testPass) {
		t.Fatalf("failed run: %+v", r)
	}
	c := e.counts()
	if c["raw"] != 5 || c["messages"] != 5 {
		t.Fatalf("after the stop: %v", c)
	}
	st := e.a.imp.loadState(e.ctx)
	if st.Manual == nil || st.Manual.Done || st.Manual.Low == 0 {
		t.Fatalf("the position was not kept: %+v", st.Manual)
	}

	e.must(e.importMonth())
	r = e.lastRun("month")
	if r.Status != "done" || r.Stored != 7 || r.Fetched != 9 {
		t.Fatalf("resumed run: %+v", r)
	}
	c = e.counts()
	if c["raw"] != 12 || c["messages"] != 12 || c["items"] != 12 || c["events"] != 12 || c["pool"] != 12 {
		t.Fatalf("after the resume: %v", c)
	}
	if st := e.a.imp.loadState(e.ctx); !st.Manual.Done {
		t.Fatal("the import did not finish")
	}
}

// TestHistoryItemsOlderThanTheReviewWindowDoNotWaitForClaude.
func TestHistoryItemsOlderThanTheReviewWindowDoNotWaitForClaude(t *testing.T) {
	e := newEnv(t)
	e.box.add(e.in("old", "Old matter", 60))
	e.box.add(e.in("mix", "Mixed matter", 90))
	e.box.add(e.out("ownold", "Own recent", 5))
	e.box.add(e.in("mix", "Re: Mixed matter", 2))
	e.box.add(e.in("recent", "Recent matter", 3))
	e.must(e.importYear())

	old := e.item("old")
	if old.Stage != stageHistory || old.NeedsClaude {
		t.Fatalf("old: %+v", old)
	}
	if it := e.item("mix"); it.Stage != stageNew || !it.NeedsClaude {
		t.Fatalf("mix (newest message is recent): %+v", it)
	}
	if it := e.item("recent"); it.Stage != stageNew || !it.NeedsClaude {
		t.Fatalf("recent: %+v", it)
	}
	if it := e.item("ownold"); it.Stage != stageHistory || it.NeedsClaude {
		t.Fatalf("an item the owner started himself waits for Claude: %+v", it)
	}
	// A new message on a history item brings it to New.
	e.box.add(e.in("old", "Re: Old matter", 0.1))
	e.must(e.sync())
	if it := e.item("old"); it.Stage != stageNew || !it.NeedsClaude || !e.event(it, "reopened") {
		t.Fatalf("history item with new mail: %+v", it)
	}
}

// TestRulesDropAtImportAndReimport: a matching rule writes a skipped row and
// nothing else, the body is never downloaded, the first enabled rule wins,
// and importing the message again brings it in.
func TestRulesDropAtImportAndReimport(t *testing.T) {
	e := newEnv(t)
	ci := e.in("gh1", "Run failed: pipeline - main (abc1234)", 1)
	ci.from = "GitHub <notifications@github.com>"
	ci.extra = []string{"X-GitHub-Reason: ci_activity"}
	ciMsg := e.box.add(ci)
	pr := e.in("gh2", "[repo] Pull request opened", 1)
	pr.from = "GitHub <notifications@github.com>"
	pr.extra = []string{"X-GitHub-Reason: mention"}
	e.box.add(pr)
	// A later rule that would also match, and a disabled one, never win.
	e.must(e.a.store.Update(e.ctx, func(tx *repo.Tx) error {
		if err := tx.EnsureRule(e.ctx, repo.Rule{Name: "all GitHub", Enabled: true, Source: "gmail", Field: "sender_domain", Op: "equals", Value: "github.com", CreatedBy: "test"}, 1); err != nil {
			return err
		}
		return tx.EnsureRule(e.ctx, repo.Rule{Name: "off", Enabled: false, Source: "gmail", Field: "subject", Op: "contains", Value: "Pull request", CreatedBy: "test"}, 1)
	}))
	e.must(e.importMonth())

	c := e.counts()
	if c["skipped"] != 2 || c["raw"] != 0 || c["messages"] != 0 || c["items"] != 0 || c["events"] != 0 || c["files"] != 0 || c["people"] != 0 || c["pool"] != 0 {
		t.Fatalf("rules wrote more than skipped rows: %v", c)
	}
	if len(e.box.fetchedBodies()) != 0 {
		t.Fatal("a dropped message was downloaded")
	}
	if len(e.box.peeked()) != 2 {
		t.Fatal("a dropped message should only have its beginning read")
	}
	if r := e.lastRun("month"); r.Skipped != 2 {
		t.Fatalf("run: %+v", r)
	}
	sk, ok := e.a.store.SkippedBySource(e.ctx, srcGmail, ciMsg.msgid)
	if !ok {
		t.Fatal("no skipped row for the pipeline mail")
	}
	first, _ := e.a.store.RuleByName(e.ctx, "GitHub pipeline mails")
	if sk.RuleID != first.ID || sk.Sender != "notifications@github.com" || !strings.HasPrefix(sk.Subject, "Run failed") {
		t.Fatalf("skipped row: %+v (rule %d)", sk, first.ID)
	}
	// Running it again writes no second line.
	e.must(e.importMonth())
	if e.counts()["skipped"] != 2 {
		t.Fatal("a repeat wrote another skipped row")
	}

	e.must(e.a.imp.Reimport(e.ctx, sk.ID))
	c = e.counts()
	if c["raw"] != 1 || c["messages"] != 1 || c["items"] != 1 || c["pool"] != 1 {
		t.Fatalf("after the re-import: %v", c)
	}
	if got, _ := e.a.store.SkippedByID(e.ctx, sk.ID); !got.Reimported {
		t.Fatal("the skipped row does not say it was brought back")
	}
	if e.item("gh1").Stage != stageNew {
		t.Fatal("the brought-back mail is not an item waiting")
	}
}

// TestDraftsAreLeftOut: drafts, by flag or by label, are never imported.
func TestDraftsAreLeftOut(t *testing.T) {
	e := newEnv(t)
	d1 := e.out("d1", "Unsent", 1)
	d1.flags = []string{`\Draft`}
	d2 := e.out("d2", "Unsent too", 1)
	d2.labels = []string{`\Draft`}
	e.box.add(d1)
	e.box.add(d2)
	e.box.add(e.in("ok", "Real", 1))
	e.must(e.importMonth())
	if c := e.counts(); c["raw"] != 1 || c["skipped"] != 0 {
		t.Fatalf("counts %v", c)
	}
}

type countingTransport struct{ n atomic.Int64 }

func (c *countingTransport) RoundTrip(*http.Request) (*http.Response, error) {
	c.n.Add(1)
	return nil, io.ErrClosedPipe
}

// TestContentIsTextAndNothingIsFetched: an HTML-only mail is stored as text,
// a script tag in a subject shows as text on the page, an attachment and an
// inline picture are stored once by hash, and importing and showing a mail
// with remote image addresses makes no outbound request.
func TestContentIsTextAndNothingIsFetched(t *testing.T) {
	e := newEnv(t)
	var hits atomic.Int64
	pixel := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1) }))
	defer pixel.Close()
	rt := &countingTransport{}
	old := http.DefaultTransport
	http.DefaultTransport = rt
	defer func() { http.DefaultTransport = old }()

	h := e.in("html", `<script>alert(1)</script> Offer`, 1)
	h.text = ""
	h.html = `<html><head><style>p{color:red}</style><title>t</title></head><body><script>evil()</script><p>Hello <b>world</b></p>` +
		`<ul><li>one</li><li>two</li></ul><img src="` + pixel.URL + `/p.gif" alt="logo"><a href="` + pixel.URL + `/track">link</a></body></html>`
	e.box.add(h)

	logo := []byte("PNGDATA-the-logo-bytes-0123456789")
	m1 := e.in("a1", "With files", 2)
	m1.attach = []attachment{
		{name: "terms.pdf", ctype: "application/pdf", content: []byte("%PDF terms body")},
		{name: "logo.png", ctype: "image/png", content: logo, inline: true, cid: "logo@x"},
	}
	m1.html, m1.text = `<img src="cid:logo@x"><img src="`+pixel.URL+`/remote.png">`, ""
	e.box.add(m1)
	m2 := e.in("a2", "Same logo", 1)
	m2.attach = []attachment{{name: "logo.png", ctype: "image/png", content: logo, inline: true, cid: "logo@x"}}
	e.box.add(m2)
	e.must(e.importMonth())
	claudeLooksAtAll(t, e.a)

	hm, _ := e.a.store.MessageBySource(e.ctx, srcGmail, e.box.msgs[0].msgid)
	for _, want := range []string{"Hello world", "- one", "- two", "[image: logo]"} {
		if !strings.Contains(hm.Text, want) {
			t.Errorf("text lacks %q:\n%s", want, hm.Text)
		}
	}
	for _, bad := range []string{"<", "evil", "color:red", "track"} {
		if strings.Contains(hm.Text, bad) {
			t.Errorf("text contains %q:\n%s", bad, hm.Text)
		}
	}

	page := get(e.h, "/list/new").Body.String()
	if !strings.Contains(page, "&lt;script&gt;alert(1)&lt;/script&gt; Offer") || strings.Contains(page, "<script>alert(1)") {
		t.Fatalf("the subject is not shown as text:\n%s", page)
	}

	// Files: three rows, two distinct pool files for them, plus the three originals.
	var rows []repo.File
	for _, x := range e.box.msgs {
		m := e.message(x.msgid)
		fs, err := e.a.store.FilesOfMessage(e.ctx, m.ID)
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
		if f.Status != "saved" || f.SHA == "" || f.Path != relPath(f.SHA) {
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

// TestMemoryALargeAttachmentIsStreamed: a message with a 30 MB attachment is
// downloaded, read and saved through small buffers, never held whole.
func TestMemoryALargeAttachmentIsStreamed(t *testing.T) {
	e := newEnv(t)
	const lines = 540_000 // 57 bytes each: about 30.8 MB of attachment, 41 MB on the wire
	m := e.in("big", "Big file", 1)
	m.raw = "From: Alice Example <alice@example.test>\r\nTo: " + testAddr + "\r\nSubject: Big file\r\nDate: " + e.ago(1).Format(time.RFC1123Z) +
		"\r\nMIME-Version: 1.0\r\nContent-Type: multipart/mixed; boundary=\"BND\"\r\n\r\n"
	e.box.addGenerated(m, func(w io.Writer) {
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
	mm := e.message(e.box.msgs[0].msgid)
	fs, _ := e.a.store.FilesOfMessage(e.ctx, mm.ID)
	if len(fs) != 1 || fs[0].Status != "saved" || fs[0].Size != int64(lines)*57 || mm.Text != "The big one." {
		t.Fatalf("message %q files %+v", mm.Text, fs)
	}
	st2, err := os.Stat(filepath.Join(e.a.dataDir, "files", filepath.FromSlash(fs[0].Path)))
	if err != nil || st2.Size() != fs[0].Size {
		t.Fatalf("the saved file: %v", err)
	}
}

// TestTimerStartsTheFirstImportOnlyAfterTheMonthImport and syncs otherwise.
func TestTimerStartsTheFirstImportOnlyAfterTheMonthImport(t *testing.T) {
	e := newEnv(t)
	for i := 0; i < 4; i++ {
		e.box.add(e.in("t"+string(rune('a'+i)), "Matter", float64(i+1)*20+20))
	}
	e.box.add(e.in("recent", "Recent", 2))
	// Held: the timer only syncs, so nothing old arrives by itself.
	e.must(e.a.imp.Tick(e.ctx))
	if e.counts()["raw"] != 0 {
		t.Fatal("the timer imported before the owner ran the one-month import")
	}
	if _, ok := e.a.store.LatestRun(e.ctx, srcGmail, "sync"); !ok {
		t.Fatal("the timer did not sync")
	}
	e.must(e.importMonth())
	if e.counts()["raw"] != 1 {
		t.Fatalf("the month import stored %d messages", e.counts()["raw"])
	}
	// Now the first import starts by itself and reaches back a year.
	e.must(e.a.imp.Tick(e.ctx))
	if r := e.lastRun("first"); r.Status != "done" || r.Stored != 4 {
		t.Fatalf("first import: %+v", r)
	}
	if e.counts()["raw"] != 5 {
		t.Fatalf("raw %d", e.counts()["raw"])
	}
	// After that the timer only syncs.
	n := e.a.store.CountRows(e.ctx, "sync_runs")
	e.must(e.a.imp.Tick(e.ctx))
	if e.a.store.CountRows(e.ctx, "sync_runs") != n+1 || e.lastRun("first").ID != e.lastRun("first").ID {
		t.Fatal("the timer did not do one plain sync")
	}
	if r, _ := e.a.store.LatestRun(e.ctx, srcGmail, "sync"); r.Mode != "sync" {
		t.Fatal("not a sync")
	}
}

// TestFirstImportYieldsToTheTimer: while a long import runs, the regular
// sync still happens every interval.
func TestFirstImportYieldsToTheTimer(t *testing.T) {
	e := newEnv(t)
	e.a.imp.holdFirstImport = false
	e.a.imp.pageSize = 2
	for i := 0; i < 8; i++ {
		e.box.add(e.in("t"+string(rune('a'+i)), "Matter", float64(8-i)))
	}
	// Every body fetch moves the clock three minutes on; the interval is five.
	e.fake.box = e.box
	e.box.onBody = func() { e.skew.Add(180) }
	e.must(e.a.imp.Tick(e.ctx))
	imp := e.lastRun("first")
	if imp.Status != "done" || imp.Stored != 8 {
		t.Fatalf("import: %+v", imp)
	}
	sync, ok := e.a.store.LatestRun(e.ctx, srcGmail, "sync")
	if !ok || sync.ID <= imp.ID || sync.Status != "done" {
		t.Fatalf("the import did not yield to the timer: %+v", sync)
	}
}

// TestConnectorsPageShowsTheImport: the buttons, the state of the last
// import, the time of the last sync and the totals, with text from mail
// inserted as text only. The list of items has its own page.
func TestConnectorsPageShowsTheImport(t *testing.T) {
	e := newEnv(t)
	m := e.in("x", "Plain", 1)
	m.from = `"<b>Eve</b>" <eve@example.test>`
	e.box.add(m)
	e.box.add(e.in("y", "Another", 2))
	e.must(e.importMonth())
	e.must(e.sync())

	rec := get(e.h, "/connectors")
	page := rec.Body.String()
	for _, want := range []string{"Import one week", "Import one month", "Import of one month (", "finished. It read 2 messages and stored 2 as new (2 items made)", "Last sync by the timer: 19 Nov 2026",
		"2 messages in 2 items stored."} {
		if !strings.Contains(page, want) {
			t.Errorf("page lacks %q", want)
		}
	}
	if strings.Contains(page, "Latest items") {
		t.Fatal("the temporary table of items is still on the page")
	}
	if !strings.Contains(rec.Header().Get("Content-Security-Policy"), "script-src 'self'") || rec.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatal("security headers missing")
	}

	// The buttons start an import; a second press while one is queued is told so.
	if rec := post(e.h, "/connectors/gmail/import", map[string][]string{"depth": {"week"}}); rec.Code != http.StatusSeeOther || !strings.Contains(rec.Header().Get("Location"), "gmailimp") {
		t.Fatalf("import button: %d %s", rec.Code, rec.Header().Get("Location"))
	}
	if rec := post(e.h, "/connectors/gmail/import", map[string][]string{"depth": {"month"}}); !strings.Contains(rec.Header().Get("Location"), "gmailbusy") {
		t.Fatalf("a second press: %s", rec.Header().Get("Location"))
	}
	req := <-e.a.imp.kick
	if req.Mode != "week" || req.Depth != 7*24*time.Hour {
		t.Fatalf("request %+v", req)
	}
	if rec := post(e.h, "/connectors/gmail/import", map[string][]string{"depth": {"year"}}); rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown depth: %d", rec.Code)
	}

	// A failed import says so in plain words and shows no password.
	e.fake.box.failAfter = 1
	e.box.add(e.in("z1", "More", 1))
	e.box.add(e.in("z2", "More", 1))
	e.a.imp.RunImport(e.ctx, "week", 7*24*time.Hour)
	page = get(e.h, "/connectors").Body.String()
	if !strings.Contains(page, "Import of one week (") || !strings.Contains(page, "Gmail could not be reached") || strings.Contains(page, testPass) {
		t.Fatalf("failed import page:\n%s", page)
	}
}

// TestPipelineRuleDropsAMatchingMailFromBothSources: the starting rule
// "GitHub pipeline mails" drops an automatic run mail from Gmail and from the
// Office mailbox (Graph), and nothing else from either.
func TestPipelineRuleDropsAMatchingMailFromBothSources(t *testing.T) {
	if startingRules[0].Source != "any" {
		t.Fatalf("the starting rule covers %q", startingRules[0].Source)
	}
	// Gmail.
	g := newEnv(t)
	ci := g.in("gh1", "Run failed: pipeline - main (abc1234)", 1)
	ci.from = "GitHub <notifications@github.com>"
	ci.extra = []string{"X-GitHub-Reason: ci_activity"}
	g.box.add(ci)
	other := g.in("gh2", "[repo] Pull request opened", 1)
	other.from = "GitHub <notifications@github.com>"
	other.extra = []string{"X-GitHub-Reason: mention"}
	g.box.add(other)
	g.must(g.importMonth())
	if c := g.counts(); c["skipped"] != 1 || c["items"] != 1 {
		t.Fatalf("gmail counts %v", c)
	}
	// The Office mailbox.
	o := newGraphEnv(t)
	oc := o.in("gh1", "Run failed: pipeline - main (abc1234)", 1)
	oc.from = "GitHub <notifications@github.com>"
	oc.extra = []string{"X-GitHub-Reason: ci_activity"}
	om := o.box.add("F-inbox", oc)
	op := o.in("gh2", "[repo] Pull request opened", 1)
	op.from = "GitHub <notifications@github.com>"
	op.extra = []string{"X-GitHub-Reason: mention"}
	o.box.add("F-inbox", op)
	o.must(o.importMonth())
	if c := o.counts(); c["skipped"] != 1 || c["items"] != 1 || c["messages"] != 1 {
		t.Fatalf("outlook counts %v", c)
	}
	rule, _ := o.a.store.RuleByName(o.ctx, "GitHub pipeline mails")
	sk, ok := o.a.store.SkippedBySource(o.ctx, srcGraph, om.id)
	if !ok || sk.RuleID != rule.ID {
		t.Fatalf("the Office mail was not dropped by the starting rule: %+v", sk)
	}
	for _, id := range o.box.mimeFetched() {
		if id == om.id {
			t.Fatal("a dropped mail was downloaded")
		}
	}
}

// A second mail with the same time as the one already reviewed brings the item
// back to Claude, but last_message_at does not move: the review job must not
// say "done" for the new mail.
func TestAMailWithTheSameSecondAsTheReviewedOneIsReviewedAgain(t *testing.T) {
	e := newEnv(t)
	e.box.add(e.in("same", "Invoice", 2))
	e.must(e.importMonth())
	it := e.item("same")
	e.must(e.a.store.Update(e.ctx, func(tx *repo.Tx) error {
		if _, err := tx.ClaimReview(e.ctx, it, e.a.now().Unix()); err != nil {
			return err
		}
		if err := tx.EndReview(e.ctx, it.ID, "done", 0, 0, "", e.a.now().Unix()); err != nil {
			return err
		}
		if _, err := tx.AnswerReview(e.ctx, it.ID, e.a.now().Unix()); err != nil {
			return err
		}
		return nil
	}))
	e.setItem("same", func(it *repo.Item) { it.Stage = stageResolved })
	e.box.add(e.in("same", "Re: Invoice", 2)) // the same second
	e.must(e.sync())
	cur := e.item("same")
	if !cur.NeedsClaude {
		t.Fatalf("the new mail did not wait for Claude: %+v", cur)
	}
	if got, found := e.a.store.NextReviewItem(e.ctx, e.a.now().Unix()); !found || got.ID != cur.ID {
		j, _ := e.a.store.ReviewJobOf(e.ctx, cur.ID)
		t.Errorf("an item that waits for Claude is never handed over: %+v", j)
	}
}
