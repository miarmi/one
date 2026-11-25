package main

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeHost records commands instead of running them.
type fakeHost struct {
	mu      sync.Mutex
	cmds    []string
	answers map[string]string // command prefix -> output
	fail    map[string]bool   // command prefix -> exit with an error
}

func (f *fakeHost) run(_ context.Context, _ time.Duration, name string, args ...string) (string, error) {
	cmd := strings.TrimSpace(name + " " + strings.Join(args, " "))
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cmds = append(f.cmds, cmd)
	out, best := "", -1
	for prefix, a := range f.answers {
		if strings.HasPrefix(cmd, prefix) && len(prefix) > best {
			out, best = a, len(prefix)
		}
	}
	for prefix := range f.fail {
		if strings.HasPrefix(cmd, prefix) {
			return out, errors.New("exit status 3")
		}
	}
	return out, nil
}

func (f *fakeHost) ran(prefix string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.cmds {
		if strings.HasPrefix(c, prefix) {
			return true
		}
	}
	return false
}

func testHostEnv(t *testing.T, f *fakeHost) *hostEnv {
	dir := t.TempDir()
	return &hostEnv{
		sysctlConf:   filepath.Join(dir, "sysctl.d", "90-lab-hardening.conf"),
		modprobeConf: filepath.Join(dir, "modprobe.d", "00-lab-hardening.conf"),
		procSys:      filepath.Join(dir, "proc", "sys"),
		procModules:  filepath.Join(dir, "proc", "modules"),
		unitDir:      filepath.Join(dir, "systemd"),
		journaldConf: filepath.Join(dir, "journald.conf.d", "50-lab.conf"),
		shadow:       filepath.Join(dir, "shadow"),
		binary:       "/usr/local/bin/labd",
		firewallFile: "/etc/lab/apps-firewall.nft",
		run:          f.run,
	}
}

