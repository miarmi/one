package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// The server itself (CONTRACT.md, "The server"). labd runs as root, so on a
// systemd server it keeps the kernel settings and module rules that shrink
// what a broken-into app could attack, keeps SSH off (the firewall blocks it
// anyway; the Hetzner console is the emergency door), and installs a guard
// that puts the previous labd back when a new one keeps failing to start.
// GET /api/host reports all of it together with the patch state.

type sysctlSetting struct {
	key, value string
	optional   bool // missing on some kernels: "-" in the file, not an error
}

// hostSysctls are kernel settings for a server that runs untrusted apps.
var hostSysctls = []sysctlSetting{
	{"kernel.kptr_restrict", "2", false},
	{"kernel.dmesg_restrict", "1", false},
	{"kernel.perf_event_paranoid", "3", false},
	{"kernel.unprivileged_bpf_disabled", "1", false},
	{"net.core.bpf_jit_harden", "2", true},
	{"kernel.kexec_load_disabled", "1", true},
	{"kernel.io_uring_disabled", "2", true},
	{"kernel.unprivileged_userns_clone", "0", true},
	{"kernel.yama.ptrace_scope", "2", true},
	{"dev.tty.ldisc_autoload", "0", true},
	{"vm.unprivileged_userfaultfd", "0", true},
	{"fs.protected_symlinks", "1", false},
	{"fs.protected_hardlinks", "1", false},
	{"fs.protected_fifos", "2", false},
	{"fs.protected_regular", "2", false},
	{"fs.suid_dumpable", "0", false},
	{"net.ipv4.conf.all.accept_redirects", "0", false},
	{"net.ipv4.conf.default.accept_redirects", "0", false},
	{"net.ipv6.conf.all.accept_redirects", "0", true},
	{"net.ipv6.conf.default.accept_redirects", "0", true},
	{"net.ipv4.conf.all.send_redirects", "0", false},
	{"net.ipv4.conf.default.send_redirects", "0", false},
	{"net.ipv4.conf.all.accept_source_route", "0", false},
	{"net.ipv6.conf.all.accept_source_route", "0", true},
	{"net.ipv4.tcp_syncookies", "1", false},
	{"net.ipv4.icmp_echo_ignore_broadcasts", "1", false},
}

// blockedModules are kernel modules this virtual web server never needs.
// Several had serious bugs that were exploited or published unpatched in
// 2025-2026 (ksmbd, kernel TLS, AF_ALG, ebtables, nvmet, vsock, vmwgfx).
var blockedModules = []string{
	// network file sharing and storage servers
	"ksmbd", "cifs", "nfsd", "nvmet",
	// kernel TLS and the crypto socket interface
	"tls", "af_alg", "algif_aead", "algif_hash", "algif_rng", "algif_skcipher",
	// bridge firewalling and rarely used network protocols
	"ebtables", "appletalk", "atm", "ax25", "caif", "can", "dccp", "ieee802154", "kcm", "netrom",
	"nfc", "phonet", "qrtr", "rds", "rose", "rxrpc", "sctp", "smc", "tipc", "x25",
	"vsock", "hv_sock", "vmw_vsock_virtio_transport", "vmw_vsock_virtio_transport_common", "vmw_vsock_vmci_transport",
	"vsock_diag", "vsock_loopback",
	// serial line disciplines
	"n_gsm", "n_hdlc",
	// hardware a cloud server doesn't have
	"bluetooth", "firewire_core", "thunderbolt", "usb_storage", "vivid", "vmwgfx",
	// file systems nothing here mounts
	"cramfs", "freevxfs", "hfs", "hfsplus", "jffs2", "udf",
}

// hostEnv is where host hardening reads and writes; tests point it at a
// temporary directory and record commands instead of running them.
type hostEnv struct {
	sysctlConf   string
	modprobeConf string
	procSys      string
	procModules  string
	unitDir      string
	journaldConf string
	shadow       string // /etc/shadow, for root's password aging
	binary       string
	firewallFile string
	run          func(ctx context.Context, timeout time.Duration, name string, args ...string) (string, error)
}

