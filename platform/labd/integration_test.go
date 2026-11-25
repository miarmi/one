//go:build integration

package main

// TestIntegration runs labd the way the server does: the real binary as the
// labd.service unit from the server setup text, with the systemd runner and
// the nginx edge, on a throwaway machine (the pipeline's runner) as root.
// Acting as the pipeline, it checks what CONTRACT.md promises: the hardened
// app sandbox, the access gate, copies of production data, host hardening,
// platform updates and the guard that puts the previous labd back.
// platform/scripts/integration-test.sh builds hello first and runs this.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

const (
	itBinary = "/usr/local/bin/labd"
	itState  = "/srv/lab"
	itData   = "/var/lib/private/lab"
	itBase   = "127-0-0-1.sslip.io"
)

type itLab struct {
	t         *testing.T
	ti        *testIssuer
	web       *http.Client
	bindRules bool // this systemd enforces SocketBindAllow
}

func sh(t *testing.T, name string, args ...string) string {
	t.Helper()
	out, err := exec.Command(name, args...).CombinedOutput()
	if err != nil {
		t.Fatalf("%s %s: %v\n%s", name, strings.Join(args, " "), err, out)
	}
	return string(out)
}

func buildLabd(t *testing.T, version, out string) {
	t.Helper()
	cmd := exec.Command("go", "build", "-trimpath", "-ldflags", "-X main.version="+version, "-o", out, ".")
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("building labd: %v\n%s", err, b)
	}
}

// fromSetupText returns one of the files the server setup text installs.
func fromSetupText(t *testing.T, name string) string {
	return sh(t, "python3", "-c", "import sys; sys.path.insert(0, '../cloudinit'); import build; sys.stdout.write(build."+name+")")
}

