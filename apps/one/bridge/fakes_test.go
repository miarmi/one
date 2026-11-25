package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// A fake Beeper and a fake ONE, both invented. Nothing here reaches a real
// chat, and every name, text and token is made up.
//
// The fake Beeper is as strict as Beeper's documentation and a little more:
// accounts are a bare array; lists page with cursor and direction and say
// hasMore; messages are read with nothing but cursor and direction (a `limit`
// is refused); an attachment's `size` is an object and its bytes are
// `fileSize`; a file is fetched with the download call, which answers with a
// local file, and sent with the upload call, whose uploadID is used once; a
// send answers a pendingMessageID and the chatID, and an unknown field in the
// body is refused.

const (
	tokBeeper = "beeper-token-AAAA1111BBBB2222"
	tokOne    = "one-token-CCCC3333DDDD4444"
	secretTxt = "ZXQ-hemlig-text-4711"
)

type sentRec struct {
	Chat, Text, UploadID, FileName string
}

type uploadRec struct {
	Name  string
	Bytes []byte
	Used  bool
}

type fakeBeeper struct {
	mu       sync.Mutex
	t        *testing.T
	accounts []bAccount
	chats    []bChat
	msgs     map[string][]bMessage
	keys     map[string]string // message id -> sortKey, kept across edits
	nextKey  int
	pageSize int // messages per page
	chatPage int // chats per page (0: all)
	reqs     []string
	sent     []sentRec
	sendFail int                     // status to answer sends with, 0 = ok
	status   map[string]*bSendStatus // by pending id; nil entry: SUCCESS
	gone     map[string]bool         // pending ids that answer 404 afterwards
	media    map[string]string       // mxc address -> local file
	uploads  map[string]*uploadRec
	nUpload  int
	nPending int
	srv      *httptest.Server
}

func newFakeBeeper(t *testing.T) *fakeBeeper {
	f := &fakeBeeper{t: t, msgs: map[string][]bMessage{}, keys: map[string]string{}, pageSize: 2, status: map[string]*bSendStatus{}, gone: map[string]bool{},
		media: map[string]string{}, uploads: map[string]*uploadRec{},
		accounts: []bAccount{{AccountID: "whatsapp", Network: "WhatsApp", Status: "connected"}, {AccountID: "facebookgo", Network: "Facebook/Messenger", Status: "connected"}}}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeBeeper) bad(w http.ResponseWriter, why string) {
	f.t.Errorf("the bridge asked Beeper in a way its documentation does not allow: %s", why)
	http.Error(w, why, 400)
}

func okKeys(r *http.Request, allowed ...string) (string, bool) {
	for k := range r.URL.Query() {
		found := false
		for _, a := range allowed {
			found = found || a == k
		}
		if !found {
			return k, false
		}
	}
	return "", true
}

func cursorOf(key string) string  { return "c-" + key }
func keyOfCursor(c string) string { return strings.TrimPrefix(c, "c-") }

