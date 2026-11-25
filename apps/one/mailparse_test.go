package main

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseMailDecodesCharsetsAndEncodedHeaders(t *testing.T) {
	msg := "From: =?ISO-8859-2?Q?Jan_=A9ev=E8=EDk?= <Jan@Example.TEST>\r\n" +
		"To: The owner <owner@example.test>, other@example.test\r\n" +
		"Subject: =?UTF-8?B?xI1lc2vDvSDDunZvZA==?= and =?ISO-8859-2?Q?p=F8=EDli=B9?=\r\n" +
		"Date: Thu, 19 Nov 2026 10:00:00 +0100\r\n" +
		"MIME-Version: 1.0\r\nContent-Type: text/plain; charset=iso-8859-2\r\nContent-Transfer-Encoding: quoted-printable\r\n\r\n" +
		"P=F8=EDli=B9 =BElu=BBou=E8k=FD k=F9=F2\r\n"
	pm, err := parseMail(strings.NewReader(msg), countingSink{})
	if err != nil {
		t.Fatal(err)
	}
	if pm.Subject != "český úvod and příliš" {
		t.Errorf("subject %q", pm.Subject)
	}
	if pm.FromAddr != "jan@example.test" || pm.FromName != "Jan Ševčík" {
		t.Errorf("from %q <%s>", pm.FromName, pm.FromAddr)
	}
	if len(pm.To) != 2 || pm.To[0] != "owner@example.test" {
		t.Errorf("to %v", pm.To)
	}
	if strings.TrimSpace(pm.Text) != "Příliš žluťoučký kůň" {
		t.Errorf("text %q", pm.Text)
	}
	if pm.Date.IsZero() {
		t.Error("date not read")
	}
}

func TestParseMailPrefersPlainTextAndKeepsPartNumbers(t *testing.T) {
	msg := "From: a@b.test\r\nSubject: s\r\nMIME-Version: 1.0\r\nContent-Type: multipart/mixed; boundary=OUT\r\n\r\n" +
		"--OUT\r\nContent-Type: multipart/alternative; boundary=ALT\r\n\r\n" +
		"--ALT\r\nContent-Type: text/plain\r\n\r\nplain wins\r\n" +
		"--ALT\r\nContent-Type: multipart/related; boundary=REL\r\n\r\n" +
		"--REL\r\nContent-Type: text/html\r\n\r\n<p>html loses</p>\r\n" +
		"--REL\r\nContent-Type: image/png\r\nContent-ID: <pic1>\r\nContent-Transfer-Encoding: base64\r\n\r\naGVsbG8=\r\n--REL--\r\n" +
		"--ALT--\r\n" +
		"--OUT\r\nContent-Type: application/pdf; name=\"=?UTF-8?Q?z=C3=A1pis.pdf?=\"\r\nContent-Disposition: attachment\r\nContent-Transfer-Encoding: base64\r\n\r\nJVBERg==\r\n--OUT--\r\n"
	pm, err := parseMail(strings.NewReader(msg), countingSink{})
	if err != nil {
		t.Fatal(err)
	}
	if pm.Text != "plain wins" {
		t.Errorf("text %q", pm.Text)
	}
	if len(pm.Parts) != 2 {
		t.Fatalf("parts %+v", pm.Parts)
	}
	p := pm.Parts[0]
	if p.Part != "1.1.2.2" || pm.Parts[1].Part != "1.2" {
		t.Errorf("part paths %q and %q", p.Part, pm.Parts[1].Part)
	}
	if p.Disposition != "inline" || p.ContentID != "pic1" || p.ContentType != "image/png" || p.Size != 5 {
		t.Errorf("inline picture %+v", p)
	}
	if q := pm.Parts[1]; q.Filename != "zápis.pdf" || q.Disposition != "attachment" || q.Size != 4 {
		t.Errorf("attachment %+v", q)
	}
}

func TestParseMailSurvivesDamage(t *testing.T) {
	// A broken base64 attachment and a part cut short: what can be read is kept.
	msg := "From: a@b.test\r\nSubject: s\r\nContent-Type: multipart/mixed; boundary=B\r\n\r\n" +
		"--B\r\nContent-Type: text/plain\r\n\r\nbody\r\n--B\r\nContent-Type: application/octet-stream\r\nContent-Transfer-Encoding: base64\r\n\r\n!!!not base64!!!\r\n"
	pm, err := parseMail(strings.NewReader(msg), countingSink{})
	if err != nil || pm.Text != "body" || len(pm.Parts) != 1 {
		t.Fatalf("%+v %v", pm, err)
	}
}