func TestIntegration(t *testing.T) {
	if os.Getenv("LAB_INTEGRATION") != "1" || os.Geteuid() != 0 {
		t.Skip("runs only from platform/scripts/integration-test.sh, as root on a throwaway machine")
	}
	lab := &itLab{t: t, ti: newTestIssuer(t), web: &http.Client{Timeout: 90 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
	t.Cleanup(func() {
		if t.Failed() {
			for _, c := range [][]string{
				{"journalctl", "--no-pager", "-n", "300", "-u", "labd", "-u", "labd-fallback"},
				{"journalctl", "--no-pager", "-n", "200", "-u", "lab-app-*"},
				{"systemctl", "--no-pager", "status", "labd", "lab-app-*"},
				{"nft", "list", "table", "inet", "lab_apps"},
				{"sh", "-c", "ls -la /etc/modprobe.d; head -8 /etc/modprobe.d/00-lab-hardening.conf; modprobe -n -v ksmbd"},
				{"sh", "-c", "curl -s -H 'Authorization: x' http://127.0.0.1/_lab/edge"},
				{"cat", "/etc/nginx/sites-available/lab"},
				{"tail", "-n", "40", "/var/log/nginx/error.log"},
			} {
				out, _ := exec.Command(c[0], c[1:]...).CombinedOutput()
				t.Logf("$ %s\n%s", strings.Join(c, " "), out)
			}
		}
	})

	// The server as the setup text leaves it, with labd instead of the seed.
	buildLabd(t, "itest01", itBinary)
	os.MkdirAll("/etc/lab", 0o755)
	cfg, _ := json.Marshal(map[string]string{"repo": testRepo, "listen": "127.0.0.1:7070", "oidc_jwks_url": lab.ti.srv.URL, "certs": "selfsigned"})
	os.WriteFile("/etc/lab/labd.json", cfg, 0o644)
	os.WriteFile("/etc/systemd/system/labd.service", []byte(fromSetupText(t, "LABD_SERVICE")), 0o644)
	os.WriteFile("/etc/nginx/sites-available/lab", []byte(fromSetupText(t, "NGINX_SITE")), 0o644)
	os.Remove("/etc/nginx/sites-enabled/default")
	os.Symlink("/etc/nginx/sites-available/lab", "/etc/nginx/sites-enabled/lab")
	os.MkdirAll("/var/www/acme", 0o755)
	os.MkdirAll(itState, 0o755)
	sh(t, "systemctl", "daemon-reload")
	sh(t, "systemctl", "restart", "nginx")
	sh(t, "systemctl", "start", "labd")
	lab.waitVersion("itest01", 90*time.Second)

	t.Run("sandbox and host", lab.sandboxAndHost)
	t.Run("apps, gate and sandbox probe", lab.appsAndProbe)
	t.Run("every app runs in the hardened sandbox", lab.everyApp)
	t.Run("preview copies production data safely", lab.previewData)
	t.Run("apps come back after a reboot", lab.reboot)
	t.Run("platform update and the guard", lab.platformUpdate)
	t.Run("domain mode: HTTPS and the login gate", lab.domainMode)
}

// waitVersion waits until labd answers with this version.
func (l *itLab) waitVersion(version string, within time.Duration) {
	l.t.Helper()
	deadline := time.Now().Add(within)
	last := ""
	for time.Now().Before(deadline) {
		resp, err := http.Get("http://127.0.0.1:7070/healthz")
		if err == nil {
			b, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			last = string(b)
			var js map[string]any
			if json.Unmarshal(b, &js) == nil && js["version"] == version {
				return
			}
		} else {
			last = err.Error()
		}
		time.Sleep(time.Second)
	}
	l.t.Fatalf("labd %s did not come up within %v (last answer: %s)", version, within, last)
}

// waitDown waits until labd stops answering.
func (l *itLab) waitDown(within time.Duration) {
	l.t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		resp, err := http.Get("http://127.0.0.1:7070/healthz")
		if err != nil {
			return
		}
		resp.Body.Close()
		time.Sleep(200 * time.Millisecond)
	}
	l.t.Fatalf("labd still answers %v after installing a new one", within)
}

// api calls labd through nginx with a token bound to the request, like
// platform/scripts/lab-api.sh.
func (l *itLab) api(method, uri string, body []byte, ref string) (int, map[string]any, string) {
	l.t.Helper()
	aud := apiAudience(method, uri, sha256Hex(body))
	if uri == "/api/platform" {
		aud = platformAudience(sha256Hex(body))
	}
	req, _ := http.NewRequest(method, "http://127.0.0.1"+uri, bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+l.ti.token(l.t, baseClaims(ref, aud)))
	resp, err := l.web.Do(req)
	if err != nil {
		l.t.Fatalf("%s %s: %v", method, uri, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var js map[string]any
	json.Unmarshal(raw, &js)
	return resp.StatusCode, js, string(raw)
}

// page fetches an app page through nginx, as a browser at host would.
func (l *itLab) page(method, host, path, cookie string, form url.Values) (int, string) {
	l.t.Helper()
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req, _ := http.NewRequest(method, "http://127.0.0.1"+path, body)
	req.Host = host
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Origin", "http://"+host)
	}
	if cookie != "" {
		req.Header.Set("Cookie", "lab_session="+cookie)
	}
	resp, err := l.web.Do(req)
	if err != nil {
		l.t.Fatalf("%s http://%s%s: %v", method, host, path, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func (l *itLab) pass(app, env, ref string) string {
	l.t.Helper()
	code, js, raw := l.api("POST", "/api/session?app="+app+"&env="+env, nil, ref)
	mustStatus(l.t, "pass for "+app+"/"+env, code, 200, raw)
	return js["cookie"].(string)
}

func (l *itLab) deploy(app, env, ref, bundle string) {
	l.t.Helper()
	data, err := os.ReadFile(bundle)
	if err != nil {
		l.t.Fatal(err)
	}
	code, _, raw := l.api("POST", "/api/deploy?app="+app+"&env="+env+"&commit=abc1234", data, ref)
	mustStatus(l.t, "deploy "+app+"/"+env, code, 200, raw)
}

func (l *itLab) sandboxAndHost(t *testing.T) {
	code, _, raw := l.api("POST", "/api/settings", []byte(`{"domain":"","public_ip":"127.0.0.1","allow_ips":["192.0.2.1"]}`), refMain)
	mustStatus(t, "settings", code, 200, raw)
	var js map[string]any
	for i := 0; i < 60; i++ { // labd's startup work runs in the background
		code, js, raw = l.api("GET", "/api/status", nil, refMain)
		mustStatus(t, "status", code, 200, raw)
		if sb, _ := js["sandbox"].(map[string]any); sb["settled"] == true {
			break
		}
		time.Sleep(time.Second)
	}
	sb, _ := js["sandbox"].(map[string]any)
	// systemd enforces binding rules only when built with its BPF framework
	// (Debian 13 is; the runner's Ubuntu 24.04 isn't): labd must say which.
	l.bindRules = !strings.Contains(sh(t, "systemctl", "--version"), "-BPF_FRAMEWORK")
	if sb["hardened"] != true || sb["bind_rules"] != l.bindRules || sb["firewall"] != "applied" || sb["version"] != sandboxVersion {
		t.Fatalf("sandbox (binding rules expected: %v): %s", l.bindRules, raw)
	}
	code, js, raw = l.api("GET", "/api/host", nil, refMain)
	mustStatus(t, "host", code, 200, raw)
	hard, _ := js["hardening"].(map[string]any)
	sys, _ := hard["sysctl"].(map[string]any)
	if failed, _ := sys["failed"].([]any); hard == nil || len(failed) != 0 {
		t.Fatalf("host hardening: %s", raw)
	}
	if hard["labd_guard"] != "installed" || (hard["ssh"] != "off" && hard["ssh"] != "not installed") {
		t.Fatalf("host hardening: %s", raw)
	}
	for key, want := range map[string]string{"kernel/kptr_restrict": "2", "kernel/io_uring_disabled": "2", "kernel/unprivileged_bpf_disabled": "1"} {
		if b, err := os.ReadFile("/proc/sys/" + key); err == nil && strings.TrimSpace(string(b)) != want {
			t.Errorf("%s = %s, want %s", key, b, want)
		}
	}
	for _, m := range []string{"ksmbd", "tls", "sctp"} {
		if _, err := os.Stat("/sys/module/" + m); err == nil {
			t.Logf("%s is loaded here already (built in, or in use), so loading it can't be checked", m)
			continue
		}
		if out := sh(t, "modprobe", "-n", "-v", m); !strings.Contains(out, "install /bin/false") {
			t.Errorf("%s can still be loaded: %s", m, out)
		}
	}
	if !strings.Contains(sh(t, "systemctl", "show", "labd", "-p", "OnFailure"), "labd-fallback.service") {
		t.Error("the labd guard is not active")
	}
}

func (l *itLab) appsAndProbe(t *testing.T) {
	l.deploy("hello", "prod", refMain, "../../dist/hello.tar.gz")
	// The probe app: labd's test app, asked to try what a broken-into app
	// would try.
	dir := t.TempDir()
	bin := filepath.Join(dir, "app")
	cmd := exec.Command("go", "build", "-o", bin, "./testdata/app")
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, b)
	}
	os.WriteFile(filepath.Join(dir, "probe.tar.gz"), bundleOf(t, bin, "probe", ""), 0o644)
	l.deploy("probe", "prod", refMain, filepath.Join(dir, "probe.tar.gz"))

	// The gate: nothing without a pass; a pass opens only its own host.
	helloHost, probeHost := "hello."+itBase, "probe."+itBase
	if code, body := l.page("GET", helloHost, "/", "", nil); code != 403 || !strings.Contains(body, "This lab is private.") {
		t.Fatalf("no pass: %d %s", code, body)
	}
	helloPass, probePass := l.pass("hello", "prod", refMain), l.pass("probe", "prod", refMain)
	if code, _ := l.page("GET", helloHost, "/", probePass, nil); code != 403 {
		t.Fatalf("another app's pass opened hello: %d", code)
	}
	if code, body := l.page("POST", helloHost, "/notes", helloPass, url.Values{"text": {"from the integration test"}}); code != 303 {
		t.Fatalf("adding a note: %d %s", code, body)
	}
	if code, body := l.page("GET", helloHost, "/", helloPass, nil); code != 200 || !strings.Contains(body, "from the integration test") {
		t.Fatalf("hello: %d %s", code, body)
	}

	var st struct {
		Instances map[string]Instance `json:"instances"`
	}
	raw, _ := os.ReadFile(filepath.Join(itState, "state.json"))
	json.Unmarshal(raw, &st)
	var addrs []string
	for _, a := range localAddrs("") {
		addrs = append(addrs, a.String())
	}
	q := url.Values{"state": {itState}, "other": {strconv.Itoa(st.Instances["hello--prod"].Port)}, "addrs": {strings.Join(addrs, ",")}}
	// labd waits until nginx serves a new route before a deploy answers, so
	// the probe answers at once.
	code, body := l.page("GET", probeHost, "/probe?"+q.Encode(), probePass, nil)
	if code != 200 {
		t.Fatalf("probe: %d %s", code, body)
	}
	var res map[string]struct {
		OK     bool   `json:"ok"`
		Detail string `json:"detail"`
	}
	if err := json.Unmarshal([]byte(body), &res); err != nil {
		t.Fatalf("probe: %v %s", err, body)
	}
	for _, want := range []string{"connect_labd", "connect_other_app", "connect_metadata", "connect_internet", "bind_other_port",
		"unix_socket", "write_data_dir", "private_files", "write_tmp", "labd_state_hidden", "labd_state_listing", "exec_shell",
		"exec_from_data_dir", "see_other_processes", "see_kernel_settings", "io_uring", "keyring", "userfaultfd",
		"user_namespace", "setuid", "chown", "setrlimit", "mlock", "write_execute_memory"} {
		if _, ok := res[want]; !ok {
			t.Errorf("probe did not check %s", want)
		}
	}
	for name, r := range res {
		switch {
		case r.OK:
			t.Logf("sandbox ok: %s: %s", name, r.Detail)
		case name == "bind_other_port" && !l.bindRules:
			t.Logf("sandbox: %s: %s (this systemd doesn't enforce binding rules)", name, r.Detail)
		default:
			t.Errorf("sandbox: %s: %s", name, r.Detail)
		}
	}
	// Even an app that takes another program's port can't answer on it.
	if code, body := l.page("GET", probeHost, "/impostor?addr=127.0.0.1:7999", probePass, nil); code != 200 {
		t.Fatalf("impostor: %d %s", code, body)
	} else if strings.HasPrefix(body, "listening") {
		if c, err := net.DialTimeout("tcp", "127.0.0.1:7999", 3*time.Second); err == nil {
			c.SetReadDeadline(time.Now().Add(3 * time.Second))
			b, _ := io.ReadAll(c)
			c.Close()
			if strings.Contains(string(b), "impostor") {
				t.Error("an app answered on a port outside the apps' range")
			}
		}
	} else if l.bindRules {
		t.Logf("impostor: %s", body)
	} else {
		t.Errorf("impostor could not listen although binding isn't limited here: %s", body)
	}
	if len(addrs) == 0 {
		t.Log("no non-loopback address to probe")
	}
}

// everyApp deploys every app built from this branch (dist/<app>.tar.gz) and
// checks it is healthy and runs in the hardened sandbox.
func (l *itLab) everyApp(t *testing.T) {
	bundles, _ := filepath.Glob("../../dist/*.tar.gz")
	for _, b := range bundles {
		app := strings.TrimSuffix(filepath.Base(b), ".tar.gz")
		if app == "hello" {
			continue // deployed above
		}
		l.deploy(app, "prod", refMain, b)
	}
	code, js, raw := l.api("GET", "/api/status", nil, refMain)
	mustStatus(t, "status", code, 200, raw)
	for _, inst := range js["instances"].([]any) {
		i := inst.(map[string]any)
		if i["status"] != "running" || i["sandbox"] != "hardened" {
			t.Errorf("%s/%s: %s in the %s sandbox", i["app"], i["env"], i["status"], i["sandbox"])
		}
	}
}

func (l *itLab) previewData(t *testing.T) {
	prod := filepath.Join(itData, "hello--prod")
	// What a broken-into production app could plant for the copy to follow.
	os.Symlink(filepath.Join(itState, "secret"), filepath.Join(prod, "leak.txt"))
	os.Symlink(filepath.Join(itState, "state.json"), filepath.Join(prod, "evil.db"))
	os.Symlink(filepath.Join(itData, "probe--prod"), filepath.Join(prod, "neighbour"))
	os.MkdirAll(filepath.Join(prod, "secrets"), 0o700)
	os.WriteFile(filepath.Join(prod, "secrets", "api_key"), []byte("sk-prod"), 0o600)
	os.MkdirAll(filepath.Join(prod, "uploads"), 0o700)
	os.WriteFile(filepath.Join(prod, "uploads", "a.txt"), []byte("attachment"), 0o600)

	l.deploy("hello", "itest", "refs/heads/itest", "../../dist/hello.tar.gz")
	preview := filepath.Join(itData, "hello--itest")
	for _, gone := range []string{"leak.txt", "evil.db", "neighbour", "secrets"} {
		if _, err := os.Lstat(filepath.Join(preview, gone)); !os.IsNotExist(err) {
			t.Errorf("%s was copied into the preview", gone)
		}
	}
	if b, _ := os.ReadFile(filepath.Join(preview, "uploads", "a.txt")); string(b) != "attachment" {
		t.Error("a plain file was not copied")
	}
	pass := l.pass("hello", "itest", "refs/heads/itest")
	if code, body := l.page("GET", "itest--hello."+itBase, "/", pass, nil); code != 200 || !strings.Contains(body, "from the integration test") {
		t.Fatalf("the preview lacks production's note: %d %s", code, body)
	}
	// promote-data: the preview's data, secrets included, replaces
	// production's, which is kept beside it; systemd gives the copy to
	// production's user on start.
	os.MkdirAll(filepath.Join(preview, "secrets"), 0o700)
	os.WriteFile(filepath.Join(preview, "secrets", "token"), []byte("tok-preview"), 0o600)
	code, js, raw := l.api("POST", "/api/promote-data?app=hello&env=itest&confirm=hello", nil, refMain)
	mustStatus(t, "start promoting the preview's data", code, 202, raw)
	var job map[string]any
	for deadline := time.Now().Add(3 * time.Minute); ; time.Sleep(time.Second) {
		code, js, raw = l.api("GET", "/api/promote-data?app=hello", nil, refMain)
		mustStatus(t, "promote status", code, 200, raw)
		if job, _ = js["job"].(map[string]any); job["state"] != "running" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("promote-data still running: %v", job)
		}
	}
	if job["state"] != "done" {
		t.Fatalf("promote-data: %v", job)
	}
	backup, _ := job["backup"].(string)
	if b, _ := os.ReadFile(filepath.Join(backup, "secrets", "api_key")); string(b) != "sk-prod" {
		t.Errorf("production's old data was not kept in %q", backup)
	}
	var dir, tok syscall.Stat_t
	if err := syscall.Stat(prod, &dir); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Stat(filepath.Join(prod, "secrets", "token"), &tok); err != nil {
		t.Fatalf("the preview's secrets did not reach production: %v", err)
	}
	if dir.Uid == 0 || tok.Uid != dir.Uid || tok.Mode&0o777 != 0o600 {
		t.Errorf("production's token: uid %d mode %o, its folder's uid %d", tok.Uid, tok.Mode&0o777, dir.Uid)
	}
	prodPass := l.pass("hello", "prod", refMain)
	if code, body := l.page("GET", "hello."+itBase, "/", prodPass, nil); code != 200 || !strings.Contains(body, "from the integration test") {
		t.Fatalf("production after the promote: %d %s", code, body)
	}
	code, _, raw = l.api("DELETE", "/api/instances?app=hello&env=itest", nil, "refs/heads/itest")
	mustStatus(t, "delete the preview", code, 200, raw)
	if _, err := os.Stat(preview); !os.IsNotExist(err) {
		t.Error("the preview's data is still there")
	}
}

// After a reboot transient units are gone; labd starts the apps again.
func (l *itLab) reboot(t *testing.T) {
	sh(t, "systemctl", "stop", "labd")
	sh(t, "systemctl", "stop", unitName("hello--prod"), unitName("probe--prod"))
	sh(t, "systemctl", "start", "labd")
	l.waitVersion("itest01", 90*time.Second)
	pass := l.pass("hello", "prod", refMain)
	deadline := time.Now().Add(60 * time.Second)
	for {
		code, body := l.page("GET", "hello."+itBase, "/", pass, nil)
		if code == 200 && strings.Contains(body, "from the integration test") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("hello did not come back: %d %s", code, body)
		}
		time.Sleep(time.Second)
	}
	code, _, raw := l.api("GET", "/api/logs?app=hello&env=prod&lines=20", nil, refMain)
	mustStatus(t, "logs", code, 200, raw)
}

// A new labd installs itself and restarts; one that can't start is replaced
// by the previous one within about a minute, without anyone on the server.
func (l *itLab) platformUpdate(t *testing.T) {
	dir := t.TempDir()
	next := filepath.Join(dir, "labd")
	buildLabd(t, "itest02", next)
	data, _ := os.ReadFile(next)
	code, _, raw := l.api("POST", "/api/platform", data, refMain)
	mustStatus(t, "platform update", code, 200, raw)
	l.waitVersion("itest02", 90*time.Second)
	if _, err := os.Stat(itBinary + ".prev"); err != nil {
		t.Fatalf("the previous labd was not kept: %v", err)
	}

	// Passes labd's own checks, then fails at every start.
	broken := []byte("#!/bin/sh\ncase \"$1\" in -version) echo 'labd broken'; exit 0;; -selftest) echo 'selftest ok'; exit 0;; esac\necho 'cannot start' >&2\nexit 1\n")
	code, _, raw = l.api("POST", "/api/platform", broken, refMain)
	mustStatus(t, "installing a broken labd", code, 200, raw)
	l.waitDown(30 * time.Second)
	l.waitVersion("itest02", 3*time.Minute)
	if out := sh(t, "journalctl", "--no-pager", "-u", "labd-fallback"); !strings.Contains(out, "labd") {
		t.Logf("fallback journal: %s", out)
	}
	// Still deploying after all that.
	l.deploy("hello", "prod", refMain, "../../dist/hello.tar.gz")
}

// HTTPS through nginx at 127.0.0.1, whatever name a URL has: the names below
// itDomain exist only here. Certificates are labd's self-made ones (config
// "certs": "selfsigned"), so the check is that each host gets its own.
const itDomain = "lab.test"

func (l *itLab) https() *http.Client {
	dialer := &net.Dialer{Timeout: 10 * time.Second}
	return &http.Client{
		Timeout:       60 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				_, port, _ := net.SplitHostPort(addr)
				return dialer.DialContext(ctx, network, "127.0.0.1:"+port)
			},
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		},
	}
}

