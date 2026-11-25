package main

import (
	"bytes"
	"fmt"
	"io/fs"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"lab/apps/one/repo"
)

// Tests of what the owner writes in a chat himself (SPEC 17, second paragraph) and
// of the networks that need a new login (SPEC 24). Everything is invented; the
// bridge is played by these tests and nothing reaches a real Beeper.

type upFile struct {
	name string
	data []byte
}

// writeAs is the owner's page posting a message (and files) into a chat.
func (e *chatEnv) writeAs(it repo.Item, text string, files ...upFile) *httptest.ResponseRecorder {
	e.t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	mw.WriteField("text", text)
	for _, f := range files {
		h := textproto.MIMEHeader{}
		h.Set("Content-Disposition", fmt.Sprintf(`form-data; name="file"; filename="%s"`, f.name))
		h.Set("Content-Type", "application/octet-stream")
		w, _ := mw.CreatePart(h)
		w.Write(f.data)
	}
	mw.Close()
	req := httptest.NewRequest("POST", fmt.Sprintf("/items/%d/write", it.ID), &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("X-Requested-With", "one")
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	return rec
}

// macIsHere is the bridge being heard just now.
func (e *chatEnv) macIsHere() {
	e.t.Helper()
	if rec := e.asBridge("POST", "/api/bridge/alive", map[string]any{"networks": []bridgeNet{{Network: "messenger", State: "connected"}}}); rec.Code != 200 {
		e.t.Fatalf("alive: %d", rec.Code)
	}
}

func (e *chatEnv) posts() []map[string]any {
	e.t.Helper()
	rec := e.asBridge("POST", "/api/bridge/outbox", nil)
	if rec.Code != 200 {
		e.t.Fatalf("outbox: %d %s", rec.Code, rec.Body.String())
	}
	var out []map[string]any
	for _, p := range answerOf(e.t, rec)["posts"].([]any) {
		out = append(out, p.(map[string]any))
	}
	return out
}

func (e *chatEnv) postResult(id any, state, errText, msg string) *httptest.ResponseRecorder {
	return e.asBridge("POST", fmt.Sprintf("/api/bridge/posts/%v/result", id), map[string]any{"state": state, "error": errText, "message_id": msg})
}

func (e *chatEnv) count(table string) int { return e.a.store.CountRows(e.ctx, table) }

func TestOwnerWritesInAChatFromThePageAndEveryAddressUnderApiRefusesIt(t *testing.T) {
	e := newChatEnv(t)
	it := e.chatItemForReply("messenger", "c1", "Ondra", "Pošli adresu")
	page := e.rowPage(it.ID)
	for _, want := range []string{`placeholder="Write in Messenger…"`, ">Send</button>", ">Attach</button>"} {
		if !strings.Contains(page, want) {
			t.Errorf("the box lacks %q:\n%s", want, page)
		}
	}
	if strings.Contains(page, "Approve") && strings.Contains(page, "chatbox") && regexp.MustCompile(`(?s)class="say chatbox".*?Approve`).MatchString(page) {
		t.Error("the box has an Approve")
	}
	// From another site: refused by the app's cross-site guard, nothing stored.
	req := httptest.NewRequest("POST", fmt.Sprintf("/items/%d/write", it.ID), strings.NewReader("text=x"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	if rec.Code != 403 || e.count("chat_out") != 0 {
		t.Errorf("a cross-site post: %d, %d parts", rec.Code, e.count("chat_out"))
	}
	// From the signed-in page: accepted.
	if rec := e.writeAs(it, "Cabin 12."); rec.Code != 200 {
		t.Fatalf("the page: %d %s", rec.Code, rec.Body.String())
	}
	if e.count("chat_out") != 1 {
		t.Fatalf("parts: %d", e.count("chat_out"))
	}
	// Under /api/ nothing creates, changes or triggers such a message, with either token.
	for _, path := range []string{fmt.Sprintf("/api/items/%d/write", it.ID), "/api/items/Q-0001/write", "/api/items/1/say", "/api/chat-messages", "/api/items/1/chat-message", "/api/bridge/write", "/api/bridge/posts", "/api/writes/1/withdraw"} {
		for name, tok := range map[string]string{"agent": e.a.agentTokenValue(), "bridge": e.a.bridgeTokenValue(), "none": ""} {
			for _, method := range []string{"POST", "PUT", "GET"} {
				rec := e.call(method, path, tok, map[string]any{"text": "Z agentu", "item": "Q-0001"})
				if rec.Code < 400 {
					t.Errorf("%s %s with the %s token answered %d", method, path, name, rec.Code)
				}
			}
		}
	}
	if e.count("chat_out") != 1 {
		t.Errorf("something under /api/ made a message: %d parts", e.count("chat_out"))
	}
	// An agent's reply proposal is not the owner's write, and no connector tool writes in a chat.
	rec = e.call("POST", "/api/mcp", e.a.agentTokenValue(), map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/list"})
	for _, bad := range []string{"write_chat", "send_chat", "chat_message", "post_message", "send_message"} {
		if strings.Contains(rec.Body.String(), bad) {
			t.Errorf("the connector has a tool %q", bad)
		}
	}
	if proposeReplyAs(t, e.h, e.a, it, "Z agentu"); len(e.posts()) != 1 {
		t.Error("a reply proposal reached the bridge as a message of the owner's, or his message was lost")
	}
}

func TestAChatMessageOwnerWritesIsHandedOutOnceShownAsSendingThenSentAndIsOneMessageWhenItComesBack(t *testing.T) {
	e := newChatEnv(t)
	it := e.chatItemForReply("messenger", "c1", "Ondra", "Pošli adresu")
	e.macIsHere()
	before := e.count("messages")
	const text = "Cabin 12, Springfield.\n  Přijď v osm."
	rec := e.writeAs(it, text)
	if rec.Code != 200 {
		t.Fatalf("write: %d %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "Sending…") || !strings.Contains(body, "Cabin 12, Springfield.") || strings.Contains(body, "Waiting for the Mac") {
		t.Errorf("the chat does not show it going out at once:\n%s", body)
	}
	// It does not make the chat wait for Claude, and the item stays the owner's until it has gone.
	cur, _ := e.a.store.ItemByID(e.ctx, it.ID)
	if cur.NeedsClaude || cur.Stage != stageInProgress {
		t.Fatalf("the item: needs Claude %v, stage %s", cur.NeedsClaude, cur.Stage)
	}
	if ids := e.queueIDs(); len(ids) != 0 {
		t.Errorf("Claude's queue: %v", ids)
	}
	// Handed out once, with the exact text.
	posts := e.posts()
	if len(posts) != 1 || posts[0]["text"] != text || posts[0]["chat_id"] != "c1" || posts[0]["network"] != "messenger" || posts[0]["kind"] != "text" {
		t.Fatalf("handed out: %v", posts)
	}
	for i := 0; i < 3; i++ {
		if again := e.posts(); len(again) != 0 {
			t.Fatalf("handed out again: %v", again)
		}
	}
	if page := e.rowPage(it.ID); !strings.Contains(page, "Sending…") || strings.Contains(page, "Withdraw") {
		t.Errorf("after hand-out:\n%s", page)
	}
	// Reported once; a second report changes nothing.
	e.now = e.now.Add(time.Minute)
	id := posts[0]["id"]
	if rec := e.postResult(id, "sent", "", "bm-9"); rec.Code != 200 || answerOf(t, rec)["state"] != "sent" {
		t.Fatalf("report: %d %s", rec.Code, rec.Body.String())
	}
	events := e.count("events")
	for _, st := range []string{"sent", "failed"} {
		if rec := e.postResult(id, st, "late", ""); rec.Code != 200 || answerOf(t, rec)["unchanged"] != true {
			t.Errorf("a second report %s: %d %s", st, rec.Code, rec.Body.String())
		}
	}
	if e.count("events") != events {
		t.Error("a second report wrote to the record")
	}
	page := e.rowPage(it.ID)
	if !strings.Contains(page, "Sent "+e.now.In(e.a.loc).Format("15:04")+".") || strings.Contains(page, "Sending…") {
		t.Errorf("sent:\n%s", page)
	}
	if cur, _ := e.a.store.ItemByID(e.ctx, it.ID); cur.Stage != stagePending || cur.NeedsClaude {
		t.Errorf("a message that went out left the item %s (needs Claude %v)", cur.Stage, cur.NeedsClaude)
	}
	if len(e.notes.all()) != 0 {
		t.Errorf("notices: %v", e.notes.all())
	}
	// It comes back from Beeper through the bridge: the same one message, not a second.
	e.batch(chatOf("messenger", "c1", "Ondra", "direct", e.in("m1", "p.1", "Ondra", "Pošli adresu", 120), e.mine("bm-9", text, 0)))
	if got := e.count("messages") - before; got != 1 {
		t.Errorf("the message came back as %d messages", got)
	}
	page = e.rowPage(it.ID)
	if n := strings.Count(page, `Přijď v osm.</div>`); n != 1 {
		t.Errorf("the text is shown %d times:\n%s", n, page)
	}
	if strings.Contains(page, "Sending…") || strings.Contains(page, "Sent "+e.now.In(e.a.loc).Format("15:04")+".") {
		t.Errorf("the pending copy is still shown:\n%s", page)
	}
	if cur, _ := e.a.store.ItemByID(e.ctx, it.ID); cur.NeedsClaude {
		t.Error("his own message, come back, woke the review")
	}
	// The same message posted again changes nothing (the bridge may send it twice).
	e.batch(chatOf("messenger", "c1", "Ondra", "direct", e.mine("bm-9", text, 0)))
	if got := e.count("messages") - before; got != 1 {
		t.Errorf("after the same batch twice: %d messages", got)
	}
	// If the network gave it another id, it is still the one message (matched by chat, time and text).
	e.writeAs(it, "Druhá zpráva")
	p2 := e.posts()
	e.postResult(p2[0]["id"], "sent", "", "pending-77")
	e.batch(chatOf("messenger", "c1", "Ondra", "direct", e.mine("final-1", "Druhá zpráva", 0)))
	if got := strings.Count(e.rowPage(it.ID), `Druhá zpráva</div>`); got != 1 {
		t.Errorf("a message with another id is shown %d times", got)
	}
}

func TestWhileTheMacIsNotHeardAMessageWaitsWithWithdrawAndWithdrawBeforeHandOutStopsIt(t *testing.T) {
	e := newChatEnv(t)
	it := e.chatItemForReply("whatsapp", "c1", "Jana", "Jdeš dnes?")
	e.now = e.now.Add(5 * time.Minute) // the batch that brought the chat was the last time the Mac was heard
	rec := e.writeAs(it, "Jdu v šest.")
	body := rec.Body.String()
	if !strings.Contains(body, "Waiting for the Mac to send this.") || !strings.Contains(body, "Withdraw") || strings.Contains(body, "Sending…") {
		t.Fatalf("the Mac has not been heard from:\n%s", body)
	}
	group := regexp.MustCompile(`/writes/([0-9a-f]{16})/withdraw`).FindStringSubmatch(body)
	if group == nil {
		t.Fatal("no Withdraw form")
	}
	rec = e.ajaxPost("/writes/"+group[1]+"/withdraw", nil)
	if rec.Code != 200 || strings.Contains(rec.Body.String(), "Jdu v šest.") {
		t.Fatalf("withdraw: %d\n%s", rec.Code, rec.Body.String())
	}
	if posts := e.posts(); len(posts) != 0 {
		t.Fatalf("a withdrawn message was handed out: %v", posts)
	}
	if page := e.rowPage(it.ID); !strings.Contains(page, "You withdrew a message you wrote.") || strings.Contains(page, "Waiting for the Mac") {
		t.Errorf("the thread:\n%s", page)
	}
	// The bridge cannot report on what it never had.
	var pid int64
	e.a.store.Update(e.ctx, func(tx *repo.Tx) error { ps, _ := tx.OutByGroup(e.ctx, group[1]); pid = ps[0].ID; return nil })
	if rec := e.postResult(pid, "sent", "", ""); rec.Code != 409 {
		t.Errorf("a report on a message never handed out: %d", rec.Code)
	}
	// Another one, handed out: it can no longer be withdrawn.
	e.writeAs(it, "Ještě jedna")
	posts := e.posts()
	if len(posts) != 1 {
		t.Fatalf("%v", posts)
	}
	var g2 string
	e.a.store.Update(e.ctx, func(tx *repo.Tx) error {
		p, _ := tx.OutByID(e.ctx, int64(posts[0]["id"].(float64)))
		g2 = p.Group
		return nil
	})
	if rec := e.ajaxPost("/writes/"+g2+"/withdraw", nil); rec.Code != 409 {
		t.Errorf("withdraw after hand-out: %d", rec.Code)
	}
	if rec := e.ajaxPost("/writes/nothexnothexnothe/withdraw", nil); rec.Code != 404 {
		t.Errorf("a made-up group: %d", rec.Code)
	}
}

func (e *chatEnv) ajaxPost(path string, form map[string][]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("POST", path, strings.NewReader(""))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Requested-With", "one")
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	return rec
}

func TestAMessageOwnerWroteThatCouldNotBeSentSaysWhyAndSendsOneNotice(t *testing.T) {
	e := newChatEnv(t)
	it := e.chatItemForReply("messenger", "c1", "Ondra", "Pošli adresu")
	e.macIsHere()
	e.writeAs(it, "První", upFile{"plan.pdf", []byte("%PDF invented")}, upFile{"foto.png", []byte("PNG invented")})
	posts := e.posts()
	if len(posts) != 3 {
		t.Fatalf("%v", posts)
	}
	e.postResult(posts[0]["id"], "sent", "", "bm-1")
	e.postResult(posts[1]["id"], "failed", "Beeper refused the message (answered 404).", "")
	body := e.rowPage(it.ID)
	if !strings.Contains(body, "Could not send: plan.pdf: Beeper refused the message (answered 404).") {
		t.Errorf("the failure does not say which file:\n%s", body)
	}
	// The bridge does not send the message after it, and says why.
	e.postResult(posts[2]["id"], "failed", "Not sent: the message before it did not go.", "")
	body = e.rowPage(it.ID)
	if !strings.Contains(body, "Could not send: foto.png: Not sent: the message before it did not go.") {
		t.Errorf("the next file:\n%s", body)
	}
	got := e.notes.all()
	if len(got) != 1 || got[0][0] != "Could not send" || strings.Contains(got[0][1], "plan.pdf") || strings.Contains(got[0][1], "Ondra") || strings.Contains(got[0][1], "První") {
		t.Errorf("notices: %v", got)
	}
	// The one after it did not go either: still one notice for the write.
	if len(e.notes.all()) != 1 {
		t.Errorf("a second notice: %v", e.notes.all())
	}
	if cur, _ := e.a.store.ItemByID(e.ctx, it.ID); cur.Stage != stageInProgress {
		t.Errorf("the item is %s", cur.Stage)
	}
}

func TestFiveFilesPassAndASixthOrATooLargeOneIsRefusedWithAPlainSentence(t *testing.T) {
	e := newChatEnv(t)
	it := e.chatItemForReply("messenger", "c1", "Ondra", "Pošli fotky")
	files := func(n int) []upFile {
		var out []upFile
		for i := 0; i < n; i++ {
			out = append(out, upFile{fmt.Sprintf("foto%d.jpg", i), []byte(fmt.Sprintf("jpeg invented %d", i))})
		}
		return out
	}
	rec := e.writeAs(it, "", files(6)...)
	if rec.Code != 400 || strings.TrimSpace(rec.Body.String()) != "At most 5 files can be sent at once." || e.count("chat_out") != 0 {
		t.Errorf("six files: %d %q, %d parts", rec.Code, rec.Body.String(), e.count("chat_out"))
	}
	big := make([]byte, 20<<20+1)
	rec = e.writeAs(it, "x", upFile{"velky.bin", big})
	if rec.Code != 400 || strings.TrimSpace(rec.Body.String()) != "velky.bin is larger than 20 MB." || e.count("chat_out") != 0 {
		t.Errorf("a file over 20 MB: %d %q", rec.Code, rec.Body.String())
	}
	if rec = e.writeAs(it, "", upFile{"prazdny.txt", nil}); rec.Code != 400 || !strings.Contains(rec.Body.String(), "prazdny.txt is empty.") {
		t.Errorf("an empty file: %d %q", rec.Code, rec.Body.String())
	}
	if rec = e.writeAs(it, "  \n "); rec.Code != 400 || strings.TrimSpace(rec.Body.String()) != "Write something first." {
		t.Errorf("nothing: %d %q", rec.Code, rec.Body.String())
	}
	// Exactly 20 MB and exactly five files go.
	exact := make([]byte, 20<<20)
	if rec = e.writeAs(it, "", upFile{"presne.bin", exact}); rec.Code != 200 {
		t.Errorf("a file of exactly 20 MB: %d %s", rec.Code, rec.Body.String())
	}
	if rec = e.writeAs(it, "pět", files(5)...); rec.Code != 200 || e.count("chat_out") != 1+6 {
		t.Errorf("five files: %d, %d parts", rec.Code, e.count("chat_out"))
	}
}

func TestEachFileGoesAsItsOwnMessageAfterTheTextInOrderAndIsListedUnderAttachmentsWithWhoAddedIt(t *testing.T) {
	e := newChatEnv(t)
	it := e.chatItemForReply("messenger", "c1", "Ondra", "Pošli plán")
	e.macIsHere()
	same := []byte("the same bytes, invented")
	e.writeAs(it, "Plán v příloze", upFile{"plan.pdf", same}, upFile{"kopie.pdf", same}, upFile{"foto.png", []byte("PNG invented")})
	posts := e.posts()
	kinds := []string{}
	for i, p := range posts {
		kinds = append(kinds, fmt.Sprintf("%v:%v", p["seq"], p["kind"]))
		if int(p["seq"].(float64)) != i {
			t.Errorf("out of order: %v", posts)
		}
	}
	if strings.Join(kinds, ",") != "0:text,1:file,2:file,3:file" {
		t.Fatalf("handed out: %v", kinds)
	}
	if f := posts[1]["file"].(map[string]any); f["name"] != "plan.pdf" || f["size"].(float64) != float64(len(same)) {
		t.Errorf("the file: %v", f)
	}
	// The bridge fetches each with its own token.
	for i, want := range [][]byte{nil, same, same, []byte("PNG invented")} {
		if i == 0 {
			continue
		}
		rec := e.asBridge("GET", fmt.Sprintf("/api/bridge/posts/%v/file", posts[i]["id"]), nil)
		if rec.Code != 200 || !bytes.Equal(rec.Body.Bytes(), want) {
			t.Errorf("file %d: %d %q", i, rec.Code, rec.Body.String())
		}
	}
	// Stored once by hash: the two files with the same bytes are one file in the pool.
	n := 0
	filepath.WalkDir(e.a.imp.pool.root, func(p string, d fs.DirEntry, err error) error {
		if rel, _ := filepath.Rel(e.a.imp.pool.root, p); err == nil && d.Type().IsRegular() && !strings.HasPrefix(rel, "tmp") {
			n++
		}
		return nil
	})
	if n != 2 {
		t.Errorf("the pool holds %d files for 3 uploads of 2 different contents", n)
	}
	// Listed under Attachments, with who added them, as downloads.
	page := e.rowPage(it.ID)
	if !strings.Contains(page, ">Attachments</h3>") || strings.Count(page, "added by You") != 3 || !strings.Contains(page, "plan.pdf") || !strings.Contains(page, "foto.png") {
		t.Errorf("the Attachments list:\n%s", page)
	}
	m := regexp.MustCompile(`href="(/files/\d+)" download><span class="fname">foto.png`).FindStringSubmatch(page)
	if m == nil {
		t.Fatalf("no download chip for foto.png:\n%s", page)
	}
	if rec := get(e.h, m[1]); rec.Code != 200 || rec.Body.String() != "PNG invented" || !strings.HasPrefix(rec.Header().Get("Content-Disposition"), "attachment") {
		t.Errorf("download: %d %v", rec.Code, rec.Header())
	}
	// The sent file comes back from Beeper as a message with an attachment: still one entry, and the bytes are the ones ONE holds.
	for i := 0; i < 4; i++ {
		e.postResult(posts[i]["id"], "sent", "", fmt.Sprintf("bm-%d", i))
	}
	back := e.mine("bm-1", "", 0)
	back.Attachments = []bridgeAttachment{{ID: "att-1", Name: "plan.pdf", Type: "application/pdf", Size: int64(len(same))}}
	before := e.count("files")
	res := e.batch(chatOf("messenger", "c1", "Ondra", "direct", e.in("m1", "p.1", "Ondra", "Pošli plán", 120), e.mine("bm-0", "Plán v příloze", 0), back))
	if missing := res["missing_files"].([]any); len(missing) != 0 {
		t.Errorf("ONE asks the bridge for a file it already holds: %v", missing)
	}
	_ = before
	page = e.rowPage(it.ID)
	if strings.Count(page, "plan.pdf") != 2 { // the message's own chip, and the Attachments list; not a third copy
		t.Errorf("plan.pdf is shown %d times:\n%s", strings.Count(page, "plan.pdf"), page)
	}
}

func TestTheBridgeCanFetchOnlyFilesOfMessagesHandedToIt(t *testing.T) {
	e := newChatEnv(t)
	it := e.chatItemForReply("messenger", "c1", "Ondra", "Pošli plán")
	e.macIsHere()
	e.writeAs(it, "text", upFile{"plan.pdf", []byte("PDF invented")})
	var filePart, textPart int64
	e.a.store.Update(e.ctx, func(tx *repo.Tx) error {
		ps, _ := tx.OutOfItem(e.ctx, it.ID)
		textPart, filePart = ps[0].ID, ps[1].ID
		return nil
	})
	path := func(id int64) string { return fmt.Sprintf("/api/bridge/posts/%d/file", id) }
	// Not handed out yet: not there.
	if rec := e.asBridge("GET", path(filePart), nil); rec.Code != 404 {
		t.Errorf("a file that was not handed out: %d", rec.Code)
	}
	e.posts()
	if rec := e.asBridge("GET", path(filePart), nil); rec.Code != 200 || rec.Body.String() != "PDF invented" {
		t.Errorf("a file that was handed out: %d", rec.Code)
	}
	// The agents' token and no token open nothing, and the bridge token opens only this address.
	for _, tok := range []string{e.a.agentTokenValue(), ""} {
		if rec := e.bridgeCall("GET", path(filePart), tok, nil); rec.Code != 401 {
			t.Errorf("token %q: %d", tok, rec.Code)
		}
	}
	// A text part is no file; a number that is not a part is none; an attachment of a mail or a chat message is not one of these.
	for _, id := range []int64{textPart, 999, 0} {
		if rec := e.asBridge("GET", path(id), nil); rec.Code != 404 {
			t.Errorf("part %d: %d", id, rec.Code)
		}
	}
	var someFile int64
	e.a.store.Update(e.ctx, func(tx *repo.Tx) error {
		ps, _ := tx.OutOfItem(e.ctx, it.ID)
		someFile = ps[1].FileID
		return nil
	})
	if rec := e.asBridge("GET", path(someFile+100), nil); rec.Code != 404 {
		t.Errorf("a file number: %d", rec.Code)
	}
	// Once reported, it is no longer there.
	e.postResult(filePart, "sent", "", "bm-1")
	if rec := e.asBridge("GET", path(filePart), nil); rec.Code != 404 {
		t.Errorf("a file of a message already reported: %d", rec.Code)
	}
}

func TestMailRowsHaveNoBoxToWriteInAndMailTakesNoWrite(t *testing.T) {
	e := newChatEnv(t)
	it := e.seed(stageInProgress, seedMsg{subject: "Faktura", name: "Alice", sender: "alice@example.test", text: "Prosím o zaplacení.", at: e.ago(1)})
	page := e.rowPage(it.ID)
	if strings.Contains(page, "Write in") || strings.Contains(page, "chatbox") {
		t.Errorf("a mail row has the chat box:\n%s", page)
	}
	if strings.Count(page, ">Attach</span></button>") != 1 { // the note box has Attach on every row (SPEC 7)
		t.Errorf("a mail row should have Attach in its note box only:\n%s", page)
	}
	rec := e.writeAs(it, "Odpověď")
	if rec.Code != 400 || !strings.Contains(rec.Body.String(), "Only a chat takes a message here") || e.count("chat_out") != 0 {
		t.Errorf("a write to mail: %d %q", rec.Code, rec.Body.String())
	}
	// A chat that is still hidden from the owner takes none either.
	if rec := e.writeAs(repo.Item{ID: 99999}, "x"); rec.Code != 404 {
		t.Errorf("an item that is not there: %d", rec.Code)
	}
}

func TestANetworkThatNeedsALoginIsNamedOnTheConnectorsRowAndOneNoticeIsSent(t *testing.T) {
	e := newChatEnv(t)
	alive := func(status, state string) {
		e.t.Helper()
		n := []bridgeNet{{Network: "whatsapp", State: "connected", Status: "connected"}, {Network: "messenger", State: state, Status: status, StatusText: "Sign in again"}}
		if rec := e.asBridge("POST", "/api/bridge/alive", map[string]any{"networks": n}); rec.Code != 200 {
			e.t.Fatalf("alive: %d", rec.Code)
		}
	}
	alive("connected", "connected")
	if page := string(get(e.h, "/connectors").Body.Bytes()); strings.Contains(page, "needs a new login") {
		t.Fatalf("all fine, yet:\n%s", page)
	}
	e.now = e.now.Add(time.Minute)
	alive("connection_required", "connected") // Beeper's own status decides, whatever state was sent
	page := get(e.h, "/connectors").Body.String()
	for _, want := range []string{"Messenger", "Needs login", "needs a new login in Beeper.", "Beeper says: Sign in again", "Needed a new login once in the last 30 days"} {
		if !strings.Contains(page, want) {
			t.Errorf("the Connectors row lacks %q:\n%s", want, page)
		}
	}
	// The strip, the dot and one notice.
	home := get(e.h, "/").Body.String()
	if !strings.Contains(home, "Chat bridge needs login. See Connectors.") || !strings.Contains(home, "a source has stopped") {
		t.Errorf("the strip or the dot:\n%s", home)
	}
	got := e.notes.all()
	if len(got) != 1 || got[0][0] != "Chat bridge needs login" || !strings.Contains(got[0][1], "Messenger") || strings.Contains(got[0][1], "WhatsApp") {
		t.Fatalf("notices: %v", got)
	}
	for i := 0; i < 3; i++ { // reported again and again: still one notice, still one login counted
		e.now = e.now.Add(time.Minute)
		alive("reconnect_required", "needs_login")
	}
	if len(e.notes.all()) != 1 {
		t.Errorf("notices: %v", e.notes.all())
	}
	if !strings.Contains(get(e.h, "/connectors").Body.String(), "Needed a new login once") {
		t.Error("a network that stayed logged out was counted again")
	}
	// It is fine again, and then needs a login again: a second event, dated, and a second notice.
	e.now = e.now.Add(time.Minute)
	alive("connected", "connected")
	e.a.checkSources(e.ctx)
	e.now = e.now.Add(time.Hour)
	alive("attention_required", "needs_login")
	page = get(e.h, "/connectors").Body.String()
	if !strings.Contains(page, "Needed a new login 2 times in the last 30 days (2 times in all counted), last on "+e.now.In(e.a.loc).Format("2 Jan 15:04")+".") {
		t.Errorf("the dated count:\n%s", page)
	}
	if got := e.notes.all(); len(got) != 2 {
		t.Errorf("notices: %v", got)
	}
	// A network switched off in Beeper is not a login.
	e.now = e.now.Add(time.Minute)
	alive("connected", "connected")
	e.a.checkSources(e.ctx)
	if rec := e.asBridge("POST", "/api/bridge/alive", map[string]any{"networks": []bridgeNet{{Network: "signal", State: "connected", Status: "disabled"}}}); rec.Code != 200 {
		t.Fatal(rec.Code)
	}
	if page := get(e.h, "/connectors").Body.String(); !strings.Contains(page, "is switched off in Beeper.") || len(e.notes.all()) != 2 {
		t.Errorf("a network switched off:\n%s\n%v", page, e.notes.all())
	}
}

func TestADeletedMessageStaysMarkedDeletedAndAnEditIsShownAsEdited(t *testing.T) {
	e := newChatEnv(t)
	it := e.chatItemForReply("messenger", "c1", "Ondra", "Dnes ne")
	m := e.in("m1", "p.1", "Ondra", "Dnes ne", 120)
	m.EditedAt = "2026-11-19T11:00:00Z"
	m.Text = "Dnes ano"
	res := e.batch(chatOf("messenger", "c1", "Ondra", "direct", m))
	if res["versions"].(float64) != 1 {
		t.Fatalf("the edit: %v", res)
	}
	if page := e.rowPage(it.ID); !strings.Contains(page, "Dnes ano") || !strings.Contains(page, `<span class="edited">edited</span>`) {
		t.Errorf("edited:\n%s", page)
	}
	del := e.in("m1", "p.1", "Ondra", "", 120)
	del.EditedAt, del.Deleted = m.EditedAt, true
	res = e.batch(chatOf("messenger", "c1", "Ondra", "direct", del))
	if res["versions"].(float64) != 1 || res["messages"].(float64) != 1 {
		t.Fatalf("the deletion: %v", res)
	}
	page := e.rowPage(it.ID)
	if !strings.Contains(page, "Dnes ano") || !strings.Contains(page, ">deleted</span>") {
		t.Errorf("a deleted message must stay, marked:\n%s", page)
	}
	if e.count("messages") != 1 {
		t.Errorf("a deleted message became %d messages", e.count("messages"))
	}
	// The same deletion again writes nothing.
	events := e.count("raw")
	e.batch(chatOf("messenger", "c1", "Ondra", "direct", del))
	if e.count("raw") != events {
		t.Error("the same version was written twice")
	}
}

func TestTheServerTellsTheBridgeHowFarBackToReadAndWhichFileSizeItTakes(t *testing.T) {
	e := newChatEnv(t)
	rec := e.asBridge("POST", "/api/bridge/alive", map[string]any{"networks": []bridgeNet{}})
	ans := answerOf(t, rec)
	if ans["backfill_days"].(float64) != 30 || ans["max_file_bytes"].(float64) != float64(1<<30) {
		t.Errorf("alive: %v", ans)
	}
	ans = answerOf(t, e.asBridge("POST", "/api/bridge/outbox", nil))
	if ans["backfill_days"].(float64) != 30 || ans["max_file_bytes"].(float64) != float64(1<<30) {
		t.Errorf("outbox: %v", ans)
	}
}

var _ = http.StatusOK
var _ = os.Getenv

// noteAs posts a note with files to the thread box of a row, as the page does.
func (e *chatEnv) noteAs(it repo.Item, text string, files ...upFile) *httptest.ResponseRecorder {
	e.t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	mw.WriteField("text", text)
	for _, f := range files {
		h := textproto.MIMEHeader{}
		h.Set("Content-Disposition", fmt.Sprintf(`form-data; name="file"; filename="%s"`, f.name))
		h.Set("Content-Type", "application/octet-stream")
		w, _ := mw.CreatePart(h)
		w.Write(f.data)
	}
	mw.Close()
	req := httptest.NewRequest("POST", fmt.Sprintf("/items/%d/notes", it.ID), &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("X-Requested-With", "one")
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	return rec
}

// SPEC 7: the note box of every row has Attach; a file attached to a note is
// listed under Attachments (added by You) and the entry says "Attached: <name>".
func TestSpec7NoteBoxAttachesFilesOnEveryRow(t *testing.T) {
	e := newChatEnv(t)
	it := e.chatItemForReply("whatsapp", "n1", "Jana", "Ahoj")
	page := e.rowPage(it.ID)
	if n := strings.Count(page, ">Attach<"); n != 2 {
		t.Errorf("a chat row should have Attach in its chat box and its note box, found %d", n)
	}
	rec := e.noteAs(it, "Here is the plan", upFile{"plan.pdf", []byte("%PDF-1 invented")})
	if rec.Code != 200 {
		t.Fatalf("note with a file: %d %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "Attachments") || !strings.Contains(body, "plan.pdf") || !strings.Contains(body, "Attached: <span class=\"fname\">plan.pdf</span>") || !strings.Contains(body, "added by You") {
		t.Errorf("the row does not list the file and the entry:\n%s", body)
	}
	// a file alone is a note; six files are refused, nothing is written
	if rec := e.noteAs(it, "", upFile{"a.txt", []byte("a")}); rec.Code != 200 {
		t.Errorf("a file alone: %d", rec.Code)
	}
	var six []upFile
	for i := 0; i < 6; i++ {
		six = append(six, upFile{fmt.Sprintf("f%d.txt", i), []byte("x")})
	}
	if rec := e.noteAs(it, "too many", six...); rec.Code != 400 {
		t.Errorf("six files: %d", rec.Code)
	}
}
