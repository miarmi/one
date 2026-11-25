package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

var testApps struct{ good, crash, unhealthy, redirect string }

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "labd-test-")
	if err != nil {
		panic(err)
	}
	for mode, out := range map[string]*string{"ok": &testApps.good, "crash": &testApps.crash, "unhealthy": &testApps.unhealthy, "redirect": &testApps.redirect} {
		*out = filepath.Join(dir, "testapp-"+mode)
		cmd := exec.Command("go", "build", "-o", *out, "-ldflags", "-X main.mode="+mode, "./testdata/app")
		cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
		if b, err := cmd.CombinedOutput(); err != nil {
			panic(fmt.Sprintf("building test app: %v\n%s", err, b))
		}
	}
	// The workspace has no sqlite3 CLI; the server does. This shim implements
	// the one command labd uses, with python3's sqlite3 backup API.
	if _, err := exec.LookPath("sqlite3"); err != nil {
		bin := filepath.Join(dir, "bin")
		os.Mkdir(bin, 0o755)
		shim := "#!/usr/bin/env python3\nimport os, re, sqlite3, sys\n" +
			"args = [a for a in sys.argv[1:] if a != '-nofollow']\n" +
			"if '-nofollow' in sys.argv and os.path.islink(args[0]): sys.exit('symlink refused: ' + args[0])\n" +
			"m = re.fullmatch(r\"\\.backup '(.+)'\", args[1])\n" +
			"if not m: sys.exit('unsupported: ' + args[1])\n" +
			"s = sqlite3.connect(args[0]); d = sqlite3.connect(m.group(1)); s.backup(d); d.close(); s.close()\n"
		os.WriteFile(filepath.Join(bin, "sqlite3"), []byte(shim), 0o755)
		os.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

type harness struct {
	t      *testing.T
	ti     *testIssuer
	srv    *Server
	ts     *httptest.Server
	runner *execRunner
	edge   *noneEdge
	cfg    *Config
	exited chan struct{}
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	ti := newTestIssuer(t)
	dir := t.TempDir()
	cfg := &Config{Repo: testRepo, OIDCJWKSURL: ti.srv.URL, StateDir: filepath.Join(dir, "state"), Runner: "exec", Edge: "none", Certs: "selfsigned"}
	cfg.paths = defaultPaths("127.0.0.1:7070")
	cfg.paths.binary = filepath.Join(dir, "bin", "labd")
	if err := cfg.finish(); err != nil {
		t.Fatal(err)
	}
	store, err := openStore(cfg.StateDir)
	if err != nil {
		t.Fatal(err)
	}
	runner := newExecRunner(cfg.StateDir)
	edge := &noneEdge{}
	srv := newServer(cfg, store, runner, edge)
	srv.version = "test"
	srv.host = testHostEnv(t, &fakeHost{}) // never the real machine's settings
	srv.reboot = func() { t.Error("reboot called") }
	srv.mgr.healthTimeout = 5 * time.Second
	srv.auth.iterations = 1000 // the real 600000 takes a while for every password tried
	h := &harness{t: t, ti: ti, srv: srv, runner: runner, edge: edge, cfg: cfg, exited: make(chan struct{}, 1)}
	srv.exit = func() { h.exited <- struct{}{} }
	h.ts = httptest.NewServer(srv.routes())
	t.Cleanup(func() {
		h.ts.Close()
		runner.StopAll()
		// ApplySettings leaves EnsureCerts writing certificates in the
		// background. Wait for it, and keep a late start from writing into
		// the temp dir while the test framework removes it: the lock is
		// never released, so a goroutine that has not started yet gives up
		// at its TryLock.
		srv.mgr.certWork.Lock()
	})
	return h
}

// call sends a request with a fresh token bound to it, like the pipeline does.
func (h *harness) call(method, uri string, body []byte, ref string, mutate ...func(map[string]any)) (int, map[string]any, string) {
	h.t.Helper()
	aud := apiAudience(method, uri, sha256Hex(body))
	if strings.HasPrefix(uri, "/api/platform") {
		aud = platformAudience(sha256Hex(body))
	}
	claims := baseClaims(ref, aud)
	for _, f := range mutate {
		f(claims)
	}
	return h.raw(method, uri, body, h.ti.token(h.t, claims))
}

func (h *harness) raw(method, uri string, body []byte, token string) (int, map[string]any, string) {
	h.t.Helper()
	req, _ := http.NewRequest(method, h.ts.URL+uri, bytes.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var js map[string]any
	json.Unmarshal(raw, &js)
	return resp.StatusCode, js, string(raw)
}

func bundleOf(t *testing.T, bin, name string, extra string) []byte {
	t.Helper()
	data, err := os.ReadFile(bin)
	if err != nil {
		t.Fatal(err)
	}
	man := `{"name":"` + name + `","title":"Test","health":"/healthz"` + extra + `}`
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	tw.WriteHeader(&tar.Header{Name: "app", Mode: 0o755, Size: int64(len(data)), Typeflag: tar.TypeReg})
	tw.Write(data)
	tw.WriteHeader(&tar.Header{Name: "app.json", Mode: 0o644, Size: int64(len(man)), Typeflag: tar.TypeReg})
	tw.Write([]byte(man))
	tw.Close()
	gz.Close()
	return buf.Bytes()
}

func (h *harness) appGet(app, env, path string) (int, string) {
	h.t.Helper()
	inst, ok := h.srv.store.Get(instanceID(app, env))
	if !ok {
		h.t.Fatalf("no instance %s/%s", app, env)
	}
	resp, err := http.Get("http://127.0.0.1:" + strconv.Itoa(inst.Port) + path)
	if err != nil {
		return 0, err.Error()
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

const refMain = "refs/heads/main"

func mustStatus(t *testing.T, what string, got, want int, raw string) {
	t.Helper()
	if got != want {
		t.Fatalf("%s: status %d, want %d: %s", what, got, want, raw)
	}
}

func TestEndToEnd(t *testing.T) {
	h := newHarness(t)
	good := bundleOf(t, testApps.good, "hello", "")

	code, js, raw := h.raw("GET", "/healthz", nil, "")
	mustStatus(t, "healthz", code, 200, raw)
	if js["stage"] != "labd" {
		t.Fatalf("healthz: %s", raw)
	}

	// Deploys need settings first.
	code, _, raw = h.call("POST", "/api/deploy?app=hello&env=prod&commit=abc1234", good, refMain)
	mustStatus(t, "deploy before settings", code, 409, raw)

	code, js, raw = h.call("POST", "/api/settings", []byte(`{"domain":"","public_ip":"127.0.0.1"}`), refMain)
	mustStatus(t, "settings", code, 200, raw)
	if js["base"] != "127-0-0-1.sslip.io" {
		t.Fatalf("settings: %s", raw)
	}

	// First production deploy.
	code, js, raw = h.call("POST", "/api/deploy?app=hello&env=prod&commit=abc1234", good, refMain)
	mustStatus(t, "prod deploy", code, 200, raw)
	if js["url"] != "http://hello.127-0-0-1.sslip.io" {
		t.Fatalf("url: %s", raw)
	}
	first := js["release"].(string)
	_, envJSON := h.appGet("hello", "prod", "/env")
	var env map[string]string
	json.Unmarshal([]byte(envJSON), &env)
	if env["APP_ENV"] != "prod" || env["APP_VERSION"] != "abc1234" || env["BASE_URL"] != "http://hello.127-0-0-1.sslip.io" ||
		env["DATA_DIR"] != filepath.Join(h.cfg.StateDir, "data", "hello--prod") {
		t.Fatalf("app environment: %v", env)
	}
	if v := h.edge.last; len(v.Routes) != 1 || v.Routes[0].Host != "hello.127-0-0-1.sslip.io" {
		t.Fatalf("routes: %+v", v.Routes)
	}

	// A crashing release is refused and the old one keeps serving.
	code, js, raw = h.call("POST", "/api/deploy?app=hello&env=prod&commit=bad0001", bundleOf(t, testApps.crash, "hello", ""), refMain)
	mustStatus(t, "crashing deploy", code, 422, raw)
	if js["rolled_back"] != true || !strings.Contains(js["logs"].(string), "crashing on purpose") {
		t.Fatalf("crash result: %s", raw)
	}
	if c, body := h.appGet("hello", "prod", "/"); c != 200 || !strings.Contains(body, "abc1234") {
		t.Fatalf("old release not serving: %d %s", c, body)
	}
	// So is one that runs but never gets healthy.
	code, js, raw = h.call("POST", "/api/deploy?app=hello&env=prod&commit=bad0002", bundleOf(t, testApps.unhealthy, "hello", ""), refMain)
	mustStatus(t, "unhealthy deploy", code, 422, raw)
	if inst, _ := h.srv.store.Get("hello--prod"); inst.Release != first || inst.Status != "running" {
		t.Fatalf("state after failed deploys: %+v", inst)
	}

	// Authorization and token binding.
	denied := []struct {
		what, method, uri, ref string
		want                   int
	}{
		{"branch deploying prod", "POST", "/api/deploy?app=hello&env=prod", "refs/heads/idea", 403},
		{"branch deploying another preview", "POST", "/api/deploy?app=hello&env=other", "refs/heads/idea", 403},
		{"main deploying a preview", "POST", "/api/deploy?app=hello&env=idea", refMain, 403},
		{"tag deploying prod", "POST", "/api/deploy?app=hello&env=prod", "refs/tags/v1", 403},
		{"branch changing settings", "POST", "/api/settings", "refs/heads/idea", 403},
		{"branch running gc", "POST", "/api/gc", "refs/heads/idea", 403},
		{"branch rolling back prod", "POST", "/api/rollback?app=hello&env=prod", "refs/heads/idea", 403},
		{"branch deleting another preview", "DELETE", "/api/instances?app=hello&env=other", "refs/heads/idea", 403},
		{"deleting prod without retire", "DELETE", "/api/instances?app=hello&env=prod", refMain, 400},
		{"retiring prod from a branch", "DELETE", "/api/instances?app=hello&env=prod&retire=hello", "refs/heads/idea", 403},
		{"bad app name", "POST", "/api/deploy?app=../x&env=prod", refMain, 400},
		{"bad env", "POST", "/api/deploy?app=hello&env=a--b", refMain, 400},
	}
	for _, d := range denied {
		code, _, raw := h.call(d.method, d.uri, good, d.ref)
		if code != d.want {
			t.Errorf("%s: status %d, want %d: %s", d.what, code, d.want, raw)
		}
	}
	// A token for one request does not work for another.
	tok := h.ti.token(t, baseClaims(refMain, apiAudience("GET", "/api/status", sha256Hex(nil))))
	code, _, raw = h.raw("POST", "/api/deploy?app=hello&env=prod", good, tok)
	mustStatus(t, "token for another request", code, 401, raw)
	// And a token works only once.
	tok = h.ti.token(t, baseClaims(refMain, apiAudience("GET", "/api/status", sha256Hex(nil))))
	code, _, raw = h.raw("GET", "/api/status", nil, tok)
	mustStatus(t, "status", code, 200, raw)
	code, _, raw = h.raw("GET", "/api/status", nil, tok)
	mustStatus(t, "replayed token", code, 401, raw)
	code, _, raw = h.raw("GET", "/api/status", nil, "")
	mustStatus(t, "no token", code, 401, raw)
	code, _, raw = h.call("GET", "/api/status", nil, refMain, func(c map[string]any) { c["repository"] = "evil/lab" })
	mustStatus(t, "other repo", code, 401, raw)

	// A preview gets a copy of production's data.
	prodData := filepath.Join(h.cfg.StateDir, "data", "hello--prod")
	py := exec.Command("python3", "-c", `import sqlite3,sys
c=sqlite3.connect(sys.argv[1]); c.execute("create table notes(t text)"); c.execute("insert into notes values('from prod')"); c.commit()`,
		filepath.Join(prodData, "hello.db"))
	if b, err := py.CombinedOutput(); err != nil {
		t.Fatalf("seeding prod db: %v %s", err, b)
	}
	os.MkdirAll(filepath.Join(prodData, "uploads"), 0o700)
	os.WriteFile(filepath.Join(prodData, "uploads", "a.txt"), []byte("attachment"), 0o600)
	os.WriteFile(filepath.Join(prodData, "hello.db-wal"), []byte("stale wal"), 0o600)
	os.Symlink("/etc/passwd", filepath.Join(prodData, "link"))
	os.MkdirAll(filepath.Join(prodData, "secrets"), 0o700)
	os.WriteFile(filepath.Join(prodData, "secrets", "api_key"), []byte("sk-prod"), 0o600)

	// A branch named otherwise can't take a preview's name.
	code, _, raw = h.call("POST", "/api/deploy?app=hello&env=feature-dark-mode&commit=def5678", good, "refs/heads/Feature/Dark Mode!")
	mustStatus(t, "preview deploy from a look-alike branch", code, 403, raw)
	branch := "refs/heads/feature-dark-mode"
	code, js, raw = h.call("POST", "/api/deploy?app=hello&env=feature-dark-mode&commit=def5678", good, branch)
	mustStatus(t, "preview deploy", code, 200, raw)
	if js["url"] != "http://feature-dark-mode--hello.127-0-0-1.sslip.io" {
		t.Fatalf("preview url: %s", raw)
	}
	previewData := filepath.Join(h.cfg.StateDir, "data", "hello--feature-dark-mode")
	out, err := exec.Command("python3", "-c", `import sqlite3,sys
print(sqlite3.connect(sys.argv[1]).execute("select t from notes").fetchone()[0])`, filepath.Join(previewData, "hello.db")).CombinedOutput()
	if err != nil || strings.TrimSpace(string(out)) != "from prod" {
		t.Fatalf("preview db: %v %s", err, out)
	}
	if b, _ := os.ReadFile(filepath.Join(previewData, "uploads", "a.txt")); string(b) != "attachment" {
		t.Fatal("plain file not copied")
	}
	for _, skipped := range []string{"hello.db-wal", "link", "secrets"} {
		if _, err := os.Lstat(filepath.Join(previewData, skipped)); !os.IsNotExist(err) {
			t.Fatalf("%s should not be copied", skipped)
		}
	}
	_, envJSON = h.appGet("hello", "feature-dark-mode", "/env")
	if !strings.Contains(envJSON, `"APP_ENV":"feature-dark-mode"`) {
		t.Fatalf("preview env: %s", envJSON)
	}

	// Second good prod release, then roll back to the first.
	code, js, raw = h.call("POST", "/api/deploy?app=hello&env=prod&commit=0a1b2c3", good, refMain)
	mustStatus(t, "second prod deploy", code, 200, raw)
	if js["previous"] != first {
		t.Fatalf("previous: %s", raw)
	}
	code, js, raw = h.call("POST", "/api/rollback?app=hello&env=prod", nil, refMain)
	mustStatus(t, "rollback", code, 200, raw)
	if js["release"] != first {
		t.Fatalf("rollback result: %s", raw)
	}
	if _, body := h.appGet("hello", "prod", "/"); !strings.Contains(body, "abc1234") {
		t.Fatalf("rollback not serving the first release: %s", body)
	}

	// Logs.
	code, _, raw = h.call("GET", "/api/logs?app=hello&env=prod&lines=50", nil, "refs/heads/anything")
	mustStatus(t, "logs", code, 200, raw)
	if !strings.Contains(raw, "testapp listening") {
		t.Fatalf("logs: %s", raw)
	}

	// Status.
	code, js, raw = h.call("GET", "/api/status", nil, refMain)
	mustStatus(t, "status", code, 200, raw)
	if n := len(js["instances"].([]any)); n != 2 {
		t.Fatalf("status lists %d instances: %s", n, raw)
	}

	// The branch deletes its own preview: unit, files, data and route go.
	code, _, raw = h.call("DELETE", "/api/instances?app=hello&env=feature-dark-mode", nil, branch)
	mustStatus(t, "delete preview", code, 200, raw)
	if _, err := os.Stat(previewData); !os.IsNotExist(err) {
		t.Fatal("preview data left behind")
	}
	if _, err := os.Stat(filepath.Join(h.cfg.StateDir, "instances", "hello--feature-dark-mode")); !os.IsNotExist(err) {
		t.Fatal("preview files left behind")
	}
	if h.runner.Active(t.Context(), "hello--feature-dark-mode") {
		t.Fatal("preview still running")
	}
	code, _, raw = h.call("DELETE", "/api/instances?app=hello&env=feature-dark-mode", nil, branch)
	mustStatus(t, "delete again", code, 404, raw)

	// GC removes previews of branches that are gone.
	code, _, raw = h.call("POST", "/api/deploy?app=hello&env=old-idea&commit=1111111", good, "refs/heads/old-idea")
	mustStatus(t, "old preview", code, 200, raw)
	code, js, raw = h.call("POST", "/api/gc", []byte(`{"previews":["something-else"]}`), refMain)
	mustStatus(t, "gc", code, 200, raw)
	if fmt.Sprint(js["removed"]) != "[hello--old-idea]" {
		t.Fatalf("gc: %s", raw)
	}

	// A first deploy that fails leaves a failed instance with no route.
	code, js, raw = h.call("POST", "/api/deploy?app=broke&env=prod&commit=2222222", bundleOf(t, testApps.crash, "broke", ""), refMain)
	mustStatus(t, "broken first deploy", code, 422, raw)
	if js["rolled_back"] != false {
		t.Fatalf("broken first deploy: %s", raw)
	}
	if inst, _ := h.srv.store.Get("broke--prod"); inst.Status != "failed" {
		t.Fatalf("broke status: %+v", inst)
	}
	for _, r := range h.edge.last.Routes {
		if strings.HasPrefix(r.Host, "broke.") {
			t.Fatal("failed app has a route")
		}
	}
	// A later good deploy fixes it.
	code, _, raw = h.call("POST", "/api/deploy?app=broke&env=prod&commit=3333333", bundleOf(t, testApps.good, "broke", ""), refMain)
	mustStatus(t, "fixing deploy", code, 200, raw)

	// Retiring production: only from main, only with retire=<app>, and then
	// unit, files, data and route are gone for good.
	code, _, raw = h.call("DELETE", "/api/instances?app=broke&env=prod", nil, refMain)
	mustStatus(t, "retire without confirmation", code, 400, raw)
	code, _, raw = h.call("DELETE", "/api/instances?app=broke&env=prod&retire=hello", nil, refMain)
	mustStatus(t, "retire with the wrong app", code, 400, raw)
	code, _, raw = h.call("DELETE", "/api/instances?app=broke&env=prod&retire=broke", nil, "refs/heads/idea")
	mustStatus(t, "retire from a branch", code, 403, raw)
	brokeData, _ := h.runner.DataDir("broke--prod")
	code, _, raw = h.call("DELETE", "/api/instances?app=broke&env=prod&retire=broke", nil, refMain)
	mustStatus(t, "retire prod", code, 200, raw)
	if _, err := os.Stat(brokeData); !os.IsNotExist(err) {
		t.Fatal("retired prod data left behind")
	}
	if h.runner.Active(t.Context(), "broke--prod") {
		t.Fatal("retired prod still running")
	}
	if _, ok := h.srv.store.Get("broke--prod"); ok {
		t.Fatal("retired prod still in the store")
	}
	for _, r := range h.edge.last.Routes {
		if strings.HasPrefix(r.Host, "broke.") {
			t.Fatal("retired app still has a route")
		}
	}

	// Access gate: an allow list in the settings, and short-lived passes for CI.
	code, js, raw = h.call("POST", "/api/settings", []byte(`{"domain":"","public_ip":"127.0.0.1","allow_ips":["203.0.113.7","198.51.100.9/24"]}`), refMain)
	mustStatus(t, "settings with allow list", code, 200, raw)
	if got := fmt.Sprint(h.edge.last.AllowIPs); got != "[203.0.113.7 198.51.100.0/24]" {
		t.Fatalf("edge allow list %s", got)
	}
	for _, bad := range []string{`["0.0.0.0/0"]`, `["nonsense"]`, `["1.2.3.4; allow all"]`} {
		code, _, raw = h.call("POST", "/api/settings", []byte(`{"domain":"","public_ip":"127.0.0.1","allow_ips":`+bad+`}`), refMain)
		mustStatus(t, "bad allow list "+bad, code, 400, raw)
	}
	code, _, raw = h.call("POST", "/api/session?app=hello&env=prod", nil, "refs/heads/idea")
	mustStatus(t, "session for prod from a branch", code, 403, raw)
	code, _, raw = h.call("POST", "/api/session?app=nope&env=prod", nil, refMain)
	mustStatus(t, "session for a missing app", code, 404, raw)
	code, js, raw = h.call("POST", "/api/session?app=hello&env=prod", nil, refMain)
	mustStatus(t, "session", code, 200, raw)
	cookie, _ := js["cookie"].(string)
	authCheck := func(c string, host ...string) int {
		req, _ := http.NewRequest("GET", h.ts.URL+"/_lab/auth", nil)
		req.Header.Set("X-Lab-Host", "hello.127-0-0-1.sslip.io")
		if len(host) > 0 {
			req.Header.Set("X-Lab-Host", host[0])
		}
		if c != "" {
			req.AddCookie(&http.Cookie{Name: "lab_session", Value: c})
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if got := authCheck(cookie); got != 204 {
		t.Fatalf("valid pass: %d", got)
	}
	if got := authCheck(""); got != 401 {
		t.Fatalf("no pass: %d", got)
	}
	if got := authCheck(cookie[:len(cookie)-2] + "AA"); got != 401 {
		t.Fatalf("forged pass: %d", got)
	}
	// A pass is for one host: production's doesn't open a preview, nor the reverse.
	code, _, raw = h.call("POST", "/api/deploy?app=hello&env=gate-test&commit=2222222", good, "refs/heads/gate-test")
	mustStatus(t, "preview for the gate", code, 200, raw)
	if got := authCheck(cookie, "gate-test--hello.127-0-0-1.sslip.io"); got != 401 {
		t.Fatalf("prod pass on a preview: %d", got)
	}
	code, js, raw = h.call("POST", "/api/session?app=hello&env=gate-test", nil, "refs/heads/gate-test")
	mustStatus(t, "preview session", code, 200, raw)
	previewPass, _ := js["cookie"].(string)
	if got := authCheck(previewPass, "gate-test--hello.127-0-0-1.sslip.io"); got != 204 {
		t.Fatalf("preview pass on its preview: %d", got)
	}
	if got := authCheck(previewPass); got != 401 {
		t.Fatalf("preview pass on production: %d", got)
	}
	// Restart: state survives, and reconcile brings stopped instances back.
	h.runner.StopAll()
	store2, err := openStore(h.cfg.StateDir)
	if err != nil {
		t.Fatal(err)
	}
	mgr2 := newManager(h.cfg, store2, h.runner, h.edge)
	mgr2.Reconcile(t.Context())
	if c, _ := h.appGet("hello", "prod", "/healthz"); c != 200 {
		t.Fatal("reconcile did not restart prod")
	}
}

func TestSelfUpdate(t *testing.T) {
	h := newHarness(t)
	os.MkdirAll(filepath.Dir(h.cfg.paths.binary), 0o755)
	os.WriteFile(h.cfg.paths.binary, []byte("old labd"), 0o755)
	h.cfg.path = filepath.Join(t.TempDir(), "labd.json")
	os.WriteFile(h.cfg.path, []byte(`{"repo":"OWNER/REPO"}`), 0o644)

	failing := []byte("#!/bin/sh\ncase \"$1\" in -version) echo 'labd 2.0';; *) echo 'selftest failed' >&2; exit 1;; esac\n")
	code, _, raw := h.call("POST", "/api/platform", failing, refMain)
	mustStatus(t, "failing selftest", code, 400, raw)
	if b, _ := os.ReadFile(h.cfg.paths.binary); string(b) != "old labd" {
		t.Fatal("binary replaced despite failing selftest")
	}

	code, _, raw = h.call("POST", "/api/platform", []byte("#!/bin/sh\necho 'labd 2.0'\n"), "refs/heads/idea")
	mustStatus(t, "platform from a branch", code, 403, raw)

	good := []byte("#!/bin/sh\necho 'labd 2.0'\n")
	code, js, raw := h.call("POST", "/api/platform", good, refMain)
	mustStatus(t, "platform", code, 200, raw)
	if js["installed"] != "labd 2.0" {
		t.Fatalf("installed: %s", raw)
	}
	if b, _ := os.ReadFile(h.cfg.paths.binary); !bytes.Equal(b, good) {
		t.Fatal("binary not replaced")
	}
	if b, _ := os.ReadFile(h.cfg.paths.binary + ".prev"); string(b) != "old labd" {
		t.Fatal("old binary not kept")
	}
	select {
	case <-h.exited:
	case <-time.After(3 * time.Second):
		t.Fatal("labd did not exit after installing")
	}
	entries, _ := os.ReadDir(filepath.Dir(h.cfg.paths.binary))
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".labd-upload") {
			t.Fatalf("upload left behind: %s", e.Name())
		}
	}
}

// A pipeline run cancelled mid-deploy drops the request; the deploy must
// still finish instead of stopping the app halfway.
func TestDeploySurvivesCancelledRequest(t *testing.T) {
	h := newHarness(t)
	code, _, raw := h.call("POST", "/api/settings", []byte(`{"domain":"","public_ip":"127.0.0.1"}`), refMain)
	mustStatus(t, "settings", code, 200, raw)
	send := func(commit string) *httptest.ResponseRecorder {
		body := bundleOf(t, testApps.good, "hello", "")
		uri := "/api/deploy?app=hello&env=prod&commit=" + commit
		tok := h.ti.token(t, baseClaims(refMain, apiAudience("POST", uri, sha256Hex(body))))
		ctx, cancel := context.WithCancel(context.Background())
		cancel() // the client is already gone
		req := httptest.NewRequest("POST", uri, bytes.NewReader(body)).WithContext(ctx)
		req.Header.Set("Authorization", "Bearer "+tok)
		rec := httptest.NewRecorder()
		h.srv.routes().ServeHTTP(rec, req)
		return rec
	}
	for _, commit := range []string{"abc1234", "def5678"} {
		if rec := send(commit); rec.Code != 200 {
			t.Fatalf("deploy %s with a cancelled request: %d %s", commit, rec.Code, rec.Body.String())
		}
		if c, body := h.appGet("hello", "prod", "/"); c != 200 || !strings.Contains(body, commit) {
			t.Fatalf("after %s: %d %s", commit, c, body)
		}
	}
}

// The health check doesn't follow redirects: an app can't make labd fetch
// another address (security review L4).
func TestHealthCheckIgnoresRedirects(t *testing.T) {
	h := newHarness(t)
	h.call("POST", "/api/settings", []byte(`{"domain":"","public_ip":"127.0.0.1"}`), refMain)
	code, _, raw := h.call("POST", "/api/deploy?app=hello&env=prod&commit=abc1234", bundleOf(t, testApps.redirect, "hello", ""), refMain)
	mustStatus(t, "deploy of an app whose health check redirects", code, http.StatusUnprocessableEntity, raw)
}

// A failed deploy over an instance that had already failed leaves "current"
// pointing at a release that exists, or at nothing (security review L5).
func TestFailedDeployOverFailedInstance(t *testing.T) {
	h := newHarness(t)
	h.call("POST", "/api/settings", []byte(`{"domain":"","public_ip":"127.0.0.1"}`), refMain)
	current := filepath.Join(h.cfg.StateDir, "instances", "hello--prod", "current")
	check := func(what string) {
		t.Helper()
		if _, err := os.Lstat(current); err == nil {
			if _, err := os.Stat(current); err != nil {
				t.Fatalf("%s: current points at a removed release: %v", what, err)
			}
		}
	}
	for i, bin := range []string{testApps.crash, testApps.crash, testApps.unhealthy} {
		code, _, raw := h.call("POST", "/api/deploy?app=hello&env=prod&commit=bad000"+strconv.Itoa(i), bundleOf(t, bin, "hello", ""), refMain)
		mustStatus(t, "failing deploy", code, http.StatusUnprocessableEntity, raw)
		check("after failing deploy " + strconv.Itoa(i))
	}
	code, _, raw := h.call("POST", "/api/deploy?app=hello&env=prod&commit=abc1234", bundleOf(t, testApps.good, "hello", ""), refMain)
	mustStatus(t, "good deploy after failures", code, 200, raw)
	check("after the good deploy")
}

// The state and settings files are root's alone; the state directory can be
// passed through but not listed (security review L7).
func TestStateFilesArePrivate(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, "settings.json"), []byte(`{"domain":"","public_ip":"127.0.0.1"}`), 0o644)
	s, err := openStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Put(Instance{App: "hello", Env: "prod", Port: 20000, Status: "running"}); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]os.FileMode{".": 0o711, "state.json": 0o600, "settings.json": 0o600} {
		info, err := os.Stat(filepath.Join(dir, name))
		if err != nil || info.Mode().Perm() != want {
			t.Errorf("%s: mode %v, want %v (%v)", name, info.Mode().Perm(), want, err)
		}
	}
}
