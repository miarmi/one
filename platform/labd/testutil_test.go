package main

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

const (
	testRepo    = "OWNER/REPO"
	testRepoID  = "870001234"
	testOwnerID = "12345678"
)

// testIssuer signs GitHub-like OIDC tokens and serves its JWKS.
type testIssuer struct {
	key  *rsa.PrivateKey
	kid  string
	srv  *httptest.Server
	hits int
	down bool // answer 503, like GitHub during an outage
}

func newTestIssuer(t *testing.T) *testIssuer {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	ti := &testIssuer{key: key, kid: "test-key"}
	ti.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ti.hits++
		if ti.down {
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{jwk(&ti.key.PublicKey, ti.kid)}})
	}))
	t.Cleanup(ti.srv.Close)
	return ti
}

func jwk(pub *rsa.PublicKey, kid string) map[string]string {
	return map[string]string{
		"kty": "RSA", "kid": kid, "alg": "RS256", "use": "sig",
		"n": base64.RawURLEncoding.EncodeToString(pub.N.Bytes()),
		"e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(pub.E)).Bytes()),
	}
}

func (ti *testIssuer) verifier() *Verifier {
	return newVerifier(githubIssuer, ti.srv.URL, testRepo)
}

func baseClaims(ref, aud string) map[string]any {
	now := time.Now().Unix()
	return map[string]any{
		"iss": githubIssuer, "aud": aud, "repository": testRepo, "ref": ref,
		"repository_id": testRepoID, "repository_owner_id": testOwnerID,
		"sha": "0123456789abcdef0123456789abcdef01234567", "event_name": "push",
		"jti": randHex(8), "iat": now, "nbf": now, "exp": now + 300,
	}
}

func randHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// sign makes a token with the given header fields and claims.
func sign(t *testing.T, key *rsa.PrivateKey, header map[string]any, claims map[string]any) string {
	t.Helper()
	h, _ := json.Marshal(header)
	c, _ := json.Marshal(claims)
	input := base64.RawURLEncoding.EncodeToString(h) + "." + base64.RawURLEncoding.EncodeToString(c)
	digest := sha256.Sum256([]byte(input))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	return input + "." + base64.RawURLEncoding.EncodeToString(sig)
}

func (ti *testIssuer) token(t *testing.T, claims map[string]any) string {
	return sign(t, ti.key, map[string]any{"alg": "RS256", "kid": ti.kid, "typ": "JWT"}, claims)
}

func sha256Hex(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}
