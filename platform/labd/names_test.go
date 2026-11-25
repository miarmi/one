package main

import (
	"strings"
	"testing"
)

// A branch's preview name is the branch name itself, so no two branches can
// share a preview; other names get none.
func TestPreviewName(t *testing.T) {
	for _, in := range []string{"dark-mode", "a", "main2", "x" + strings.Repeat("y", 29), "release-2026-10", "new-goal", "2fa"} {
		if got, err := previewName(in); err != nil || got != in {
			t.Errorf("previewName(%q) = %q, %v", in, got, err)
		}
	}
	for _, bad := range []string{"main", "Main", "MAIN", "prod", "PROD", "", "---", "///", "ÜÄÖ", "Dark Mode!", "Dark-Mode",
		"dark_mode", "feature/login", "a--b", "-idea", "idea-", "\u212a-thing", "Über-Idee", "x" + strings.Repeat("y", 30), "release/2026.10"} {
		if got, err := previewName(bad); err == nil {
			t.Errorf("previewName(%q) = %q, want error", bad, got)
		}
	}
}

func TestValidNames(t *testing.T) {
	for _, a := range []string{"hello", "tracker", "ab", "app2"} {
		if !validAppName(a) {
			t.Errorf("%q rejected", a)
		}
	}
	for _, a := range []string{"a", "lab", "www", "api", "mail", "Hello", "my-app", "2app", strings.Repeat("a", 21), "a.b"} {
		if validAppName(a) {
			t.Errorf("%q accepted", a)
		}
	}
	for _, e := range []string{"prod", "dark-mode", "a", "x1"} {
		if !validEnv(e) {
			t.Errorf("env %q rejected", e)
		}
	}
	for _, e := range []string{"main", "", "-x", "x-", "a--b", "Dark", strings.Repeat("a", 31), "a_b"} {
		if validEnv(e) {
			t.Errorf("env %q accepted", e)
		}
	}
}

func TestHosts(t *testing.T) {
	s := Settings{PublicIP: "203.0.113.10"}
	if got := hostFor("hello", "prod", s); got != "hello.203-0-113-10.sslip.io" {
		t.Error(got)
	}
	if got := urlFor("hello", "dark-mode", s); got != "http://dark-mode--hello.203-0-113-10.sslip.io" {
		t.Error(got)
	}
	s.Domain = "example.com"
	if got := urlFor("hello", "prod", s); got != "https://hello.example.com" {
		t.Error(got)
	}
	if hostFor("hello", "prod", Settings{}) != "" {
		t.Error("host without settings")
	}
	for _, ip := range []string{"203.0.113.10", "127.0.0.1"} {
		if !validPublicIPv4(ip) {
			t.Error(ip)
		}
	}
	for _, ip := range []string{"", "::1", "2a01:4f9::1", "1.2.3", "1.2.3.4.5", "example.com", "::ffff:1.2.3.4"} {
		if validPublicIPv4(ip) {
			t.Error(ip)
		}
	}
}

func TestBranchOf(t *testing.T) {
	if branchOf("refs/heads/a/b") != "a/b" || branchOf("refs/tags/v1") != "" || branchOf("main") != "" {
		t.Fatal("branchOf")
	}
}
