package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// Apps may bind only their own port, when systemd can enforce it, and run in
// the hardened sandbox unless it failed for them.
func TestUnitProperties(t *testing.T) {
	inst := "/srv/lab/instances/hello--prod"
	spec := UnitSpec{ID: "hello--prod", Memory: "256M", Env: map[string]string{"PORT": "20000"}, Instance: inst, State: "/srv/lab",
		Data: []string{"/var/lib/private/lab/hello--prod", "/var/lib/lab/hello--prod"}}
	full := unitProperties(spec, sandboxCaps{bind: true, hardened: true})
	for _, want := range []string{
		"SocketBindAllow=tcp:20000", "SocketBindDeny=any", "DynamicUser=yes", "MemoryMax=256M",
		"SystemCallFilter=@system-service", "SystemCallArchitectures=native", "SystemCallErrorNumber=EPERM",
		"CapabilityBoundingSet=", "NoNewPrivileges=yes", "ProtectProc=invisible", "ProcSubset=pid",
		"ProtectKernelLogs=yes", "MemoryDenyWriteExecute=yes", "RestrictAddressFamilies=AF_INET AF_INET6",
		"TemporaryFileSystem=/srv/lab:ro", "BindReadOnlyPaths=" + inst, "ExecPaths=" + inst,
		"NoExecPaths=/ -/tmp -/var/tmp -/dev/shm -/var/lib/private/lab/hello--prod -/var/lib/lab/hello--prod",
		"UMask=0077", "PrivateIPC=yes", "CPUQuota=100%",
	} {
		if !slices.Contains(full, want) {
			t.Errorf("hardened sandbox lacks %s", want)
		}
	}
	var deny string
	for _, p := range full {
		if strings.HasPrefix(p, "SystemCallFilter=~") {
			deny = p
		}
	}
	// The system calls recent kernel attacks start from are not allowed.
	for _, g := range []string{"@aio", "@keyring", "@debug", "@privileged", "@mount", "@module", "@setuid", "@resources", "userfaultfd"} {
		if !slices.Contains(strings.Fields(strings.TrimPrefix(deny, "SystemCallFilter=~")), g) {
			t.Errorf("system call filter allows %s: %q", g, deny)
		}
	}
	if slices.Contains(full, "CPUWeight=20") {
		t.Error("production yields CPU like a preview")
	}
	preview := spec
	preview.Preview = true
	if !slices.Contains(unitProperties(preview, sandboxCaps{hardened: true}), "CPUWeight=20") {
		t.Error("a preview doesn't yield CPU to production")
	}

	// The basic sandbox is version 2's, exactly.
	basic := spec
	basic.Basic = true
	for _, props := range [][]string{unitProperties(basic, sandboxCaps{bind: true, hardened: true}), unitProperties(spec, sandboxCaps{bind: true})} {
		if !slices.Contains(props, "RestrictAddressFamilies=AF_INET AF_INET6 AF_UNIX") || !slices.Contains(props, "SocketBindDeny=any") {
			t.Errorf("basic sandbox changed: %v", props)
		}
		for _, p := range props {
			if strings.HasPrefix(p, "SystemCallFilter") || strings.HasPrefix(p, "ExecPaths") {
				t.Errorf("basic sandbox has %s", p)
			}
		}
	}
	for _, p := range unitProperties(spec, sandboxCaps{hardened: true}) {
		if strings.HasPrefix(p, "SocketBind") {
			t.Errorf("%s given to a systemd that doesn't know it", p)
		}
	}
}

