package main

import (
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestPagesDemo serves labd's sign-in, setup and home pages with demo apps,
// the way nginx puts them on the lab's own host, for a UX review:
//
//	LAB_PAGES_DEMO=127.0.0.1:8765 go test -run TestPagesDemo -timeout 0 ./platform/labd
//
// Then open http://127.0.0.1:8765/_lab/setup?code=7K3M-9QX2-HD4R-T8WA (the
// setup code), /_lab/login, and / (the list of apps, once signed in). The
// visitor counts as away from home; with LAB_PAGES_DEMO_HOME=1 as at home.
// It runs for LAB_PAGES_DEMO_MINUTES (default 60).
func TestPagesDemo(t *testing.T) {
	addr := os.Getenv("LAB_PAGES_DEMO")
	if addr == "" {
		t.Skip("set LAB_PAGES_DEMO=127.0.0.1:8765 to serve the pages for a review")
	}
	h := newHarness(t)
	code, _, raw := h.call("POST", "/api/settings", []byte(`{"domain":"","public_ip":"127.0.0.1","allow_ips":["`+homeIP+`"]}`), refMain)
	mustStatus(t, "settings", code, 200, raw)
	for _, d := range []struct{ app, env, ref, title string }{
		{"hello", "prod", refMain, "Hello"},
		{"tracker", "prod", refMain, "Habit tracker"},
		{"tracker", "new-goal", "refs/heads/new-goal", "Habit tracker"},
		{"helpers", "helpers-part1", "refs/heads/helpers-part1", "Helper scripts"},
	} {
		code, _, raw = h.call("POST", "/api/deploy?app="+d.app+"&env="+d.env+"&commit=1234567",
			bundleOf(t, testApps.good, d.app, `,"title":"`+d.title+`"`), d.ref)
		mustStatus(t, "deploy "+d.app, code, 200, raw)
	}
	code, _, raw = h.call("POST", "/api/settings", []byte(`{"domain":"`+testDomain+`","public_ip":"127.0.0.1","allow_ips":["`+homeIP+`"]}`), refMain)
	mustStatus(t, "domain", code, 200, raw)
	code, _, raw = h.call("POST", "/api/setup-code", []byte(`{"code_sha256":"`+codeSum("7K3M-9QX2-HD4R-T8WA")+`"}`), refMain)
	mustStatus(t, "setup code", code, 200, raw)

	target, _ := url.Parse(h.ts.URL)
	ip := awayIP
	if os.Getenv("LAB_PAGES_DEMO_HOME") == "1" {
		ip = homeIP
	}
	here := "http://" + addr
	proxy := &httputil.ReverseProxy{
		Rewrite: func(r *httputil.ProxyRequest) {
			r.SetURL(target)
			r.Out.Host = testDomain
			r.Out.Header.Set("X-Lab-Scheme", "https")
			r.Out.Header.Set("X-Real-IP", ip)
		},
		// What a browser on http://<addr> needs: addresses and cookies for
		// this address instead of the lab's https name.
		ModifyResponse: func(resp *http.Response) error {
			if loc := resp.Header.Get("Location"); strings.HasPrefix(loc, "https://"+testDomain) {
				resp.Header.Set("Location", here+strings.TrimPrefix(loc, "https://"+testDomain))
			}
			cookies := resp.Header.Values("Set-Cookie")
			resp.Header.Del("Set-Cookie")
			for _, c := range cookies {
				c = strings.ReplaceAll(c, "; Domain="+testDomain, "")
				c = strings.ReplaceAll(c, "; Secure", "")
				resp.Header.Add("Set-Cookie", c)
			}
			resp.Header.Set("Content-Security-Policy", strings.ReplaceAll(resp.Header.Get("Content-Security-Policy"), "form-action 'self'", "form-action 'self' "+here))
			return nil
		},
	}
	minutes := 60
	if m, err := strconv.Atoi(os.Getenv("LAB_PAGES_DEMO_MINUTES")); err == nil && m > 0 {
		minutes = m
	}
	srv := &http.Server{Addr: addr, Handler: proxy}
	go func() {
		time.Sleep(time.Duration(minutes) * time.Minute)
		srv.Close()
	}()
	t.Logf("labd's pages: %s/_lab/setup?code=7K3M-9QX2-HD4R-T8WA, %s/_lab/login, %s/ (for %d minutes)", here, here, here, minutes)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		t.Fatal(err)
	}
}
