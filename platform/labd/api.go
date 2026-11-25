package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var commitRe = regexp.MustCompile(`^[0-9a-f]{7,40}$`)

const smallBody = 64 << 10

// Server is labd's HTTP API.
type Server struct {
	cfg          *Config
	verifier     *Verifier
	mgr          *Manager
	store        *Store
	runner       Runner
	sessions     *Sessions
	auth         *Auth
	limiter      *limiter                    // sign-in tries
	setupLimiter *limiter                    // setup-code tries, counted apart so setting up never locks out signing in
	cop          *http.CrossOriginProtection // labd's own forms refuse posts from other sites
	host         *hostEnv
	notifier     *notifier   // the way to the owner's phone (notify.go)
	errors       errorsCache // the Errors page's data (logs.go)
	version      string
	exit         func()        // after a platform install; os.Exit(0) on the server
	reboot       func()        // POST /api/host/reboot; systemctl reboot on the server
	uploads      chan struct{} // bounds large uploads arriving at once
}

func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.healthz)
	mux.HandleFunc("POST /api/platform", s.platform)
	mux.HandleFunc("POST /api/settings", s.settings)
	mux.HandleFunc("POST /api/deploy", s.deploy)
	mux.HandleFunc("POST /api/rollback", s.rollback)
	mux.HandleFunc("DELETE /api/instances", s.deleteInstance)
	mux.HandleFunc("POST /api/gc", s.gc)
	mux.HandleFunc("POST /api/promote-data", s.promoteData)
	mux.HandleFunc("GET /api/promote-data", s.promoteStatus)
	mux.HandleFunc("GET /api/status", s.status)
	mux.HandleFunc("GET /api/logs", s.logs)
	mux.HandleFunc("GET /api/host", s.hostInfo)
	mux.HandleFunc("GET /api/host/audit", s.hostAudit)
	mux.HandleFunc("POST /api/host/upgrade", s.hostUpgrade)
	mux.HandleFunc("POST /api/host/reboot", s.hostReboot)
	mux.HandleFunc("POST /api/notify", s.notify)
	mux.HandleFunc("POST /api/session", s.session)
	mux.HandleFunc("POST /api/setup-code", s.setupCode)
	mux.HandleFunc("GET /_lab/auth", s.labAuth)
	mux.HandleFunc("/_lab/redirect", s.loginRedirect)
	mux.HandleFunc("GET /_lab/login", s.page(s.loginPage))
	mux.HandleFunc("POST /_lab/login", s.page(s.login))
	mux.HandleFunc("POST /_lab/logout", s.page(s.logout))
	mux.HandleFunc("GET /_lab/setup", s.page(s.setupPage))
	mux.HandleFunc("POST /_lab/setup", s.page(s.setup))
	mux.HandleFunc("GET /_lab/lab.css", s.page(s.css))
	mux.HandleFunc("GET /_lab/icon.png", s.page(s.icon))
	mux.HandleFunc("GET /_lab/manifest.webmanifest", s.page(s.manifest))
	mux.HandleFunc("GET /_lab/errors", s.page(s.errorsPage))
	mux.HandleFunc("GET /{$}", s.page(s.home))
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeErr(w, http.StatusNotFound, "not found")
	})
	return recoverer(logRequests(mux))
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]any{"ok": false, "error": msg})
}

func recoverer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if v := recover(); v != nil {
				if v == http.ErrAbortHandler {
					panic(v)
				}
				log.Printf("panic serving %s %s: %v", r.Method, r.URL.Path, v)
				writeErr(w, http.StatusInternalServerError, "internal error")
			}
		}()
		next.ServeHTTP(w, r)
	})
}

type statusRecorder struct {
	http.ResponseWriter
	code int
}

func (s *statusRecorder) WriteHeader(code int) {
	s.code = code
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, code: http.StatusOK}
		next.ServeHTTP(rec, r)
		if r.URL.Path != "/healthz" && r.URL.Path != "/_lab/auth" {
			log.Printf("%s %s %d %s", r.Method, r.URL.Path, rec.code, time.Since(start).Round(time.Millisecond))
		}
	})
}

// detached gives work that changes the server a context that outlives the
// request: when a pipeline run is cancelled mid-deploy, the deploy still
// finishes or rolls back cleanly instead of stopping halfway.
func detached(r *http.Request) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(r.Context()), 10*time.Minute)
}

