package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const keepReleases = 5

// Manager owns instances: deploys, rollbacks, deletes and reconciling.
// One mutex serializes every change, which also serializes deploys to the
// same instance as the contract requires.
type Manager struct {
	cfg    *Config
	store  *Store
	runner Runner
	edge   Edge
	certs  Certs

	mu            sync.Mutex
	certWork      sync.Mutex             // one EnsureCerts at a time
	promoteMu     sync.Mutex             // guards promoting
	promoting     map[string]*PromoteJob // app → its running promote-data job
	healthTimeout time.Duration
	client        *http.Client
	now           func() time.Time
}

func newManager(cfg *Config, store *Store, runner Runner, edge Edge) *Manager {
	return &Manager{
		cfg:           cfg,
		store:         store,
		runner:        runner,
		edge:          edge,
		certs:         buildCerts(cfg),
		healthTimeout: 30 * time.Second,
		// The health check never follows a redirect, so an app can't make
		// labd fetch some other address.
		client: &http.Client{Timeout: 2 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		}},
		now: time.Now,
	}
}

// DeployError is a deploy that failed its health check.
type DeployError struct {
	Msg        string
	RolledBack bool
	Logs       string
}

func (e *DeployError) Error() string { return e.Msg }

// DeployResult is returned on success.
type DeployResult struct {
	URL      string `json:"url"`
	Release  string `json:"release"`
	Previous string `json:"previous"`
}

func (m *Manager) instanceDir(id string) string {
	return filepath.Join(m.cfg.StateDir, "instances", id)
}

func (m *Manager) spec(inst Instance) UnitSpec {
	set := m.store.Settings()
	rootData, appData := m.runner.DataDir(inst.ID())
	current := filepath.Join(m.instanceDir(inst.ID()), "current")
	return UnitSpec{
		ID:       inst.ID(),
		Exec:     filepath.Join(current, "app"),
		WorkDir:  current,
		Memory:   inst.Memory,
		Instance: m.instanceDir(inst.ID()),
		State:    m.cfg.StateDir,
		Data:     []string{rootData, appData},
		Preview:  inst.Env != prodEnv,
		Basic:    inst.Sandbox == sandboxBasic,
		Env: map[string]string{
			"PORT":        strconv.Itoa(inst.Port),
			"DATA_DIR":    appData,
			"APP_ENV":     inst.Env,
			"APP_NAME":    inst.App,
			"APP_VERSION": inst.Commit,
			"BASE_URL":    urlFor(inst.App, inst.Env, set),
		},
	}
}

func newReleaseID(now time.Time, commit string) string {
	c := commit
	if len(c) > 7 {
		c = c[:7]
	}
	if c == "" {
		c = "nocommit"
	}
	return now.UTC().Format("20060102T150405.000Z") + "-" + c
}

// setCurrent atomically points current at releases/<release>.
func setCurrent(instDir, release string) error {
	tmp := filepath.Join(instDir, ".current-"+strconv.FormatInt(time.Now().UnixNano(), 36))
	if err := os.Symlink(filepath.Join("releases", release), tmp); err != nil {
		return err
	}
	if err := os.Rename(tmp, filepath.Join(instDir, "current")); err != nil {
		os.Remove(tmp)
		return err
	}
	return syncDir(instDir)
}

// waitHealthy polls the health path until it returns 200, the unit stops
// running for good, or the timeout passes.
func (m *Manager) waitHealthy(ctx context.Context, inst Instance) bool {
	url := "http://127.0.0.1:" + strconv.Itoa(inst.Port) + inst.Health
	deadline := m.now().Add(m.healthTimeout)
	inactive := 0
	for m.now().Before(deadline) {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if resp, err := m.client.Do(req); err == nil {
			io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return true
			}
		}
		if m.runner.Active(ctx, inst.ID()) {
			inactive = 0
		} else if inactive++; inactive >= 8 {
			return false // crashed and not coming back within ~2 s
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(250 * time.Millisecond):
		}
	}
	return false
}