// Apps can't open connections to the server itself or to private, link-local
// and metadata addresses; DNS to the resolvers comes first.
func TestAppFirewall(t *testing.T) {
	text := appFirewall(
		[]netip.Addr{netip.MustParseAddr("185.12.64.1"), netip.MustParseAddr("2a01:4ff:ff00::add:1")},
		[]netip.Addr{netip.MustParseAddr("203.0.113.10"), netip.MustParseAddr("2a01:4f8:c012:abcd::1")})
	lines := strings.Split(text, "\n")
	if lines[0] != "table inet lab_apps" || lines[1] != "delete table inet lab_apps" {
		t.Fatal("the firewall must replace itself when applied again")
	}
	firstDrop, lastAccept := -1, -1
	for i, l := range lines {
		if !strings.Contains(l, "addr ") {
			continue
		}
		if !strings.HasPrefix(l, "\t\tmeta skuid 61184-65519 ") {
			t.Errorf("rule not limited to the apps' dynamic users: %q", l)
		}
		if strings.HasSuffix(l, " drop") && firstDrop < 0 {
			firstDrop = i
		}
		if strings.HasSuffix(l, " accept") {
			lastAccept = i
		}
	}
	if firstDrop < 0 || lastAccept > firstDrop {
		t.Fatalf("an accept rule comes after a drop:\n%s", text)
	}
	for _, want := range []string{
		"ip daddr 185.12.64.1 udp dport 53 accept", "ip6 daddr 2a01:4ff:ff00::add:1 tcp dport 53 accept",
		"ip daddr 127.0.0.0/8 ct state new drop", "ip6 daddr ::1 ct state new drop",
		"ip saddr 127.0.0.0/8 tcp sport != 20000-29999 drop", "ip6 saddr ::1 tcp sport != 20000-29999 drop",
		"ip daddr { 203.0.113.10 } ct state new drop", "ip6 daddr { 2a01:4f8:c012:abcd::1 } ct state new drop",
		"169.254.0.0/16", "10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "100.64.0.0/10", "fe80::/10", "fc00::/7",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("firewall lacks %q:\n%s", want, text)
		}
	}
	// nft itself accepts the text, where this machine lets us ask.
	if _, err := exec.LookPath("nft"); err == nil {
		cmd := exec.Command("nft", "-c", "-f", "-")
		cmd.Stdin = strings.NewReader(text)
		if out, err := cmd.CombinedOutput(); err != nil {
			if strings.Contains(string(out), "Operation not permitted") || strings.Contains(string(out), "Permission denied") {
				t.Logf("nft can't check here: %s", out)
			} else {
				t.Fatalf("nft rejects the firewall: %v\n%s\n%s", err, out, text)
			}
		}
	}
}

// Blocked clients are dropped before the system's rules, in a table that
// replaces itself; an entry that is not an address is skipped, not applied.
func TestBlockedFirewall(t *testing.T) {
	for _, c := range blockedClients {
		if a, err := netip.ParseAddr(c); err != nil || a.Zone() != "" {
			t.Errorf("blockedClients entry %q is not a plain address", c)
		}
	}
	text := blockedFirewall([]string{"45.138.12.16", "2001:db8::7", "not-an-address", "10.0.0.0/8", "198.51.100.9"})
	lines := strings.Split(text, "\n")
	if lines[0] != "table inet lab_blocked" || lines[1] != "delete table inet lab_blocked" {
		t.Fatal("the blocked table must replace itself when applied again")
	}
	for _, want := range []string{
		"type filter hook input priority filter - 10; policy accept;",
		"\t\tip saddr { 45.138.12.16, 198.51.100.9 } drop\n",
		"\t\tip6 saddr { 2001:db8::7 } drop\n",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("blocked table lacks %q:\n%s", want, text)
		}
	}
	for _, bad := range []string{"not-an-address", "10.0.0.0/8"} {
		if strings.Contains(text, bad) {
			t.Errorf("blocked table applies %q:\n%s", bad, text)
		}
	}
	if empty := blockedFirewall(nil); strings.Contains(empty, "saddr") || !strings.Contains(empty, "chain input") {
		t.Errorf("an empty list must still replace the table:\n%s", empty)
	}
	// nft itself accepts the text, where this machine lets us ask.
	if _, err := exec.LookPath("nft"); err == nil {
		cmd := exec.Command("nft", "-c", "-f", "-")
		cmd.Stdin = strings.NewReader(text)
		if out, err := cmd.CombinedOutput(); err != nil {
			if strings.Contains(string(out), "Operation not permitted") || strings.Contains(string(out), "Permission denied") {
				t.Logf("nft can't check here: %s", out)
			} else {
				t.Fatalf("nft rejects the blocked table: %v\n%s\n%s", err, out, text)
			}
		}
	}
}

