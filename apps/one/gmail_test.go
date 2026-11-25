package main

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSecretFileIsPrivateAndAtomic(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "secrets")
	if err := writeSecret(dir, gmailSecretFile, gmailSecret{Address: testAddr, Password: testPass}); err != nil {
		t.Fatal(err)
	}
	if err := writeSecret(dir, gmailSecretFile, gmailSecret{Address: testAddr, Password: testPass + "2"}); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(dir, gmailSecretFile))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("secret file mode %v, want 0600", info.Mode().Perm())
	}
	if _, err := os.Stat(filepath.Join(dir, gmailSecretFile+".tmp")); !os.IsNotExist(err) {
		t.Fatal("the temporary file was left behind")
	}
	if d, _ := os.Stat(dir); d.Mode().Perm() != 0o700 {
		t.Fatalf("secrets folder mode %v, want 0700", d.Mode().Perm())
	}
}

func TestGmailConnectTestAndNeverShowPassword(t *testing.T) {
	a, h := newTestApp(t)
	fake := useFakeGmail(t, a)

	// A wrong password is refused in plain words, is not stored, and is not echoed.
	rec := post(h, "/connectors/gmail", map[string][]string{"address": {testAddr}, "password": {"wrong password here"}})
	body := rec.Body.String()
	if rec.Code != http.StatusBadRequest || !strings.Contains(body, "Invalid credentials") {
		t.Fatalf("wrong password: %d %s", rec.Code, body)
	}
	if strings.Contains(body, "wrongpasswordhere") || strings.Contains(body, "wrong password here") {
		t.Fatal("the typed password came back on the page")
	}
	if _, ok := a.gmailSecret(); ok {
		t.Fatal("a refused password was stored")
	}

	// The right one (typed with Google's spaces) connects.
	rec = post(h, "/connectors/gmail", map[string][]string{"address": {testAddr}, "password": {"abcd efgh ijkl mnop"}})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("connect: %d %s", rec.Code, rec.Body.String())
	}
	sec, ok := a.gmailSecret()
	if !ok || sec.Password != testPass {
		t.Fatal("the app password was not stored without its spaces")
	}

	// Test now: counts and the five newest, with thread id and labels, newest first.
	rec = post(h, "/connectors/gmail/test", nil)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("test: %d", rec.Code)
	}
	last, ok := a.store.LastTest(t.Context(), gmailConn)
	if !ok || !last.OK {
		t.Fatalf("last test: %+v", last)
	}
	for _, want := range []string{"Sign-in ok.", "Messages in [Gmail]/All Mail: 42", "José Novák", "český úvod", "thread 100042", `labels: \Inbox, Work, My Label`, "Subject 38"} {
		if !strings.Contains(last.Result, want) {
			t.Errorf("test result lacks %q:\n%s", want, last.Result)
		}
	}
	if strings.Index(last.Result, "Subject 41") > strings.Index(last.Result, "Subject 40") {
		t.Error("messages are not newest first")
	}
	if strings.Contains(last.Result, testPass) {
		t.Fatal("the password is in the stored result")
	}
	for _, c := range fake.commands() {
		if strings.HasPrefix(c, "SELECT") || strings.Contains(c, "BODY[") {
			t.Errorf("the trial must only read without side effects, sent %q", c)
		}
	}

	// The page shows the stored result after a reload and never the password.
	page := get(h, "/connectors").Body.String()
	if !strings.Contains(page, "Connected as <b>"+testAddr+"</b>") || !strings.Contains(page, "Messages in [Gmail]/All Mail: 42") {
		t.Fatalf("page lacks the connected line or last test:\n%s", page)
	}
	if strings.Contains(page, testPass) || strings.Contains(page, "abcd efgh") {
		t.Fatal("the password is on the page")
	}

	// Disconnect removes the secret.
	post(h, "/connectors/gmail/disconnect", nil)
	if _, ok := a.gmailSecret(); ok {
		t.Fatal("disconnect left the password")
	}
}

func TestGmailRefusalAtTestTimeIsPlain(t *testing.T) {
	a, _ := newTestApp(t)
	useFakeGmail(t, a)
	text, ok := gmailTest(a.gmail, gmailSecret{Address: testAddr, Password: "revoked-password"}, time.UTC)
	if ok || !strings.Contains(text, "Gmail refused: ") || !strings.Contains(text, "Invalid credentials") {
		t.Fatalf("got ok=%v %q", ok, text)
	}
	if strings.Contains(text, "revoked-password") {
		t.Fatal("password in the message")
	}
}

func TestTestMailGoesOnlyToTheConnectedAddress(t *testing.T) {
	rcpt, msg := buildTestMail(testAddr, time.Now())
	if len(rcpt) != 1 || rcpt[0] != testAddr {
		t.Fatalf("recipients %v", rcpt)
	}
	text := string(msg)
	if strings.Count(text, "\nTo: ") != 1 || !strings.Contains(text, "\r\nTo: "+testAddr+"\r\n") || strings.Contains(strings.ToLower(text), "\r\ncc:") || strings.Contains(strings.ToLower(text), "\r\nbcc:") {
		t.Fatalf("headers:\n%s", text)
	}
	// An address that could smuggle a second recipient or a header is refused.
	for _, bad := range []string{"a@b.test\r\nBcc: x@y.test", "a@b.test, c@d.test", "The owner <a@b.test>", "a@b.test;c@d.test", "", "no-at-sign", "a b@c.test"} {
		if _, ok := cleanAddress(bad); ok {
			t.Errorf("cleanAddress accepted %q", bad)
		}
	}
	if a, ok := cleanAddress(" " + testAddr + " "); !ok || a != testAddr {
		t.Error("a plain address was refused")
	}
}

func TestParseFetchHandlesLiteralsAndOddLabels(t *testing.T) {
	line := "7 FETCH (X-GM-THRID 55 X-GM-LABELS () INTERNALDATE \"1-Dec-2026 08:05:06 +0000\" BODY[HEADER.FIELDS (FROM SUBJECT DATE)] \"From: a@b.test\r\nSubject: folded\r\n  line\r\n\r\n\")"
	f, ok := parseFetch(line)
	if !ok || f.Seq != 7 || f.ThreadID != "55" || len(f.Labels) != 0 || f.Subject != "folded line" || f.From != "a@b.test" || f.Date.IsZero() {
		t.Fatalf("%+v ok=%v", f, ok)
	}
	if _, ok := parseFetch("garbage"); ok {
		t.Fatal("garbage parsed")
	}
}
