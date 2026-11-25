// labd is the server program of the lab platform: it receives tested builds
// from this repo's GitHub Actions pipeline and runs them as sandboxed systemd
// units behind nginx. See platform/CONTRACT.md.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"
)

// version is set at build time with -ldflags "-X main.version=<sha7>".
var version = "dev"

func main() {
	configPath := flag.String("config", "/etc/lab/labd.json", "config file")
	showVersion := flag.Bool("version", false, "print the version and exit")
	selftest := flag.Bool("selftest", false, "check that labd can run with this config and exit")
	copyMode := flag.Bool("copy-data", false, "copy production data SRC into a new preview's DST and exit (used by labd itself)")
	copyAll := flag.Bool("all", false, "with -copy-data: copy a stopped preview SRC whole, secrets included, into production's DST (promote-data)")
	tryBind := flag.String("try-bind", "", "try to listen on ADDR and say whether that was allowed (used by labd's sandbox check)")
	sandboxCheck := flag.Bool("sandbox-check", false, "do what every app does first, in the app sandbox (used by labd itself)")
	flag.Parse()
	log.SetFlags(0)

	if *showVersion {
		fmt.Println("labd " + version)
		return
	}
	if *tryBind != "" {
		l, err := net.Listen("tcp", *tryBind)
		if err != nil {
			fmt.Println("LAB-BIND-REFUSED:", err)
			os.Exit(3)
		}
		l.Close()
		fmt.Println("bound")
		return
	}
	if *sandboxCheck {
		if err := checkSandbox(); err != nil {
			log.Fatalf("sandbox check: %v", err)
		}
		fmt.Println("LAB-SANDBOX-OK")
		return
	}
	if *copyMode {
		if flag.NArg() != 2 {
			log.Fatal("usage: labd -copy-data [-all] SRC DST")
		}
		if *copyAll {
			if err := copyAllData(context.Background(), flag.Arg(0), flag.Arg(1)); err != nil {
				log.Fatalf("copying the preview's data: %v", err)
			}
			return
		}
		if err := copyData(context.Background(), flag.Arg(0), flag.Arg(1)); err != nil {
			log.Fatalf("copying production data: %v", err)
		}
		return
	}
	cfg, err := loadConfig(*configPath)
	if err != nil {
		log.Fatalf("config: %v", err)
	}
	if *selftest {
		if err := selfTest(cfg); err != nil {
			fmt.Fprintln(os.Stderr, "selftest failed:", err)
			os.Exit(1)
		}
		fmt.Println("selftest ok")
		return
	}
	if err := serve(cfg); err != nil {
		log.Fatal(err)
	}
}

// selfTest checks the config, the state files and the tools this config needs.
func selfTest(cfg *Config) error {
	if err := os.MkdirAll(cfg.StateDir, 0o755); err != nil {
		return err
	}
	probe := filepath.Join(cfg.StateDir, ".selftest")
	if err := os.WriteFile(probe, []byte("ok"), 0o600); err != nil {
		return fmt.Errorf("state dir not writable: %w", err)
	}
	os.Remove(probe)
	if _, err := openStore(cfg.StateDir); err != nil {
		return err
	}
	tools := []string{"sqlite3"}
	if cfg.Runner == "systemd" {
		tools = append(tools, "systemd-run", "systemctl", "journalctl", "nft")
	}
	if cfg.Edge == "nginx" {
		tools = append(tools, "nginx", "systemctl")
	}
	for _, t := range tools {
		if _, err := exec.LookPath(t); err != nil {
			return fmt.Errorf("missing tool %s", t)
		}
	}
	return nil
}

// checkSandbox does what every app does first: listen on its port and write
// and read its data folder.
func checkSandbox() error {
	l, err := net.Listen("tcp", "127.0.0.1:"+os.Getenv("PORT"))
	if err != nil {
		return err
	}
	l.Close()
	p := filepath.Join(os.Getenv("DATA_DIR"), "sandbox-check")
	if err := os.WriteFile(p, []byte("ok"), 0o600); err != nil {
		return err
	}
	b, err := os.ReadFile(p)
	if err != nil || string(b) != "ok" {
		return fmt.Errorf("reading the data folder back: %q %v", b, err)
	}
	return os.Remove(p)
}

