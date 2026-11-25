package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// fakeGraphBox is the mailbox behind the fake Microsoft: folders, messages
// with immutable ids, conversations, read state, flags and categories, and
// a delta feed per folder as Graph gives it (a round ends with a delta
// link; the next round returns what changed, with @removed for messages
// that left the folder). It answers only reads and records every call.

type gmFolder struct {
	id, name, wk, parent string
}

type gmMsg struct {
	id, conv, folder string
	received         time.Time
	read, flagged    bool
	draft            bool
	cats             []string
	from, fromName   string
	subject          string
	header           string
	body             []byte
	gen              func(w io.Writer)
	size             int64
	gone             bool
}

type gmChange struct {
	seq     int
	id      string
	folder  string
	removed bool
}

type fakeGraphBox struct {
	mu      sync.Mutex
	base    string
	folders []gmFolder
	msgs    []*gmMsg
	seq     int
	changes []gmChange
	nextID  int

	calls    []string // method and path of every call, never a token
	prefers  []string // the Prefer header of every call
	queries  []string // the query of every call
	statuses map[int]int

	mimeServed    int
	mimeIDs       []string
	dropMimeAfter int // the connection is dropped at the next original once this many were served; 0 = never
	throttle      int // answer this many calls with 429 and Retry-After
	badTokens     int // answer this many calls with 401
	resync        map[string]bool
	onMime        func()
	wrongLink     bool // hand out links that point to another address
	listSelects   []string
	listPages     int
	onList        func() // called, without the lock, after each page of the message list
}

func newFakeGraphBox() *fakeGraphBox {
	return &fakeGraphBox{statuses: map[int]int{}, resync: map[string]bool{}, nextID: 100}
}

func (b *fakeGraphBox) addFolder(id, name, wk, parent string) {
	b.folders = append(b.folders, gmFolder{id: id, name: name, wk: wk, parent: parent})
}

// standard makes the folders a mailbox has.
func (b *fakeGraphBox) standard() {
	b.addFolder("F-inbox", "Inbox", "inbox", "")
	b.addFolder("F-sent", "Sent Items", "sentitems", "")
	b.addFolder("F-drafts", "Drafts", "drafts", "")
	b.addFolder("F-deleted", "Deleted Items", "deleteditems", "")
	b.addFolder("F-junk", "Junk Email", "junkemail", "")
	b.addFolder("F-archive", "Archive", "archive", "")
	b.addFolder("F-proj", "Projects", "", "F-inbox")
	b.addFolder("F-trashsub", "Old stuff", "", "F-deleted")
}

// add puts a message in a folder; m.thrid is the conversation id.
func (b *fakeGraphBox) add(folder string, m testMail) *gmMsg {
	h, body := m.bytes()
	b.mu.Lock()
	defer b.mu.Unlock()
	b.nextID++
	b.seq++
	id := fmt.Sprintf("AAMk-%d", b.nextID)
	name, addr := splitFrom(m.from)
	gm := &gmMsg{id: id, conv: m.thrid, folder: folder, received: m.date.UTC(), read: slices.Contains(m.flags, `\Seen`), from: addr, fromName: name,
		subject: m.subject, header: h, body: body, size: int64(len(h) + len(body))}
	if gm.conv == "" {
		gm.conv = "conv-" + id
	}
	b.msgs = append(b.msgs, gm)
	b.changes = append(b.changes, gmChange{seq: b.seq, id: id, folder: folder})
	return gm
}

func (b *fakeGraphBox) addGenerated(folder string, m testMail, gen func(w io.Writer)) *gmMsg {
	gm := b.add(folder, m)
	b.mu.Lock()
	defer b.mu.Unlock()
	cw := &countWriter{}
	gen(cw)
	gm.body, gm.gen = nil, gen
	gm.size = int64(len(gm.header)) + cw.n
	return gm
}

func splitFrom(s string) (name, addr string) {
	if i := strings.Index(s, "<"); i >= 0 {
		return strings.Trim(strings.TrimSpace(s[:i]), `"`), strings.TrimSuffix(s[i+1:], ">")
	}
	return "", s
}

