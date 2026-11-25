package main

import (
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"time"
)

const clockSkew = 60 * time.Second

// Claims are the GitHub Actions OIDC token fields labd uses.
type Claims struct {
	Issuer       string   `json:"iss"`
	Audience     audience `json:"aud"`
	Repository   string   `json:"repository"`
	RepositoryID string   `json:"repository_id"`
	OwnerID      string   `json:"repository_owner_id"`
	Ref          string   `json:"ref"`
	SHA          string   `json:"sha"`
	EventName    string   `json:"event_name"`
	JTI          string   `json:"jti"`
	Exp          *float64 `json:"exp"`
	Nbf          *float64 `json:"nbf"`
	Iat          *float64 `json:"iat"`
}

// audience accepts both a string and a list of strings.
type audience []string

func (a *audience) UnmarshalJSON(b []byte) error {
	var one string
	if err := json.Unmarshal(b, &one); err == nil {
		*a = audience{one}
		return nil
	}
	var many []string
	if err := json.Unmarshal(b, &many); err != nil {
		return errors.New("aud must be a string or a list of strings")
	}
	*a = many
	return nil
}

func (c *Claims) hasAudience(want string) bool {
	for _, a := range c.Audience {
		if a == want {
			return true
		}
	}
	return false
}

// platformAudience binds a token to one uploaded labd binary (same as seed.py).
func platformAudience(bodySHA256 string) string {
	return "lab-platform:sha256:" + bodySHA256
}

// apiAudience binds a token to one request: method, exact URI and body.
func apiAudience(method, requestURI, bodySHA256 string) string {
	sum := sha256.Sum256([]byte(method + " " + requestURI + "\n" + bodySHA256))
	return "lab-api:sha256:" + hex.EncodeToString(sum[:])
}

// Verifier checks GitHub Actions OIDC tokens for one repository.
type Verifier struct {
	issuer    string
	jwksURL   string
	repo      string
	client    *http.Client
	now       func() time.Time
	statePath string // used tokens and the pinned repository IDs survive restarts here

	fetchMu sync.Mutex // one key fetch at a time, without blocking v.mu

	mu        sync.Mutex
	keys      map[string]*rsa.PublicKey
	fetchedAt time.Time
	failedAt  time.Time
	seen      map[string]time.Time // jti -> forget after
	repoID    string               // pinned on first use (verifierState)
	ownerID   string
}

func newVerifier(issuer, jwksURL, repo string) *Verifier {
	return &Verifier{
		issuer:  issuer,
		jwksURL: jwksURL,
		repo:    repo,
		client:  &http.Client{Timeout: 5 * time.Second},
		now:     time.Now,
		keys:    map[string]*rsa.PublicKey{},
		seen:    map[string]time.Time{},
	}
}

// verifierState is what Verifier keeps on disk: the tokens already used, so
// a captured request can't be replayed after a restart, and the repository's
// numeric IDs, pinned by the first valid token, so a repository deleted and
// re-created under the same name can't deploy here.
type verifierState struct {
	Used    map[string]int64 `json:"used"`
	RepoID  string           `json:"repository_id,omitempty"`
	OwnerID string           `json:"repository_owner_id,omitempty"`
}

