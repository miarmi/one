package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// UnitSpec is everything needed to start one instance.
type UnitSpec struct {
	ID       string
	Exec     string // path through the instance's "current" symlink
	WorkDir  string
	Env      map[string]string
	Memory   string
	Instance string   // the instance's own directory: the only part of labd's state the app sees
	State    string   // labd's state directory, hidden from apps
	Data     []string // the data directory's paths (root's and the app's view)
	Preview  bool     // previews yield CPU and disk time to production
	Basic    bool     // the earlier, looser sandbox, for an app that failed in the hardened one
}

// Runner starts and stops app instances.
type Runner interface {
	// Start (re)starts the instance with this spec; a running one is stopped first.
	Start(ctx context.Context, spec UnitSpec) error
	Stop(ctx context.Context, id string) error
	Active(ctx context.Context, id string) bool
	Logs(ctx context.Context, id string, lines int) (string, error)
	// Journal returns everything the instance logged in the last `since`,
	// oldest first, for the Errors page and the error-rate alert (logs.go).
	Journal(ctx context.Context, id string, since time.Duration) (string, error)
	// DataDir returns the data directory as root sees it and as the app sees it.
	DataDir(id string) (root, app string)
	// CopyData copies production's data directory into a new preview's
	// (data.go), where nothing else is in reach.
	CopyData(ctx context.Context, src, dst string) error
	// CopyAllData copies a stopped preview's whole data directory, secrets
	// included, into production's new one (Manager.PromoteData).
	CopyAllData(ctx context.Context, src, dst string) error
}

// run executes a command with a timeout and returns combined output.
func run(ctx context.Context, timeout time.Duration, name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := cmd.Run()
	text := strings.TrimSpace(out.String())
	if len(text) > 4000 {
		text = text[len(text)-4000:]
	}
	if err != nil {
		if text != "" {
			return text, fmt.Errorf("%s: %w: %s", name, err, text)
		}
		return text, fmt.Errorf("%s: %w", name, err)
	}
	return text, nil
}

// runBig is run for commands whose output matters whole, like a journal
// dump: it keeps the last limit bytes instead of 4000.
func runBig(ctx context.Context, timeout time.Duration, limit int, name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	var out bytes.Buffer
	cmd.Stdout = &out
	var errOut bytes.Buffer
	cmd.Stderr = &errOut
	err := cmd.Run()
	text := out.String()
	if len(text) > limit {
		text = text[len(text)-limit:]
		if i := strings.IndexByte(text, '\n'); i >= 0 {
			text = text[i+1:] // drop the cut line
		}
	}
	if err != nil {
		return text, fmt.Errorf("%s: %w: %s", name, err, strings.TrimSpace(errOut.String()))
	}
	return text, nil
}

// systemdRunner runs each instance as a sandboxed transient unit.
type systemdRunner struct {
	dataRoot    string
	appDataRoot string
	stateDir    string
	labd        string // labd's own binary: the sandbox probe and the data copy run it
	caps        sandboxCaps
}

// sandboxCaps is what this server's systemd can enforce, probed once at start
// so a unit is never started with a property systemd doesn't know.
type sandboxCaps struct {
	bind     bool // apps bind only their own port (SocketBindAllow, v249+)
	hardened bool // the hardened sandbox: system call filter, hidden files, no exec
}

func newSystemdRunner(dataRoot, appDataRoot, stateDir, labd string) *systemdRunner {
	r := &systemdRunner{dataRoot: dataRoot, appDataRoot: appDataRoot, stateDir: stateDir, labd: labd}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	berr := r.probeBind(ctx)
	r.caps.bind = berr == nil
	if berr != nil {
		log.Printf("apps may bind any port: systemd doesn't limit it here: %v", berr)
	}
	herr := r.probeHardened(ctx)
	r.caps.hardened = herr == nil
	if herr != nil {
		log.Printf("apps run in the basic sandbox: the hardened one failed here: %v", herr)
	}
	sandboxState.Lock()
	sandboxState.bindRules = r.caps.bind
	sandboxState.bindErr = ""
	if berr != nil {
		sandboxState.bindErr = berr.Error()
	}
	sandboxState.hardened = r.caps.hardened
	sandboxState.hardenedErr = ""
	if herr != nil {
		sandboxState.hardenedErr = herr.Error()
	}
	sandboxState.Unlock()
	return r
}

