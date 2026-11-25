package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"lab/apps/one/repo"
)

// SPEC 10, "One matter can cross mail and chat" (the owner, 2026-11-21). A chat and
// a mail are joined only by a proposal on the owner's Approve; after that the one
// row holds both, replies go to the channel of the newest message, and a split
// puts everything back. Everything is invented.

type crossEnv struct{ *rEnv }

func newCrossEnv(t *testing.T) *crossEnv {
	t.Helper()
	e := &crossEnv{newReplyEnv(t)}
	t.Cleanup(func() { crossChannelJoin = true })
	return e
}

func (e *crossEnv) chatAt(minutesAgo float64) string {
	return e.a.now().Add(-time.Duration(minutesAgo * float64(time.Minute))).UTC().Format(time.RFC3339)
}

// chatIn posts a chat with messages from someone else as the bridge does.
func (e *crossEnv) chatIn(id, name string, msgs ...[3]any) {
	e.t.Helper()
	var bm []bridgeMessage
	for _, m := range msgs {
		bm = append(bm, bridgeMessage{ID: m[0].(string), Sender: "p." + id, SenderName: name, At: e.chatAt(m[2].(float64)), Text: m[1].(string)})
	}
	c := bridgeChat{Network: "whatsapp", ChatID: id, Name: name, Type: "direct", Messages: bm,
		Participants: []repo.Participant{{Handle: "me-" + id, Name: "the owner", Self: true}, {Handle: "p." + id, Name: name}}}
	b, _ := json.Marshal(bridgeBatch{Chats: []bridgeChat{c}})
	req := httptest.NewRequest("POST", "/api/bridge/batch", bytes.NewReader(b))
	req.Header.Set("Authorization", "Bearer "+e.a.bridgeTokenValue())
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		e.t.Fatalf("batch: %d %s", rec.Code, rec.Body.String())
	}
}

func (e *crossEnv) chatItem(id string) repo.Item {
	e.t.Helper()
	it, ok := e.a.store.ItemByLink(e.ctx, chatLinkKind, repo.ChatKey("whatsapp", id))
	if !ok {
		e.t.Fatalf("no item for chat %s", id)
	}
	return it
}

func (e *crossEnv) fresh(it repo.Item) repo.Item {
	e.t.Helper()
	got, ok := e.a.store.ItemByID(e.ctx, it.ID)
	if !ok {
		e.t.Fatal("no item")
	}
	return got
}

// join proposes the join as Claude and approves it as the owner.
func (e *crossEnv) join(keep, drop repo.Item) {
	e.t.Helper()
	code, m := agentCall(e.h, e.a, "POST", "/api/items/"+ref(keep)+"/merge-proposals", map[string]any{"join": ref(drop), "reason": "Same supplier, same order.", "agent": "Claude"})
	if code != 200 || m["ok"] != true {
		e.t.Fatalf("propose merge: %d %v", code, m)
	}
	pid := int64(m["proposal_id"].(float64))
	if rec := e.ajaxPost(fmt.Sprintf("/proposals/%d/approve", pid), nil); rec.Code != 200 {
		e.t.Fatalf("approve: %d %s", rec.Code, rec.Body.String())
	}
}

func (e *crossEnv) messages(id int64) []repo.Message {
	e.t.Helper()
	ms, _, err := e.a.store.MessagesSince(e.ctx, id, 0)
	if err != nil {
		e.t.Fatal(err)
	}
	return ms
}

func (e *crossEnv) sources(id int64) string {
	var out []string
	for _, m := range e.messages(id) {
		out = append(out, m.Network)
	}
	return strings.Join(out, ",")
}

// mixed makes a mail row and a chat row that are about the same matter.
func (e *crossEnv) mixed() (mailRow, chatRow repo.Item) {
	e.t.Helper()
	m := e.question("Order 77 delivery")
	m.thrid = "T-cross-1"
	e.box.add(m)
	e.must(e.importMonth())
	e.chatIn("sup", "Supplier Sam", [3]any{"c1", "Order 77 leaves on Monday.", 600.0})
	claudeLooksAtAll(e.t, e.a)
	return e.item("T-cross-1"), e.chatItem("sup")
}