// set changes a message the way Outlook would after a click.
func (b *fakeGraphBox) set(id string, fn func(*gmMsg)) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, m := range b.msgs {
		if m.id == id {
			fn(m)
			b.seq++
			b.changes = append(b.changes, gmChange{seq: b.seq, id: id, folder: m.folder})
		}
	}
}

// move puts a message in another folder: removed in the old one, changed in the new one.
func (b *fakeGraphBox) move(id, to string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, m := range b.msgs {
		if m.id == id {
			b.seq++
			b.changes = append(b.changes, gmChange{seq: b.seq, id: id, folder: m.folder, removed: true})
			m.folder = to
			b.seq++
			b.changes = append(b.changes, gmChange{seq: b.seq, id: id, folder: to})
		}
	}
}

// remove deletes a message for good.
func (b *fakeGraphBox) remove(id string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, m := range b.msgs {
		if m.id == id {
			b.seq++
			b.changes = append(b.changes, gmChange{seq: b.seq, id: id, folder: m.folder, removed: true})
			m.gone = true
		}
	}
}

func (b *fakeGraphBox) callList() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return slices.Clone(b.calls)
}

func (b *fakeGraphBox) queryList() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return slices.Clone(b.queries)
}

func (b *fakeGraphBox) mimeFetched() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return slices.Clone(b.mimeIDs)
}

func (b *fakeGraphBox) find(id string) *gmMsg {
	for _, m := range b.msgs {
		if m.id == id && !m.gone {
			return m
		}
	}
	return nil
}

func (b *fakeGraphBox) json(m *gmMsg) map[string]any {
	flag := "notFlagged"
	if m.flagged {
		flag = "flagged"
	}
	addr := map[string]any{"emailAddress": map[string]any{"name": m.fromName, "address": m.from}}
	cats := m.cats
	if cats == nil {
		cats = []string{}
	}
	return map[string]any{
		"id": m.id, "conversationId": m.conv, "parentFolderId": m.folder,
		"receivedDateTime": m.received.Format("2006-01-02T15:04:05Z"), "sentDateTime": m.received.Format("2006-01-02T15:04:05Z"),
		"isRead": m.read, "isDraft": m.draft, "flag": map[string]any{"flagStatus": flag}, "categories": cats,
		"from": addr, "sender": addr, "subject": m.subject, "internetMessageId": "<" + m.id + "@example.test>", "hasAttachments": false,
	}
}

func gmError(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"code": code, "message": "invented message"}})
}

func (b *fakeGraphBox) serve(w http.ResponseWriter, r *http.Request) {
	b.mu.Lock()
	b.calls = append(b.calls, r.Method+" "+r.URL.Path)
	b.prefers = append(b.prefers, r.Header.Get("Prefer"))
	b.queries = append(b.queries, r.URL.RawQuery)
	tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	switch {
	case !strings.HasPrefix(tok, secretTok):
		b.statuses[401]++
		b.mu.Unlock()
		gmError(w, 401, "InvalidAuthenticationToken")
		return
	case b.badTokens > 0:
		b.badTokens--
		b.mu.Unlock()
		gmError(w, 401, "InvalidAuthenticationToken")
		return
	case b.throttle > 0:
		b.throttle--
		b.statuses[429]++
		b.mu.Unlock()
		w.Header().Set("Retry-After", "1")
		gmError(w, 429, "TooManyRequests")
		return
	}
	if r.Method != "GET" {
		b.mu.Unlock()
		gmError(w, 405, "MethodNotAllowed")
		return
	}
	path := r.URL.Path
	switch {
	case strings.HasPrefix(path, "/me/messages/"):
		b.message(w, r, strings.TrimPrefix(path, "/me/messages/")) // unlocks
	case path == "/me/mailFolders":
		b.listFolders(w, "")
	default:
		parts := strings.Split(strings.TrimPrefix(path, "/me/mailFolders/"), "/")
		switch {
		case len(parts) == 1:
			b.oneFolder(w, parts[0])
		case len(parts) == 2 && parts[1] == "childFolders":
			b.listFolders(w, parts[0])
		case len(parts) == 3 && parts[1] == "messages" && parts[2] == "delta":
			b.delta(w, r, parts[0])
		case len(parts) == 2 && parts[1] == "messages":
			b.list(w, r, parts[0])
		default:
			b.mu.Unlock()
			gmError(w, 404, "ErrorItemNotFound")
		}
	}
}