// probeBind checks that systemd really refuses a bind the unit isn't allowed:
// without its BPF framework, systemd accepts SocketBindAllow= but ignores it.
func (r *systemdRunner) probeBind(ctx context.Context) error {
	out, err := run(ctx, 20*time.Second, "systemd-run", "--quiet", "--wait", "--pipe", "--collect",
		"--property=DynamicUser=yes", "--property=SocketBindAllow=tcp:9", "--property=SocketBindDeny=any",
		"--", r.labd, "-try-bind", "127.0.0.1:29999")
	switch {
	case strings.Contains(out, "LAB-BIND-REFUSED"):
		return nil
	case err != nil:
		return err
	}
	return errors.New("a unit could bind a port it was not allowed (systemd without its BPF framework)")
}

// probeHardened runs labd's own binary (a static Go program, like the apps)
// with "-sandbox-check" (listen on the port, write and read the data folder)
// in a unit with the full hardened sandbox, laid out like a real instance.
func (r *systemdRunner) probeHardened(ctx context.Context) error {
	const id = "sandbox-probe"
	dir := filepath.Join(r.stateDir, "instances", "."+id)
	os.RemoveAll(dir)
	defer os.RemoveAll(dir)
	defer os.RemoveAll(filepath.Join(r.dataRoot, id))
	defer os.Remove(filepath.Join(r.appDataRoot, id)) // systemd's symlink to it
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	bin := filepath.Join(dir, "app")
	if err := copyExecutable(r.labd, bin); err != nil {
		return err
	}
	spec := UnitSpec{ID: id, Exec: bin, WorkDir: dir, Memory: "128M", Instance: dir, State: r.stateDir,
		Data: []string{filepath.Join(r.dataRoot, id), filepath.Join(r.appDataRoot, id)},
		Env:  map[string]string{"PORT": "29998", "DATA_DIR": filepath.Join(r.appDataRoot, id)}}
	args := []string{"--quiet", "--wait", "--pipe", "--collect", "--working-directory=" + dir,
		"--setenv=PORT=" + spec.Env["PORT"], "--setenv=DATA_DIR=" + spec.Env["DATA_DIR"]}
	for _, p := range unitProperties(spec, sandboxCaps{hardened: true, bind: r.caps.bind}) {
		if !strings.HasPrefix(p, "Restart") {
			args = append(args, "--property="+p)
		}
	}
	args = append(args, "--", bin, "-sandbox-check")
	out, err := run(ctx, 30*time.Second, "systemd-run", args...)
	if err == nil && !strings.Contains(out, "LAB-SANDBOX-OK") {
		err = fmt.Errorf("unexpected answer %q", out)
	}
	return err
}

func prefixed(prefix string, paths []string) string {
	var b strings.Builder
	for _, p := range paths {
		b.WriteString(" " + prefix + p)
	}
	return b.String()
}

func copyExecutable(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

func (r *systemdRunner) DataDir(id string) (string, string) {
	return filepath.Join(r.dataRoot, id), filepath.Join(r.appDataRoot, id)
}

func (r *systemdRunner) Start(ctx context.Context, spec UnitSpec) error {
	_ = r.Stop(ctx, spec.ID)
	args := []string{
		"--unit=" + unitName(spec.ID),
		"--description=lab app " + spec.ID,
		"--collect",
		"--working-directory=" + spec.WorkDir,
	}
	for _, p := range unitProperties(spec, r.caps) {
		args = append(args, "--property="+p)
	}
	keys := make([]string, 0, len(spec.Env))
	for k := range spec.Env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		args = append(args, "--setenv="+k+"="+spec.Env[k])
	}
	args = append(args, "--", spec.Exec)
	_, err := run(ctx, 30*time.Second, "systemd-run", args...)
	return err
}

// sandboxVersion changes whenever unitProperties do, so that apps already
// running are restarted into the new sandbox once (Manager.RefreshSandbox).
const sandboxVersion = "3"