func (s *Server) healthz(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "stage": "labd", "version": s.version})
}

// fromLocalProxy reports whether the request came from nginx on this host.
func fromLocalProxy(r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return false
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func (s *Server) overTLS(r *http.Request) bool {
	return fromLocalProxy(r) && r.Header.Get("X-Lab-Scheme") == "https"
}

// Authorization rules (CONTRACT.md, "Authorization by the token's ref").

func mainOnly(c *Claims) error {
	if c.Ref != "refs/heads/main" {
		return errors.New("only the main branch may do this")
	}
	return nil
}

func anyRef(*Claims) error { return nil }

func envRule(env string) func(*Claims) error {
	return func(c *Claims) error {
		if env == prodEnv {
			return mainOnly(c)
		}
		branch := branchOf(c.Ref)
		if branch == "" || branch == "main" {
			return errors.New("a preview can only be changed from its own branch")
		}
		name, err := previewName(branch)
		if err != nil || name != env {
			return fmt.Errorf("this branch may not change preview %q", env)
		}
		return nil
	}
}

// deleteRule: previews may be deleted from main or from their own branch.
// Production may be retired only from main, and only when the request
// repeats the app's name in `retire=<app>` so a stray call cannot do it.
func deleteRule(env, app, retire string) func(*Claims) error {
	return func(c *Claims) error {
		if env == prodEnv {
			if c.Ref != "refs/heads/main" {
				return errors.New("production can only be retired from main")
			}
			if retire != app {
				return errors.New("retiring production needs retire=<app>")
			}
			return nil
		}
		if c.Ref == "refs/heads/main" {
			return nil
		}
		return envRule(env)(c)
	}
}

// promoteRule: a preview's data may replace production's only from main,
// and only when the request repeats the app's name in `confirm=<app>`.
func promoteRule(app, confirm string) func(*Claims) error {
	return func(c *Claims) error {
		if err := mainOnly(c); err != nil {
			return err
		}
		if confirm != app {
			return errors.New("promoting data needs confirm=<app>")
		}
		return nil
	}
}

// body is an authenticated request body: in memory, or in a temp file.
type body struct {
	data []byte
	path string
}

func (b *body) cleanup() {
	if b.path != "" {
		os.Remove(b.path)
	}
}

type audKind int

const (
	audAPI audKind = iota
	audPlatform
)

// authenticate checks the OIDC token, the authorization rule, reads the body
// (into fileDir when set), checks the token is bound to exactly this request,
// and burns the token. It writes the error response itself and returns nil
// on failure.
func (s *Server) authenticate(w http.ResponseWriter, r *http.Request, kind audKind, limit int64, fileDir string, allow func(*Claims) error) (*Claims, *body) {
	auth := r.Header.Get("Authorization")
	if !strings.HasPrefix(auth, "Bearer ") {
		writeErr(w, http.StatusUnauthorized, "missing bearer token")
		return nil, nil
	}
	claims, err := s.verifier.Verify(strings.TrimSpace(auth[len("Bearer "):]))
	if err != nil {
		writeErr(w, http.StatusUnauthorized, err.Error())
		return nil, nil
	}
	if err := allow(claims); err != nil {
		writeErr(w, http.StatusForbidden, err.Error())
		return nil, nil
	}
	if r.ContentLength > limit {
		writeErr(w, http.StatusRequestEntityTooLarge, "request body too large")
		return nil, nil
	}
	b := &body{}
	var sum string
	if fileDir != "" {
		// Large uploads land on disk before the token is checked against
		// them, so only a few may arrive at once.
		select {
		case s.uploads <- struct{}{}:
		default:
			writeErr(w, http.StatusTooManyRequests, "too many uploads at once; try again shortly")
			return nil, nil
		}
		path, digest, err := receiveFile(r.Body, fileDir, limit)
		<-s.uploads
		if errors.Is(err, errTooLarge) {
			writeErr(w, http.StatusRequestEntityTooLarge, "request body too large")
			return nil, nil
		}
		if err != nil {
			writeErr(w, http.StatusBadRequest, "could not read the request body")
			return nil, nil
		}
		b.path, sum = path, digest
	} else {
		data, err := io.ReadAll(io.LimitReader(r.Body, limit+1))
		if err != nil {
			writeErr(w, http.StatusBadRequest, "could not read the request body")
			return nil, nil
		}
		if int64(len(data)) > limit {
			writeErr(w, http.StatusRequestEntityTooLarge, "request body too large")
			return nil, nil
		}
		d := sha256.Sum256(data)
		b.data, sum = data, hex.EncodeToString(d[:])
	}
	want := apiAudience(r.Method, r.RequestURI, sum)
	if kind == audPlatform {
		want = platformAudience(sum)
	}
	if !claims.hasAudience(want) {
		b.cleanup()
		writeErr(w, http.StatusUnauthorized, "token is not bound to this request")
		return nil, nil
	}
	if err := s.verifier.Consume(claims); err != nil {
		b.cleanup()
		writeErr(w, http.StatusUnauthorized, err.Error())
		return nil, nil
	}
	return claims, b
}

// appEnv reads and validates ?app=&env=.
func appEnv(w http.ResponseWriter, r *http.Request) (string, string, bool) {
	q := r.URL.Query()
	app, env := q.Get("app"), q.Get("env")
	if !validAppName(app) {
		writeErr(w, http.StatusBadRequest, "invalid app name")
		return "", "", false
	}
	if !validEnv(env) {
		writeErr(w, http.StatusBadRequest, "invalid env")
		return "", "", false
	}
	return app, env, true
}

func (s *Server) tmpDir() (string, error) {
	dir := filepath.Join(s.cfg.StateDir, "tmp")
	return dir, os.MkdirAll(dir, 0o700)
}

func (s *Server) platform(w http.ResponseWriter, r *http.Request) {
	_, b := s.authenticate(w, r, audPlatform, maxUpload, filepath.Dir(s.cfg.paths.binary), mainOnly)
	if b == nil {
		return
	}
	defer b.cleanup()
	ctx, cancel := detached(r)
	defer cancel()
	out, err := installBinary(ctx, s.cfg, b.path)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	b.path = "" // renamed into place
	log.Printf("installed %s; restarting", out)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "installed": out})
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	if s.exit != nil {
		time.AfterFunc(500*time.Millisecond, s.exit)
	}
}

