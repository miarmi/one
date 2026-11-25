package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	testTenant = "11111111-2222-3333-4444-555555555555"
	testClient = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	secretTok  = "ACCESS-TOKEN-VALUE"
	secretRef  = "REFRESH-TOKEN-VALUE"
)

// fakeMS is a scripted Microsoft: a login endpoint and a Graph endpoint.
type fakeMS struct {
	mu          sync.Mutex
	pollAnswers []string // errors to answer at the token endpoint before success; "" = success
	refreshErr  string
	sent        []graphMail
	srv         *httptest.Server
	tokenCalls  int
	box         *fakeGraphBox // the mailbox behind /me/mailFolders and /me/messages/{id}

	replies     []fakeReply // every call of the reply address
	replyStatus int         // when set, the reply call answers this status with an error body
	writes      []string    // every request that is not a read, as "METHOD /path"
}

// fakeReply is one call of POST /me/messages/{id}/reply as the fake saw it.
type fakeReply struct {
	ID     string
	Prefer string
	Body   map[string]any
}

func startFakeMS(t *testing.T) *fakeMS {
	t.Helper()
	f := &fakeMS{box: newFakeGraphBox()}
	mux := http.NewServeMux()
	mux.HandleFunc("/me/mailFolders", f.box.serve)
	mux.HandleFunc("/me/mailFolders/", f.box.serve)
	mux.HandleFunc("/me/messages/", f.box.serve)
	mux.HandleFunc("POST /"+testTenant+"/oauth2/v2.0/devicecode", func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		if r.Form.Get("client_id") != testClient || !strings.Contains(r.Form.Get("scope"), "Mail.Send") || strings.Contains(r.Form.Get("scope"), "client_secret") {
			http.Error(w, "bad", 400)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"device_code": "DEVICE-SECRET", "user_code": "ABCD1234", "verification_uri": "https://microsoft.com/devicelogin", "expires_in": 900, "interval": 1})
	})
	mux.HandleFunc("POST /"+testTenant+"/oauth2/v2.0/token", func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		f.mu.Lock()
		defer f.mu.Unlock()
		f.tokenCalls++
		if r.Form.Get("client_secret") != "" {
			t.Error("a client secret was sent")
		}
		if r.Form.Get("grant_type") == "refresh_token" {
			if f.refreshErr != "" {
				w.WriteHeader(400)
				json.NewEncoder(w).Encode(map[string]any{"error": f.refreshErr, "error_description": "AADSTS70008: gone\r\nTrace ID: x"})
				return
			}
			if r.Form.Get("refresh_token") != secretRef {
				t.Error("wrong refresh token sent")
			}
			json.NewEncoder(w).Encode(map[string]any{"access_token": secretTok + "-2", "expires_in": 3600})
			return
		}
		if r.Form.Get("device_code") != "DEVICE-SECRET" {
			t.Error("wrong device code")
		}
		if len(f.pollAnswers) > 0 {
			e := f.pollAnswers[0]
			f.pollAnswers = f.pollAnswers[1:]
			if e != "" {
				w.WriteHeader(400)
				json.NewEncoder(w).Encode(map[string]any{"error": e, "error_description": "waiting"})
				return
			}
		}
		json.NewEncoder(w).Encode(map[string]any{"access_token": secretTok, "refresh_token": secretRef, "expires_in": 3600})
	})
	mux.HandleFunc("GET /me", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"mail": "owner@example.test", "userPrincipalName": "owner@x.onmicrosoft.com"})
	})
	mux.HandleFunc("GET /me/messages", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"value":[{"receivedDateTime":"2026-11-19T09:30:00Z","subject":"Offer","conversationId":"AAQkConv1","from":{"emailAddress":{"name":"Jana","address":"jana@example.test"}}}]}`)
	})
	mux.HandleFunc("GET /me/calendarView", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"value":[{"subject":"Site visit","isAllDay":false,"start":{"dateTime":"2026-11-20T08:00:00.0000000","timeZone":"UTC"}}]}`)
	})
	mux.HandleFunc("POST /me/messages/{id}/reply", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		f.replies = append(f.replies, fakeReply{ID: r.PathValue("id"), Prefer: r.Header.Get("Prefer"), Body: body})
		status := f.replyStatus
		f.mu.Unlock()
		if status != 0 {
			gmError(w, status, "ErrorAccessDenied")
			return
		}
		w.WriteHeader(202)
	})
	mux.HandleFunc("POST /me/sendMail", func(w http.ResponseWriter, r *http.Request) {
		var m graphMail
		json.NewDecoder(r.Body).Decode(&m)
		f.mu.Lock()
		f.sent = append(f.sent, m)
		f.mu.Unlock()
		w.WriteHeader(202)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer "+secretTok) && strings.HasPrefix(r.URL.Path, "/me") {
			w.WriteHeader(401)
		}
	})
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" && !strings.Contains(r.URL.Path, "/oauth2/") {
			f.mu.Lock()
			f.writes = append(f.writes, r.Method+" "+r.URL.Path)
			f.mu.Unlock()
		}
		mux.ServeHTTP(w, r)
	}))
	f.box.base = f.srv.URL
	t.Cleanup(f.srv.Close)
	return f
}

