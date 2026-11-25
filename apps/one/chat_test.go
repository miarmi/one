package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"lab/apps/one/repo"
)

// Tests of the chat slice (SPEC 4, 10, 13, 16, 17, 21, 24). Everything is
// invented: the bridge is played by these tests, and nothing reaches a real
// Beeper or a real chat. Each test name says what it proves, so it can be cited.

type chatEnv struct {
	*agentEnv
	notes *noticeLog
}

func newChatEnv(t *testing.T) *chatEnv {
	t.Helper()
	e := &chatEnv{agentEnv: newAgentEnv(t), notes: &noticeLog{}}
	e.a.notify = e.notes.add
	e.a.imp.seedRules(e.ctx)
	return e
}

// bridgeCall is a request to the bridge's addresses with the given token.
func (e *chatEnv) bridgeCall(method, path, token string, body any) *httptest.ResponseRecorder {
	var rd *bytes.Reader
	switch b := body.(type) {
	case nil:
		rd = bytes.NewReader(nil)
	case []byte:
		rd = bytes.NewReader(b)
	default:
		j, _ := json.Marshal(body)
		rd = bytes.NewReader(j)
	}
	req := httptest.NewRequest(method, path, rd)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	return rec
}

func (e *chatEnv) asBridge(method, path string, body any) *httptest.ResponseRecorder {
	return e.bridgeCall(method, path, e.a.bridgeTokenValue(), body)
}

func answerOf(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("the answer is not JSON: %q", rec.Body.String())
	}
	return m
}

func (e *chatEnv) at(minutesAgo float64) string {
	return e.now.Add(-time.Duration(minutesAgo * float64(time.Minute))).UTC().Format(time.RFC3339)
}

func chatOf(network, id, name, typ string, msgs ...bridgeMessage) bridgeChat {
	c := bridgeChat{Network: network, ChatID: id, Name: name, Type: typ, Messages: msgs,
		Participants: []repo.Participant{{Handle: "me-" + id, Name: "the owner", Self: true}}}
	return c
}

func (e *chatEnv) in(id, sender, name, text string, minutesAgo float64) bridgeMessage {
	return bridgeMessage{ID: id, Sender: sender, SenderName: name, At: e.at(minutesAgo), Text: text}
}

func (e *chatEnv) mine(id, text string, minutesAgo float64) bridgeMessage {
	return bridgeMessage{ID: id, Sender: "me", At: e.at(minutesAgo), Text: text, Own: true, Read: true}
}

// batch posts chats as the bridge and returns the answer.
func (e *chatEnv) batch(chats ...bridgeChat) map[string]any {
	e.t.Helper()
	rec := e.asBridge("POST", "/api/bridge/batch", bridgeBatch{Chats: chats})
	if rec.Code != 200 {
		e.t.Fatalf("batch: %d %s", rec.Code, rec.Body.String())
	}
	return answerOf(e.t, rec)
}

func (e *chatEnv) itemOfChat(network, chatID string) repo.Item {
	e.t.Helper()
	it, ok := e.a.store.ItemByLink(e.ctx, chatLinkKind, repo.ChatKey(network, chatID))
	if !ok {
		e.t.Fatalf("no item for the chat %s/%s", network, chatID)
	}
	return it
}

func (e *chatEnv) counts() map[string]int {
	out := map[string]int{}
	for _, t := range []string{"raw", "messages", "items", "events", "files", "skipped", "people", "person_handles", "item_links"} {
		out[t] = e.a.store.CountRows(e.ctx, t)
	}
	out["chats"] = e.a.store.CountChats(e.ctx)
	return out
}

func (e *chatEnv) queueIDs() []string {
	rec := e.call("GET", "/api/queue", e.a.agentTokenValue(), nil)
	var out struct {
		Items []struct {
			ID string `json:"id"`
		} `json:"items"`
	}
	json.Unmarshal(rec.Body.Bytes(), &out)
	var ids []string
	for _, i := range out.Items {
		ids = append(ids, i.ID)
	}
	return ids
}

func (e *chatEnv) rowPage(id int64) string {
	e.t.Helper()
	rec := get(e.h, fmt.Sprintf("/items/%d/row?open=1", id))
	if rec.Code != 200 {
		e.t.Fatalf("row %d: %d", id, rec.Code)
	}
	return rec.Body.String()
}

func (e *chatEnv) heardNow() {
	e.asBridge("POST", "/api/bridge/alive", map[string]any{"networks": []bridgeNet{{Network: "whatsapp", State: "connected"}}})
}

// --- who can use which address (SPEC 14, 16) ---

