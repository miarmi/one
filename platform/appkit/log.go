package appkit

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"runtime"
	"strings"
	"sync"
	"time"
)

// Logging the way every app in the lab does it: one JSON object per line on
// stdout (journald keeps them 30 days), with the app's name, environment and
// version on every line, so the lab's Errors page and its alerts can read
// them without knowing the app. Levels are slog's: DEBUG, INFO, WARN, ERROR.
// Error is the level the lab shows and alerts on, so an app logs at ERROR
// only what someone should look at.

var (
	loggerOnce sync.Once
	logger     *slog.Logger
	alsoTo     io.Writer
)

// AlsoLogTo makes every log line go to w as well as to stdout, so an app can
// keep its own lines (each Write is one whole JSON line). Call it before the
// first Logger(); later calls change nothing. A failing w never stops stdout.
func AlsoLogTo(w io.Writer) {
	if logger == nil {
		alsoTo = w
	}
}

// both writes to stdout first and then to the second writer, whose error is
// dropped: the server's log must not depend on the app's own copy.
type both struct{ first, second io.Writer }

func (b both) Write(p []byte) (int, error) {
	n, err := b.first.Write(p)
	b.second.Write(p)
	return n, err
}

// Logger returns the app's logger: JSON lines on stdout with the keys
// "time", "level", "msg" and the constant "app", "env" and "version" from
// the environment labd sets (APP_NAME, APP_ENV, APP_VERSION; "local" and
// "dev" when run by hand). The first call also makes it slog's default, so
// slog.Info(...) anywhere in the app lands in the same stream.
func Logger() *slog.Logger {
	loggerOnce.Do(func() {
		var w io.Writer = os.Stdout
		if alsoTo != nil {
			w = both{os.Stdout, alsoTo}
		}
		logger = newLogger(w, os.Getenv("APP_NAME"), os.Getenv("APP_ENV"), os.Getenv("APP_VERSION"))
		slog.SetDefault(logger)
	})
	return logger
}

func newLogger(w io.Writer, app, env, version string) *slog.Logger {
	return slog.New(newHandler(w, app, env, version))
}

// setLogger replaces the app's logger; tests use it to capture lines.
func setLogger(l *slog.Logger) {
	loggerOnce.Do(func() {})
	logger = l
	slog.SetDefault(l)
}

func newHandler(w io.Writer, app, env, version string) slog.Handler {
	if env == "" {
		env = "local"
	}
	if version == "" {
		version = "dev"
	}
	h := slog.NewJSONHandler(w, &slog.HandlerOptions{Level: slog.LevelInfo, ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
		if len(groups) == 0 && a.Key == slog.TimeKey {
			return slog.String(slog.TimeKey, a.Value.Time().UTC().Format(time.RFC3339Nano))
		}
		return a
	}})
	attrs := []slog.Attr{slog.String("env", env), slog.String("version", version)}
	if app != "" {
		attrs = append([]slog.Attr{slog.String("app", app)}, attrs...)
	}
	return h.WithAttrs(attrs)
}

// Error logs msg at ERROR with the error's text under "err" and any further
// attributes (key, value pairs as slog takes them).
func Error(msg string, err error, attrs ...any) {
	if err != nil {
		attrs = append([]any{"err", err.Error()}, attrs...)
	}
	Logger().Error(msg, attrs...)
}

// LogRequests logs one line per request: method, path, status, ms (how long
// it took), bytes (of the answer) and rid (the request id, taken from an
// X-Request-ID header or made up and set on the answer). 2xx and 3xx log at
// INFO, 4xx at WARN, 5xx at ERROR. A panic in a handler is logged at ERROR
// with where it happened and answered with a 500 instead of killing the
// connection. The health check is logged only when it does not answer 200,
// so labd's checks every few seconds don't fill the journal.
func LogRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rid := requestID(r)
		w.Header().Set("X-Request-ID", rid)
		sw := &statusWriter{ResponseWriter: w, code: http.StatusOK}
		defer func() {
			if p := recover(); p != nil {
				if p == http.ErrAbortHandler {
					panic(p)
				}
				if !sw.wrote {
					http.Error(sw, "internal error", http.StatusInternalServerError)
				}
				Logger().Error("panic in handler", "panic", fmt.Sprint(p), "at", panicSite(),
					"method", r.Method, "path", r.URL.Path, "status", sw.code, "ms", ms(start), "rid", rid)
				return
			}
			logRequest(r, sw, start, rid)
		}()
		next.ServeHTTP(sw, r)
	})
}

func logRequest(r *http.Request, sw *statusWriter, start time.Time, rid string) {
	if r.URL.Path == "/healthz" && sw.code == http.StatusOK {
		return
	}
	level := slog.LevelInfo
	switch {
	case sw.code >= 500:
		level = slog.LevelError
	case sw.code >= 400:
		level = slog.LevelWarn
	}
	Logger().Log(r.Context(), level, "request",
		"method", r.Method, "path", r.URL.Path, "status", sw.code, "ms", ms(start), "bytes", sw.bytes, "rid", rid)
}

func ms(start time.Time) int64 { return time.Since(start).Milliseconds() }

// requestID takes a sane X-Request-ID from the caller (nginx or a test) or
// makes a random one.
func requestID(r *http.Request) string {
	if id := strings.TrimSpace(r.Header.Get("X-Request-ID")); id != "" && len(id) <= 64 && clean(id) {
		return id
	}
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%x", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}

func clean(s string) bool {
	for _, c := range s {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_' || c == '.') {
			return false
		}
	}
	return true
}

// panicSite names the first frame of the stack outside this package and the
// runtime: where the handler went wrong.
func panicSite() string {
	pcs := make([]uintptr, 32)
	n := runtime.Callers(3, pcs)
	frames := runtime.CallersFrames(pcs[:n])
	for {
		f, more := frames.Next()
		if !strings.HasPrefix(f.Function, "runtime.") && !strings.HasSuffix(f.File, "platform/appkit/log.go") {
			return fmt.Sprintf("%s (%s:%d)", f.Function, f.File, f.Line)
		}
		if !more {
			return "unknown"
		}
	}
}

type statusWriter struct {
	http.ResponseWriter
	code  int
	bytes int
	wrote bool
}

func (s *statusWriter) WriteHeader(code int) {
	if s.wrote {
		return
	}
	s.code = code
	s.wrote = true
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusWriter) Write(b []byte) (int, error) {
	if !s.wrote {
		s.WriteHeader(http.StatusOK)
	}
	n, err := s.ResponseWriter.Write(b)
	s.bytes += n
	return n, err
}

// Flush passes streaming through to the real writer when it supports it.
func (s *statusWriter) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}
