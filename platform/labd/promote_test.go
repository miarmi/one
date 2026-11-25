package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// promoteSetup deploys hello to production and to preview "itest", and puts
// recognisable data into both.
func promoteSetup(t *testing.T) (h *harness, prodRoot, prevRoot string) {
	h = newHarness(t)
	h.call("POST", "/api/settings", []byte(`{"domain":"","public_ip":"127.0.0.1"}`), refMain)
	code, _, raw := h.call("POST", "/api/deploy?app=hello&env=prod&commit=abc1234", bundleOf(t, testApps.good, "hello", ""), refMain)
	mustStatus(t, "deploy prod", code, 200, raw)
	code, _, raw = h.call("POST", "/api/deploy?app=hello&env=itest&commit=abc1234", bundleOf(t, testApps.good, "hello", ""), "refs/heads/itest")
	mustStatus(t, "deploy preview", code, 200, raw)
	prevRoot, _ = h.runner.DataDir("hello--itest")
	prodRoot, _ = h.runner.DataDir("hello--prod")
	os.MkdirAll(prodRoot, 0o700)
	os.WriteFile(filepath.Join(prodRoot, "prod-only.txt"), []byte("old production"), 0o600)
	os.MkdirAll(filepath.Join(prevRoot, "secrets"), 0o755)
	os.WriteFile(filepath.Join(prevRoot, "secrets", "token"), []byte("tok"), 0o644)
	os.WriteFile(filepath.Join(prevRoot, "hello.db"), []byte("db"), 0o600)
	os.WriteFile(filepath.Join(prevRoot, "hello.db-wal"), []byte("wal"), 0o600)
	os.WriteFile(filepath.Join(prevRoot, "hello.db-shm"), []byte("shm"), 0o600)
	os.MkdirAll(filepath.Join(prevRoot, "sub"), 0o700)
	os.WriteFile(filepath.Join(prevRoot, "sub", "run.sh"), []byte("x"), 0o755)
	return h, prodRoot, prevRoot
}