func TestBridgeTokenCannotReadAnythingAndCannotApprove(t *testing.T) {
	e := newChatEnv(t)
	e.batch(chatOf("whatsapp", "c1", "Jana", "direct", e.in("m1", "+420111", "Jana", "Ahoj, máš čas?", 100)))
	it := e.itemOfChat("whatsapp", "c1")
	claudeLooksAtAll(t, e.a)
	tok := e.a.bridgeTokenValue()
	if tok == "" || tok == e.a.agentTokenValue() {
		t.Fatal("the bridge token is missing or is the agents' token")
	}
	num := repo.Number(it.Number)
	for _, c := range []struct{ method, path string }{
		{"GET", "/api/queue"}, {"GET", "/api/items"}, {"GET", "/api/items/" + num}, {"GET", "/api/items/" + num + "/thread"},
		{"GET", "/api/items/" + num + "/events"}, {"GET", "/api/search?q=ahoj"}, {"GET", "/api/people"}, {"GET", "/api/agenda"},
		{"GET", "/api/senders"}, {"GET", "/api/decisions"}, {"POST", "/api/notes"}, {"POST", "/api/items/" + num + "/comments"},
		{"POST", "/api/items/" + num + "/reply-proposals"}, {"POST", "/api/items/" + num + "/skip"}, {"POST", "/api/rule-proposals"},
		{"POST", "/api/people/details"}, {"POST", "/api/proposals/1/approve"}, {"POST", "/api/settings"},
	} {
		rec := e.bridgeCall(c.method, c.path, tok, map[string]any{"text": "x", "reason": "y"})
		if rec.Code != 401 || strings.TrimSpace(rec.Body.String()) != wrongTokenMsg {
			t.Errorf("%s %s with the bridge token: %d %s", c.method, c.path, rec.Code, rec.Body.String())
		}
	}
	// The connector too: a tool call needs the agents' token.
	rec := e.bridgeCall("POST", "/api/mcp", tok, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": "list_review_queue", "arguments": map[string]any{}}})
	if !strings.Contains(rec.Body.String(), "wrong token") {
		t.Errorf("the connector answered the bridge token: %s", rec.Body.String())
	}
	// Nothing under the bridge's own addresses approves, sends or changes a setting.
	before := e.counts()
	for _, path := range []string{"/api/bridge/approve", "/api/bridge/proposals/1/approve", "/api/bridge/settings", "/api/bridge/send", "/api/bridge/guards", "/api/bridge/token"} {
		rec := e.asBridge("POST", path, map[string]any{})
		if rec.Code != 403 || strings.TrimSpace(rec.Body.String()) != onlyOwnerMsg {
			t.Errorf("POST %s: %d %s", path, rec.Code, rec.Body.String())
		}
	}
	if rec := e.asBridge("GET", "/api/bridge/queue", nil); rec.Code != 404 && rec.Code != 405 {
		t.Errorf("an address the bridge does not have answered %d", rec.Code)
	}
	after := e.counts()
	if after["events"] != before["events"]+6 { // each refusal is noted, nothing else
		t.Errorf("refusals noted: %d -> %d", before["events"], after["events"])
	}
	delete(after, "events")
	delete(before, "events")
	if fmt.Sprint(after) != fmt.Sprint(before) {
		t.Errorf("a refused request wrote something: %v -> %v", before, after)
	}
}

func TestAgentTokenCannotUseTheBridgeAddresses(t *testing.T) {
	e := newChatEnv(t)
	agent := e.a.agentTokenValue()
	batch := bridgeBatch{Chats: []bridgeChat{chatOf("whatsapp", "c1", "Jana", "direct", e.in("m1", "+420111", "Jana", "Ahoj", 100))}}
	before := e.counts()
	for _, c := range []struct {
		method, path string
		body         any
	}{
		{"POST", "/api/bridge/batch", batch}, {"POST", "/api/bridge/alive", map[string]any{}}, {"POST", "/api/bridge/outbox", map[string]any{}},
		{"POST", "/api/bridge/outbox/1/result", map[string]any{"state": "sent"}}, {"PUT", "/api/bridge/files?network=whatsapp&chat_id=c1&message_id=m1&attachment=a", []byte("x")},
	} {
		for _, tok := range []string{agent, "", "wrong-" + agent} {
			rec := e.bridgeCall(c.method, c.path, tok, c.body)
			if rec.Code != 401 || strings.TrimSpace(rec.Body.String()) != wrongTokenMsg {
				t.Errorf("%s %s with token set %v: %d %s", c.method, c.path, tok != "", rec.Code, rec.Body.String())
			}
		}
	}
	if after := e.counts(); fmt.Sprint(after) != fmt.Sprint(before) {
		t.Errorf("a refused request wrote something")
	}
	if e.a.bridgeHeardAt(e.ctx) != 0 {
		t.Error("a request with the wrong token counted as the bridge being heard")
	}
}

func TestTheBridgeTokenIsMadeOnceKeptPrivateShownOnConnectorsAndRenewedOnlyByOwner(t *testing.T) {
	e := newChatEnv(t)
	tok := e.a.bridgeTokenValue()
	b, err := os.ReadFile(e.a.secretsDir + "/" + bridgeTokenFile)
	if err != nil || strings.TrimSpace(string(b)) != tok {
		t.Fatalf("the token is not kept in the secrets folder: %v", err)
	}
	if st, _ := os.Stat(e.a.secretsDir + "/" + bridgeTokenFile); st.Mode().Perm() != 0o600 {
		t.Errorf("the token file has mode %v", st.Mode().Perm())
	}
	page := get(e.h, "/connectors").Body.String()
	for _, want := range []string{"Bridge token", "Agent token", "Chat bridge", tok, "New token", ">Copy</button>"} {
		if !strings.Contains(page, want) {
			t.Errorf("the Connectors page lacks %q", want)
		}
	}
	if strings.Count(page, ">Copy</button>") != 3 || strings.Count(page, ">New token</button>") != 3 {
		t.Errorf("each token section has Copy and New token")
	}
	// A new token ends the old one at once.
	if rec := post(e.h, "/connectors/bridge-token/new", nil); rec.Code != 303 {
		t.Fatalf("new token: %d", rec.Code)
	}
	if e.a.bridgeTokenValue() == tok {
		t.Fatal("the token did not change")
	}
	if rec := e.bridgeCall("POST", "/api/bridge/alive", tok, map[string]any{}); rec.Code != 401 {
		t.Errorf("the old token still works: %d", rec.Code)
	}
	if rec := e.asBridge("POST", "/api/bridge/alive", map[string]any{}); rec.Code != 200 {
		t.Errorf("the new token does not work: %d", rec.Code)
	}
	// The agents' token is its own.
	if e.a.agentTokenValue() == e.a.bridgeTokenValue() {
		t.Error("one token for both")
	}
}

// --- chats as items (SPEC 4, 10, 16) ---

func TestABatchPostedTwiceWritesNothingTheSecondTime(t *testing.T) {
	e := newChatEnv(t)
	c := chatOf("messenger", "c1", "Ondra", "direct",
		e.in("m1", "t.1", "Ondra", "Ahoj", 200), e.mine("m2", "Čau", 190), e.in("m3", "t.1", "Ondra", "Zítra v osm?", 180))
	c.Participants = append(c.Participants, repo.Participant{Handle: "t.1", Name: "Ondra"})
	first := e.batch(c)
	if first["stored"].(float64) != 3 {
		t.Fatalf("first batch: %v", first)
	}
	before := e.counts()
	second := e.batch(c)
	if second["stored"].(float64) != 0 || second["versions"].(float64) != 0 || second["unchanged"].(float64) != 3 {
		t.Errorf("second batch: %v", second)
	}
	if after := e.counts(); fmt.Sprint(after) != fmt.Sprint(before) {
		t.Errorf("a batch posted twice wrote again: %v -> %v", before, after)
	}
	if before["items"] != 1 || before["messages"] != 3 || before["raw"] != 3 {
		t.Errorf("counts after the first batch: %v", before)
	}
}

func TestAnEditedChatMessageIsAVersionNotASecondMessage(t *testing.T) {
	e := newChatEnv(t)
	c := chatOf("whatsapp", "c1", "Jana", "direct", e.in("m1", "+420111", "Jana", "Sejdeme se v pět", 300))
	e.batch(c)
	claudeLooksAtAll(t, e.a)
	it := e.itemOfChat("whatsapp", "c1")
	e.now = e.now.Add(time.Minute)
	c.Messages[0].Text = "Sejdeme se v šest"
	res := e.batch(c)
	if res["versions"].(float64) != 1 || res["stored"].(float64) != 0 {
		t.Fatalf("the edit: %v", res)
	}
	c2 := e.a.store.CountRows(e.ctx, "messages")
	if c2 != 1 || e.a.store.CountRows(e.ctx, "raw") != 2 {
		t.Errorf("%d messages, %d raw rows", c2, e.a.store.CountRows(e.ctx, "raw"))
	}
	msg, _ := e.a.store.MessageBySource(e.ctx, "whatsapp", chatSourceID("c1", "m1"))
	if msg.Text != "Sejdeme se v šest" || !msg.Edited {
		t.Errorf("the message: %q edited=%v", msg.Text, msg.Edited)
	}
	page := e.rowPage(it.ID)
	if !strings.Contains(page, "Sejdeme se v šest") || !strings.Contains(page, `<span class="edited">edited</span>`) || strings.Contains(page, "v pět") {
		t.Errorf("the row does not show the new text marked edited:\n%s", page)
	}
	// The chat had been quiet for hours, so the edit brings it back to Claude at once.
	if ids := e.queueIDs(); len(ids) != 1 || ids[0] != repo.Number(it.Number) {
		t.Errorf("an edited message did not bring the chat back to Claude: %v", ids)
	}
	// The same edit again changes nothing.
	n := e.counts()
	e.batch(c)
	if fmt.Sprint(e.counts()) != fmt.Sprint(n) {
		t.Error("the same edit again wrote something")
	}
}

func TestAReactionOrAReadMarkIsAVersionAndWakesNothing(t *testing.T) {
	e := newChatEnv(t)
	c := chatOf("messenger", "c1", "Ondra", "direct", e.in("m1", "t.1", "Ondra", "Posílám plán", 300))
	e.batch(c)
	claudeLooksAtAll(t, e.a)
	it := e.itemOfChat("messenger", "c1")
	// a reaction
	c.Messages[0].Reactions = []bridgeReaction{{By: "me", Emoji: "👍"}}
	res := e.batch(c)
	if res["versions"].(float64) != 1 {
		t.Fatalf("the reaction: %v", res)
	}
	// and a read mark
	c.Messages[0].Read = true
	res = e.batch(c)
	if res["versions"].(float64) != 1 {
		t.Fatalf("the read mark: %v", res)
	}
	e.now = e.now.Add(2 * time.Hour)
	if ids := e.queueIDs(); len(ids) != 0 {
		t.Errorf("a reaction or a read mark brought the chat back to Claude: %v", ids)
	}
	cur, _ := e.a.store.ItemByID(e.ctx, it.ID)
	if cur.NeedsClaude || cur.ChatSince != 0 || cur.Stage != stageNew {
		t.Errorf("the item changed: %+v", cur)
	}
	if e.a.store.CountRows(e.ctx, "messages") != 1 || e.a.store.CountRows(e.ctx, "raw") != 3 {
		t.Error("a reaction or a read mark made a second message or no version")
	}
	msg, _ := e.a.store.MessageBySource(e.ctx, "messenger", chatSourceID("c1", "m1"))
	if msg.Edited || !msg.Seen {
		t.Errorf("edited=%v seen=%v", msg.Edited, msg.Seen)
	}
}

func TestOwnersOwnChatMessageDoesNotWakeTheReviewAndMovesInProgressToPending(t *testing.T) {
	e := newChatEnv(t)
	c := chatOf("whatsapp", "c1", "Jana", "direct", e.in("m1", "+420111", "Jana", "Zavoláš mi?", 300))
	e.batch(c)
	claudeLooksAtAll(t, e.a)
	it := e.itemOfChat("whatsapp", "c1")
	if rec := post(e.h, fmt.Sprintf("/items/%d/open", it.ID), nil); rec.Code != 303 {
		t.Fatalf("open: %d", rec.Code)
	}
	if cur, _ := e.a.store.ItemByID(e.ctx, it.ID); cur.Stage != stageInProgress {
		t.Fatalf("stage %s", cur.Stage)
	}
	e.now = e.now.Add(time.Minute)
	c.Messages = append(c.Messages, e.mine("m2", "Zavolám večer", 0))
	e.batch(c)
	cur, _ := e.a.store.ItemByID(e.ctx, it.ID)
	if cur.Stage != stagePending || cur.NeedsClaude || cur.ChatSince != 0 {
		t.Errorf("after the owner's own message: stage %s, needs Claude %v, live %v", cur.Stage, cur.NeedsClaude, cur.ChatSince)
	}
	e.now = e.now.Add(3 * time.Hour)
	if ids := e.queueIDs(); len(ids) != 0 {
		t.Errorf("The owner's own message woke the review: %v", ids)
	}
	var woke bool
	for _, ty := range e.a.store.EventTypes(e.ctx, it.ID) {
		woke = woke || ty == "reopened"
	}
	if woke {
		t.Error("The owner's own message reopened the item")
	}
	// Someone else writing again brings it back to New, and to Claude when it is quiet.
	c.Messages = append(c.Messages, e.in("m3", "+420111", "Jana", "Díky!", 0))
	e.batch(c)
	if cur, _ = e.a.store.ItemByID(e.ctx, it.ID); cur.Stage != stageNew || cur.ChatSince == 0 {
		t.Errorf("a new message from someone else: stage %s live %v", cur.Stage, cur.ChatSince)
	}
}

func TestOneChatIsOneItemAndTwoChatsWithTheSameNameStayTwo(t *testing.T) {
	e := newChatEnv(t)
	e.batch(chatOf("whatsapp", "c1", "Rodina", "group", e.in("a1", "+420111", "Jana", "Ahoj", 100), e.in("a2", "+420222", "Karel", "Čau", 99)))
	e.batch(chatOf("whatsapp", "c1", "Rodina", "group", e.in("a3", "+420111", "Jana", "Kdo jde?", 98)))
	e.batch(chatOf("whatsapp", "c2", "Rodina", "group", e.in("b1", "+420333", "Eva", "Jiná rodina", 97)))
	e.batch(chatOf("messenger", "c1", "Rodina", "group", e.in("b1", "+420333", "Eva", "Jiná síť, stejné id", 96)))
	claudeLooksAtAll(t, e.a)
	if n := e.a.store.CountRows(e.ctx, "items"); n != 3 {
		t.Fatalf("%d items, want 3 (one chat is one item; the same name is not the same chat)", n)
	}
	one := e.itemOfChat("whatsapp", "c1")
	two := e.itemOfChat("whatsapp", "c2")
	three := e.itemOfChat("messenger", "c1")
	if one.ID == two.ID || one.ID == three.ID || two.ID == three.ID {
		t.Fatal("two chats share an item")
	}
	rows, _ := e.a.store.ListItems(e.ctx, stageNew, 50, 0)
	counts := map[int64]int{}
	for _, r := range rows {
		counts[r.ID] = r.Count
	}
	if counts[one.ID] != 3 || counts[two.ID] != 1 || counts[three.ID] != 1 {
		t.Errorf("messages per item: %v", counts)
	}
}

func TestAGroupChatShowsWhoWroteEachMessageAndKnowsEveryone(t *testing.T) {
	e := newChatEnv(t)
	c := chatOf("whatsapp", "g1", "Cabin", "group",
		e.in("m1", "+420111", "Jana", "Kdo veze dřevo?", 50), e.in("m2", "+420222", "Karel", "Já přivezu nosníky", 49), e.mine("m3", "Dobře", 48),
		e.in("m4", "+420111", "Jana", "Super", 47))
	c.Participants = append(c.Participants, repo.Participant{Handle: "+420111", Name: "Jana"}, repo.Participant{Handle: "+420222", Name: "Karel"}, repo.Participant{Handle: "+420333", Name: "Eva"})
	e.batch(c)
	claudeLooksAtAll(t, e.a)
	it := e.itemOfChat("whatsapp", "g1")
	page := e.rowPage(it.ID)
	for _, want := range []string{"Jana", "Karel", "You", "Kdo veze dřevo?", "Já přivezu nosníky", "4 messages"} {
		if !strings.Contains(page, want) {
			t.Errorf("the open row lacks %q", want)
		}
	}
	if strings.Count(page, `class="from"`) != 4 {
		t.Errorf("each message does not show its sender: %d", strings.Count(page, `class="from"`))
	}
	// Everyone in the chat is a person on first sight, with the handle on that network.
	for _, h := range []string{"+420111", "+420222", "+420333"} {
		if m, _ := e.a.store.PeopleByHandles(e.ctx, []repo.Handle{{Network: "whatsapp", Value: h}}); len(m) != 1 {
			t.Errorf("%s was not added as a person", h)
		}
	}
	if m, _ := e.a.store.PeopleByHandles(e.ctx, []repo.Handle{{Network: "whatsapp", Value: "me-g1"}}); len(m) != 0 {
		t.Error("The owner himself was added as a person")
	}
	// The row's source mark is the network.
	if !strings.Contains(page, "WhatsApp") {
		t.Error("the source mark is not the network")
	}
}

// --- live chats and when Claude looks (SPEC 17) ---

func TestAChatGoingOnWaitsTheQuietTimeBeforeClaudeSeesItAndGoesAtOnceWhenItPasses(t *testing.T) {
	e := newChatEnv(t)
	e.batch(chatOf("messenger", "c1", "Ondra", "direct", e.in("m1", "t.1", "Ondra", "Jsi tam?", 2)))
	it := e.itemOfChat("messenger", "c1")
	// Shown at once, marked Live, and not in Claude's queue.
	if it.Hidden || it.Stage != stageNew || it.NeedsClaude || it.ChatSince == 0 {
		t.Fatalf("a live chat: %+v", it)
	}
	page := get(e.h, "/").Body.String()
	if strings.Contains(page, "waiting for Claude") {
		t.Error("a live chat is counted as waiting for Claude")
	}
	lg := get(e.h, "/list/new").Body.String()
	if !strings.Contains(lg, "Ondra") || !strings.Contains(lg, ">Live<") {
		t.Errorf("the list does not show the live chat:\n%s", lg)
	}
	if ids := e.queueIDs(); len(ids) != 0 {
		t.Fatalf("Claude sees a chat that is still going on: %v", ids)
	}
	// A new message keeps it going: the quiet time counts from the last one.
	e.now = e.now.Add(20 * time.Minute)
	e.batch(chatOf("messenger", "c1", "Ondra", "direct", e.in("m2", "t.1", "Ondra", "Haló?", 0)))
	e.now = e.now.Add(29 * time.Minute)
	if ids := e.queueIDs(); len(ids) != 0 {
		t.Fatalf("29 minutes after the last message it is still going on: %v", ids)
	}
	if !strings.Contains(e.rowPage(it.ID), ">Live<") {
		t.Error("the open row is not marked Live")
	}
	// The moment the quiet time passes, with no timer run in between, it is in the queue, once.
	e.now = e.now.Add(2 * time.Minute)
	ids := e.queueIDs()
	if len(ids) != 1 || ids[0] != repo.Number(it.Number) {
		t.Fatalf("after the quiet time: %v", ids)
	}
	if strings.Contains(e.rowPage(it.ID), ">Live<") {
		t.Error("a chat that has gone to Claude is still marked Live")
	}
	cur, _ := e.a.store.ItemByID(e.ctx, it.ID)
	if cur.Hidden || cur.ChatSince != 0 || !cur.NeedsClaude {
		t.Errorf("after release: %+v", cur)
	}
	// The timer does the same without a request.
	e.batch(chatOf("messenger", "c1", "Ondra", "direct", e.in("m3", "t.1", "Ondra", "A ještě", 0)))
	if cur, _ = e.a.store.ItemByID(e.ctx, it.ID); cur.NeedsClaude {
		t.Error("a new message after the chat went to Claude left it in the queue")
	}
	e.now = e.now.Add(31 * time.Minute)
	e.a.releaseChats(e.ctx)
	if cur, _ = e.a.store.ItemByID(e.ctx, it.ID); !cur.NeedsClaude || cur.ChatSince != 0 {
		t.Errorf("the timer did not release the chat: %+v", cur)
	}
}

func TestTheChatQuietTimeIsASetting(t *testing.T) {
	e := newChatEnv(t)
	if code, _ := e.saveSettings(url.Values{keyChatQuiet: {"10"}}); code != 303 {
		t.Fatalf("save: %d", code)
	}
	e.batch(chatOf("messenger", "c1", "Ondra", "direct", e.in("m1", "t.1", "Ondra", "Jsi tam?", 0)))
	e.now = e.now.Add(9 * time.Minute)
	if ids := e.queueIDs(); len(ids) != 0 {
		t.Fatalf("%v", ids)
	}
	e.now = e.now.Add(2 * time.Minute)
	if ids := e.queueIDs(); len(ids) != 1 {
		t.Errorf("a quiet time of 10 minutes did not send the chat after 11: %v", ids)
	}
}

func TestAChatThatArrivedFinishedWaitsForClaudeAtOnceHiddenWithTheNotice(t *testing.T) {
	e := newChatEnv(t)
	e.batch(chatOf("whatsapp", "c1", "Jana", "direct", e.in("m1", "+420111", "Jana", "Díky za pomoc", 180)))
	it := e.itemOfChat("whatsapp", "c1")
	if !it.Hidden || !it.NeedsClaude || it.ChatSince != 0 {
		t.Fatalf("a chat that arrived finished: %+v", it)
	}
	if ids := e.queueIDs(); len(ids) != 1 {
		t.Fatalf("queue %v", ids)
	}
	if get(e.h, "/list/new").Body.String() == "" || strings.Contains(get(e.h, "/list/new").Body.String(), "Jana") {
		t.Error("a chat Claude has not looked at is in the owner's list")
	}
	claudeLooksAtAll(t, e.a)
	if page := e.rowPage(it.ID); !strings.Contains(page, "Arrived settled.") || strings.Contains(page, ">Live<") {
		t.Errorf("the thread does not say Arrived settled.:\n%s", page)
	}
	// A chat whose last message is older than the review window is history, not waiting.
	e.batch(chatOf("whatsapp", "c2", "Stará", "direct", e.in("o1", "+420999", "Stará", "Dávno", 60*24*45)))
	old := e.itemOfChat("whatsapp", "c2")
	if old.Stage != stageHistory || old.NeedsClaude || old.ChatSince != 0 || old.Hidden {
		t.Errorf("an old chat: %+v", old)
	}
}

func TestTheOpenChatAsksTheServerAndTheRowFollowsNewMessages(t *testing.T) {
	e := newChatEnv(t)
	e.batch(chatOf("messenger", "c1", "Ondra", "direct", e.in("m1", "t.1", "Ondra", "Ahoj", 1)))
	it := e.itemOfChat("messenger", "c1")
	page := e.rowPage(it.ID)
	if !strings.Contains(page, `data-chat="1"`) || !strings.Contains(page, `data-pulse="`) {
		t.Fatalf("the row of a chat does not ask for news:\n%s", page)
	}
	pulse := func() string { return get(e.h, fmt.Sprintf("/items/%d/pulse", it.ID)).Body.String() }
	p1 := pulse()
	if !strings.Contains(page, `data-pulse="`+p1+`"`) {
		t.Errorf("the row holds %q, the server says %q", page, p1)
	}
	if pulse() != p1 {
		t.Error("the pulse changed with nothing new")
	}
	e.batch(chatOf("messenger", "c1", "Ondra", "direct", e.in("m2", "t.1", "Ondra", "Nová zpráva", 0)))
	p2 := pulse()
	if p1 == p2 {
		t.Error("a new message did not change the pulse")
	}
	if !strings.Contains(e.rowPage(it.ID), "Nová zpráva") {
		t.Error("the row does not show the new message")
	}
	c := chatOf("messenger", "c1", "Ondra", "direct", e.in("m2", "t.1", "Ondra", "Opravená zpráva", 0))
	e.batch(c)
	if pulse() == p2 {
		t.Error("an edit did not change the pulse")
	}
	// A mail row never asks.
	rec := get(e.h, "/items/999/pulse")
	if rec.Code != 404 {
		t.Errorf("pulse of no item: %d", rec.Code)
	}
}

// --- import rules (SPEC 21) ---

func TestTheMutedGroupRuleDropsAndImportAgainBringsBack(t *testing.T) {
	e := newChatEnv(t)
	muted := chatOf("whatsapp", "g9", "Reklamy", "group", e.in("m1", "+420555", "Obchod", "Sleva 50 %", 100))
	muted.Muted = true
	res := e.batch(muted)
	if res["dropped"].(float64) != 1 || res["stored"].(float64) != 0 {
		t.Fatalf("a muted group chat: %v", res)
	}
	if e.a.store.CountRows(e.ctx, "items") != 0 || e.a.store.CountRows(e.ctx, "messages") != 0 || e.a.store.CountChats(e.ctx) != 0 {
		t.Error("a dropped message was imported")
	}
	if e.a.store.CountRows(e.ctx, "skipped") != 1 {
		t.Fatal("no line was kept for the dropped message")
	}
	// A muted one-to-one chat is not a group chat: it comes in.
	e.batch(func() bridgeChat {
		c := chatOf("whatsapp", "d1", "Jana", "direct", e.in("m1", "+420111", "Jana", "Ahoj", 100))
		c.Muted = true
		return c
	}())
	if e.a.store.CountRows(e.ctx, "items") != 1 {
		t.Error("a muted one-to-one chat was dropped by the rule for muted group chats")
	}
	// A group chat that is not muted comes in too.
	e.batch(chatOf("whatsapp", "g2", "Rodina", "group", e.in("m1", "+420111", "Jana", "Ahoj", 100)))
	if e.a.store.CountRows(e.ctx, "items") != 2 {
		t.Error("an unmuted group chat was dropped")
	}
	// Posting the dropped message again keeps one line and drops it again.
	if r := e.batch(muted); r["dropped"].(float64) != 0 || e.a.store.CountRows(e.ctx, "skipped") != 1 {
		t.Errorf("the dropped message was counted twice: %v", r)
	}
	settings := get(e.h, "/settings").Body.String()
	for _, want := range []string{"Muted group chats", "Dropped (1)", "Reklamy", "Import again"} {
		if !strings.Contains(settings, want) {
			t.Errorf("Settings lacks %q", want)
		}
	}
	// Import again asks the Mac; the server never calls it.
	line, _ := e.a.store.SkippedBySource(e.ctx, "whatsapp", chatSourceID("g9", "m1"))
	id := line.ID
	if id == 0 {
		t.Fatal("the dropped line is not listed")
	}
	if rec := post(e.h, fmt.Sprintf("/settings/skipped/%d/again", id), nil); rec.Code != 303 {
		t.Fatalf("Import again: %d", rec.Code)
	}
	if e.a.store.CountRows(e.ctx, "items") != 2 {
		t.Fatal("the message came back without the bridge")
	}
	out := answerOf(t, e.asBridge("POST", "/api/bridge/outbox", nil))
	resend := out["resend"].([]any)
	if len(resend) != 1 || resend[0].(map[string]any)["chat_id"] != "g9" || resend[0].(map[string]any)["message_id"] != "m1" || resend[0].(map[string]any)["network"] != "whatsapp" {
		t.Fatalf("the bridge was not asked for the message: %v", out)
	}
	// The bridge brings it again, marked as asked for: the rules are left out.
	rec := e.asBridge("POST", "/api/bridge/batch", bridgeBatch{Again: true, Chats: []bridgeChat{muted}})
	if rec.Code != 200 || answerOf(t, rec)["stored"].(float64) != 1 {
		t.Fatalf("import again: %d %s", rec.Code, rec.Body.String())
	}
	if e.a.store.CountRows(e.ctx, "items") != 3 || e.a.store.CountChats(e.ctx) != 3 {
		t.Error("the dropped chat did not come back")
	}
	out = answerOf(t, e.asBridge("POST", "/api/bridge/outbox", nil))
	if len(out["resend"].([]any)) != 0 {
		t.Errorf("the bridge is asked again for what it brought: %v", out)
	}
	if page := get(e.h, "/settings").Body.String(); !strings.Contains(page, "Imported again") {
		t.Error("Settings does not say the message was imported again")
	}
}

// --- attachments (SPEC 7, 16) ---

func TestAnAttachmentIsStoredOnceByHashHoweverManyMessagesHoldIt(t *testing.T) {
	e := newChatEnv(t)
	png := append([]byte("\x89PNG\r\n\x1a\n"), bytes.Repeat([]byte{7}, 200)...)
	m1 := e.in("m1", "t.1", "Ondra", "Foto z chaty", 20)
	m1.Attachments = []bridgeAttachment{{ID: "a1", Name: "cabin.png", Type: "image/png", Size: int64(len(png))}}
	m2 := e.in("m2", "t.1", "Ondra", "A ještě jednou", 19)
	m2.Attachments = []bridgeAttachment{{ID: "a1", Name: "cabin-kopie.png", Type: "image/png", Size: int64(len(png))}}
	res := e.batch(chatOf("messenger", "c1", "Ondra", "direct", m1, m2))
	missing := res["missing_files"].([]any)
	if len(missing) != 2 {
		t.Fatalf("the bridge is not told what to upload: %v", res)
	}
	put := func(msg string) *httptest.ResponseRecorder {
		return e.asBridge("PUT", "/api/bridge/files?network=messenger&chat_id=c1&message_id="+msg+"&attachment=a1", png)
	}
	if rec := put("m1"); rec.Code != 200 {
		t.Fatalf("upload: %d %s", rec.Code, rec.Body.String())
	}
	if rec := put("m2"); rec.Code != 200 {
		t.Fatalf("upload: %d %s", rec.Code, rec.Body.String())
	}
	// The same bytes sit once in the pool, and both messages point at them.
	n := 0
	var sha string
	filepath.WalkDir(e.a.imp.pool.root, func(path string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && poolPathRE.MatchString(strings.TrimPrefix(path, e.a.imp.pool.root+"/")) {
			n++
		}
		return nil
	})
	if n != 1 {
		t.Errorf("%d files in the pool, want 1", n)
	}
	it := e.itemOfChat("messenger", "c1")
	files, _ := e.a.store.FileList(e.ctx, it.ID)
	if len(files) != 2 {
		t.Fatalf("%d file rows", len(files))
	}
	for _, f := range files {
		if f.Status != "saved" || f.SHA == "" || (sha != "" && f.SHA != sha) {
			t.Errorf("file %+v", f)
		}
		sha = f.SHA
	}
	// Once uploaded, a second batch has nothing left to upload and a second upload is a no-op.
	res = e.batch(chatOf("messenger", "c1", "Ondra", "direct", m1, m2))
	if len(res["missing_files"].([]any)) != 0 {
		t.Errorf("still missing: %v", res["missing_files"])
	}
	if rec := put("m1"); rec.Code != 200 || answerOf(t, rec)["unchanged"] != true {
		t.Errorf("a second upload: %d %s", rec.Code, rec.Body.String())
	}
	// The picture is shown from the app's own copy, and it is a download behind the sign-in.
	page := e.rowPage(it.ID)
	if !strings.Contains(page, fmt.Sprintf(`src="/files/%d"`, files[0].ID)) {
		t.Errorf("the picture is not shown:\n%s", page)
	}
	if rec := get(e.h, fmt.Sprintf("/files/%d", files[0].ID)); rec.Code != 200 || !bytes.Equal(rec.Body.Bytes(), png) {
		t.Errorf("download: %d", rec.Code)
	}
	// An attachment the batch did not announce is refused.
	if rec := e.asBridge("PUT", "/api/bridge/files?network=messenger&chat_id=c1&message_id=m1&attachment=zz", png); rec.Code != 404 {
		t.Errorf("an unknown attachment: %d", rec.Code)
	}
}

// --- replies in a chat (SPEC 13) ---

// chatItemForReply brings a chat from someone else in as an item the owner has open.
func (e *chatEnv) chatItemForReply(network, id, name, text string) repo.Item {
	e.t.Helper()
	c := chatOf(network, id, name, "direct", e.in("m1", "p.1", name, text, 120))
	c.Participants = append(c.Participants, repo.Participant{Handle: "p.1", Name: name})
	e.batch(c)
	claudeLooksAtAll(e.t, e.a)
	it := e.itemOfChat(network, id)
	if err := e.a.store.Update(e.ctx, func(tx *repo.Tx) error { return tx.SetItemStage(e.ctx, it.ID, stageInProgress, e.now.Unix()) }); err != nil {
		e.t.Fatal(err)
	}
	it, _ = e.a.store.ItemByID(e.ctx, it.ID)
	return it
}

func (e *chatEnv) outbox() (replies []map[string]any, resend []any) {
	e.t.Helper()
	rec := e.asBridge("POST", "/api/bridge/outbox", nil)
	if rec.Code != 200 {
		e.t.Fatalf("outbox: %d %s", rec.Code, rec.Body.String())
	}
	out := answerOf(e.t, rec)
	for _, r := range out["replies"].([]any) {
		replies = append(replies, r.(map[string]any))
	}
	return replies, out["resend"].([]any)
}

func TestAChatReplyIsHandedOutOnceReportedOnceAndNeverSentTwice(t *testing.T) {
	e := newChatEnv(t)
	it := e.chatItemForReply("messenger", "c1", "Ondra", "Můžeš zítra v osm?")
	const reply = "Ano, zítra v osm budu u tebe. the owner"
	pid := proposeReplyAs(t, e.h, e.a, it, reply)
	// The proposal says where it goes, and nothing is sent.
	page := e.rowPage(it.ID)
	if !strings.Contains(page, "Will be sent · reply in Messenger") || !strings.Contains(page, reply) || !strings.Contains(page, "Ondra") {
		t.Fatalf("the proposal box:\n%s", page)
	}
	if replies, _ := e.outbox(); len(replies) != 0 {
		t.Fatalf("a proposal was handed to the bridge: %v", replies)
	}
	// Approve records the intent with the exact text the owner saw, edited.
	const shown = "Ano, zítra v osm budu u tebe. The owner (upraveno)"
	rec := approveAs(e.h, pid, shown)
	if rec.Code != 200 {
		t.Fatalf("approve: %d %s", rec.Code, rec.Body.String())
	}
	p, _ := e.a.store.ProposalByID(e.ctx, pid)
	if p.SendState != "queued" || p.FinalText != shown || p.Text != reply || p.Status != "approved" {
		t.Fatalf("the intent: state %q final %q original %q status %q", p.SendState, p.FinalText, p.Text, p.Status)
	}
	if body := rec.Body.String(); !strings.Contains(body, "Waiting for the Mac to send this.") || !strings.Contains(body, "Withdraw") {
		t.Errorf("the thread does not say it waits for the Mac:\n%s", body)
	}
	// Approving twice sends once: it is still the one intent.
	approveAs(e.h, pid, "something else")
	if p2, _ := e.a.store.ProposalByID(e.ctx, pid); p2.FinalText != shown || p2.SendState != "queued" {
		t.Errorf("a second Approve changed the intent: %+v", p2)
	}
	// The bridge asks: it is handed out, with the chat and the exact text.
	replies, _ := e.outbox()
	if len(replies) != 1 || replies[0]["text"] != shown || replies[0]["chat_id"] != "c1" || replies[0]["network"] != "messenger" || int64(replies[0]["id"].(float64)) != pid {
		t.Fatalf("handed out: %v", replies)
	}
	// It asks again before reporting: the reply is not handed out again.
	for i := 0; i < 3; i++ {
		if again, _ := e.outbox(); len(again) != 0 {
			t.Fatalf("a reply handed out was handed out again: %v", again)
		}
	}
	if body := e.rowPage(it.ID); strings.Contains(body, "Withdraw") || !strings.Contains(body, "The Mac has this reply") {
		t.Errorf("a reply the Mac has can still be withdrawn:\n%s", body)
	}
	if rec := post(e.h, fmt.Sprintf("/proposals/%d/withdraw", pid), nil); rec.Code != 409 {
		t.Errorf("withdraw after hand-out: %d", rec.Code)
	}
	// It reports once.
	e.now = e.now.Add(time.Minute)
	path := fmt.Sprintf("/api/bridge/outbox/%d/result", pid)
	rec = e.asBridge("POST", path, map[string]any{"state": "sent", "message_id": "bm-77"})
	if rec.Code != 200 || answerOf(t, rec)["state"] != "sent" {
		t.Fatalf("report: %d %s", rec.Code, rec.Body.String())
	}
	body := e.rowPage(it.ID)
	if !strings.Contains(body, "Sent "+e.now.In(e.a.loc).Format("15:04")+".") {
		t.Errorf("the thread does not say Sent <time>.:\n%s", body)
	}
	if cur, _ := e.a.store.ItemByID(e.ctx, it.ID); cur.Stage != stagePending {
		t.Errorf("a reply that went out left the item %s", cur.Stage)
	}
	// A second report changes nothing, even a contradicting one.
	events := e.a.store.CountRows(e.ctx, "events")
	for _, st := range []string{"sent", "failed"} {
		rec = e.asBridge("POST", path, map[string]any{"state": st, "error": "late"})
		if rec.Code != 200 || answerOf(t, rec)["unchanged"] != true {
			t.Errorf("a second report %s: %d %s", st, rec.Code, rec.Body.String())
		}
	}
	if e.a.store.CountRows(e.ctx, "events") != events {
		t.Error("a second report wrote to the record")
	}
	if strings.Count(e.rowPage(it.ID), "Sent ") != 1 {
		t.Error("the thread says Sent more than once")
	}
	if len(e.notes.all()) != 0 {
		t.Errorf("a reply that went out sent a notice: %v", e.notes.all())
	}
	// A reply on the same message cannot be proposed again.
	code, m := agentCall(e.h, e.a, "POST", "/api/items/"+repo.Number(it.Number)+"/reply-proposals", map[string]any{"text": "znovu", "reason": "x"})
	if code == 200 {
		t.Errorf("a second reply to a message already answered: %v", m)
	}
}

func TestAChatReplyThatWasWithdrawnIsNeverHandedOut(t *testing.T) {
	e := newChatEnv(t)
	it := e.chatItemForReply("whatsapp", "c1", "Jana", "Jdeš dnes?")
	pid := proposeReplyAs(t, e.h, e.a, it, "Jdu.")
	approveAs(e.h, pid, "Jdu.")
	rec := post(e.h, fmt.Sprintf("/proposals/%d/withdraw", pid), nil)
	if rec.Code != 303 {
		t.Fatalf("withdraw: %d", rec.Code)
	}
	if replies, _ := e.outbox(); len(replies) != 0 {
		t.Fatalf("a withdrawn reply was handed out: %v", replies)
	}
	p, _ := e.a.store.ProposalByID(e.ctx, pid)
	if p.SendState != "withdrawn" || p.Status != "withdrawn" {
		t.Errorf("state %q status %q", p.SendState, p.Status)
	}
	if page := e.rowPage(it.ID); !strings.Contains(page, "You withdrew this reply.") || strings.Contains(page, "Waiting for the Mac") {
		t.Errorf("the thread:\n%s", page)
	}
	// The bridge cannot report on what it never had.
	rec = e.asBridge("POST", fmt.Sprintf("/api/bridge/outbox/%d/result", pid), map[string]any{"state": "sent"})
	if rec.Code != 409 {
		t.Errorf("a report on a reply never handed out: %d", rec.Code)
	}
	if p, _ = e.a.store.ProposalByID(e.ctx, pid); p.SendState != "withdrawn" {
		t.Errorf("the report changed a withdrawn reply: %q", p.SendState)
	}
	// The owner can write another one for the same message.
	if code, _ := agentCall(e.h, e.a, "POST", "/api/items/"+repo.Number(it.Number)+"/reply-proposals", map[string]any{"text": "Jdu v 18:00.", "reason": "x"}); code != 200 {
		t.Errorf("a new proposal after withdrawing: %d", code)
	}
}

func TestAChatReplyThatCouldNotBeSentSaysWhyAndSendsOneNotice(t *testing.T) {
	e := newChatEnv(t)
	it := e.chatItemForReply("messenger", "c1", "Ondra", "Pošli adresu")
	pid := proposeReplyAs(t, e.h, e.a, it, "Cabin 12.")
	approveAs(e.h, pid, "Cabin 12.")
	e.outbox()
	rec := e.asBridge("POST", fmt.Sprintf("/api/bridge/outbox/%d/result", pid), map[string]any{"state": "failed", "error": "Beeper refused: the chat is gone"})
	if rec.Code != 200 {
		t.Fatalf("report: %d", rec.Code)
	}
	if body := e.rowPage(it.ID); !strings.Contains(body, "Could not send: Beeper refused: the chat is gone") {
		t.Errorf("the thread:\n%s", body)
	}
	got := e.notes.all()
	if len(got) != 1 || got[0][0] != "Could not send" || strings.Contains(got[0][1], "Cabin") || strings.Contains(got[0][1], "Ondra") {
		t.Errorf("notices: %v", got)
	}
	if cur, _ := e.a.store.ItemByID(e.ctx, it.ID); cur.Stage != stageInProgress {
		t.Errorf("the item needs the owner again: stage %s", cur.Stage)
	}
	e.asBridge("POST", fmt.Sprintf("/api/bridge/outbox/%d/result", pid), map[string]any{"state": "failed", "error": "again"})
	if len(e.notes.all()) != 1 {
		t.Error("a second report sent a second notice")
	}
}

func TestAChatReplyTheMacTookAndNeverReportedIsInDoubtAndNeverHandedOutAgain(t *testing.T) {
	e := newChatEnv(t)
	it := e.chatItemForReply("messenger", "c1", "Ondra", "Pošli adresu")
	pid := proposeReplyAs(t, e.h, e.a, it, "Cabin 12.")
	approveAs(e.h, pid, "Cabin 12.")
	e.heardNow()
	if replies, _ := e.outbox(); len(replies) != 1 {
		t.Fatal("not handed out")
	}
	e.now = e.now.Add(14 * time.Minute)
	e.a.chatSendsInDoubt(e.ctx)
	if p, _ := e.a.store.ProposalByID(e.ctx, pid); p.SendState != "handed" {
		t.Fatalf("marked in doubt too early: %q", p.SendState)
	}
	e.now = e.now.Add(2 * time.Minute)
	e.a.chatSendsInDoubt(e.ctx)
	p, _ := e.a.store.ProposalByID(e.ctx, pid)
	if p.SendState != "unknown" {
		t.Fatalf("state %q", p.SendState)
	}
	if body := e.rowPage(it.ID); !strings.Contains(body, "may have gone") {
		t.Errorf("the thread does not say it may have gone:\n%s", body)
	}
	if replies, _ := e.outbox(); len(replies) != 0 {
		t.Error("a reply in doubt was handed out again")
	}
	if len(e.notes.all()) != 1 || e.notes.all()[0][0] != "Could not send" {
		t.Errorf("notices %v", e.notes.all())
	}
	// A late, definite report from the Mac settles it.
	rec := e.asBridge("POST", fmt.Sprintf("/api/bridge/outbox/%d/result", pid), map[string]any{"state": "sent", "message_id": "bm-1"})
	if rec.Code != 200 {
		t.Fatalf("late report: %d", rec.Code)
	}
	if p, _ = e.a.store.ProposalByID(e.ctx, pid); p.SendState != "sent" {
		t.Errorf("state %q", p.SendState)
	}
}

func TestTheSentChatMessageThatComesBackIsTheReplyAndNotASecondMessage(t *testing.T) {
	e := newChatEnv(t)
	it := e.chatItemForReply("messenger", "c1", "Ondra", "Pošli adresu")
	pid := proposeReplyAs(t, e.h, e.a, it, "Cabin 12.")
	approveAs(e.h, pid, "Cabin 12.")
	e.outbox()
	e.asBridge("POST", fmt.Sprintf("/api/bridge/outbox/%d/result", pid), map[string]any{"state": "sent", "message_id": "bm-1"})
	// The message arrives in the chat as the owner's own, by the id the bridge reported.
	c := chatOf("messenger", "c1", "Ondra", "direct", e.in("m1", "p.1", "Ondra", "Pošli adresu", 120), e.mine("bm-1", "Cabin 12.", 0))
	e.batch(c)
	p, _ := e.a.store.ProposalByID(e.ctx, pid)
	if p.SentMsgID == 0 {
		t.Error("the reply was not matched with the message that came back")
	}
	if body := e.rowPage(it.ID); strings.Count(body, `<div class="text">Cabin 12.<`) != 1 {
		t.Errorf("the reply is shown %d times:\n%s", strings.Count(body, "Cabin 12."), body)
	}
	if cur, _ := e.a.store.ItemByID(e.ctx, it.ID); cur.Stage != stagePending || cur.NeedsClaude {
		t.Errorf("stage %s", cur.Stage)
	}
}

func TestAChatReplyIsRefusedWhereAMailReplyIsAndForTheSameReasons(t *testing.T) {
	e := newChatEnv(t)
	// Blocked targets: a chat with someone on the list.
	c := chatOf("imessage", "c5", "Banka", "direct", e.in("m1", "info@unicreditbank.cz", "Banka", "Váš účet", 120))
	e.batch(c)
	claudeLooksAtAll(t, e.a)
	it := e.itemOfChat("imessage", "c5")
	code, m := agentCall(e.h, e.a, "POST", "/api/items/"+repo.Number(it.Number)+"/reply-proposals", map[string]any{"text": "Dobrý den", "reason": "x"})
	if code != 403 || !strings.Contains(fmt.Sprint(m["error"]), "Refused by guard: Blocked targets") {
		t.Errorf("a reply to a bank: %d %v", code, m)
	}
	// Same thread: the newest message is the owner's own.
	e.batch(chatOf("whatsapp", "c6", "Jana", "direct", e.in("m1", "+420111", "Jana", "Ahoj", 120), e.mine("m2", "Čau", 100)))
	claudeLooksAtAll(t, e.a)
	own := e.itemOfChat("whatsapp", "c6")
	code, m = agentCall(e.h, e.a, "POST", "/api/items/"+repo.Number(own.Number)+"/reply-proposals", map[string]any{"text": "Ahoj", "reason": "x"})
	if code != 403 || !strings.Contains(fmt.Sprint(m["error"]), "Refused by guard: Same thread") {
		t.Errorf("a reply to the owner's own message: %d %v", code, m)
	}
	// A recipient is never named by the caller.
	other := e.chatItemForReply("whatsapp", "c7", "Eva", "Ahoj")
	code, _ = agentCall(e.h, e.a, "POST", "/api/items/"+repo.Number(other.Number)+"/reply-proposals", map[string]any{"text": "Ahoj", "reason": "x", "to": "+420999"})
	if code != 400 {
		t.Errorf("a reply that names a recipient: %d", code)
	}
	// A new message on the chat withdraws the waiting proposal.
	pid := proposeReplyAs(t, e.h, e.a, other, "Čau Evo")
	e.batch(chatOf("whatsapp", "c7", "Eva", "direct", e.in("m9", "p.1", "Eva", "Ještě jedna věc", 0)))
	if p, _ := e.a.store.ProposalByID(e.ctx, pid); p.Status != "withdrawn" {
		t.Errorf("status %q", p.Status)
	}
	if rec := approveAs(e.h, pid, "Čau Evo"); rec.Code != 200 {
		t.Errorf("approve of a withdrawn reply: %d", rec.Code)
	}
	if replies, _ := e.outbox(); len(replies) != 0 {
		t.Errorf("a withdrawn proposal was handed out: %v", replies)
	}
	// The agents' tools and the address cannot approve or send a chat reply.
	pid = proposeReplyAs(t, e.h, e.a, e.itemOfChat2("whatsapp", "c7"), "Čau")
	if code, _ := agentCall(e.h, e.a, "POST", fmt.Sprintf("/api/proposals/%d/approve", pid), map[string]any{"text": "x"}); code != 403 {
		t.Errorf("an agent approved: %d", code)
	}
	if p, _ := e.a.store.ProposalByID(e.ctx, pid); p.SendState != "" || p.Status != "pending" {
		t.Errorf("the proposal moved: %+v", p)
	}
}

func (e *chatEnv) itemOfChat2(network, id string) repo.Item { return e.itemOfChat(network, id) }

func TestWaitingForTheMacShowsWhileTheBridgeIsSilentAndNothingIsSentMeanwhile(t *testing.T) {
	e := newChatEnv(t)
	it := e.chatItemForReply("whatsapp", "c1", "Jana", "Jdeš dnes?")
	pid := proposeReplyAs(t, e.h, e.a, it, "Jdu.")
	rec := approveAs(e.h, pid, "Jdu.")
	for _, want := range []string{"Waiting for the Mac to send this.", "Withdraw"} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Errorf("the answer lacks %q:\n%s", want, rec.Body.String())
		}
	}
	// Hours pass with the Mac off: it still waits, the row says so, and nothing went anywhere.
	e.now = e.now.Add(5 * time.Hour)
	if page := e.rowPage(it.ID); !strings.Contains(page, "Waiting for the Mac to send this.") || !strings.Contains(page, ">Withdraw<") {
		t.Errorf("after hours:\n%s", page)
	}
	if p, _ := e.a.store.ProposalByID(e.ctx, pid); p.SendState != "queued" {
		t.Errorf("state %q", p.SendState)
	}
	// When the Mac comes back it takes it.
	if replies, _ := e.outbox(); len(replies) != 1 {
		t.Error("the bridge did not get it when it came back")
	}
}