func newHostEnv(p paths) *hostEnv {
	return &hostEnv{
		sysctlConf:   p.sysctlConf,
		modprobeConf: p.modprobeConf,
		procSys:      p.procSys,
		procModules:  p.procModules,
		unitDir:      p.unitDir,
		journaldConf: p.journaldConf,
		shadow:       p.shadow,
		binary:       p.binary,
		firewallFile: p.appFirewall,
		run:          run,
	}
}

// journalRetention is how long the server keeps what the apps and the
// system logged: 30 days, within 2 GB, always leaving 5 GB of disk free.
// The owner's Errors page and the daily review read the journal, so it has to
// outlast a long weekend; platform/cloudinit/build.py writes the same file
// on a fresh server so labd finds nothing to change there.
const journalRetention = "30day"

const journaldConf = `# Managed by labd. Keep the apps' logs 30 days (CLAUDE.md, "Logging").
[Journal]
Storage=persistent
MaxRetentionSec=30day
SystemMaxUse=2G
SystemKeepFree=5G
Compress=yes
`

// keepJournal writes journald's drop-in and restarts journald when the file
// changed. It reports "30day" or why it failed.
func keepJournal(ctx context.Context, env *hostEnv) string {
	if env.journaldConf == "" {
		return "not configured"
	}
	changed, err := writeIfChanged(env.journaldConf, journaldConf)
	if err != nil {
		return "failed: " + err.Error()
	}
	if changed {
		if _, err := env.run(ctx, 60*time.Second, "systemctl", "restart", "systemd-journald"); err != nil {
			return "failed: " + err.Error()
		}
		log.Printf("host: journald now keeps logs for %s", journalRetention)
	}
	return journalRetention
}

// hostState is what the last hardening pass achieved, for GET /api/host.
var hostState struct {
	sync.Mutex
	report map[string]any
}

func sysctlFile() string {
	var b strings.Builder
	b.WriteString("# Managed by labd. Kernel settings for a server that runs untrusted apps.\n")
	b.WriteString("# A leading '-' marks a setting some kernels don't have.\n")
	for _, s := range hostSysctls {
		if s.optional {
			b.WriteString("-")
		}
		b.WriteString(s.key + " = " + s.value + "\n")
	}
	return b.String()
}

// modprobeFile blocks each module with an install command that fails, which
// also stops loading on demand. modprobe ignores install commands for modules
// with soft dependencies (ksmbd has a dozen), so each also gets an empty
// softdep line; modprobe uses the first softdep line for a module, and this
// file sorts before every other modprobe configuration (00-) and before the
// kernel's modules.softdep.
func modprobeFile() string {
	var b strings.Builder
	b.WriteString("# Managed by labd. Kernel modules this server never needs. Several had\n")
	b.WriteString("# serious bugs that were exploited recently.\n")
	for _, m := range blockedModules {
		b.WriteString("softdep " + m + " pre: post:\n")
		b.WriteString("install " + m + " /bin/false\n")
	}
	return b.String()
}

// labdGuardDropIn makes systemd give up on a labd that keeps failing to start
// and run labd-fallback.service, which puts the previous binary back.
const labdGuardDropIn = `# Managed by labd. If a new labd keeps failing to start, put the previous one back.
[Unit]
StartLimitIntervalSec=300
StartLimitBurst=5
OnFailure=labd-fallback.service
`

func labdFallbackUnit(binary string) string {
	return fmt.Sprintf(`# Managed by labd.
[Unit]
Description=put the previous labd back after the new one failed to start

[Service]
Type=oneshot
ExecStart=/bin/sh -c 'if [ -x %[1]s.prev ]; then cp -f %[1]s.prev %[1]s.fallback && mv -f %[1]s.fallback %[1]s; fi'
ExecStartPost=/bin/systemctl reset-failed labd.service
ExecStartPost=/bin/systemctl --no-block start labd.service
`, binary)
}