func (b *fakeGraphBox) oneFolder(w http.ResponseWriter, key string) {
	defer b.mu.Unlock()
	for _, f := range b.folders {
		if f.wk == key || f.id == key {
			json.NewEncoder(w).Encode(map[string]any{"id": f.id})
			return
		}
	}
	gmError(w, 404, "ErrorItemNotFound")
}

func (b *fakeGraphBox) listFolders(w http.ResponseWriter, parent string) {
	defer b.mu.Unlock()
	var out []map[string]any
	for _, f := range b.folders {
		if f.parent != parent {
			continue
		}
		n := 0
		for _, c := range b.folders {
			if c.parent == f.id {
				n++
			}
		}
		out = append(out, map[string]any{"id": f.id, "displayName": f.name, "childFolderCount": n})
	}
	json.NewEncoder(w).Encode(map[string]any{"value": out})
}

func (b *fakeGraphBox) message(w http.ResponseWriter, r *http.Request, rest string) {
	id, tail, _ := strings.Cut(rest, "/")
	m := b.find(id)
	if m == nil {
		b.mu.Unlock()
		gmError(w, 404, "ErrorItemNotFound")
		return
	}
	if tail == "" {
		// Where the message is now, and its headers for the rules that look at them.
		var hs []map[string]string
		for _, l := range strings.Split(strings.ReplaceAll(m.header, "\r\n", "\n"), "\n") {
			if k, v, ok := strings.Cut(l, ":"); ok {
				hs = append(hs, map[string]string{"name": k, "value": strings.TrimSpace(v)})
			}
		}
		full := b.json(m)
		b.mu.Unlock()
		if strings.Contains(r.URL.Query().Get("$select"), "conversationId") {
			json.NewEncoder(w).Encode(full) // the message as the list describes it
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"id": id, "parentFolderId": m.folder, "internetMessageHeaders": hs})
		return
	}
	if tail != "$value" {
		b.mu.Unlock()
		gmError(w, 404, "ErrorItemNotFound")
		return
	}
	if b.dropMimeAfter > 0 && b.mimeServed >= b.dropMimeAfter {
		b.mu.Unlock() // the client repeats a GET once on a dropped connection, so every try is dropped until the test says otherwise
		panic(http.ErrAbortHandler)
	}
	b.mimeServed++
	b.mimeIDs = append(b.mimeIDs, id)
	onMime := b.onMime
	b.mu.Unlock()
	w.Header().Set("Content-Type", "message/rfc822")
	io.WriteString(w, m.header)
	if m.gen != nil {
		m.gen(w)
	} else {
		w.Write(m.body)
	}
	if onMime != nil {
		onMime()
	}
}

func prefSize(r *http.Request) int {
	for _, p := range strings.Split(r.Header.Get("Prefer"), ",") {
		if k, v, ok := strings.Cut(strings.TrimSpace(p), "="); ok && k == "odata.maxpagesize" {
			if n, err := strconv.Atoi(v); err == nil && n > 0 {
				return n
			}
		}
	}
	return 10
}

