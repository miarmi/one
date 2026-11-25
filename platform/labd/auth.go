package main

import (
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// The login gate's password and setup codes (CONTRACT.md, "Login gate").
// The owner has one password. He chooses it himself with a one-time setup code;
// only the code's SHA-256 ever reaches the server (POST /api/setup-code), so
// neither the code nor the password passes through GitHub.

const (
	passwordIterations = 600000
	minPassword        = 12
	maxPassword        = 1024
	setupCodeTTL       = 60 * time.Minute
	userSessionTTL     = 180 * 24 * time.Hour
)

var (
	errPasswordSet = errors.New("a password is already set; reset=1 replaces it")
	errBadCode     = errors.New("this setup code is wrong, used or expired")
	errBadPassword = errors.New("the password must have 12 to 1024 characters")
)

type authState struct {
	Password   *storedPassword `json:"password,omitempty"`
	Generation int             `json:"generation"` // goes up with every new password; older passes stop working
	Setup      *storedSetup    `json:"setup,omitempty"`
}

type storedPassword struct {
	Salt       string `json:"salt"`
	Hash       string `json:"hash"`
	Iterations int    `json:"iterations"`
}

type storedSetup struct {
	SHA256  string    `json:"sha256"`
	Expires time.Time `json:"expires"`
}

// Auth keeps <state_dir>/auth.json (0600).
type Auth struct {
	path       string
	now        func() time.Time
	iterations int // lower in tests

	mu     sync.Mutex
	st     authState
	loaded bool
}

func newAuth(stateDir string) *Auth {
	return &Auth{path: filepath.Join(stateDir, "auth.json"), now: time.Now, iterations: passwordIterations}
}

// state returns the stored state; a.mu must be held.
func (a *Auth) state() (authState, error) {
	if !a.loaded {
		var st authState
		if err := readJSON(a.path, &st); err != nil {
			return authState{}, err
		}
		a.st, a.loaded = st, true
	}
	return a.st, nil
}

// save stores st; a.mu must be held.
func (a *Auth) save(st authState) error {
	raw, err := jsonIndent(st)
	if err != nil {
		return err
	}
	if err := writeFileAtomic(a.path, raw, 0o600); err != nil {
		return err
	}
	a.st, a.loaded = st, true
	return nil
}

// Status says whether a password is set and which generation passes need.
func (a *Auth) Status() (bool, int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	st, err := a.state()
	if err != nil {
		return false, 0
	}
	return st.Password != nil, st.Generation
}

// SetSetupCode stores the SHA-256 (hex) of a new setup code, valid for an
// hour. Once a password is set, only reset may replace it.
func (a *Auth) SetSetupCode(sum string, reset bool) (time.Time, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	st, err := a.state()
	if err != nil {
		return time.Time{}, err
	}
	if st.Password != nil && !reset {
		return time.Time{}, errPasswordSet
	}
	exp := a.now().Add(setupCodeTTL).Truncate(time.Second)
	st.Setup = &storedSetup{SHA256: strings.ToLower(sum), Expires: exp}
	return exp, a.save(st)
}

// codeMatches reports whether code is the stored, unexpired setup code;
// a.mu must be held.
func (a *Auth) codeMatches(st authState, code string) bool {
	norm, ok := normalizeSetupCode(code)
	if !ok || st.Setup == nil || !a.now().Before(st.Setup.Expires) {
		return false
	}
	want, err := hex.DecodeString(st.Setup.SHA256)
	got := sha256.Sum256([]byte(norm))
	return err == nil && subtle.ConstantTimeCompare(want, got[:]) == 1
}

// SetupPending reports whether an unexpired setup code is waiting.
func (a *Auth) SetupPending() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	st, err := a.state()
	return err == nil && st.Setup != nil && a.now().Before(st.Setup.Expires)
}

// SetupCodeValid reports whether code would work now, without using it.
func (a *Auth) SetupCodeValid(code string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	st, err := a.state()
	return err == nil && a.codeMatches(st, code)
}