// --- silence is never silent (SPEC 24) ---

func TestABridgeSilentPastTheLimitShowsTheStripTheDotAndOneNotice(t *testing.T) {
	e := newChatEnv(t)
	// Never heard: not configured, no strip.
	if s := alertsFor(e.a.sourceStates(e.ctx)); len(s) != 0 {
		t.Fatalf("strip before the bridge was ever heard: %v", s)
	}
	e.asBridge("POST", "/api/bridge/alive", map[string]any{"networks": []bridgeNet{{Network: "whatsapp", State: "connected"}, {Network: "messenger", State: "connected"}}, "version": "0.1"})
	e.a.checkSources(e.ctx)
	if page := get(e.h, "/").Body.String(); !strings.Contains(page, "Connectors: all fine") || strings.Contains(page, "has stopped") {
		t.Fatal("a bridge that was just heard is shown as failed")
	}
	e.now = e.now.Add(14 * time.Minute)
	e.a.checkSources(e.ctx)
	if len(e.notes.all()) != 0 || len(alertsFor(e.a.sourceStates(e.ctx))) != 0 {
		t.Fatal("14 minutes of silence is already a failure")
	}
	e.now = e.now.Add(2 * time.Minute)
	for i := 0; i < 4; i++ { // the timer runs every minute: one notice, not one per tick
		e.a.checkSources(e.ctx)
		e.now = e.now.Add(time.Minute)
	}
	got := e.notes.all()
	if len(got) != 1 || got[0][0] != "The Mac bridge is silent" {
		t.Fatalf("notices: %v", got)
	}
	if strings.Contains(got[0][1], "Jana") || strings.Contains(got[0][1], e.a.bridgeTokenValue()) {
		t.Errorf("the notice holds more than it should: %v", got[0])
	}
	strips := alertsFor(e.a.sourceStates(e.ctx))
	if len(strips) != 1 || strips[0] != "Chat bridge has stopped. See Connectors." {
		t.Errorf("strip %v", strips)
	}
	for _, p := range []string{"/", "/connectors", "/settings", "/closed"} {
		page := get(e.h, p).Body.String()
		if !strings.Contains(page, "Chat bridge has stopped. See Connectors.") || !strings.Contains(page, "Connectors: a source has stopped") {
			t.Errorf("%s does not show the strip and the dot", p)
		}
	}
	conn := get(e.h, "/connectors").Body.String()
	for _, want := range []string{"Chat bridge", "Error", "The Mac has not been heard from for", "Last read: 19 Nov 2026 13:00", "Known limitations",
		"Chats arrive only while the Mac is on and Beeper is running.", "A network that has logged itself out delivers nothing until the owner logs in on the Mac.", "Phone calls are not covered."} {
		if !strings.Contains(conn, want) {
			t.Errorf("Connectors lacks %q", want)
		}
	}
	// Heard again: the strip goes, and the next silence is a new failure with a new notice.
	e.asBridge("POST", "/api/bridge/alive", map[string]any{})
	e.a.checkSources(e.ctx)
	if len(alertsFor(e.a.sourceStates(e.ctx))) != 0 {
		t.Error("the strip stays after the bridge was heard")
	}
	e.now = e.now.Add(20 * time.Minute)
	e.a.checkSources(e.ctx)
	if len(e.notes.all()) != 2 {
		t.Errorf("a second silence did not send a second notice: %d", len(e.notes.all()))
	}
}