func (m *Manager) logsFor(ctx context.Context, id string) string {
	out, err := m.runner.Logs(ctx, id, 100)
	if err != nil {
		return "logs unavailable: " + err.Error()
	}
	return out
}

// Deploy installs bundle as a new release of app/env and keeps it only if it
// passes the health check.
func (m *Manager) Deploy(ctx context.Context, app, env, commit string, bundle io.Reader) (*DeployResult, error) {
	if err := m.promoteBusy(app); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	id := instanceID(app, env)
	instDir := m.instanceDir(id)
	if err := makePrivateDirs(m.cfg.StateDir, filepath.Join("instances", id, "releases")); err != nil {
		return nil, err
	}
	if _, exists := m.store.Get(id); !exists && env != prodEnv && diskNearlyFull(m.runner) {
		return nil, &conflict{"the server's disk is nearly full: no new previews until there is room"}
	}
	release := newReleaseID(m.now(), commit)
	relDir := filepath.Join(instDir, "releases", release)
	manifest, err := extractBundle(bundle, relDir, app)
	if err != nil {
		os.RemoveAll(relDir)
		return nil, &badRequest{err.Error()}
	}

	old, existed := m.store.Get(id)
	inst := old
	if !existed {
		port, err := m.store.FreePort()
		if err != nil {
			os.RemoveAll(relDir)
			return nil, err
		}
		inst = Instance{App: app, Env: env, Port: port}
		if env != prodEnv {
			if err := m.seedPreviewData(ctx, app, id); err != nil {
				os.RemoveAll(relDir)
				return nil, fmt.Errorf("copying production data: %w", err)
			}
		}
	}
	previous := old.Release
	if old.Status != "running" {
		previous = "" // nothing healthy to fall back to
	}
	inst.Release = release
	inst.Commit = commit
	inst.Title = manifest.Title
	inst.Public = manifest.Public
	inst.OpenPaths = manifest.OpenPaths
	inst.Memory = manifest.Memory
	inst.Health = manifest.Health
	inst.Sandbox = "" // a new release always starts in the hardened sandbox
	inst.RolledBack = false
	inst.Note = ""

	if err := setCurrent(instDir, release); err != nil {
		os.RemoveAll(relDir)
		return nil, err
	}
	if err := m.runner.Start(ctx, m.spec(inst)); err == nil && m.waitHealthy(ctx, inst) {
		inst.Previous = previous
		inst.PrevCommit = ""
		if previous != "" {
			inst.PrevCommit = old.Commit
		}
		inst.Status = "running"
		inst.DeployedAt = m.now().UTC()
		if err := m.store.Put(inst); err != nil {
			return nil, err
		}
		m.prune(instDir, inst)
		// In domain mode a new host gets its certificate before the deploy
		// answers, so its https address works at once.
		var certErr error
		if set := m.store.Settings(); set.Domain != "" {
			certErr = m.certs.Ensure(ctx, hostFor(app, env, set))
		}
		if err := m.applyEdge(ctx); err != nil {
			return nil, fmt.Errorf("deployed, but routing failed: %w", err)
		}
		if certErr != nil {
			return nil, fmt.Errorf("deployed, but its HTTPS certificate failed (labd tries again every 10 minutes): %w", certErr)
		}
		return &DeployResult{URL: urlFor(app, env, m.store.Settings()), Release: release, Previous: previous}, nil
	}

	logs := m.logsFor(ctx, id)
	derr := &DeployError{Msg: "the new release did not pass its health check", Logs: logs}
	if previous != "" {
		if err := setCurrent(instDir, previous); err == nil {
			_ = m.runner.Start(ctx, m.spec(old))
			if m.waitHealthy(ctx, old) {
				derr.RolledBack = true
				derr.Msg += "; the previous release is serving again"
			} else {
				derr.Msg += "; the previous release did not come back either"
				old.Status = "failed"
				_ = m.store.Put(old)
			}
		}
	} else {
		_ = m.runner.Stop(ctx, id)
		if existed {
			old.Status = "failed"
			_ = m.store.Put(old)
			// current points at the release that is about to be removed;
			// point it back at the one the state names, or at nothing.
			if _, err := os.Stat(filepath.Join(instDir, "releases", old.Release)); old.Release != "" && err == nil {
				_ = setCurrent(instDir, old.Release)
			} else {
				os.Remove(filepath.Join(instDir, "current"))
			}
		} else {
			inst.Release = ""
			inst.Status = "failed"
			inst.DeployedAt = m.now().UTC()
			_ = m.store.Put(inst)
			os.Remove(filepath.Join(instDir, "current"))
		}
	}
	os.RemoveAll(relDir)
	_ = m.applyEdge(ctx)
	return nil, derr
}

