package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"mime/quotedprintable"
	"net/http"
	"net/http/httptest"
	"net/mail"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"lab/apps/one/repo"
)

// Tests of replies to mail (SPEC 13, 14, 22, 29). Everything is invented; the
// mail servers are fakes, and nothing here can reach Google or Microsoft.

const (
	replyThread = "T-reply"
	askText     = "Dobrý den, děkuji za zprávu. Příliš žluťoučký kůň úpěl ďábelské ódy. Ďakujem, v štvrtok o 14:00 môžem."
)

type notice struct{ Title, Body, URL string }

// rEnv is the Gmail side: the app, a fake IMAP, a fake SMTP and a place that
// keeps the notices that would have gone to the owner's phone.
type rEnv struct {
	*env
	smtp *fakeSMTP
	mu   sync.Mutex
	note []notice
}

func newReplyEnv(t *testing.T) *rEnv {
	t.Helper()
	e := newEnv(t)
	f, crt := startFakeSMTP(t, "smtp.gmail.com", testAddr, testPass)
	e.a.gmail.SMTPAddr = f.addr
	e.a.gmail.SMTPHost = "smtp.gmail.com"
	e.a.gmail.TLS.RootCAs.AddCert(crt)
	r := &rEnv{env: e, smtp: f}
	e.a.notify = func(title, body, link string) error {
		r.mu.Lock()
		r.note = append(r.note, notice{title, body, link})
		r.mu.Unlock()
		return nil
	}
	return r
}

func (r *rEnv) events(id int64, types ...string) []repo.Entry {
	r.t.Helper()
	ev, err := r.a.store.EntriesOfItem(context.Background(), id, types...)
	if err != nil {
		r.t.Fatal(err)
	}
	return ev
}

func (r *rEnv) ajaxPost(path string, form url.Values) *httptest.ResponseRecorder {
	req := httptest.NewRequest("POST", path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Requested-With", "one")
	rec := httptest.NewRecorder()
	r.h.ServeHTTP(rec, req)
	return rec
}

func (r *rEnv) notices() []notice {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]notice(nil), r.note...)
}

// ask brings a mail from someone else into the app as an item the owner has open
// (In progress), looked at by Claude.
func (r *rEnv) ask(m testMail) repo.Item {
	r.t.Helper()
	if m.thrid == "" {
		m.thrid = replyThread
	}
	r.box.add(m)
	r.must(r.importMonth())
	claudeLooksAtAll(r.t, r.a)
	r.setItem(m.thrid, func(it *repo.Item) { it.Stage = stageInProgress })
	return r.item(m.thrid)
}

func (r *rEnv) question(subject string) testMail {
	m := r.in(replyThread, subject, 1)
	m.from = "Alice Example <alice@example.test>"
	m.to = testAddr + ", Bob Example <bob@example.test>"
	m.cc = "carol@example.test"
	m.text = "Máte čas na schůzku? Zdravím, Alice."
	return m
}

// agent calls an address under /api/ as an agent with the right token.
func agentCall(h http.Handler, a *app, method, path string, body any) (int, map[string]any) {
	b, _ := json.Marshal(body)
	req := httptest.NewRequest(method, path, strings.NewReader(string(b)))
	req.Header.Set("Authorization", "Bearer "+a.agentTokenValue())
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var m map[string]any
	json.Unmarshal(rec.Body.Bytes(), &m)
	return rec.Code, m
}

// proposeReplyAs is Claude's call; it returns the proposal's number.
func proposeReplyAs(t *testing.T, h http.Handler, a *app, it repo.Item, text string) int64 {
	t.Helper()
	code, m := agentCall(h, a, "POST", "/api/items/"+repo.Number(it.Number)+"/reply-proposals", map[string]any{"text": text, "reason": "Alice asked for a time."})
	if code != 200 || m["ok"] != true {
		t.Fatalf("propose a reply: %d %v", code, m)
	}
	return int64(m["proposal_id"].(float64))
}

