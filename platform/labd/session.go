package main

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	sessionCookie = "lab_session"
	ciSessionTTL  = 15 * time.Minute
)

// Sessions issues and checks lab_session cookies (CONTRACT.md, "Login gate"):
// base64url(expiry uint64 BE || 16 random bytes || host scope) "."
// base64url(HMAC-SHA256 of the first part). The host scope ties a pass to the
// one host it was issued for, so a pass for a preview never opens production.
type Sessions struct {
	path string
	now  func() time.Time

	mu     sync.Mutex
	secret []byte
}

func newSessions(stateDir string) *Sessions {
	return &Sessions{path: filepath.Join(stateDir, "secret"), now: time.Now}
}

// key loads the secret, creating it on first use.
func (s *Sessions) key() ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.secret != nil {
		return s.secret, nil
	}
	b, err := os.ReadFile(s.path)
	if err == nil && len(b) == 32 {
		s.secret = b
		return b, nil
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	b = make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return nil, err
	}
	if err := writeFileAtomic(s.path, b, 0o600); err != nil {
		return nil, err
	}
	s.secret = b
	return b, nil
}

// hostScope is the part of a cookie that names its host.
func hostScope(host string) []byte {
	sum := sha256.Sum256([]byte("lab-host:" + strings.TrimSuffix(strings.ToLower(host), ".")))
	return sum[:16]
}

// userScope is the scope of the owner's own pass from the login page: every host
// of the lab's domain, until the password changes (generation).
func userScope(base string, generation int) []byte {
	sum := sha256.Sum256([]byte(fmt.Sprintf("lab-user:%d:%s", generation, strings.ToLower(base))))
	return sum[:16]
}

// Issue returns a cookie value valid for ttl on host, and its expiry.
func (s *Sessions) Issue(ttl time.Duration, host string) (string, time.Time, error) {
	if host == "" {
		return "", time.Time{}, errors.New("a pass needs a host")
	}
	return s.issue(ttl, hostScope(host))
}

// IssueUser returns the owner's pass for every host of the domain base.
func (s *Sessions) IssueUser(ttl time.Duration, base string, generation int) (string, time.Time, error) {
	if base == "" {
		return "", time.Time{}, errors.New("a pass needs a domain")
	}
	return s.issue(ttl, userScope(base, generation))
}

func (s *Sessions) issue(ttl time.Duration, scope []byte) (string, time.Time, error) {
	key, err := s.key()
	if err != nil {
		return "", time.Time{}, err
	}
	exp := s.now().Add(ttl).Truncate(time.Second)
	payload := make([]byte, 40)
	binary.BigEndian.PutUint64(payload, uint64(exp.Unix()))
	if _, err := rand.Read(payload[8:24]); err != nil {
		return "", time.Time{}, err
	}
	copy(payload[24:], scope)
	first := base64.RawURLEncoding.EncodeToString(payload)
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(first))
	return first + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil)), exp, nil
}

// Valid reports whether a cookie value is genuine, not expired and issued
// for host.
func (s *Sessions) Valid(value, host string) bool {
	if host == "" {
		return false
	}
	scope, ok := s.scope(value)
	return ok && hmac.Equal(scope, hostScope(host))
}

// ValidUser reports whether a cookie value is the owner's pass for the domain
// base (generation as now) and host belongs to that domain.
func (s *Sessions) ValidUser(value, host, base string, generation int) bool {
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	if base == "" || (host != base && !strings.HasSuffix(host, "."+base)) {
		return false
	}
	scope, ok := s.scope(value)
	return ok && hmac.Equal(scope, userScope(base, generation))
}

// scope checks a cookie value's signature and expiry and returns its scope.
func (s *Sessions) scope(value string) ([]byte, bool) {
	first, sig, ok := strings.Cut(value, ".")
	if !ok || len(first) != 54 || len(sig) != 43 {
		return nil, false
	}
	key, err := s.key()
	if err != nil {
		return nil, false
	}
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(first))
	got, err := base64.RawURLEncoding.DecodeString(sig)
	if err != nil || !hmac.Equal(got, mac.Sum(nil)) {
		return nil, false
	}
	payload, err := base64.RawURLEncoding.DecodeString(first)
	if err != nil || len(payload) != 40 {
		return nil, false
	}
	exp := time.Unix(int64(binary.BigEndian.Uint64(payload)), 0)
	if !s.now().Before(exp) {
		return nil, false
	}
	return payload[24:], true
}