func TestHardenHost(t *testing.T) {
	f := &fakeHost{
		answers: map[string]string{"systemctl list-unit-files --no-legend ssh.service": "ssh.service enabled enabled", "systemctl is-active": "inactive"},
		fail:    map[string]bool{"systemctl is-active": true},
	}
	env := testHostEnv(t, f)
	// A kernel that has every setting except two optional ones.
	missing := map[string]bool{"kernel.unprivileged_userns_clone": true, "dev.tty.ldisc_autoload": true}
	for _, s := range hostSysctls {
		if missing[s.key] {
			continue
		}
		p := filepath.Join(env.procSys, strings.ReplaceAll(s.key, ".", "/"))
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.WriteFile(p, []byte("0\n"), 0o644)
	}
	os.WriteFile(env.procModules, []byte("tls 155648 0 - Live 0x0\nsctp 471040 2 - Live 0x0\next4 1105920 1 - Live 0x0\n"), 0o644)
	// root as Hetzner ships it: the password must be changed at the next login.
	os.WriteFile(env.shadow, []byte("root:$y$j9T$abc:0:0:99999:7:::\ndaemon:*:20000:0:99999:7:::\n"), 0o600)

	rep := hardenHost(t.Context(), env)
	for _, s := range hostSysctls {
		b, err := os.ReadFile(filepath.Join(env.procSys, strings.ReplaceAll(s.key, ".", "/")))
		if missing[s.key] {
			continue
		}
		if err != nil || strings.TrimSpace(string(b)) != s.value {
			t.Errorf("%s = %q, want %s", s.key, b, s.value)
		}
	}
	sys := rep["sysctl"].(map[string]any)
	if len(sys["failed"].([]string)) != 0 || sys["applied"] != len(hostSysctls)-2 || len(sys["not_on_this_kernel"].([]string)) != 2 {
		t.Fatalf("sysctl report %v", sys)
	}
	conf, _ := os.ReadFile(env.sysctlConf)
	for _, want := range []string{"kernel.io_uring_disabled = 2", "-kernel.unprivileged_userns_clone = 0", "kernel.kptr_restrict = 2", "kernel.unprivileged_bpf_disabled = 1"} {
		if !strings.Contains(string(conf), want) {
			t.Errorf("sysctl file lacks %q", want)
		}
	}
	mods, _ := os.ReadFile(env.modprobeConf)
	for _, m := range []string{"ksmbd", "tls", "af_alg", "ebtables", "nvmet", "vmwgfx", "sctp"} {
		// modprobe ignores install commands of modules with soft dependencies
		// unless the first softdep line for them is empty.
		if !strings.Contains(string(mods), "softdep "+m+" pre: post:\ninstall "+m+" /bin/false\n") {
			t.Errorf("module %s not blocked", m)
		}
	}
	// Unused blocked modules are unloaded; used ones and others stay.
	if !f.ran("modprobe -r tls") || f.ran("modprobe -r sctp") || f.ran("modprobe -r ext4") {
		t.Errorf("module unloading: %v", f.cmds)
	}
	if rep["ssh"] != "off" || !f.ran("systemctl disable --now ssh.service") || !f.ran("systemctl mask ssh.service") || f.ran("systemctl disable --now ssh.socket") {
		t.Errorf("ssh: %v %v", rep["ssh"], f.cmds)
	}
	if nft, _ := os.ReadFile(filepath.Join(env.unitDir, "nftables.service.d", "50-lab-apps.conf")); !strings.Contains(string(nft), "ExecStartPost=-/usr/sbin/nft -f /etc/lab/apps-firewall.nft") ||
		!strings.Contains(string(nft), "ExecReload=-/usr/sbin/nft -f /etc/lab/apps-firewall.nft") {
		t.Errorf("nftables drop-in: %s", nft)
	}
	guard, _ := os.ReadFile(filepath.Join(env.unitDir, "labd.service.d", "50-lab-guard.conf"))
	fallback, _ := os.ReadFile(filepath.Join(env.unitDir, "labd-fallback.service"))
	if !strings.Contains(string(guard), "OnFailure=labd-fallback.service") || !strings.Contains(string(fallback), "/usr/local/bin/labd.prev") {
		t.Errorf("guard units:\n%s\n%s", guard, fallback)
	}
	if rep["labd_guard"] != "installed" || !f.ran("systemctl daemon-reload") {
		t.Errorf("guard: %v %v", rep["labd_guard"], f.cmds)
	}
	// journald keeps the logs 30 days; it is restarted once, for the new file.
	jc, _ := os.ReadFile(env.journaldConf)
	if rep["journal"] != "30day" || !strings.Contains(string(jc), "MaxRetentionSec=30day") || !strings.Contains(string(jc), "SystemMaxUse=2G") ||
		!strings.Contains(string(jc), "Storage=persistent") || !f.ran("systemctl restart systemd-journald") {
		t.Errorf("journal: %v %s %v", rep["journal"], jc, f.cmds)
	}
	today := time.Now().UTC().Format("2006-01-02")
	if rep["root_password"] != "cleared a forced password change" || !f.ran("chage -d "+today+" root") {
		t.Errorf("root password: %v %v", rep["root_password"], f.cmds)
	}
	// Applying again changes nothing and reloads nothing.
	f.cmds = nil
	os.WriteFile(env.shadow, []byte("root:$y$j9T$abc:"+strconv.Itoa(int(time.Now().Unix()/86400))+":0:99999:7:::\n"), 0o600)
	rep = hardenHost(t.Context(), env)
	if rep["root_password"] != "ok" || f.ran("chage") {
		t.Errorf("root password touched although it is fine: %v %v", rep["root_password"], f.cmds)
	}
	if f.ran("systemctl daemon-reload") {
		t.Error("systemd reloaded although nothing changed")
	}
	if f.ran("systemctl restart systemd-journald") {
		t.Error("journald restarted although its file did not change")
	}
}

func TestRootPasswordAging(t *testing.T) {
	today := int(time.Now().Unix() / 86400)
	date := time.Now().UTC().Format("2006-01-02")
	cases := []struct{ shadow, want, cmd string }{
		{"root:!:" + strconv.Itoa(today-10) + ":0:99999:7:::\n", "ok", ""},
		{"root:!::0:99999:7:::\n", "ok", ""},                          // aging off
		{"root:!:" + strconv.Itoa(today-10) + ":0::7:::\n", "ok", ""}, // no maximum
		{"root:!:0:0:99999:7:::\n", "cleared a forced password change", "chage -d " + date + " root"},
		{"root:!:" + strconv.Itoa(today-100) + ":0:90:7:::\n", "lifted the password's maximum age", "chage -d " + date + " -M 99999 root"},
		{"daemon:*:20000:0:99999:7:::\n", "failed: no root entry", ""},
	}
	for _, c := range cases {
		f := &fakeHost{}
		env := testHostEnv(t, f)
		os.WriteFile(env.shadow, []byte(c.shadow), 0o600)
		got := rootPasswordAging(t.Context(), env)
		if got != c.want || (c.cmd == "" && len(f.cmds) != 0) || (c.cmd != "" && !slices.Contains(f.cmds, c.cmd)) {
			t.Errorf("%q: got %q, commands %v; want %q, %q", c.shadow, got, f.cmds, c.want, c.cmd)
		}
	}
	f := &fakeHost{fail: map[string]bool{"chage": true}}
	env := testHostEnv(t, f)
	os.WriteFile(env.shadow, []byte("root:!:0:0:99999:7:::\n"), 0o600)
	if got := rootPasswordAging(t.Context(), env); !strings.HasPrefix(got, "failed: ") {
		t.Errorf("chage failure not reported: %q", got)
	}
	env.shadow = filepath.Join(t.TempDir(), "missing")
	if got := rootPasswordAging(t.Context(), env); !strings.HasPrefix(got, "failed: ") {
		t.Errorf("missing shadow not reported: %q", got)
	}
}