func TestSpec10NothingJoinsAChatToAMailByItself(t *testing.T) {
	e := newCrossEnv(t)
	// A pattern that reads the order number in both, the same person on both
	// channels, the same subject and name: none of it joins a chat to a mail.
	post(e.h, "/settings/matter-patterns", url.Values{"name": {"Order"}, "pattern": {`Order ([0-9]+)`}})
	m := e.question("Order 77 delivery")
	m.thrid = "T-auto-1"
	e.box.add(m)
	e.must(e.importMonth())
	if err := e.a.store.Update(e.ctx, func(tx *repo.Tx) error {
		// The same person has the mail address and the chat handle.
		_, err := tx.EnsurePerson(e.ctx, "mail", "alice@example.test", "Alice Example", 1)
		if err != nil {
			return err
		}
		_, err = tx.EnsurePerson(e.ctx, "whatsapp", "p.sup", "Alice Example", 1)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ { // the bridge, twice
		e.chatIn("sup", "Alice Example", [3]any{"c1", "Order 77 leaves on Monday.", 90.0})
	}
	e.box.add(e.in("T-auto-2", "Order 77 invoice", 0.2)) // the importer, once more, after the chat
	e.must(e.importMonth())
	mailRow, chatRow := e.item("T-auto-1"), e.chatItem("sup")
	if mailRow.ID == chatRow.ID {
		t.Fatal("a chat joined a mail by itself")
	}
	if mailRow.Kind != "thread" || chatRow.Kind != "chat" {
		t.Errorf("kinds: %q %q", mailRow.Kind, chatRow.Kind)
	}
	if e.sources(mailRow.ID) != "mail,mail" && e.sources(mailRow.ID) != "mail" {
		t.Errorf("the mail row holds %s", e.sources(mailRow.ID))
	}
	if e.sources(chatRow.ID) != "whatsapp" {
		t.Errorf("the chat row holds %s", e.sources(chatRow.ID))
	}
	if strings.Contains(rowOf(e.h, chatRow), "msg-src") || strings.Contains(rowOf(e.h, mailRow), "msg-src") {
		t.Error("a row of one source marks its messages")
	}
	if links, _ := e.a.store.LinksOfItem(e.ctx, chatRow.ID); len(links) != 1 {
		t.Errorf("the chat row has links %v: a matter link would let mail in", links)
	}
	// An approved pattern proposal makes no difference for the chat either.
	if err := e.a.store.Update(e.ctx, func(tx *repo.Tx) error {
		_, err := tx.InsertMatterProposal(e.ctx, repo.MatterProposal{Name: "Ord", Pattern: `Order ([0-9]+)`, Reason: "x", ProposedBy: "claude", ProposedAt: 1})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	e.chatIn("sup", "Alice Example", [3]any{"c2", "Order 77 again", 0.1})
	if e.chatItem("sup").ID != chatRow.ID || e.fresh(chatRow).Kind != "chat" || e.fresh(mailRow).Kind != "thread" {
		t.Error("something moved a row")
	}
	if n := e.a.store.CountRows(e.ctx, "items"); n != 3 { // the mail row, the second mail's row (no pattern joined it) and the chat
		t.Logf("rows: %d", n)
	}
	// A proposal alone changes nothing either.
	code, pm := agentCall(e.h, e.a, "POST", "/api/items/"+ref(mailRow)+"/merge-proposals", map[string]any{"join": ref(chatRow), "reason": "Same order."})
	if code != 200 || e.a.store.MergedInto(e.ctx, chatRow.ID) != 0 || e.sources(chatRow.ID) == "" {
		t.Fatalf("a proposal joined: %d %v", code, pm)
	}
}

func TestSpec10AJoinedChatAndMailShowInOneRowInTimeOrderAndNewMessagesLandThere(t *testing.T) {
	e := newCrossEnv(t)
	mailRow, chatRow := e.mixed()
	e.join(mailRow, chatRow)
	row := e.fresh(mailRow)
	if row.Kind != "chat" || row.Hidden {
		t.Errorf("the row: kind %q hidden %v", row.Kind, row.Hidden)
	}
	if got := e.sources(row.ID); got != "whatsapp,mail" { // the chat is older
		t.Errorf("messages in time order: %s", got)
	}
	if it, err := e.a.findItem(e.ctx, e.a.store.Q, ref(chatRow)); err != nil || it.ID != row.ID {
		t.Errorf("the number of the joined chat leads to %v %v", it.ID, err)
	}
	page := e.ajaxPost(fmt.Sprintf("/items/%d/row?open=1", row.ID), nil)
	_ = page
	open := rowOf(e.h, row)
	for _, want := range []string{"Claude joined " + ref(chatRow) + " into this row.", "Order 77 leaves on Monday.", "WhatsApp", `class="msg-src">Gmail<`, `class="msg-src">WhatsApp<`, "Write in WhatsApp…"} {
		if !strings.Contains(open, want) {
			t.Errorf("the open row lacks %q", want)
		}
	}
	// A new chat message lands in the row, a new mail too, and each moves it.
	e.chatIn("sup", "Supplier Sam", [3]any{"c2", "Sorry, Tuesday.", 1.0})
	if e.chatItem("sup").ID != row.ID {
		t.Fatal("a new chat message made a row of its own")
	}
	late := e.in("T-cross-1", "Order 77 delivery", 0)
	late.date = e.a.now()
	e.box.add(late)
	e.must(e.importMonth())
	if e.item("T-cross-1").ID != row.ID {
		t.Fatal("a new mail made a row of its own")
	}
	if n := len(e.messages(row.ID)); n < 4 {
		t.Errorf("the row holds %d messages", n)
	}
	if e.fresh(row).MovedAt < e.a.now().Add(-2*time.Minute).Unix() {
		t.Error("new messages did not move the row")
	}
	if e.a.store.CountRows(e.ctx, "items") != 2 {
		t.Error("a message made a row")
	}
}

func TestSpec10AJoinedChatKeepsItsQuietTimeAndAPhoneListsTheRow(t *testing.T) {
	e := newCrossEnv(t)
	mailRow, chatRow := e.mixed()
	e.join(mailRow, chatRow)
	row := e.fresh(mailRow)
	// A phone lists a joined row (a chat row alone, with nothing grown from it, is not listed).
	e.chatIn("lonely", "Lone Lee", [3]any{"l1", "hi", 5.0})
	phone := func(path string) string {
		req := httptest.NewRequest("GET", path, nil)
		req.AddCookie(&http.Cookie{Name: "one-phone", Value: "1"})
		rec := httptest.NewRecorder()
		e.h.ServeHTTP(rec, req)
		return rec.Body.String()
	}
	_ = phone
	ctx := repo.WithPhone(e.ctx)
	rows, err := e.a.store.ListItems(ctx, stageNew, 50, 0)
	if err != nil {
		t.Fatal(err)
	}
	listed := map[int64]bool{}
	for _, r := range rows {
		listed[r.ID] = true
	}
	lonely := e.chatItem("lonely")
	if !listed[row.ID] {
		t.Error("a phone does not list the joined row")
	}
	if listed[lonely.ID] {
		t.Error("a phone lists a chat nothing grew from")
	}
	// A chat message still going on holds the chat for the quiet time, in the joined row too.
	claudeLooksAtAll(t, e.a)
	e.chatIn("sup", "Supplier Sam", [3]any{"c9", "One more thing", 1.0})
	got := e.fresh(row)
	if got.ChatSince == 0 || got.NeedsClaude {
		t.Errorf("quiet time: chat_since %d, needs claude %v", got.ChatSince, got.NeedsClaude)
	}
	e.skew.Add(31 * 60)
	e.a.releaseChats(e.ctx)
	if got := e.fresh(row); got.ChatSince != 0 || !got.NeedsClaude {
		t.Errorf("after the quiet time: chat_since %d, needs claude %v", got.ChatSince, got.NeedsClaude)
	}
	if e.fresh(chatRow).NeedsClaude || e.fresh(chatRow).ChatSince != 0 {
		t.Error("the joined-away row waits for Claude as well")
	}
}

func TestSpec10ASplitPutsTheChatAndTheMailBackAsTwoRowsWithNothingLost(t *testing.T) {
	e := newCrossEnv(t)
	mailRow, chatRow := e.mixed()
	e.chatIn("sup", "Supplier Sam", [3]any{"c2", "Second chat line", 30.0})
	// The owner's own note and Claude's comment belong to a row, not to a thread.
	e.must(e.a.store.Update(e.ctx, func(tx *repo.Tx) error {
		return tx.InsertEvent(e.ctx, e.a.now().Unix(), chatRow.ID, "claude", "commented", map[string]any{"text": "chat comment", "kind": "comment"})
	}))
	count := func() map[string]int {
		out := map[string]int{}
		for _, t := range []string{"messages", "files", "chats", "chat_out", "item_links", "notes", "events", "proposals"} {
			out[t] = e.a.store.CountRows(e.ctx, t)
		}
		return out
	}
	before := count()
	beforeMail, beforeChat := len(e.messages(mailRow.ID)), len(e.messages(chatRow.ID))
	e.join(mailRow, chatRow)
	// Split the chat off again, by proposal.
	row := e.fresh(mailRow)
	var chatMsg repo.Message
	for _, m := range e.messages(row.ID) {
		if m.Network != "mail" {
			chatMsg = m
		}
	}
	code, m := agentCall(e.h, e.a, "POST", "/api/items/"+ref(row)+"/split-proposals", map[string]any{"message": chatMsg.ID, "reason": "Different matters."})
	if code != 200 {
		t.Fatalf("split proposal: %d %v", code, m)
	}
	if len(e.messages(row.ID)) != beforeMail+beforeChat {
		t.Fatal("a proposal moved messages")
	}
	if rec := e.ajaxPost(fmt.Sprintf("/proposals/%d/approve", int64(m["proposal_id"].(float64))), nil); rec.Code != 200 {
		t.Fatalf("approve: %d", rec.Code)
	}
	chatNow, mailNow := e.chatItem("sup"), e.item("T-cross-1")
	if chatNow.ID == mailNow.ID {
		t.Fatal("still one row")
	}
	if len(e.messages(mailNow.ID)) != beforeMail || len(e.messages(chatNow.ID)) != beforeChat {
		t.Errorf("messages: mail %d (want %d), chat %d (want %d)", len(e.messages(mailNow.ID)), beforeMail, len(e.messages(chatNow.ID)), beforeChat)
	}
	if e.fresh(chatNow).Kind != "chat" || e.fresh(mailNow).Kind != "thread" {
		t.Errorf("kinds after the split: %q %q", e.fresh(chatNow).Kind, e.fresh(mailNow).Kind)
	}
	if c, ok := e.a.store.ChatOfItem(e.ctx, chatNow.ID); !ok || c.Key != repo.ChatKey("whatsapp", "sup") {
		t.Error("the chat did not go with its row")
	}
	if _, ok := e.a.store.ChatOfItem(e.ctx, mailNow.ID); ok {
		t.Error("the mail row still holds a chat")
	}
	after := count()
	for table, n := range before {
		// The two proposals and their decisions, and the notices, are the only new records.
		if table == "events" || table == "proposals" {
			if after[table] < n {
				t.Errorf("%s lost records: %d -> %d", table, n, after[table])
			}
			continue
		}
		if after[table] != n {
			t.Errorf("%s: %d before, %d after", table, n, after[table])
		}
	}
	if !strings.Contains(rowOf(e.h, e.fresh(chatNow)), "split") || !strings.Contains(rowOf(e.h, e.fresh(mailNow)), "split") {
		t.Error("a row lacks its notice")
	}
	// Each row takes new messages of its own thread again.
	e.chatIn("sup", "Supplier Sam", [3]any{"c3", "Back to normal", 0.5})
	if e.chatItem("sup").ID != chatNow.ID {
		t.Error("the chat lands elsewhere after the split")
	}
}

func TestSpec10ARepliesOnAMixedRowGoToTheChannelOfTheNewestMessageAndTheGuardsLookAtThatThread(t *testing.T) {
	e := newCrossEnv(t)
	// The mail side is a bank; the chat side a person.
	m := e.question("Váš účet")
	m.thrid, m.from = "T-bank", "Banka <info@unicreditbank.cz>"
	e.box.add(m)
	e.must(e.importMonth())
	e.chatIn("friend", "Friend Fia", [3]any{"f1", "Did the bank write to you?", 3000.0})
	claudeLooksAtAll(t, e.a)
	bank, friend := e.item("T-bank"), e.chatItem("friend")
	e.join(friend, bank) // the chat row stays, the bank mail joins it
	row := e.fresh(friend)
	e.must(e.a.store.Update(e.ctx, func(tx *repo.Tx) error { return tx.SetItemStage(e.ctx, row.ID, stageInProgress, e.a.now().Unix()) }))
	row = e.fresh(row)
	propose := func(text string) (int, map[string]any) {
		return agentCall(e.h, e.a, "POST", "/api/items/"+ref(row)+"/reply-proposals", map[string]any{"text": text, "reason": "x"})
	}
	// Newest is the bank's mail: a mail reply is refused, by the bank's thread.
	if code, m := propose("Díky"); code != 403 || !strings.Contains(fmt.Sprint(m["error"]), "Refused by guard: Blocked targets") {
		t.Fatalf("a reply to the bank: %d %v", code, m)
	}
	// A newer chat message: the reply goes to the chat, the bank on the other thread does not block it.
	e.chatIn("friend", "Friend Fia", [3]any{"f2", "Hello?", 0.2})
	code, pm := propose("Ahoj, jo.")
	if code != 200 {
		t.Fatalf("a chat reply on the mixed row: %d %v", code, pm)
	}
	pid := int64(pm["proposal_id"].(float64))
	p := proposalOf(t, e.a, pid)
	if p.Action != replyChat {
		t.Fatalf("the reply is a %q", p.Action)
	}
	open := rowOf(e.h, row)
	if !strings.Contains(open, "Will be sent · reply in WhatsApp") || !strings.Contains(open, "Friend Fia") {
		t.Error("the reply box does not name the channel and whom it goes to")
	}
	if strings.Contains(open, "reply to the mail") {
		t.Error("the box names the mail channel")
	}
	if rec := approveAs(e.h, pid, "Ahoj, jo."); rec.Code != 200 {
		t.Fatalf("approve: %d", rec.Code)
	}
	if p := proposalOf(t, e.a, pid); p.SendState != "queued" {
		t.Fatalf("send state %q", p.SendState)
	}
	replies, _ := (&chatEnv{agentEnv: &agentEnv{&listEnv{t: t, a: e.a, h: e.h, ctx: e.ctx, now: e.a.now()}}}).outbox()
	if len(replies) != 1 || replies[0]["chat_id"] != "friend" {
		t.Errorf("handed to the bridge: %v", replies)
	}
	// Send once, per message: the chat message that was answered is not answered again.
	if code, m := propose("Nochmal"); code != 403 || !strings.Contains(fmt.Sprint(m["error"]), "Refused by guard: Send once") {
		t.Errorf("a second reply to the same message: %d %v", code, m)
	}
	// A new mail on the other thread withdraws what waits (the row changed); the bank blocks a mail reply again.
	e.chatIn("friend", "Friend Fia", [3]any{"f3", "And?", 0.05})
	if code, m := propose("Zítra"); code != 200 {
		t.Errorf("a reply to a newer chat message: %d %v", code, m)
	}
}

func TestSpec10AMailReplyOnAMixedRowGoesThroughTheMailPath(t *testing.T) {
	e := newCrossEnv(t)
	e.chatIn("sup", "Supplier Sam", [3]any{"c1", "Order 77 leaves on Monday.", 3000.0})
	m := e.question("Order 77 delivery")
	m.thrid = "T-cross-2"
	e.box.add(m)
	e.must(e.importMonth())
	claudeLooksAtAll(t, e.a)
	mailRow, chatRow := e.item("T-cross-2"), e.chatItem("sup")
	e.join(mailRow, chatRow)
	row := e.fresh(mailRow)
	e.must(e.a.store.Update(e.ctx, func(tx *repo.Tx) error { return tx.SetItemStage(e.ctx, row.ID, stageInProgress, e.a.now().Unix()) }))
	pid := proposeReplyAs(t, e.h, e.a, row, askText)
	if p := proposalOf(t, e.a, pid); p.Action != replyGmail {
		t.Fatalf("action %q", p.Action)
	}
	if open := rowOf(e.h, row); !strings.Contains(open, "Will be sent · reply to the mail") || !strings.Contains(open, "alice@example.test") {
		t.Error("the box does not name the mail channel and whom it goes to")
	}
	if rec := approveAs(e.h, pid, askText); rec.Code != 200 {
		t.Fatalf("approve: %d", rec.Code)
	}
	if len(e.smtp.handed()) != 1 {
		t.Errorf("mails sent: %d", len(e.smtp.handed()))
	}
	if p := proposalOf(t, e.a, pid); p.SendState != "sent" {
		t.Errorf("send state %q", p.SendState)
	}
}

func TestSpec10AOwnersOwnBoxWritesToTheChatOfTheNewestChatMessageWhenARowHoldsTwoChats(t *testing.T) {
	e := newCrossEnv(t)
	e.chatIn("one", "Chat One", [3]any{"a1", "first", 100.0})
	e.chatIn("two", "Chat Two", [3]any{"b1", "second", 50.0})
	one, two := e.chatItem("one"), e.chatItem("two")
	e.join(one, two)
	row := e.fresh(one)
	if c, ok := e.a.store.ChatOfItem(e.ctx, row.ID); !ok || c.ChatID != "two" {
		t.Fatalf("the box writes to %v", c.ChatID)
	}
	e.chatIn("one", "Chat One", [3]any{"a2", "later", 5.0})
	if c, _ := e.a.store.ChatOfItem(e.ctx, row.ID); c.ChatID != "one" {
		t.Errorf("after a newer message in chat one the box writes to %v", c.ChatID)
	}
}

func TestSpec10TheCrossChannelSwitchRefusesAsBeforeAndLeavesJoinedRowsAlone(t *testing.T) {
	e := newCrossEnv(t)
	mailRow, chatRow := e.mixed()
	e.chatIn("other", "Other", [3]any{"o1", "hi", 800.0})
	other := e.chatItem("other")
	e.join(mailRow, chatRow)
	crossChannelJoin = false
	// A waiting proposal made while the switch was on is withdrawn at Approve; new ones are refused.
	code, m := agentCall(e.h, e.a, "POST", "/api/items/"+ref(mailRow)+"/merge-proposals", map[string]any{"join": ref(other), "reason": "x"})
	if code != 409 || !strings.Contains(fmt.Sprint(m["error"]), "Only rows of mail are joined. A chat is one row already.") {
		t.Errorf("with the switch off: %d %v", code, m)
	}
	// The row joined before stays as it is, and it can still be split.
	row := e.fresh(mailRow)
	if e.sources(row.ID) != "whatsapp,mail" || row.Hidden {
		t.Errorf("the joined row: %s hidden %v", e.sources(row.ID), row.Hidden)
	}
	var chatMsg repo.Message
	for _, m := range e.messages(row.ID) {
		if m.Network != "mail" {
			chatMsg = m
		}
	}
	if code, m := agentCall(e.h, e.a, "POST", "/api/items/"+ref(row)+"/split-proposals", map[string]any{"message": chatMsg.ID, "reason": "x"}); code != 200 {
		t.Errorf("a split with the switch off: %d %v", code, m)
	}
	// Two mails are joined as ever.
	m2 := e.question("Another")
	m2.thrid = "T-cross-9"
	e.box.add(m2)
	e.must(e.importMonth())
	if code, _ := agentCall(e.h, e.a, "POST", "/api/items/"+ref(e.item("T-cross-9"))+"/merge-proposals", map[string]any{"join": ref(row), "reason": "x"}); code == 200 {
		t.Log("a row that holds a chat is refused with the switch off, as a chat row was")
	}
}

func TestSpec10ARunCanOnlyJoinRowsThatCarryItsOwnMark(t *testing.T) {
	e := newRunEnv(t)
	runMail := e.a.store
	_ = runMail
	// A real mail row and a chat row of run A: refused, in both directions.
	real := e.seed(stageNew, e.mail("Real order", 3))
	if _, c := e.batchRun(runA, chatOf("whatsapp", "run-1", "Run Chat", "direct", e.in("r1", "p.run", "Run Chat", "Marker-cross hello", 5))); c["ok"] != true {
		t.Fatalf("batch: %v", c)
	}
	chat, ok := e.a.store.ItemByLink(e.ctx, chatLinkKind, repo.ChatKey("whatsapp", "run-1"))
	if !ok || chat.TestRun != runA {
		t.Fatalf("the run's chat: %+v", chat)
	}
	for _, pair := range [][2]repo.Item{{real, chat}, {chat, real}} {
		code, m := e.apiRun(runA, "POST", "/api/items/"+ref(pair[0])+"/merge-proposals", map[string]any{"join": ref(pair[1]), "reason": "x"})
		if code != 403 {
			t.Errorf("a run joined %s with %s: %d %v", ref(pair[0]), ref(pair[1]), code, m)
		}
		if code, m := e.apiRun("", "POST", "/api/items/"+ref(pair[0])+"/merge-proposals", map[string]any{"join": ref(pair[1]), "reason": "x"}); code == 200 {
			t.Errorf("a real call joined a row of a run with a real one: %v", m)
		}
	}
	if msg := mergeMisfit(e.ctx, e.a.store.Q, real, chat); msg == "" {
		t.Error("mergeMisfit lets a real row join a run's chat")
	}
}

func TestSpec10SameThreadLooksAtTheThreadTheReplyGoesToNotAtTheOtherOne(t *testing.T) {
	e := newCrossEnv(t)
	// The mail thread ends with the owner's own mail; the newest message of the row is a chat message
	// from someone else, so the reply goes to the chat although the mail side has nothing to answer.
	m := e.question("Order 77 delivery")
	m.thrid, m.date = "T-cross-3", e.ago(2)
	e.box.add(m)
	e.box.add(e.out("T-cross-3", "Re: Order 77 delivery", 1.5))
	e.must(e.importMonth())
	e.chatIn("sup", "Supplier Sam", [3]any{"c1", "Order 77 leaves on Monday.", 60.0})
	claudeLooksAtAll(t, e.a)
	mailRow, chatRow := e.item("T-cross-3"), e.chatItem("sup")
	e.join(mailRow, chatRow)
	row := e.fresh(mailRow)
	e.must(e.a.store.Update(e.ctx, func(tx *repo.Tx) error { return tx.SetItemStage(e.ctx, row.ID, stageInProgress, e.a.now().Unix()) }))
	pid := proposeReplyAs(t, e.h, e.a, row, "Díky, počkám.")
	if p := proposalOf(t, e.a, pid); p.Action != replyChat {
		t.Errorf("action %q", p.Action)
	}
	// And the other way round: when the newest message is the owner's own mail, nothing waits for an answer.
	e.box.add(e.out("T-cross-3", "Re: Order 77 delivery", 0.01))
	e.must(e.importMonth())
	if code, m := agentCall(e.h, e.a, "POST", "/api/items/"+ref(row)+"/reply-proposals", map[string]any{"text": "x", "reason": "x"}); code != 403 || !strings.Contains(fmt.Sprint(m["error"]), "Same thread") {
		t.Errorf("a reply to the owner's own mail: %d %v", code, m)
	}
}