func TestHTMLToText(t *testing.T) {
	got := htmlToText(`<html><head><title>x</title><style>a{}</style></head><body><h1>Title</h1><p>One&nbsp;&amp; two</p>` +
		`<script>var a = "<b>";</script><table><tr><td>a</td><td>b</td></tr></table><br>line<pre>  keep
  this</pre><img src="http://tracker.example/p.gif"><img src="x" alt="Logo"></body></html>`)
	want := []string{"Title", "One & two", "a b", "line", "keep\nthis", "[image: Logo]"}
	for _, w := range want {
		if !strings.Contains(got, w) {
			t.Errorf("lacks %q in %q", w, got)
		}
	}
	for _, bad := range []string{"var a", "tracker", "a{}", "<"} {
		if strings.Contains(got, bad) {
			t.Errorf("contains %q in %q", bad, got)
		}
	}
}

func TestParseMetaReadsGmailsFetchLine(t *testing.T) {
	line := `5 FETCH (UID 41 X-GM-MSGID 1700000000000000001 X-GM-THRID 1700000000000000000 X-GM-LABELS (\Inbox Work "My Label" "&AWE-t") FLAGS (\Seen \Flagged) ` +
		`INTERNALDATE " 9-Nov-2026 08:05:06 +0100" RFC822.SIZE 2048 MODSEQ (777) BODY[HEADER.FIELDS (FROM SUBJECT)] "` +
		"From: a@b.test\r\nSubject: hi\r\n  there\r\n\r\n" + `")`
	m, ok := parseMeta(line)
	if !ok {
		t.Fatal("not parsed")
	}
	if m.UID != 41 || m.MsgID != "1700000000000000001" || m.ThreadID != "1700000000000000000" || m.Size != 2048 || m.ModSeq != 777 {
		t.Errorf("%+v", m)
	}
	if strings.Join(m.Labels, "|") != `\Inbox|Work|My Label|št` || !m.hasFlag(`\Seen`) || !m.hasFlag(`\flagged`) {
		t.Errorf("labels %v flags %v", m.Labels, m.Flags)
	}
	if m.Date.UTC() != time.Date(2026, 11, 9, 7, 5, 6, 0, time.UTC) {
		t.Errorf("date %v", m.Date)
	}
	if m.Header["subject"] != "hi there" {
		t.Errorf("header %v", m.Header)
	}
	if _, ok := parseMeta("5 FETCH (FLAGS (\\Seen))"); ok {
		t.Error("a line without a UID and Gmail id was accepted")
	}
}

func TestRuleMatching(t *testing.T) {
	f := ruleFacts{Sender: "notifications@github.com", Subject: "Run failed: ci", Labels: []string{`\Inbox`, "Work"}, Headers: map[string]string{"x-github-reason": "ci_activity"}}
	cases := []struct {
		field, op, value string
		want             bool
	}{
		{"sender", "equals", "Notifications@GitHub.com", true},
		{"sender_domain", "equals", "github.com", true},
		{"sender_domain", "ends_with", "hub.com", true},
		{"subject", "contains", "failed", true},
		{"subject", "starts_with", "run", true},
		{"label", "equals", "work", true},
		{"folder", "equals", "Spam", false},
		{"header:x-github-reason", "equals", "ci_activity", true},
		{"header:x-github-reason", "equals", "mention", false},
		{"header:x-missing", "equals", "", false},
		{"subject", "contains", "", false},
		{"unknown", "equals", "x", false},
	}
	for _, c := range cases {
		r := repoRule(c.field, c.op, c.value)
		if got := ruleMatches(r, f); got != c.want {
			t.Errorf("%s %s %q: %v", c.field, c.op, c.value, got)
		}
	}
}

// TestPendingFilesAreFinishedByTheNextRun: when saving an attachment fails
// (here the disk place for it is blocked), the message stays stored with its
// file pending and the run says what happened; the next run finishes the
// file from the stored original without asking Gmail again.
func TestPendingFilesAreFinishedByTheNextRun(t *testing.T) {
	e := newEnv(t)
	content := []byte("ATTACHMENT-CONTENT-THAT-IS-SAVED-LATER")
	sum := sha256.Sum256(content)
	sha := hex.EncodeToString(sum[:])
	m := e.in("f", "With a file", 1)
	m.attach = []attachment{{name: "note.txt", ctype: "text/plain", content: content}}
	e.box.add(m)
	blocker := filepath.Join(e.a.dataDir, "files", sha[:2])
	e.must(os.MkdirAll(filepath.Dir(blocker), 0o700))
	e.must(os.WriteFile(blocker, []byte("in the way"), 0o600))

	capturedLog(e.a)
	// One message whose file cannot be saved no longer fails the run: the
	// message is stored, the file stays pending, and the message waits in the
	// files ledger to be tried again.
	e.must(e.importMonth())
	if r := e.lastRun("month"); r.Status != "done" || r.Stored != 1 {
		t.Fatalf("run: %+v", r)
	}
	if e.a.failedCount(e.ctx, srcGmail+"-files") != 1 {
		t.Fatal("the message with the unsaved file is not waiting for a new try")
	}
	if c := e.counts(); c["raw"] != 1 || c["messages"] != 1 || c["files"] != 1 {
		t.Fatalf("counts %v", c)
	}
	if n := e.a.store.PendingFileCount(e.ctx); n != 1 {
		t.Fatalf("%d files pending, want 1", n)
	}
	e.must(os.Remove(blocker))
	bodies := len(e.box.fetchedBodies())
	e.must(e.sync())
	if n := e.a.store.PendingFileCount(e.ctx); n != 0 {
		t.Fatalf("%d files still pending", n)
	}
	if len(e.box.fetchedBodies()) != bodies {
		t.Fatal("the message was downloaded again")
	}
	mm := e.message(e.box.msgs[0].msgid)
	fs, _ := e.a.store.FilesOfMessage(e.ctx, mm.ID)
	if len(fs) != 1 || fs[0].Status != "saved" || fs[0].SHA != sha {
		t.Fatalf("files %+v", fs)
	}
	b, _ := os.ReadFile(filepath.Join(e.a.dataDir, "files", filepath.FromSlash(fs[0].Path)))
	if string(b) != string(content) {
		t.Fatal("saved content differs")
	}
}

