package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeNotify stands in for the Notify app's production instance.
type fakeNotify struct {
	srv    *httptest.Server
	mu     sync.Mutex
	got    []message
	auths  []string
	status int // 0 means 200
}

func (f *fakeNotify) messages() []message {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]message(nil), f.got...)
}

func (f *fakeNotify) titles() string {
	var ts []string
	for _, m := range f.messages() {
		ts = append(ts, m.Title)
	}
	return strings.Join(ts, "|")
}

// installFakeNotify registers a running notify/prod instance that points at
// the fake and writes its token where Notify would keep it.
func installFakeNotify(t *testing.T, h *harness) *fakeNotify {
	t.Helper()
	f := &fakeNotify{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/send" {
			w.Write([]byte("ok")) // its health check
			return
		}
		var m message
		json.NewDecoder(r.Body).Decode(&m)
		f.mu.Lock()
		f.got = append(f.got, m)
		f.auths = append(f.auths, r.Header.Get("Authorization"))
		status := f.status
		f.mu.Unlock()
		if status == 0 {
			status = http.StatusOK
		}
		w.WriteHeader(status)
		w.Write([]byte(`{"ok":true,"delivered":1}`))
	}))
	t.Cleanup(f.srv.Close)
	u, _ := url.Parse(f.srv.URL)
	port, _ := strconv.Atoi(u.Port())
	inst := Instance{App: notifyApp, Env: prodEnv, Port: port, Status: "running", Health: "/healthz", DeployedAt: time.Now()}
	if err := h.srv.store.Put(inst); err != nil {
		t.Fatal(err)
	}
	root, _ := h.runner.DataDir(inst.ID())
	if err := os.MkdirAll(filepath.Join(root, secretsFolder), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, secretsFolder, "token"), []byte("tok-123\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	h.srv.notifier.sleep = func(time.Duration) {}
	return f
}

func TestNotifierSendsThroughNotify(t *testing.T) {
	h := newHarness(t)
	n := h.srv.notifier
	ctx := context.Background()

	// Nothing deployed yet: dropped, not an error.
	if n.send(ctx, message{Topic: "lab", Title: "early"}) {
		t.Fatal("sent without Notify")
	}
	f := installFakeNotify(t, h)
	if !n.send(ctx, message{Topic: "lab", Title: "hello", Body: "b", URL: "https://x.example"}) {
		t.Fatal("not sent")
	}
	got := f.messages()
	if len(got) != 1 || got[0].Title != "hello" || got[0].Topic != "lab" || got[0].Body != "b" || f.auths[0] != "Bearer tok-123" {
		t.Fatalf("got %+v auth %v", got, f.auths)
	}
	if n.send(ctx, message{Topic: "nope", Title: "x"}) || n.send(ctx, message{Topic: "lab", Title: ""}) {
		t.Fatal("an invalid message was sent")
	}
	if len(f.messages()) != 1 {
		t.Fatal("invalid messages reached Notify")
	}

	// A refusal is not retried; a server error is retried once.
	f.status = http.StatusUnauthorized
	if n.send(ctx, message{Topic: "lab", Title: "refused"}) || len(f.messages()) != 2 {
		t.Fatalf("refusal: %s", f.titles())
	}
	f.status = http.StatusInternalServerError
	if n.send(ctx, message{Topic: "lab", Title: "flaky"}) || len(f.messages()) != 4 {
		t.Fatalf("retry: %s", f.titles())
	}
	f.status = 0

	// sendOnce: the same key within its window is sent once, and the state
	// survives a restart.
	if !n.sendOnce(ctx, "k", time.Hour, message{Topic: "lab", Title: "once"}) {
		t.Fatal("first sendOnce")
	}
	if n.sendOnce(ctx, "k", time.Hour, message{Topic: "lab", Title: "once"}) {
		t.Fatal("second sendOnce sent")
	}
	again := newNotifier(h.cfg, h.srv.store, h.runner, "test")
	if _, ok := again.sent["k"]; !ok {
		t.Fatal("dedupe state not kept on disk")
	}
	n.forget("k")
	if !n.sendOnce(ctx, "k", time.Hour, message{Topic: "lab", Title: "once"}) {
		t.Fatal("sendOnce after forget")
	}

	// Without the token file nothing goes out.
	root, _ := h.runner.DataDir(instanceID(notifyApp, prodEnv))
	os.Remove(filepath.Join(root, secretsFolder, "token"))
	if n.send(ctx, message{Topic: "lab", Title: "no token"}) {
		t.Fatal("sent without a token")
	}
}

