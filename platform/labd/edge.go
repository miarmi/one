package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strings"
	"sync"
	"text/template"
	"time"
)

// Route is one public host and the local port behind it.
type Route struct {
	Host   string
	Port   int
	Public bool
	// OpenPaths are path prefixes such as "/kid/" that skip the access and
	// login gates (app.json "open_paths"); the app guards them itself.
	OpenPaths []string
	Cert      string // domain mode: the host's certificate chain and key, once it has them
	Key       string
}

// EdgeView is everything the front door needs to know.
type EdgeView struct {
	Mode     string // "bootstrap" (plain HTTP on sslip.io names) or "domain" (HTTPS)
	Upstream string // where labd listens, e.g. http://127.0.0.1:7070
	Webroot  string
	Routes   []Route
	AllowIPs []string // when set, these addresses reach the apps without a pass

	// Domain mode: the lab's own host (the base domain), where labd's pages
	// and API answer, and its certificate. Without one it gets no HTTPS yet.
	Control     string
	ControlCert string
	ControlKey  string

	Version string // set by renderNginx: names this configuration, see nginxEdge.waitLive
}

// Edge applies routing to the front door.
type Edge interface {
	Apply(ctx context.Context, view EdgeView) error
}

type noneEdge struct {
	mu   sync.Mutex
	last EdgeView
}

func (e *noneEdge) Apply(_ context.Context, v EdgeView) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.last = v
	return nil
}

// Last is the view applied last.
func (e *noneEdge) Last() EdgeView {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.last
}

// nginxEdge renders labd's two nginx files, tests the whole configuration and
// reloads. A failing test restores the previous files.
type nginxEdge struct {
	sitePath   string
	commonPath string
	liveURL    string // where nginx says which configuration it serves; "" skips the wait
	mu         sync.Mutex
}

