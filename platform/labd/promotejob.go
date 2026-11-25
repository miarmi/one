package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// promote-data runs as a job (CONTRACT.md, "Promote data"): the request
// starts it and answers at once, the workflow polls its status until it is
// done or failed. The job's state is a file, written before each step, so a
// labd that restarts in the middle can say (and, where it is clear, undo)
// what happened instead of leaving the workflow waiting or guessing.

// Job states and steps.
const (
	jobRunning = "running"
	jobDone    = "done"
	jobFailed  = "failed"

	stepChecking    = "checking"     // nothing changed yet
	stepStopping    = "stopping"     // stopping both instances; data untouched
	stepCopying     = "copying"      // production's data moved to the backup (or about to be); copy under way
	stepStarting    = "starting"     // the copy is complete; production starting on it
	stepPuttingBack = "putting-back" // production's own data going back
)

// PromoteJob is the state of an app's latest promote-data job.
type PromoteJob struct {
	ID         string    `json:"id"`
	App        string    `json:"app"`
	From       string    `json:"from"`
	State      string    `json:"state"`
	Step       string    `json:"step"`
	Backup     string    `json:"backup"` // where production's previous data is; "" when there is none (any more)
	Error      string    `json:"error,omitempty"`
	RolledBack bool      `json:"rolled_back,omitempty"`
	Logs       string    `json:"logs,omitempty"` // the app's last log lines after a failed health check
	Started    time.Time `json:"started"`
	Finished   time.Time `json:"finished,omitzero"`
}

func (m *Manager) promoteFile(app string) string {
	return filepath.Join(m.cfg.StateDir, "promote", app+".json")
}

// savePromoteLocked writes the job's state; m.promoteMu is held.
func (m *Manager) savePromoteLocked(job *PromoteJob) {
	raw, _ := json.MarshalIndent(job, "", "  ")
	err := os.MkdirAll(filepath.Dir(m.promoteFile(job.App)), 0o700)
	if err == nil {
		err = writeFileAtomic(m.promoteFile(job.App), append(raw, '\n'), 0o600)
	}
	if err != nil {
		log.Printf("promote-data: %s: saving the job's state: %v", job.App, err)
	}
}

// promoteBusy refuses changes to an app while its data is being promoted.
func (m *Manager) promoteBusy(app string) error {
	m.promoteMu.Lock()
	defer m.promoteMu.Unlock()
	if job := m.promoting[app]; job != nil {
		return &conflict{fmt.Sprintf("promote-data is moving %s's data right now (step %s); try again when it has finished", app, job.Step)}
	}
	return nil
}

// promoteStep records the step a running job is at, before it is taken.
func (m *Manager) promoteStep(app, step, backup string) {
	m.promoteMu.Lock()
	defer m.promoteMu.Unlock()
	job := m.promoting[app]
	if job == nil {
		return // PromoteData called directly (tests)
	}
	job.Step = step
	if step != stepStopping {
		job.Backup = backup
	}
	m.savePromoteLocked(job)
}

// StartPromote starts a promote-data job and returns at once. A second job
// for the same app is refused while one runs.
func (m *Manager) StartPromote(app, from string) (*PromoteJob, error) {
	if from == prodEnv || !validEnv(from) {
		return nil, &badRequest{"from must be a preview"}
	}
	m.promoteMu.Lock()
	defer m.promoteMu.Unlock()
	if job := m.promoting[app]; job != nil {
		return nil, &conflict{fmt.Sprintf("a promote-data job for %s is already running (started %s, step %s); wait for it to finish", app, job.Started.UTC().Format(time.RFC3339), job.Step)}
	}
	if _, ok := m.store.Get(instanceID(app, prodEnv)); !ok {
		return nil, &conflict{app + " has no production instance; ship it first. Nothing was changed."}
	}
	if _, ok := m.store.Get(instanceID(app, from)); !ok {
		return nil, &conflict{fmt.Sprintf("%s has no preview %q. Nothing was changed.", app, from)}
	}
	id := make([]byte, 4)
	rand.Read(id)
	job := &PromoteJob{
		ID: m.now().UTC().Format("20060102T150405Z") + "-" + hex.EncodeToString(id), App: app, From: from,
		State: jobRunning, Step: stepChecking, Started: m.now().UTC(),
	}
	if m.promoting == nil {
		m.promoting = map[string]*PromoteJob{}
	}
	m.promoting[app] = job
	m.savePromoteLocked(job)
	started := *job
	go m.runPromote(job)
	return &started, nil
}