// seedPreviewData copies production's data into a new preview's data dir.
func (m *Manager) seedPreviewData(ctx context.Context, app, id string) error {
	if _, ok := m.store.Get(instanceID(app, prodEnv)); !ok {
		return nil
	}
	src, _ := m.runner.DataDir(instanceID(app, prodEnv))
	dst, _ := m.runner.DataDir(id)
	if err := os.RemoveAll(dst); err != nil {
		return err
	}
	return m.runner.CopyData(ctx, src, dst)
}

// prune keeps the newest releases plus current and previous.
func (m *Manager) prune(instDir string, inst Instance) {
	entries, err := os.ReadDir(filepath.Join(instDir, "releases"))
	if err != nil {
		return
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() {
			names = append(names, e.Name())
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(names)))
	for i, n := range names {
		if i < keepReleases || n == inst.Release || n == inst.Previous {
			continue
		}
		os.RemoveAll(filepath.Join(instDir, "releases", n))
	}
}

// Rollback makes the previous release current again.
func (m *Manager) Rollback(ctx context.Context, app, env string) (*DeployResult, error) {
	if err := m.promoteBusy(app); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	id := instanceID(app, env)
	inst, ok := m.store.Get(id)
	if !ok {
		return nil, errNotFound
	}
	if inst.Previous == "" {
		return nil, &conflict{"there is no previous release to go back to"}
	}
	instDir := m.instanceDir(id)
	target := inst
	target.Release, target.Previous = inst.Previous, inst.Release
	target.Commit, target.PrevCommit = inst.PrevCommit, inst.Commit
	if err := setCurrent(instDir, target.Release); err != nil {
		return nil, err
	}
	if err := m.runner.Start(ctx, m.spec(target)); err == nil && m.waitHealthy(ctx, target) {
		target.Status = "running"
		target.RolledBack = true // the pipeline leaves it alone until its app changes again
		target.DeployedAt = m.now().UTC()
		if err := m.store.Put(target); err != nil {
			return nil, err
		}
		return &DeployResult{URL: urlFor(app, env, m.store.Settings()), Release: target.Release, Previous: target.Previous}, nil
	}
	logs := m.logsFor(ctx, id)
	_ = setCurrent(instDir, inst.Release)
	_ = m.runner.Start(ctx, m.spec(inst))
	return nil, &DeployError{Msg: "the previous release did not pass its health check; kept the current one", Logs: logs}
}

// Delete removes an instance completely: unit, files, data (secrets
// included), certificate and route. For production this is the retire path;
// the API layer makes sure it is called only from main and on purpose.
func (m *Manager) Delete(ctx context.Context, app, env string) error {
	if err := m.promoteBusy(app); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.deleteLocked(ctx, instanceID(app, env))
}