// fetch sends a request over HTTPS (or plain HTTP for http:// URLs) as a
// browser would, with Cookie header cookie.
func (l *itLab) fetch(method, rawURL, cookie string, form url.Values) (*http.Response, string) {
	l.t.Helper()
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req, _ := http.NewRequest(method, rawURL, body)
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Origin", "https://"+req.URL.Host)
		req.Header.Set("Sec-Fetch-Site", "same-origin")
	}
	if cookie != "" {
		req.Header.Set("Cookie", cookie)
	}
	resp, err := l.https().Do(req)
	if err != nil {
		l.t.Fatalf("%s %s: %v", method, rawURL, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.TLS != nil {
		if err := resp.TLS.PeerCertificates[0].VerifyHostname(req.URL.Hostname()); err != nil {
			l.t.Fatalf("%s: wrong certificate: %v", rawURL, err)
		}
	}
	return resp, string(b)
}

func (l *itLab) domainMode(t *testing.T) {
	code, js, raw := l.api("POST", "/api/settings", []byte(`{"domain":"`+itDomain+`","public_ip":"127.0.0.1","allow_ips":["192.0.2.1"]}`), refMain)
	mustStatus(t, "domain settings", code, 200, raw)
	if js["mode"] != "domain" {
		t.Fatalf("settings: %s", raw)
	}
	hello, probe := "https://hello."+itDomain, "https://probe."+itDomain
	// Each app's host switches to HTTPS once it has its certificate.
	deadline := time.Now().Add(2 * time.Minute)
	for _, u := range []string{hello, probe, "https://" + itDomain} {
		for {
			resp, err := l.https().Get(u + "/healthz")
			if err == nil {
				resp.Body.Close()
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("%s has no HTTPS: %v", u, err)
			}
			time.Sleep(500 * time.Millisecond)
		}
	}
	// Names labd doesn't know are refused before TLS.
	if resp, err := l.https().Get("https://nope." + itDomain + "/"); err == nil {
		resp.Body.Close()
		t.Fatal("an unknown name got an HTTPS answer")
	}
	// Plain HTTP only redirects.
	if code, _ := l.page("GET", "hello."+itDomain, "/notes?a=1", "", nil); code != 301 {
		t.Fatalf("plain HTTP: %d", code)
	}
	// The gate sends a visitor to the lab's own sign-in page and back.
	resp, _ := l.fetch("GET", hello+"/notes?a=1&b=2", "", nil)
	want := "https://" + itDomain + "/_lab/login?next=" + url.QueryEscape(hello+"/notes?a=1&b=2")
	if resp.StatusCode != 302 || resp.Header.Get("Location") != want {
		t.Fatalf("gate: %d to %q, want %q", resp.StatusCode, resp.Header.Get("Location"), want)
	}
	if !strings.Contains(resp.Header.Get("Strict-Transport-Security"), "includeSubDomains") {
		t.Fatalf("no HSTS: %v", resp.Header)
	}
	for _, path := range []string{"/_lab/login", "/_lab/auth", "/_lab/setup"} {
		if resp, _ := l.fetch("GET", hello+path, "", nil); resp.StatusCode != 404 {
			t.Fatalf("%s on an app's host: %d", path, resp.StatusCode)
		}
	}
	resp, body := l.fetch("GET", "https://"+itDomain+"/_lab/login", "", nil)
	if resp.StatusCode != 200 || !strings.Contains(body, "No password yet") || resp.Header.Get("X-Frame-Options") != "DENY" {
		t.Fatalf("sign-in page before setup: %d %v %s", resp.StatusCode, resp.Header, body)
	}

	// Setup with a one-time code, sent to labd only as its SHA-256.
	setup := "QZ7R-M2KD-8HXT-4WNC"
	norm, _ := normalizeSetupCode(setup)
	sum := sha256.Sum256([]byte(norm))
	code, _, raw = l.api("POST", "/api/setup-code", []byte(`{"code_sha256":"`+hex.EncodeToString(sum[:])+`"}`), refMain)
	mustStatus(t, "setup code", code, 200, raw)
	if resp, body = l.fetch("GET", "https://"+itDomain+"/_lab/setup?code="+setup, "", nil); resp.StatusCode != 200 || strings.Contains(body, "doesn") {
		t.Fatalf("setup page: %d %s", resp.StatusCode, body)
	}
	pw := "integration test password"
	resp, _ = l.fetch("POST", "https://"+itDomain+"/_lab/setup", "", url.Values{"code": {setup}, "password": {pw}, "password2": {pw}})
	if resp.StatusCode != 303 {
		t.Fatalf("setup: %d", resp.StatusCode)
	}
	var pass *http.Cookie
	for _, c := range resp.Cookies() {
		if c.Name == "lab_session" {
			pass = c
		}
	}
	if pass == nil || pass.Domain != itDomain || !pass.Secure || !pass.HttpOnly {
		t.Fatalf("pass: %+v", pass)
	}
	cookie := "lab_session=" + pass.Value

	// The pass opens every app on the lab's domain.
	if resp, body = l.fetch("GET", hello+"/", cookie, nil); resp.StatusCode != 200 || !strings.Contains(body, "from the integration test") {
		t.Fatalf("hello with the pass: %d %s", resp.StatusCode, body)
	}
	// It never reaches an app, even when a page planted a second one in
	// front of it; three are refused.
	if resp, body = l.fetch("GET", probe+"/cookies", "lab_session=planted; a=1; "+cookie, nil); resp.StatusCode != 200 || strings.Contains(body, "lab_session") || !strings.Contains(body, "a=1") {
		t.Fatalf("cookies reaching the app: %d %q", resp.StatusCode, body)
	}
	if resp, _ = l.fetch("GET", probe+"/cookies", "lab_session=x; lab_session=y; "+cookie, nil); resp.StatusCode != 400 {
		t.Fatalf("three passes: %d", resp.StatusCode)
	}
	if resp, body = l.fetch("GET", "https://"+itDomain+"/", cookie, nil); resp.StatusCode != 200 || !strings.Contains(body, hello) {
		t.Fatalf("the list of apps: %d %s", resp.StatusCode, body)
	}

	// Signing out and in again.
	if resp, _ = l.fetch("POST", "https://"+itDomain+"/_lab/logout", cookie, url.Values{}); resp.StatusCode != 303 {
		t.Fatalf("sign out: %d", resp.StatusCode)
	}
	if resp, _ = l.fetch("POST", "https://"+itDomain+"/_lab/login", "", url.Values{"password": {"not the password"}}); resp.StatusCode != 303 || !strings.Contains(resp.Header.Get("Location"), "e=wrong") {
		t.Fatalf("wrong password: %d", resp.StatusCode)
	}
	resp, _ = l.fetch("POST", "https://"+itDomain+"/_lab/login", "", url.Values{"password": {pw}, "next": {probe + "/"}})
	if resp.StatusCode != 303 || resp.Header.Get("Location") != probe+"/" {
		t.Fatalf("sign in: %d %v", resp.StatusCode, resp.Header)
	}

	// The pipeline's passes come over HTTPS only.
	code, _, raw = l.api("POST", "/api/session?app=hello&env=prod", nil, refMain)
	mustStatus(t, "session over plain HTTP", code, 403, raw)
	uri := "/api/session?app=hello&env=prod"
	req, _ := http.NewRequest("POST", "https://"+itDomain+uri, nil)
	req.Header.Set("Authorization", "Bearer "+l.ti.token(t, baseClaims(refMain, apiAudience("POST", uri, sha256Hex(nil)))))
	r2, err := l.https().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(r2.Body)
	r2.Body.Close()
	var sess map[string]any
	json.Unmarshal(b, &sess)
	if r2.StatusCode != 200 || sess["cookie"] == nil {
		t.Fatalf("session over HTTPS: %d %s", r2.StatusCode, b)
	}
	if resp, _ = l.fetch("GET", hello+"/", "lab_session="+sess["cookie"].(string), nil); resp.StatusCode != 200 {
		t.Fatalf("hello with the pipeline's pass: %d", resp.StatusCode)
	}
	if resp, _ = l.fetch("GET", probe+"/", "lab_session="+sess["cookie"].(string), nil); resp.StatusCode != 302 {
		t.Fatalf("probe with hello's pipeline pass: %d", resp.StatusCode)
	}

	code, js, raw = l.api("GET", "/api/status", nil, refMain)
	mustStatus(t, "status", code, 200, raw)
	if js["certificate"] != true || strings.Contains(raw, `"certificate":false`) || !strings.Contains(raw, `"password_set":true`) {
		t.Fatalf("status: %s", raw)
	}
}