func (f *fakeBeeper) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Header.Get("Authorization") != "Bearer "+tokBeeper {
		http.Error(w, "no", 401)
		return
	}
	f.reqs = append(f.reqs, r.Method+" "+r.URL.RequestURI())
	p := r.URL.Path
	switch {
	case p == "/v1/accounts" && r.Method == "GET":
		json.NewEncoder(w).Encode(f.accounts) // a bare array
	case p == "/v1/chats" && r.Method == "GET":
		if k, ok := okKeys(r, "limit", "cursor", "direction", "accountIDs"); !ok {
			f.bad(w, "chats: "+k)
			return
		}
		if l, err := strconv.Atoi(r.URL.Query().Get("limit")); r.URL.Query().Has("limit") && (err != nil || l < 1 || l > 200) {
			f.bad(w, "chats: limit out of 1 to 200")
			return
		}
		f.chatsPage(w, r)
	case strings.HasPrefix(p, "/v1/chats/") && strings.HasSuffix(p, "/messages") && r.Method == "GET":
		if k, ok := okKeys(r, "cursor", "direction"); !ok {
			f.bad(w, "messages: undocumented parameter "+k)
			return
		}
		id := strings.TrimSuffix(strings.TrimPrefix(p, "/v1/chats/"), "/messages")
		f.messagesPage(w, r, id)
	case strings.HasPrefix(p, "/v1/chats/") && strings.Contains(p, "/messages/") && r.Method == "GET":
		rest := strings.TrimPrefix(p, "/v1/chats/")
		i := strings.Index(rest, "/messages/")
		chat, mid := rest[:i], rest[i+len("/messages/"):]
		f.oneMessage(w, chat, mid)
	case strings.HasPrefix(p, "/v1/chats/") && strings.HasSuffix(p, "/messages") && r.Method == "POST":
		f.send(w, r, strings.TrimSuffix(strings.TrimPrefix(p, "/v1/chats/"), "/messages"))
	case p == "/v1/assets/download" && r.Method == "POST":
		var in struct{ URL string }
		dec := json.NewDecoder(r.Body)
		dec.DisallowUnknownFields()
		if err := dec.Decode(&in); err != nil || !(strings.HasPrefix(in.URL, "mxc://") || strings.HasPrefix(in.URL, "localmxc://")) {
			f.bad(w, "download: the body is {url: an mxc address}")
			return
		}
		local, ok := f.media[in.URL]
		if !ok {
			json.NewEncoder(w).Encode(map[string]any{"error": "not found"})
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"srcURL": "file://" + local})
	case p == "/v1/assets/upload" && r.Method == "POST":
		f.upload(w, r)
	default:
		f.bad(w, "an address that is not documented: "+r.Method+" "+p)
	}
}

func (f *fakeBeeper) chatsPage(w http.ResponseWriter, r *http.Request) {
	items := f.chats
	start, end := 0, len(items)
	dir := r.URL.Query().Get("direction")
	if dir != "" && dir != "before" && dir != "after" {
		f.bad(w, "chats: direction")
		return
	}
	if c := r.URL.Query().Get("cursor"); c != "" {
		n, _ := strconv.Atoi(strings.TrimPrefix(c, "cc-"))
		if dir == "after" {
			end = n
		} else {
			start = n + 1
		}
	}
	if f.chatPage > 0 && dir != "after" && end-start > f.chatPage {
		end = start + f.chatPage
	}
	if start > end {
		start = end
	}
	page := items[start:end]
	out := map[string]any{"items": page, "hasMore": end < len(items) && dir != "after", "newestCursor": "cc-" + strconv.Itoa(start), "oldestCursor": "cc-" + strconv.Itoa(end-1)}
	json.NewEncoder(w).Encode(out)
}

func (f *fakeBeeper) messagesPage(w http.ResponseWriter, r *http.Request, chat string) {
	all := f.msgs[chat]
	dir := r.URL.Query().Get("direction")
	cur := r.URL.Query().Get("cursor")
	if dir != "" && dir != "before" && dir != "after" {
		f.bad(w, "messages: direction")
		return
	}
	if dir == "after" && cur == "" {
		f.bad(w, "messages: direction after needs a cursor")
		return
	}
	lo, hi := 0, len(all) // the page is all[lo:hi]
	pos := len(all)
	if cur != "" {
		pos = -1
		for i, m := range all {
			if m.SortKey == keyOfCursor(cur) {
				pos = i
			}
		}
		if pos < 0 {
			f.bad(w, "messages: a cursor Beeper never gave")
			return
		}
	}
	hasMore := false
	switch {
	case dir == "after":
		lo = pos + 1
		hi = min(lo+f.pageSize, len(all))
		hasMore = hi < len(all)
	case cur == "":
		hi = len(all)
		lo = max(0, hi-f.pageSize)
		hasMore = lo > 0
	default: // before the cursor
		hi = pos
		lo = max(0, hi-f.pageSize)
		hasMore = lo > 0
	}
	page := append([]bMessage{}, all[lo:hi]...)
	out := map[string]any{"items": page, "hasMore": hasMore}
	if len(page) > 0 {
		out["newestCursor"] = cursorOf(page[len(page)-1].SortKey)
		out["oldestCursor"] = cursorOf(page[0].SortKey)
	}
	json.NewEncoder(w).Encode(out)
}