func (m *Manager) deleteLocked(ctx context.Context, id string) error {
	inst, ok := m.store.Get(id)
	if !ok {
		return errNotFound
	}
	if err := m.runner.Stop(ctx, id); err != nil {
		return err
	}
	root, appView := m.runner.DataDir(id)
	if err := os.RemoveAll(root); err != nil {
		return err
	}
	if appView != root {
		os.Remove(appView) // systemd's symlink to the private dir
	}
	if inst.Env == prodEnv { // retiring: all data, backups promote-data kept included
		for _, b := range promoteBackups(root) {
			if err := os.RemoveAll(b); err != nil {
				return err
			}
		}
	}
	if err := os.RemoveAll(m.instanceDir(id)); err != nil {
		return err
	}
	if err := m.store.Delete(id); err != nil {
		return err
	}
	if err := m.applyEdge(ctx); err != nil {
		return err
	}
	if set := m.store.Settings(); set.Domain != "" {
		if err := m.certs.Remove(ctx, hostFor(inst.App, inst.Env, set)); err != nil {
			log.Printf("certificates: removing %s's: %v", id, err)
		}
	}
	return nil
}

// GC deletes every preview whose name is not in keep.
func (m *Manager) GC(ctx context.Context, keep []string) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	want := map[string]bool{}
	for _, k := range keep {
		want[k] = true
	}
	var removed []string
	for _, inst := range m.store.List() {
		if inst.Env == prodEnv || want[inst.Env] {
			continue
		}
		if err := m.deleteLocked(ctx, inst.ID()); err != nil && !errors.Is(err, errNotFound) {
			return removed, err
		}
		removed = append(removed, inst.ID())
	}
	return removed, nil
}

// Reconcile starts every instance that should run but doesn't (for example
// after a reboot: transient units are gone) and re-applies routing. The
// health checks that follow are only logged and run without the lock, so
// deploys don't wait for them. When the disk is nearly full it stops the
// previews first, so a runaway preview can't take production, labd or the
// system updates down with it.
func (m *Manager) Reconcile(ctx context.Context) {
	m.mu.Lock()
	if diskNearlyFull(m.runner) {
		for _, inst := range m.store.List() {
			if inst.Env == prodEnv || inst.Status != "running" {
				continue
			}
			log.Printf("reconcile: the disk is nearly full; stopping preview %s", inst.ID())
			_ = m.runner.Stop(ctx, inst.ID())
			inst.Status = "failed"
			inst.Note = "stopped because the server's disk was nearly full"
			_ = m.store.Put(inst)
		}
	}
	var started []Instance
	for _, inst := range m.store.List() {
		if inst.Status != "running" || m.runner.Active(ctx, inst.ID()) {
			continue
		}
		if err := m.runner.Start(ctx, m.spec(inst)); err != nil {
			log.Printf("reconcile: starting %s: %v", inst.ID(), err)
			continue
		}
		started = append(started, inst)
	}
	if err := m.applyEdge(ctx); err != nil {
		log.Printf("reconcile: routing: %v", err)
	}
	m.mu.Unlock()
	for _, inst := range started {
		if !m.waitHealthy(ctx, inst) {
			log.Printf("reconcile: %s started but is not healthy", inst.ID())
		}
	}
}