// TestSenderNameInQuotesOrOnTwoLinesIsDecoded: a display name sent as an
// encoded word inside quotes, or folded over two lines, is stored readable.
func TestSenderNameInQuotesOrOnTwoLinesIsDecoded(t *testing.T) {
	for in, want := range map[string]string{
		`"=?UTF-8?Q?Anglick=C3=BD=20Tr=C3=A1vn=C3=ADk?=" <shop@example.test>`:                   "Anglický Trávník",
		`=?UTF-8?Q?Anglick=C3=BD=20Tr=C3=A1vn=C3=ADk?= <shop@example.test>`:                     "Anglický Trávník",
		"=?utf-8?Q?OBCHOD=20=28predt=C3=BDm=20In=C3=BD=29?=\r\n <shop@example.test>":            "OBCHOD (predtým Iný)",
		`"=?utf-8?Q?OBCHOD=20=28predt=C3=BDm?= =?utf-8?Q?=20In=C3=BD=29?=" <shop@example.test>`: "OBCHOD (predtým Iný)",
		`Plain Name <shop@example.test>`:                                                        "Plain Name",
	} {
		name, addr := parseAddress(in)
		if name != want || addr != "shop@example.test" {
			t.Errorf("%q: got %q %q, want %q", in, name, addr, want)
		}
	}
}

// TestStoredTextHoldsNoMarkup: an HTML-only mail, a mail with Word's XML in
// it, a plain part that holds a page, and a page escaped twice all become text.
func TestStoredTextHoldsNoMarkup(t *testing.T) {
	head := "From: a@example.test\r\nTo: p@example.test\r\nSubject: s\r\nMIME-Version: 1.0\r\n"
	cases := map[string]string{
		"html only": head + "Content-Type: text/html; charset=utf-8\r\n\r\n<html><body><p>Hello <b>there</b>,</p><div>second line</div></body></html>\r\n",
		"word xml": head + "Content-Type: text/html; charset=utf-8\r\n\r\n<html><body><xml><w:WordDocument><w:View>Normal</w:View></w:WordDocument></xml>" +
			"<p class=MsoNormal>Hello there,<o:p></o:p></p><div>second line</div></body></html>\r\n",
		"page in the plain part": head + "Content-Type: text/plain; charset=utf-8\r\n\r\n<!DOCTYPE html><html><body><p>Hello there,</p><div>second line</div></body></html>\r\n",
		"no content type":        head + "\r\n<div>Hello there,</div><div>second line</div>\r\n",
		"escaped twice":          head + "Content-Type: text/html; charset=utf-8\r\n\r\n&lt;p&gt;Hello there,&lt;/p&gt;&lt;div&gt;second line&lt;/div&gt;\r\n",
	}
	for name, raw := range cases {
		pm, err := parseMail(strings.NewReader(raw), countingSink{})
		if err != nil {
			t.Fatal(name, err)
		}
		if strings.ContainsAny(pm.Text, "<>") || strings.Contains(pm.Text, "Normal") || !strings.Contains(pm.Text, "Hello") || !strings.Contains(pm.Text, "second line") {
			t.Errorf("%s: %q", name, pm.Text)
		}
	}
	// A plain mail that only mentions a tag is left as it is.
	pm, _ := parseMail(strings.NewReader(head+"Content-Type: text/plain\r\n\r\nUse the <div> tag, then reply to <a@example.test>.\r\n"), countingSink{})
	if !strings.Contains(pm.Text, "<div> tag") || !strings.Contains(pm.Text, "<a@example.test>") {
		t.Errorf("plain text changed: %q", pm.Text)
	}
}