func TestTheBridgeSilenceLimitIsASettingAndAnyCallCountsAsHeard(t *testing.T) {
	e := newChatEnv(t)
	e.saveSettings(url.Values{keyBridgeSilence: {"5"}})
	e.asBridge("POST", "/api/bridge/outbox", nil) // asking for replies counts as being heard
	if e.a.bridgeHeardAt(e.ctx) == 0 {
		t.Fatal("not heard")
	}
	e.now = e.now.Add(6 * time.Minute)
	if len(alertsFor(e.a.sourceStates(e.ctx))) != 1 {
		t.Error("6 minutes of silence with a limit of 5 is not a failure")
	}
}

func TestANetworkThatLoggedItselfOutIsTold(t *testing.T) {
	e := newChatEnv(t)
	e.asBridge("POST", "/api/bridge/alive", map[string]any{"networks": []bridgeNet{{Network: "whatsapp", State: "needs_login"}, {Network: "messenger", State: "connected"}}})
	strips := alertsFor(e.a.sourceStates(e.ctx))
	if len(strips) != 1 || strips[0] != "Chat bridge needs login. See Connectors." {
		t.Errorf("strip %v", strips)
	}
	if got := e.notes.all(); len(got) != 1 || got[0][0] != "Chat bridge needs login" {
		t.Errorf("notices %v", got)
	}
	e.asBridge("POST", "/api/bridge/alive", map[string]any{"networks": []bridgeNet{{Network: "whatsapp", State: "needs_login"}}})
	if len(e.notes.all()) != 1 {
		t.Error("the same login problem sent a second notice")
	}
	conn := get(e.h, "/connectors").Body.String()
	if !strings.Contains(conn, "WhatsApp") || !strings.Contains(conn, "Needs login") {
		t.Errorf("the page does not name the network:\n%s", conn)
	}
	e.asBridge("POST", "/api/bridge/alive", map[string]any{"networks": []bridgeNet{{Network: "whatsapp", State: "connected"}}})
	if len(alertsFor(e.a.sourceStates(e.ctx))) != 0 {
		t.Error("the strip stays after the login")
	}
}