// writeIfChanged writes a managed file and reports whether it changed.
func writeIfChanged(path, content string) (bool, error) {
	if old, err := os.ReadFile(path); err == nil && string(old) == content {
		return false, nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return false, err
	}
	return true, writeFileAtomic(path, []byte(content), 0o644)
}

// hardenHost applies everything above. Failures are reported, never fatal:
// labd must keep serving deploys to be able to receive a fix.
func hardenHost(ctx context.Context, env *hostEnv) map[string]any {
	report := map[string]any{"checked_at": time.Now().UTC().Format(time.RFC3339)}

	sys := map[string]any{}
	if _, err := writeIfChanged(env.sysctlConf, sysctlFile()); err != nil {
		sys["file"] = "failed: " + err.Error()
	} else {
		sys["file"] = env.sysctlConf
	}
	applied, failed, missing := 0, []string{}, []string{}
	for _, s := range hostSysctls {
		p := filepath.Join(env.procSys, strings.ReplaceAll(s.key, ".", "/"))
		err := writeExisting(p, s.value+"\n")
		if err == nil {
			var got []byte
			if got, err = os.ReadFile(p); err == nil && strings.TrimSpace(string(got)) != s.value {
				err = fmt.Errorf("reads back %q", strings.TrimSpace(string(got)))
			}
		}
		switch {
		case err == nil:
			applied++
		case s.optional && errors.Is(err, os.ErrNotExist):
			missing = append(missing, s.key)
		default:
			failed = append(failed, s.key+": "+err.Error())
		}
	}
	sys["applied"], sys["failed"], sys["not_on_this_kernel"] = applied, failed, missing
	report["sysctl"] = sys

	mods := map[string]any{}
	if _, err := writeIfChanged(env.modprobeConf, modprobeFile()); err != nil {
		mods["file"] = "failed: " + err.Error()
	} else {
		mods["file"] = env.modprobeConf
	}
	// Unload blocked modules that are loaded but unused, again while
	// unloading one frees another; a module in use stays until the next
	// reboot.
	tried := map[string]bool{}
	for pass := 0; pass < 5; pass++ {
		unloaded := false
		for _, m := range loadedModules(env.procModules, true) {
			if tried[m] {
				continue
			}
			tried[m] = true
			if _, err := env.run(ctx, 20*time.Second, "modprobe", "-r", m); err != nil {
				log.Printf("host: could not unload %s: %v", m, err)
				continue
			}
			unloaded = true
		}
		if !unloaded {
			break
		}
	}
	mods["still_loaded"] = loadedModules(env.procModules, false)
	report["modules"] = mods

	report["ssh"] = sshOff(ctx, env)
	report["root_password"] = rootPasswordAging(ctx, env)
	report["labd_guard"] = installLabdGuard(ctx, env)
	report["journal"] = keepJournal(ctx, env)

	hostState.Lock()
	hostState.report = report
	hostState.Unlock()
	log.Printf("host hardening: %d kernel settings applied, %d failed, %d not on this kernel; ssh %v; root password %v; labd guard %v; journal %v",
		applied, len(failed), len(missing), report["ssh"], report["root_password"], report["labd_guard"], report["journal"])
	return report
}

// writeExisting writes to a file that must already exist, like a kernel
// setting under /proc/sys.
func writeExisting(path, value string) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_TRUNC, 0)
	if err != nil {
		return err
	}
	if _, err := f.WriteString(value); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// loadedModules lists blocked modules that are loaded (only unused ones