func TestResolversAndLocalAddrs(t *testing.T) {
	conf := filepath.Join(t.TempDir(), "resolv.conf")
	os.WriteFile(conf, []byte("# Hetzner\nnameserver 185.12.64.2\nnameserver 127.0.0.53\nnameserver 185.12.64.2\nnameserver fe80::1%eth0\noptions edns0\n"), 0o644)
	got := resolvers(conf)
	if len(got) != 1 || got[0].String() != "185.12.64.2" {
		t.Fatalf("resolvers %v", got)
	}
	locals := localAddrs("203.0.113.5")
	if !slices.Contains(locals, netip.MustParseAddr("203.0.113.5")) {
		t.Fatalf("public IP missing from %v", locals)
	}
	for _, a := range locals {
		if a.IsLoopback() || a.IsLinkLocalUnicast() {
			t.Fatalf("%s listed", a)
		}
	}
}

// After the sandbox changes, running apps are restarted into it once.
func TestRefreshSandbox(t *testing.T) {
	h := newHarness(t)
	h.call("POST", "/api/settings", []byte(`{"domain":"","public_ip":"127.0.0.1"}`), refMain)
	code, _, raw := h.call("POST", "/api/deploy?app=hello&env=prod&commit=abc1234", bundleOf(t, testApps.good, "hello", ""), refMain)
	mustStatus(t, "deploy", code, 200, raw)
	pid := func() int {
		h.runner.mu.Lock()
		defer h.runner.mu.Unlock()
		return h.runner.procs["hello--prod"].cmd.Process.Pid
	}
	before := pid()
	stamp := filepath.Join(h.cfg.StateDir, "sandbox")
	h.srv.mgr.RefreshSandbox(t.Context(), stamp)
	after := pid()
	if after == before {
		t.Fatal("not restarted")
	}
	if b, _ := os.ReadFile(stamp); strings.TrimSpace(string(b)) != sandboxVersion {
		t.Fatalf("stamp %q", b)
	}
	if c, _ := h.appGet("hello", "prod", "/healthz"); c != 200 {
		t.Fatal("not healthy after the restart")
	}
	h.srv.mgr.RefreshSandbox(t.Context(), stamp)
	if pid() != after {
		t.Fatal("restarted again with the same sandbox")
	}
}

// An upload that says it is too big is refused before it is read, and only a
// few large uploads may arrive at once (security review 2026-10-17, M6).
func TestUploadLimits(t *testing.T) {
	h := newHarness(t)
	h.call("POST", "/api/settings", []byte(`{"domain":"","public_ip":"127.0.0.1"}`), refMain)
	uri := "/api/deploy?app=hello&env=prod&commit=abc1234"
	body := bundleOf(t, testApps.good, "hello", "")
	tok := h.ti.token(t, baseClaims(refMain, apiAudience("POST", uri, sha256Hex(body))))
	req, _ := http.NewRequest("POST", h.ts.URL+uri, bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+tok)
	req.ContentLength = maxUpload + 1
	req.Body = io.NopCloser(io.MultiReader(bytes.NewReader(body), bytes.NewReader(make([]byte, maxUpload+1-len(body)))))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("declared too big: %d", resp.StatusCode)
	}
	for range cap(h.srv.uploads) {
		h.srv.uploads <- struct{}{}
	}
	code, _, raw := h.call("POST", uri, body, refMain)
	mustStatus(t, "too many uploads", code, http.StatusTooManyRequests, raw)
	for range cap(h.srv.uploads) {
		<-h.srv.uploads
	}
	code, _, raw = h.call("POST", uri, body, refMain)
	mustStatus(t, "upload after the others", code, 200, raw)
}