func TestTheConnectorsRowSaysWhatTheBridgeHasBrought(t *testing.T) {
	e := newChatEnv(t)
	e.batch(chatOf("whatsapp", "c1", "Jana", "direct", e.in("m1", "+420111", "Jana", "Ahoj", 100), e.in("m2", "+420111", "Jana", "Čau", 99)))
	e.batch(chatOf("messenger", "c2", "Ondra", "direct", e.in("m1", "t.1", "Ondra", "Ahoj", 100)))
	e.asBridge("POST", "/api/bridge/alive", map[string]any{"networks": []bridgeNet{{Network: "whatsapp", State: "connected"}}})
	conn := get(e.h, "/connectors").Body.String()
	for _, want := range []string{"2 chats, 3 messages", "Last 24 hours", "Last read: 19 Nov 2026 13:00", ">OK<"} {
		if !strings.Contains(conn, want) {
			t.Errorf("the row lacks %q", want)
		}
	}
	if !strings.Contains(conn, `<section class="crow" id="chatbridge"`) {
		t.Error("no Chat bridge row")
	}
}

// --- what is kept out of logs, notices and pages ---

func TestNothingOfAChatOrTheTokensIsLogged(t *testing.T) {
	e := newChatEnv(t)
	secret := "ZXQ-hovor-o-penězích-4711"
	handle := "+420777000111"
	m := e.in("m1", handle, "Jana Tajná", secret, 100)
	m.Attachments = []bridgeAttachment{{ID: "a1", Name: "smlouva-ZXQ.pdf", Type: "application/pdf", Size: 3}}
	e.batch(chatOf("whatsapp", "c1", "Skupina ZXQ", "direct", m))
	e.asBridge("PUT", "/api/bridge/files?network=whatsapp&chat_id=c1&message_id=m1&attachment=a1", []byte("pdf"))
	e.asBridge("POST", "/api/bridge/alive", map[string]any{"last_error": "network trouble"})
	e.asBridge("POST", "/api/bridge/outbox", nil)
	e.bridgeCall("POST", "/api/bridge/batch", "wrong-token-123456789012345678901234567890", nil)
	it := e.itemOfChat("whatsapp", "c1")
	claudeLooksAtAll(t, e.a)
	pid := proposeReplyAs(t, e.h, e.a, e.itemOfChat("whatsapp", "c1"), "Odpověď ZXQ-tajná")
	_ = it
	approveAs(e.h, pid, "Odpověď ZXQ-tajná")
	e.outbox()
	e.asBridge("POST", fmt.Sprintf("/api/bridge/outbox/%d/result", pid), map[string]any{"state": "failed", "error": "nope"})
	b, _ := os.ReadFile(logFile)
	log := string(b)
	for _, bad := range []string{"ZXQ", "hovor", handle, "Jana Tajná", e.a.bridgeTokenValue(), e.a.agentTokenValue(), "wrong-token-1234"} {
		if strings.Contains(log, bad) {
			t.Errorf("the log holds %q", bad)
		}
	}
	for _, n := range e.notes.all() {
		for _, f := range n {
			if strings.Contains(f, "ZXQ") || strings.Contains(f, handle) {
				t.Errorf("a notice holds chat content: %v", n)
			}
		}
	}
}