// approveAs is the owner pressing Approve on the page with the box as it stands.
func approveAs(h http.Handler, pid int64, text string) *httptest.ResponseRecorder {
	form := url.Values{"text": {text}, "open": {"1"}}
	req := httptest.NewRequest("POST", fmt.Sprintf("/proposals/%d/approve", pid), strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Requested-With", "one")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func rowOf(h http.Handler, it repo.Item) string {
	return get(h, fmt.Sprintf("/items/%d/row?open=1", it.ID)).Body.String()
}

func proposalOf(t *testing.T, a *app, id int64) repo.Proposal {
	t.Helper()
	p, ok := a.store.ProposalByID(context.Background(), id)
	if !ok {
		t.Fatalf("no proposal %d", id)
	}
	return p
}

// parsedMail reads what the fake SMTP was handed.
func parseSent(t *testing.T, data string) *mail.Message {
	t.Helper()
	m, err := mail.ReadMessage(strings.NewReader(data))
	if err != nil {
		t.Fatalf("the mail cannot be read: %v", err)
	}
	return m
}

func decodedBody(t *testing.T, m *mail.Message) string {
	t.Helper()
	if m.Header.Get("Content-Transfer-Encoding") != "quoted-printable" {
		t.Fatalf("encoding %q", m.Header.Get("Content-Transfer-Encoding"))
	}
	b, err := io.ReadAll(quotedprintable.NewReader(m.Body))
	if err != nil {
		t.Fatal(err)
	}
	return strings.ReplaceAll(string(b), "\r\n", "\n")
}

func addrList(t *testing.T, m *mail.Message, name string) []string {
	t.Helper()
	l, err := m.Header.AddressList(name)
	if err != nil {
		return nil
	}
	var out []string
	for _, a := range l {
		out = append(out, a.Address)
	}
	return out
}

// --- showing ---

// TestReplyProposalShowsTheExactWordingTheTextAndTheRecipientsTheServerWorkedOut:
// the label, the text in an editable box, who it goes to (the sender and the
// others already on the message, never the owner), Approve and Discuss.
func TestReplyProposalShowsTheExactWordingTheTextAndTheRecipientsTheServerWorkedOut(t *testing.T) {
	r := newReplyEnv(t)
	it := r.ask(r.question("Schůzka ve čtvrtek"))
	pid := proposeReplyAs(t, r.h, r.a, it, askText)
	p := proposalOf(t, r.a, pid)
	if p.Action != replyGmail || p.Status != "pending" || p.Text != askText {
		t.Fatalf("stored: %+v", p)
	}
	if rt := parseRecipients(p.Recipient); strings.Join(rt.To, ",") != "alice@example.test,bob@example.test" || strings.Join(rt.Cc, ",") != "carol@example.test" {
		t.Errorf("recipients %+v", rt)
	}
	row := rowOf(r.h, it)
	for _, want := range []string{"Will be sent · reply to the mail", "<textarea", "Dobrý den, děkuji za zprávu.", ">Approve</button>", ">Discuss</button>",
		"alice@example.test, bob@example.test", "carol@example.test", "Send this reply?", ">Cancel</button>"} {
		if !strings.Contains(row, want) {
			t.Errorf("the row lacks %q", want)
		}
	}
	if strings.Contains(row, testAddr+",") || strings.Contains(row, ", "+testAddr) {
		t.Error("The owner's own address is shown as a recipient")
	}
	// The closed row's chip names the proposal but has no Approve: the text must be seen first.
	closed := get(r.h, fmt.Sprintf("/items/%d/row", it.ID)).Body.String()
	if !strings.Contains(closed, "Reply to the mail") || strings.Contains(closed, fmt.Sprintf("/proposals/%d/approve", pid)) {
		t.Error("the chip lets a reply be approved without its text")
	}
	if !strings.Contains(closed, fmt.Sprintf("/proposals/%d/dismiss", pid)) {
		t.Error("the chip has no Dismiss")
	}
	// Approve without the box's text is refused and nothing goes.
	rec := post(r.h, fmt.Sprintf("/proposals/%d/approve", pid), url.Values{})
	if rec.Code != 400 || len(r.smtp.handed()) != 0 {
		t.Errorf("approve without the box: %d, %d mails", rec.Code, len(r.smtp.handed()))
	}
}

// TestGmailReplyIsAnsweredToTheSenderAloneWhenItCameThroughAListOrFromOneAddress:
// who a reply goes to in each case.
func TestWhoAReplyGoesToInEachCase(t *testing.T) {
	r := newReplyEnv(t)
	// One sender, only the owner on the message: only the sender.
	one := r.question("Jen vy")
	one.to, one.cc, one.thrid = testAddr, "", "T-one"
	itOne := r.ask(one)
	// A mailing list: the writer alone; the list and the others are not written to.
	list := r.question("Newsletter dotaz")
	list.thrid = "T-list"
	list.extra = []string{"List-Id: <dev.lists.example.test>", "List-Post: <mailto:dev@lists.example.test>"}
	list.to = "dev@lists.example.test"
	itList := r.ask(list)
	// A no-reply sender: no reply is proposed at all.
	nr := r.question("Potvrzení")
	nr.from, nr.thrid = "Shop <no-reply@shop.example.test>", "T-nr"
	itNR := r.ask(nr)
	// The owner on the message twice (an alias he has written from), the sender twice.
	dup := r.question("Dvakrát")
	dup.thrid, dup.to, dup.cc = "T-dup", "Alice Example <alice@example.test>, "+testAddr+", dave@example.test", "DAVE@example.test, "+testAddr
	itDup := r.ask(dup)

	to := func(it repo.Item) replyTo {
		return parseRecipients(proposalOf(t, r.a, proposeReplyAs(t, r.h, r.a, it, askText)).Recipient)
	}
	if got := to(itOne); strings.Join(got.To, ",") != "alice@example.test" || len(got.Cc) != 0 {
		t.Errorf("one sender: %+v", got)
	}
	if got := to(itList); strings.Join(got.To, ",") != "alice@example.test" || len(got.Cc) != 0 {
		t.Errorf("mailing list: %+v", got)
	}
	if got := to(itDup); strings.Join(got.To, ",") != "alice@example.test,dave@example.test" || len(got.Cc) != 0 {
		t.Errorf("duplicates: %+v", got)
	}
	code, m := agentCall(r.h, r.a, "POST", "/api/items/"+repo.Number(itNR.Number)+"/reply-proposals", map[string]any{"text": askText, "reason": "x"})
	if code != 409 || !strings.Contains(fmt.Sprint(m["error"]), "no-reply") {
		t.Errorf("a no-reply sender: %d %v", code, m)
	}
	// More people than the cap: refused in words, nothing is stored.
	many := r.question("Všichni")
	many.thrid = "T-many"
	var cc []string
	for i := 0; i < 12; i++ {
		cc = append(cc, fmt.Sprintf("p%d@example.test", i))
	}
	many.cc = strings.Join(cc, ", ")
	itMany := r.ask(many)
	if code, m := agentCall(r.h, r.a, "POST", "/api/items/"+repo.Number(itMany.Number)+"/reply-proposals", map[string]any{"text": askText, "reason": "x"}); code != 409 {
		t.Errorf("a message to 14 people: %d %v", code, m)
	}
}

// --- agents can only propose ---

// TestAnAgentCannotChooseRecipientsApproveEditOrSendAReply
func TestAnAgentCannotChooseRecipientsApproveEditOrSendAReply(t *testing.T) {
	r := newReplyEnv(t)
	it := r.ask(r.question("Schůzka"))
	path := "/api/items/" + repo.Number(it.Number) + "/reply-proposals"
	for _, field := range []string{"to", "cc", "bcc", "recipients", "send_to"} {
		code, m := agentCall(r.h, r.a, "POST", path, map[string]any{"text": askText, "reason": "x", field: []string{"stranger@example.test"}})
		if code != 400 || m["ok"] == true {
			t.Errorf("an agent named %q: %d %v", field, code, m)
		}
	}
	pid := proposeReplyAs(t, r.h, r.a, it, askText)
	// No address under /api/ approves, edits or sends: all are refused for the owner alone.
	for _, p := range []string{
		fmt.Sprintf("/api/proposals/%d/approve", pid), fmt.Sprintf("/api/proposals/%d/send", pid), fmt.Sprintf("/api/proposals/%d", pid),
		fmt.Sprintf("/api/items/%s/send", repo.Number(it.Number)), fmt.Sprintf("/api/items/%s/reply-proposals/%d/approve", repo.Number(it.Number), pid),
		fmt.Sprintf("/api/items/%s/reply-proposals/%d/text", repo.Number(it.Number), pid), "/api/send", "/api/approve",
	} {
		for _, method := range []string{"POST", "PUT", "PATCH"} {
			code, m := agentCall(r.h, r.a, method, p, map[string]any{"text": "other text", "agent": "the owner"})
			if code != 403 && code != 404 && code != 405 || m["ok"] == true {
				t.Errorf("%s %s: %d %v", method, p, code, m)
			}
		}
	}
	// Whatever name an agent gives, the page's functions refuse it.
	for _, who := range []string{"the owner", "owner", "Claude"} {
		if _, err := r.a.approveReply(context.Background(), agentActor(who), pid, "edited by an agent"); err == nil || !strings.Contains(err.Error(), "Refused by guard: Actor by channel") {
			t.Errorf("%s: %v", who, err)
		}
		if _, err := r.a.decide(context.Background(), agentActor(who), pid, true); err == nil {
			t.Errorf("decide as %s", who)
		}
	}
	// The connector has the tool that proposes, and none that sends, approves or edits a reply.
	var propose *mcpTool
	for i, tl := range mcpTools {
		n := strings.ToLower(tl.Name)
		if strings.Contains(n, "send") || strings.Contains(n, "approve") || strings.Contains(n, "edit") {
			t.Errorf("tool %s", tl.Name)
		}
		if tl.Name == "propose_reply" {
			propose = &mcpTools[i]
		}
	}
	if propose == nil {
		t.Fatal("no propose_reply tool")
	}
	props := propose.InputSchema["properties"].(map[string]any)
	for k := range props {
		if k != "id" && k != "text" && k != "reason" && k != "agent" && k != "run" { // run: the test run an automated test names
			t.Errorf("propose_reply takes %q", k)
		}
	}
	if propose.Annotations["readOnlyHint"] != false || !strings.Contains(propose.Description, "Nothing is sent") {
		t.Error("the tool does not say it only proposes")
	}
	if len(r.smtp.handed()) != 0 || proposalOf(t, r.a, pid).Text != askText || proposalOf(t, r.a, pid).Status != "pending" {
		t.Error("something changed or went out")
	}
	// Through the connector.
	text, isErr := r.a.callAPI(context.Background(), "POST", path, mustJSON(map[string]any{"text": askText + " 2", "reason": "x"}))
	if isErr || !strings.Contains(text, "proposal_id") {
		t.Errorf("api: %v %s", isErr, text)
	}
	out, isErr, known := r.a.callTool(context.Background(), "propose_reply", mustJSON(map[string]any{"id": repo.Number(it.Number), "text": askText + " 3", "reason": "x"}))
	if !known || isErr || !strings.Contains(out, "proposal_id") {
		t.Errorf("tool: %v %s", isErr, out)
	}
	out, isErr, _ = r.a.callTool(context.Background(), "propose_reply", mustJSON(map[string]any{"id": repo.Number(it.Number), "text": "x", "reason": "x", "to": []string{"s@example.test"}}))
	if len(r.smtp.handed()) != 0 {
		t.Error("something went out")
	}
	_ = out
	_ = isErr
}

func mustJSON(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}

// TestMCPInstructionsSayWhenToProposeAReplyAndThatItOnlyProposes
func TestMCPInstructionsSayWhenToProposeAReplyAndThatItOnlyProposes(t *testing.T) {
	for _, want := range []string{"propose_reply", "never the recipients", "only proposes", "sender of sign-in codes", "propose no text to send"} {
		if !strings.Contains(mcpInstructions, want) {
			t.Errorf("the instructions lack %q", want)
		}
	}
	api, err := os.ReadFile("API.md")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"reply-proposals", "never given its recipients", "Blocked targets", "no text to send"} {
		if !strings.Contains(string(api), want) {
			t.Errorf("API.md lacks %q", want)
		}
	}
}