// when unusedOnly), from /proc/modules.
func loadedModules(procModules string, unusedOnly bool) []string {
	f, err := os.Open(procModules)
	if err != nil {
		return []string{}
	}
	defer f.Close()
	blocked := map[string]bool{}
	for _, m := range blockedModules {
		blocked[strings.ReplaceAll(m, "-", "_")] = true
	}
	out := []string{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 3 || !blocked[fields[0]] {
			continue
		}
		if unusedOnly && fields[2] != "0" {
			continue
		}
		out = append(out, fields[0])
	}
	sort.Strings(out)
	return out
}

// sshOff stops and masks the SSH server: no SSH is a design rule here
// (CLAUDE.md), the firewall already blocks port 22, and a server that isn't
// running can't be attacked if the firewall ever fails to load. Masked, an
// update or another unit can't start it again either.
func sshOff(ctx context.Context, env *hostEnv) string {
	var units []string
	for _, u := range []string{"ssh.socket", "ssh.service"} {
		out, _ := env.run(ctx, 10*time.Second, "systemctl", "list-unit-files", "--no-legend", u)
		if strings.HasPrefix(strings.TrimSpace(out), u) {
			units = append(units, u)
		}
	}
	if len(units) == 0 {
		return "not installed"
	}
	for _, u := range units {
		if _, err := env.run(ctx, 30*time.Second, "systemctl", "disable", "--now", u); err != nil {
			return "failed: " + err.Error()
		}
		if _, err := env.run(ctx, 30*time.Second, "systemctl", "mask", u); err != nil {
			return "failed: " + err.Error()
		}
	}
	for _, u := range units {
		if out, _ := env.run(ctx, 10*time.Second, "systemctl", "is-active", u); strings.TrimSpace(out) == "active" {
			return "failed: " + u + " is still running"
		}
	}
	return "off"
}

// rootPasswordAging clears an expired password on root. Hetzner's images ship
// root with "change the password at the next login" (last change day 0 in
// /etc/shadow). Nobody logs in here (SSH is off), but cron checks root's
// account before every job it runs for root and skips the job, logging
// "Authentication token is no longer valid; new one required" in the journal
// every hour, which the daily audit then reports as errors. The last-change
// day is set to today (and a maximum age that ran out is lifted); the password
// itself stays what it is. It reports "ok", what it cleared, or why it failed.
func rootPasswordAging(ctx context.Context, env *hostEnv) string {
	if env.shadow == "" {
		return "not configured"
	}
	raw, err := os.ReadFile(env.shadow)
	if err != nil {
		return "failed: " + err.Error()
	}
	today := int(time.Now().Unix() / 86400)
	for _, line := range strings.Split(string(raw), "\n") {
		f := strings.Split(line, ":")
		if len(f) < 5 || f[0] != "root" {
			continue
		}
		if f[2] == "" {
			return "ok" // no aging at all
		}
		last, err := strconv.Atoi(f[2])
		if err != nil {
			return "failed: unreadable last change " + strconv.Quote(f[2])
		}
		max, maxErr := strconv.Atoi(f[4])
		args := []string{"-d", time.Now().UTC().Format("2006-01-02")}
		var what string
		switch {
		case last == 0:
			what = "cleared a forced password change"
		case maxErr == nil && max >= 0 && last+max < today:
			what = "lifted the password's maximum age"
			args = append(args, "-M", "99999")
		default:
			return "ok"
		}
		if _, err := env.run(ctx, 20*time.Second, "chage", append(args, "root")...); err != nil {
			return "failed: " + err.Error()
		}
		log.Printf("host: %s on root, so cron runs root's jobs again", what)
		return what
	}
	return "failed: no root entry"
}

// nftablesDropIn makes nftables.service load the apps' firewall right after
// the system's (whose "flush ruleset" removes it), at boot before labd runs
// and whenever the system's rules are loaded again.
func nftablesDropIn(firewallFile string) string {
	return "# Managed by labd: the apps' firewall comes back with the system's.\n[Service]\n" +
		"ExecStartPost=-/usr/sbin/nft -f " + firewallFile + "\n" +
		"ExecReload=-/usr/sbin/nft -f " + firewallFile + "\n"
}