// --- checks of what the bridge sends ---

func TestABadBatchIsRefusedWithASentenceAndWritesNothing(t *testing.T) {
	e := newChatEnv(t)
	good := e.in("m1", "x", "X", "ok", 10)
	cases := []struct {
		name string
		body any
	}{
		{"no chat", bridgeBatch{}},
		{"bad network", bridgeBatch{Chats: []bridgeChat{chatOf("What App", "c", "n", "direct", good)}}},
		{"no chat id", bridgeBatch{Chats: []bridgeChat{chatOf("whatsapp", "", "n", "direct", good)}}},
		{"bad type", bridgeBatch{Chats: []bridgeChat{chatOf("whatsapp", "c", "n", "channel", good)}}},
		{"no message id", bridgeBatch{Chats: []bridgeChat{chatOf("whatsapp", "c", "n", "direct", bridgeMessage{At: e.at(1), Text: "x"})}}},
		{"bad time", bridgeBatch{Chats: []bridgeChat{chatOf("whatsapp", "c", "n", "direct", bridgeMessage{ID: "1", At: "yesterday", Text: "x"})}}},
		{"not json", []byte("{")},
	}
	for _, c := range cases {
		rec := e.asBridge("POST", "/api/bridge/batch", c.body)
		if rec.Code != 400 {
			t.Errorf("%s: %d %s", c.name, rec.Code, rec.Body.String())
		}
	}
	if e.a.store.CountRows(e.ctx, "raw") != 0 || e.a.store.CountRows(e.ctx, "items") != 0 {
		t.Error("a refused batch wrote")
	}
	// A time in the future is the time now.
	m := good
	m.At = e.now.Add(48 * time.Hour).UTC().Format(time.RFC3339)
	e.batch(chatOf("whatsapp", "c", "n", "direct", m))
	msg, _ := e.a.store.MessageBySource(e.ctx, "whatsapp", chatSourceID("c", "m1"))
	if msg.ReceivedAt != e.now.Unix() {
		t.Errorf("a message from the future is at %d", msg.ReceivedAt)
	}
}

