package main

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"lab/apps/one/repo"
)

// Replies to Office mail, through Graph's reply call, against the fake
// Microsoft. Nothing here can reach Microsoft.

const graphConv = "C-reply"

type gReplyEnv struct {
	*genv
	mu   sync.Mutex
	note []notice
	msg  *gmMsg
}

func newGraphReplyEnv(t *testing.T) *gReplyEnv {
	t.Helper()
	e := &gReplyEnv{genv: newGraphEnv(t)}
	e.a.notify = func(title, body, link string) error {
		e.mu.Lock()
		e.note = append(e.note, notice{title, body, link})
		e.mu.Unlock()
		return nil
	}
	m := e.in(graphConv, "Nabídka na zakázku č. 12", 1)
	m.from = "Jana Example <jana@example.test>"
	m.to = graphSelf + ", Pavel <pavel@example.test>"
	m.cc = "eva@example.test"
	m.text = "Prosím o potvrzení termínu. Jana"
	e.msg = e.box.add("F-inbox", m)
	e.must(e.importMonth())
	claudeLooksAtAll(t, e.a)
	e.setItem(graphConv, func(it *repo.Item) { it.Stage = stageInProgress })
	return e
}

func (e *gReplyEnv) notices() []notice {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]notice(nil), e.note...)
}

// TestOfficeReplyUsesTheReplyCallForTheRightMessageAndNothingElseIsWritten
func TestOfficeReplyUsesTheReplyCallForTheRightMessageAndNothingElseIsWritten(t *testing.T) {
	e := newGraphReplyEnv(t)
	it := e.item(graphConv)
	pid := proposeReplyAs(t, e.h, e.a, it, askText)
	row := rowOf(e.h, it)
	for _, want := range []string{"Will be sent · reply to the Office mail", "jana@example.test, pavel@example.test", "eva@example.test", ">Approve</button>", ">Discuss</button>"} {
		if !strings.Contains(row, want) {
			t.Errorf("the row lacks %q", want)
		}
	}
	if strings.Contains(row, graphSelf+",") || strings.Contains(row, ", "+graphSelf) {
		t.Error("The owner's own address is a recipient")
	}
	edited := askText + "\n\nS pozdravem,\nPetr"
	if rec := approveAs(e.h, pid, edited); rec.Code != 200 {
		t.Fatalf("approve: %d %s", rec.Code, rec.Body.String())
	}
	if len(e.ms.replies) != 1 {
		t.Fatalf("%d reply calls", len(e.ms.replies))
	}
	call := e.ms.replies[0]
	if call.ID != e.msg.id || !strings.Contains(call.Prefer, "ImmutableId") {
		t.Errorf("reply to %q (%q)", call.ID, call.Prefer)
	}
	msg := call.Body["message"].(map[string]any)
	body := msg["body"].(map[string]any)
	if body["contentType"] != "Text" || body["content"] != edited {
		t.Errorf("body %v", body)
	}
	if _, has := call.Body["comment"]; has {
		t.Error("a comment is sent with the message body")
	}
	var to, cc []string
	for _, r := range msg["toRecipients"].([]any) {
		to = append(to, r.(map[string]any)["emailAddress"].(map[string]any)["address"].(string))
	}
	for _, r := range msg["ccRecipients"].([]any) {
		cc = append(cc, r.(map[string]any)["emailAddress"].(map[string]any)["address"].(string))
	}
	if strings.Join(to, ",") != "jana@example.test,pavel@example.test" || strings.Join(cc, ",") != "eva@example.test" {
		t.Errorf("to %v cc %v", to, cc)
	}
	// The only write is the reply call.
	if len(e.ms.writes) != 1 || e.ms.writes[0] != "POST /me/messages/"+e.msg.id+"/reply" {
		t.Errorf("writes: %v", e.ms.writes)
	}
	if len(e.ms.sent) != 0 {
		t.Error("sendMail was used")
	}
	if now := e.item(graphConv); now.Stage != stagePending || now.NeedsClaude {
		t.Errorf("item %+v", now)
	}
	if !strings.Contains(rowOf(e.h, it), "Sent 13:00.") {
		t.Error("the thread does not say it was sent")
	}
	// Twice is once.
	approveAs(e.h, pid, edited)
	if len(e.ms.replies) != 1 {
		t.Errorf("%d reply calls after a second press", len(e.ms.replies))
	}
}