func (f *fakeBeeper) oneMessage(w http.ResponseWriter, chat, mid string) {
	if f.gone[mid] {
		http.Error(w, "gone", 404)
		return
	}
	for _, m := range f.msgs[chat] {
		if m.ID == mid {
			json.NewEncoder(w).Encode(m)
			return
		}
	}
	if strings.HasPrefix(mid, "pending-") {
		m := bMessage{ID: mid, ChatID: chat, IsSender: true, SendStatus: f.status[mid]}
		if m.SendStatus == nil {
			m.SendStatus = &bSendStatus{Status: "SUCCESS"}
		}
		json.NewEncoder(w).Encode(m)
		return
	}
	http.Error(w, "no such message", 404)
}

func (f *fakeBeeper) send(w http.ResponseWriter, r *http.Request, chat string) {
	if f.sendFail != 0 {
		http.Error(w, "x", f.sendFail)
		return
	}
	var in struct {
		Text             string `json:"text"`
		ReplyToMessageID string `json:"replyToMessageID"`
		Attachment       *struct {
			UploadID string          `json:"uploadID"`
			FileName string          `json:"fileName"`
			MimeType string          `json:"mimeType"`
			Type     string          `json:"type"`
			Size     json.RawMessage `json:"size"`
			Duration float64         `json:"duration"`
		} `json:"attachment"`
	}
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		f.bad(w, "send: "+err.Error())
		return
	}
	rec := sentRec{Chat: chat, Text: in.Text}
	if in.Attachment != nil {
		up := f.uploads[in.Attachment.UploadID]
		if up == nil || up.Used {
			f.bad(w, "send: an uploadID that was not made or was used already")
			return
		}
		switch in.Attachment.Type {
		case "image", "video", "audio", "file", "gif", "voice-note", "sticker":
		default:
			f.bad(w, "send: attachment type "+in.Attachment.Type)
			return
		}
		up.Used = true
		rec.UploadID, rec.FileName = in.Attachment.UploadID, in.Attachment.FileName
	} else if in.Text == "" {
		f.bad(w, "send: neither text nor attachment")
		return
	}
	f.sent = append(f.sent, rec)
	f.nPending++
	// A message sent in a merged chat can go to another chat: the answer says which.
	real := chat
	if strings.HasSuffix(chat, "-merged") {
		real = strings.TrimSuffix(chat, "-merged")
	}
	json.NewEncoder(w).Encode(map[string]any{"pendingMessageID": fmt.Sprintf("pending-%d", f.nPending), "chatID": real})
}

func (f *fakeBeeper) upload(w http.ResponseWriter, r *http.Request) {
	mt, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mt != "multipart/form-data" {
		f.bad(w, "upload: not multipart/form-data")
		return
	}
	mr := multipart.NewReader(r.Body, params["boundary"])
	rec := &uploadRec{}
	gotFile := false
	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			f.bad(w, "upload: "+err.Error())
			return
		}
		b, _ := io.ReadAll(part)
		switch part.FormName() {
		case "file":
			rec.Bytes, gotFile = b, true
		case "fileName":
			rec.Name = string(b)
		case "mimeType":
		default:
			f.bad(w, "upload: field "+part.FormName())
			return
		}
	}
	if !gotFile {
		f.bad(w, "upload: no file")
		return
	}
	f.nUpload++
	id := fmt.Sprintf("up-%d", f.nUpload)
	f.uploads[id] = rec
	json.NewEncoder(w).Encode(map[string]any{"uploadID": id, "fileName": rec.Name, "fileSize": len(rec.Bytes)})
}

// --- the fake ONE ---

type postResult = map[string]any