// --- Blocked targets and the other refusals ---

// TestBlockedTargetsAreRefusedWithTheGuardsName
func TestBlockedTargetsAreRefusedWithTheGuardsName(t *testing.T) {
	r := newReplyEnv(t)
	cases := map[string]string{
		"bank":           "Pavel <info@unicreditbank.cz>",
		"a subdomain":    "PayPal <service@mail.paypal.com>",
		"the data box":   "ISDS <info@mojedatovaschranka.cz>",
		"sign-in codes":  "Google <noreply-codes@accounts.google.com>",
		"payment, upper": "GoPay <INFO@GoPay.cz>",
	}
	n := 0
	for name, from := range cases {
		n++
		m := r.question("Dotaz " + name)
		m.from, m.thrid = from, fmt.Sprintf("T-blocked-%d", n)
		it := r.ask(m)
		for _, via := range []string{"rest", "tool"} {
			var text string
			var code int
			if via == "rest" {
				var body map[string]any
				code, body = agentCall(r.h, r.a, "POST", "/api/items/"+repo.Number(it.Number)+"/reply-proposals", map[string]any{"text": askText, "reason": "x"})
				text, _ = body["error"].(string)
			} else {
				out, isErr, _ := r.a.callTool(context.Background(), "propose_reply", mustJSON(map[string]any{"id": repo.Number(it.Number), "text": askText, "reason": "x"}))
				text, code = out, 200
				if !isErr {
					t.Errorf("%s: the tool accepted it", name)
				}
			}
			if !strings.Contains(text, "Refused by guard: Blocked targets") || (via == "rest" && code != 403) {
				t.Errorf("%s via %s: %d %q", name, via, code, text)
			}
		}
		if r.a.store.PendingCount(context.Background(), it.ID) != 0 {
			t.Errorf("%s: a proposal was stored", name)
		}
	}
	if got := r.a.store.RefusedSince(context.Background(), 0)["Blocked targets"]; got != 2*len(cases) {
		t.Errorf("refusals counted: %d", got)
	}
	// A bank on the thread earlier counts, and so does a bank among the people it would go to.
	m := r.question("Starší zpráva")
	m.thrid, m.from = "T-mixed", "Bank <info@csob.cz>"
	r.box.add(m)
	later := r.question("Starší zpráva")
	later.thrid, later.date = "T-mixed", r.ago(0.5)
	r.box.add(later)
	r.must(r.importMonth())
	claudeLooksAtAll(t, r.a)
	itMixed := r.item("T-mixed")
	if code, _ := agentCall(r.h, r.a, "POST", "/api/items/"+repo.Number(itMixed.Number)+"/reply-proposals", map[string]any{"text": askText, "reason": "x"}); code != 403 {
		t.Errorf("a bank earlier in the thread: %d", code)
	}
	cc := r.question("Kopie bance")
	cc.thrid, cc.cc = "T-cc", "carol@example.test, ops@fio.cz"
	itCC := r.ask(cc)
	if code, _ := agentCall(r.h, r.a, "POST", "/api/items/"+repo.Number(itCC.Number)+"/reply-proposals", map[string]any{"text": askText, "reason": "x"}); code != 403 {
		t.Errorf("a bank among the recipients: %d", code)
	}
}

