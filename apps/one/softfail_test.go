package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"lab/apps/one/repo"
)

// failTrigger makes the database refuse one record: a row of the table whose
// column holds the value cannot be written. It stands for any single record
// that cannot be stored.
func failTrigger(t *testing.T, dataDir, name, table, col, val string) (drop func()) {
	t.Helper()
	db, err := sql.Open("sqlite3", "file:"+filepath.Join(dataDir, "one.db")+"?_busy_timeout=5000")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.Exec("CREATE TRIGGER " + name + " BEFORE INSERT ON " + table + " WHEN NEW." + col + " = '" + val + "' BEGIN SELECT RAISE(ABORT, 'refused'); END"); err != nil {
		t.Fatal(err)
	}
	return func() {
		if _, err := db.Exec("DROP TRIGGER " + name); err != nil {
			t.Fatal(err)
		}
	}
}

// capturedLog puts a JSON logger in front of the app's own error lines.
func capturedLog(a *app) func() []map[string]any {
	var buf bytes.Buffer
	a.log = slog.New(slog.NewJSONHandler(&buf, nil))
	return func() []map[string]any {
		var out []map[string]any
		for _, l := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
			var m map[string]any
			if json.Unmarshal([]byte(l), &m) == nil {
				out = append(out, m)
			}
		}
		return out
	}
}

// A page with one message that fails to store imports the others; the failed
// one is logged without its words, counted on the run, and tried again on the
// next run.
func TestOneBadMessageDoesNotStopThePageAndIsTriedAgain(t *testing.T) {
	e := newEnv(t)
	lines := capturedLog(e.a)
	e.box.add(e.in("A", "First of A", 3))
	e.box.add(e.in("B", "BOOM secret subject", 2))
	e.box.add(e.in("C", "Third", 1))
	drop := failTrigger(t, e.a.dataDir, "refuse_boom", "messages", "subject", "BOOM secret subject")
	e.must(e.importMonth())
	r := e.lastRun("month")
	if r.Status != "done" || r.Stored != 2 || e.a.failedCount(e.ctx, srcGmail) != 1 {
		t.Fatalf("run: %+v", r)
	}
	if e.a.store.CountRows(e.ctx, "messages") != 2 {
		t.Fatal("the other two messages were not stored")
	}
	var found bool
	for _, l := range lines() {
		raw, _ := json.Marshal(l)
		if strings.Contains(string(raw), "BOOM") || strings.Contains(string(raw), "alice@example.test") {
			t.Fatalf("the log holds message words: %s", raw)
		}
		if l["msg"] == "record left out" && l["level"] == "ERROR" && l["source"] == "gmail" && l["id"] == "1002" {
			found = true
		}
	}
	if !found {
		t.Fatalf("no error line for the message: %v", lines())
	}
	// Nothing is half stored, and the next run, with the cause gone, takes it.
	if e.a.store.CountRows(e.ctx, "raw") != 2 {
		t.Fatalf("a raw row of the failed message was left: %d", e.a.store.CountRows(e.ctx, "raw"))
	}
	drop()
	e.must(e.sync())
	if e.a.store.CountRows(e.ctx, "messages") != 3 {
		t.Fatal("the failed message was not tried again")
	}
	if e.a.failedCount(e.ctx, srcGmail) != 0 {
		t.Fatal("the message is still in the ledger")
	}
}

// A message that keeps failing is given up after a bound, not read for ever.
func TestAPermanentlyBrokenMessageIsGivenUp(t *testing.T) {
	e := newEnv(t)
	capturedLog(e.a)
	e.box.add(e.in("A", "BOOM", 1))
	failTrigger(t, e.a.dataDir, "refuse_boom", "messages", "subject", "BOOM")
	e.must(e.importMonth())
	if e.a.failedCount(e.ctx, srcGmail) != 1 {
		t.Fatal("the message did not fail")
	}
	for i := 0; i < 10; i++ {
		e.skew.Add(int64(25 * time.Hour / time.Second))
		e.must(e.sync())
	}
	if n := e.a.failedCount(e.ctx, srcGmail); n != 0 {
		t.Fatalf("%d messages still waiting for a try", n)
	}
	if !e.a.failHas(e.ctx, srcGmail, "1001") {
		t.Fatal("the given-up message left no line")
	}
	fetched := len(e.box.fetchedBodies())
	e.skew.Add(int64(25 * time.Hour / time.Second))
	e.must(e.sync())
	if len(e.box.fetchedBodies()) != fetched {
		t.Fatal("a given-up message was read again")
	}
}

