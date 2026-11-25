package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"lab/apps/one/repo"
)

// One bad record must not stop the rest. A failure that belongs to a single
// message or event (it cannot be parsed, stored or filed) is logged at level
// error with the source, the record's own id and a scrubbed reason, counted
// on the run, remembered in a small ledger so it is tried again later, and
// the loop goes on. A failure of the connection or of the whole fetch
// (network, sign-in, a stopped context, an unusable database) still stops the
// run exactly as before: SPEC 24's failed source depends on that.

// connErr marks an error of the connection or of the whole fetch, however it
// came up in the middle of one record's work.
type connErr struct{ err error }

func (e *connErr) Error() string { return e.err.Error() }
func (e *connErr) Unwrap() error { return e.err }

// conn marks err as the connection's (nil stays nil).
func conn(err error) error {
	if err == nil {
		return nil
	}
	return &connErr{err}
}

// isFatal says whether err ends the run rather than one record.
func isFatal(ctx context.Context, err error) bool {
	var ce *connErr
	var gl *gmailLoginErr
	return ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) ||
		errors.As(err, &ce) || errors.As(err, &gl) || errors.Is(err, errNeedsLogin) || errors.Is(err, errNotConnected) ||
		repo.Unavailable(err)
}

// maxConsecutiveFailures ends a run whose records all fail one after the
// other: that is a systemic fault (a full disk, a broken store), not a bad
// record, and going on would only fill the log.
const maxConsecutiveFailures = 20

// The ledger of records that failed on their own. A record is tried again on
// a later run, with waits that grow, and given up after maxFailTries so one
// permanently broken record is not read again at full cost for ever. A record
// that is given up stays in the ledger (as a line, not as work) until it is a
// month old; a change of the record at its source brings it back by itself,
// because the next run meets it as a changed message.
const (
	maxFailTries   = 6
	maxLedgerLines = 300
	ledgerKeep     = 30 * 24 * time.Hour
)

// failWaits is how long to wait after the nth failure before the next try.
var failWaits = [maxFailTries]time.Duration{0, 0, 15 * time.Minute, time.Hour, 6 * time.Hour, 24 * time.Hour}

type failEntry struct {
	Tries int   `json:"n"`
	At    int64 `json:"at"`
}

// failLedger keeps one map per ledger name in memory and in the settings
// ("failed.<name>"), so it survives a restart.
type failLedger struct {
	mu   sync.Mutex
	maps map[string]map[string]failEntry
	cal  map[string]int // rounds in a row a calendar had entries fail
}

func ledgerKey(name string) string { return "failed." + name }

func (a *app) ledgerLoad(ctx context.Context, name string) map[string]failEntry {
	l := &a.fails
	if l.maps == nil {
		l.maps = map[string]map[string]failEntry{}
	}
	m, ok := l.maps[name]
	if !ok {
		m = map[string]failEntry{}
		json.Unmarshal([]byte(a.store.Setting(ctx, ledgerKey(name))), &m)
		l.maps[name] = m
	}
	return m
}

func (a *app) ledgerSave(ctx context.Context, name string, m map[string]failEntry) {
	now := a.now()
	for id, e := range m {
		if e.Tries >= maxFailTries && now.Sub(time.Unix(e.At, 0)) > ledgerKeep {
			delete(m, id)
		}
	}
	if len(m) > maxLedgerLines {
		ids := make([]string, 0, len(m))
		for id := range m {
			ids = append(ids, id)
		}
		slices.SortFunc(ids, func(x, y string) int { return int(m[x].At - m[y].At) })
		for _, id := range ids[:len(m)-maxLedgerLines] {
			delete(m, id)
		}
	}
	b, _ := json.Marshal(m)
	a.store.SetSettingBy(context.WithoutCancel(ctx), ledgerKey(name), string(b), "system", now)
}

// failNote records one more failed try of a record and says how many it has had.
func (a *app) failNote(ctx context.Context, name, id string) int {
	a.fails.mu.Lock()
	defer a.fails.mu.Unlock()
	m := a.ledgerLoad(ctx, name)
	e := m[id]
	e.Tries++
	e.At = a.now().Unix()
	m[id] = e
	a.ledgerSave(ctx, name, m)
	return e.Tries
}

// failNoteNew is failNote for a record the bridge is asked to send again: a
// record already in the ledger keeps its count (asking again counts as a try).
func (a *app) failNoteNew(ctx context.Context, name, id string) int {
	a.fails.mu.Lock()
	defer a.fails.mu.Unlock()
	m := a.ledgerLoad(ctx, name)
	if e, ok := m[id]; ok {
		return e.Tries
	}
	m[id] = failEntry{Tries: 1, At: a.now().Unix()}
	a.ledgerSave(ctx, name, m)
	return 1
}

// failHas says whether a record has a line in the ledger.
func (a *app) failHas(ctx context.Context, name, id string) bool {
	a.fails.mu.Lock()
	defer a.fails.mu.Unlock()
	_, ok := a.ledgerLoad(ctx, name)[id]
	return ok
}

// failClear forgets a record that went through (or is gone).
func (a *app) failClear(ctx context.Context, name, id string) {
	a.fails.mu.Lock()
	defer a.fails.mu.Unlock()
	m := a.ledgerLoad(ctx, name)
	if _, ok := m[id]; ok {
		delete(m, id)
		a.ledgerSave(ctx, name, m)
	}
}