func useFakeMS(t *testing.T, a *app) *fakeMS {
	f := startFakeMS(t)
	a.graph = graphConfig{LoginBase: f.srv.URL, APIBase: f.srv.URL, HTTP: f.srv.Client(), PollEvery: 20 * time.Millisecond}
	return f
}

func waitState(t *testing.T, a *app, want string) flowView {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if v := a.flow.view(); v.State == want {
			return v
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("flow never reached %q, is %+v", want, a.flow.view())
	return flowView{}
}

func saveIDs(t *testing.T, h http.Handler) {
	t.Helper()
	rec := post(h, "/connectors/graph/save", url.Values{"tenant": {testTenant}, "client": {testClient}})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("save ids: %d %s", rec.Code, rec.Body.String())
	}
}

func TestDeviceCodeFlowPendingThenSignedIn(t *testing.T) {
	a, h := newTestApp(t)
	f := useFakeMS(t, a)
	f.pollAnswers = []string{"authorization_pending", "slow_down", "authorization_pending", ""}

	// Ids are checked before anything is asked of Microsoft.
	if rec := post(h, "/connectors/graph/signin", nil); rec.Code != http.StatusBadRequest {
		t.Fatalf("sign in without ids: %d", rec.Code)
	}
	if rec := post(h, "/connectors/graph/save", url.Values{"tenant": {"no spaces allowed"}, "client": {"x"}}); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad ids accepted: %d", rec.Code)
	}
	saveIDs(t, h)
	if rec := post(h, "/connectors/graph/signin", nil); rec.Code != http.StatusSeeOther {
		t.Fatalf("sign in: %d %s", rec.Code, rec.Body.String())
	}
	v := a.flow.view()
	if v.State != "pending" || v.UserCode != "ABCD1234" || v.Verify != "https://microsoft.com/devicelogin" {
		t.Fatalf("flow right after start: %+v", v)
	}
	page := get(h, "/connectors").Body.String()
	if !strings.Contains(page, "ABCD1234") || !strings.Contains(page, "https://microsoft.com/devicelogin") || strings.Contains(page, "DEVICE-SECRET") {
		t.Fatal("the page must show the code and address, and never the device code")
	}
	waitState(t, a, "done")

	// The status address the page script polls says the same.
	st := get(h, "/connectors/graph/status")
	if !strings.Contains(st.Body.String(), `"state":"done"`) || strings.Contains(st.Body.String(), "DEVICE-SECRET") {
		t.Fatalf("status: %s", st.Body.String())
	}

	path := filepath.Join(a.secretsDir, graphSecretFile)
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("graph.json: %v %v", err, info)
	}
	page = get(h, "/connectors").Body.String()
	if !strings.Contains(page, "Connected as <b>owner@example.test</b>") {
		t.Fatal("not shown as connected")
	}
	for _, s := range []string{secretTok, secretRef, "DEVICE-SECRET"} {
		if strings.Contains(page, s) {
			t.Fatalf("%s is on the page", s)
		}
	}
	if f.tokenCalls != 4 {
		t.Errorf("token endpoint called %d times, want 4 (3 waits and a success)", f.tokenCalls)
	}
}