func buildRunner(cfg *Config) Runner {
	if cfg.Runner == "exec" {
		return newExecRunner(cfg.StateDir)
	}
	return newSystemdRunner(cfg.paths.dataRoot, cfg.paths.appDataRoot, cfg.StateDir, cfg.paths.binary)
}

func buildEdge(cfg *Config) Edge {
	if cfg.Edge == "none" {
		return &noneEdge{}
	}
	return &nginxEdge{sitePath: cfg.paths.nginxSite, commonPath: cfg.paths.nginxCommon, liveURL: "http://127.0.0.1/_lab/edge"}
}

// buildCerts gives domain mode its certificates: Let's Encrypt on the server,
// self-made ones in tests.
func buildCerts(cfg *Config) Certs {
	if cfg.Certs == "selfsigned" {
		return &selfsignedCerts{dir: filepath.Join(cfg.StateDir, "certs")}
	}
	return newCertbotCerts(cfg.paths.webroot, cfg.paths.certLive)
}

func newServer(cfg *Config, store *Store, runner Runner, edge Edge) *Server {
	v := newVerifier(cfg.OIDCIssuer, cfg.OIDCJWKSURL, cfg.Repo)
	v.statePath = filepath.Join(cfg.StateDir, "tokens.json")
	v.load()
	return &Server{
		cfg:          cfg,
		verifier:     v,
		mgr:          newManager(cfg, store, runner, edge),
		store:        store,
		runner:       runner,
		sessions:     newSessions(cfg.StateDir),
		auth:         newAuth(cfg.StateDir),
		limiter:      newLimiter(),
		setupLimiter: newLimiter(),
		cop:          http.NewCrossOriginProtection(),
		host:         newHostEnv(cfg.paths),
		notifier:     newNotifier(cfg, store, runner, version),
		version:      version,
		uploads:      make(chan struct{}, 4),
		reboot:       func() { _, _ = run(context.Background(), time.Minute, "systemctl", "reboot") },
	}
}

func serve(cfg *Config) error {
	store, err := openStore(cfg.StateDir)
	if err != nil {
		return err
	}
	runner := buildRunner(cfg)
	srv := newServer(cfg, store, runner, buildEdge(cfg))
	srv.exit = func() { os.Exit(0) } // systemd restarts us with the new binary
	// Before anything restarts apps: a promote cut short is undone or described.
	srv.mgr.RecoverPromotes()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	httpSrv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           srv.routes(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    64 << 10,
	}
	errc := make(chan error, 1)
	go func() { errc <- httpSrv.ListenAndServe() }()
	log.Printf("labd %s listening on %s (runner %s, edge %s)", version, cfg.Listen, cfg.Runner, cfg.Edge)

	go func() {
		if cfg.Runner == "systemd" {
			hardenHost(ctx, srv.host)
			ensureAppFirewall(ctx, store.Settings(), cfg.paths.appFirewall)
			srv.mgr.RefreshSandbox(ctx, filepath.Join(cfg.StateDir, "sandbox"))
		}
		srv.mgr.Reconcile(ctx)
		setSettled()
		srv.mgr.EnsureCerts(ctx)
		t := time.NewTicker(10 * time.Minute)
		defer t.Stop()
		outbox := time.NewTicker(15 * time.Second)
		defer outbox.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-outbox.C:
				srv.notifier.scanOutboxes(ctx)
			case <-t.C:
				if cfg.Runner == "systemd" {
					ensureAppFirewall(ctx, store.Settings(), cfg.paths.appFirewall) // a reloaded firewall drops it
				}
				srv.mgr.Reconcile(ctx)
				srv.mgr.EnsureCerts(ctx)
				srv.notifier.watchHealth(ctx)
				srv.notifier.watchErrors(ctx)
				srv.notifier.heartbeat(ctx)
			}
		}
	}()

	select {
	case err := <-errc:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return httpSrv.Shutdown(shutdownCtx)
	}
}
