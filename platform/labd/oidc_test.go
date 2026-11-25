package main

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestVerifyAcceptsValidToken(t *testing.T) {
	ti := newTestIssuer(t)
	v := ti.verifier()
	aud := apiAudience("GET", "/api/status", sha256Hex(nil))
	c, err := v.Verify(ti.token(t, baseClaims("refs/heads/main", aud)))
	if err != nil {
		t.Fatal(err)
	}
	if !c.hasAudience(aud) || c.Ref != "refs/heads/main" {
		t.Fatalf("unexpected claims %+v", c)
	}
	if err := v.Consume(c); err != nil {
		t.Fatal(err)
	}
	if err := v.Consume(c); err == nil {
		t.Fatal("a token was accepted twice")
	}
}

func TestVerifyRejections(t *testing.T) {
	ti := newTestIssuer(t)
	v := ti.verifier()
	other, _ := rsa.GenerateKey(rand.Reader, 2048)
	small, _ := rsa.GenerateKey(rand.Reader, 1024)
	now := time.Now().Unix()
	mod := func(f func(map[string]any)) map[string]any {
		c := baseClaims("refs/heads/main", "x")
		f(c)
		return c
	}
	hdr := map[string]any{"alg": "RS256", "kid": ti.kid}
	cases := map[string]string{
		"wrong repository": ti.token(t, mod(func(c map[string]any) { c["repository"] = "evil/lab" })),
		"wrong issuer":     ti.token(t, mod(func(c map[string]any) { c["iss"] = "https://example.com" })),
		"expired":          ti.token(t, mod(func(c map[string]any) { c["exp"] = now - 600 })),
		"no exp":           ti.token(t, mod(func(c map[string]any) { delete(c, "exp") })),
		"future iat":       ti.token(t, mod(func(c map[string]any) { c["iat"] = now + 600 })),
		"future nbf":       ti.token(t, mod(func(c map[string]any) { c["nbf"] = now + 600 })),
		"no jti":           ti.token(t, mod(func(c map[string]any) { delete(c, "jti") })),
		"other key":        sign(t, other, hdr, baseClaims("refs/heads/main", "x")),
		"small key":        sign(t, small, hdr, baseClaims("refs/heads/main", "x")),
		"unknown kid":      sign(t, ti.key, map[string]any{"alg": "RS256", "kid": "nope"}, baseClaims("refs/heads/main", "x")),
		"alg none":         unsigned(map[string]any{"alg": "none", "kid": ti.kid}, baseClaims("refs/heads/main", "x")),
		"alg HS256":        hs256(t, ti, baseClaims("refs/heads/main", "x")),
		"two parts":        "abc.def",
		"padding":          ti.token(t, baseClaims("refs/heads/main", "x")) + "==",
	}
	tampered := ti.token(t, baseClaims("refs/heads/idea", "x"))
	parts := strings.Split(tampered, ".")
	forged := baseClaims("refs/heads/main", "x")
	fb, _ := json.Marshal(forged)
	cases["tampered payload"] = parts[0] + "." + base64.RawURLEncoding.EncodeToString(fb) + "." + parts[2]

	for name, tok := range cases {
		if _, err := v.Verify(tok); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func unsigned(header, claims map[string]any) string {
	h, _ := json.Marshal(header)
	c, _ := json.Marshal(claims)
	return base64.RawURLEncoding.EncodeToString(h) + "." + base64.RawURLEncoding.EncodeToString(c) + "."
}

// hs256 signs with the public modulus as an HMAC key: the classic alg confusion.
func hs256(t *testing.T, ti *testIssuer, claims map[string]any) string {
	h, _ := json.Marshal(map[string]any{"alg": "HS256", "kid": ti.kid})
	c, _ := json.Marshal(claims)
	input := base64.RawURLEncoding.EncodeToString(h) + "." + base64.RawURLEncoding.EncodeToString(c)
	mac := hmac.New(sha256.New, ti.key.PublicKey.N.Bytes())
	mac.Write([]byte(input))
	return input + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func TestJWKSCaching(t *testing.T) {
	ti := newTestIssuer(t)
	v := ti.verifier()
	clock := time.Now()
	v.now = func() time.Time { return clock }
	for i := 0; i < 3; i++ {
		if _, err := v.Verify(ti.token(t, baseClaims("refs/heads/main", "x"))); err != nil {
			t.Fatal(err)
		}
	}
	if ti.hits != 1 {
		t.Fatalf("JWKS fetched %d times, want 1", ti.hits)
	}
	// Unknown kids don't hammer the issuer.
	for i := 0; i < 3; i++ {
		v.Verify(sign(t, ti.key, map[string]any{"alg": "RS256", "kid": "rotated"}, baseClaims("refs/heads/main", "x")))
	}
	if ti.hits != 1 {
		t.Fatalf("unknown kid refetched within a minute (%d fetches)", ti.hits)
	}
	clock = clock.Add(11 * time.Minute)
	v.Verify(ti.token(t, baseClaims("refs/heads/main", "x")))
	if ti.hits != 2 {
		t.Fatalf("cache not refreshed after 10 minutes (%d fetches)", ti.hits)
	}
}

func TestAudienceFormats(t *testing.T) {
	body := []byte("hello")
	if got, want := platformAudience(sha256Hex(body)), "lab-platform:sha256:"+sha256Hex(body); got != want {
		t.Fatalf("platform audience %q, want %q", got, want)
	}
	// Independent computation of the lab-api audience.
	inner := "POST /api/deploy?app=hello&env=prod&commit=abc1234\n" + sha256Hex(body)
	want := "lab-api:sha256:" + sha256Hex([]byte(inner))
	if got := apiAudience("POST", "/api/deploy?app=hello&env=prod&commit=abc1234", sha256Hex(body)); got != want {
		t.Fatalf("api audience %q, want %q", got, want)
	}
	if apiAudience("POST", "/api/deploy?app=hello&env=prod", sha256Hex(body)) == want {
		t.Fatal("audience does not depend on the query string")
	}
}

// The seed and labd must agree on the platform audience.
func TestPlatformAudienceMatchesSeed(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not available")
	}
	body := []byte("some labd binary")
	script := `import hashlib,sys,importlib.util
spec=importlib.util.spec_from_file_location("seed","../seed/seed.py")
seed=importlib.util.module_from_spec(spec); spec.loader.exec_module(seed)
d=hashlib.sha256(sys.stdin.buffer.read()).hexdigest()
print(seed.expected_digest({"aud":"lab-platform:sha256:"+d})==d)`
	cmd := exec.Command("python3", "-c", script)
	cmd.Stdin = strings.NewReader(string(body))
	out, err := cmd.CombinedOutput()
	if err != nil || strings.TrimSpace(string(out)) != "True" {
		t.Fatalf("seed disagrees: %s %v", out, err)
	}
	if !strings.HasPrefix(platformAudience(sha256Hex(body)), "lab-platform:sha256:") {
		t.Fatal("prefix changed")
	}
}

func TestAudienceListClaim(t *testing.T) {
	ti := newTestIssuer(t)
	v := ti.verifier()
	c := baseClaims("refs/heads/main", "")
	c["aud"] = []string{"other", "lab-api:sha256:abc"}
	claims, err := v.Verify(ti.token(t, c))
	if err != nil {
		t.Fatal(err)
	}
	if !claims.hasAudience("lab-api:sha256:abc") || claims.hasAudience("lab-api:sha256:abd") {
		t.Fatal("audience list handled wrongly")
	}
}

// Used tokens are remembered across restarts, so a captured request can't be
// replayed after one (security review L2).
func TestUsedTokensSurviveRestart(t *testing.T) {
	ti := newTestIssuer(t)
	state := filepath.Join(t.TempDir(), "tokens.json")
	v := ti.verifier()
	v.statePath = state
	tok := ti.token(t, baseClaims("refs/heads/main", "x"))
	c, err := v.Verify(tok)
	if err != nil {
		t.Fatal(err)
	}
	if err := v.Consume(c); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(state); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("token state: %v %v", info, err)
	}
	again := ti.verifier()
	again.statePath = state
	again.load()
	c2, err := again.Verify(tok)
	if err != nil {
		t.Fatal(err)
	}
	if err := again.Consume(c2); err == nil {
		t.Fatal("a token was accepted again after a restart")
	}
}

// The repository's numeric IDs are pinned by the first token, so a
// repository re-created under the same name can't deploy (review L3).
func TestRepositoryIDsPinned(t *testing.T) {
	ti := newTestIssuer(t)
	state := filepath.Join(t.TempDir(), "tokens.json")
	v := ti.verifier()
	v.statePath = state
	noID := baseClaims("refs/heads/main", "x")
	delete(noID, "repository_id")
	if _, err := v.Verify(ti.token(t, noID)); err == nil {
		t.Fatal("a token without repository_id was accepted")
	}
	c, err := v.Verify(ti.token(t, baseClaims("refs/heads/main", "x")))
	if err != nil {
		t.Fatal(err)
	}
	if err := v.Consume(c); err != nil {
		t.Fatal(err)
	}
	again := ti.verifier()
	again.statePath = state
	again.load()
	for field, value := range map[string]string{"repository_id": "999", "repository_owner_id": "999"} {
		claims := baseClaims("refs/heads/main", "x")
		claims[field] = value
		if _, err := again.Verify(ti.token(t, claims)); err == nil || !strings.Contains(err.Error(), "same name") {
			t.Fatalf("%s changed: %v", field, err)
		}
	}
	if _, err := again.Verify(ti.token(t, baseClaims("refs/heads/main", "x"))); err != nil {
		t.Fatal(err)
	}
}

// While GitHub's keys can't be fetched, recently fetched keys keep working,
// failed fetches are not retried at once, and nothing waits on a lock that a
// fetch holds (security review L1).
func TestKeysDuringOutage(t *testing.T) {
	ti := newTestIssuer(t)
	v := ti.verifier()
	clock := time.Now()
	v.now = func() time.Time { return clock }
	tok := func(kid string) string { // a token that is fresh at the test's clock
		c := baseClaims("refs/heads/main", "x")
		c["iat"], c["nbf"], c["exp"] = clock.Unix(), clock.Unix(), clock.Unix()+300
		return sign(t, ti.key, map[string]any{"alg": "RS256", "kid": kid}, c)
	}
	if _, err := v.Verify(tok(ti.kid)); err != nil {
		t.Fatal(err)
	}
	ti.down = true
	clock = clock.Add(11 * time.Minute)
	if _, err := v.Verify(tok(ti.kid)); err != nil {
		t.Fatalf("known key refused during an outage: %v", err)
	}
	hits := ti.hits
	for i := 0; i < 3; i++ {
		v.Verify(tok(ti.kid))
		v.Verify(tok("rotated"))
	}
	if ti.hits != hits {
		t.Fatalf("fetched again right after a failure (%d fetches)", ti.hits-hits)
	}
	clock = clock.Add(25 * time.Hour)
	if _, err := v.Verify(tok(ti.kid)); err == nil {
		t.Fatal("a key fetched more than a day ago is still trusted")
	}
	clock = clock.Add(time.Second) // within the backoff after that failed fetch
	if _, err := v.Verify(tok(ti.kid)); err == nil {
		t.Fatal("a key fetched more than a day ago is trusted during the backoff")
	}
	ti.down = false
	clock = clock.Add(time.Minute)
	if _, err := v.Verify(tok(ti.kid)); err != nil {
		t.Fatalf("not recovered after the outage: %v", err)
	}
}