// /api/status says what the sandbox enforces, so a platform update can be
// checked from the pipeline.
func TestSandboxStatus(t *testing.T) {
	setFirewallState("applied")
	sandboxState.Lock()
	sandboxState.bindRules = true
	sandboxState.hardened = true
	sandboxState.Unlock()
	if got := sandboxStatus(); got["bind_rules"] != true || got["firewall"] != "applied" || got["hardened"] != true || got["version"] != sandboxVersion {
		t.Fatalf("status %v", got)
	}
	if s := statusOf(Instance{App: "hello", Env: "prod"}, Settings{}, true)["sandbox"]; s != "hardened" {
		t.Fatalf("instance sandbox %v", s)
	}
	if s := statusOf(Instance{App: "hello", Env: "prod", Sandbox: sandboxBasic}, Settings{}, true)["sandbox"]; s != "basic" {
		t.Fatalf("instance sandbox %v", s)
	}
	if s := statusOf(Instance{App: "hello", Env: "prod"}, Settings{}, false)["sandbox"]; s != "basic" {
		t.Fatalf("instance sandbox without the hardened one %v", s)
	}
	h := newHarness(t)
	code, js, raw := h.call("GET", "/api/status", nil, refMain)
	mustStatus(t, "status", code, 200, raw)
	if _, ok := js["sandbox"]; ok {
		t.Error("the exec runner has no sandbox to report")
	}
}

// pickyRunner refuses to start chosen instances in the hardened sandbox, like
// an app that needs something the sandbox forbids.
type pickyRunner struct {
	*execRunner
	failHardened map[string]bool
}

func (p *pickyRunner) Start(ctx context.Context, spec UnitSpec) error {
	if !spec.Basic && p.failHardened[spec.ID] {
		_ = p.execRunner.Stop(ctx, spec.ID)
		return errors.New("refused in the hardened sandbox")
	}
	return p.execRunner.Start(ctx, spec)
}

// An app that fails in the hardened sandbox goes back to the basic one
// instead of staying down; its next deploy tries the hardened one again.
func TestRefreshSandboxFallsBack(t *testing.T) {
	h := newHarness(t)
	h.call("POST", "/api/settings", []byte(`{"domain":"","public_ip":"127.0.0.1"}`), refMain)
	for _, app := range []string{"hello", "other"} {
		code, _, raw := h.call("POST", "/api/deploy?app="+app+"&env=prod&commit=abc1234", bundleOf(t, testApps.good, app, ""), refMain)
		mustStatus(t, "deploy "+app, code, 200, raw)
	}
	picky := &pickyRunner{execRunner: h.runner, failHardened: map[string]bool{"hello--prod": true}}
	m := newManager(h.cfg, h.srv.store, picky, h.edge)
	m.healthTimeout = 5 * time.Second
	m.RefreshSandbox(t.Context(), filepath.Join(h.cfg.StateDir, "sandbox"))
	if inst, _ := h.srv.store.Get("hello--prod"); inst.Sandbox != sandboxBasic || inst.Status != "running" {
		t.Fatalf("hello: %+v", inst)
	}
	if inst, _ := h.srv.store.Get("other--prod"); inst.Sandbox != "" {
		t.Fatalf("other: %+v", inst)
	}
	if c, _ := h.appGet("hello", "prod", "/healthz"); c != 200 {
		t.Fatal("hello is down after falling back")
	}
	// Restarts keep it in the basic sandbox.
	h.runner.Stop(t.Context(), "hello--prod")
	m.Reconcile(t.Context())
	if c, _ := h.appGet("hello", "prod", "/healthz"); c != 200 {
		t.Fatal("hello not restarted in the basic sandbox")
	}
	// The next deploy starts in the hardened sandbox again.
	code, _, raw := h.call("POST", "/api/deploy?app=hello&env=prod&commit=abc1235", bundleOf(t, testApps.good, "hello", ""), refMain)
	mustStatus(t, "redeploy", code, 200, raw)
	if inst, _ := h.srv.store.Get("hello--prod"); inst.Sandbox != "" {
		t.Fatalf("after redeploy: %+v", inst)
	}
}

