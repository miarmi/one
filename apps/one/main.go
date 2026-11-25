// one is the trial slice of ONE: it connects Gmail and Office mail from the
// server and checks the disk, so three trials of the design can be run.
package main

import (
	"context"
	"errors"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"lab/platform/appkit"
)

func main() {
	// The app keeps its own log lines for agents (SPEC 29, get_log); this must
	// be set before the first line is logged.
	if d := os.Getenv("DATA_DIR"); d != "" {
		appkit.AlsoLogTo(newLogKeeper(logDir(d)))
	}
	logger := appkit.Logger()
	port := os.Getenv("PORT")
	if port == "" {
		logger.Error("PORT is required")
		os.Exit(1)
	}
	dataDir := os.Getenv("DATA_DIR")
	if dataDir == "" {
		logger.Error("DATA_DIR is required")
		os.Exit(1)
	}
	st, err := openStore(filepath.Join(dataDir, "one.db"))
	if err != nil {
		appkit.Error("opening database", err)
		os.Exit(1)
	}
	defer st.Close()
	// Names imported before quoted encoded words were decoded.
	if n, err := st.RepairNames(context.Background(), decodeName); err != nil {
		appkit.Error("repairing sender names", err)
	} else if n > 0 {
		appkit.Logger().Info("sender names repaired", "count", n)
	}

	app := newApp(st, dataDir, os.Getenv("APP_ENV"), os.Getenv("APP_VERSION"))
	// A database that fails SQLite's quick check is not served.
	if !app.checkDatabase(context.Background(), startCheckLimit, "start") {
		logger.Error("refusing to serve: the database failed its check at start")
		os.Exit(1)
	}
	// A reply that was being sent when the app last stopped is never sent again.
	app.recoverSends(context.Background())
	srv := &http.Server{
		Addr:              "127.0.0.1:" + port,
		Handler:           app.routes(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      70 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	loopDone := make(chan struct{})
	go func() { app.imp.Loop(ctx); close(loopDone) }()
	// The app's own timer for what needs no request: closing quiet Resolved
	// items and watching the sources (SPEC 3, 24).
	go app.housekeeping(ctx)
	// The review by the app's own model, when a key is saved (SPEC 31).
	go app.reviewLoop(ctx)
	// The database check, once a day.
	go app.dailyCheckLoop(ctx)
	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	logger.Info("one listening", "addr", srv.Addr)

	select {
	case err := <-errc:
		if !errors.Is(err, http.ErrServerClosed) {
			appkit.Error("serving", err)
			os.Exit(1)
		}
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 9*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdown); err != nil {
			appkit.Error("shutdown", err)
		}
		select {
		case <-loopDone:
		case <-shutdown.Done():
		}
		logger.Info("one stopped")
	}
}