var _ = context.Background
var _ = http.StatusOK

// SPEC 4: an edited chat message "brings the item back to Claude": a resolved
// item is back in New, waiting for Claude after the chat's quiet time, and an
// edit that only carries the network's edited mark counts too.
func TestSpec4EditedChatMessageBringsAResolvedItemBackToClaude(t *testing.T) {
	e := newChatEnv(t)
	c := chatOf("whatsapp", "c9", "Jana", "direct", e.in("m1", "+420111", "Jana", "Sejdeme se v pět", 300))
	e.batch(c)
	claudeLooksAtAll(t, e.a)
	it := e.itemOfChat("whatsapp", "c9")
	if err := e.a.store.Update(e.ctx, func(tx *repo.Tx) error {
		cur, _ := tx.ItemByID(e.ctx, it.ID)
		cur.Stage = stageResolved
		cur.NeedsClaude = false
		return tx.SaveItem(e.ctx, cur)
	}); err != nil {
		t.Fatal(err)
	}
	e.now = e.now.Add(time.Minute)
	c.Messages[0].Text = "Sejdeme se v šest"
	c.Messages[0].EditedAt = e.at(0)
	e.batch(c)
	got, _ := e.a.store.ItemByID(e.ctx, it.ID)
	if got.Stage != stageNew {
		t.Errorf("an edited message left the item in %q, want new", got.Stage)
	}
	e.now = e.now.Add(31 * time.Minute)
	if ids := e.queueIDs(); len(ids) != 1 || ids[0] != repo.Number(it.Number) {
		t.Errorf("the edited chat is not waiting for Claude: %v", ids)
	}
	// Only the edited mark, same text, on a message not yet marked.
	c2 := chatOf("whatsapp", "c10", "Eva", "direct", e.in("m1", "+420222", "Eva", "Ahoj", 300))
	e.batch(c2)
	it2 := e.itemOfChat("whatsapp", "c10")
	claudeLooksAtAll(t, e.a)
	c2.Messages[0].EditedAt = e.at(0)
	if res := e.batch(c2); res["versions"].(float64) != 1 {
		t.Fatalf("the edited mark: %v", res)
	}
	e.now = e.now.Add(31 * time.Minute)
	found := false
	for _, id := range e.queueIDs() {
		found = found || id == repo.Number(it2.Number)
	}
	if !found {
		t.Error("an edited mark alone did not bring the chat back to Claude")
	}
}

