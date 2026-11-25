package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"lab/apps/one/repo"
)

// Tests of add_file (SPEC 29, 7). Every file is made in the test.

// put sends raw bytes to an upload address with the agents' token.
func (e *agentEnv) put(path string, body []byte, header map[string]string) (int, map[string]any) {
	e.t.Helper()
	req := httptest.NewRequest("PUT", path, bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+e.a.agentTokenValue())
	for k, v := range header {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	var m map[string]any
	json.Unmarshal(rec.Body.Bytes(), &m)
	return rec.Code, m
}

func (e *agentEnv) filesOf(it repo.Item) []map[string]any {
	e.t.Helper()
	var out []map[string]any
	for _, f := range e.getJSON("/api/items/" + ref(it) + "/files")["files"].([]any) {
		out = append(out, f.(map[string]any))
	}
	return out
}

func TestAddFileWithTextListsItUnderTheAgentsNameAndGetFileReadsItBack(t *testing.T) {
	e := newAgentEnv(t)
	it := e.newItem(stageNew, false, "Parcel", "")
	m := e.toolJSON("add_file", map[string]any{"id": ref(it), "name": "plan.md", "text": "# Plan\nCall the carrier on Monday.", "agent": "Claude"})
	if m["ok"] != true || m["name"] != "plan.md" || m["size"] != float64(len("# Plan\nCall the carrier on Monday.")) {
		t.Fatalf("add_file: %v", m)
	}
	fs := e.filesOf(it)
	if len(fs) != 1 || fs[0]["name"] != "plan.md" || fs[0]["added_by"] != "Claude" || fs[0]["saved"] != true {
		t.Fatalf("files: %v", fs)
	}
	got := e.toolJSON("get_file", map[string]any{"id": ref(it), "file": "plan.md"})
	if !strings.Contains(fmt.Sprint(got["text"]), "Call the carrier on Monday.") {
		t.Errorf("get_file: %v", got)
	}
	// The thread has an entry, and the row shows the file like the owner's own.
	th := e.getJSON("/api/items/" + ref(it) + "/thread")
	if !strings.Contains(fmt.Sprint(th), "file_added") {
		t.Errorf("thread: %v", th)
	}
	page := get(e.h, "/items/"+fmt.Sprint(it.ID)).Body.String()
	if !strings.Contains(page, "Attached: <span class=\"fname\">plan.md</span>") || !strings.Contains(page, "added by Claude") || !strings.Contains(page, "Attachments") {
		t.Errorf("the row does not show it:\n%s", page)
	}
	// It is only listed: no reply, no proposal, nothing queued to go out.
	if len(e.events(it.ID, "proposal_made")) != 0 || len(e.events(it.ID, "reply_sent")) != 0 {
		t.Error("adding a file made a proposal or sent something")
	}
	if outs, _ := e.a.store.OutOfItem(e.ctx, it.ID); len(outs) != 0 {
		t.Errorf("a file was queued to go out: %v", outs)
	}
	// The same file twice is the first one.
	again := e.toolJSON("add_file", map[string]any{"id": ref(it), "name": "plan.md", "text": "# Plan\nCall the carrier on Monday.", "agent": "Claude"})
	if again["unchanged"] != true || len(e.filesOf(it)) != 1 || len(e.events(it.ID, "file_added")) != 1 {
		t.Errorf("twice: %v", again)
	}
}

func TestAddFileRefusals(t *testing.T) {
	e := newAgentEnv(t)
	it := e.newItem(stageNew, false, "Parcel", "")
	other := e.newItem(stageNew, false, "Other", "")
	cases := []struct {
		name string
		args map[string]any
		want string
	}{
		{"no name", map[string]any{"id": ref(it), "text": "x"}, "name is required"},
		{"not text", map[string]any{"id": ref(it), "name": "photo.png", "text": "x"}, "txt, md, csv, ics, html or json"},
		{"empty", map[string]any{"id": ref(it), "name": "a.txt", "text": "  "}, "empty"},
		{"too long", map[string]any{"id": ref(it), "name": "a.txt", "text": strings.Repeat("x", maxAgentText+1)}, "at most 200 kB"},
		{"both", map[string]any{"id": ref(it), "name": "a.txt", "text": "x", "from_item": ref(other), "file_id": 1}, "not both"},
		{"no file id", map[string]any{"id": ref(it), "from_item": ref(other)}, "file_id is required"},
		{"no such file", map[string]any{"id": ref(it), "from_item": ref(other), "file_id": 999}, "no file with that file_id"},
		{"no such item", map[string]any{"id": "Q-9999", "name": "a.txt", "text": "x"}, "no such item"},
		{"no such source", map[string]any{"id": ref(it), "from_item": "Q-9999", "file_id": 1}, "no such item"},
		{"bad id", map[string]any{"id": "nope", "name": "a.txt", "text": "x"}, "id must be"},
	}
	for _, c := range cases {
		text, bad := e.tool("add_file", c.args)
		if !bad || !strings.Contains(text, c.want) {
			t.Errorf("%s: %q %v", c.name, text, bad)
		}
	}
	if len(e.filesOf(it)) != 0 || len(e.events(it.ID, "file_added")) != 0 {
		t.Error("a refused call listed a file")
	}
	// The owner's own channel is refused: the action exists for agents.
	if _, err := e.a.addFile(e.ctx, actor{owner: true}, ref(it), fileAdd{Name: "a.txt", Text: "x", HasText: true}); err == nil {
		t.Error("The owner's channel added a file through the agents' code")
	}
}

func TestAddFileFromAnotherItemStoresTheBytesOnce(t *testing.T) {
	e := newAgentEnv(t)
	src, ids := e.itemWithFiles(stageNew, [3]any{"summons.pdf", "application/pdf", tinyPDF("Hearing on 14 December 2026")})
	dst := e.newItem(stageNew, false, "Case", "")
	m := e.toolJSON("add_file", map[string]any{"id": ref(dst), "from_item": ref(src), "file_id": ids[0], "agent": "Helper"})
	if m["ok"] != true {
		t.Fatalf("from_item: %v", m)
	}
	fs := e.filesOf(dst)
	if len(fs) != 1 || fs[0]["name"] != "summons.pdf" || fs[0]["added_by"] != "Helper" {
		t.Fatalf("files: %v", fs)
	}
	f2, ok := e.a.store.FileByID(e.ctx, int64(fs[0]["file_id"].(float64)))
	if !ok || f2.Path == "" {
		t.Fatalf("the copy: %v", f2)
	}
	orig, _ := e.a.store.FileByID(e.ctx, ids[0])
	if f2.Path != orig.Path || f2.SHA != orig.SHA {
		t.Errorf("not stored once: %s vs %s", f2.Path, orig.Path)
	}
	got := e.toolJSON("get_file", map[string]any{"id": ref(dst), "file_id": fs[0]["file_id"]})
	if !strings.Contains(fmt.Sprint(got), "Hearing on 14 December 2026") {
		t.Errorf("get_file: %v", got)
	}
	// A new name can be given.
	m = e.toolJSON("add_file", map[string]any{"id": ref(dst), "from_item": ref(src), "file_id": ids[0], "name": "../x/vorladung.pdf"})
	if m["name"] != "vorladung.pdf" {
		t.Errorf("rename: %v", m)
	}
}

func TestAddFileByUploadIsOneTimeBoundAndSanitised(t *testing.T) {
	e := newAgentEnv(t)
	it := e.newItem(stageNew, false, "Parcel", "")
	other := e.newItem(stageNew, false, "Other", "")
	m := e.toolJSON("add_file", map[string]any{"id": ref(it), "name": "../../etc/Report 2026.bin", "agent": "Claude"})
	up, _ := m["upload"].(map[string]any)
	if up == nil || up["method"] != "PUT" || up["uses"] != float64(1) || !strings.Contains(fmt.Sprint(up["url"]), "/api/uploads/") || m["name"] != "Report 2026.bin" {
		t.Fatalf("upload: %v", m)
	}
	if len(e.filesOf(it)) != 0 {
		t.Fatal("a file is listed before its bytes came")
	}
	url := up["url"].(string)
	// The address needs the agents' token as well.
	if rec := e.call("PUT", url, "", nil); rec.Code != 401 {
		t.Errorf("without a token: %d", rec.Code)
	}
	if rec := e.call("PUT", url, "wrong", nil); rec.Code != 401 {
		t.Errorf("a wrong token: %d", rec.Code)
	}
	body := bytes.Repeat([]byte{0, 1, 2, 3, 250}, 1000)
	code, res := e.put(url, body, nil)
	if code != 200 || res["ok"] != true || res["name"] != "Report 2026.bin" || res["size"] != float64(len(body)) {
		t.Fatalf("upload: %d %v", code, res)
	}
	fs := e.filesOf(it)
	if len(fs) != 1 || fs[0]["added_by"] != "Claude" || fs[0]["name"] != "Report 2026.bin" || len(e.filesOf(other)) != 0 {
		t.Fatalf("files: %v", fs)
	}
	if len(e.events(it.ID, "file_added")) != 1 {
		t.Error("no entry in the thread")
	}
	// It works once.
	if code, _ := e.put(url, body, nil); code != 404 {
		t.Errorf("a second use: %d", code)
	}
	// A refused upload uses the address up too, and an expired one is refused.
	m = e.toolJSON("add_file", map[string]any{"id": ref(it), "name": "big.bin"})
	url2 := m["upload"].(map[string]any)["url"].(string)
	if code, res := e.put(url2, bytes.Repeat([]byte("x"), maxWriteFile+1), nil); code != 413 {
		t.Errorf("a file over the limit: %d %v", code, res)
	}
	if code, _ := e.put(url2, []byte("x"), nil); code != 404 {
		t.Errorf("an address that was used: %d", code)
	}
	m = e.toolJSON("add_file", map[string]any{"id": ref(it), "name": "late.bin"})
	url3 := m["upload"].(map[string]any)["url"].(string)
	e.now = e.now.Add(uploadTicketLife + time.Second)
	if code, _ := e.put(url3, []byte("x"), nil); code != 404 {
		t.Errorf("an expired address: %d", code)
	}
	m = e.toolJSON("add_file", map[string]any{"id": ref(it), "name": "empty.bin"})
	if code, _ := e.put(m["upload"].(map[string]any)["url"].(string), nil, nil); code != 400 {
		t.Errorf("an empty file: %d", code)
	}
	if n := len(e.filesOf(it)); n != 1 {
		t.Errorf("refused uploads left files: %d", n)
	}
	// An address that never existed.
	if code, _ := e.put("/api/uploads/00112233", []byte("x"), nil); code != 404 {
		t.Errorf("a made-up address: %d", code)
	}
}

func TestAddFileHonoursTheTestRun(t *testing.T) {
	e := newAgentEnv(t)
	run := "e2e-attach00001"
	real := e.newItem(stageNew, false, "Real", "")
	realSrc, ids := e.itemWithFiles(stageNew, [3]any{"r.txt", "text/plain", []byte("real file")})
	old := e.ctx
	e.ctx = repo.WithRun(old, run)
	marked := e.newItem(stageNew, false, "Marked", "")
	e.ctx = old
	if marked.TestRun != run {
		t.Fatalf("the item is not marked: %q", marked.TestRun)
	}
	// A call under a run may write only to an item of that run (as every writing tool).
	if text, bad := e.tool("add_file", map[string]any{"id": ref(real), "name": "a.txt", "text": "x", "run": run}); !bad || text != (&repo.RunError{}).Error() {
		t.Errorf("a run writing to a real item: %q %v", text, bad)
	}
	if text, bad := e.tool("add_file", map[string]any{"id": ref(marked), "name": "a.txt", "text": "x", "run": "bad"}); !bad || text != badRunMessage {
		t.Errorf("a bad run name: %q", text)
	}
	if text, bad := e.tool("add_file", map[string]any{"id": ref(marked), "from_item": ref(realSrc), "file_id": ids[0], "run": run}); !bad || text != (&repo.RunError{}).Error() {
		t.Errorf("a real file into a run's item: %q %v", text, bad)
	}
	if m := e.toolJSON("add_file", map[string]any{"id": ref(marked), "name": "a.txt", "text": "run text", "run": run}); m["ok"] != true {
		t.Fatalf("under the run: %v", m)
	}
	if len(e.filesOf(real)) != 0 {
		t.Error("a real item got a file")
	}
	// An upload address made under a run works only under that run, and the file carries its mark.
	m := e.toolJSON("add_file", map[string]any{"id": ref(marked), "name": "b.bin", "run": run})
	url := m["upload"].(map[string]any)["url"].(string)
	if code, _ := e.put(url, []byte("x"), nil); code != 403 {
		t.Errorf("without the run: %d", code)
	}
	m = e.toolJSON("add_file", map[string]any{"id": ref(marked), "name": "b.bin", "run": run})
	url = m["upload"].(map[string]any)["url"].(string)
	if code, res := e.put(url, []byte("xyz"), map[string]string{runHeader: run}); code != 200 {
		t.Fatalf("with the run: %d %v", code, res)
	}
	if n := len(e.filesOf(marked)); n != 2 {
		t.Errorf("the run's files: %d", n)
	}
}