// promote starts a promote-data job through the API and polls its status
// until it ends, as the workflow does.
func (h *harness) promote(t *testing.T) map[string]any {
	t.Helper()
	code, js, raw := h.call("POST", "/api/promote-data?app=hello&env=itest&confirm=hello", nil, refMain)
	mustStatus(t, "start promote", code, 202, raw)
	id := js["job"].(map[string]any)["id"]
	for range 300 {
		code, js, raw = h.call("GET", "/api/promote-data?app=hello", nil, refMain)
		mustStatus(t, "promote status", code, 200, raw)
		job := js["job"].(map[string]any)
		if job["id"] != id {
			t.Fatalf("status of another job: %v", job)
		}
		if job["state"] != jobRunning {
			return job
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("the promote job did not end")
	return nil
}

func mode(t *testing.T, p string) os.FileMode {
	t.Helper()
	info, err := os.Lstat(p)
	if err != nil {
		t.Fatal(err)
	}
	return info.Mode().Perm()
}

func TestPromoteDataRefusals(t *testing.T) {
	h, prodRoot, _ := promoteSetup(t)
	for _, c := range []struct {
		what, uri, ref string
		want           int
	}{
		{"from a branch", "/api/promote-data?app=hello&env=itest&confirm=hello", "refs/heads/itest", 403},
		{"without confirmation", "/api/promote-data?app=hello&env=itest", refMain, 400},
		{"wrong confirmation", "/api/promote-data?app=hello&env=itest&confirm=hell", refMain, 400},
		{"production as source", "/api/promote-data?app=hello&env=prod&confirm=hello", refMain, 400},
		{"no such preview", "/api/promote-data?app=hello&env=other&confirm=hello", refMain, 409},
		{"no production", "/api/promote-data?app=nope&env=itest&confirm=nope", refMain, 409},
	} {
		code, _, raw := h.call("POST", c.uri, nil, c.ref)
		mustStatus(t, c.what, code, c.want, raw)
	}
	// Production down: refused, and nothing touched.
	h.runner.Stop(t.Context(), "hello--prod")
	job := h.promote(t)
	if job["state"] != jobFailed || !strings.Contains(job["error"].(string), "not healthy") {
		t.Fatalf("production unhealthy: %v", job)
	}
	if b, _ := os.ReadFile(filepath.Join(prodRoot, "prod-only.txt")); string(b) != "old production" {
		t.Error("a refused promote changed production's data")
	}
	if len(promoteBackups(prodRoot)) != 0 {
		t.Error("a refused promote made a backup")
	}
}

func TestPromoteData(t *testing.T) {
	h, prodRoot, prevRoot := promoteSetup(t)
	var offset time.Duration
	h.srv.mgr.now = func() time.Time { return time.Now().Add(offset) }

	js := h.promote(t)
	backup, _ := js["backup"].(string)
	if js["state"] != jobDone {
		t.Fatalf("promote: %v", js)
	}
	if !strings.HasPrefix(backup, prodRoot+promoteBackupMark) {
		t.Fatalf("backup %q", backup)
	}
	if b, _ := os.ReadFile(filepath.Join(backup, "prod-only.txt")); string(b) != "old production" {
		t.Error("production's old data was not kept")
	}
	if _, err := os.Stat(filepath.Join(prodRoot, "prod-only.txt")); !os.IsNotExist(err) {
		t.Error("production still has its old data")
	}
	for name, want := range map[string]string{"secrets/token": "tok", "hello.db": "db", "hello.db-wal": "wal", "hello.db-shm": "shm", "sub/run.sh": "x"} {
		if b, _ := os.ReadFile(filepath.Join(prodRoot, name)); string(b) != want {
			t.Errorf("%s: %q, want %q", name, b, want)
		}
	}
	for name, want := range map[string]os.FileMode{".": 0o700, "secrets": 0o700, "secrets/token": 0o600, "sub/run.sh": 0o700, "hello.db": 0o600} {
		if got := mode(t, filepath.Join(prodRoot, name)); got != want {
			t.Errorf("%s: mode %v, want %v", name, got, want)
		}
	}
	if b, _ := os.ReadFile(filepath.Join(prevRoot, "secrets", "token")); string(b) != "tok" {
		t.Error("the preview lost its copy")
	}
	for _, env := range []string{"prod", "itest"} {
		if c, _ := h.appGet("hello", env, "/healthz"); c != 200 {
			t.Errorf("%s is not running after the promote", env)
		}
	}

	// A second run is safe: another backup, the first one kept.
	offset = 2 * time.Second
	js = h.promote(t)
	if js["state"] != jobDone || js["backup"] == backup || len(promoteBackups(prodRoot)) != 2 {
		t.Fatalf("second backup: %v, all: %v", js["backup"], promoteBackups(prodRoot))
	}
	if c, _ := h.appGet("hello", "prod", "/healthz"); c != 200 {
		t.Error("production is down after the second promote")
	}

	// Retiring production removes the backups with the rest of its data.
	code, _, raw := h.call("DELETE", "/api/instances?app=hello&env=prod&retire=hello", nil, refMain)
	mustStatus(t, "retire", code, 200, raw)
	if left := promoteBackups(prodRoot); len(left) != 0 {
		t.Errorf("retire left backups: %v", left)
	}
}

// poisonRunner can't start production while its data holds a "poison" file,
// like an app that can't read the data it was given.
type poisonRunner struct{ *execRunner }

func (p *poisonRunner) Start(ctx context.Context, spec UnitSpec) error {
	if _, err := os.Stat(filepath.Join(spec.Data[0], "poison")); err == nil && !spec.Preview {
		_ = p.execRunner.Stop(ctx, spec.ID)
		return errors.New("cannot open its data")
	}
	return p.execRunner.Start(ctx, spec)
}

func TestPromoteDataPutsBackOnFailedHealthCheck(t *testing.T) {
	h, prodRoot, prevRoot := promoteSetup(t)
	os.WriteFile(filepath.Join(prevRoot, "poison"), nil, 0o600)
	m := newManager(h.cfg, h.srv.store, &poisonRunner{h.runner}, h.edge)
	m.healthTimeout = 5 * time.Second

	_, err := m.PromoteData(t.Context(), "hello", "itest")
	var de *DeployError
	if !errors.As(err, &de) || !de.RolledBack {
		t.Fatalf("want a rolled-back DeployError, got %v", err)
	}
	if b, _ := os.ReadFile(filepath.Join(prodRoot, "prod-only.txt")); string(b) != "old production" {
		t.Error("production's own data was not put back")
	}
	for _, gone := range []string{"poison", "secrets"} {
		if _, err := os.Stat(filepath.Join(prodRoot, gone)); !os.IsNotExist(err) {
			t.Errorf("%s from the preview is still in production", gone)
		}
	}
	if left := promoteBackups(prodRoot); len(left) != 0 {
		t.Errorf("the backup should be back in place, found %v", left)
	}
	for _, env := range []string{"prod", "itest"} {
		if c, _ := h.appGet("hello", env, "/healthz"); c != 200 {
			t.Errorf("%s is not running after the rollback", env)
		}
	}
}

// The promote copy follows no link either, and keeps only the owner's bits.
func TestCopyAllDataFollowsNothing(t *testing.T) {
	src, outside := t.TempDir(), t.TempDir()
	dst := filepath.Join(t.TempDir(), "prod")
	os.WriteFile(filepath.Join(outside, "secret"), []byte("TOPSECRET"), 0o600)
	os.Symlink(filepath.Join(outside, "secret"), filepath.Join(src, "leak.txt"))
	os.Symlink(outside, filepath.Join(src, "secrets"))
	os.WriteFile(filepath.Join(src, "open.txt"), []byte("o"), 0o666)
	if err := copyAllData(t.Context(), src, dst); err != nil {
		t.Fatal(err)
	}
	for _, gone := range []string{"leak.txt", "secrets"} {
		if _, err := os.Lstat(filepath.Join(dst, gone)); !os.IsNotExist(err) {
			t.Errorf("%s was copied", gone)
		}
	}
	if got := mode(t, filepath.Join(dst, "open.txt")); got != 0o600 {
		t.Errorf("open.txt: mode %v, want 0600", got)
	}
}

// slowRunner holds the copy until the test lets it go.
type slowRunner struct {
	*execRunner
	copying, release chan struct{}
}

func (s *slowRunner) CopyAllData(ctx context.Context, src, dst string) error {
	close(s.copying)
	<-s.release
	return s.execRunner.CopyAllData(ctx, src, dst)
}

func waitJob(t *testing.T, m *Manager, app string) *PromoteJob {
	t.Helper()
	for range 300 {
		job, err := m.PromoteStatus(app)
		if err != nil {
			t.Fatal(err)
		}
		if job.State != jobRunning {
			return job
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("the promote job did not end")
	return nil
}

// While a job runs: its status says so, a second job and changes to the
// app are refused, other apps are not affected by the refusal.
func TestPromoteJobWhileRunning(t *testing.T) {
	h, prodRoot, _ := promoteSetup(t)
	slow := &slowRunner{h.runner, make(chan struct{}), make(chan struct{})}
	m := newManager(h.cfg, h.srv.store, slow, h.edge)
	m.healthTimeout = 5 * time.Second
	if _, err := m.PromoteStatus("hello"); !errors.Is(err, errNotFound) {
		t.Fatalf("status before any job: %v", err)
	}
	started, err := m.StartPromote("hello", "itest")
	if err != nil || started.State != jobRunning {
		t.Fatalf("start: %v %+v", err, started)
	}
	<-slow.copying
	job, _ := m.PromoteStatus("hello")
	if job.State != jobRunning || job.Step != stepCopying || !strings.HasPrefix(job.Backup, prodRoot+promoteBackupMark) {
		t.Fatalf("status while copying: %+v", job)
	}
	var cf *conflict
	if _, err := m.StartPromote("hello", "itest"); !errors.As(err, &cf) || !strings.Contains(err.Error(), "already running") {
		t.Errorf("second job: %v", err)
	}
	if _, err := m.Deploy(t.Context(), "hello", "prod", "abc", strings.NewReader("")); !errors.As(err, &cf) {
		t.Errorf("deploy during the job: %v", err)
	}
	if _, err := m.Rollback(t.Context(), "hello", "itest"); !errors.As(err, &cf) {
		t.Errorf("rollback during the job: %v", err)
	}
	if err := m.Delete(t.Context(), "hello", "prod"); !errors.As(err, &cf) {
		t.Errorf("retire during the job: %v", err)
	}
	close(slow.release)
	job = waitJob(t, m, "hello")
	if job.State != jobDone || job.Error != "" || job.Finished.IsZero() {
		t.Fatalf("finished job: %+v", job)
	}
	if _, err := os.Stat(filepath.Join(job.Backup, "prod-only.txt")); err != nil {
		t.Errorf("backup: %v", err)
	}
	if err := m.promoteBusy("hello"); err != nil {
		t.Errorf("still busy after the job: %v", err)
	}
}

// A job whose production fails its health check ends failed, rolled back,
// with no backup left to point at (the data is back in place).
func TestPromoteJobFailedRollsBack(t *testing.T) {
	h, prodRoot, prevRoot := promoteSetup(t)
	os.WriteFile(filepath.Join(prevRoot, "poison"), nil, 0o600)
	m := newManager(h.cfg, h.srv.store, &poisonRunner{h.runner}, h.edge)
	m.healthTimeout = 5 * time.Second
	if _, err := m.StartPromote("hello", "itest"); err != nil {
		t.Fatal(err)
	}
	job := waitJob(t, m, "hello")
	if job.State != jobFailed || !job.RolledBack || job.Backup != "" || !strings.Contains(job.Error, "put back") {
		t.Fatalf("job: %+v", job)
	}
	if b, _ := os.ReadFile(filepath.Join(prodRoot, "prod-only.txt")); string(b) != "old production" {
		t.Error("production's own data was not put back")
	}
}

// A labd that restarts mid-copy marks the job failed, says so, and puts
// production's own data back before anything starts the app again.
func TestPromoteJobAfterRestart(t *testing.T) {
	h, prodRoot, _ := promoteSetup(t)
	h.runner.Stop(t.Context(), "hello--prod")
	backup := prodRoot + promoteBackupMark + "20261116T120000Z"
	os.Rename(prodRoot, backup)
	os.MkdirAll(filepath.Join(prodRoot, "secrets"), 0o700) // a partial copy
	m := newManager(h.cfg, h.srv.store, h.runner, h.edge)
	m.promoteMu.Lock()
	m.savePromoteLocked(&PromoteJob{ID: "j1", App: "hello", From: "itest", State: jobRunning, Step: stepCopying, Backup: backup, Started: time.Now()})
	m.promoteMu.Unlock()

	restarted := newManager(h.cfg, h.srv.store, h.runner, h.edge)
	restarted.RecoverPromotes()
	job, err := restarted.PromoteStatus("hello")
	if err != nil {
		t.Fatal(err)
	}
	if job.State != jobFailed || !strings.Contains(job.Error, "labd restarted") || !strings.Contains(job.Error, "put back") || job.Backup != "" {
		t.Fatalf("job after restart: %+v", job)
	}
	if b, _ := os.ReadFile(filepath.Join(prodRoot, "prod-only.txt")); string(b) != "old production" {
		t.Error("production's own data was not put back")
	}
	if _, err := os.Stat(filepath.Join(prodRoot, "secrets")); !os.IsNotExist(err) {
		t.Error("the partial copy is still there")
	}

	// A restart after the copy finished does not claim success either.
	restarted.promoteMu.Lock()
	restarted.savePromoteLocked(&PromoteJob{ID: "j2", App: "hello", From: "itest", State: jobRunning, Step: stepStarting, Backup: backup, Started: time.Now()})
	restarted.promoteMu.Unlock()
	newManager(h.cfg, h.srv.store, h.runner, h.edge).RecoverPromotes()
	job, _ = restarted.PromoteStatus("hello")
	if job.State != jobFailed || !strings.Contains(job.Error, "not checked healthy") {
		t.Fatalf("job after a restart while starting: %+v", job)
	}
}