// SPEC 4, test "4: an edited chat message ... goes back to Claude": a chat
// whose newest message is older than the quiet time goes to Claude at once
// when a message in it is edited, from any stage.
func TestSpec4EditOfAQuietChatWaitsForClaudeAtOnce(t *testing.T) {
	for _, stage := range []string{stageResolved, stageClosed, stagePending, stageNew} {
		e := newChatEnv(t)
		c := chatOf("whatsapp", "q1", "Jana", "direct", e.in("m1", "+420111", "Jana", "Sejdeme se v pět", 120))
		e.batch(c)
		claudeLooksAtAll(t, e.a)
		it := e.itemOfChat("whatsapp", "q1")
		if err := e.a.store.Update(e.ctx, func(tx *repo.Tx) error {
			cur, _ := tx.ItemByID(e.ctx, it.ID)
			cur.Stage, cur.NeedsClaude = stage, false
			return tx.SaveItem(e.ctx, cur)
		}); err != nil {
			t.Fatal(err)
		}
		c.Messages[0].Text = "Sejdeme se v šest"
		c.Messages[0].EditedAt = e.at(0)
		if res := e.batch(c); res["versions"].(float64) != 1 {
			t.Fatalf("%s: the edit: %v", stage, res)
		}
		if ids := e.queueIDs(); len(ids) != 1 || ids[0] != repo.Number(it.Number) {
			t.Errorf("%s: an edit of a quiet chat is not waiting for Claude: %v", stage, ids)
		}
	}
}

// A chat still being written in waits for its quiet time, counted from its
// newest message, not from the edit.
func TestSpec4EditOfALiveChatWaitsForTheQuietTime(t *testing.T) {
	e := newChatEnv(t)
	c := chatOf("whatsapp", "q2", "Jana", "direct", e.in("m1", "+420111", "Jana", "Sejdeme se v pět", 2))
	e.batch(c)
	it := e.itemOfChat("whatsapp", "q2")
	c.Messages[0].Text = "Sejdeme se v šest"
	e.batch(c)
	if ids := e.queueIDs(); len(ids) != 0 {
		t.Fatalf("an edit of a live chat went to Claude at once: %v", ids)
	}
	e.now = e.now.Add(29 * time.Minute)
	if ids := e.queueIDs(); len(ids) != 1 || ids[0] != repo.Number(it.Number) {
		t.Errorf("after the quiet time the edited chat is not waiting: %v", ids)
	}
}

// A call carrying a test run's mark is not the real bridge: it moves nothing
// the Connectors page shows (SPEC 16), and a bridge never heard from is "Not
// configured" with no silence notice.
func TestTestRunCallsDoNotMoveWhatTheBridgeRowShows(t *testing.T) {
	e := newChatEnv(t)
	const run = "e2e-abcd1234"
	call := func(method, path string, body any) {
		j, _ := json.Marshal(body)
		req := httptest.NewRequest(method, path, bytes.NewReader(j))
		req.Header.Set("Authorization", "Bearer "+e.a.bridgeTokenValue())
		req.Header.Set("X-Test-Run", run)
		rec := httptest.NewRecorder()
		e.h.ServeHTTP(rec, req)
		if rec.Code != 200 {
			t.Fatalf("%s: %d %s", path, rec.Code, rec.Body.String())
		}
	}
	call("POST", "/api/bridge/alive", map[string]any{"networks": []bridgeNet{{Network: "whatsapp", State: "needs_login"}}, "version": "9.9", "last_error": "boom"})
	call("POST", "/api/bridge/outbox", nil)
	call("POST", "/api/bridge/batch", bridgeBatch{Chats: []bridgeChat{chatOf("whatsapp", "c1", "Jana", "direct", e.in("m1", "+420111", "Jana", "Ahoj", 100))}})
	if e.a.bridgeHeardAt(e.ctx) != 0 {
		t.Fatal("a test call counts as the bridge being heard")
	}
	st := e.a.bridgeState(e.ctx, bridgeSrc)
	if st.Status != statusNotConf || st.Alert || st.Error != "" {
		t.Errorf("never heard: status %q alert %v error %q", st.Status, st.Alert, st.Error)
	}
	conn := get(e.h, "/connectors").Body.String()
	for _, bad := range []string{"Last read: ", "boom", "Bridge version", "1 chat"} {
		if strings.Contains(conn, bad) {
			t.Errorf("the row shows %q after test calls only", bad)
		}
	}
	// Silence of any length raises nothing for a bridge never heard from.
	e.now = e.now.Add(48 * time.Hour)
	if got := alertsFor(e.a.sourceStates(e.ctx)); len(got) != 0 || len(e.notes.all()) != 0 {
		t.Errorf("silence alarm for a bridge never heard from: %v %v", got, e.notes.all())
	}
	// The real bridge still counts.
	e.asBridge("POST", "/api/bridge/alive", map[string]any{"networks": []bridgeNet{{Network: "whatsapp", State: "connected"}}})
	if e.a.bridgeHeardAt(e.ctx) == 0 {
		t.Error("the real bridge is not heard")
	}
	if n := e.a.store.CountChatMessages(e.ctx); n != 0 {
		t.Errorf("%d test messages counted", n)
	}
}
