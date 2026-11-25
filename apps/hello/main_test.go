package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
)

func newTestApp(t *testing.T, env, version string) (*app, http.Handler) {
	t.Helper()
	st, err := openStore(filepath.Join(t.TempDir(), "hello.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	a := newApp(st, env, version)
	return a, a.routes()
}

func do(h http.Handler, method, path string, form url.Values, hdr map[string]string) *httptest.ResponseRecorder {
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req := httptest.NewRequest(method, path, body)
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestAddListDelete(t *testing.T) {
	a, h := newTestApp(t, "", "")
	for _, text := range []string{"first", "  second  "} {
		rec := do(h, "POST", "/notes", url.Values{"text": {text}}, nil)
		if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/" {
			t.Fatalf("add %q: %d %s", text, rec.Code, rec.Header().Get("Location"))
		}
	}
	notes, _ := a.store.List(t.Context(), 10)
	if len(notes) != 2 || notes[0].Text != "second" || notes[1].Text != "first" {
		t.Fatalf("notes: %+v", notes)
	}
	page := do(h, "GET", "/", nil, nil).Body.String()
	if strings.Index(page, "second") > strings.Index(page, "first") {
		t.Fatal("newest note is not first on the page")
	}
	rec := do(h, "POST", "/notes/"+itoa(notes[0].ID)+"/delete", url.Values{}, nil)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("delete: %d", rec.Code)
	}
	notes, _ = a.store.List(t.Context(), 10)
	if len(notes) != 1 || notes[0].Text != "first" {
		t.Fatalf("after delete: %+v", notes)
	}
	if rec := do(h, "POST", "/notes/abc/delete", url.Values{}, nil); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad id: %d", rec.Code)
	}
}

func itoa(n int64) string { b, _ := json.Marshal(n); return string(b) }

func TestValidation(t *testing.T) {
	a, h := newTestApp(t, "", "")
	for _, text := range []string{"", "   ", "\t\n"} {
		rec := do(h, "POST", "/notes", url.Values{"text": {text}}, nil)
		if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "Write something first.") {
			t.Fatalf("empty %q: %d", text, rec.Code)
		}
	}
	ok280 := strings.Repeat("é", 280) // 560 bytes, 280 characters
	if rec := do(h, "POST", "/notes", url.Values{"text": {ok280}}, nil); rec.Code != http.StatusSeeOther {
		t.Fatalf("280 characters refused: %d", rec.Code)
	}
	rec := do(h, "POST", "/notes", url.Values{"text": {strings.Repeat("é", 281)}}, nil)
	if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "Keep it under 280 characters.") {
		t.Fatalf("281 characters: %d", rec.Code)
	}
	notes, _ := a.store.List(t.Context(), 10)
	if len(notes) != 1 {
		t.Fatalf("stored %d notes", len(notes))
	}
}

// Form posts from other sites are refused, whether the browser says so with
// Sec-Fetch-Site or only with Origin (also the "null" origin of a sandboxed
// frame).
func TestCrossSitePostRefused(t *testing.T) {
	_, h := newTestApp(t, "", "")
	for name, hdr := range map[string]map[string]string{
		"other origin":        {"Origin": "https://evil.example"},
		"null origin":         {"Origin": "null"},
		"cross-site fetch":    {"Sec-Fetch-Site": "cross-site", "Origin": "https://evil.example"},
		"same-site subdomain": {"Sec-Fetch-Site": "same-site", "Origin": "http://other.example.com"},
	} {
		if rec := do(h, "POST", "/notes", url.Values{"text": {"x"}}, hdr); rec.Code != http.StatusForbidden {
			t.Errorf("%s: %d", name, rec.Code)
		}
	}
	for name, hdr := range map[string]map[string]string{
		"same origin":       {"Origin": "http://example.com"}, // httptest's Host is example.com
		"same-origin fetch": {"Sec-Fetch-Site": "same-origin", "Origin": "http://example.com"},
		"no browser":        nil,
	} {
		if rec := do(h, "POST", "/notes", url.Values{"text": {"x"}}, hdr); rec.Code != http.StatusSeeOther {
			t.Errorf("%s: %d", name, rec.Code)
		}
	}
}

func TestEnvironmentAndVersion(t *testing.T) {
	cases := []struct{ env, version, wantEnv, wantVersion string }{
		{"", "", "Environment: development", "Version: dev"},
		{"prod", "0123456789abcdef", "Environment: production", "Version: 0123456"},
		{"dark-mode", "abc1234", "Environment: preview: dark-mode", "Version: abc1234"},
	}
	for _, c := range cases {
		_, h := newTestApp(t, c.env, c.version)
		page := do(h, "GET", "/", nil, nil).Body.String()
		if !strings.Contains(page, c.wantEnv) || !strings.Contains(page, c.wantVersion) {
			t.Errorf("%q/%q: page lacks %q or %q", c.env, c.version, c.wantEnv, c.wantVersion)
		}
	}
}

func TestPageEscapesNotes(t *testing.T) {
	_, h := newTestApp(t, "", "")
	do(h, "POST", "/notes", url.Values{"text": {`<script>alert(1)</script>`}}, nil)
	page := do(h, "GET", "/", nil, nil).Body.String()
	if strings.Contains(page, "<script>alert") {
		t.Fatal("note not escaped")
	}
}

func TestHealthManifestIconsHeaders(t *testing.T) {
	_, h := newTestApp(t, "", "")
	if rec := do(h, "GET", "/healthz", nil, nil); rec.Code != 200 || rec.Body.String() != "ok" {
		t.Fatalf("healthz: %d %q", rec.Code, rec.Body.String())
	}
	rec := do(h, "GET", "/manifest.webmanifest", nil, nil)
	var m map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil || m["name"] != "Hello" || m["short_name"] != "Hello" {
		t.Fatalf("manifest: %s", rec.Body.String())
	}
	for _, p := range []string{"/apple-touch-icon.png", "/icon-192.png", "/icon-512.png"} {
		rec := do(h, "GET", p, nil, nil)
		if rec.Code != 200 || rec.Header().Get("Content-Type") != "image/png" || !strings.HasPrefix(rec.Body.String(), "\x89PNG") {
			t.Fatalf("%s: %d %s", p, rec.Code, rec.Header().Get("Content-Type"))
		}
	}
	if rec := do(h, "GET", "/static/style.css", nil, nil); rec.Code != 200 || !strings.Contains(rec.Body.String(), "overflow-wrap") {
		t.Fatalf("css: %d", rec.Code)
	}
	page := do(h, "GET", "/", nil, nil)
	if page.Header().Get("X-Content-Type-Options") != "nosniff" || !strings.Contains(page.Header().Get("Content-Security-Policy"), "default-src 'none'") {
		t.Fatal("security headers missing")
	}
	if rec := do(h, "GET", "/nope", nil, nil); rec.Code != 404 {
		t.Fatalf("unknown path: %d", rec.Code)
	}
}
