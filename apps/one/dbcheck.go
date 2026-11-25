package main

import (
	"context"
	"errors"
	"time"

	"lab/apps/one/repo"
)

// Database health. SQLite's PRAGMA quick_check runs once at start (a database
// that fails it is not served) and once a day. A failed daily check is logged
// at error, which alerts the owner, and makes /healthz fail until a later
// check passes. The check runs on its own read-only connection and is cut off
// after a bound, so a large database cannot hold requests back.
const (
	startCheckLimit = 30 * time.Second
	dailyCheckLimit = 2 * time.Minute
	dailyCheckEvery = 24 * time.Hour
)

// checkDatabase runs the check once within limit and says whether the
// database is sound. A check that ran out of time says nothing about the
// database: it is logged as a warning and the verdict stays as it was.
func (a *app) checkDatabase(ctx context.Context, limit time.Duration, when string) (sound bool) {
	cctx, cancel := context.WithTimeout(ctx, limit)
	defer cancel()
	began := time.Now()
	err := a.quickCheck(cctx)
	took := time.Since(began).Milliseconds()
	switch {
	case err == nil:
		a.dbBad.Store(false)
		a.logger().Info("database check passed", "when", when, "ms", took)
		return true
	case errors.Is(err, repo.ErrCheckTimeout):
		a.logger().Warn("database check did not finish in time", "when", when, "ms", took)
		return !a.dbBad.Load()
	case ctx.Err() != nil:
		return !a.dbBad.Load() // the app is stopping
	}
	a.dbBad.Store(true)
	a.logger().Error("database check failed", "when", when, "ms", took, "reason", err.Error())
	return false
}

// dailyCheckLoop is the background check, once a day after the one at start.
func (a *app) dailyCheckLoop(ctx context.Context) {
	t := time.NewTicker(dailyCheckEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			a.runUnit("daily database check", func() { a.checkDatabase(ctx, dailyCheckLimit, "daily") })
		}
	}
}