func (m *Manager) runPromote(job *PromoteJob) {
	res, err := m.PromoteData(context.Background(), job.App, job.From)
	m.promoteMu.Lock()
	defer m.promoteMu.Unlock()
	job.Finished = m.now().UTC()
	if err == nil {
		job.State, job.Backup = jobDone, res.Backup
	} else {
		job.State, job.Error = jobFailed, err.Error()
		var de *DeployError
		if errors.As(err, &de) {
			job.RolledBack, job.Logs = de.RolledBack, de.Logs
		}
		if job.Backup != "" {
			if _, serr := os.Lstat(job.Backup); serr != nil {
				job.Backup = "" // put back where it came from
			}
		}
	}
	m.savePromoteLocked(job)
	delete(m.promoting, job.App)
	log.Printf("promote-data: %s: job %s %s", job.App, job.ID, job.State)
}

// PromoteStatus returns the app's running or latest promote-data job.
func (m *Manager) PromoteStatus(app string) (*PromoteJob, error) {
	m.promoteMu.Lock()
	defer m.promoteMu.Unlock()
	if job := m.promoting[app]; job != nil {
		c := *job
		return &c, nil
	}
	raw, err := os.ReadFile(m.promoteFile(app))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, errNotFound
	}
	if err != nil {
		return nil, err
	}
	var job PromoteJob
	if err := json.Unmarshal(raw, &job); err != nil {
		return nil, err
	}
	return &job, nil
}

// RecoverPromotes runs once at start, before anything (re)starts apps. A job
// still marked running was cut short by a labd restart: it is marked failed
// with what state the data is in, and where that is clear (the copy was
// under way, or the old data was going back) production's own data is put
// back. Reconcile then starts both instances as usual.
func (m *Manager) RecoverPromotes() {
	files, _ := filepath.Glob(filepath.Join(m.cfg.StateDir, "promote", "*.json"))
	for _, f := range files {
		raw, err := os.ReadFile(f)
		var job PromoteJob
		if err != nil || json.Unmarshal(raw, &job) != nil || job.State != jobRunning || !validAppName(job.App) {
			continue
		}
		job.State, job.Finished = jobFailed, m.now().UTC()
		job.Error = "labd restarted during the job (step " + job.Step + "): " + m.recoverPromote(&job)
		m.promoteMu.Lock()
		m.savePromoteLocked(&job)
		m.promoteMu.Unlock()
		log.Printf("promote-data: %s: %s", job.App, job.Error)
	}
}

func (m *Manager) recoverPromote(job *PromoteJob) string {
	prodRoot, _ := m.runner.DataDir(instanceID(job.App, prodEnv))
	backupThere := false
	if job.Backup != "" && strings.HasPrefix(job.Backup, prodRoot+promoteBackupMark) {
		_, err := os.Lstat(job.Backup)
		backupThere = err == nil
	}
	putBack := func() string {
		if err := os.RemoveAll(prodRoot); err != nil { // the partial or unverified copy; the preview has it
			return fmt.Sprintf("removing the copy failed (%v); production's own data is in %s", err, job.Backup)
		}
		if backupThere {
			if err := os.Rename(job.Backup, prodRoot); err != nil {
				return fmt.Sprintf("putting production's data back failed (%v); it is in %s", err, job.Backup)
			}
			syncDir(filepath.Dir(prodRoot))
		}
		job.Backup = ""
		return "production's own data was put back; nothing else changed"
	}
	switch job.Step {
	case stepCopying:
		if job.Backup != "" && !backupThere {
			job.Backup = ""
			return "production's data had not been moved yet; nothing was changed"
		}
		return putBack()
	case stepPuttingBack:
		if !backupThere {
			job.Backup = ""
			return "production's own data had already been put back"
		}
		return putBack()
	case stepStarting:
		return "the copy had finished and production was starting on the preview's data, but it was not checked healthy; production's previous data is kept in " + job.Backup + " (see CONTRACT.md, \"Promote data\", to restore it, or run promote-data again)"
	default:
		job.Backup = ""
		return "no data had been touched; nothing was changed"
	}
}