func (e *nginxEdge) Apply(ctx context.Context, v EdgeView) error {
	site, common, err := renderNginx(v)
	if err != nil {
		return err
	}
	version := ""
	if m := edgeVersionRe.FindSubmatch(site); m != nil {
		version = string(m[1])
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	oldSite, siteErr := os.ReadFile(e.sitePath)
	oldCommon, commonErr := os.ReadFile(e.commonPath)
	if siteErr == nil && commonErr == nil && bytes.Equal(oldSite, site) && bytes.Equal(oldCommon, common) {
		return nil
	}
	if err := writeFileAtomic(e.commonPath, common, 0o644); err != nil {
		return err
	}
	if err := writeFileAtomic(e.sitePath, site, 0o644); err != nil {
		e.restore(oldCommon, commonErr, oldSite, siteErr)
		return err
	}
	if _, err := run(ctx, 30*time.Second, "nginx", "-t", "-q"); err != nil {
		e.restore(oldCommon, commonErr, oldSite, siteErr)
		return fmt.Errorf("new nginx configuration rejected, kept the old one: %w", err)
	}
	if _, err := run(ctx, 30*time.Second, "systemctl", "reload", "nginx"); err != nil {
		return err
	}
	e.waitLive(ctx, version)
	return nil
}

// waitLive waits (up to 10 seconds) until nginx serves the configuration just
// written: a reload only signals nginx, whose new workers start a moment
// later, and a deploy's URL must work as soon as the deploy answers.
func (e *nginxEdge) waitLive(ctx context.Context, version string) {
	if e.liveURL == "" {
		return
	}
	client := &http.Client{Timeout: time.Second}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) && ctx.Err() == nil {
		if resp, err := client.Get(e.liveURL); err == nil {
			b, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
			resp.Body.Close()
			if strings.TrimSpace(string(b)) == version {
				return
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	log.Printf("nginx: the new configuration was not live after 10 seconds")
}

// The site file names itself by its content at /_lab/edge on the default
// server: renderNginx hashes the file with a placeholder in that place.
const edgeVersionPlaceholder = "LAB_EDGE_VERSION"

var edgeVersionRe = regexp.MustCompile(`return 200 "([0-9a-f]{16})\\n";`)

func (e *nginxEdge) restore(common []byte, commonErr error, site []byte, siteErr error) {
	if commonErr == nil {
		_ = writeFileAtomic(e.commonPath, common, 0o644)
	} else {
		_ = os.Remove(e.commonPath)
	}
	if siteErr == nil {
		_ = writeFileAtomic(e.sitePath, site, 0o644)
	}
}

const nginxCommonTmpl = `# Managed by labd. Do not edit; changes are overwritten.
# A preview's host name can reach 70 characters (a 30-character branch, "--",
# a 20-character app and the domain); nginx's default bucket of 64 rejects the
# whole configuration then ("could not build server_names_hash").
server_names_hash_bucket_size 128;
map $http_upgrade $lab_connection_upgrade {
    default upgrade;
    ''      close;
}
# The Cookie header without lab_session, so the gate's pass never reaches an
# app: two are taken out (a page can add a second one of its own to push the
# real pass out of the first place), and a request with more is refused.
map $http_cookie $lab_cookie_1 {
    default $http_cookie;
    "~^(?<lab_a>(?:[^;]*;\s*)*?)lab_session=[^;]*(?:;\s*)?(?<lab_b>.*)$" "$lab_a$lab_b";
}
map $lab_cookie_1 $lab_app_cookie {
    default $lab_cookie_1;
    "~^(?<lab_c>(?:[^;]*;\s*)*?)lab_session=[^;]*(?:;\s*)?(?<lab_d>.*)$" "$lab_c$lab_d";
}
map $lab_app_cookie $lab_cookie_extra {
    default 0;
    "~(?:^|;)\s*lab_session=" 1;
}
`

const nginxSiteTmpl = `{{define "acme"}}    location ^~ /.well-known/acme-challenge/ {
        root {{.Webroot}};
        default_type text/plain;
    }
{{- end}}
{{- define "tls"}}    ssl_protocols TLSv1.2 TLSv1.3;
    ssl_ciphers ECDHE-ECDSA-AES128-GCM-SHA256:ECDHE-RSA-AES128-GCM-SHA256:ECDHE-ECDSA-AES256-GCM-SHA384:ECDHE-RSA-AES256-GCM-SHA384:ECDHE-ECDSA-CHACHA20-POLY1305:ECDHE-RSA-CHACHA20-POLY1305;
    ssl_prefer_server_ciphers off;
    ssl_session_cache shared:lab_tls:10m;
    ssl_session_timeout 1d;
    ssl_session_tickets off;
{{- end}}
{{- define "labd"}}
        proxy_pass {{.Upstream}};
        proxy_set_header Host $host;
        proxy_set_header X-Real-IP $remote_addr;
        proxy_set_header X-Lab-Scheme $scheme;
{{- end}}
{{- define "app"}}
        if ($lab_cookie_extra) {
            return 400;
        }
{{- if .Gate}}
        satisfy any;
{{- range .AllowIPs}}
        allow {{.}};
{{- end}}
        deny all;
        auth_request /_lab/auth;
        error_page 401 403 = {{.Denied}};
{{- end}}
        proxy_pass http://127.0.0.1:{{.Port}};
        proxy_http_version 1.1;
        proxy_set_header Host $host;
        proxy_set_header X-Real-IP $remote_addr;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto $scheme;
        proxy_set_header X-Lab-Scheme $scheme;
        proxy_set_header Cookie $lab_app_cookie;
        proxy_set_header Upgrade $http_upgrade;
        proxy_set_header Connection $lab_connection_upgrade;
{{- if .HTTPS}}
        proxy_hide_header Strict-Transport-Security;
{{- end}}
        proxy_read_timeout 300s;
{{- end}}
{{- define "auth"}}
    location = /_lab/auth {
        internal;
        proxy_pass {{.Upstream}}/_lab/auth;
        proxy_pass_request_body off;
        proxy_set_header Content-Length "";
        proxy_set_header X-Lab-Host $host;
    }
{{- end -}}
# Managed by labd. Do not edit; changes are overwritten.
# Mode: {{.Mode}}

server {
    listen 80 default_server;
    listen [::]:80 default_server;
    server_name _;
    server_tokens off;
    client_max_body_size 200m;

{{template "acme" .}}
    location = /healthz {
        proxy_pass {{.Upstream}};
        proxy_set_header X-Lab-Scheme $scheme;
    }
    location = /_lab/edge {
        default_type text/plain;
        return 200 "{{.Version}}\n";
    }
    location ^~ /api/ {
        proxy_pass {{.Upstream}};
        proxy_set_header Host $host;
        proxy_set_header X-Real-IP $remote_addr;
        proxy_set_header X-Lab-Scheme $scheme;
        proxy_read_timeout 300s;
        proxy_send_timeout 300s;
    }
    location / {
        return 404;
    }
}
{{- if eq .Mode "domain"}}

# HTTPS answers only the hosts below; any other name is refused before TLS.
server {
    listen 443 ssl default_server;
    listen [::]:443 ssl default_server;
    server_name _;
    server_tokens off;
    ssl_reject_handshake on;
{{template "tls"}}
}
{{- if .ControlCert}}

server {
    listen 80;
    listen [::]:80;
    server_name {{.Control}};
    server_tokens off;

{{template "acme" .}}
    location / {
        return 301 https://{{.Control}}$request_uri;
    }
}

server {
    listen 443 ssl;
    listen [::]:443 ssl;
    server_name {{.Control}};
    server_tokens off;
    client_max_body_size 200m;
    ssl_certificate {{.ControlCert}};
    ssl_certificate_key {{.ControlKey}};
{{template "tls"}}
    add_header Strict-Transport-Security "max-age=31536000; includeSubDomains" always;

    location = /healthz {
        proxy_pass {{.Upstream}};
        proxy_set_header X-Lab-Scheme $scheme;
    }
    location ^~ /api/ {
{{- template "labd" .}}
        proxy_read_timeout 300s;
        proxy_send_timeout 300s;
    }
    # labd's own pages: sign-in, setup, sign-out and the list of apps.
    location ^~ /_lab/ {
{{- template "labd" .}}
    }
    location = / {
{{- template "labd" .}}
    }
    location / {
        return 404;
    }
}
{{- end}}
{{- end}}
{{- range $r := .Routes}}
{{- if .Cert}}

server {
    listen 80;
    listen [::]:80;
    server_name {{.Host}};
    server_tokens off;

{{template "acme" $}}
    location / {
        return 301 https://{{.Host}}$request_uri;
    }
}

server {
    listen 443 ssl;
    listen [::]:443 ssl;
    server_name {{.Host}};
    server_tokens off;
    client_max_body_size 200m;
    ssl_certificate {{.Cert}};
    ssl_certificate_key {{.Key}};
{{template "tls"}}
    add_header Strict-Transport-Security "max-age=31536000; includeSubDomains" always;

    # /_lab/ belongs to the platform; signing in happens on the lab's own host.
    location ^~ /_lab/ {
        return 404;
    }
{{- range $p := .OpenPaths}}
    # Open path: the app checks these requests itself (app.json open_paths).
    location {{openMatch $p}} {{$p}} {
{{- template "app" (openBlock $r true)}}
    }
{{- end}}
{{- if not .Public}}
{{- template "auth" $}}
    location @lab_login {
        rewrite ^ /_lab/redirect? break;
        proxy_pass {{$.Upstream}};
        proxy_pass_request_body off;
        proxy_set_header Content-Length "";
        proxy_set_header X-Lab-Host $host;
        proxy_set_header X-Lab-Original-URI $request_uri;
        proxy_set_header X-Lab-Scheme $scheme;
    }
{{- end}}
    location / {
{{- template "app" (appBlock $ . true)}}
    }
}
{{- else if eq $.Mode "domain"}}

server {
    listen 80;
    listen [::]:80;
    server_name {{.Host}};
    server_tokens off;

{{template "acme" $}}
    location / {
        default_type text/plain;
        return 503 "This address is still getting its certificate. Try again in a few minutes.\n";
    }
}
{{- else}}

server {
    listen 80;
    listen [::]:80;
    server_name {{.Host}};
    server_tokens off;
    client_max_body_size 200m;

{{template "acme" $}}
{{- range $p := .OpenPaths}}
    location {{openMatch $p}} {{$p}} {
{{- template "app" (openBlock $r false)}}
    }
{{- end}}
{{- if $.AllowIPs}}
{{- template "auth" $}}
    location @lab_private {
        default_type text/plain;
        return 403 "This lab is private.\n";
    }
{{- end}}
    location / {
{{- template "app" (appBlock $ . false)}}
    }
}
{{- end}}
{{- end}}
`

// appBlock is what the "app" template needs for one route.
type appView struct {
	Port     int
	Gate     bool
	AllowIPs []string
	Denied   string // where a request without access goes
	HTTPS    bool
}

func appBlock(v EdgeView, r Route, https bool) appView {
	if https {
		return appView{Port: r.Port, Gate: !r.Public, AllowIPs: v.AllowIPs, Denied: "@lab_login", HTTPS: true}
	}
	return appView{Port: r.Port, Gate: len(v.AllowIPs) > 0, AllowIPs: v.AllowIPs, Denied: "@lab_private"}
}

// openBlock is an open path of a route: no gate, the app guards it.
func openBlock(r Route, https bool) appView {
	return appView{Port: r.Port, HTTPS: https}
}

// openPathRe is the only shape an open path may have, so nothing can break
// out of a location line: one lowercase segment, with a trailing slash for a
// prefix ("/kid/", everything below it) or without one for a single exact
// path ("/send", the Notify app's endpoint for other programs).
var openPathRe = regexp.MustCompile(`^/[a-z0-9][a-z0-9-]{0,30}/?$`)

// openMatch is the nginx matcher for an open path: a prefix or an exact path.
func openMatch(p string) string {
	if strings.HasSuffix(p, "/") {
		return "^~"
	}
	return "="
}

// validOpenPaths checks app.json open_paths: at most four, well formed,
// never the platform's /_lab/, no duplicates.
func validOpenPaths(paths []string) bool {
	if len(paths) > 4 {
		return false
	}
	seen := map[string]bool{}
	for _, p := range paths {
		if !openPathRe.MatchString(p) || p == "/_lab/" || seen[p] {
			return false
		}
		seen[p] = true
	}
	return true
}

var siteTmpl = template.Must(template.New("site").Funcs(template.FuncMap{"appBlock": appBlock, "openBlock": openBlock, "openMatch": openMatch}).Parse(nginxSiteTmpl))

// safeCertPath allows only plain absolute paths in ssl_certificate lines.
var safeCertPath = regexp.MustCompile(`^/[A-Za-z0-9._/-]+$`)

func certPathsOK(cert, key string) bool {
	if cert == "" && key == "" {
		return true
	}
	return safeCertPath.MatchString(cert) && safeCertPath.MatchString(key) && !strings.Contains(cert+key, "..")
}

// renderNginx returns the site file and the common file.
func renderNginx(v EdgeView) (site, common []byte, err error) {
	if v.Mode != "bootstrap" && v.Mode != "domain" {
		return nil, nil, fmt.Errorf("unknown mode %q", v.Mode)
	}
	routes := append([]Route(nil), v.Routes...)
	sort.Slice(routes, func(a, b int) bool { return routes[a].Host < routes[b].Host })
	for _, r := range routes {
		if !safeHost(r.Host) || r.Port < portMin || r.Port > portMax || !certPathsOK(r.Cert, r.Key) || (r.Cert != "" && v.Mode != "domain") || !validOpenPaths(r.OpenPaths) {
			return nil, nil, fmt.Errorf("refusing to render route %q:%d", r.Host, r.Port)
		}
	}
	if !strings.HasPrefix(v.Upstream, "http://127.0.0.1:") || strings.ContainsAny(v.Upstream, " ;{}\n") {
		return nil, nil, errors.New("bad upstream")
	}
	if !strings.HasPrefix(v.Webroot, "/") || strings.ContainsAny(v.Webroot, " ;{}\n") {
		return nil, nil, errors.New("bad webroot")
	}
	if v.Mode == "domain" && !safeHost(v.Control) || !certPathsOK(v.ControlCert, v.ControlKey) || v.Mode != "domain" && (v.Control != "" || v.ControlCert != "") {
		return nil, nil, fmt.Errorf("refusing to render control host %q", v.Control)
	}
	v.Routes = routes
	for _, a := range v.AllowIPs {
		if norm, ok := normalizeAllowIP(a); !ok || norm != a {
			return nil, nil, fmt.Errorf("refusing to render allow entry %q", a)
		}
	}
	v.Version = edgeVersionPlaceholder
	var buf bytes.Buffer
	if err := siteTmpl.Execute(&buf, v); err != nil {
		return nil, nil, err
	}
	sum := sha256.Sum256(append(buf.Bytes(), nginxCommonTmpl...))
	site = bytes.Replace(buf.Bytes(), []byte(edgeVersionPlaceholder), []byte(hex.EncodeToString(sum[:])[:16]), 1)
	return site, []byte(nginxCommonTmpl), nil
}

// safeHost allows only lowercase DNS names, so nothing can break out of a
// server_name line.
func safeHost(h string) bool {
	if len(h) == 0 || len(h) > 253 {
		return false
	}
	for _, label := range strings.Split(h, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for i := 0; i < len(label); i++ {
			c := label[i]
			if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
				return false
			}
		}
	}
	return true
}