// A failure of the connection is not one message's: the run still stops, as
// a failed run that counts toward a failed source.
func TestConnectionFailureStillStopsTheRunAndCountsAsFailedSource(t *testing.T) {
	e := newEnv(t)
	e.box.add(e.in("A", "First", 1))
	e.a.gmail.IMAPAddr = "127.0.0.1:1"
	for i := 0; i < failuresToStop; i++ {
		if err := e.importMonth(); err == nil {
			t.Fatal("a refused connection did not stop the run")
		}
	}
	if r := e.lastRun("month"); r.Status != "failed" {
		t.Fatalf("run: %+v", r)
	}
	if n, _, _, _, _ := e.a.runsStreak(e.ctx, srcGmail); n != failuresToStop {
		t.Fatalf("failed streak %d", n)
	}
	// And the classification itself.
	s := &mailScope{c: e.a.imp.core, src: gmailSrc, run: e.lastRunPtr()}
	cerr := errors.New("connection reset")
	if got := s.tolerate(e.ctx, "x", conn(cerr)); got != cerr {
		t.Fatalf("a connection error was not returned as it was: %v", got)
	}
	cctx, cancel := context.WithCancel(e.ctx)
	cancel()
	if s.tolerate(cctx, "x", errors.New("whatever")) == nil {
		t.Fatal("a stopped context did not stop the run")
	}
	if s.run.Failed != 0 {
		t.Fatal("a fatal error was counted as left out")
	}
}

// A calendar feed with one broken entry stores the rest.
func TestCalendarFeedWithOneBrokenEventStoresTheRest(t *testing.T) {
	e := newGoogleEnv(t)
	lines := capturedLog(e.a)
	e.g.set(gPathA, vcal(
		gevent("u1", "Dentist", "20261120T080000Z", "20261120T090000Z"),
		gevent("u2", "BOOM", "20261121T080000Z", "20261121T090000Z"),
		gevent("u3", "Checkup", "20261122T080000Z", "20261122T090000Z")))
	drop := failTrigger(t, e.a.dataDir, "refuse_ev", "agenda", "title", "BOOM")
	e.connect("Mine", gAddrA)
	if n := e.a.store.CountRows(e.ctx, "agenda"); n != 2 {
		t.Fatalf("stored %d entries, want the 2 good ones", n)
	}
	if r, ok := e.a.store.LatestRun(e.ctx, srcGoogleCal, "sync"); !ok || r.Status != "done" || r.Stored != 2 {
		t.Fatalf("run: %+v", r)
	}
	var found bool
	for _, l := range lines() {
		raw, _ := json.Marshal(l)
		if strings.Contains(string(raw), "BOOM") {
			t.Fatalf("the log holds an entry's words: %s", raw)
		}
		found = found || (l["msg"] == "record left out" && l["kind"] == "event" && l["level"] == "ERROR")
	}
	if !found {
		t.Fatal("no error line for the event")
	}
	drop()
	e.mustRead() // the feed is read whole again: the entry comes
	if n := e.a.store.CountRows(e.ctx, "agenda"); n != 3 {
		t.Fatalf("after the cause is gone: %d entries", n)
	}
}