// A preview that fails in the hardened sandbox stays down rather than
// running in the basic one.
func TestRefreshSandboxStopsFailingPreview(t *testing.T) {
	h := newHarness(t)
	h.call("POST", "/api/settings", []byte(`{"domain":"","public_ip":"127.0.0.1"}`), refMain)
	code, _, raw := h.call("POST", "/api/deploy?app=hello&env=idea&commit=abc1234", bundleOf(t, testApps.good, "hello", ""), "refs/heads/idea")
	mustStatus(t, "preview deploy", code, 200, raw)
	picky := &pickyRunner{execRunner: h.runner, failHardened: map[string]bool{"hello--idea": true}}
	m := newManager(h.cfg, h.srv.store, picky, h.edge)
	m.healthTimeout = 3 * time.Second
	m.RefreshSandbox(t.Context(), filepath.Join(h.cfg.StateDir, "sandbox"))
	inst, _ := h.srv.store.Get("hello--idea")
	if inst.Status != "failed" || inst.Sandbox != "" || inst.Note == "" || h.runner.Active(t.Context(), "hello--idea") {
		t.Fatalf("preview: %+v", inst)
	}
}

// A rollback is remembered until the next deploy, so the pipeline doesn't
// ship the rolled-back version again with an unrelated change.
func TestRollbackIsRemembered(t *testing.T) {
	h := newHarness(t)
	h.call("POST", "/api/settings", []byte(`{"domain":"","public_ip":"127.0.0.1"}`), refMain)
	for _, c := range []string{"aaa1111", "bbb2222"} {
		code, _, raw := h.call("POST", "/api/deploy?app=hello&env=prod&commit="+c, bundleOf(t, testApps.good, "hello", ""), refMain)
		mustStatus(t, "deploy "+c, code, 200, raw)
	}
	code, _, raw := h.call("POST", "/api/rollback?app=hello&env=prod", nil, refMain)
	mustStatus(t, "rollback", code, 200, raw)
	_, js, _ := h.call("GET", "/api/status", nil, refMain)
	if i := js["instances"].([]any)[0].(map[string]any); i["rolled_back"] != true || i["commit"] != "aaa1111" {
		t.Fatalf("after rollback: %v", i)
	}
	code, _, raw = h.call("POST", "/api/deploy?app=hello&env=prod&commit=ccc3333", bundleOf(t, testApps.good, "hello", ""), refMain)
	mustStatus(t, "deploy after rollback", code, 200, raw)
	_, js, _ = h.call("GET", "/api/status", nil, refMain)
	if i := js["instances"].([]any)[0].(map[string]any); i["rolled_back"] != false {
		t.Fatalf("after deploy: %v", i)
	}
}

// With the disk nearly full, previews are stopped and no new one starts;
// production keeps running (security review M7).
func TestDiskNearlyFull(t *testing.T) {
	h := newHarness(t)
	h.call("POST", "/api/settings", []byte(`{"domain":"","public_ip":"127.0.0.1"}`), refMain)
	code, _, raw := h.call("POST", "/api/deploy?app=hello&env=prod&commit=abc1234", bundleOf(t, testApps.good, "hello", ""), refMain)
	mustStatus(t, "prod", code, 200, raw)
	code, _, raw = h.call("POST", "/api/deploy?app=hello&env=idea&commit=abc1234", bundleOf(t, testApps.good, "hello", ""), "refs/heads/idea")
	mustStatus(t, "preview", code, 200, raw)
	old := diskFree
	diskFree = func(string) (uint64, error) { return 100 << 20, nil }
	defer func() { diskFree = old }()
	h.srv.mgr.Reconcile(t.Context())
	if inst, _ := h.srv.store.Get("hello--idea"); inst.Status != "failed" || h.runner.Active(t.Context(), "hello--idea") {
		t.Fatalf("preview still running: %+v", inst)
	}
	if inst, _ := h.srv.store.Get("hello--prod"); inst.Status != "running" || !h.runner.Active(t.Context(), "hello--prod") {
		t.Fatalf("production stopped: %+v", inst)
	}
	code, _, raw = h.call("POST", "/api/deploy?app=hello&env=other&commit=abc1234", bundleOf(t, testApps.good, "hello", ""), "refs/heads/other")
	mustStatus(t, "new preview on a full disk", code, http.StatusConflict, raw)
}