// RefreshSandbox restarts running apps once after the sandbox changed, so
// they run with the same limits as new deploys. stamp remembers the version.
func (m *Manager) RefreshSandbox(ctx context.Context, stamp string) {
	if b, err := os.ReadFile(stamp); err == nil && strings.TrimSpace(string(b)) == sandboxVersion {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, inst := range m.store.List() {
		if inst.Status == "running" {
			m.restartInSandbox(ctx, inst)
		}
	}
	if err := writeFileAtomic(stamp, []byte(sandboxVersion+"\n"), 0o600); err != nil {
		log.Printf("sandbox: %v", err)
	}
}

// restartInSandbox restarts a running instance in the hardened sandbox. A
// production app that doesn't come up there goes back to the basic sandbox
// rather than staying down; the state records that (and /api/status and the
// pipeline show it), so restarts keep it there until its next deploy, which
// tries the hardened sandbox again. A preview is prototype code: it stays
// down instead, until it is deployed again.
func (m *Manager) restartInSandbox(ctx context.Context, inst Instance) {
	hard := inst
	hard.Sandbox = ""
	if err := m.runner.Start(ctx, m.spec(hard)); err == nil && m.waitHealthy(ctx, hard) {
		if inst.Sandbox != "" {
			_ = m.store.Put(hard)
		}
		return
	}
	logs := m.logsFor(ctx, inst.ID())
	if inst.Env != prodEnv {
		log.Printf("sandbox: preview %s is not healthy in the hardened sandbox; stopped:\n%s", inst.ID(), logs)
		_ = m.runner.Stop(ctx, inst.ID())
		inst.Status = "failed"
		inst.Note = "did not start in the hardened sandbox; deploy it again to retry"
		_ = m.store.Put(inst)
		_ = m.applyEdge(ctx)
		return
	}
	log.Printf("sandbox: %s is not healthy in the hardened sandbox; going back to the basic one:\n%s", inst.ID(), logs)
	basic := inst
	basic.Sandbox = sandboxBasic
	if err := m.runner.Start(ctx, m.spec(basic)); err != nil || !m.waitHealthy(ctx, basic) {
		log.Printf("sandbox: %s is not healthy in the basic sandbox either", inst.ID())
		return
	}
	if inst.Sandbox != sandboxBasic {
		_ = m.store.Put(basic)
	}
}

func (m *Manager) view() EdgeView {
	set := m.store.Settings()
	v := EdgeView{Mode: "bootstrap", Upstream: m.cfg.paths.labdUpstream, Webroot: m.cfg.paths.webroot, AllowIPs: set.AllowIPs}
	if set.Domain != "" {
		v.Mode = "domain"
		v.Control = set.Domain
		if cert, key, ok := m.certs.Paths(set.Domain); ok {
			v.ControlCert, v.ControlKey = cert, key
		}
	}
	for _, inst := range m.store.List() {
		if inst.Status != "running" {
			continue
		}
		host := hostFor(inst.App, inst.Env, set)
		if host == "" {
			continue
		}
		r := Route{Host: host, Port: inst.Port, Public: inst.Public, OpenPaths: inst.OpenPaths}
		if set.Domain != "" {
			if cert, key, ok := m.certs.Paths(host); ok {
				r.Cert, r.Key = cert, key
			}
		}
		v.Routes = append(v.Routes, r)
	}
	return v
}

func (m *Manager) applyEdge(ctx context.Context) error {
	return m.edge.Apply(ctx, m.view())
}

// certError is a domain the lab can't switch to: no certificate for it.
type certError struct{ msg string }

func (e *certError) Error() string { return e.msg }

// ApplySettings stores new settings and re-renders routing. A domain is
// taken only once the lab's own host has its certificate (which also proves
// the DNS records point here); the apps' certificates follow in the
// background, each host switching to HTTPS as soon as it has one. When the
// lab's address changes, running apps restart to learn theirs (BASE_URL).
func (m *Manager) ApplySettings(ctx context.Context, set Settings) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	old := m.store.Settings()
	if set.Domain != "" {
		if err := m.certs.Ensure(ctx, set.Domain); err != nil {
			return &certError{fmt.Sprintf("could not get a certificate for %s, so the lab stays as it is (do %s and *.%s point to this server?): %v",
				set.Domain, set.Domain, set.Domain, err)}
		}
	}
	if err := m.store.SetSettings(set); err != nil {
		return err
	}
	if baseDomain(old) != baseDomain(set) {
		for _, inst := range m.store.List() {
			if inst.Status != "running" {
				continue
			}
			if err := m.runner.Start(ctx, m.spec(inst)); err != nil || !m.waitHealthy(ctx, inst) {
				log.Printf("settings: %s did not come back healthy after learning its new address: %v", inst.ID(), err)
			}
		}
	}
	if err := m.applyEdge(ctx); err != nil {
		return err
	}
	if set.Domain != "" {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
			defer cancel()
			m.EnsureCerts(ctx)
		}()
	}
	return nil
}