// A bridge batch with one bad message stores the rest and answers 200 with
// left_out; the message is then asked for again through the outbox, and
// stored when it comes.
func TestBridgeBatchWithOneBadMessageStoresTheRest(t *testing.T) {
	e := newChatEnv(t)
	lines := capturedLog(e.a)
	drop := failTrigger(t, e.a.dataDir, "refuse_chat", "messages", "text", "BOOM text")
	ans := e.batch(chatOf("signal", "c1", "Alice", "direct",
		e.in("m1", "alice", "Alice", "hello", 10), e.in("m2", "alice", "Alice", "BOOM text", 9), e.in("m3", "alice", "Alice", "bye", 8)))
	if ans["stored"] != float64(2) || ans["left_out"] != float64(1) {
		t.Fatalf("answer %v", ans)
	}
	if e.a.store.CountRows(e.ctx, "messages") != 2 || e.a.store.CountRows(e.ctx, "raw") != 2 {
		t.Fatal("the rest was not stored whole")
	}
	for _, l := range lines() {
		raw, _ := json.Marshal(l)
		if strings.Contains(string(raw), "BOOM") || strings.Contains(string(raw), "c1") {
			t.Fatalf("the log holds chat words or the chat id: %s", raw)
		}
	}
	// The bridge only re-sends what it is asked for: the outbox names it.
	out := answerOf(t, e.asBridge("POST", "/api/bridge/outbox", map[string]any{}))
	rs, _ := out["resend"].([]any)
	if len(rs) != 1 || rs[0].(map[string]any)["message_id"] != "m2" || rs[0].(map[string]any)["chat_id"] != "c1" {
		t.Fatalf("resend %v", out["resend"])
	}
	drop()
	rec := e.asBridge("POST", "/api/bridge/batch", bridgeBatch{Again: true, Chats: []bridgeChat{chatOf("signal", "c1", "Alice", "direct",
		e.in("m2", "alice", "Alice", "BOOM text", 9))}})
	if rec.Code != 200 || e.a.store.CountRows(e.ctx, "messages") != 3 {
		t.Fatalf("the resent message was not stored: %d %s", rec.Code, rec.Body.String())
	}
	if e.a.failedCount(e.ctx, "chat") != 0 {
		t.Fatal("still in the ledger")
	}
}

// A new chat none of whose messages can be stored leaves no item behind.
func TestBridgeNewChatWithNothingStorableLeavesNoItem(t *testing.T) {
	e := newChatEnv(t)
	capturedLog(e.a)
	failTrigger(t, e.a.dataDir, "refuse_chat", "messages", "text", "BOOM text")
	ans := e.batch(chatOf("signal", "c9", "Bob", "direct", e.in("m1", "bob", "Bob", "BOOM text", 5)))
	if ans["left_out"] != float64(1) || e.a.store.CountRows(e.ctx, "items") != 0 {
		t.Fatalf("answer %v, items %d", ans, e.a.store.CountRows(e.ctx, "items"))
	}
}

// --- panics ---

// seePanics records the panics the app recovers, for the length of a test.
func seePanics(t *testing.T) *[]string {
	t.Helper()
	var seen []string
	h := func(unit string, v any) { seen = append(seen, unit) }
	panicSeen.Store(&h)
	t.Cleanup(func() { panicSeen.Store(nil) })
	return &seen
}

// A panicking unit of background work is logged (the app's frames, no data)
// and the loop that runs it survives.
func TestPanickingUnitIsLoggedAndTheLoopSurvives(t *testing.T) {
	a, _ := newTestApp(t)
	lines := capturedLog(a)
	seen := seePanics(t)
	ran := 0
	for i := 0; i < 3; i++ {
		panicked := a.runUnit("test unit", func() {
			ran++
			if i == 1 {
				var m map[string]int
				m["secret words in a key"] = 1 // a panic of the runtime
			}
		})
		if panicked != (i == 1) {
			t.Fatalf("round %d panicked=%v", i, panicked)
		}
	}
	if ran != 3 || len(*seen) != 1 || (*seen)[0] != "test unit" {
		t.Fatalf("ran %d, seen %v", ran, *seen)
	}
	var found bool
	for _, l := range lines() {
		raw, _ := json.Marshal(l)
		if strings.Contains(string(raw), "secret words") {
			t.Fatalf("the log holds data: %s", raw)
		}
		if l["msg"] == "panic in background work" && l["level"] == "ERROR" && l["unit"] == "test unit" {
			found = strings.Contains(string(raw), "TestPanickingUnit")
		}
	}
	if !found {
		t.Fatalf("no error line with the app's own frames: %v", lines())
	}
}