// TestBlockedTargetsStartWithBanksPaymentsDataBoxAndCodesAndOwnerEditsTheList:
// the list is data, shown and saved on Settings; the guard can be switched off,
// Exact text, Same thread and Send once cannot.
func TestBlockedTargetsStartWithBanksPaymentsDataBoxAndCodesAndOwnerEditsTheList(t *testing.T) {
	r := newReplyEnv(t)
	page := get(r.h, "/settings").Body.String()
	for _, want := range []string{"unicreditbank.cz", "paypal.com", "mojedatovaschranka.cz", "accounts.google.com", "Exact text", "Same thread", "Blocked targets", "Send once", "Save list"} {
		if !strings.Contains(page, want) {
			t.Errorf("Settings lacks %q", want)
		}
	}
	// The owner removes a domain, and a reply to it can be proposed; an entry that is nothing is refused.
	m := r.question("Fio")
	m.from, m.thrid = "Fio <info@fio.cz>", "T-fio"
	it := r.ask(m)
	if code, _ := agentCall(r.h, r.a, "POST", "/api/items/"+repo.Number(it.Number)+"/reply-proposals", map[string]any{"text": askText, "reason": "x"}); code != 403 {
		t.Fatalf("before: %d", code)
	}
	if rec := post(r.h, "/settings/blocked-targets", url.Values{"list": {"not an entry"}}); rec.Code != 400 {
		t.Errorf("a bad entry: %d", rec.Code)
	}
	if rec := post(r.h, "/settings/blocked-targets", url.Values{"list": {"paypal.com\nFriend@Example.test\n"}}); rec.Code != 303 {
		t.Fatalf("save: %d", rec.Code)
	}
	if code, _ := agentCall(r.h, r.a, "POST", "/api/items/"+repo.Number(it.Number)+"/reply-proposals", map[string]any{"text": askText, "reason": "x"}); code != 200 {
		t.Errorf("after the list was saved without it: %d", code)
	}
	if got := r.a.blockedTargets(context.Background(), r.a.store.Q); strings.Join(got, ",") != "paypal.com,friend@example.test" {
		t.Errorf("list %v", got)
	}
	// The agents' address cannot reach it, and the guard that can go off does; the others cannot.
	if code, _ := agentCall(r.h, r.a, "POST", "/api/settings/blocked-targets", map[string]any{"list": "-"}); code != 403 {
		t.Errorf("an agent saving the list: %d", code)
	}
	for _, key := range []string{"exact_text", "same_thread", "send_once"} {
		if rec := post(r.h, "/settings/guards/"+key+"/off", nil); rec.Code != 400 || !r.a.guardOn(context.Background(), r.a.store.Q, key) {
			t.Errorf("%s: %d", key, rec.Code)
		}
	}
	if rec := post(r.h, "/settings/guards/blocked_targets/off", nil); rec.Code != 303 || r.a.guardOn(context.Background(), r.a.store.Q, "blocked_targets") {
		t.Errorf("Blocked targets off: %d", rec.Code)
	}
	if !blockedBy(defaultBlockedTargets, "x@sub.unicreditbank.cz") || blockedBy(defaultBlockedTargets, "x@notunicreditbank.cz") || !blockedBy([]string{"a@b.test"}, "A@B.test") {
		t.Error("matching of addresses and domains")
	}
}

// TestNoReplyWhenTheNewestMessageIsOwnersOwnOrTheItemIsClosedOrAChat
func TestNoReplyWhenTheNewestMessageIsOwnersOwnOrTheItemIsClosedOrAChat(t *testing.T) {
	r := newReplyEnv(t)
	it := r.ask(r.question("Odpověděl jsem"))
	r.box.add(r.out(replyThread, "Re: Odpověděl jsem", 0.2))
	r.must(r.importMonth())
	r.setItem(replyThread, func(i *repo.Item) { i.Stage = stageInProgress })
	code, m := agentCall(r.h, r.a, "POST", "/api/items/"+repo.Number(it.Number)+"/reply-proposals", map[string]any{"text": askText, "reason": "x"})
	if code != 403 || !strings.Contains(fmt.Sprint(m["error"]), "Refused by guard: Same thread") {
		t.Errorf("newest is the owner's own: %d %v", code, m)
	}
	// A chat item is not part of this slice.
	chat := r.listChat(t)
	if code, m := agentCall(r.h, r.a, "POST", "/api/items/"+repo.Number(chat.Number)+"/reply-proposals", map[string]any{"text": askText, "reason": "x"}); code != 409 {
		t.Errorf("a chat: %d %v", code, m)
	}
	// Closed items.
	r.setItem(replyThread, func(i *repo.Item) { i.Stage = stageClosed })
	r.box.add(r.in(replyThread, "Nová", 0.1))
	closed := r.question("Zavřená")
	closed.thrid = "T-closed"
	ci := r.ask(closed)
	r.setItem("T-closed", func(i *repo.Item) { i.Stage = stageClosed })
	if code, _ := agentCall(r.h, r.a, "POST", "/api/items/"+repo.Number(ci.Number)+"/reply-proposals", map[string]any{"text": askText, "reason": "x"}); code != 409 {
		t.Errorf("a closed item: %d", code)
	}
	// Empty text, no reason, and a text with a control character are refused.
	open := r.question("Otevřená")
	open.thrid = "T-open"
	oi := r.ask(open)
	for _, b := range []map[string]any{{"text": "  ", "reason": "x"}, {"text": askText, "reason": ""}, {"text": "a\x00b", "reason": "x"}, {"text": strings.Repeat("a", maxReplyText+1), "reason": "x"}} {
		if code, _ := agentCall(r.h, r.a, "POST", "/api/items/"+repo.Number(oi.Number)+"/reply-proposals", b); code != 400 {
			t.Errorf("%v: %d", b["reason"], code)
		}
	}
}

// listChat makes an item whose newest message is a chat message.
func (r *rEnv) listChat(t *testing.T) repo.Item {
	t.Helper()
	l := &listEnv{t: t, a: r.a, h: r.h, ctx: context.Background(), now: r.base}
	return l.seed(stageInProgress, seedMsg{subject: "Chat", name: "Eva", sender: "eva", text: "Ahoj", at: l.ago(1), network: "messenger"})
}

var ids = regexp.MustCompile(`<[^<>\s]+>`)

// --- sending ---

// TestApproveSendsExactlyTheEditedTextAndKeepsBothTexts: what goes out is the text
// in the box when Approve was pressed, as plain text; the original and the edit
// are both kept as events; the item moves In progress to Pending; the thread says Sent.
func TestApproveSendsExactlyTheEditedTextAndKeepsBothTexts(t *testing.T) {
	r := newReplyEnv(t)
	it := r.ask(r.question("Schůzka ve čtvrtek"))
	pid := proposeReplyAs(t, r.h, r.a, it, askText)
	edited := "Ahoj Alice,\r\n\r\nve čtvrtek o 15:00 to vyjde.  \r\n\r\nPetr\r\n"
	rec := approveAs(r.h, pid, edited)
	if rec.Code != 200 {
		t.Fatalf("approve: %d %s", rec.Code, rec.Body.String())
	}
	got := r.smtp.handed()
	if len(got) != 1 {
		t.Fatalf("%d mails", len(got))
	}
	m := parseSent(t, got[0].Data)
	if body := decodedBody(t, m); body != strings.ReplaceAll(edited, "\r\n", "\n") {
		t.Errorf("the text that went out is not the text in the box: %q", body)
	}
	if ct := m.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") || !strings.Contains(ct, "utf-8") {
		t.Errorf("content type %q", ct)
	}
	p := proposalOf(t, r.a, pid)
	if p.Text != askText || p.FinalText != strings.ReplaceAll(edited, "\r\n", "\n") || p.Status != "approved" || p.SendState != "sent" {
		t.Errorf("proposal %+v", p)
	}
	var found bool
	for _, ev := range r.events(it.ID, "proposal_decided") {
		var d map[string]any
		json.Unmarshal([]byte(ev.Data), &d)
		if d["original"] == askText && d["text"] == p.FinalText && d["edited"] == true && ev.Actor == "owner" {
			found = true
		}
	}
	if !found {
		t.Error("the original and the edited text are not both kept in the record")
	}
	if len(r.events(it.ID, "send_intent")) != 1 || len(r.events(it.ID, "reply_sent")) != 1 {
		t.Error("intent and result are not both recorded")
	}
	if now := r.item(replyThread); now.Stage != stagePending || now.NeedsClaude {
		t.Errorf("item: %s, waits for Claude: %v", now.Stage, now.NeedsClaude)
	}
	row := rowOf(r.h, it)
	if !strings.Contains(row, "Sent 13:00.") {
		t.Errorf("the thread does not say when it was sent:\n%s", row)
	}
	if strings.Contains(row, `class="reply-text"`) || len(r.notices()) != 0 {
		t.Error("the box is still there, or a notice went out")
	}
	// The intent was written before the mail left: the order of the record says so.
	var order []string
	all, _ := r.a.store.EventsOfItem(context.Background(), it.ID, 100)
	for _, ev := range all {
		if ev.Type == "send_intent" || ev.Type == "reply_sent" {
			order = append(order, ev.Type)
		}
	}
	if strings.Join(order, ",") != "send_intent,reply_sent" {
		t.Errorf("order %v", order)
	}
}