// failIsDue says whether a record may be worked on now: it has no line in
// the ledger, or its wait is over and it has tries left.
func (a *app) failIsDue(ctx context.Context, name, id string) bool {
	a.fails.mu.Lock()
	defer a.fails.mu.Unlock()
	e, ok := a.ledgerLoad(ctx, name)[id]
	if !ok {
		return true
	}
	return e.Tries < maxFailTries && !a.now().Before(time.Unix(e.At, 0).Add(failWaits[e.Tries]))
}

// failDue lists up to limit records whose wait is over and that have tries left.
func (a *app) failDue(ctx context.Context, name string, limit int) []string {
	a.fails.mu.Lock()
	defer a.fails.mu.Unlock()
	now := a.now()
	var out []string
	for id, e := range a.ledgerLoad(ctx, name) {
		if e.Tries >= maxFailTries || now.Before(time.Unix(e.At, 0).Add(failWaits[e.Tries])) {
			continue
		}
		out = append(out, id)
	}
	slices.Sort(out)
	if len(out) > limit {
		out = out[:limit]
	}
	return out
}

// failedCount is how many records of a ledger are still waiting for a try.
func (a *app) failedCount(ctx context.Context, name string) int {
	a.fails.mu.Lock()
	defer a.fails.mu.Unlock()
	n := 0
	for _, e := range a.ledgerLoad(ctx, name) {
		if e.Tries < maxFailTries {
			n++
		}
	}
	return n
}

var (
	quotedRE = regexp.MustCompile(`"[^"]*"|'[^']*'|\x60[^\x60]*\x60`)
	atWordRE = regexp.MustCompile(`\S*@\S*`)
)

// scrubReason makes an error fit for the log: whatever sits between quotes
// and any word with an @ is cut out, because an error from a parser can quote
// the message it choked on. What is left says what went wrong, not who wrote.
func scrubReason(err error) string {
	if err == nil {
		return ""
	}
	s := quotedRE.ReplaceAllString(err.Error(), "…")
	s = atWordRE.ReplaceAllString(s, "…")
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > 160 {
		s = strings.ToValidUTF8(s[:160], "") + "…"
	}
	return s
}

// logID is the record's own id as the log may show it: the part of a chat
// message id that belongs to the message (the chat id can be a phone number),
// and the part of an event id before an @.
func logID(id string) string {
	if i := strings.LastIndex(id, "|"); i >= 0 {
		id = id[i+1:]
	}
	if i := strings.Index(id, "@"); i >= 0 {
		id = id[:i]
	}
	if len(id) > 120 {
		id = id[:120]
	}
	return strings.ToValidUTF8(id, "")
}

// logLeftOut says, at level error (which is what alerts the owner), that one
// record was left out of a run and why. Never a text, a subject or an address.
func (a *app) logLeftOut(source, id, kind string, tries int, err error) {
	a.logger().Error("record left out", "source", source, "id", logID(id), "kind", kind, "tries", tries, "reason", scrubReason(err))
}

// logger is where the app's own error lines go; tests replace it.
func (a *app) logger() *slog.Logger {
	if a.log != nil {
		return a.log
	}
	return a.imp.log
}

// --- mail scopes ---

// unit runs one record's work so that a panic in it is that record's own
// failure (logged with the app's frames), not the run's. A message asked for
// by hand (strict) lets the panic through to the handler that answers 500.
func (s *mailScope) unit(fn func() error) error {
	if s.strict {
		return fn()
	}
	return s.c.a.tryUnit(s.src.Name+" message", fn)
}

// tolerate decides what a failure of one message means for the run: a failure
// of the connection or the whole fetch is returned (the run ends as it
// always did); a failure that belongs to this message is logged, counted,
// written in the ledger, and nil is returned so the loop goes on. A
// message that went through leaves the ledger.
func (s *mailScope) tolerate(ctx context.Context, id string, err error) error {
	a := s.c.a
	if err == nil {
		s.consec = 0
		a.failClear(ctx, s.src.Name, id)
		return nil
	}
	var ce *connErr
	if errors.As(err, &ce) {
		return ce.err
	}
	if s.strict || isFatal(ctx, err) {
		return err
	}
	s.consec++
	s.run.Failed++
	tries := a.failNote(ctx, s.src.Name, id)
	a.logLeftOut(s.src.Name, id, "message", tries, err)
	if s.consec >= maxConsecutiveFailures {
		return fmt.Errorf("%d messages in a row could not be stored, so the run stopped: %w", s.consec, err)
	}
	return nil
}

// calRetry says whether a calendar read that left out `failed` entries should
// be read whole again next round. Calendars are read whole or by delta, so an
// entry that failed comes round again by itself; this bounds it: after
// maxFailTries rounds in a row with failures the calendar goes back to its
// normal way and the entry waits for its next change. It is kept in memory.
func (a *app) calRetry(key string, failed int) bool {
	a.fails.mu.Lock()
	defer a.fails.mu.Unlock()
	if a.fails.cal == nil {
		a.fails.cal = map[string]int{}
	}
	if failed == 0 {
		delete(a.fails.cal, key)
		return false
	}
	a.fails.cal[key]++
	if a.fails.cal[key] >= maxFailTries {
		delete(a.fails.cal, key)
		return false
	}
	return true
}