// installLabdGuard installs labd.service's drop-in, labd-fallback.service and
// nftables.service's drop-in.
func installLabdGuard(ctx context.Context, env *hostEnv) string {
	a, err := writeIfChanged(filepath.Join(env.unitDir, "labd.service.d", "50-lab-guard.conf"), labdGuardDropIn)
	if err != nil {
		return "failed: " + err.Error()
	}
	b, err := writeIfChanged(filepath.Join(env.unitDir, "labd-fallback.service"), labdFallbackUnit(env.binary))
	if err != nil {
		return "failed: " + err.Error()
	}
	c, err := writeIfChanged(filepath.Join(env.unitDir, "nftables.service.d", "50-lab-apps.conf"), nftablesDropIn(env.firewallFile))
	if err != nil {
		return "failed: " + err.Error()
	}
	if a || b || c {
		if _, err := env.run(ctx, 30*time.Second, "systemctl", "daemon-reload"); err != nil {
			return "failed: " + err.Error()
		}
	}
	return "installed"
}

// --- patch state ---------------------------------------------------------

var aptInstRe = regexp.MustCompile(`^Inst (\S+) (?:\[([^\]]*)\] )?\((\S+) (.*) \[[^\]]*\]\)`)

type pendingUpdate struct {
	Package  string `json:"package"`
	From     string `json:"from,omitempty"`
	To       string `json:"to"`
	Security bool   `json:"security"`
}

// parseAptSimulation reads `apt-get -s dist-upgrade` output.
func parseAptSimulation(out string) []pendingUpdate {
	list := []pendingUpdate{}
	for _, line := range strings.Split(out, "\n") {
		m := aptInstRe.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		list = append(list, pendingUpdate{Package: m[1], From: m[2], To: m[3],
			Security: strings.Contains(m[4], "-security") || strings.Contains(m[4], "Debian-Security")})
	}
	return list
}

func osName() string {
	raw, err := os.ReadFile("/etc/os-release")
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(raw), "\n") {
		if v, ok := strings.CutPrefix(line, "PRETTY_NAME="); ok {
			return strings.Trim(v, `"`)
		}
	}
	return ""
}

func kernelRelease() string {
	var u syscall.Utsname
	if err := syscall.Uname(&u); err != nil {
		return ""
	}
	var b strings.Builder
	for _, c := range u.Release {
		if c == 0 {
			break
		}
		b.WriteByte(byte(c))
	}
	return b.String()
}

func bootTime() string {
	raw, err := os.ReadFile("/proc/stat")
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(raw), "\n") {
		if v, ok := strings.CutPrefix(line, "btime "); ok {
			if n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64); err == nil {
				return time.Unix(n, 0).UTC().Format(time.RFC3339)
			}
		}
	}
	return ""
}

func modTime(path string) string {
	if info, err := os.Stat(path); err == nil {
		return info.ModTime().UTC().Format(time.RFC3339)
	}
	return ""
}

func firstModTime(paths ...string) string {
	for _, p := range paths {
		if t := modTime(p); t != "" {
			return t
		}
	}
	return ""
}

func tailFile(path string, n int) string {
	raw, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return trimLines(strings.TrimRight(string(raw), "\n"), n)
}

func diskSpace(path string) map[string]any {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return map[string]any{"error": err.Error()}
	}
	total := st.Blocks * uint64(st.Bsize)
	free := st.Bavail * uint64(st.Bsize)
	pct := 0.0
	if total > 0 {
		pct = float64(free) * 100 / float64(total)
	}
	return map[string]any{"total_gb": gb(total), "free_gb": gb(free), "free_percent": int(pct)}
}

func gb(n uint64) float64 { return float64(n*10/(1<<30)) / 10 }

const upgradeUnit = "lab-host-upgrade.service"

