package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSessions(t *testing.T) {
	dir := t.TempDir()
	s := newSessions(dir)
	clock := time.Now()
	s.now = func() time.Time { return clock }
	const host = "dark-mode--hello.203-0-113-10.sslip.io"
	cookie, exp, err := s.Issue(15*time.Minute, host)
	if err != nil {
		t.Fatal(err)
	}
	if !s.Valid(cookie, host) || !s.Valid(cookie, "Dark-Mode--Hello.203-0-113-10.sslip.io.") {
		t.Fatal("fresh cookie rejected")
	}
	// A pass opens only the host it was issued for.
	for _, other := range []string{"hello.203-0-113-10.sslip.io", "dark-mode--other.203-0-113-10.sslip.io", ""} {
		if s.Valid(cookie, other) {
			t.Errorf("pass for %s opens %q", host, other)
		}
	}
	if _, _, err := s.Issue(time.Minute, ""); err == nil {
		t.Error("a pass without a host was issued")
	}
	if exp.Sub(clock) > 15*time.Minute || exp.Sub(clock) < 14*time.Minute {
		t.Fatalf("expiry %v", exp.Sub(clock))
	}
	info, err := os.Stat(filepath.Join(dir, "secret"))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("secret file: %v %v", info, err)
	}
	// The same secret is used after a restart.
	s2 := newSessions(dir)
	s2.now = s.now
	if !s2.Valid(cookie, host) {
		t.Fatal("cookie rejected after restart")
	}
	// Tampering, garbage, other secrets and expiry are rejected.
	first, sig, _ := strings.Cut(cookie, ".")
	flipped := []byte(first)
	flipped[5] ^= 1
	for name, bad := range map[string]string{
		"empty":          "",
		"no dot":         first,
		"tampered":       string(flipped) + "." + sig,
		"bad signature":  first + "." + strings.Repeat("A", 43),
		"garbage":        "x.y",
		"extra":          cookie + "x",
		"other instance": func() string { o := newSessions(t.TempDir()); c, _, _ := o.Issue(time.Hour, host); return c }(),
	} {
		if s.Valid(bad, host) {
			t.Errorf("%s accepted", name)
		}
	}
	clock = clock.Add(16 * time.Minute)
	if s.Valid(cookie, host) {
		t.Fatal("expired cookie accepted")
	}
}

func TestNormalizeAllowIP(t *testing.T) {
	ok := map[string]string{
		"203.0.113.7":      "203.0.113.7",
		"198.51.100.0/24":  "198.51.100.0/24",
		"198.51.100.7/24":  "198.51.100.0/24",
		"2001:db8::1":      "2001:db8::1",
		"2001:db8::/32":    "2001:db8::/32",
		"::ffff:192.0.2.1": "192.0.2.1",
	}
	for in, want := range ok {
		if got, good := normalizeAllowIP(in); !good || got != want {
			t.Errorf("%q -> %q %v, want %q", in, got, good, want)
		}
	}
	for _, bad := range []string{"", "0.0.0.0/0", "::/0", "1.2.3", "example.com", "1.2.3.4; deny all", "1.2.3.4\n", "all"} {
		if got, good := normalizeAllowIP(bad); good {
			t.Errorf("%q accepted as %q", bad, got)
		}
	}
}