func TestDeviceCodeDeclinedAndExpired(t *testing.T) {
	for code, want := range map[string]string{"authorization_declined": "declined", "expired_token": "ran out"} {
		a, h := newTestApp(t)
		f := useFakeMS(t, a)
		f.pollAnswers = []string{code}
		saveIDs(t, h)
		post(h, "/connectors/graph/signin", nil)
		v := waitState(t, a, "error")
		if !strings.Contains(v.Message, want) {
			t.Errorf("%s: message %q", code, v.Message)
		}
		if _, err := os.Stat(filepath.Join(a.secretsDir, graphSecretFile)); err == nil {
			t.Errorf("%s: a sign-in was stored", code)
		}
		if page := get(h, "/connectors").Body.String(); !strings.Contains(page, want) {
			t.Errorf("%s: the page does not say why", code)
		}
	}
}

func signedIn(t *testing.T) (*app, http.Handler, *fakeMS) {
	a, h := newTestApp(t)
	f := useFakeMS(t, a)
	saveIDs(t, h)
	post(h, "/connectors/graph/signin", nil)
	waitState(t, a, "done")
	return a, h, f
}

func TestGraphTestNowShowsMailAndCalendar(t *testing.T) {
	a, h, _ := signedIn(t)
	post(h, "/connectors/graph/test", nil)
	last, ok := a.store.LastTest(t.Context(), graphConn)
	if !ok || !last.OK {
		t.Fatalf("last: %+v", last)
	}
	for _, want := range []string{"Signed in as owner@example.test", "Jana <jana@example.test>", "Offer", "conversation AAQkConv1", "Site visit", "20 Nov 2026 09:00"} {
		if !strings.Contains(last.Result, want) {
			t.Errorf("missing %q in:\n%s", want, last.Result)
		}
	}
	if strings.Contains(last.Result, secretTok) {
		t.Fatal("token in the result")
	}
	if page := get(h, "/connectors").Body.String(); !strings.Contains(page, "Site visit") {
		t.Fatal("the last test is not shown after a reload")
	}
}

func TestGraphTestMailGoesOnlyToSignedInAddress(t *testing.T) {
	a, h, f := signedIn(t)
	// A request that names another recipient changes nothing.
	post(h, "/connectors/graph/send", url.Values{"to": {"evil@example.test"}, "address": {"evil@example.test"}, "recipient": {"evil@example.test"}})
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.sent) != 1 {
		t.Fatalf("sent %d mails", len(f.sent))
	}
	m := f.sent[0].Message
	if len(m.ToRecipients) != 1 || m.ToRecipients[0].E.Address != "owner@example.test" || m.Body.ContentType != "Text" {
		t.Fatalf("message: %+v", m)
	}
	raw, _ := json.Marshal(f.sent[0])
	if strings.Contains(string(raw), "evil") || strings.Contains(string(raw), "ccRecipients") || strings.Contains(string(raw), "bccRecipients") {
		t.Fatalf("body: %s", raw)
	}
	last, _ := a.store.LastTest(t.Context(), graphConn+"-send")
	if !last.OK || !strings.HasPrefix(last.Result, "Sent ") {
		t.Fatalf("result: %+v", last)
	}
}