func TestNotifyEndpoint(t *testing.T) {
	h := newHarness(t)
	body := []byte(`{"topic":"lab","title":"Installing 1 update","body":"started"}`)

	code, _, raw := h.call("POST", "/api/notify", body, "refs/heads/idea")
	mustStatus(t, "notify from a branch", code, http.StatusForbidden, raw)

	code, js, raw := h.call("POST", "/api/notify", body, refMain)
	mustStatus(t, "notify without Notify", code, 200, raw)
	if js["delivered"] != false {
		t.Fatalf("delivered without Notify: %s", raw)
	}

	f := installFakeNotify(t, h)
	code, js, raw = h.call("POST", "/api/notify", body, refMain)
	mustStatus(t, "notify", code, 200, raw)
	if js["delivered"] != true || f.titles() != "Installing 1 update" {
		t.Fatalf("notify: %s / %s", raw, f.titles())
	}
	for _, bad := range []string{`not json`, `{"topic":"news","title":"x"}`, `{"topic":"lab","title":""}`, `{"topic":"lab","title":"x","url":"ftp://x"}`} {
		code, _, raw = h.call("POST", "/api/notify", []byte(bad), refMain)
		mustStatus(t, "bad body "+bad, code, http.StatusBadRequest, raw)
	}
	if len(f.messages()) != 1 {
		t.Fatal("a bad message reached Notify")
	}
}

func TestOutbox(t *testing.T) {
	h := newHarness(t)
	f := installFakeNotify(t, h)
	n := h.srv.notifier
	ctx := context.Background()

	prod := Instance{App: "hello", Env: prodEnv, Port: 20001, Status: "running", Health: "/healthz"}
	preview := Instance{App: "hello", Env: "idea", Port: 20002, Status: "running", Health: "/healthz"}
	stopped := Instance{App: "hello", Env: "old", Port: 20003, Status: "failed", Health: "/healthz"}
	for _, inst := range []Instance{prod, preview, stopped} {
		if err := h.srv.store.Put(inst); err != nil {
			t.Fatal(err)
		}
	}
	outbox := func(inst Instance) string {
		root, _ := h.runner.DataDir(inst.ID())
		dir := filepath.Join(root, outboxFolder)
		os.MkdirAll(dir, 0o700)
		return dir
	}
	pd, vd, sd := outbox(prod), outbox(preview), outbox(stopped)
	write := func(dir, name, content string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(pd, "a.json", `{"title":"Hi","body":"there","url":"https://hello.example"}`)
	write(pd, "b.json", `{"topic":"security","title":"`+strings.Repeat("x", 300)+`"}`)
	write(pd, "big.json", `{"title":"`+strings.Repeat("y", outboxMaxFile)+`"}`)
	write(pd, "broken.json", `{"title": `)
	write(pd, "draft.tmp", `{"title":"not yet"}`)
	os.Symlink(filepath.Join(pd, "a.json"), filepath.Join(pd, "link.json"))
	write(vd, "p.json", `{"title":"from a preview"}`)
	write(sd, "s.json", `{"title":"from a stopped instance"}`)

	n.scanOutboxes(ctx)

	got := f.messages()
	if len(got) != 2 {
		t.Fatalf("messages: %+v", got)
	}
	if got[0].Title != "hello: Hi" || got[0].Topic != "bots" || got[0].Body != "there" || got[0].URL != "https://hello.example" {
		t.Fatalf("first: %+v", got[0])
	}
	if got[1].Topic != "security" || len([]rune(got[1].Title)) != notifyMaxTitle {
		t.Fatalf("second: %+v", got[1])
	}
	for _, name := range []string{"a.json", "b.json", "big.json", "broken.json", "link.json"} {
		if _, err := os.Lstat(filepath.Join(pd, name)); err == nil {
			t.Fatalf("%s still in the outbox", name)
		}
	}
	if _, err := os.Stat(filepath.Join(pd, "draft.tmp")); err != nil {
		t.Fatal("a .tmp file on its way should stay")
	}
	if _, err := os.Stat(filepath.Join(vd, "p.json")); err == nil {
		t.Fatal("preview message not removed")
	}
	if _, err := os.Stat(filepath.Join(sd, "s.json")); err != nil {
		t.Fatal("a stopped instance's outbox should be left alone")
	}

	// The hourly budget.
	now := time.Now()
	n.now = func() time.Time { return now }
	for i := 0; i < outboxMaxHour; i++ {
		if !n.allow("x") {
			t.Fatalf("message %d refused", i)
		}
	}
	if n.allow("x") {
		t.Fatal("budget not enforced")
	}
	now = now.Add(61 * time.Minute)
	if !n.allow("x") {
		t.Fatal("budget did not renew")
	}
}

func TestWatchHealth(t *testing.T) {
	h := newHarness(t)
	f := installFakeNotify(t, h)
	n := h.srv.notifier
	ctx := context.Background()

	healthy := true
	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !healthy {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
		w.Write([]byte("ok"))
	}))
	defer app.Close()
	u, _ := url.Parse(app.URL)
	port, _ := strconv.Atoi(u.Port())
	if err := h.srv.store.Put(Instance{App: "hello", Env: prodEnv, Port: port, Status: "running", Health: "/healthz"}); err != nil {
		t.Fatal(err)
	}
	// A preview that is down is not the owner's business.
	if err := h.srv.store.Put(Instance{App: "hello", Env: "idea", Port: 1, Status: "running", Health: "/healthz"}); err != nil {
		t.Fatal(err)
	}

	n.watchHealth(ctx)
	if f.titles() != "" {
		t.Fatalf("healthy app reported: %s", f.titles())
	}
	healthy = false
	n.watchHealth(ctx)
	n.watchHealth(ctx)
	if f.titles() != "hello is down" {
		t.Fatalf("down: %s", f.titles())
	}
	healthy = true
	n.watchHealth(ctx)
	if f.titles() != "hello is down|hello is back" {
		t.Fatalf("back: %s", f.titles())
	}
	healthy = false
	n.watchHealth(ctx)
	if f.titles() != "hello is down|hello is back|hello is down" {
		t.Fatalf("down again: %s", f.titles())
	}
}

