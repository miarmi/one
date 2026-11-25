package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"html/template"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

const (
	testDomain = "lab.example.test"
	homeIP     = "203.0.113.7"
	awayIP     = "198.51.100.20"
)

// visit is a request to one of labd's pages as nginx forwards it from the
// lab's own HTTPS host.
type visit struct {
	method, path string
	host         string // default testDomain
	ip           string // default awayIP
	form         url.Values
	cookies      []string // lab_session values
	header       map[string]string
	plainHTTP    bool
}

func (h *harness) visit(v visit) (*http.Response, string) {
	h.t.Helper()
	var body *strings.Reader
	if v.form != nil {
		body = strings.NewReader(v.form.Encode())
	} else {
		body = strings.NewReader("")
	}
	req, _ := http.NewRequest(v.method, h.ts.URL+v.path, body)
	req.Host = testDomain
	if v.host != "" {
		req.Host = v.host
	}
	if !v.plainHTTP {
		req.Header.Set("X-Lab-Scheme", "https")
	}
	ip := awayIP
	if v.ip != "" {
		ip = v.ip
	}
	req.Header.Set("X-Real-IP", ip)
	if v.form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Origin", "https://"+req.Host)
		req.Header.Set("Sec-Fetch-Site", "same-origin")
	}
	for _, c := range v.cookies {
		req.AddCookie(&http.Cookie{Name: sessionCookie, Value: c})
	}
	for k, val := range v.header {
		req.Header.Set(k, val)
	}
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	defer resp.Body.Close()
	b := new(strings.Builder)
	buf := make([]byte, 4096)
	for {
		n, err := resp.Body.Read(buf)
		b.Write(buf[:n])
		if err != nil {
			break
		}
	}
	return resp, b.String()
}

// esc is how a text appears in a page.
func esc(s string) string { return template.HTMLEscapeString(s) }

func passFrom(t *testing.T, resp *http.Response) *http.Cookie {
	t.Helper()
	for _, c := range resp.Cookies() {
		if c.Name == sessionCookie {
			return c
		}
	}
	t.Fatalf("no lab_session cookie set (status %d, headers %v)", resp.StatusCode, resp.Header)
	return nil
}

func codeSum(code string) string {
	norm, ok := normalizeSetupCode(code)
	if !ok {
		panic("bad test code " + code)
	}
	s := sha256.Sum256([]byte(norm))
	return hex.EncodeToString(s[:])
}