// delta answers one page of a round. A round that starts without a token
// lists the messages of the folder received since the filter, newest first;
// one that starts with a delta token lists what changed since.
func (b *fakeGraphBox) delta(w http.ResponseWriter, r *http.Request, folder string) {
	defer b.mu.Unlock()
	q := r.URL.Query()
	kind, since, from, off, newStart := "init", int64(0), 0, 0, b.seq
	switch {
	case q.Get("$skiptoken") != "":
		p := strings.Split(q.Get("$skiptoken"), ".")
		kind = p[0]
		from, _ = strconv.Atoi(p[1])
		since, _ = strconv.ParseInt(p[2], 10, 64)
		off, _ = strconv.Atoi(p[3])
		newStart, _ = strconv.Atoi(p[4])
	case q.Get("$deltatoken") != "":
		kind = "delta"
		p := strings.Split(q.Get("$deltatoken"), "-")
		from, _ = strconv.Atoi(p[0])
		since, _ = strconv.ParseInt(p[1], 10, 64)
		if b.resync[folder] {
			gmError(w, 410, "syncStateNotFound")
			return
		}
	default:
		f := q.Get("$filter")
		if i := strings.Index(f, "receivedDateTime ge "); i >= 0 {
			if t, err := time.Parse("2006-01-02T15:04:05Z", strings.TrimSpace(f[i+len("receivedDateTime ge "):])); err == nil {
				since = t.Unix()
			}
		}
	}
	var all []map[string]any
	if kind == "init" {
		var in []*gmMsg
		for _, m := range b.msgs {
			if !m.gone && m.folder == folder && m.received.Unix() >= since {
				in = append(in, m)
			}
		}
		sort.SliceStable(in, func(i, j int) bool { return in[i].received.After(in[j].received) })
		for _, m := range in {
			all = append(all, b.json(m))
		}
	} else {
		last := map[string]gmChange{}
		var order []string
		for _, c := range b.changes {
			if c.seq > from && c.folder == folder {
				if _, seen := last[c.id]; !seen {
					order = append(order, c.id)
				}
				last[c.id] = c
			}
		}
		for _, id := range order {
			c := last[id]
			var m *gmMsg
			for _, x := range b.msgs {
				if x.id == id {
					m = x
				}
			}
			switch {
			case c.removed:
				all = append(all, map[string]any{"id": id, "@removed": map[string]any{"reason": "deleted"}})
			case m != nil && !m.gone && m.folder == folder && m.received.Unix() >= since:
				all = append(all, b.json(m))
			}
		}
	}
	size := prefSize(r)
	end := min(off+size, len(all))
	page := all[off:end]
	if page == nil {
		page = []map[string]any{}
	}
	base := b.base
	if b.wrongLink {
		base = "https://elsewhere.invalid/v1.0"
	}
	link := base + "/me/mailFolders/" + url.PathEscape(folder) + "/messages/delta"
	out := map[string]any{"value": page}
	if end < len(all) {
		out["@odata.nextLink"] = fmt.Sprintf("%s?$skiptoken=%s.%d.%d.%d.%d", link, kind, from, since, end, newStart)
	} else {
		out["@odata.deltaLink"] = fmt.Sprintf("%s?$deltatoken=%d-%d", link, newStart, since)
	}
	json.NewEncoder(w).Encode(out)
}

// list answers one page of the plain message list of a folder, newest first,
// with the fields the caller selected only (no subject and no body unless it
// asked for them). A page's link carries the offset.
func (b *fakeGraphBox) list(w http.ResponseWriter, r *http.Request, folder string) {
	defer b.mu.Unlock()
	q := r.URL.Query()
	b.listSelects = append(b.listSelects, q.Get("$select"))
	var in []*gmMsg
	for _, m := range b.msgs {
		if !m.gone && m.folder == folder {
			in = append(in, m)
		}
	}
	sort.SliceStable(in, func(i, j int) bool { return in[i].received.After(in[j].received) })
	off, _ := strconv.Atoi(q.Get("$skiptoken"))
	size := prefSize(r)
	end := min(off+size, len(in))
	page := []map[string]any{}
	want := map[string]bool{"id": true}
	for _, f := range strings.Split(q.Get("$select"), ",") {
		want[strings.TrimSpace(f)] = true
	}
	for _, m := range in[min(off, len(in)):end] {
		full := b.json(m)
		for k := range full {
			if !want[k] {
				delete(full, k) // like Graph: only what was asked for
			}
		}
		page = append(page, full)
	}
	out := map[string]any{"value": page}
	if end < len(in) {
		v := url.Values{}
		v.Set("$select", q.Get("$select"))
		v.Set("$orderby", q.Get("$orderby"))
		v.Set("$skiptoken", strconv.Itoa(end))
		out["@odata.nextLink"] = b.base + "/me/mailFolders/" + url.PathEscape(folder) + "/messages?" + strings.ReplaceAll(v.Encode(), "+", "%20")
	}
	b.listPages++
	if b.onList != nil {
		b.mu.Unlock()
		b.onList()
		b.mu.Lock()
	}
	json.NewEncoder(w).Encode(out)
}