func (v *Verifier) load() {
	if v.statePath == "" {
		return
	}
	var st verifierState
	if err := readJSON(v.statePath, &st); err != nil {
		log.Printf("tokens: %v", err)
		return
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	for jti, until := range st.Used {
		v.seen[jti] = time.Unix(until, 0)
	}
	v.repoID, v.ownerID = st.RepoID, st.OwnerID
}

func (v *Verifier) saveLocked() error {
	if v.statePath == "" {
		return nil
	}
	st := verifierState{Used: map[string]int64{}, RepoID: v.repoID, OwnerID: v.ownerID}
	for jti, until := range v.seen {
		st.Used[jti] = until.Unix()
	}
	raw, err := json.Marshal(st)
	if err != nil {
		return err
	}
	return writeFileAtomic(v.statePath, append(raw, '\n'), 0o600)
}

func b64url(s string) ([]byte, error) {
	if strings.ContainsAny(s, "=+/") {
		return nil, errors.New("invalid base64url")
	}
	return base64.RawURLEncoding.DecodeString(s)
}

// Signing keys are cached for 10 minutes. An unknown key ID triggers a fetch
// at most once a minute, and after a failed fetch nothing is fetched for 30
// seconds. While GitHub can't be reached, keys fetched in the last day keep
// working. Fetches never hold v.mu, so they don't stall other requests.
const (
	keysFresh     = 10 * time.Minute
	keysRetry     = time.Minute
	keysBackoff   = 30 * time.Second
	keysStaleKeep = 24 * time.Hour
)

func (v *Verifier) key(kid string) (*rsa.PublicKey, error) {
	v.mu.Lock()
	k, ok := v.keys[kid]
	age := v.now().Sub(v.fetchedAt)
	backoff := v.now().Sub(v.failedAt) < keysBackoff
	v.mu.Unlock()
	switch {
	case ok && age <= keysFresh:
		return k, nil
	case !ok && (age <= keysRetry || backoff):
		return nil, errors.New("unknown signing key")
	case ok && backoff && age <= keysStaleKeep:
		return k, nil // a fetch just failed: keep trusting a recently fetched key
	case backoff:
		return nil, errors.New("GitHub's signing keys could not be fetched just now")
	}
	err := v.refresh()
	v.mu.Lock()
	defer v.mu.Unlock()
	switch nk, found := v.keys[kid]; {
	case err == nil && found:
		return nk, nil
	case err == nil:
		return nil, errors.New("unknown signing key")
	case ok && v.now().Sub(v.fetchedAt) <= keysStaleKeep:
		return k, nil // GitHub unreachable: keep trusting a recently fetched key
	}
	return nil, fmt.Errorf("fetching signing keys: %w", err)
}

// refresh fetches the key set unless another request just did.
func (v *Verifier) refresh() error {
	start := v.now()
	v.fetchMu.Lock()
	defer v.fetchMu.Unlock()
	v.mu.Lock()
	fetched, failed := !v.fetchedAt.Before(start), !v.failedAt.Before(start)
	v.mu.Unlock()
	switch {
	case fetched:
		return nil
	case failed:
		return errors.New("GitHub's signing keys could not be fetched just now")
	}
	keys, err := v.fetch()
	v.mu.Lock()
	defer v.mu.Unlock()
	if err != nil {
		v.failedAt = v.now()
		return err
	}
	v.keys, v.fetchedAt = keys, v.now()
	return nil
}

func (v *Verifier) fetch() (map[string]*rsa.PublicKey, error) {
	resp, err := v.client.Get(v.jwksURL)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("status %d", resp.StatusCode)
	}
	var doc struct {
		Keys []struct {
			Kty string `json:"kty"`
			Kid string `json:"kid"`
			N   string `json:"n"`
			E   string `json:"e"`
		} `json:"keys"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&doc); err != nil {
		return nil, err
	}
	keys := map[string]*rsa.PublicKey{}
	for _, k := range doc.Keys {
		if k.Kty != "RSA" || k.Kid == "" {
			continue
		}
		n, err1 := b64url(k.N)
		e, err2 := b64url(k.E)
		if err1 != nil || err2 != nil || len(e) == 0 || len(e) > 4 {
			continue
		}
		exp := int(new(big.Int).SetBytes(e).Int64())
		pub := &rsa.PublicKey{N: new(big.Int).SetBytes(n), E: exp}
		if pub.N.BitLen() < 2048 || exp < 3 || exp%2 == 0 {
			continue
		}
		keys[k.Kid] = pub
	}
	return keys, nil
}

// Verify checks signature, issuer, time and repository. The caller still has
// to check the audience against the request and then call Consume.
func (v *Verifier) Verify(token string) (*Claims, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil, errors.New("malformed token")
	}
	rawHeader, err := b64url(parts[0])
	if err != nil {
		return nil, errors.New("malformed token header")
	}
	var header struct {
		Alg string `json:"alg"`
		Kid string `json:"kid"`
	}
	if err := json.Unmarshal(rawHeader, &header); err != nil {
		return nil, errors.New("malformed token header")
	}
	if header.Alg != "RS256" {
		return nil, errors.New("unsupported token algorithm")
	}
	sig, err := b64url(parts[2])
	if err != nil {
		return nil, errors.New("malformed token signature")
	}
	pub, err := v.key(header.Kid)
	if err != nil {
		return nil, err
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(pub, crypto.SHA256, digest[:], sig); err != nil {
		return nil, errors.New("bad token signature")
	}
	rawClaims, err := b64url(parts[1])
	if err != nil {
		return nil, errors.New("malformed token claims")
	}
	var c Claims
	if err := json.Unmarshal(rawClaims, &c); err != nil {
		return nil, errors.New("malformed token claims")
	}
	now := v.now()
	switch {
	case c.Issuer != v.issuer:
		return nil, errors.New("wrong token issuer")
	case c.Exp == nil || unix(*c.Exp).Add(clockSkew).Before(now):
		return nil, errors.New("token expired")
	case c.Nbf != nil && unix(*c.Nbf).Add(-clockSkew).After(now):
		return nil, errors.New("token not valid yet")
	case c.Iat != nil && unix(*c.Iat).Add(-clockSkew).After(now):
		return nil, errors.New("token issued in the future")
	case c.Repository != v.repo:
		return nil, errors.New("token is for another repository")
	case c.RepositoryID == "" || c.OwnerID == "":
		return nil, errors.New("token has no repository_id or repository_owner_id")
	case c.JTI == "":
		return nil, errors.New("token has no jti")
	}
	v.mu.Lock()
	repoID, ownerID := v.repoID, v.ownerID
	v.mu.Unlock()
	if (repoID != "" && c.RepositoryID != repoID) || (ownerID != "" && c.OwnerID != ownerID) {
		return nil, errors.New("token is for another repository with the same name")
	}
	return &c, nil
}

// Consume marks the token as used; a second use fails.
func (v *Verifier) Consume(c *Claims) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	now := v.now()
	for jti, until := range v.seen {
		if until.Before(now) {
			delete(v.seen, jti)
		}
	}
	if _, used := v.seen[c.JTI]; used {
		return errors.New("token already used")
	}
	if (v.repoID != "" && c.RepositoryID != v.repoID) || (v.ownerID != "" && c.OwnerID != v.ownerID) {
		return errors.New("token is for another repository with the same name")
	}
	v.seen[c.JTI] = unix(*c.Exp).Add(clockSkew)
	if v.repoID == "" && v.ownerID == "" {
		v.repoID, v.ownerID = c.RepositoryID, c.OwnerID
		log.Printf("tokens: pinned repository %s to id %s (owner id %s)", c.Repository, c.RepositoryID, c.OwnerID)
	}
	if err := v.saveLocked(); err != nil {
		log.Printf("tokens: could not save used tokens: %v", err)
	}
	return nil
}

func unix(f float64) time.Time { return time.Unix(int64(f), 0) }