func TestGraphTokenIsRefreshedWhenNeeded(t *testing.T) {
	a, h, f := signedIn(t)
	// Make the stored access token about to expire.
	var sec graphSecret
	readSecret(a.secretsDir, graphSecretFile, &sec)
	sec.AccessToken, sec.ExpiresAt = "old", time.Now().Add(30*time.Second).Unix()
	writeSecret(a.secretsDir, graphSecretFile, sec)
	tok, _, err := a.graphAccess(t.Context())
	if err != nil || tok != secretTok+"-2" {
		t.Fatalf("token %q err %v", tok, err)
	}
	readSecret(a.secretsDir, graphSecretFile, &sec)
	if sec.RefreshToken != secretRef || sec.AccessToken != secretTok+"-2" {
		t.Fatal("the refreshed sign-in was not kept")
	}
	if info, _ := os.Stat(filepath.Join(a.secretsDir, graphSecretFile)); info.Mode().Perm() != 0o600 {
		t.Fatal("mode changed on refresh")
	}

	// When Microsoft no longer accepts the refresh token, the page says to sign in.
	sec.ExpiresAt = 1
	writeSecret(a.secretsDir, graphSecretFile, sec)
	f.mu.Lock()
	f.refreshErr = "invalid_grant"
	f.mu.Unlock()
	post(h, "/connectors/graph/test", nil)
	last, _ := a.store.LastTest(t.Context(), graphConn)
	if last.OK || !strings.Contains(last.Result, "Needs login") {
		t.Fatalf("result: %+v", last)
	}
}

func TestGraphDisconnectRemovesTheSignIn(t *testing.T) {
	a, h, _ := signedIn(t)
	post(h, "/connectors/graph/disconnect", nil)
	if _, err := os.Stat(filepath.Join(a.secretsDir, graphSecretFile)); err == nil {
		t.Fatal("sign-in file left")
	}
	if a.flow.view().State != "" {
		t.Fatal("flow not reset")
	}
}

func TestPagesAndHeaders(t *testing.T) {
	a, h := newTestApp(t)
	_ = a
	for path, want := range map[string][]string{
		"/":           {"<title>ONE</title>", "Nothing needs you", "Nothing here yet.", `href="/connectors"`, "Environment: development"},
		"/connectors": {"<title>Connectors</title>", `id="page-title">Connectors</h2>`, "Gmail address", "App password", ">Connect<", "Directory (tenant) ID", "Application (client) ID", ">Save<", "Free space", "Not tested yet."},
	} {
		rec := get(h, path)
		if rec.Code != 200 {
			t.Fatalf("%s: %d", path, rec.Code)
		}
		body := rec.Body.String()
		for _, w := range want {
			if !strings.Contains(body, w) {
				t.Errorf("%s lacks %q", path, w)
			}
		}
		csp := rec.Header().Get("Content-Security-Policy")
		if !strings.Contains(csp, "script-src 'self'") || strings.Contains(csp, "unsafe-inline") {
			t.Errorf("%s: csp %q", path, csp)
		}
		if strings.Contains(body, " onclick=") || strings.Contains(body, "<script>") || strings.Contains(body, "style=") {
			t.Errorf("%s has inline script or style", path)
		}
	}
	if rec := get(h, "/healthz"); rec.Code != 200 || rec.Body.String() != "ok" {
		t.Fatal("healthz")
	}
	// A request marked as coming from another site is refused.
	req := httptest.NewRequest("POST", "/connectors/gmail/disconnect", nil)
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("cross-site post: %d", rec.Code)
	}
}

func TestEnvironmentLines(t *testing.T) {
	for env, want := range map[string]string{"prod": "Environment: production", "my-branch": "Environment: preview: my-branch", "": "Environment: development"} {
		st, _ := openStore(filepath.Join(t.TempDir(), "x.db"))
		a := newApp(st, t.TempDir(), env, "")
		if body := get(a.routes(), "/").Body.String(); !strings.Contains(body, want) {
			t.Errorf("%q: no %q", env, want)
		}
		st.Close()
	}
}

func TestServerSectionAndRecordedTest(t *testing.T) {
	a, h := newTestApp(t)
	post(h, "/connectors/server/test", nil)
	last, ok := a.store.LastTest(t.Context(), "server")
	if !ok || !last.OK || !strings.Contains(last.Result, "Free space:") {
		t.Fatalf("%+v", last)
	}
}