func TestHeartbeat(t *testing.T) {
	h := newHarness(t)
	f := installFakeNotify(t, h)
	n := h.srv.notifier
	ctx := context.Background()
	loc, err := time.LoadLocation("Europe/Prague")
	if err != nil {
		t.Skip("no tzdata")
	}
	now := time.Date(2026, 11, 9, 7, 30, 0, 0, loc)
	n.now = func() time.Time { return now }

	n.heartbeat(ctx)
	if f.titles() != "" {
		t.Fatal("heartbeat before 08:00")
	}
	now = time.Date(2026, 11, 9, 8, 5, 0, 0, loc)
	n.heartbeat(ctx)
	n.heartbeat(ctx)
	if f.titles() != "Lab is alive" {
		t.Fatalf("heartbeat: %s", f.titles())
	}
	if m := f.messages()[0]; !strings.Contains(m.Body, "1 aplikací běží") || !strings.Contains(m.Body, "zatím žádný") {
		t.Fatalf("body: %q", m.Body)
	}
	n.auditDone(ctx, "ok", nil)
	now = time.Date(2026, 11, 10, 9, 0, 0, 0, loc)
	n.heartbeat(ctx)
	if got := f.messages(); len(got) != 2 || !strings.Contains(got[1].Body, "poslední audit: ok") {
		t.Fatalf("next day: %+v", got)
	}
}

func TestAuditCriticalNotifies(t *testing.T) {
	h := newHarness(t)
	f := installFakeNotify(t, h)
	n := h.srv.notifier
	ctx := context.Background()

	fs := findings{{"critical", "firewall-open", "the firewall does not drop incoming traffic by default"}, {"warning", "memory-low", "only 5 % of memory is available"}}
	n.auditDone(ctx, "critical", fs)
	n.auditDone(ctx, "critical", fs)
	got := f.messages()
	if len(got) != 1 || got[0].Topic != "security" || got[0].Title != "Server: 1 critical finding(s)" || got[0].Body != fs[0].Text {
		t.Fatalf("critical: %+v", got)
	}
	n.auditDone(ctx, "warning", fs[1:])
	if len(f.messages()) != 1 {
		t.Fatal("a warning was pushed")
	}
	// A different set of critical findings is news again.
	n.auditDone(ctx, "critical", findings{{"critical", "ssh-on", "the SSH server is not off: active"}})
	if len(f.messages()) != 2 {
		t.Fatal("new critical finding not pushed")
	}
}

func TestDeployFailureNotifies(t *testing.T) {
	h := newHarness(t)
	f := installFakeNotify(t, h)

	code, _, raw := h.call("POST", "/api/settings", []byte(`{"domain":"","public_ip":"127.0.0.1"}`), refMain)
	mustStatus(t, "settings", code, 200, raw)
	code, _, raw = h.call("POST", "/api/deploy?app=hello&env=prod&commit=abc1234", bundleOf(t, testApps.good, "hello", ""), refMain)
	mustStatus(t, "good deploy", code, 200, raw)
	code, _, raw = h.call("POST", "/api/deploy?app=hello&env=idea&commit=abc1235", bundleOf(t, testApps.crash, "hello", ""), "refs/heads/idea")
	mustStatus(t, "crashing preview", code, http.StatusUnprocessableEntity, raw)
	if f.titles() != "" {
		t.Fatalf("a preview failure was pushed: %s", f.titles())
	}
	code, _, raw = h.call("POST", "/api/deploy?app=hello&env=prod&commit=abc1236", bundleOf(t, testApps.crash, "hello", ""), refMain)
	mustStatus(t, "crashing prod deploy", code, http.StatusUnprocessableEntity, raw)
	got := f.messages()
	if len(got) != 1 || got[0].Title != "Deploy of hello failed" || !strings.Contains(got[0].Body, "previous release is serving again") || got[0].URL != "http://hello.127-0-0-1.sslip.io" {
		t.Fatalf("deploy failure: %+v", got)
	}
}