type fakeOne struct {
	mu        sync.Mutex
	t         *testing.T
	batches   []map[string]any
	alives    []map[string]any
	alive     int
	replies   []wReply
	posts     []wPost
	handed    map[int64]bool
	handedP   map[int64]bool
	postFiles map[int64][]byte
	resend    []wMissing
	results   map[string]map[string]any
	pResults  map[string]map[string]any
	reportErr bool // answer 500 to results
	missing   []wMissing
	files     map[string][]byte
	fileFail  int // status to answer file uploads with
	backfill  int // days ONE tells the bridge
	maxFile   int64
	srv       *httptest.Server
}

func newFakeOne(t *testing.T) *fakeOne {
	f := &fakeOne{t: t, handed: map[int64]bool{}, handedP: map[int64]bool{}, postFiles: map[int64][]byte{}, results: map[string]map[string]any{}, pResults: map[string]map[string]any{},
		files: map[string][]byte{}, backfill: 30, maxFile: 1 << 30}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeOne) limits() map[string]any {
	return map[string]any{"backfill_days": f.backfill, "max_file_bytes": f.maxFile}
}

func (f *fakeOne) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Header.Get("Authorization") != "Bearer "+tokOne {
		http.Error(w, `{"ok":false,"error":"wrong token"}`, 401)
		return
	}
	body, _ := io.ReadAll(r.Body)
	path := r.URL.Path
	switch {
	case path == "/api/bridge/alive":
		f.alive++
		var m map[string]any
		json.Unmarshal(body, &m)
		f.alives = append(f.alives, m)
		out := f.limits()
		out["ok"] = true
		json.NewEncoder(w).Encode(out)
	case path == "/api/bridge/batch":
		var m map[string]any
		json.Unmarshal(body, &m)
		f.batches = append(f.batches, m)
		out := f.limits()
		out["ok"], out["stored"], out["missing_files"] = true, 1, f.missing
		f.missing = nil
		json.NewEncoder(w).Encode(out)
	case path == "/api/bridge/outbox":
		var out []wReply
		for _, rp := range f.replies {
			if !f.handed[rp.ID] {
				f.handed[rp.ID] = true
				out = append(out, rp)
			}
		}
		var posts []wPost
		for _, p := range f.posts {
			if !f.handedP[p.ID] {
				f.handedP[p.ID] = true
				posts = append(posts, p)
			}
		}
		rs := f.resend
		f.resend = nil
		ans := f.limits()
		ans["ok"], ans["replies"], ans["posts"], ans["resend"] = true, out, posts, append([]wMissing{}, rs...)
		json.NewEncoder(w).Encode(ans)
	case strings.HasPrefix(path, "/api/bridge/outbox/") && strings.HasSuffix(path, "/result"):
		if f.reportErr {
			http.Error(w, "x", 500)
			return
		}
		var m map[string]any
		json.Unmarshal(body, &m)
		f.results[strings.Split(path, "/")[4]] = m
		w.Write([]byte(`{"ok":true}`))
	case strings.HasPrefix(path, "/api/bridge/posts/") && strings.HasSuffix(path, "/result"):
		if f.reportErr {
			http.Error(w, "x", 500)
			return
		}
		var m map[string]any
		json.Unmarshal(body, &m)
		f.pResults[strings.Split(path, "/")[4]] = m
		w.Write([]byte(`{"ok":true}`))
	case strings.HasPrefix(path, "/api/bridge/posts/") && strings.HasSuffix(path, "/file") && r.Method == "GET":
		id, _ := strconv.ParseInt(strings.Split(path, "/")[4], 10, 64)
		b, ok := f.postFiles[id]
		// Only a file of a message that was handed out is there.
		if !ok || !f.handedP[id] {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(b)))
		w.Write(b)
	case path == "/api/bridge/files":
		if f.fileFail != 0 {
			http.Error(w, "x", f.fileFail)
			return
		}
		f.files[r.URL.Query().Get("attachment")] = body
		w.Write([]byte(`{"ok":true}`))
	default:
		f.t.Errorf("unexpected address %s", path)
		http.NotFound(w, r)
	}
}