// hostStatus is GET /api/host: the system, its patch state and the hardening.
func hostStatus(ctx context.Context, env *hostEnv) map[string]any {
	out := map[string]any{
		"os":        osName(),
		"kernel":    kernelRelease(),
		"booted_at": bootTime(),
		"disk":      diskSpace("/"),
	}
	_, err := os.Stat("/run/reboot-required")
	out["reboot_required"] = err == nil
	pkgs := strings.Fields(tailFile("/run/reboot-required.pkgs", 50))
	if pkgs == nil {
		pkgs = []string{}
	}
	out["reboot_for"] = pkgs

	updates := map[string]any{"lists_updated_at": firstModTime("/var/lib/apt/periodic/update-success-stamp",
		"/var/lib/apt/periodic/update-stamp", "/var/cache/apt/pkgcache.bin", "/var/lib/apt/lists")}
	if sim, err := env.run(ctx, 90*time.Second, "apt-get", "-s", "-q", "-o", "Debug::NoLocking=1", "dist-upgrade"); err != nil {
		updates["error"] = err.Error()
	} else {
		list := parseAptSimulation(sim)
		sec := 0
		for _, u := range list {
			if u.Security {
				sec++
			}
		}
		updates["pending"], updates["security"], updates["packages"] = len(list), sec, list
	}
	out["updates"] = updates
	out["last_unattended_upgrade"] = map[string]any{
		"at":  modTime("/var/lib/apt/periodic/unattended-upgrades-stamp"),
		"log": tailFile("/var/log/unattended-upgrades/unattended-upgrades.log", 12),
	}

	job := map[string]any{}
	if show, err := env.run(ctx, 10*time.Second, "systemctl", "show", upgradeUnit, "-p", "ActiveState,Result"); err == nil {
		for _, line := range strings.Split(show, "\n") {
			if k, v, ok := strings.Cut(line, "="); ok {
				job[strings.ToLower(k)] = v
			}
		}
	}
	if logs, err := env.run(ctx, 20*time.Second, "journalctl", "--unit="+upgradeUnit, "--lines=30", "--no-pager", "--output=short-iso"); err == nil {
		job["log"] = logs
	}
	out["upgrade_job"] = job

	journal := map[string]any{"retention": journalRetention}
	if usage, err := env.run(ctx, 20*time.Second, "journalctl", "--disk-usage"); err == nil {
		journal["disk_usage"] = journalDiskUsage(usage)
	}
	out["journal"] = journal

	hostState.Lock()
	out["hardening"] = hostState.report
	hostState.Unlock()
	return out
}

// journalDiskUsage picks the size out of `journalctl --disk-usage`
// ("Archived and active journals take up 1.2G in the file system.").
func journalDiskUsage(out string) string {
	fields := strings.Fields(out)
	for i, f := range fields {
		if f == "up" && i+1 < len(fields) {
			return strings.TrimSuffix(fields[i+1], ".")
		}
	}
	return strings.TrimSpace(out)
}

// startUpgrade installs pending updates now, the same way the nightly
// unattended upgrade does, in a unit of its own so it finishes even if labd
// restarts. It reports a conflict when one is already running.
func startUpgrade(ctx context.Context, env *hostEnv) error {
	if out, _ := env.run(ctx, 10*time.Second, "systemctl", "is-active", upgradeUnit); strings.TrimSpace(out) == "active" ||
		strings.TrimSpace(out) == "activating" {
		return &conflict{"an upgrade is already running"}
	}
	_, _ = env.run(ctx, 10*time.Second, "systemctl", "reset-failed", upgradeUnit)
	_, err := env.run(ctx, 30*time.Second, "systemd-run", "--unit="+upgradeUnit, "--no-block",
		"--description=lab: install updates now", "--setenv=DEBIAN_FRONTEND=noninteractive",
		"--property=TimeoutStartSec=45min",
		"--", "/bin/sh", "-c", "apt-get update -q && unattended-upgrade -v")
	return err
}

func rebootRequired() bool {
	_, err := os.Stat("/run/reboot-required")
	return err == nil
}
