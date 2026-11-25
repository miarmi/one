package repo

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func open(t *testing.T) (*Store, context.Context) {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s, context.Background()
}

func sampleRaw() Raw {
	return Raw{Source: "gmail", SourceID: "1", VersionKey: "a@0", ThreadKey: "gmail:1", ReceivedAt: 10, IngestedAt: 11, BlobSHA: "ab", Payload: "{}"}
}

// TestRepeatKnownRawRowReportsNotInserted is the "repeat" test of the
// storage layer: a row that is already there is reported as not inserted,
// also straight after an insert into another table, whose row id is then the
// last one the connection saw.
func TestRepeatKnownRawRowReportsNotInserted(t *testing.T) {
	s, ctx := open(t)
	err := s.Update(ctx, func(tx *Tx) error {
		id, ok, err := tx.InsertRaw(ctx, sampleRaw())
		if err != nil || !ok || id == 0 {
			t.Fatalf("first insert: id=%d ok=%v err=%v", id, ok, err)
		}
		// An insert into another table moves the connection's last row id.
		if err := tx.EnsureRule(ctx, Rule{Name: "r", Enabled: true, Source: "gmail", Field: "sender", Op: "equals", Value: "x", CreatedBy: "t"}, 1); err != nil {
			return err
		}
		if err := tx.InsertEvent(ctx, 1, 0, "system", "ingested", nil); err != nil {
			return err
		}
		id2, ok2, err := tx.InsertRaw(ctx, sampleRaw())
		if err != nil {
			return err
		}
		if ok2 || id2 != 0 {
			t.Fatalf("a known raw row came back as inserted: id=%d ok=%v", id2, ok2)
		}
		sk := Skipped{At: 1, Source: "gmail", SourceID: "9", Sender: "a@b", Subject: "s", ReceivedAt: 1, RuleID: 1}
		if ok, err := tx.InsertSkipped(ctx, sk); err != nil || !ok {
			t.Fatalf("skipped insert: %v %v", ok, err)
		}
		if ok, err := tx.InsertSkipped(ctx, sk); err != nil || ok {
			t.Fatalf("a known skipped row came back as inserted: %v %v", ok, err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if n := s.CountRows(ctx, "raw"); n != 1 {
		t.Fatalf("%d raw rows, want 1", n)
	}
}

func TestRawAndEventsAreAppendOnly(t *testing.T) {
	s, ctx := open(t)
	if _, ok, err := s.InsertRaw(ctx, sampleRaw()); err != nil || !ok {
		t.Fatal(ok, err)
	}
	if err := s.InsertEvent(ctx, 1, 0, "system", "ingested", nil); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		"UPDATE raw SET payload='x'", "DELETE FROM raw", "UPDATE events SET type='x'", "DELETE FROM events",
	} {
		if _, err := s.db.ExecContext(ctx, q); err == nil {
			t.Errorf("%q was allowed", q)
		}
	}
}

func TestItemNumbersCountUp(t *testing.T) {
	s, ctx := open(t)
	for i, want := range []string{"Q-0001", "Q-0002"} {
		var got Item
		err := s.Update(ctx, func(tx *Tx) error {
			var err error
			got, err = tx.CreateItem(ctx, Item{Kind: "thread", Subject: "s", Stage: "new", FirstAt: 1, LastAt: 1}, "gmail_thread", string(rune('a'+i)), "system", 1)
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		if Number(got.Number) != want {
			t.Errorf("item %d is %s, want %s", i, Number(got.Number), want)
		}
	}
}

func TestSettingsKeepTheTrialsRowsAfterUpgrade(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	// The trial's database: settings with only key and value.
	old, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	old.db.Exec("DROP TABLE settings")
	old.db.Exec("CREATE TABLE settings (key TEXT PRIMARY KEY, value TEXT NOT NULL)")
	old.db.Exec("INSERT INTO settings (key, value) VALUES ('a', 'b')")
	old.Close()
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	if s.Setting(ctx, "a") != "b" {
		t.Fatal("the old setting is gone")
	}
	if err := s.SetSetting(ctx, "c", "d"); err != nil || s.Setting(ctx, "c") != "d" {
		t.Fatal("cannot write after the upgrade", err)
	}
}

// TestOldItemsTableGetsItsPlaceColumn: a database made before the list page
// has items without the column that orders a group; opening it adds the
// column, fills it from the last message and loses no item.
func TestOldItemsTableGetsItsPlaceColumn(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	db, err := sql.Open("sqlite3", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`CREATE TABLE items (
		id INTEGER PRIMARY KEY, number INTEGER NOT NULL UNIQUE, kind TEXT NOT NULL, subject TEXT NOT NULL, stage TEXT NOT NULL,
		needs_claude INTEGER NOT NULL DEFAULT 0, pending_since INTEGER, first_message_at INTEGER NOT NULL, last_message_at INTEGER NOT NULL,
		reviewed_at INTEGER, resolved_at INTEGER, closed_at INTEGER, outcome TEXT NOT NULL DEFAULT '', alias_numbers TEXT NOT NULL DEFAULT '');
		INSERT INTO items (number, kind, subject, stage, first_message_at, last_message_at) VALUES (1, 'thread', 's', 'new', 100, 500);`)
	db.Close()
	if err != nil {
		t.Fatal(err)
	}
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	it, ok := s.ItemByID(context.Background(), 1)
	if !ok || it.MovedAt != 500 || it.Subject != "s" {
		t.Fatalf("item after the upgrade: %+v %v", it, ok)
	}
	rows, err := s.ListItems(context.Background(), "new", 10, 0)
	if err != nil || len(rows) != 1 {
		t.Fatalf("list after the upgrade: %v %v", rows, err)
	}
	s.Close()
	if s2, err := Open(path); err != nil {
		t.Fatalf("a second open: %v", err)
	} else {
		s2.Close()
	}
}

// TestRepairNamesRewritesOnlyUndecodedNames: names stored as undecoded mail
// header words are rewritten once; other names are left alone.
func TestRepairNamesRewritesOnlyUndecodedNames(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "one.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	for _, n := range []string{"=?UTF-8?Q?X?=", "Plain"} {
		if _, err := st.db.ExecContext(ctx, `INSERT INTO people (name, updated_by, updated_at) VALUES (?, 'system', 1)`, n); err != nil {
			t.Fatal(err)
		}
	}
	decode := func(s string) string { return strings.ReplaceAll(s, "=?UTF-8?Q?X?=", "Decoded") }
	if n, err := st.RepairNames(ctx, decode); err != nil || n != 1 {
		t.Fatalf("first repair: %d %v", n, err)
	}
	if n, err := st.RepairNames(ctx, decode); err != nil || n != 0 {
		t.Fatalf("second repair: %d %v", n, err)
	}
	var got string
	st.db.QueryRowContext(ctx, `SELECT group_concat(name, '|') FROM (SELECT name FROM people ORDER BY id)`).Scan(&got)
	if got != "Decoded|Plain" {
		t.Fatalf("names: %q", got)
	}
}

// TestPipelineRuleIsWidenedOnceOnAnExistingDatabase: a database that holds the
// starting rule for Gmail only gets it widened to both mail sources when it
// is opened, once; a rule the owner changed, or narrows again later, stays as it is.
func TestPipelineRuleIsWidenedOnceOnAnExistingDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	ctx := context.Background()
	old := Rule{Name: "GitHub pipeline mails", Enabled: true, Source: "gmail", Field: "header:x-github-reason", Op: "equals", Value: "ci_activity", CreatedBy: "system"}
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	// Make the database look like one from before the change: the old rule and no marker.
	if err := s.Update(ctx, func(tx *Tx) error { return tx.EnsureRule(ctx, old, 1) }); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec("DELETE FROM settings WHERE key LIKE 'migration:%'"); err != nil {
		t.Fatal(err)
	}
	s.Close()

	source := func() string {
		s, err := Open(path)
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
		r, ok := s.RuleByName(ctx, "GitHub pipeline mails")
		if !ok {
			t.Fatal("the rule is gone")
		}
		return r.Source
	}
	if got := source(); got != "any" {
		t.Fatalf("after opening an old database the rule covers %q", got)
	}
	for _, src := range []string{"gmail", "outlook"} {
		s, _ := Open(path)
		rules, err := s.EnabledRules(ctx, src)
		s.Close()
		if err != nil || len(rules) != 1 {
			t.Fatalf("%s: %v %v", src, rules, err)
		}
	}
	// The owner narrows it again: opening the database later does not undo that.
	s, _ = Open(path)
	if _, err := s.db.Exec("UPDATE rules SET source='gmail' WHERE name='GitHub pipeline mails'"); err != nil {
		t.Fatal(err)
	}
	s.Close()
	if got := source(); got != "gmail" {
		t.Fatalf("the widening ran twice: %q", got)
	}

	// A rule the owner had edited before is left alone.
	path2 := filepath.Join(t.TempDir(), "edited.db")
	s, _ = Open(path2)
	edited := old
	edited.Value = "something_else"
	s.Update(ctx, func(tx *Tx) error { return tx.EnsureRule(ctx, edited, 1) })
	s.db.Exec("DELETE FROM settings WHERE key LIKE 'migration:%'")
	s.Close()
	s, _ = Open(path2)
	r, _ := s.RuleByName(ctx, "GitHub pipeline mails")
	s.Close()
	if r.Source != "gmail" {
		t.Fatalf("an edited rule was widened: %+v", r)
	}
}

// TestAnOldDatabaseGetsTheMarkColumnsAndTheStricterTriggers: a file made before
// test runs existed is upgraded on open, keeps its rows unmarked, and from then
// on refuses to delete an unmarked raw row or event.
func TestAnOldDatabaseGetsTheMarkColumnsAndTheStricterTriggers(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := s.Update(ctx, func(tx *Tx) error { _, _, err := tx.InsertRaw(ctx, sampleRaw()); return err }); err != nil {
		t.Fatal(err)
	}
	// Turn the file back into an old one: no mark columns, no ledger, the old triggers.
	db := s.db
	idx, _ := db.Query(`SELECT name FROM sqlite_master WHERE type = 'index' AND sql LIKE '%test_run%'`)
	var names []string
	for idx.Next() {
		var n string
		idx.Scan(&n)
		names = append(names, n)
	}
	idx.Close()
	for _, n := range names {
		if _, err := db.Exec(`DROP INDEX ` + n); err != nil {
			t.Fatal(err)
		}
	}
	for _, tbl := range []string{"raw", "events"} {
		if _, err := db.Exec(`DROP TRIGGER ` + tbl + `_no_delete`); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`CREATE TRIGGER ` + tbl + `_no_delete BEFORE DELETE ON ` + tbl + ` BEGIN SELECT RAISE(ABORT, 'append only'); END`); err != nil {
			t.Fatal(err)
		}
	}
	for _, tbl := range RunTables {
		var err error
		if tbl == "pool_marks" {
			_, err = db.Exec(`DROP TABLE pool_marks`)
		} else {
			_, err = db.Exec(`ALTER TABLE ` + tbl + ` DROP COLUMN test_run`)
		}
		if err != nil {
			t.Fatal(tbl, err)
		}
	}
	s.Close()
	s, err = Open(path)
	if err != nil {
		t.Fatalf("reopening an old file: %v", err)
	}
	defer s.Close()
	for _, tbl := range RunTables {
		if tbl == "pool_marks" {
			continue
		}
		var n int
		if err := s.db.QueryRow(`SELECT COUNT(*) FROM ` + tbl + ` WHERE test_run = ''`).Scan(&n); err != nil {
			t.Errorf("%s has no mark column: %v", tbl, err)
		}
	}
	var rows int
	s.db.QueryRow(`SELECT COUNT(*) FROM raw WHERE test_run = ''`).Scan(&rows)
	if rows != 1 {
		t.Errorf("the old row is not unmarked: %d", rows)
	}
	if _, err := s.db.Exec(`DELETE FROM raw`); err == nil {
		t.Error("an unmarked raw row could be deleted")
	}
	if _, err := s.db.Exec(`UPDATE raw SET test_run = 'e2e-abcdefgh1'`); err == nil {
		t.Error("an unmarked raw row could be marked")
	}
}

// TestPendingFilesOfAChatAreNotMails: a chat message has no stored original
// (its raw blob hash is empty), so the mail sync's retry of pending files must
// not list it; a mail with a stored original is still listed.
func TestPendingFilesOfAChatAreNotMails(t *testing.T) {
	s, ctx := open(t)
	add := func(source, srcID, blob string) int64 {
		var msgID int64
		err := s.Update(ctx, func(tx *Tx) error {
			raw := Raw{Source: source, SourceID: srcID, VersionKey: "v", ThreadKey: source + ":" + srcID, ReceivedAt: 10, IngestedAt: 11, BlobSHA: blob, Payload: "{}"}
			rawID, _, err := tx.InsertRaw(ctx, raw)
			if err != nil {
				return err
			}
			it, err := tx.CreateItemOnly(ctx, Item{Kind: "mail", Subject: "s", Stage: "new", FirstAt: 10, LastAt: 10})
			if err != nil {
				return err
			}
			msgID, _, err = tx.InsertMessage(ctx, Message{ItemID: it.ID, RawID: rawID, Source: source, SourceID: srcID, ThreadKey: raw.ThreadKey, Sender: "a@b", ReceivedAt: 10, Text: "x"})
			if err != nil {
				return err
			}
			_, err = tx.InsertFile(ctx, File{ItemID: it.ID, Source: source, MessageID: msgID, Part: "1", Filename: "f.pdf", ContentType: "application/pdf", Disposition: "attachment", Status: "pending", AddedAt: 12})
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		return msgID
	}
	chat := add("beeper", "c1", "")
	mail := add("gmail", "m1", "ab")
	ids, err := s.MessagesWithPendingFiles(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 1 || ids[0] != mail {
		t.Fatalf("pending list %v: want only the mail %d, never the chat %d", ids, mail, chat)
	}
}

func TestQuickCheckNamesADamagedDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "one.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 500; i++ {
		s.SetSetting(context.Background(), fmt.Sprintf("key-%d", i), strings.Repeat("value", 80))
	}
	if err := s.QuickCheck(context.Background()); err != nil {
		t.Fatalf("a sound database: %v", err)
	}
	s.Close()
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	fi, _ := f.Stat()
	junk := bytes.Repeat([]byte{0xFF}, 2048)
	f.WriteAt(junk, fi.Size()/2)
	f.Close()
	s2, err := Open(path)
	if err != nil {
		return // refused at open: it is not served either
	}
	defer s2.Close()
	if err := s2.QuickCheck(context.Background()); err == nil {
		t.Fatal("a damaged database passed the check")
	}
}

// An upgrade that fails midway changes nothing: the old schema stays, and
// Open refuses with a plain error (main logs it and does not serve). Once the
// cause is gone the same file upgrades completely, backfill included.
func TestUpgradeThatFailsMidwayLeavesTheOldSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	db, err := sql.Open("sqlite3", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	// An old items table, and a table that stands in the way of a later step.
	if _, err := db.Exec(`CREATE TABLE items (
		id INTEGER PRIMARY KEY, number INTEGER NOT NULL UNIQUE, kind TEXT NOT NULL, subject TEXT NOT NULL, stage TEXT NOT NULL,
		needs_claude INTEGER NOT NULL DEFAULT 0, pending_since INTEGER, first_message_at INTEGER NOT NULL, last_message_at INTEGER NOT NULL,
		reviewed_at INTEGER, resolved_at INTEGER, closed_at INTEGER, outcome TEXT NOT NULL DEFAULT '', alias_numbers TEXT NOT NULL DEFAULT '');
		INSERT INTO items (number, kind, subject, stage, first_message_at, last_message_at) VALUES (1, 'thread', 's', 'new', 100, 500);
		CREATE TABLE items_stage (x INTEGER);`); err != nil {
		t.Fatal(err)
	}
	s, err := Open(path)
	if err == nil {
		s.Close()
		t.Fatal("a failing upgrade must stop the start")
	}
	if !strings.Contains(err.Error(), "rolled back") {
		t.Errorf("the error says what happened: %v", err)
	}
	have, err := columns(db, "items")
	if err != nil {
		t.Fatal(err)
	}
	if have["moved_at"] || have["hidden"] {
		t.Errorf("a half-made upgrade was kept: %v", have)
	}
	var tables int
	db.QueryRow("SELECT COUNT(*) FROM sqlite_master WHERE name IN ('events', 'finder', 'rules')").Scan(&tables)
	if tables != 0 {
		t.Errorf("%d tables of the new schema were kept", tables)
	}
	// The cause goes away: the next start applies everything, with the backfill.
	if _, err := db.Exec("DROP TABLE items_stage"); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if it, ok := s.ItemByID(context.Background(), 1); !ok || it.MovedAt != 500 {
		t.Errorf("after the fixed upgrade: %+v %v", it, ok)
	}
}
