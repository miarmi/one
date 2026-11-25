package main

import (
	"flag"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

var updateGolden = flag.Bool("update", false, "rewrite golden files")

func TestRenderNginxBootstrapGolden(t *testing.T) {
	site, common, err := renderNginx(EdgeView{
		Mode:     "bootstrap",
		Upstream: "http://127.0.0.1:7070",
		Webroot:  "/var/www/acme",
		Routes: []Route{
			{Host: "hello.203-0-113-10.sslip.io", Port: 20000},
			{Host: "dark-mode--hello.203-0-113-10.sslip.io", Port: 20001},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	golden(t, "nginx-bootstrap-site.conf", site)
	golden(t, "nginx-common.conf", common)
	if strings.Count(string(site), "{") != strings.Count(string(site), "}") {
		t.Fatal("unbalanced braces")
	}
	// The default server must keep labd reachable by IP.
	for _, want := range []string{"listen 80 default_server;", "location ^~ /api/", "location = /healthz", "/.well-known/acme-challenge/"} {
		if !strings.Contains(string(site), want) {
			t.Errorf("site lacks %q", want)
		}
	}
	// Routes are sorted so output is stable.
	if strings.Index(string(site), "dark-mode--hello") > strings.Index(string(site), "server_name hello.") {
		t.Error("routes not sorted")
	}
}

func TestRenderNginxAllowListGolden(t *testing.T) {
	site, _, err := renderNginx(EdgeView{
		Mode:     "bootstrap",
		Upstream: "http://127.0.0.1:7070",
		Webroot:  "/var/www/acme",
		Routes:   []Route{{Host: "hello.203-0-113-10.sslip.io", Port: 20000}},
		AllowIPs: []string{"203.0.113.7", "198.51.100.0/24"},
	})
	if err != nil {
		t.Fatal(err)
	}
	golden(t, "nginx-allow-site.conf", site)
	for _, want := range []string{"satisfy any;", "allow 203.0.113.7;", "allow 198.51.100.0/24;", "deny all;", "auth_request /_lab/auth;", "internal;"} {
		if !strings.Contains(string(site), want) {
			t.Errorf("site lacks %q", want)
		}
	}
	for _, bad := range []string{"1.2.3.4; allow all", "0.0.0.0/0", "198.51.100.7/24"} {
		_, _, err := renderNginx(EdgeView{Mode: "bootstrap", Upstream: "http://127.0.0.1:7070", Webroot: "/var/www/acme", AllowIPs: []string{bad}})
		if err == nil {
			t.Errorf("rendered allow entry %q", bad)
		}
	}
}

// The gate's pass is taken out of the Cookie header before a request reaches
// an app: nginx's maps use these regular expressions (PCRE); Go's engine
// reads them the same way for these cases.
func TestPassNeverReachesApps(t *testing.T) {
	for _, mode := range []string{"bootstrap", "domain"} {
		v := EdgeView{Mode: mode, Upstream: "http://127.0.0.1:7070", Webroot: "/var/www/acme",
			Routes: []Route{{Host: "hello.203-0-113-10.sslip.io", Port: 20000}}, AllowIPs: []string{"203.0.113.7"}}
		if mode == "domain" {
			v.Control = "lab.example.com"
			v.Routes = []Route{{Host: "hello.lab.example.com", Port: 20000, Cert: "/c/fullchain.pem", Key: "/c/privkey.pem"}}
		}
		site, _, err := renderNginx(v)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Count(string(site), "proxy_set_header Cookie $lab_app_cookie;") != 1 || !strings.Contains(string(site), "if ($lab_cookie_extra) {") {
			t.Fatalf("%s: app requests keep the pass:\n%s", mode, site)
		}
	}
	common := string([]byte(nginxCommonTmpl))
	if !strings.Contains(common, "\nserver_names_hash_bucket_size 128;\n") {
		t.Fatalf("long preview host names need a bigger server_names_hash bucket:\n%s", common)
	}
	step := func(a, b string) func(string) string {
		m := regexp.MustCompile(`"~(\^[^"]+)" "\$` + a + `\$` + b + `";`).FindStringSubmatch(common)
		if m == nil {
			t.Fatalf("no cookie map with %s and %s in %s", a, b, common)
		}
		re := regexp.MustCompile(m[1])
		return func(cookie string) string {
			g := re.FindStringSubmatch(cookie)
			if g == nil {
				return cookie
			}
			return g[re.SubexpIndex(a)] + g[re.SubexpIndex(b)]
		}
	}
	first, second := step("lab_a", "lab_b"), step("lab_c", "lab_d")
	m := regexp.MustCompile(`"~(\(\?:\^\|;\)[^"]+)" 1;`).FindStringSubmatch(common)
	if m == nil {
		t.Fatalf("no check for a pass left over in %s", common)
	}
	extra := regexp.MustCompile(m[1])
	for in, want := range map[string]string{
		"lab_session=abc.def":                              "",
		"lab_session=abc.def; lang=cs":                     "lang=cs",
		"lang=cs; lab_session=abc.def":                     "lang=cs; ",
		"a=1; lab_session=abc.def; b=2":                    "a=1; b=2",
		"a=1;lab_session=abc.def;b=2":                      "a=1;b=2",
		"xlab_session=1; lang=cs":                          "xlab_session=1; lang=cs",
		"lang=cs":                                          "lang=cs",
		"lab_session_old=1; lab_session=abc.d":             "lab_session_old=1; ",
		"lab_session=planted; lab_session=real":            "",
		"a=1; lab_session=planted; b=2; lab_session=r":     "a=1; b=2; ",
		"lab_session=x; lab_session=y; lab_session=z":      "refused",
		" lab_session=x":                                   "refused",
		"a=1;  lab_session=x; lab_session=y;lab_session=z": "refused",
		"a=1; lab_session":                                 "a=1; lab_session", // a nameless value, never a pass
	} {
		got := second(first(in))
		if extra.MatchString(got) {
			got = "refused"
		}
		if got != want {
			t.Errorf("%q -> %q, want %q", in, got, want)
		}
	}
}

// Domain mode: HTTPS for every known host, nothing for unknown ones, labd's
// pages only on the lab's own host, the login gate on every private app.
func TestRenderNginxDomainGolden(t *testing.T) {
	site, _, err := renderNginx(EdgeView{
		Mode:        "domain",
		Upstream:    "http://127.0.0.1:7070",
		Webroot:     "/var/www/acme",
		AllowIPs:    []string{"203.0.113.7"},
		Control:     "lab.example.com",
		ControlCert: "/etc/letsencrypt/live/lab.example.com/fullchain.pem",
		ControlKey:  "/etc/letsencrypt/live/lab.example.com/privkey.pem",
		Routes: []Route{
			{Host: "hello.lab.example.com", Port: 20000, Cert: "/etc/letsencrypt/live/hello.lab.example.com/fullchain.pem", Key: "/etc/letsencrypt/live/hello.lab.example.com/privkey.pem"},
			{Host: "open.lab.example.com", Port: 20001, Public: true, Cert: "/etc/letsencrypt/live/open.lab.example.com/fullchain.pem", Key: "/etc/letsencrypt/live/open.lab.example.com/privkey.pem"},
			{Host: "dark-mode--hello.lab.example.com", Port: 20002},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	golden(t, "nginx-domain-site.conf", site)
	s := string(site)
	if strings.Count(s, "{") != strings.Count(s, "}") {
		t.Fatal("unbalanced braces")
	}
	for _, want := range []string{
		"listen 443 ssl default_server;", "ssl_reject_handshake on;", "ssl_protocols TLSv1.2 TLSv1.3;",
		"return 301 https://hello.lab.example.com$request_uri;", "return 301 https://lab.example.com$request_uri;",
		`add_header Strict-Transport-Security "max-age=31536000; includeSubDomains" always;`,
		"error_page 401 403 = @lab_login;", "rewrite ^ /_lab/redirect? break;", "proxy_set_header X-Lab-Original-URI $request_uri;",
		"still getting its certificate",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("site lacks %q", want)
		}
	}
	for _, bad := range []string{"http2", "@lab_private", "TLSv1.1", "ssl_stapling"} {
		if strings.Contains(s, bad) {
			t.Errorf("site has %q", bad)
		}
	}
	// The public app has no gate; the private one has one; /_lab/ is
	// labd's only on the lab's own host.
	open := s[strings.Index(s, "server_name open.lab.example.com;\n    server_tokens off;\n    client_max_body_size"):]
	open = open[:strings.Index(open, "\n}\n")]
	if strings.Contains(open, "auth_request") || !strings.Contains(open, "location ^~ /_lab/ {\n        return 404;") {
		t.Errorf("public app block:\n%s", open)
	}
	if strings.Count(s, "auth_request /_lab/auth;") != 1 || strings.Count(s, "location ^~ /_lab/ {\n        proxy_pass") != 1 {
		t.Error("gate or labd pages in the wrong places")
	}
	// No control certificate yet: no HTTPS for labd's host, apps keep theirs.
	site, _, err = renderNginx(EdgeView{Mode: "domain", Upstream: "http://127.0.0.1:7070", Webroot: "/var/www/acme", Control: "lab.example.com",
		Routes: []Route{{Host: "hello.lab.example.com", Port: 20000, Cert: "/c/f.pem", Key: "/c/k.pem"}}})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(site), "server_name lab.example.com;") || !strings.Contains(string(site), "server_name hello.lab.example.com;") {
		t.Errorf("without a control certificate:\n%s", site)
	}
}

func TestRenderNginxDomainRefuses(t *testing.T) {
	ok := EdgeView{Mode: "domain", Upstream: "http://127.0.0.1:7070", Webroot: "/var/www/acme", Control: "lab.example.com"}
	for name, mutate := range map[string]func(*EdgeView){
		"bad control":         func(v *EdgeView) { v.Control = "lab.example.com; include /x" },
		"no control":          func(v *EdgeView) { v.Control = "" },
		"cert path injection": func(v *EdgeView) { v.ControlCert, v.ControlKey = "/a;b", "/k" },
		"relative cert":       func(v *EdgeView) { v.ControlCert, v.ControlKey = "a/b", "/k" },
		"dot dot":             func(v *EdgeView) { v.ControlCert, v.ControlKey = "/a/../b", "/k" },
		"half a pair":         func(v *EdgeView) { v.ControlCert = "/a" },
		"route cert": func(v *EdgeView) {
			v.Routes = []Route{{Host: "a.lab.example.com", Port: 20000, Cert: "/c\n}", Key: "/k"}}
		},
		"unknown mode": func(v *EdgeView) { v.Mode = "both" },
		"cert in bootstrap": func(v *EdgeView) {
			v.Mode, v.Control = "bootstrap", ""
			v.Routes = []Route{{Host: "a.b.sslip.io", Port: 20000, Cert: "/c", Key: "/k"}}
		},
		"control in bootstrap": func(v *EdgeView) { v.Mode = "bootstrap" },
	} {
		v := ok
		mutate(&v)
		if _, _, err := renderNginx(v); err == nil {
			t.Errorf("%s: rendered", name)
		}
	}
	if _, _, err := renderNginx(ok); err != nil {
		t.Fatal(err)
	}
}

func golden(t *testing.T, name string, got []byte) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if *updateGolden {
		os.MkdirAll("testdata", 0o755)
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run go test -run Golden -update)", err)
	}
	if string(want) != string(got) {
		t.Fatalf("%s differs from golden:\n%s", name, got)
	}
}

func TestRenderNginxRefusesInjection(t *testing.T) {
	bad := []Route{
		{Host: "hello.example.com; include /etc/passwd", Port: 20000},
		{Host: "hello.example.com\n}", Port: 20000},
		{Host: "Hello.example.com", Port: 20000},
		{Host: "", Port: 20000},
		{Host: "hello.example.com", Port: 80},
		{Host: "-x.example.com", Port: 20000},
	}
	for _, r := range bad {
		if _, _, err := renderNginx(EdgeView{Mode: "bootstrap", Upstream: "http://127.0.0.1:7070", Webroot: "/var/www/acme", Routes: []Route{r}}); err == nil {
			t.Errorf("rendered %+v", r)
		}
	}
	if _, _, err := renderNginx(EdgeView{Mode: "bootstrap", Upstream: "http://evil:7070", Webroot: "/var/www/acme"}); err == nil {
		t.Error("accepted a non-local upstream")
	}
}

func TestNginxEdgeRestoresOnFailure(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	os.Mkdir(bin, 0o755)
	// A fake nginx that rejects every configuration.
	os.WriteFile(filepath.Join(bin, "nginx"), []byte("#!/bin/sh\necho 'emerg: broken' >&2\nexit 1\n"), 0o755)
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	e := &nginxEdge{sitePath: filepath.Join(dir, "site"), commonPath: filepath.Join(dir, "common")}
	os.WriteFile(e.sitePath, []byte("old site"), 0o644)
	err := e.Apply(t.Context(), EdgeView{Mode: "bootstrap", Upstream: "http://127.0.0.1:7070", Webroot: "/var/www/acme"})
	if err == nil || !strings.Contains(err.Error(), "kept the old one") {
		t.Fatalf("err = %v", err)
	}
	if b, _ := os.ReadFile(e.sitePath); string(b) != "old site" {
		t.Fatalf("site not restored: %q", b)
	}
	if _, err := os.Stat(e.commonPath); !os.IsNotExist(err) {
		t.Fatal("common file left behind")
	}
}

// Each configuration names itself at /_lab/edge, so labd can wait until
// nginx serves it after a reload.
func TestEdgeVersion(t *testing.T) {
	render := func(routes ...Route) string {
		site, _, err := renderNginx(EdgeView{Mode: "bootstrap", Upstream: "http://127.0.0.1:7070", Webroot: "/var/www/acme", Routes: routes})
		if err != nil {
			t.Fatal(err)
		}
		m := edgeVersionRe.FindSubmatch(site)
		if m == nil || strings.Contains(string(site), edgeVersionPlaceholder) {
			t.Fatalf("no version in:\n%s", site)
		}
		return string(m[1])
	}
	a, b := render(Route{Host: "hello.example.com", Port: 20000}), render(Route{Host: "hello.example.com", Port: 20001})
	if a == b || a != render(Route{Host: "hello.example.com", Port: 20000}) {
		t.Fatalf("versions %s %s", a, b)
	}
}

func TestRenderNginxOpenPaths(t *testing.T) {
	route := Route{Host: "example.lab.example.com", Port: 20003, OpenPaths: []string{"/kid/"},
		Cert: "/etc/letsencrypt/live/example.lab.example.com/fullchain.pem", Key: "/etc/letsencrypt/live/example.lab.example.com/privkey.pem"}
	site, _, err := renderNginx(EdgeView{Mode: "domain", Upstream: "http://127.0.0.1:7070", Webroot: "/var/www/acme",
		AllowIPs: []string{"203.0.113.7"}, Control: "lab.example.com", Routes: []Route{route}})
	if err != nil {
		t.Fatal(err)
	}
	s := string(site)
	if strings.Count(s, "{") != strings.Count(s, "}") {
		t.Fatal("unbalanced braces")
	}
	block := s[strings.Index(s, "server_name example.lab.example.com;\n    server_tokens off;\n    client_max_body_size"):]
	block = block[:strings.Index(block, "\n}\n")]
	open := block[strings.Index(block, "location ^~ /kid/ {"):]
	open = open[:strings.Index(open, "\n    }")]
	// The open path has no gate but keeps everything else: the cookie strip,
	// the two-cookie refusal and HSTS hiding.
	for _, want := range []string{"proxy_pass http://127.0.0.1:20003;", "proxy_set_header Cookie $lab_app_cookie;", "if ($lab_cookie_extra)", "proxy_hide_header Strict-Transport-Security;"} {
		if !strings.Contains(open, want) {
			t.Errorf("open path lacks %q:\n%s", want, open)
		}
	}
	if strings.Contains(open, "auth_request") || strings.Contains(open, "allow ") {
		t.Errorf("open path is gated:\n%s", open)
	}
	// The rest of the app stays behind the gate, and /_lab/ stays closed.
	rest := block[strings.Index(block, "    location / {"):]
	if !strings.Contains(rest, "auth_request /_lab/auth;") || !strings.Contains(block, "location ^~ /_lab/ {\n        return 404;") {
		t.Errorf("app no longer gated:\n%s", block)
	}
	// Bootstrap with an allow list: the open path skips the 403 too.
	site, _, err = renderNginx(EdgeView{Mode: "bootstrap", Upstream: "http://127.0.0.1:7070", Webroot: "/var/www/acme",
		AllowIPs: []string{"203.0.113.7"}, Routes: []Route{{Host: "example.1-2-3-4.sslip.io", Port: 20003, OpenPaths: []string{"/kid/"}}}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(site), "location ^~ /kid/ {") || strings.Count(string(site), "auth_request /_lab/auth;") != 1 {
		t.Errorf("bootstrap open path:\n%s", site)
	}
}

// An open path without a trailing slash is one exact path (Notify's /send):
// nginx matches it with "=", so /send/x or /sendmail stay behind the gate.
func TestRenderNginxExactOpenPath(t *testing.T) {
	for _, mode := range []string{"domain", "bootstrap"} {
		route := Route{Host: "notify.lab.example.com", Port: 20004, OpenPaths: []string{"/send"}}
		v := EdgeView{Mode: mode, Upstream: "http://127.0.0.1:7070", Webroot: "/var/www/acme", AllowIPs: []string{"203.0.113.7"}, Routes: []Route{route}}
		if mode == "domain" {
			route.Cert, route.Key = "/etc/letsencrypt/live/notify.lab.example.com/fullchain.pem", "/etc/letsencrypt/live/notify.lab.example.com/privkey.pem"
			v.Routes, v.Control = []Route{route}, "lab.example.com"
		} else {
			route.Host = "notify.1-2-3-4.sslip.io"
			v.Routes = []Route{route}
		}
		site, _, err := renderNginx(v)
		if err != nil {
			t.Fatal(err)
		}
		s := string(site)
		if !strings.Contains(s, "location = /send {") || strings.Contains(s, "location ^~ /send") {
			t.Errorf("%s: exact open path not rendered with =:\n%s", mode, s)
		}
		open := s[strings.Index(s, "location = /send {"):]
		open = open[:strings.Index(open, "\n    }")]
		if !strings.Contains(open, "proxy_pass http://127.0.0.1:20004;") || strings.Contains(open, "auth_request") || strings.Contains(open, "allow ") {
			t.Errorf("%s: exact open path wrong:\n%s", mode, open)
		}
		if strings.Count(s, "auth_request /_lab/auth;") != 1 {
			t.Errorf("%s: the rest of the app is not gated:\n%s", mode, s)
		}
	}
}

func TestOpenPathsRefuseInjection(t *testing.T) {
	for _, paths := range [][]string{
		{"kid/"}, {"/"}, {"/_lab/"}, {"/_lab"}, {"/Kid/"}, {"/kid/x/"}, {"/kid/x"}, {"/kid/;"}, {"/kid/ {"}, {"/kid/\n"}, {"/send;"}, {"/send\n"},
		{"/../"}, {"/.."}, {"/a/", "/a/"}, {"/a", "/a"}, {"/a/", "/b/", "/c/", "/d/", "/e/"}, {"/" + strings.Repeat("a", 40) + "/"}, {"/" + strings.Repeat("a", 40)},
	} {
		v := EdgeView{Mode: "bootstrap", Upstream: "http://127.0.0.1:7070", Webroot: "/var/www/acme",
			Routes: []Route{{Host: "example.1-2-3-4.sslip.io", Port: 20003, OpenPaths: paths}}}
		if _, _, err := renderNginx(v); err == nil {
			t.Errorf("rendered open paths %q", paths)
		}
		m := Manifest{Name: "example", OpenPaths: paths}
		if err := m.validate("example"); err == nil {
			t.Errorf("manifest accepted open paths %q", paths)
		}
	}
	m := Manifest{Name: "example", OpenPaths: []string{"/kid/"}}
	if err := m.validate("example"); err != nil {
		t.Fatal(err)
	}
}