// unitProperties is the sandbox from CONTRACT.md. With caps.bind, the app may
// bind only its own port, so it can never take labd's or another app's.
// With caps.hardened (and not spec.Basic) it gets the hardened sandbox:
// a system call allow list without the calls recent kernel attacks start
// from (io_uring, keyrings, bpf, ptrace, userfaultfd, ...), no capabilities,
// no other processes or kernel files in sight, none of labd's state but its
// own releases, and nothing it may run but its own binary.
func unitProperties(spec UnitSpec, caps sandboxCaps) []string {
	var props []string
	if caps.hardened && !spec.Basic {
		props = hardenedProperties(spec)
	} else {
		props = basicProperties(spec)
	}
	if port := spec.Env["PORT"]; caps.bind && port != "" {
		props = append(props, "SocketBindAllow=tcp:"+port, "SocketBindDeny=any")
	}
	return props
}

// basicProperties is the sandbox of sandbox version 2 (plus CPU limits):
// what a production app falls back to when it can't run in the hardened one.
func basicProperties(spec UnitSpec) []string {
	return append([]string{
		"DynamicUser=yes",
		"StateDirectory=lab/" + spec.ID,
		"NoNewPrivileges=yes",
		"ProtectSystem=strict",
		"ProtectHome=yes",
		"PrivateTmp=yes",
		"PrivateDevices=yes",
		"ProtectKernelTunables=yes",
		"ProtectKernelModules=yes",
		"ProtectControlGroups=yes",
		"RestrictSUIDSGID=yes",
		"LockPersonality=yes",
		"RestrictNamespaces=yes",
		"RestrictAddressFamilies=AF_INET AF_INET6 AF_UNIX",
		"MemoryMax=" + spec.Memory,
		"TasksMax=256",
		"Restart=always",
		"RestartSec=1",
		"TimeoutStopSec=15",
	}, cpuProperties(spec)...)
}

// cpuProperties: one CPU at most, and previews yield CPU and disk time to
// production when both are busy.
func cpuProperties(spec UnitSpec) []string {
	props := []string{"CPUQuota=100%"}
	if spec.Preview {
		props = append(props, "CPUWeight=20", "IOWeight=20")
	}
	return props
}

func hardenedProperties(spec UnitSpec) []string {
	props := []string{
		"DynamicUser=yes",
		"StateDirectory=lab/" + spec.ID,
		"StateDirectoryMode=0700",
		"UMask=0077",
		"NoNewPrivileges=yes",
		"CapabilityBoundingSet=",
		"ProtectSystem=strict",
		"ProtectHome=yes",
		"PrivateTmp=yes",
		"PrivateDevices=yes",
		"PrivateIPC=yes",
		"ProtectKernelTunables=yes",
		"ProtectKernelModules=yes",
		"ProtectKernelLogs=yes",
		"ProtectControlGroups=yes",
		"ProtectClock=yes",
		"ProtectHostname=yes",
		"ProtectProc=invisible",
		"ProcSubset=pid",
		"RestrictSUIDSGID=yes",
		"RestrictRealtime=yes",
		"RestrictNamespaces=yes",
		"LockPersonality=yes",
		"MemoryDenyWriteExecute=yes",
		"RestrictAddressFamilies=AF_INET AF_INET6",
		"SystemCallArchitectures=native",
		"SystemCallFilter=@system-service",
		"SystemCallFilter=~@aio @chown @clock @cpu-emulation @debug @keyring @memlock @module @mount @obsolete @privileged @raw-io @reboot @resources @setuid @swap userfaultfd",
		"SystemCallErrorNumber=EPERM",
		"TemporaryFileSystem=" + spec.State + ":ro",
		"BindReadOnlyPaths=" + spec.Instance,
		// Nothing the app can write may run: "/" alone doesn't reach the
		// mounts systemd makes for its data and temporary folders.
		"NoExecPaths=/ -/tmp -/var/tmp -/dev/shm" + prefixed("-", spec.Data),
		"ExecPaths=" + spec.Instance,
		"MemoryMax=" + spec.Memory,
		"TasksMax=256",
		"Restart=always",
		"RestartSec=1",
		"TimeoutStopSec=15",
	}
	return append(props, cpuProperties(spec)...)
}

// CopyData runs "labd -copy-data" as root in a throwaway unit that can write
// only the two data directories, can't see labd's state or any other app's
// data, and has no network: even a link copyData didn't catch would lead
// nowhere.
func (r *systemdRunner) CopyData(ctx context.Context, src, dst string) error {
	return r.copyUnit(ctx, src, dst, false)
}