func (h *harness) authFor(host string, cookies ...string) int {
	h.t.Helper()
	req, _ := http.NewRequest("GET", h.ts.URL+"/_lab/auth", nil)
	req.Header.Set("X-Lab-Host", host)
	for _, c := range cookies {
		req.AddCookie(&http.Cookie{Name: sessionCookie, Value: c})
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

type failingCerts struct{ Certs }

func (failingCerts) Ensure(context.Context, string) error {
	return errors.New("challenge failed: no A record")
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestDomainMode(t *testing.T) {
	h := newHarness(t)
	good := bundleOf(t, testApps.good, "hello", "")
	code, _, raw := h.call("POST", "/api/settings", []byte(`{"domain":"","public_ip":"127.0.0.1","allow_ips":["`+homeIP+`"]}`), refMain)
	mustStatus(t, "bootstrap settings", code, 200, raw)
	code, _, raw = h.call("POST", "/api/deploy?app=hello&env=prod&commit=1111111", good, refMain)
	mustStatus(t, "deploy prod", code, 200, raw)
	code, _, raw = h.call("POST", "/api/deploy?app=hello&env=dark-mode&commit=2222222", good, "refs/heads/dark-mode")
	mustStatus(t, "deploy preview", code, 200, raw)

	// Domains labd refuses.
	for _, bad := range []string{"1.2.3.4", "lab", "x.203-0-113-10.sslip.io", "a..b.cz", "-a.example.com", "lab.example.com; include x", strings.Repeat("a", 60) + "." + strings.Repeat("b", 60) + "." + strings.Repeat("c", 60) + ".much.longer.example.com"} {
		code, _, raw = h.call("POST", "/api/settings", []byte(`{"domain":"`+bad+`","public_ip":"127.0.0.1"}`), refMain)
		mustStatus(t, "domain "+bad, code, 400, raw)
	}

	// No certificate for the lab's own host: the lab stays as it is.
	real := h.srv.mgr.certs
	h.srv.mgr.certs = failingCerts{real}
	code, _, raw = h.call("POST", "/api/settings", []byte(`{"domain":"`+testDomain+`","public_ip":"127.0.0.1","allow_ips":["`+homeIP+`"]}`), refMain)
	mustStatus(t, "domain without DNS", code, 409, raw)
	if !strings.Contains(raw, "point to this server") || h.srv.store.Settings().Domain != "" || h.edge.Last().Mode != "bootstrap" {
		t.Fatalf("a failed switch changed something: %s %+v", raw, h.srv.store.Settings())
	}
	h.srv.mgr.certs = real

	// The switch: HTTPS for the lab's host at once, the apps' hosts follow.
	code, js, raw := h.call("POST", "/api/settings", []byte(`{"domain":"Lab.Example.Test.","public_ip":"127.0.0.1","allow_ips":["`+homeIP+`"]}`), refMain)
	mustStatus(t, "domain settings", code, 200, raw)
	if js["mode"] != "domain" || js["base"] != testDomain {
		t.Fatalf("settings answer: %s", raw)
	}
	waitFor(t, "certificates for every host", func() bool {
		v := h.srv.mgr.view()
		if v.ControlCert == "" || len(v.Routes) != 2 {
			return false
		}
		for _, r := range v.Routes {
			if r.Cert == "" {
				return false
			}
		}
		return true
	})
	v := h.edge.Last()
	if v.Mode != "domain" || v.Control != testDomain || v.ControlCert == "" {
		t.Fatalf("edge view: %+v", v)
	}
	// Apps learn their new address.
	if _, body := h.appGet("hello", "prod", "/env"); !strings.Contains(body, `"BASE_URL":"https://hello.`+testDomain+`"`) {
		t.Fatalf("hello's address after the switch: %s", body)
	}
	code, js, raw = h.call("GET", "/api/status", nil, refMain)
	mustStatus(t, "status", code, 200, raw)
	if js["mode"] != "domain" || js["certificate"] != true || !strings.Contains(raw, `"certificate":true`) || !strings.Contains(raw, `"password_set":false`) {
		t.Fatalf("status: %s", raw)
	}

	// A new preview has its certificate as soon as its deploy answers.
	code, js, raw = h.call("POST", "/api/deploy?app=hello&env=itest&commit=3333333", good, "refs/heads/itest")
	mustStatus(t, "deploy in domain mode", code, 200, raw)
	if js["url"] != "https://itest--hello."+testDomain {
		t.Fatalf("url %v", js["url"])
	}
	if _, _, ok := h.srv.mgr.certs.Paths("itest--hello." + testDomain); !ok {
		t.Fatal("no certificate right after the deploy")
	}
	code, _, raw = h.call("DELETE", "/api/instances?app=hello&env=itest", nil, "refs/heads/itest")
	mustStatus(t, "delete preview", code, 200, raw)
	if _, _, ok := h.srv.mgr.certs.Paths("itest--hello." + testDomain); ok {
		t.Fatal("the deleted preview's certificate is still there")
	}

	// The pipeline's passes need HTTPS now.
	code, _, raw = h.call("POST", "/api/session?app=hello&env=prod", nil, refMain)
	mustStatus(t, "session over plain HTTP", code, 403, raw)
	sessionReq := func() (int, map[string]any) {
		uri := "/api/session?app=hello&env=prod"
		req, _ := http.NewRequest("POST", h.ts.URL+uri, nil)
		req.Header.Set("X-Lab-Scheme", "https")
		req.Header.Set("Authorization", "Bearer "+h.ti.token(t, baseClaims(refMain, apiAudience("POST", uri, sha256Hex(nil)))))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out map[string]any
		json.NewDecoder(resp.Body).Decode(&out)
		return resp.StatusCode, out
	}
	code, js = sessionReq()
	if code != 200 {
		t.Fatalf("session over HTTPS: %d %v", code, js)
	}
	ciPass := js["cookie"].(string)
	if h.authFor("hello."+testDomain, ciPass) != 204 || h.authFor("dark-mode--hello."+testDomain, ciPass) != 401 {
		t.Fatal("a pipeline pass must open its own host only")
	}

	// Pages answer only on the lab's own host, over HTTPS.
	if resp, _ := h.visit(visit{method: "GET", path: "/_lab/login", plainHTTP: true}); resp.StatusCode != 404 {
		t.Fatalf("sign-in page over plain HTTP: %d", resp.StatusCode)
	}
	if resp, _ := h.visit(visit{method: "GET", path: "/_lab/login", host: "hello." + testDomain}); resp.StatusCode != 404 {
		t.Fatalf("sign-in page on an app's host: %d", resp.StatusCode)
	}
	resp, body := h.visit(visit{method: "GET", path: "/_lab/login"})
	if resp.StatusCode != 200 || !strings.Contains(body, esc(textsEN.NoPassword)) || strings.Contains(body, `type="password"`) {
		t.Fatalf("sign-in page without a password: %d %s", resp.StatusCode, body)
	}
	csp := resp.Header.Get("Content-Security-Policy")
	if !strings.Contains(csp, "frame-ancestors 'none'") || !strings.Contains(csp, "default-src 'none'") || resp.Header.Get("X-Frame-Options") != "DENY" || resp.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("page headers: %v", resp.Header)
	}

	// Setup codes: only their SHA-256 comes in, only from main.
	code, _, raw = h.call("POST", "/api/setup-code", []byte(`{"code_sha256":"nothex"}`), refMain)
	mustStatus(t, "setup code with a bad hash", code, 400, raw)
	setup := "7K3M-9QX2-HD4R-T8WA"
	code, _, raw = h.call("POST", "/api/setup-code", []byte(`{"code_sha256":"`+codeSum(setup)+`"}`), "refs/heads/dark-mode")
	mustStatus(t, "setup code from a branch", code, 403, raw)
	code, js, raw = h.call("POST", "/api/setup-code", []byte(`{"code_sha256":"`+codeSum(setup)+`"}`), refMain)
	mustStatus(t, "setup code", code, 200, raw)
	if js["url"] != "https://"+testDomain+"/_lab/setup" {
		t.Fatalf("setup code answer: %s", raw)
	}
	if exp := int64(js["expires"].(float64)); exp < time.Now().Add(59*time.Minute).Unix() || exp > time.Now().Add(61*time.Minute).Unix() {
		t.Fatalf("setup code expires at %d", exp)
	}
	if b, _ := os.ReadFile(filepath.Join(h.cfg.StateDir, "auth.json")); strings.Contains(string(b), strings.ReplaceAll(setup, "-", "")) {
		t.Fatal("the setup code itself is stored")
	}
	if info, err := os.Stat(filepath.Join(h.cfg.StateDir, "auth.json")); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("auth.json: %v %v", info, err)
	}

	// The setup page.
	if _, body = h.visit(visit{method: "GET", path: "/_lab/setup?code=" + setup}); strings.Contains(body, esc(textsEN.BadCode)) || !strings.Contains(body, `type="hidden" name="code" value="`+setup+`"`) {
		t.Fatalf("setup page with the code: %s", body)
	}
	if _, body = h.visit(visit{method: "GET", path: "/_lab/setup?code=AAAA-AAAA-AAAA-AAAA"}); !strings.Contains(body, esc(textsEN.BadCode)) {
		t.Fatalf("setup page with a wrong code: %s", body)
	}
	pw := "correct horse battery"
	for name, f := range map[string]url.Values{
		"short":    {"code": {setup}, "password": {"short"}, "password2": {"short"}},
		"mismatch": {"code": {setup}, "password": {pw}, "password2": {pw + "!"}},
	} {
		// Back to the form (a reload never sends it twice), code kept,
		// and no try counted: typos aren't guesses.
		resp, _ := h.visit(visit{method: "POST", path: "/_lab/setup", form: f})
		if want := "/_lab/setup?code=" + setup + "&e=" + name; resp.StatusCode != 303 || resp.Header.Get("Location") != want {
			t.Fatalf("setup %s: %d %q", name, resp.StatusCode, resp.Header.Get("Location"))
		}
		if _, body := h.visit(visit{method: "GET", path: resp.Header.Get("Location")}); !strings.Contains(body, esc(map[string]string{"short": textsEN.TooShort, "mismatch": textsEN.Mismatch}[name])) {
			t.Fatalf("setup %s page: %s", name, body)
		}
	}
	// Forms only from the lab's own page.
	for _, hdr := range []map[string]string{
		{"Origin": "https://evil.example", "Sec-Fetch-Site": "cross-site"},
		{"Origin": "https://hello." + testDomain, "Sec-Fetch-Site": "same-site"},
	} {
		if resp, _ := h.visit(visit{method: "POST", path: "/_lab/setup", form: url.Values{"code": {setup}, "password": {pw}, "password2": {pw}}, header: hdr}); resp.StatusCode != 403 {
			t.Fatalf("setup posted from %v: %d", hdr, resp.StatusCode)
		}
	}
	resp, _ = h.visit(visit{method: "POST", path: "/_lab/setup", form: url.Values{"code": {strings.ToLower(setup)}, "password": {pw}, "password2": {pw}}})
	if resp.StatusCode != 303 || resp.Header.Get("Location") != "/?saved=1" {
		t.Fatalf("setup: %d %v", resp.StatusCode, resp.Header)
	}
	pass := passFrom(t, resp)
	if pass.Domain != testDomain || !pass.Secure || !pass.HttpOnly || pass.SameSite != http.SameSiteLaxMode || pass.Path != "/" || pass.MaxAge < 170*24*3600 {
		t.Fatalf("pass cookie: %+v", pass)
	}
	if resp, _ := h.visit(visit{method: "POST", path: "/_lab/setup", form: url.Values{"code": {setup}, "password": {pw}, "password2": {pw}}}); resp.StatusCode != 303 || !strings.HasPrefix(resp.Header.Get("Location"), "/_lab/setup?e=code&typed=") {
		t.Fatalf("a setup code worked twice: %d %v", resp.StatusCode, resp.Header)
	}
	// The used link now just points to signing in.
	if _, body = h.visit(visit{method: "GET", path: "/_lab/setup?code=" + setup}); !strings.Contains(body, esc(textsEN.AlreadySet)) || strings.Contains(body, "<form") {
		t.Fatalf("setup link after use: %s", body)
	}
	code, _, raw = h.call("POST", "/api/setup-code", []byte(`{"code_sha256":"`+codeSum(setup)+`"}`), refMain)
	mustStatus(t, "setup code once a password is set", code, 409, raw)

	// The owner's pass opens every host of the lab, and nothing else.
	for _, host := range []string{"hello." + testDomain, "dark-mode--hello." + testDomain} {
		if got := h.authFor(host, pass.Value); got != 204 {
			t.Fatalf("The owner's pass on %s: %d", host, got)
		}
	}
	for _, host := range []string{"hello.example.test", "hello." + testDomain + ".evil.example", "evil.example"} {
		if got := h.authFor(host, pass.Value); got != 401 {
			t.Fatalf("The owner's pass on %s: %d", host, got)
		}
	}
	// A cookie a page planted can't hide the real one.
	if got := h.authFor("hello."+testDomain, "planted", pass.Value); got != 204 {
		t.Fatalf("pass after a planted cookie: %d", got)
	}

	// The list of apps: signed in, or at home.
	resp, body = h.visit(visit{method: "GET", path: "/?saved=1", cookies: []string{pass.Value}})
	if resp.StatusCode != 200 || !strings.Contains(body, esc(textsEN.PasswordSaved)) || !strings.Contains(body, `href="https://hello.`+testDomain+`"`) ||
		!strings.Contains(body, "dark-mode") || !strings.Contains(body, esc(textsEN.SignOut)) {
		t.Fatalf("home signed in: %d %s", resp.StatusCode, body)
	}
	if resp, body = h.visit(visit{method: "GET", path: "/", ip: homeIP}); resp.StatusCode != 200 || strings.Contains(body, esc(textsEN.SignOut)) {
		t.Fatalf("home at home: %d %s", resp.StatusCode, body)
	}
	if resp, _ = h.visit(visit{method: "GET", path: "/"}); resp.StatusCode != 303 || resp.Header.Get("Location") != "/_lab/login" {
		t.Fatalf("home away, signed out: %d %v", resp.StatusCode, resp.Header)
	}

	// Signing in, and where it leads afterwards.
	resp, body = h.visit(visit{method: "POST", path: "/_lab/login", form: url.Values{"password": {"wrong password!"}}})
	if resp.StatusCode != 303 || resp.Header.Get("Location") != "/_lab/login?e=wrong" {
		t.Fatalf("wrong password: %d %v", resp.StatusCode, resp.Header)
	}
	if _, body = h.visit(visit{method: "GET", path: "/_lab/login?e=wrong"}); !strings.Contains(body, esc(textsEN.WrongPassword)) {
		t.Fatalf("wrong password page: %s", body)
	}
	for next, want := range map[string]string{
		"https://hello." + testDomain + "/notes?x=1&y=2": "https://hello." + testDomain + "/notes?x=1&y=2",
		"/":                     "https://" + testDomain + "/",
		"":                      "/",
		"https://evil.example/": "/",
		"//evil.example/":       "/",
		"https://hello." + testDomain + ".evil.example/": "/",
		"https://user@hello." + testDomain + "/":         "/",
		"https://hello." + testDomain + ":8443/":         "/",
		"http://hello." + testDomain + "/":               "/",
		"javascript:alert(1)":                            "/",
		"https:\\\\evil.example":                         "/",
	} {
		resp, _ = h.visit(visit{method: "POST", path: "/_lab/login", ip: "192.0.2." + strconv.Itoa(len(next)%200+1), form: url.Values{"password": {pw}, "next": {next}}})
		if resp.StatusCode != 303 || resp.Header.Get("Location") != want {
			t.Errorf("next %q: %d to %q, want %q", next, resp.StatusCode, resp.Header.Get("Location"), want)
		}
	}
	passFrom(t, resp)
	// Already signed in: the sign-in page sends you on.
	if resp, _ = h.visit(visit{method: "GET", path: "/_lab/login?next=" + url.QueryEscape("https://hello."+testDomain+"/x"), cookies: []string{pass.Value}}); resp.StatusCode != 303 || resp.Header.Get("Location") != "https://hello."+testDomain+"/x" {
		t.Fatalf("sign-in page when signed in: %d %v", resp.StatusCode, resp.Header)
	}

	// Guessing is slow: 5 wrong tries a minute per address.
	clock := time.Now()
	h.srv.limiter.now = func() time.Time { return clock }
	for i := 0; i < 5; i++ {
		h.visit(visit{method: "POST", path: "/_lab/login", ip: "192.0.2.250", form: url.Values{"password": {"guess guess guess"}}})
	}
	if resp, _ = h.visit(visit{method: "POST", path: "/_lab/login", ip: "192.0.2.250", form: url.Values{"password": {pw}}}); resp.StatusCode != 303 || resp.Header.Get("Location") != "/_lab/login?e=busy" {
		t.Fatalf("after 5 wrong tries: %d", resp.StatusCode)
	}
	if resp, _ = h.visit(visit{method: "POST", path: "/_lab/login", ip: "192.0.2.251", form: url.Values{"password": {pw}}}); resp.StatusCode != 303 {
		t.Fatalf("another address: %d", resp.StatusCode)
	}
	clock = clock.Add(61 * time.Second)
	if resp, _ = h.visit(visit{method: "POST", path: "/_lab/login", ip: "192.0.2.250", form: url.Values{"password": {pw}}}); resp.StatusCode != 303 {
		t.Fatalf("a minute later: %d", resp.StatusCode)
	}

	// The gate sends visitors to the sign-in page and back.
	req, _ := http.NewRequest("GET", h.ts.URL+"/_lab/redirect", nil)
	req.Header.Set("X-Lab-Host", "hello."+testDomain)
	req.Header.Set("X-Lab-Original-URI", "/notes?a=1&b=2")
	r2, err := (&http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	r2.Body.Close()
	if want := "https://" + testDomain + "/_lab/login?next=" + url.QueryEscape("https://hello."+testDomain+"/notes?a=1&b=2"); r2.StatusCode != 302 || r2.Header.Get("Location") != want {
		t.Fatalf("gate redirect: %d %q, want %q", r2.StatusCode, r2.Header.Get("Location"), want)
	}
	req.Header.Set("X-Lab-Host", "evil.example")
	r2, _ = (&http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}).Do(req)
	r2.Body.Close()
	if r2.Header.Get("Location") != "https://"+testDomain+"/_lab/login" {
		t.Fatalf("gate redirect for a foreign host: %q", r2.Header.Get("Location"))
	}

	// Signing out.
	resp, _ = h.visit(visit{method: "POST", path: "/_lab/logout", form: url.Values{}, cookies: []string{pass.Value}})
	if resp.StatusCode != 303 || resp.Header.Get("Location") != "/_lab/login?out=1" || passFrom(t, resp).MaxAge >= 0 {
		t.Fatalf("sign out: %d %v", resp.StatusCode, resp.Header)
	}
	if _, body = h.visit(visit{method: "GET", path: "/_lab/login?out=1"}); !strings.Contains(body, esc(textsEN.SignedOut)) {
		t.Fatalf("after signing out: %s", body)
	}

	// Czech for Czech browsers.
	if _, body = h.visit(visit{method: "GET", path: "/_lab/login", header: map[string]string{"Accept-Language": "cs-CZ,cs;q=0.9,en;q=0.8"}}); !strings.Contains(body, esc(textsCS.SignIn)) || !strings.Contains(body, `lang="cs"`) {
		t.Fatalf("Czech sign-in page: %s", body)
	}

	// A new password (reset) ends every earlier pass.
	code, _, raw = h.call("POST", "/api/setup-code?reset=1", []byte(`{"code_sha256":"`+codeSum("0000-1111-2222-3333")+`"}`), refMain)
	mustStatus(t, "reset code", code, 200, raw)
	if h.authFor("hello."+testDomain, pass.Value) != 204 {
		t.Fatal("a reset code alone must not end passes")
	}
	resp, _ = h.visit(visit{method: "POST", path: "/_lab/setup", form: url.Values{"code": {"oooo iiii 2222 3333"}, "password": {"another long password"}, "password2": {"another long password"}}})
	if resp.StatusCode != 303 {
		t.Fatalf("reset: %d", resp.StatusCode)
	}
	if h.authFor("hello."+testDomain, pass.Value) != 401 {
		t.Fatal("the old pass still works after a new password")
	}
	if h.authFor("hello."+testDomain, passFrom(t, resp).Value) != 204 {
		t.Fatal("the new pass does not work")
	}

	// Back to bootstrap mode: passes from the login page stop working there.
	code, _, raw = h.call("POST", "/api/settings", []byte(`{"domain":"","public_ip":"127.0.0.1","allow_ips":["`+homeIP+`"]}`), refMain)
	mustStatus(t, "back to bootstrap", code, 200, raw)
	if h.authFor("hello.127-0-0-1.sslip.io", passFrom(t, resp).Value) != 401 {
		t.Fatal("The owner's pass works in bootstrap mode")
	}
	if resp, _ := h.visit(visit{method: "GET", path: "/_lab/login"}); resp.StatusCode != 404 {
		t.Fatalf("sign-in page in bootstrap mode: %d", resp.StatusCode)
	}
}

func TestSetupCodes(t *testing.T) {
	for in, want := range map[string]string{
		"7K3M-9QX2-HD4R-T8WA":   "7K3M9QX2HD4RT8WA",
		"7k3m 9qx2 hd4r t8wa":   "7K3M9QX2HD4RT8WA",
		"OIL0-0000-0000-0000":   "0110000000000000",
		"7K3M-9QX2-HD4R-T8W":    "",
		"7K3M-9QX2-HD4R-T8WAX":  "",
		"7K3M-9QX2-HD4R-T8WU":   "", // U is not in the alphabet
		"7K3M-9QX2-HD4R-T8W!":   "",
		strings.Repeat("A", 70): "",
	} {
		got, ok := normalizeSetupCode(in)
		if (want == "") == ok || got != want {
			t.Errorf("%q -> %q %v, want %q", in, got, ok, want)
		}
	}

	dir := t.TempDir()
	a := newAuth(dir)
	a.iterations = 1000
	clock := time.Now()
	a.now = func() time.Time { return clock }
	if _, err := a.UseSetupCode("7K3M-9QX2-HD4R-T8WA", "a long enough password"); !errors.Is(err, errBadCode) {
		t.Fatalf("no code stored yet: %v", err)
	}
	if _, err := a.SetSetupCode(codeSum("7K3M-9QX2-HD4R-T8WA"), false); err != nil {
		t.Fatal(err)
	}
	if _, err := a.UseSetupCode("7K3M-9QX2-HD4R-T8WA", "too short"); !errors.Is(err, errBadPassword) {
		t.Fatalf("short password: %v", err)
	}
	clock = clock.Add(61 * time.Minute)
	if a.SetupCodeValid("7K3M-9QX2-HD4R-T8WA") {
		t.Fatal("expired code still valid")
	}
	clock = clock.Add(-2 * time.Minute)
	gen, err := a.UseSetupCode("7K3M-9QX2-HD4R-T8WA", "a long enough password")
	if err != nil || gen != 1 {
		t.Fatalf("use: %d %v", gen, err)
	}
	if g, ok := a.CheckPassword("a long enough password"); !ok || g != 1 {
		t.Fatal("right password refused")
	}
	if _, ok := a.CheckPassword("a long enough passwore"); ok {
		t.Fatal("wrong password accepted")
	}
	// Stored as PBKDF2 with a salt, readable by root alone, and reloaded.
	raw, _ := os.ReadFile(filepath.Join(dir, "auth.json"))
	if strings.Contains(string(raw), "long enough") || !strings.Contains(string(raw), `"iterations": 1000`) {
		t.Fatalf("auth.json: %s", raw)
	}
	b := newAuth(dir)
	if has, g := b.Status(); !has || g != 1 {
		t.Fatal("state not reloaded")
	}
	if newAuth(t.TempDir()).iterations != 600000 {
		t.Fatal("the real iteration count changed")
	}
}

func TestLimiter(t *testing.T) {
	l := newLimiter()
	clock := time.Now()
	l.now = func() time.Time { return clock }
	for i := 0; i < 5; i++ {
		if l.blocked("2001:db8:1:2::1") {
			t.Fatalf("blocked after %d", i)
		}
		l.failed("2001:db8:1:2::" + string(rune('1'+i))) // same /64
	}
	if !l.blocked("2001:db8:1:2:ffff::9") || l.blocked("2001:db8:1:3::1") {
		t.Fatal("IPv6 addresses must count by /64")
	}
	for i := 0; i < 15; i++ {
		l.failed("198.51.100." + strconv.Itoa(i))
	}
	if !l.blocked("203.0.113.99") {
		t.Fatal("20 failures a minute must stop everyone")
	}
	clock = clock.Add(61 * time.Second)
	if l.blocked("203.0.113.99") || l.blocked("2001:db8:1:2::1") {
		t.Fatal("still blocked after a minute")
	}
	if !l.acquire() || !l.acquire() || l.acquire() {
		t.Fatal("two password checks at once, no more")
	}
	l.release()
	l.release()
}