func (f *fakeOne) messageIDs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var ids []string
	for _, b := range f.batches {
		for _, c := range b["chats"].([]any) {
			for _, m := range c.(map[string]any)["messages"].([]any) {
				ids = append(ids, m.(map[string]any)["id"].(string))
			}
		}
	}
	return ids
}

// message returns the last version of a message ONE was given.
func (f *fakeOne) message(id string) map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	var last map[string]any
	for _, b := range f.batches {
		for _, c := range b["chats"].([]any) {
			for _, m := range c.(map[string]any)["messages"].([]any) {
				if m.(map[string]any)["id"] == id {
					last = m.(map[string]any)
				}
			}
		}
	}
	return last
}

type rig struct {
	t      *testing.T
	beeper *fakeBeeper
	one    *fakeOne
	cfg    Config
	logs   *bytes.Buffer
	now    time.Time
	dir    string
}

func newRig(t *testing.T) *rig {
	r := &rig{t: t, beeper: newFakeBeeper(t), one: newFakeOne(t), logs: &bytes.Buffer{}, now: time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC), dir: t.TempDir()}
	r.cfg = Config{OneURL: r.one.srv.URL, BeeperURL: r.beeper.srv.URL, StatePath: filepath.Join(r.dir, "sub", "state.json"),
		BeeperToken: tokBeeper, OneToken: tokOne, Poll: 20 * time.Second, BackfillDays: 30}
	return r
}

// start makes a bridge, as after a (re)start of the program.
func (r *rig) start() *Bridge {
	b, err := NewBridge(r.cfg, log.New(r.logs, "", 0))
	if err != nil {
		r.t.Fatal(err)
	}
	b.now = func() time.Time { return r.now }
	return b
}

// chat sets a chat and its messages (oldest first). A message keeps the sort key it first had.
func (r *rig) chat(id, title, typ string, msgs ...bMessage) {
	r.beeper.mu.Lock()
	defer r.beeper.mu.Unlock()
	c := bChat{ID: id, AccountID: "whatsapp", Network: "WhatsApp", Title: title, Type: typ, LastActivity: r.now.Format(time.RFC3339)}
	c.Participants.Items = []bPerson{{ID: "me", FullName: "the owner", IsSelf: true}, {ID: "p1", FullName: "Jana"}}
	found := false
	for i := range r.beeper.chats {
		if r.beeper.chats[i].ID == id {
			r.beeper.chats[i], found = c, true
		}
	}
	if !found {
		r.beeper.chats = append([]bChat{c}, r.beeper.chats...)
	}
	sort.SliceStable(r.beeper.chats, func(i, j int) bool { return r.beeper.chats[i].LastActivity > r.beeper.chats[j].LastActivity })
	for i := range msgs {
		k, ok := r.beeper.keys[id+"/"+msgs[i].ID]
		if !ok {
			r.beeper.nextKey++
			k = fmt.Sprintf("%08d", r.beeper.nextKey)
			r.beeper.keys[id+"/"+msgs[i].ID] = k
		}
		msgs[i].SortKey, msgs[i].ChatID = k, id
	}
	r.beeper.msgs[id] = msgs
}

func (r *rig) msg(id, text string) bMessage {
	return bMessage{ID: id, SenderID: "p1", SenderName: "Jana", Timestamp: "2026-11-19T11:00:00Z", Text: text, Type: "TEXT"}
}

func cycle(t *testing.T, b *Bridge) {
	t.Helper()
	if err := b.Cycle(testCtx); err != nil {
		t.Fatal(err)
	}
}

// writeLocal makes a file on this "Mac" and returns its path.
func (r *rig) writeLocal(name string, data []byte) string {
	p := filepath.Join(r.dir, "media", name)
	os.MkdirAll(filepath.Dir(p), 0o700)
	if err := os.WriteFile(p, data, 0o600); err != nil {
		r.t.Fatal(err)
	}
	return p
}