func TestJournalDiskUsage(t *testing.T) {
	if got := journalDiskUsage("Archived and active journals take up 1.2G in the file system."); got != "1.2G" {
		t.Errorf("got %q", got)
	}
	if got := journalDiskUsage("something else\n"); got != "something else" {
		t.Errorf("got %q", got)
	}
}

func TestParseAptSimulation(t *testing.T) {
	out := `NOTE: This is only a simulation!
Reading package lists...
Inst libssl3t64 [3.5.1-1] (3.5.1-1+deb13u1 Debian-Security:13/stable-security [amd64])
Inst openssl [3.5.1-1] (3.5.1-1+deb13u1 Debian-Security:13/stable-security [amd64])
Inst tzdata [2025b-4] (2025b-4+deb13u1 Debian:13.2/stable [all])
Inst linux-image-6.12.57+deb13-amd64 (6.12.57-1 Debian:13.2/stable, Debian-Security:13/stable-security [amd64])
Conf libssl3t64 (3.5.1-1+deb13u1 Debian-Security:13/stable-security [amd64])`
	got := parseAptSimulation(out)
	if len(got) != 4 {
		t.Fatalf("%+v", got)
	}
	want := []pendingUpdate{
		{"libssl3t64", "3.5.1-1", "3.5.1-1+deb13u1", true},
		{"openssl", "3.5.1-1", "3.5.1-1+deb13u1", true},
		{"tzdata", "2025b-4", "2025b-4+deb13u1", false},
		{"linux-image-6.12.57+deb13-amd64", "", "6.12.57-1", true},
	}
	if !slices.Equal(got, want) {
		t.Fatalf("got %+v", got)
	}
}

// The host endpoints are for main only; reboot can wait for a reason.
func TestHostEndpoints(t *testing.T) {
	h := newHarness(t)
	f := &fakeHost{answers: map[string]string{
		"apt-get -s": "Inst openssl [3.5.1-1] (3.5.1-1+deb13u1 Debian-Security:13/stable-security [amd64])",
		"systemctl show lab-host-upgrade.service": "ActiveState=inactive\nResult=success",
	}}
	h.srv.host = testHostEnv(t, f)
	rebooted := make(chan struct{}, 1)
	h.srv.reboot = func() { rebooted <- struct{}{} }

	for _, c := range []struct{ method, uri string }{{"GET", "/api/host"}, {"POST", "/api/host/upgrade"}, {"POST", "/api/host/reboot"}} {
		code, _, raw := h.call(c.method, c.uri, nil, "refs/heads/idea")
		mustStatus(t, c.uri+" from a branch", code, http.StatusForbidden, raw)
	}
	code, js, raw := h.call("GET", "/api/host", nil, refMain)
	mustStatus(t, "host", code, 200, raw)
	if js["kernel"] == "" || js["updates"].(map[string]any)["security"] != float64(1) || js["upgrade_job"].(map[string]any)["activestate"] != "inactive" {
		t.Fatalf("host: %s", raw)
	}

	code, js, raw = h.call("POST", "/api/host/upgrade", nil, refMain)
	mustStatus(t, "upgrade", code, 200, raw)
	if !f.ran("systemd-run --unit=lab-host-upgrade.service") {
		t.Fatalf("upgrade not started: %v", f.cmds)
	}
	f.answers["systemctl is-active lab-host-upgrade.service"] = "active"
	code, _, raw = h.call("POST", "/api/host/upgrade", nil, refMain)
	mustStatus(t, "second upgrade at once", code, http.StatusConflict, raw)

	if !rebootRequired() {
		code, js, raw = h.call("POST", "/api/host/reboot?if_required=1", nil, refMain)
		mustStatus(t, "reboot if required", code, 200, raw)
		if js["rebooting"] != false {
			t.Fatalf("rebooting without a reason: %s", raw)
		}
	}
	code, js, raw = h.call("POST", "/api/host/reboot", nil, refMain)
	mustStatus(t, "reboot", code, 200, raw)
	select {
	case <-rebooted:
	case <-time.After(5 * time.Second):
		t.Fatal("no reboot")
	}
}