// CopyAllData is CopyData for promote-data: "labd -copy-data -all", in the
// same jail, but the stopped preview's folder is only read.
func (r *systemdRunner) CopyAllData(ctx context.Context, src, dst string) error {
	return r.copyUnit(ctx, src, dst, true)
}

func (r *systemdRunner) copyUnit(ctx context.Context, src, dst string, all bool) error {
	if _, err := os.Stat(src); errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err := os.MkdirAll(dst, 0o700); err != nil {
		return err
	}
	args := []string{
		"--quiet", "--wait", "--pipe", "--collect",
		"--property=ProtectSystem=strict",
		"--property=ProtectHome=yes",
		"--property=PrivateTmp=yes",
		"--property=PrivateDevices=yes",
		"--property=PrivateNetwork=yes",
		"--property=PrivateIPC=yes",
		"--property=NoNewPrivileges=yes",
		// root, so it can read the app's files and give new ones back to
		// it, but with no way to change the kernel or leave the jail
		"--property=CapabilityBoundingSet=CAP_CHOWN CAP_DAC_OVERRIDE CAP_DAC_READ_SEARCH CAP_FOWNER",
		"--property=ProtectKernelTunables=yes",
		"--property=ProtectKernelModules=yes",
		"--property=ProtectKernelLogs=yes",
		"--property=ProtectControlGroups=yes",
		"--property=ProtectClock=yes",
		"--property=ProtectHostname=yes",
		"--property=ProtectProc=invisible",
		"--property=ProcSubset=pid",
		"--property=RestrictNamespaces=yes",
		"--property=RestrictRealtime=yes",
		"--property=RestrictSUIDSGID=yes",
		"--property=LockPersonality=yes",
		"--property=MemoryDenyWriteExecute=yes",
		// No network namespace to talk to, and no Unix sockets (no D-Bus):
		// systemd-run can't say "none", so allow only AF_INET, which reaches
		// nothing in a private network namespace.
		"--property=RestrictAddressFamilies=AF_INET",
		"--property=SystemCallArchitectures=native",
		"--property=SystemCallFilter=@system-service",
		"--property=SystemCallFilter=~@aio @clock @cpu-emulation @debug @keyring @memlock @module @mount @obsolete @raw-io @reboot @resources @swap userfaultfd",
		"--property=SystemCallErrorNumber=EPERM",
		"--property=TemporaryFileSystem=" + r.stateDir + ":ro",
		"--property=RuntimeMaxSec=300",
	}
	entries, err := os.ReadDir(r.dataRoot)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if p := filepath.Join(r.dataRoot, e.Name()); p != src && p != dst {
			args = append(args, "--property=InaccessiblePaths=-"+p)
		}
	}
	if all {
		args = append(args, "--description=lab: copy a preview's data into production",
			"--property=ReadWritePaths="+dst, "--", r.labd, "-copy-data", "-all", src, dst)
	} else {
		// sqlite3 may need to write the -shm file next to production's database
		args = append(args, "--description=lab: copy production data into a new preview",
			"--property=ReadWritePaths="+src+" "+dst, "--", r.labd, "-copy-data", src, dst)
	}
	_, err = run(ctx, 6*time.Minute, "systemd-run", args...)
	return err
}

func (r *systemdRunner) Stop(ctx context.Context, id string) error {
	unit := unitName(id)
	_, err := run(ctx, 30*time.Second, "systemctl", "stop", unit)
	_, _ = run(ctx, 10*time.Second, "systemctl", "reset-failed", unit)
	if err != nil && strings.Contains(err.Error(), "not loaded") {
		return nil
	}
	return err
}

func (r *systemdRunner) Active(ctx context.Context, id string) bool {
	out, _ := run(ctx, 10*time.Second, "systemctl", "is-active", unitName(id))
	return strings.TrimSpace(out) == "active"
}

func (r *systemdRunner) Logs(ctx context.Context, id string, lines int) (string, error) {
	// Up to 20000 lines may be asked for; the answer is cut at 8 MB so one
	// request cannot make labd hold an unbounded journal in memory.
	return runBig(ctx, 30*time.Second, 8<<20, "journalctl", "--unit="+unitName(id),
		"--lines="+strconv.Itoa(lines), "--no-pager", "--output=short-iso")
}