func (s *Server) settings(w http.ResponseWriter, r *http.Request) {
	_, b := s.authenticate(w, r, audAPI, smallBody, "", mainOnly)
	if b == nil {
		return
	}
	var set Settings
	dec := json.NewDecoder(bytes.NewReader(b.data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&set); err != nil {
		writeErr(w, http.StatusBadRequest, "settings must be {\"domain\": ..., \"public_ip\": ...}")
		return
	}
	set.Domain = strings.TrimSuffix(strings.ToLower(set.Domain), ".")
	if set.Domain != "" && !validDomain(set.Domain) {
		writeErr(w, http.StatusBadRequest, "domain must be a DNS name such as lab.example.com (the lab answers on it, apps on names below it)")
		return
	}
	if !validPublicIPv4(set.PublicIP) {
		writeErr(w, http.StatusBadRequest, "public_ip must be an IPv4 address")
		return
	}
	if len(set.AllowIPs) > 50 {
		writeErr(w, http.StatusBadRequest, "allow_ips may hold at most 50 entries")
		return
	}
	for i, a := range set.AllowIPs {
		norm, ok := normalizeAllowIP(a)
		if !ok {
			writeErr(w, http.StatusBadRequest, fmt.Sprintf("allow_ips: %q is not an IP address or range", a))
			return
		}
		set.AllowIPs[i] = norm
	}
	ctx, cancel := detached(r)
	defer cancel()
	if err := s.mgr.ApplySettings(ctx, set); err != nil {
		var ce *certError
		if errors.As(err, &ce) {
			writeErr(w, http.StatusConflict, ce.msg)
			return
		}
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if s.cfg.Runner == "systemd" {
		ensureAppFirewall(ctx, set, s.cfg.paths.appFirewall) // apps may not reach the public address either
	}
	mode := "bootstrap"
	if set.Domain != "" {
		mode = "domain"
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "mode": mode, "base": baseDomain(set), "allow_ips": set.AllowIPs})
}

var sha256HexRe = regexp.MustCompile(`^[0-9a-f]{64}$`)

// setupCode stores a one-time setup code for the login gate, by its SHA-256
// (CONTRACT.md, POST /api/setup-code): whoever made the code hands it to
// The owner; the code itself never passes through GitHub or this API.
func (s *Server) setupCode(w http.ResponseWriter, r *http.Request) {
	reset := r.URL.Query().Get("reset") == "1"
	_, b := s.authenticate(w, r, audAPI, smallBody, "", mainOnly)
	if b == nil {
		return
	}
	var req struct {
		CodeSHA256 string `json:"code_sha256"`
	}
	dec := json.NewDecoder(bytes.NewReader(b.data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil || !sha256HexRe.MatchString(req.CodeSHA256) {
		writeErr(w, http.StatusBadRequest, "body must be {\"code_sha256\": \"<64 hex digits>\"}")
		return
	}
	exp, err := s.auth.SetSetupCode(req.CodeSHA256, reset)
	if errors.Is(err, errPasswordSet) {
		writeErr(w, http.StatusConflict, err.Error())
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	log.Printf("setup: a new setup code is valid until %s (reset: %v)", exp.Format(time.RFC3339), reset)
	out := map[string]any{"ok": true, "expires": exp.Unix()}
	if d := s.store.Settings().Domain; d != "" {
		out["url"] = "https://" + d + "/_lab/setup"
	}
	writeJSON(w, http.StatusOK, out)
}

// session gives the pipeline a short-lived pass through the access gate, so
// it can test a deployed instance (CONTRACT.md, POST /api/session).
func (s *Server) session(w http.ResponseWriter, r *http.Request) {
	app, env, ok := appEnv(w, r)
	if !ok {
		return
	}
	if s.store.Settings().Domain != "" && !s.overTLS(r) {
		writeErr(w, http.StatusForbidden, "sessions are only issued over HTTPS")
		return
	}
	if _, b := s.authenticate(w, r, audAPI, smallBody, "", envRule(env)); b == nil {
		return
	}
	if _, found := s.store.Get(instanceID(app, env)); !found {
		writeErr(w, http.StatusNotFound, errNotFound.Error())
		return
	}
	host := hostFor(app, env, s.store.Settings())
	if host == "" {
		writeErr(w, http.StatusConflict, "settings are not set yet; POST /api/settings first")
		return
	}
	cookie, exp, err := s.sessions.Issue(ciSessionTTL, host)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "cookie": cookie, "expires": exp.Unix()})
}

// labAuth answers nginx's auth_request: 204 for a lab_session cookie that
// opens the host asked for (X-Lab-Host, set by nginx): a pipeline pass for
// that host, or in domain mode the owner's pass for the whole lab. Every
// lab_session cookie counts, so one a page planted can't hide the real one.
func (s *Server) labAuth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if !fromLocalProxy(r) {
		w.WriteHeader(http.StatusForbidden)
		return
	}
	host := r.Header.Get("X-Lab-Host")
	domain := s.store.Settings().Domain
	hasPassword, gen := s.auth.Status()
	for _, c := range r.Cookies() {
		if c.Name != sessionCookie {
			continue
		}
		if s.sessions.Valid(c.Value, host) || (domain != "" && hasPassword && s.sessions.ValidUser(c.Value, host, domain, gen)) {
			w.WriteHeader(http.StatusNoContent)
			return
		}
	}
	w.WriteHeader(http.StatusUnauthorized)
}

func (s *Server) deploy(w http.ResponseWriter, r *http.Request) {
	app, env, ok := appEnv(w, r)
	if !ok {
		return
	}
	commit := r.URL.Query().Get("commit")
	if commit != "" && !commitRe.MatchString(commit) {
		writeErr(w, http.StatusBadRequest, "invalid commit")
		return
	}
	if baseDomain(s.store.Settings()) == "" {
		writeErr(w, http.StatusConflict, "settings are not set yet; POST /api/settings first")
		return
	}
	dir, err := s.tmpDir()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	_, b := s.authenticate(w, r, audAPI, maxUpload, dir, envRule(env))
	if b == nil {
		return
	}
	defer b.cleanup()
	f, err := os.Open(b.path)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer f.Close()
	ctx, cancel := detached(r)
	defer cancel()
	res, err := s.mgr.Deploy(ctx, app, env, commit, f)
	var de *DeployError
	if errors.As(err, &de) {
		s.notifier.deployFailed(ctx, app, env, de, s.store.Settings())
	}
	s.writeResult(w, res, err)
}

func (s *Server) writeResult(w http.ResponseWriter, res *DeployResult, err error) {
	var br *badRequest
	var de *DeployError
	var cf *conflict
	switch {
	case err == nil:
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "url": res.URL, "release": res.Release, "previous": res.Previous})
	case errors.As(err, &br):
		writeErr(w, http.StatusBadRequest, br.msg)
	case errors.As(err, &de):
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"ok": false, "error": de.Msg, "rolled_back": de.RolledBack, "logs": de.Logs})
	case errors.As(err, &cf):
		writeErr(w, http.StatusConflict, cf.msg)
	case errors.Is(err, errNotFound):
		writeErr(w, http.StatusNotFound, err.Error())
	default:
		writeErr(w, http.StatusInternalServerError, err.Error())
	}
}

