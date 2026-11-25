package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

// promoteBackupMark names the production data a promote replaced:
// <data root>/<app>--prod.before-promote-<UTC time>. The dot keeps it from
// ever being an instance's folder.
const promoteBackupMark = ".before-promote-"

// PromoteResult says what PromoteData did.
type PromoteResult struct {
	From   string `json:"from"`
	Backup string `json:"backup"` // where production's previous data now is ("" if it had none)
}

// PromoteData replaces production's data with a preview's, secrets included
// (CONTRACT.md, "Promote data"). Both instances are stopped so the copy is
// consistent; production's previous data stays beside the new one as a
// dated backup and is put back when production fails its health check on
// the new data. The API layer allows it only from main, on purpose.
func (m *Manager) PromoteData(ctx context.Context, app, from string) (*PromoteResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if from == prodEnv || !validEnv(from) {
		return nil, &badRequest{"from must be a preview"}
	}
	prodID, prevID := instanceID(app, prodEnv), instanceID(app, from)
	prod, ok := m.store.Get(prodID)
	if !ok {
		return nil, &conflict{app + " has no production instance; ship it first. Nothing was changed."}
	}
	prev, ok := m.store.Get(prevID)
	if !ok {
		return nil, &conflict{fmt.Sprintf("%s has no preview %q. Nothing was changed.", app, from)}
	}
	if prod.Status != "running" || !m.healthyNow(ctx, prod) {
		return nil, &conflict{app + " in production is not healthy; nothing was changed"}
	}
	prodRoot, _ := m.runner.DataDir(prodID)
	prevRoot, _ := m.runner.DataDir(prevID)
	if info, err := os.Lstat(prevRoot); err != nil || !info.IsDir() {
		return nil, &conflict{fmt.Sprintf("preview %q of %s has no data; nothing was changed", from, app)}
	}
	if diskNearlyFull(m.runner) {
		return nil, &conflict{"the server's disk is nearly full; nothing was changed"}
	}
	backup := prodRoot + promoteBackupMark + m.now().UTC().Format("20060102T150405Z")
	if _, err := os.Lstat(backup); err == nil {
		return nil, &conflict{"a backup with this time's name already exists; try again in a few seconds"}
	}

	m.promoteStep(app, stepStopping, "")
	log.Printf("promote-data: %s: replacing production's data with preview %s's (secrets included)", app, from)
	startPreview := func() {
		if prev.Status != "running" {
			return
		}
		if err := m.runner.Start(ctx, m.spec(prev)); err != nil {
			log.Printf("promote-data: %s: starting the preview again: %v", prevID, err)
		} else if !m.waitHealthy(ctx, prev) {
			log.Printf("promote-data: %s: the preview started again but is not healthy", prevID)
		}
	}
	startProd := func() bool {
		return m.runner.Start(ctx, m.spec(prod)) == nil && m.waitHealthy(ctx, prod)
	}
	if err := m.runner.Stop(ctx, prevID); err != nil {
		startPreview()
		return nil, fmt.Errorf("stopping the preview: %w; nothing was changed", err)
	}
	if err := m.runner.Stop(ctx, prodID); err != nil {
		startProd()
		startPreview()
		return nil, fmt.Errorf("stopping production: %w; nothing was changed", err)
	}

	// Keep production's data: the rename is atomic and never touches its
	// files. The step is recorded first, so a labd that restarts in the
	// middle knows the backup may exist (recoverPromote).
	m.promoteStep(app, stepCopying, backup)
	switch err := os.Rename(prodRoot, backup); {
	case errors.Is(err, fs.ErrNotExist):
		backup = "" // production never wrote anything
		m.promoteStep(app, stepCopying, "")
	case err != nil:
		startProd()
		startPreview()
		return nil, fmt.Errorf("keeping production's data: %w; nothing was changed", err)
	default:
		syncDir(filepath.Dir(prodRoot))
		log.Printf("promote-data: %s: production's data kept as %s", app, backup)
	}
	putBack := func() error {
		if err := os.RemoveAll(prodRoot); err != nil { // the preview still has this copy
			return err
		}
		if backup == "" {
			return nil
		}
		if err := os.Rename(backup, prodRoot); err != nil {
			return err
		}
		syncDir(filepath.Dir(prodRoot))
		return nil
	}

	if err := m.runner.CopyAllData(ctx, prevRoot, prodRoot); err != nil {
		log.Printf("promote-data: %s: copying failed: %v", app, err)
		m.promoteStep(app, stepPuttingBack, backup)
		if perr := putBack(); perr != nil {
			log.Printf("promote-data: %s: putting production's data back: %v", app, perr)
			return nil, fmt.Errorf("copying the preview's data failed (%v), and putting production's data back failed too (%v); it is in %s", err, perr, backup)
		}
		healthy := startProd()
		startPreview()
		msg := "copying the preview's data failed: " + err.Error() + "; production keeps its own data"
		if !healthy {
			msg += ", but it did not pass its health check"
		}
		return nil, errors.New(msg)
	}
	m.promoteStep(app, stepStarting, backup)
	log.Printf("promote-data: %s: copied preview %s's data into production; starting it", app, from)

	if startProd() {
		startPreview()
		log.Printf("promote-data: %s: production is healthy on the promoted data", app)
		return &PromoteResult{From: from, Backup: backup}, nil
	}

	// Unhealthy on the new data: production's own data goes back.
	logs := m.logsFor(ctx, prodID)
	log.Printf("promote-data: %s: production failed its health check on the promoted data; putting its data back", app)
	_ = m.runner.Stop(ctx, prodID)
	m.promoteStep(app, stepPuttingBack, backup)
	if err := putBack(); err != nil {
		startPreview()
		return nil, &DeployError{Msg: fmt.Sprintf("production failed its health check on the preview's data, and putting its own data back failed (%v); it is in %s", err, backup), Logs: logs}
	}
	healthy := startProd()
	startPreview()
	msg := "production failed its health check on the preview's data; its own data was put back and it is healthy again"
	if !healthy {
		msg = "production failed its health check on the preview's data; its own data was put back, but it is not healthy either"
	}
	log.Printf("promote-data: %s: %s", app, msg)
	return nil, &DeployError{Msg: msg, RolledBack: true, Logs: logs}
}

// healthyNow asks the instance's health path once.
func (m *Manager) healthyNow(ctx context.Context, inst Instance) bool {
	for range 3 {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://127.0.0.1:"+strconv.Itoa(inst.Port)+inst.Health, nil)
		if resp, err := m.client.Do(req); err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return true
			}
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(time.Second):
		}
	}
	return false
}

// promoteBackups lists the backups promote-data kept of an instance's data.
func promoteBackups(root string) []string {
	matches, _ := filepath.Glob(root + promoteBackupMark + "*")
	return matches
}