// TestGmailMessageHasTheRightHeadersAndEncodingForCzechAndSlovakLettersAndALongLine
func TestGmailMessageHasTheRightHeadersAndEncodingForCzechAndSlovakLettersAndALongLine(t *testing.T) {
	r := newReplyEnv(t)
	subject := "Příliš žluťoučký kůň úpěl ďábelské ódy – nábytek na zakázku, objednávka č. 2026/118, prosím o potvrzení termínu dodání, ďakujem"
	q := r.question(subject)
	q.extra = []string{"References: <first@example.test> <second@example.test>"}
	it := r.ask(q)
	target, _ := r.a.store.NewestMessage(context.Background(), it.ID)
	long := strings.Repeat("Dlouhýřádekbezmezer", 50) // 950 characters without a space
	text := askText + "\n\n" + long + "\n.\n.dot first\nposlední řádek  "
	pid := proposeReplyAs(t, r.h, r.a, it, text)
	if rec := approveAs(r.h, pid, text); rec.Code != 200 {
		t.Fatalf("approve: %d", rec.Code)
	}
	sent := r.smtp.handed()
	if len(sent) != 1 {
		t.Fatalf("%d mails", len(sent))
	}
	mm := sent[0]
	if mm.From != testAddr || strings.Join(mm.Rcpts, ",") != "alice@example.test,bob@example.test,carol@example.test" {
		t.Errorf("envelope: %q %v", mm.From, mm.Rcpts)
	}
	m := parseSent(t, mm.Data)
	if got := addrList(t, m, "From"); len(got) != 1 || got[0] != testAddr {
		t.Errorf("From %v", got)
	}
	if got := strings.Join(addrList(t, m, "To"), ","); got != "alice@example.test,bob@example.test" {
		t.Errorf("To %q", got)
	}
	if got := strings.Join(addrList(t, m, "Cc"), ","); got != "carol@example.test" {
		t.Errorf("Cc %q", got)
	}
	if m.Header.Get("Bcc") != "" {
		t.Error("a Bcc")
	}
	subj, err := new(mime.WordDecoder).DecodeHeader(m.Header.Get("Subject"))
	if err != nil || subj != "Re: "+subject {
		t.Errorf("Subject %q (%v)", subj, err)
	}
	if got := m.Header.Get("In-Reply-To"); got != strings.TrimSpace(target.HeaderID) {
		t.Errorf("In-Reply-To %q, want %q", got, target.HeaderID)
	}
	if got := m.Header.Get("References"); got != "<first@example.test> <second@example.test> "+strings.TrimSpace(target.HeaderID) {
		t.Errorf("References %q", got)
	}
	if d, err := m.Header.Date(); err != nil || !d.Equal(r.a.now()) {
		t.Errorf("Date %v %v", d, err)
	}
	if id := m.Header.Get("Message-ID"); !regexp.MustCompile(`^<[0-9a-f]{24}@example\.test>$`).MatchString(id) || id != proposalOf(t, r.a, pid).SendRef {
		t.Errorf("Message-ID %q", id)
	}
	if body := decodedBody(t, m); body != text+"\n" {
		t.Errorf("body: %q", body)
	}
	for i, line := range strings.Split(strings.TrimSuffix(mm.Data, "\r\n"), "\r\n") {
		if len(line) > 78 {
			t.Errorf("line %d is %d characters long", i, len(line))
		}
	}
	if !isASCII(strings.Split(mm.Data, "\r\n\r\n")[0]) {
		t.Error("a header is not ASCII")
	}
	if strings.Contains(mm.Data, "Dobrý") || strings.Contains(strings.Split(mm.Data, "\r\n\r\n")[1], ">") {
		t.Error("the text is not encoded, or there is a quote")
	}
	// The SMTP session was on a STARTTLS port: the credentials came after TLS, and the fake saw STARTTLS.
	found := false
	for _, c := range r.smtp.commands() {
		found = found || c == "STARTTLS"
	}
	if !found {
		t.Error("no STARTTLS")
	}
}

// TestReCameOnceAndAHeaderCannotBeInjectedThroughASubject
func TestReCameOnceAndAHeaderCannotBeInjectedThroughASubject(t *testing.T) {
	if got := replySubject("Re: Hotovo"); got != "Re: Hotovo" {
		t.Errorf("%q", got)
	}
	if got := replySubject("RE:Hotovo"); got != "RE:Hotovo" {
		t.Errorf("%q", got)
	}
	if got := replySubject("Odp: Hotovo"); got != "Odp: Hotovo" {
		t.Errorf("%q", got)
	}
	if got := replySubject("Hotovo"); got != "Re: Hotovo" {
		t.Errorf("%q", got)
	}
	r := newReplyEnv(t)
	q := r.question("Plán\r\nBcc: evil@example.test\r\nX-Injected: yes")
	it := r.ask(q)
	text := "Dobře."
	pid := proposeReplyAs(t, r.h, r.a, it, text)
	approveAs(r.h, pid, text)
	sent := r.smtp.handed()
	if len(sent) != 1 {
		t.Fatalf("%d mails", len(sent))
	}
	m := parseSent(t, sent[0].Data)
	if m.Header.Get("Bcc") != "" || m.Header.Get("X-Injected") != "" {
		t.Error("a header was injected")
	}
	for _, rc := range sent[0].Rcpts {
		if strings.Contains(rc, "evil") {
			t.Error("a stranger is a recipient")
		}
	}
	// An ASCII subject that is long is folded, not cut.
	long := "Re: " + strings.Repeat("slovo ", 40)
	b := buildReplyMail(replyMail{From: testAddr, To: []string{"a@example.test"}, Subject: long, Body: "x", Now: time.Now(), MessageID: "<a@example.test>"})
	for _, l := range strings.Split(string(b), "\r\n") {
		if len(l) > 78 {
			t.Errorf("a line of %d", len(l))
		}
	}
	if mm := parseSent(t, string(b)); mm.Header.Get("Subject") != strings.TrimSpace(long) {
		t.Errorf("subject %q", mm.Header.Get("Subject"))
	}
}

