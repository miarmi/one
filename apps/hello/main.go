// hello is a tiny notes app that proves the lab pipeline works end to end.
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
	st, err := openStore(filepath.Join(dataDir, "hello.db"))
	if err != nil {
		appkit.Error("opening database", err)
		os.Exit(1)
	}
	defer st.Close()

	app := newApp(st, os.Getenv("APP_ENV"), os.Getenv("APP_VERSION"))
	srv := &http.Server{
		Addr:              "127.0.0.1:" + port,
		Handler:           app.routes(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	logger.Info("hello listening", "addr", srv.Addr)

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
		logger.Info("hello stopped")
	}
}