// TestTheSentMailComingBackThroughTheGraphImportIsOneMessageAndDoesNotWakeTheReview
func TestTheSentMailComingBackThroughTheGraphImportIsOneMessageAndDoesNotWakeTheReview(t *testing.T) {
	e := newGraphReplyEnv(t)
	it := e.item(graphConv)
	marker := "Graf-věta ŽLUŤOUČKÝ-77 pro Office."
	pid := proposeReplyAs(t, e.h, e.a, it, marker)
	approveAs(e.h, pid, marker)
	before := e.a.store.CountRows(context.Background(), "messages")
	back := testMail{thrid: graphConv, from: graphSelf, to: "jana@example.test", cc: "eva@example.test", subject: "RE: Nabídka na zakázku č. 12", date: e.a.now().Add(20 * time.Second),
		flags: []string{`\Seen`}, text: marker + "\n\nOd: Jana\nProsím o potvrzení termínu."}
	e.box.add("F-sent", back)
	e.must(e.sync())
	now := e.item(graphConv)
	if n := e.a.store.CountRows(context.Background(), "messages"); n != before+1 {
		t.Errorf("messages went from %d to %d", before, n)
	}
	if now.NeedsClaude || now.Hidden || now.Stage != stagePending {
		t.Errorf("the import woke the review: %+v", now)
	}
	p := proposalOf(t, e.a, pid)
	if p.SentMsgID == 0 || len(e.ms.writes) != 1 {
		t.Errorf("proposal %+v, writes %v", p, e.ms.writes)
	}
	row := rowOf(e.h, it)
	if strings.Count(row, ">Graf-věta") != 2 { // the list line's snippet and the one message
		t.Errorf("the sent text is shown %d times", strings.Count(row, ">Graf-věta"))
	}
	if strings.Contains(row, "reply-text-shown") {
		t.Error("the card shows the text again")
	}
	e.must(e.sync())
	if n := e.a.store.CountRows(context.Background(), "messages"); n != before+1 {
		t.Errorf("a second sync: %d", n)
	}
	// Every call the importer made after the send was a read.
	for _, c := range e.box.callList() {
		if !strings.HasPrefix(c, "GET ") {
			t.Errorf("a call that is not a read: %s", c)
		}
	}
}

// TestOfficeSendFailuresAreInPlainWordsAndAMissingPermissionSaysSo
func TestOfficeSendFailuresAreInPlainWordsAndAMissingPermissionSaysSo(t *testing.T) {
	cases := []struct {
		name   string
		status int
		want   string
		state  string
	}{
		{"no permission", 403, "no permission to send mail (Mail.Send)", "failed"},
		{"message gone", 404, "no longer has the message", "failed"},
		{"throttled", 429, "asked ONE to wait", "failed"},
		{"sign-in refused", 401, "did not accept the sign-in", "failed"},
		{"server error", 500, "may have gone", "unknown"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newGraphReplyEnv(t)
			it := e.item(graphConv)
			pid := proposeReplyAs(t, e.h, e.a, it, "TAJNÝ-8812 "+askText)
			e.ms.replyStatus = c.status
			approveAs(e.h, pid, "TAJNÝ-8812 "+askText)
			p := proposalOf(t, e.a, pid)
			if p.SendState != c.state || !strings.Contains(p.SendError, c.want) {
				t.Fatalf("proposal %+v", p)
			}
			if !strings.Contains(rowOf(e.h, it), "Could not send: ") {
				t.Error("the thread does not say it could not be sent")
			}
			ns := e.notices()
			if len(ns) != 1 || ns[0].Title != "Could not send" || strings.Contains(ns[0].Body+ns[0].URL, "TAJNÝ") || strings.Contains(ns[0].Body, "jana") {
				t.Errorf("notices %v", ns)
			}
			if now := e.item(graphConv); now.Stage != stageInProgress {
				t.Errorf("the item is %s", now.Stage)
			}
			e.ms.replyStatus = 0
			approveAs(e.h, pid, "TAJNÝ-8812 "+askText)
			if len(e.ms.replies) > 2 { // a refused token is renewed and tried once more; nothing else is repeated
				t.Errorf("%d reply calls", len(e.ms.replies))
			}
			if fmt.Sprint(len(e.ms.replies)) == "" {
				t.Fatal("unreachable")
			}
		})
	}
	// Not signed in.
	e := newGraphReplyEnv(t)
	it := e.item(graphConv)
	pid := proposeReplyAs(t, e.h, e.a, it, askText)
	e.must(removeSecret(e.a.secretsDir, graphSecretFile))
	approveAs(e.h, pid, askText)
	if p := proposalOf(t, e.a, pid); p.SendState != "failed" || !strings.Contains(p.SendError, "needs a new sign-in") || len(e.ms.replies) != 0 {
		t.Errorf("proposal %+v", p)
	}
}