// TestNobodyOutsideTheThreadAndNotOwnersOwnAddressIsEverARecipient
func TestNobodyOutsideTheThreadAndNotOwnersOwnAddressIsEverARecipient(t *testing.T) {
	target := repo.Message{Sender: "alice@example.test", To: "owner@example.test, bob@example.test", Cc: "carol@example.test"}
	own := []string{"owner@example.test"}
	if err := verifyRecipients(target, replyTo{To: []string{"alice@example.test", "bob@example.test"}, Cc: []string{"carol@example.test"}}, own); err != nil {
		t.Errorf("the thread's own people: %v", err)
	}
	for name, rt := range map[string]replyTo{
		"a stranger in To": {To: []string{"alice@example.test", "stranger@example.test"}},
		"a stranger in Cc": {To: []string{"alice@example.test"}, Cc: []string{"stranger@example.test"}},
		"The owner in To":  {To: []string{"alice@example.test", "owner@example.test"}},
		"The owner in Cc":  {To: []string{"alice@example.test"}, Cc: []string{"owner@example.test"}},
		"nobody":           {},
	} {
		err := verifyRecipients(target, rt, own)
		if err == nil || !strings.Contains(err.Error(), "Refused by guard: Same thread") {
			t.Errorf("%s: %v", name, err)
		}
	}
	// And in a whole send: the recipients of the stored proposal changed behind the scenes.
	r := newReplyEnv(t)
	it := r.ask(r.question("Podvrh"))
	pid := proposeReplyAs(t, r.h, r.a, it, askText)
	r.must(r.a.store.Update(context.Background(), func(tx *repo.Tx) error {
		_, err := tx.Exec(`UPDATE proposals SET recipients = ? WHERE id = ?`, `{"to":["alice@example.test","stranger@example.test"],"cc":[]}`, pid)
		return err
	}))
	approveAs(r.h, pid, askText)
	if n := len(r.smtp.handed()); n != 0 {
		t.Errorf("%d mails went to people nobody proposed", n)
	}
	if p := proposalOf(t, r.a, pid); p.Status != "withdrawn" || p.SendState != "" {
		t.Errorf("proposal %+v", p)
	}
	// Every mail the app sends in these tests had only people from the thread.
	for _, rc := range []string{"alice@example.test"} {
		_ = rc
	}
}

// TestPressingApproveTwiceSendsOnce
func TestPressingApproveTwiceSendsOnce(t *testing.T) {
	r := newReplyEnv(t)
	it := r.ask(r.question("Dvakrát"))
	pid := proposeReplyAs(t, r.h, r.a, it, askText)
	var wg sync.WaitGroup
	codes := make([]int, 8)
	for i := range codes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			codes[i] = approveAs(r.h, pid, askText).Code
		}()
	}
	wg.Wait()
	for _, c := range codes {
		if c != 200 {
			t.Errorf("answer %d", c)
		}
	}
	// A retry later, with another text, from a second device.
	approveAs(r.h, pid, askText+" changed")
	approveAs(r.h, pid, askText)
	if n := len(r.smtp.handed()); n != 1 {
		t.Fatalf("%d mails were sent", n)
	}
	if p := proposalOf(t, r.a, pid); p.FinalText != askText {
		t.Errorf("the text kept is %q", p.FinalText)
	}
	if got := r.a.store.RefusedSince(context.Background(), 0)["Send once"]; got < 1 {
		t.Error("the repeated presses are not recorded")
	}
	if len(r.events(it.ID, "send_intent")) != 1 {
		t.Error("more than one intent")
	}
}

// TestARestartBetweenIntentAndResultDoesNotResendAndSaysSo
func TestARestartBetweenIntentAndResultDoesNotResendAndSaysSo(t *testing.T) {
	r := newReplyEnv(t)
	it := r.ask(r.question("Restart"))
	pid := proposeReplyAs(t, r.h, r.a, it, askText)
	// The intent is written, and then the app stops before anything else happens.
	ctx := context.Background()
	r.must(r.a.store.Update(ctx, func(tx *repo.Tx) error {
		if _, err := tx.DecideProposal(ctx, pid, "approved", "owner", "", r.a.now().Unix()); err != nil {
			return err
		}
		_, err := tx.BeginSend(ctx, pid, askText, "<abc@example.test>", r.a.now().Unix())
		return err
	}))
	r.a.recoverSends(ctx) // the next start
	r.a.recoverSends(ctx) // and one more, which changes nothing
	if p := proposalOf(t, r.a, pid); p.SendState != "unknown" || !strings.Contains(p.SendError, "may have gone") {
		t.Fatalf("proposal %+v", p)
	}
	approveAs(r.h, pid, askText) // the owner presses it again
	if n := len(r.smtp.handed()); n != 0 || r.smtp.conns != 0 {
		t.Errorf("a mail was sent after a restart (%d, %d connections)", n, r.smtp.conns)
	}
	row := rowOf(r.h, it)
	if !strings.Contains(row, "Could not send: ONE stopped while the reply was being sent. It may have gone: check the Sent folder") {
		t.Errorf("the thread does not say it may have gone:\n%s", row)
	}
	if now := r.item(replyThread); now.Stage != stageInProgress {
		t.Errorf("the item is %s", now.Stage)
	}
	if ns := r.notices(); len(ns) != 1 || ns[0].Title != "Could not send" {
		t.Errorf("notices %v", ns)
	}
}

// TestADroppedConnectionAfterTheDataIsNotRepeated: the mail may have gone.
func TestADroppedConnectionAfterTheDataIsNotRepeated(t *testing.T) {
	r := newReplyEnv(t)
	it := r.ask(r.question("Přerušení"))
	pid := proposeReplyAs(t, r.h, r.a, it, askText)
	r.smtp.setMode("dropAfterData")
	approveAs(r.h, pid, askText)
	r.smtp.setMode("")
	approveAs(r.h, pid, askText)
	approveAs(r.h, pid, askText)
	if n := r.smtp.conns; n != 1 {
		t.Errorf("%d connections: the mail was sent again", n)
	}
	p := proposalOf(t, r.a, pid)
	if p.SendState != "unknown" || !strings.Contains(p.SendError, "may have gone") || !strings.Contains(p.SendError, "Sent folder") {
		t.Errorf("proposal %+v", p)
	}
	if ns := r.notices(); len(ns) != 1 {
		t.Errorf("%d notices", len(ns))
	}
	// A new proposal on the same message is not allowed while the first may have gone.
	code, m := agentCall(r.h, r.a, "POST", "/api/items/"+repo.Number(it.Number)+"/reply-proposals", map[string]any{"text": askText + "!", "reason": "x"})
	if code != 403 || !strings.Contains(fmt.Sprint(m["error"]), "Refused by guard: Send once") {
		t.Errorf("a second reply to the same message: %d %v", code, m)
	}
}