// UseSetupCode sets password with a valid setup code. The code is used up,
// and every pass from an earlier password stops working. It returns the
// generation new passes carry.
func (a *Auth) UseSetupCode(code, password string) (int, error) {
	if len(password) < minPassword || len(password) > maxPassword {
		return 0, errBadPassword
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	st, err := a.state()
	if err != nil {
		return 0, err
	}
	if !a.codeMatches(st, code) {
		return 0, errBadCode
	}
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return 0, err
	}
	hash, err := pbkdf2.Key(sha256.New, password, salt, a.iterations, 32)
	if err != nil {
		return 0, err
	}
	st.Password = &storedPassword{
		Salt:       base64.StdEncoding.EncodeToString(salt),
		Hash:       base64.StdEncoding.EncodeToString(hash),
		Iterations: a.iterations,
	}
	st.Generation++
	st.Setup = nil
	if err := a.save(st); err != nil {
		return 0, err
	}
	return st.Generation, nil
}

// CheckPassword reports whether password is the owner's, and the generation.
// It takes a fraction of a second on purpose; callers limit how often.
func (a *Auth) CheckPassword(password string) (int, bool) {
	a.mu.Lock()
	st, err := a.state()
	a.mu.Unlock()
	if err != nil || st.Password == nil || len(password) > maxPassword {
		return 0, false
	}
	salt, err1 := base64.StdEncoding.DecodeString(st.Password.Salt)
	want, err2 := base64.StdEncoding.DecodeString(st.Password.Hash)
	if err1 != nil || err2 != nil || st.Password.Iterations < 1 {
		return 0, false
	}
	got, err := pbkdf2.Key(sha256.New, password, salt, st.Password.Iterations, len(want))
	if err != nil || subtle.ConstantTimeCompare(got, want) != 1 {
		return 0, false
	}
	return st.Generation, true
}

// Setup codes are 16 characters of Crockford's base32 (80 random bits),
// shown as XXXX-XXXX-XXXX-XXXX. Typing is forgiving: case, spaces and
// dashes don't matter, and O, I and L read as 0, 1 and 1.
const setupAlphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

func normalizeSetupCode(s string) (string, bool) {
	if len(s) > 64 {
		return "", false
	}
	var b strings.Builder
	for _, r := range strings.ToUpper(s) {
		switch r {
		case '-', ' ':
			continue
		case 'O':
			r = '0'
		case 'I', 'L':
			r = '1'
		}
		if !strings.ContainsRune(setupAlphabet, r) {
			return "", false
		}
		b.WriteRune(r)
	}
	if b.Len() != 16 {
		return "", false
	}
	return b.String(), true
}

// limiter slows down password and code guessing (CONTRACT.md): after 5
// failures in a minute from one address, or 20 from everywhere, tries are
// refused until the minute is over. At most two passwords are checked at
// once, so guessing can't tie up the server either.
type limiter struct {
	now   func() time.Time
	mu    sync.Mutex
	perIP map[string][]time.Time
	all   []time.Time
	work  chan struct{}
}

func newLimiter() *limiter {
	return &limiter{now: time.Now, perIP: map[string][]time.Time{}, work: make(chan struct{}, 2)}
}

func recent(ts []time.Time, since time.Time) []time.Time {
	i := 0
	for i < len(ts) && !ts[i].After(since) {
		i++
	}
	return ts[i:]
}

// blocked reports whether ip may not try now.
func (l *limiter) blocked(ip string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	since := l.now().Add(-time.Minute)
	l.all = recent(l.all, since)
	key := ipKey(ip)
	l.perIP[key] = recent(l.perIP[key], since)
	if len(l.perIP[key]) == 0 {
		delete(l.perIP, key)
	}
	return len(l.perIP[key]) >= 5 || len(l.all) >= 20
}

// failed records a failed try from ip.
func (l *limiter) failed(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	since := now.Add(-time.Minute)
	for k, ts := range l.perIP { // stay small
		if ts = recent(ts, since); len(ts) == 0 {
			delete(l.perIP, k)
		}
	}
	key := ipKey(ip)
	l.perIP[key] = append(l.perIP[key], now)
	l.all = append(recent(l.all, since), now)
}

// acquire takes one of the two password-checking slots, or reports that
// both are busy.
func (l *limiter) acquire() bool {
	select {
	case l.work <- struct{}{}:
		return true
	default:
		return false
	}
}

func (l *limiter) release() { <-l.work }

// ipKey groups IPv6 addresses by their /64, the part one home or server holds.
func ipKey(s string) string {
	ip := net.ParseIP(s)
	if ip == nil {
		return s
	}
	if ip.To4() != nil {
		return ip.To4().String()
	}
	return ip.Mask(net.CIDRMask(64, 128)).String() + "/64"
}

// jsonIndent is json.MarshalIndent with a final newline.
func jsonIndent(v any) ([]byte, error) {
	raw, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(raw, '\n'), nil
}
