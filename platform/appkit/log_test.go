package appkit

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// capture installs a logger writing into a buffer and returns a way to read
// the lines back as maps.
func capture(t *testing.T) func() []map[string]any {
	t.Helper()
	var buf bytes.Buffer
	setLogger(newLogger(&buf, "hello", "prod", "abc1234"))
	return func() []map[string]any {
		var out []map[string]any
		for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
			if line == "" {
				continue
			}
			var m map[string]any
			if err := json.Unmarshal([]byte(line), &m); err != nil {
				t.Fatalf("not JSON: %q: %v", line, err)
			}
			out = append(out, m)
		}
		return out
	}
}

func TestLoggerShape(t *testing.T) {
	lines := capture(t)
	Logger().Info("hello", "n", 1)
	Error("broke", errors.New("boom"), "id", 7)
	got := lines()
	if len(got) != 2 {
		t.Fatalf("want 2 lines, got %d", len(got))
	}
	first := got[0]
	for _, k := range []string{"time", "level", "msg", "app", "env", "version"} {
		if _, ok := first[k]; !ok {
			t.Errorf("missing key %q in %v", k, first)
		}
	}
	if first["app"] != "hello" || first["env"] != "prod" || first["version"] != "abc1234" || first["level"] != "INFO" || first["n"] != float64(1) {
		t.Errorf("first line: %v", first)
	}
	if !strings.HasSuffix(first["time"].(string), "Z") {
		t.Errorf("time should be UTC RFC 3339: %v", first["time"])
	}
	if got[1]["level"] != "ERROR" || got[1]["err"] != "boom" || got[1]["id"] != float64(7) {
		t.Errorf("error line: %v", got[1])
	}
}

func TestDefaultsWithoutEnvironment(t *testing.T) {
	var buf bytes.Buffer
	l := newLogger(&buf, "", "", "")
	l.Info("x")
	var m map[string]any
	if err := json.Unmarshal(buf.Bytes(), &m); err != nil {
		t.Fatal(err)
	}
	if m["env"] != "local" || m["version"] != "dev" {
		t.Errorf("defaults: %v", m)
	}
	if _, ok := m["app"]; ok {
		t.Errorf("no APP_NAME should mean no app key: %v", m)
	}
}

func TestLogRequests(t *testing.T) {
	lines := capture(t)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /ok", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("hello")) })
	mux.HandleFunc("GET /missing", func(w http.ResponseWriter, r *http.Request) { http.Error(w, "no", http.StatusNotFound) })
	mux.HandleFunc("GET /broken", func(w http.ResponseWriter, r *http.Request) { http.Error(w, "db", http.StatusInternalServerError) })
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.RawQuery == "sick" {
			http.Error(w, "db closed", http.StatusServiceUnavailable)
			return
		}
		w.Write([]byte("ok"))
	})
	mux.HandleFunc("GET /panic", func(w http.ResponseWriter, r *http.Request) { panic("oops") })
	h := LogRequests(mux)

	do := func(path, rid string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("GET", path, nil)
		if rid != "" {
			req.Header.Set("X-Request-ID", rid)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}

	rec := do("/ok", "req-1")
	if rec.Header().Get("X-Request-ID") != "req-1" {
		t.Errorf("caller's request id not kept: %q", rec.Header().Get("X-Request-ID"))
	}
	rec = do("/ok", "bad id with spaces")
	if id := rec.Header().Get("X-Request-ID"); id == "bad id with spaces" || len(id) != 16 {
		t.Errorf("a bad request id should be replaced by a random 8-byte hex one, got %q", id)
	}
	do("/missing", "")
	do("/broken", "")
	do("/healthz", "")
	rec = do("/healthz?sick", "")
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("sick health: %d", rec.Code)
	}
	rec = do("/panic", "p-1")
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("a panic should answer 500, got %d", rec.Code)
	}

	got := lines()
	// /ok twice, /missing, /broken, /healthz (503) and the panic: the healthy
	// /healthz is not logged.
	if len(got) != 6 {
		t.Fatalf("want 6 lines, got %d: %v", len(got), got)
	}
	check := func(i int, level, path string, status float64) {
		t.Helper()
		m := got[i]
		if m["level"] != level || m["path"] != path || m["status"] != status || m["msg"] != "request" {
			t.Errorf("line %d: %v", i, m)
		}
		for _, k := range []string{"method", "ms", "bytes", "rid"} {
			if _, ok := m[k]; !ok {
				t.Errorf("line %d missing %q: %v", i, k, m)
			}
		}
	}
	check(0, "INFO", "/ok", 200)
	if got[0]["rid"] != "req-1" || got[0]["bytes"] != float64(5) {
		t.Errorf("first line: %v", got[0])
	}
	check(1, "INFO", "/ok", 200)
	check(2, "WARN", "/missing", 404)
	check(3, "ERROR", "/broken", 500)
	check(4, "ERROR", "/healthz", 503)
	p := got[5]
	if p["level"] != "ERROR" || p["msg"] != "panic in handler" || p["panic"] != "oops" || p["rid"] != "p-1" || p["status"] != float64(500) {
		t.Errorf("panic line: %v", p)
	}
	if at, _ := p["at"].(string); !strings.Contains(at, "log_test.go") {
		t.Errorf("panic site should point at the handler, got %q", at)
	}
}

type failing struct{}

func (failing) Write([]byte) (int, error) { return 0, errors.New("disk full") }

func TestBothWritesToTheSecondAndIgnoresItsFailure(t *testing.T) {
	var first, second bytes.Buffer
	newLogger(both{&first, &second}, "a", "prod", "v").Info("kept")
	if first.String() == "" || first.String() != second.String() {
		t.Fatalf("first %q, second %q", first.String(), second.String())
	}
	first.Reset()
	newLogger(both{&first, failing{}}, "a", "prod", "v").Info("still on stdout")
	if !strings.Contains(first.String(), "still on stdout") {
		t.Fatalf("a failing second writer lost the line: %q", first.String())
	}
}