func (s *Server) rollback(w http.ResponseWriter, r *http.Request) {
	app, env, ok := appEnv(w, r)
	if !ok {
		return
	}
	if _, b := s.authenticate(w, r, audAPI, smallBody, "", envRule(env)); b == nil {
		return
	}
	ctx, cancel := detached(r)
	defer cancel()
	res, err := s.mgr.Rollback(ctx, app, env)
	s.writeResult(w, res, err)
}

func (s *Server) deleteInstance(w http.ResponseWriter, r *http.Request) {
	app, env, ok := appEnv(w, r)
	if !ok {
		return
	}
	retire := r.URL.Query().Get("retire")
	if env == prodEnv && retire != app {
		writeErr(w, http.StatusBadRequest, "retiring production needs retire=<app>")
		return
	}
	if _, b := s.authenticate(w, r, audAPI, smallBody, "", deleteRule(env, app, retire)); b == nil {
		return
	}
	ctx, cancel := detached(r)
	defer cancel()
	err := s.mgr.Delete(ctx, app, env)
	if err != nil {
		s.writeResult(w, nil, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// promoteData: POST /api/promote-data?app=<app>&env=<preview>&confirm=<app>
// starts a job that replaces production's data with the preview's, secrets
// included, and answers 202 at once; GET /api/promote-data says how it went.
func (s *Server) promoteData(w http.ResponseWriter, r *http.Request) {
	app, env, ok := appEnv(w, r)
	if !ok {
		return
	}
	if env == prodEnv {
		writeErr(w, http.StatusBadRequest, "env must be the preview whose data goes to production")
		return
	}
	confirm := r.URL.Query().Get("confirm")
	if confirm != app {
		writeErr(w, http.StatusBadRequest, "promoting data needs confirm=<app>")
		return
	}
	if _, b := s.authenticate(w, r, audAPI, smallBody, "", promoteRule(app, confirm)); b == nil {
		return
	}
	job, err := s.mgr.StartPromote(app, env)
	if err != nil {
		s.writeResult(w, nil, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"ok": true, "job": job})
}

// promoteStatus: GET /api/promote-data?app=<app> → the app's running or
// latest promote-data job.
func (s *Server) promoteStatus(w http.ResponseWriter, r *http.Request) {
	app := r.URL.Query().Get("app")
	if !validAppName(app) {
		writeErr(w, http.StatusBadRequest, "invalid app name")
		return
	}
	if _, b := s.authenticate(w, r, audAPI, smallBody, "", mainOnly); b == nil {
		return
	}
	job, err := s.mgr.PromoteStatus(app)
	if err != nil {
		s.writeResult(w, nil, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "job": job})
}

func (s *Server) gc(w http.ResponseWriter, r *http.Request) {
	_, b := s.authenticate(w, r, audAPI, smallBody, "", mainOnly)
	if b == nil {
		return
	}
	var req struct {
		Previews []string `json:"previews"`
	}
	dec := json.NewDecoder(bytes.NewReader(b.data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil || req.Previews == nil {
		writeErr(w, http.StatusBadRequest, "body must be {\"previews\": [...]}")
		return
	}
	for _, p := range req.Previews {
		if p == prodEnv || !validEnv(p) {
			writeErr(w, http.StatusBadRequest, fmt.Sprintf("invalid preview name %q", p))
			return
		}
	}
	ctx, cancel := detached(r)
	defer cancel()
	removed, err := s.mgr.GC(ctx, req.Previews)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if removed == nil {
		removed = []string{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "removed": removed})
}

func (s *Server) status(w http.ResponseWriter, r *http.Request) {
	if _, b := s.authenticate(w, r, audAPI, smallBody, "", anyRef); b == nil {
		return
	}
	set := s.store.Settings()
	list := []map[string]any{}
	hardened := s.cfg.Runner == "systemd" && sandboxHardened()
	for _, inst := range s.store.List() {
		st := statusOf(inst, set, hardened)
		if set.Domain != "" {
			_, _, st["certificate"] = s.mgr.certs.Paths(hostFor(inst.App, inst.Env, set))
		}
		list = append(list, st)
	}
	mode := "bootstrap"
	if set.Domain != "" {
		mode = "domain"
	}
	hasPassword, _ := s.auth.Status()
	out := map[string]any{"ok": true, "version": s.version, "mode": mode, "base": baseDomain(set), "instances": list,
		"login": map[string]any{"password_set": hasPassword}}
	if set.Domain != "" {
		_, _, out["certificate"] = s.mgr.certs.Paths(set.Domain)
	}
	if s.cfg.Runner == "systemd" {
		out["sandbox"] = sandboxStatus()
	}
	writeJSON(w, http.StatusOK, out)
}

// hostInfo is GET /api/host: the server's system, patch state and hardening.
func (s *Server) hostInfo(w http.ResponseWriter, r *http.Request) {
	if _, b := s.authenticate(w, r, audAPI, smallBody, "", mainOnly); b == nil {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()
	out := hostStatus(ctx, s.host)
	out["ok"] = true
	writeJSON(w, http.StatusOK, out)
}

// hostAudit is GET /api/host/audit: a security check of the server.
func (s *Server) hostAudit(w http.ResponseWriter, r *http.Request) {
	if _, b := s.authenticate(w, r, audAPI, smallBody, "", mainOnly); b == nil {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Minute)
	defer cancel()
	port := 7070
	if _, p, err := net.SplitHostPort(s.cfg.Listen); err == nil {
		if n, err := strconv.Atoi(p); err == nil {
			port = n
		}
	}
	out := hostAudit(ctx, s.host, port, s.certHosts(), time.Now())
	out["ok"] = true
	if fs, ok := out["findings"].(findings); ok {
		worst, _ := out["worst"].(string)
		s.notifier.auditDone(ctx, worst, fs)
	}
	writeJSON(w, http.StatusOK, out)
}

// hostUpgrade installs pending updates now (POST /api/host/upgrade).
func (s *Server) hostUpgrade(w http.ResponseWriter, r *http.Request) {
	if _, b := s.authenticate(w, r, audAPI, smallBody, "", mainOnly); b == nil {
		return
	}
	ctx, cancel := detached(r)
	defer cancel()
	if err := startUpgrade(ctx, s.host); err != nil {
		var cf *conflict
		if errors.As(err, &cf) {
			writeErr(w, http.StatusConflict, cf.msg)
			return
		}
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "started": true})
}

// hostReboot restarts the server (POST /api/host/reboot); with
// ?if_required=1 only when an update asked for it. Apps come back by
// themselves: labd starts them at boot.
func (s *Server) hostReboot(w http.ResponseWriter, r *http.Request) {
	onlyIfRequired := r.URL.Query().Get("if_required") == "1"
	if _, b := s.authenticate(w, r, audAPI, smallBody, "", mainOnly); b == nil {
		return
	}
	if onlyIfRequired && !rebootRequired() {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "rebooting": false, "reason": "no update asked for a reboot"})
		return
	}
	log.Printf("rebooting the server as asked")
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "rebooting": true})
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	if s.reboot != nil {
		time.AfterFunc(2*time.Second, s.reboot)
	}
}

func (s *Server) logs(w http.ResponseWriter, r *http.Request) {
	app, env, ok := appEnv(w, r)
	if !ok {
		return
	}
	lines := 200
	if v := r.URL.Query().Get("lines"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 20000 {
			writeErr(w, http.StatusBadRequest, "lines must be 1 to 20000")
			return
		}
		lines = n
	}
	if s.store.Settings().Domain != "" && !s.overTLS(r) {
		writeErr(w, http.StatusForbidden, "logs are only served over HTTPS")
		return
	}
	if _, b := s.authenticate(w, r, audAPI, smallBody, "", anyRef); b == nil {
		return
	}
	if _, found := s.store.Get(instanceID(app, env)); !found {
		writeErr(w, http.StatusNotFound, errNotFound.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	out, err := s.runner.Logs(ctx, instanceID(app, env), lines)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = io.WriteString(w, trimLines(out, lines)+"\n")
}