// A panic while one message is stored is that message's failure: the rest of
// the page is stored, and the next run tries the message again.
func TestPanicInOneMessageLeavesTheRestOfThePage(t *testing.T) {
	e := newEnv(t)
	capturedLog(e.a)
	seen := seePanics(t)
	e.box.add(e.in("A", "First", 3))
	e.box.add(e.in("B", "Second", 2))
	e.box.add(e.in("C", "Third", 1))
	boom := true
	e.a.beforeStore = func(subject string) {
		if boom && subject == "Second" {
			panic("boom in the middle of storing")
		}
	}
	e.must(e.importMonth())
	if e.a.store.CountRows(e.ctx, "messages") != 2 || len(*seen) != 1 {
		t.Fatalf("messages %d, panics %v", e.a.store.CountRows(e.ctx, "messages"), *seen)
	}
	boom = false
	e.must(e.sync())
	if e.a.store.CountRows(e.ctx, "messages") != 3 {
		t.Fatal("the message that panicked was not tried again")
	}
}

// A transaction that panics is taken back, not left open holding the lock.
func TestPanicInATransactionIsRolledBack(t *testing.T) {
	a, _ := newTestApp(t)
	func() {
		defer func() { recover() }()
		a.store.Update(context.Background(), func(tx *repo.Tx) error {
			tx.SetSetting(context.Background(), "k", "v")
			panic("x")
		})
	}()
	if a.store.Setting(context.Background(), "k") != "" {
		t.Fatal("the panicking transaction was kept")
	}
	done := make(chan error, 1)
	go func() { done <- a.store.SetSetting(context.Background(), "k2", "v") }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the write lock is still held")
	}
}

// --- database health ---

// A failed quick check is logged at error and fails healthz; a later check
// that passes makes it answer ok again; a check that ran out of time changes
// nothing.
func TestQuickCheckFailureFailsHealthz(t *testing.T) {
	a, h := newTestApp(t)
	lines := capturedLog(a)
	if rec := get(h, "/healthz"); rec.Code != 200 || rec.Body.String() != "ok" {
		t.Fatalf("healthz at first: %d %s", rec.Code, rec.Body.String())
	}
	a.quickCheck = func(context.Context) error { return errors.New("quick_check found: page 7 is bad") }
	if a.checkDatabase(context.Background(), time.Second, "daily") {
		t.Fatal("a failed check was called sound")
	}
	if rec := get(h, "/healthz"); rec.Code != 503 {
		t.Fatalf("healthz after a failed check: %d", rec.Code)
	}
	var logged bool
	for _, l := range lines() {
		logged = logged || (l["msg"] == "database check failed" && l["level"] == "ERROR")
	}
	if !logged {
		t.Fatal("the failure was not logged at error")
	}
	a.quickCheck = func(context.Context) error { return repo.ErrCheckTimeout }
	a.checkDatabase(context.Background(), time.Second, "daily")
	if rec := get(h, "/healthz"); rec.Code != 503 {
		t.Fatal("a check that timed out cleared the failure")
	}
	a.quickCheck = func(context.Context) error { return nil }
	if !a.checkDatabase(context.Background(), time.Second, "daily") {
		t.Fatal("a passing check was not sound")
	}
	if rec := get(h, "/healthz"); rec.Code != 200 {
		t.Fatalf("healthz after a passing check: %d", rec.Code)
	}
}

// The real check passes on a sound database and names a damaged one.
func TestQuickCheckOnARealDatabase(t *testing.T) {
	a, _ := newTestApp(t)
	for i := 0; i < 400; i++ {
		a.store.SetSetting(context.Background(), "k"+strings.Repeat("x", i%50)+string(rune('a'+i%26))+time.Duration(i).String(), strings.Repeat("v", 200))
	}
	if err := a.store.QuickCheck(context.Background()); err != nil {
		t.Fatalf("a sound database: %v", err)
	}
	cctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := a.store.QuickCheck(cctx); !errors.Is(err, repo.ErrCheckTimeout) {
		t.Fatalf("a stopped check: %v", err)
	}
}