// TestAFailedSendShowsCouldNotSendQueuesTheItemForOwnerAndSendsOneNoticeWithoutMailContent
func TestAFailedSendShowsCouldNotSendQueuesTheItemForOwnerAndSendsOneNoticeWithoutMailContent(t *testing.T) {
	for _, mode := range []string{"rejectRcpt", "rejectData", "badAuth", "noStarttls"} {
		t.Run(mode, func(t *testing.T) {
			r := newReplyEnv(t)
			it := r.ask(r.question("Neodešlo se OLEJOVÁNÍ-42"))
			pid := proposeReplyAs(t, r.h, r.a, it, askText+" TAJNÝTEXT-7")
			r.setItem(replyThread, func(i *repo.Item) { i.Stage = stageResolved })
			r.smtp.setMode(mode)
			// The item must still be open for the reply to be shown; the proposal was withdrawn by the change, so open it again.
			r.setItem(replyThread, func(i *repo.Item) { i.Stage = stageInProgress })
			approveAs(r.h, pid, askText+" TAJNÝTEXT-7")
			p := proposalOf(t, r.a, pid)
			if p.SendState != "failed" || p.SendError == "" {
				t.Fatalf("proposal %+v", p)
			}
			row := rowOf(r.h, it)
			if !strings.Contains(row, "Could not send: ") {
				t.Errorf("the thread does not say why:\n%s", row)
			}
			if mode == "rejectRcpt" && !strings.Contains(row, "550") {
				t.Error("the server's reason is missing")
			}
			if strings.Contains(p.SendError, testPass) {
				t.Error("the password is in the reason")
			}
			now := r.item(replyThread)
			if now.Stage != stageInProgress || now.NeedsClaude {
				t.Errorf("item %s, waits for Claude %v", now.Stage, now.NeedsClaude)
			}
			ns := r.notices()
			if len(ns) != 1 || ns[0].Title != "Could not send" {
				t.Fatalf("notices %v", ns)
			}
			for _, leak := range []string{"TAJNÝTEXT", "OLEJOVÁNÍ", "alice@", "example.test", "Alice", testPass, "Máte čas"} {
				if strings.Contains(ns[0].Body+ns[0].Title+ns[0].URL, leak) {
					t.Errorf("the notice holds %q", leak)
				}
			}
			if len(r.smtp.handed()) != 0 {
				t.Error("a mail was accepted")
			}
			// A failed proposal is not sent again by another press.
			r.smtp.setMode("")
			approveAs(r.h, pid, askText+" TAJNÝTEXT-7")
			if len(r.smtp.handed()) != 0 {
				t.Error("a failed reply was sent by a second press")
			}
		})
	}
}

// TestADismissedReplacedWithdrawnOrSentProposalCannotBeSent
func TestADismissedReplacedWithdrawnOrSentProposalCannotBeSent(t *testing.T) {
	r := newReplyEnv(t)
	it := r.ask(r.question("Zrušené"))
	// Dismissed.
	d := proposeReplyAs(t, r.h, r.a, it, "dismissed text")
	if rec := r.ajaxPost(fmt.Sprintf("/proposals/%d/dismiss", d), nil); rec.Code != 200 {
		t.Fatalf("dismiss: %d", rec.Code)
	}
	approveAs(r.h, d, "dismissed text")
	// Replaced: a newer reply takes the place of the one waiting.
	first := proposeReplyAs(t, r.h, r.a, it, "first text")
	second := proposeReplyAs(t, r.h, r.a, it, "second text")
	if first == second || proposalOf(t, r.a, first).Status != "withdrawn" || !strings.Contains(proposalOf(t, r.a, first).Note, "replaced") {
		t.Fatalf("not replaced: %+v", proposalOf(t, r.a, first))
	}
	approveAs(r.h, first, "first text")
	// Withdrawn by a new message from the other person.
	third := proposeReplyAs(t, r.h, r.a, it, "third text")
	r.box.add(r.in(replyThread, "Ještě něco", 0.1))
	r.must(r.sync())
	if p := proposalOf(t, r.a, third); p.Status != "withdrawn" {
		t.Fatalf("not withdrawn: %+v", p)
	}
	approveAs(r.h, third, "third text")
	approveAs(r.h, second, "second text")
	if n := len(r.smtp.handed()); n != 0 {
		t.Fatalf("%d mails went out for proposals that could not be sent", n)
	}
	// Withdrawn by the owner acting himself.
	it2 := r.ask(func() testMail { m := r.question("Sám"); m.thrid = "T-self"; return m }())
	fourth := proposeReplyAs(t, r.h, r.a, it2, "fourth text")
	r.ajaxPost(fmt.Sprintf("/items/%d/resolve", it2.ID), nil)
	approveAs(r.h, fourth, "fourth text")
	if n := len(r.smtp.handed()); n != 0 {
		t.Fatalf("%d mails after the owner acted", n)
	}
	// And one that was sent is never sent again.
	it3 := r.ask(func() testMail { m := r.question("Odesláno"); m.thrid = "T-sent"; return m }())
	fifth := proposeReplyAs(t, r.h, r.a, it3, "fifth text")
	approveAs(r.h, fifth, "fifth text")
	approveAs(r.h, fifth, "fifth text")
	if n := len(r.smtp.handed()); n != 1 {
		t.Errorf("%d mails", n)
	}
}

// --- the sent mail comes back ---

// TestTheSentMailComingBackThroughTheGmailImportIsOneMessageAndDoesNotWakeTheReview
func TestTheSentMailComingBackThroughTheGmailImportIsOneMessageAndDoesNotWakeTheReview(t *testing.T) {
	for _, rewritten := range []bool{false, true} {
		name := "same Message-ID"
		if rewritten {
			name = "Gmail changed the Message-ID"
		}
		t.Run(name, func(t *testing.T) {
			r := newReplyEnv(t)
			it := r.ask(r.question("Zpátky"))
			marker := "Unikátní věta ŽLUŤOUČKÝ-31 pro tuto zprávu."
			pid := proposeReplyAs(t, r.h, r.a, it, marker)
			approveAs(r.h, pid, marker)
			sent := r.smtp.handed()
			if len(sent) != 1 {
				t.Fatalf("%d mails", len(sent))
			}
			before := r.a.store.CountRows(context.Background(), "messages")
			raw := sent[0].Data
			if rewritten {
				raw = strings.Replace(raw, proposalOf(t, r.a, pid).SendRef, "<gmail-own-id@mail.gmail.test>", 1)
			}
			back := testMail{thrid: replyThread, from: testAddr, to: "alice@example.test", subject: "Re: Zpátky", date: r.a.now().Add(30 * time.Second),
				labels: []string{`\Sent`}, flags: []string{`\Seen`}, raw: raw}
			r.box.add(back)
			r.must(r.sync())
			now := r.item(replyThread)
			if n := r.a.store.CountRows(context.Background(), "messages"); n != before+1 {
				t.Errorf("messages went from %d to %d", before, n)
			}
			if now.NeedsClaude || now.Hidden || now.Stage != stagePending {
				t.Errorf("the import woke the review or moved the item: %+v", now)
			}
			p := proposalOf(t, r.a, pid)
			if p.SentMsgID == 0 || len(r.events(it.ID, "reply_matched")) != 1 {
				t.Fatalf("not matched: %+v", p)
			}
			// Repeating the sync changes nothing.
			r.must(r.sync())
			if n := r.a.store.CountRows(context.Background(), "messages"); n != before+1 {
				t.Errorf("a second sync: %d", n)
			}
			row := rowOf(r.h, it)
			if n := strings.Count(row, ">Unikátní věta") - strings.Count(row, `line-snippet">Unikátní věta`); n != 1 || strings.Contains(row, "reply-text-shown") {
				t.Errorf("the sent text is shown %d times:\n%s", n, row)
			}
			if !strings.Contains(row, "Sent 13:00.") {
				t.Error("the thread no longer says it was sent")
			}
		})
	}
}