// EnsureCerts gets the certificates domain mode still lacks, the lab's own
// host first, and routes each host over HTTPS as soon as it has one. A host
// whose certificate failed waits before the next try (certRetry).
func (m *Manager) EnsureCerts(ctx context.Context) {
	if !m.certWork.TryLock() {
		return // already at it
	}
	defer m.certWork.Unlock()
	tried := map[string]bool{}
	for ctx.Err() == nil {
		host := m.missingCert(tried)
		if host == "" {
			return
		}
		tried[host] = true
		if err := m.certs.Ensure(ctx, host); err != nil {
			log.Printf("certificates: %v", err)
			continue
		}
		log.Printf("certificates: %s has its certificate", host)
		m.mu.Lock()
		if err := m.applyEdge(ctx); err != nil {
			log.Printf("certificates: routing: %v", err)
		}
		m.mu.Unlock()
	}
}

// missingCert returns a host domain mode needs a certificate for that has
// none yet, other than those in skip, or "".
func (m *Manager) missingCert(skip map[string]bool) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	set := m.store.Settings()
	if set.Domain == "" {
		return ""
	}
	hosts := []string{set.Domain}
	for _, inst := range m.store.List() {
		if inst.Status == "running" {
			hosts = append(hosts, hostFor(inst.App, inst.Env, set))
		}
	}
	for _, h := range hosts {
		if _, _, ok := m.certs.Paths(h); !ok && !skip[h] {
			return h
		}
	}
	return ""
}

var errNotFound = errors.New("no such instance")

// sandboxBasic marks an instance that runs in the basic sandbox (runner.go).
const sandboxBasic = "basic"

type badRequest struct{ msg string }

func (e *badRequest) Error() string { return e.msg }

type conflict struct{ msg string }

func (e *conflict) Error() string { return e.msg }

// statusOf describes one instance for GET /api/status.
func statusOf(inst Instance, set Settings, hardened bool) map[string]any {
	sandbox := "hardened"
	if !hardened || inst.Sandbox == sandboxBasic {
		sandbox = "basic"
	}
	return map[string]any{
		"app":         inst.App,
		"env":         inst.Env,
		"url":         urlFor(inst.App, inst.Env, set),
		"release":     inst.Release,
		"commit":      inst.Commit,
		"status":      inst.Status,
		"sandbox":     sandbox,
		"rolled_back": inst.RolledBack,
		"note":        inst.Note,
		"deployed_at": inst.DeployedAt.Format(time.RFC3339),
	}
}

// diskFree is free space for unprivileged writers where app data lives (a
// var for tests).
var diskFree = func(path string) (uint64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, err
	}
	return st.Bavail * uint64(st.Bsize), nil
}

const diskReserve = 1 << 30

// diskNearlyFull reports whether less than 1 GB is left where app data lives.
func diskNearlyFull(r Runner) bool {
	root, _ := r.DataDir("x")
	free, err := diskFree(filepath.Dir(root))
	if err != nil {
		free, err = diskFree("/")
	}
	return err == nil && free < diskReserve
}

// makePrivateDirs creates rel's folders under base so they can be passed
// through but not listed (apps in the basic sandbox run their binary from
// there; they need not see the other instances).
func makePrivateDirs(base, rel string) error {
	dir := base
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		dir = filepath.Join(dir, part)
		if err := os.Mkdir(dir, 0o711); err != nil && !errors.Is(err, os.ErrExist) {
			return err
		}
		if err := os.Chmod(dir, 0o711); err != nil {
			return err
		}
	}
	return nil
}

func trimLines(s string, n int) string {
	lines := strings.Split(s, "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}