func (r *systemdRunner) Journal(ctx context.Context, id string, since time.Duration) (string, error) {
	return runBig(ctx, 30*time.Second, journalMax, "journalctl", "--unit="+unitName(id),
		"--since=-"+strconv.Itoa(int(since.Minutes()))+"min", "--no-pager", "--output=short-iso")
}

// execRunner runs instances as plain child processes. For local tests only.
type execRunner struct {
	dir string // per-instance data and logs live here

	mu    sync.Mutex
	procs map[string]*execProc
}

type execProc struct {
	cmd  *exec.Cmd
	done chan struct{}
	log  *os.File
}

func newExecRunner(dir string) *execRunner {
	return &execRunner{dir: dir, procs: map[string]*execProc{}}
}

func (r *execRunner) DataDir(id string) (string, string) {
	d := filepath.Join(r.dir, "data", id)
	return d, d
}

func (r *execRunner) logPath(id string) string { return filepath.Join(r.dir, "logs", id+".log") }

func (r *execRunner) CopyData(ctx context.Context, src, dst string) error {
	return copyData(ctx, src, dst)
}

func (r *execRunner) CopyAllData(ctx context.Context, src, dst string) error {
	if err := os.MkdirAll(dst, 0o700); err != nil {
		return err
	}
	return copyAllData(ctx, src, dst)
}

func (r *execRunner) Start(ctx context.Context, spec UnitSpec) error {
	_ = r.Stop(ctx, spec.ID)
	root, _ := r.DataDir(spec.ID)
	if err := os.MkdirAll(root, 0o700); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(r.logPath(spec.ID)), 0o755); err != nil {
		return err
	}
	logf, err := os.OpenFile(r.logPath(spec.ID), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	cmd := exec.Command(spec.Exec)
	cmd.Dir = spec.WorkDir
	cmd.Env = []string{"PATH=/usr/bin:/bin"}
	for k, v := range spec.Env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	cmd.Stdout = logf
	cmd.Stderr = logf
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		logf.Close()
		return err
	}
	p := &execProc{cmd: cmd, done: make(chan struct{}), log: logf}
	go func() {
		_ = cmd.Wait()
		logf.Close()
		close(p.done)
	}()
	r.mu.Lock()
	r.procs[spec.ID] = p
	r.mu.Unlock()
	return nil
}

func (r *execRunner) Stop(ctx context.Context, id string) error {
	r.mu.Lock()
	p := r.procs[id]
	delete(r.procs, id)
	r.mu.Unlock()
	if p == nil {
		return nil
	}
	_ = syscall.Kill(-p.cmd.Process.Pid, syscall.SIGTERM)
	select {
	case <-p.done:
	case <-time.After(5 * time.Second):
		_ = syscall.Kill(-p.cmd.Process.Pid, syscall.SIGKILL)
		<-p.done
	}
	return nil
}

func (r *execRunner) Active(ctx context.Context, id string) bool {
	r.mu.Lock()
	p := r.procs[id]
	r.mu.Unlock()
	if p == nil {
		return false
	}
	select {
	case <-p.done:
		return false
	default:
		return true
	}
}

func (r *execRunner) Logs(ctx context.Context, id string, lines int) (string, error) {
	raw, err := os.ReadFile(r.logPath(id))
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	all := strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
	if len(all) > lines {
		all = all[len(all)-lines:]
	}
	return strings.Join(all, "\n"), nil
}

// Journal is the whole log file: a local run has no journald and no times
// other than the ones in the lines themselves.
func (r *execRunner) Journal(ctx context.Context, id string, since time.Duration) (string, error) {
	raw, err := os.ReadFile(r.logPath(id))
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if len(raw) > journalMax {
		raw = raw[len(raw)-journalMax:]
	}
	return string(raw), nil
}

// StopAll is used by tests to clean up.
func (r *execRunner) StopAll() {
	r.mu.Lock()
	ids := make([]string, 0, len(r.procs))
	for id := range r.procs {
		ids = append(ids, id)
	}
	r.mu.Unlock()
	for _, id := range ids {
		_ = r.Stop(context.Background(), id)
	}
}