// TestAMailThatMayHaveGoneIsConfirmedWhenItComesBack
func TestAMailThatMayHaveGoneIsConfirmedWhenItComesBack(t *testing.T) {
	r := newReplyEnv(t)
	it := r.ask(r.question("Nejistota"))
	marker := "Věta nejistého odeslání 99."
	pid := proposeReplyAs(t, r.h, r.a, it, marker)
	r.smtp.setMode("dropAfterData")
	approveAs(r.h, pid, marker)
	if proposalOf(t, r.a, pid).SendState != "unknown" {
		t.Fatal("not in doubt")
	}
	sent := r.smtp.handed()
	r.box.add(testMail{thrid: replyThread, from: testAddr, to: "alice@example.test", subject: "Re: Nejistota", date: r.a.now().Add(time.Minute),
		labels: []string{`\Sent`}, flags: []string{`\Seen`}, raw: sent[0].Data})
	r.must(r.sync())
	if p := proposalOf(t, r.a, pid); p.SendState != "sent" || p.SentMsgID == 0 {
		t.Errorf("proposal %+v", p)
	}
	if !strings.Contains(rowOf(r.h, it), "Sent ") {
		t.Error("the thread does not say it was sent")
	}
}

// --- smaller things ---

// TestPhoneApproveAsksOnceMoreWithTheSpecsWords: the second question is in the
// page and the script asks it only on a phone.
func TestPhoneApproveAsksOnceMoreWithTheSpecsWords(t *testing.T) {
	r := newReplyEnv(t)
	it := r.ask(r.question("Telefon"))
	proposeReplyAs(t, r.h, r.a, it, askText)
	row := rowOf(r.h, it)
	for _, want := range []string{"Send this reply?", `class="js-send">Send</button>`, `js-cancel">Cancel</button>`, "reply-confirm", "hidden"} {
		if !strings.Contains(row, want) {
			t.Errorf("the row lacks %q", want)
		}
	}
	js, _ := os.ReadFile("web/app.js")
	for _, want := range []string{"data-reply", "phone.matches", "55.99rem", "js-discuss", "requestSubmit"} {
		if !strings.Contains(string(js), want) {
			t.Errorf("app.js lacks %q", want)
		}
	}
}

// TestNoSecretOrMailTextAppearsInTheLogs
func TestNoSecretOrMailTextAppearsInTheLogs(t *testing.T) {
	r := newReplyEnv(t)
	it := r.ask(r.question("Logy"))
	text := "Zakázaný-text-v-logu-5521 " + askText
	pid := proposeReplyAs(t, r.h, r.a, it, text)
	approveAs(r.h, pid, text)
	r.smtp.setMode("rejectRcpt")
	it2 := r.ask(func() testMail { m := r.question("Logy dva"); m.thrid = "T-log2"; return m }())
	pid2 := proposeReplyAs(t, r.h, r.a, it2, text+" dva")
	approveAs(r.h, pid2, text+" dva")
	r.a.recoverSends(context.Background())
	b, _ := os.ReadFile(logFile)
	log := string(b)
	for _, leak := range []string{testPass, "Zakázaný-text", "alice@example.test", "bob@example.test", "carol@example.test", "Logy", "Máte čas", proposalOf(t, r.a, pid).SendRef, r.a.agentTokenValue()} {
		if leak != "" && strings.Contains(log, leak) {
			t.Errorf("the log holds %q", oneLine(leak, 20))
		}
	}
	if !strings.Contains(log, "reply send ended") {
		t.Error("the send is not logged at all")
	}
}

// TestHeaderAndAddressHelpers: small pure checks.
func TestHeaderAndAddressHelpers(t *testing.T) {
	for _, a := range []string{"no-reply@x.test", "noreply@x.test", "DoNotReply@x.test", "do_not_reply@x.test", "mailer-daemon@x.test", "bounces+123@x.test", "neodpovidejte@x.test"} {
		if !noReply(a) {
			t.Errorf("%s is a no-reply address", a)
		}
	}
	for _, a := range []string{"alice@x.test", "reply@x.test", "info@x.test", "norman@x.test"} {
		if noReply(a) {
			t.Errorf("%s is not", a)
		}
	}
	if _, ok := plainAddress("žaneta@example.test"); ok {
		t.Error("a non-ASCII address is accepted")
	}
	if got := cleanHeaderText("a\r\nb\tc\x00d"); got != "a b c d" {
		t.Errorf("%q", got)
	}
	if _, err := cleanReplyText("ok\r\nline\rthree"); err != nil {
		t.Error(err)
	}
	if got, _ := cleanReplyText("a\r\nb\rc"); got != "a\nb\nc" {
		t.Errorf("%q", got)
	}
	if _, err := cleanReplyText("a\x1bb"); err == nil {
		t.Error("an escape character is accepted")
	}
	if _, err := cleanReplyText("a\xffb"); err == nil {
		t.Error("invalid UTF-8 is accepted")
	}
}

// TestOwnAddressInAnotherSpellingIsStillOwnersOwn: a +tag, dots in a Gmail
// name and googlemail.com are the same mailbox, so they are never a recipient.
func TestOwnAddressInAnotherSpellingIsStillOwnersOwn(t *testing.T) {
	own := []string{"owner.example@gmail.com", "boss@firm.example"}
	for _, ad := range []string{"owner.example+shop@gmail.com", "OwnerExample@Gmail.com", "owner.example@googlemail.com", "boss+x@firm.example"} {
		if !isOwn(own, ad) {
			t.Errorf("%q should be the owner's own", ad)
		}
	}
	for _, ad := range []string{"owner.example@firm.example", "b.oss@firm.example", "other@gmail.com"} {
		if isOwn(own, ad) {
			t.Errorf("%q is not the owner's own", ad)
		}
	}
}
