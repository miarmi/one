// Package repo is the only code of ONE that touches the database. Raw rows
// and events are append-only: the schema itself refuses to change or remove
// them. Times are Unix seconds in UTC.
package repo

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/mattn/go-sqlite3"
)

// execer is what both a database and a transaction can do.
type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// Q holds every query. A Store runs them on the database, a Tx inside one
// transaction.
type Q struct{ x execer }

// Store is the open database.
type Store struct {
	Q
	db   *sql.DB
	path string
}

// Tx is one write transaction; it takes the write lock when it begins.
type Tx struct {
	Q
	tx *sql.Tx
}

// Open opens (and creates or upgrades) the database file.
func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite3", "file:"+path+"?_journal_mode=WAL&_busy_timeout=5000&_foreign_keys=on&_synchronous=NORMAL&_txlock=immediate")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(4)
	// The schema and every upgrade step run in ONE transaction: an upgrade that
	// fails midway leaves the old schema as it was, and the app does not start
	// (main logs the error and exits), instead of running on a half-made one.
	if err := upgrade(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("the database upgrade failed and was rolled back; the old schema is kept: %w", err)
	}
	return &Store{Q: Q{x: db}, db: db, path: path}, nil
}

// schemaDB is what a schema step needs: a database or one transaction.
type schemaDB interface {
	Exec(query string, args ...any) (sql.Result, error)
	Query(query string, args ...any) (*sql.Rows, error)
	QueryRow(query string, args ...any) *sql.Row
}

// upgrade makes the tables and applies every upgrade step, all or nothing.
func upgrade(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(schema + chatSchema); err != nil {
		return err
	}
	if err := migrate(tx); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) Ping(ctx context.Context) error {
	var one int
	return s.db.QueryRowContext(ctx, "SELECT 1").Scan(&one)
}

// Update runs fn in one write transaction: all of it or nothing.
func (s *Store) Update(ctx context.Context, fn func(*Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	done := false
	defer func() {
		// A panic inside fn must not leave the transaction open: it would hold
		// the write lock and a connection for good. Take it back, then let the
		// panic go on to whoever recovers it.
		if !done {
			tx.Rollback()
		}
	}()
	if err := fn(&Tx{Q: Q{x: tx}, tx: tx}); err != nil {
		done = true
		tx.Rollback()
		return err
	}
	done = true
	return tx.Commit()
}

// Try runs fn as one step inside the transaction: if it fails, only what the
// step wrote is taken back (a savepoint) and the transaction goes on, so one
// bad record can be left out without losing the rest. A step is atomic: all
// of it or nothing.
func (t *Tx) Try(ctx context.Context, fn func() error) error {
	if _, err := t.tx.ExecContext(ctx, "SAVEPOINT one_step"); err != nil {
		return err
	}
	if err := fn(); err != nil {
		if _, rerr := t.tx.ExecContext(ctx, "ROLLBACK TO one_step"); rerr != nil {
			return rerr
		}
		t.tx.ExecContext(ctx, "RELEASE one_step")
		return err
	}
	_, err := t.tx.ExecContext(ctx, "RELEASE one_step")
	return err
}

// Unavailable says that err means the database itself cannot be used (busy,
// locked, full disk, damaged, closed), as opposed to one record being
// refused (a constraint, a bad value). The first stops a run; the second is
// one record's trouble.
func Unavailable(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, sql.ErrConnDone) || errors.Is(err, sql.ErrTxDone) {
		return true
	}
	var se sqlite3.Error
	if errors.As(err, &se) {
		switch se.Code {
		case sqlite3.ErrBusy, sqlite3.ErrLocked, sqlite3.ErrIoErr, sqlite3.ErrFull, sqlite3.ErrCorrupt,
			sqlite3.ErrNotADB, sqlite3.ErrCantOpen, sqlite3.ErrReadonly, sqlite3.ErrNomem, sqlite3.ErrInterrupt, sqlite3.ErrProtocol:
			return true
		}
	}
	return strings.Contains(err.Error(), "database is closed")
}

// migrate upgrades the tables the trial slice already made.
func migrate(db schemaDB) error {
	have, err := columns(db, "settings")
	if err != nil {
		return err
	}
	if !have["updated_by"] {
		if _, err := db.Exec("ALTER TABLE settings ADD COLUMN updated_by TEXT NOT NULL DEFAULT ''"); err != nil {
			return err
		}
	}
	if !have["updated_at"] {
		if _, err := db.Exec("ALTER TABLE settings ADD COLUMN updated_at INTEGER NOT NULL DEFAULT 0"); err != nil {
			return err
		}
	}
	// The place of an item in its group: the time it last took its place there.
	items, err := columns(db, "items")
	if err != nil {
		return err
	}
	if !items["moved_at"] {
		if _, err := db.Exec("ALTER TABLE items ADD COLUMN moved_at INTEGER NOT NULL DEFAULT 0"); err != nil {
			return err
		}
		if _, err := db.Exec("UPDATE items SET moved_at = last_message_at"); err != nil {
			return err
		}
	}
	// SPEC 11: a new item waits for Claude's first look and is hidden from
	// The owner's list until then. Items already waiting when this column is added
	// (never reopened) are such items.
	if !items["hidden"] {
		if _, err := db.Exec("ALTER TABLE items ADD COLUMN hidden INTEGER NOT NULL DEFAULT 0"); err != nil {
			return err
		}
		if _, err := db.Exec(`UPDATE items SET hidden = 1 WHERE needs_claude = 1 AND stage = 'new'
			AND NOT EXISTS (SELECT 1 FROM events e WHERE e.item_id = items.id AND e.type IN ('reopened', 'opened', 'noted'))`); err != nil {
			return err
		}
	}
	// SPEC 10: a row joined into another keeps its place, hidden, and points at the row that stays.
	if !items["merged_into"] {
		if _, err := db.Exec("ALTER TABLE items ADD COLUMN merged_into INTEGER NOT NULL DEFAULT 0"); err != nil {
			return err
		}
	}
	// SPEC 17: a chat that is still being written in waits for the chat quiet time.
	if !items["chat_since"] {
		if _, err := db.Exec("ALTER TABLE items ADD COLUMN chat_since INTEGER NOT NULL DEFAULT 0"); err != nil {
			return err
		}
	}
	// SPEC 4: a chat message whose text was edited is marked.
	msgs, err := columns(db, "messages")
	if err != nil {
		return err
	}
	if !msgs["edited"] {
		if _, err := db.Exec("ALTER TABLE messages ADD COLUMN edited INTEGER NOT NULL DEFAULT 0"); err != nil {
			return err
		}
	}
	// SPEC 30: a chat message deleted in the network stays, marked as deleted.
	if !msgs["deleted"] {
		if _, err := db.Exec("ALTER TABLE messages ADD COLUMN deleted INTEGER NOT NULL DEFAULT 0"); err != nil {
			return err
		}
	}
	// SPEC 21: Import again for a chat message is a request the bridge answers.
	skp, err := columns(db, "skipped")
	if err != nil {
		return err
	}
	if !skp["again_at"] {
		if _, err := db.Exec("ALTER TABLE skipped ADD COLUMN again_at INTEGER NOT NULL DEFAULT 0"); err != nil {
			return err
		}
	}
	if _, err = db.Exec("CREATE INDEX IF NOT EXISTS items_stage ON items (stage, moved_at DESC, id DESC)"); err != nil {
		return err
	}
	// SPEC 25: what the owner calls a person.
	people, err := columns(db, "people")
	if err != nil {
		return err
	}
	if !people["call_name"] {
		if _, err := db.Exec("ALTER TABLE people ADD COLUMN call_name TEXT NOT NULL DEFAULT ''"); err != nil {
			return err
		}
	}
	// SPEC 19: a to-do an agent proposes carries the text of the to-do.
	props, err := columns(db, "proposals")
	if err != nil {
		return err
	}
	if !props["text"] {
		if _, err := db.Exec("ALTER TABLE proposals ADD COLUMN text TEXT NOT NULL DEFAULT ''"); err != nil {
			return err
		}
	}
	// SPEC 13: a reply proposal knows the message it answers and who it goes
	// to, and keeps the text that was sent and how the send ended.
	for _, c := range []struct{ name, def string }{
		{"target_id", "INTEGER NOT NULL DEFAULT 0"},
		{"recipients", "TEXT NOT NULL DEFAULT ''"},
		{"final_text", "TEXT NOT NULL DEFAULT ''"},
		{"send_state", "TEXT NOT NULL DEFAULT ''"},
		{"send_at", "INTEGER NOT NULL DEFAULT 0"},
		{"send_ref", "TEXT NOT NULL DEFAULT ''"},
		{"send_error", "TEXT NOT NULL DEFAULT ''"},
		{"sent_message_id", "INTEGER NOT NULL DEFAULT 0"},
		// SPEC 19: a to-do an agent proposes may carry a time or a due date.
		{"due", "TEXT NOT NULL DEFAULT ''"},
	} {
		if !props[c.name] {
			if _, err := db.Exec("ALTER TABLE proposals ADD COLUMN " + c.name + " " + c.def); err != nil {
				return err
			}
		}
	}
	// SPEC 26: a note can belong to a sender address or a sender domain.
	notes, err := columns(db, "notes")
	if err != nil {
		return err
	}
	if !notes["sender"] {
		if _, err := db.Exec("ALTER TABLE notes ADD COLUMN sender TEXT NOT NULL DEFAULT ''"); err != nil {
			return err
		}
	}
	// SPEC 21: an agent can propose a rule; it applies only once approved.
	rules, err := columns(db, "rules")
	if err != nil {
		return err
	}
	for _, c := range []struct{ name, def string }{
		{"status", "TEXT NOT NULL DEFAULT 'active'"}, // active, proposed or dismissed
		{"reason", "TEXT NOT NULL DEFAULT ''"},
		{"proposed_by", "TEXT NOT NULL DEFAULT ''"},
		{"proposed_at", "INTEGER NOT NULL DEFAULT 0"},
		{"decided_at", "INTEGER NOT NULL DEFAULT 0"},
		{"decided_by", "TEXT NOT NULL DEFAULT ''"},
	} {
		if !rules[c.name] {
			if _, err := db.Exec("ALTER TABLE rules ADD COLUMN " + c.name + " " + c.def); err != nil {
				return err
			}
		}
	}
	if err := widenPipelineRule(db); err != nil {
		return err
	}
	if err := ensureTestRuns(db); err != nil {
		return err
	}
	if err := ensureFinder(db); err != nil {
		return err
	}
	if err := ensureClaude(db); err != nil {
		return err
	}
	return ensureSeenFloor(db)
}

// widenPipelineRule makes the starting rule "GitHub pipeline mails" apply to
// both mail sources, once. A rule the owner changed is left as it is, and so is a
// rule he narrows again later: the marker in the settings says it was done.
func widenPipelineRule(db schemaDB) error {
	const marker = "migration:pipeline-rule-any-source"
	var done int
	if err := db.QueryRow("SELECT COUNT(*) FROM settings WHERE key=?", marker).Scan(&done); err != nil || done > 0 {
		return err
	}
	if _, err := db.Exec(`UPDATE rules SET source='any' WHERE name='GitHub pipeline mails' AND source='gmail' AND created_by='system'
		AND field='header:x-github-reason' AND op='equals' AND value='ci_activity'`); err != nil {
		return err
	}
	_, err := db.Exec("INSERT INTO settings (key, value, updated_by, updated_at) VALUES (?, '1', 'system', ?)", marker, time.Now().Unix())
	return err
}

// columns lists the column names of a table.
func columns(db schemaDB, table string) (map[string]bool, error) {
	have := map[string]bool{}
	rows, err := db.Query("PRAGMA table_info(" + table + ")")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var cid, notnull, pk int
		var name, typ string
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &typ, &notnull, &dflt, &pk); err != nil {
			return nil, err
		}
		have[name] = true
	}
	return have, rows.Err()
}

const schema = `
CREATE TABLE IF NOT EXISTS settings (
	key TEXT PRIMARY KEY,
	value TEXT NOT NULL,
	updated_by TEXT NOT NULL DEFAULT '',
	updated_at INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS tests (
	connector TEXT PRIMARY KEY,
	at INTEGER NOT NULL,
	ok INTEGER NOT NULL,
	result TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS sync_runs (
	id INTEGER PRIMARY KEY,
	source TEXT NOT NULL,
	mode TEXT NOT NULL,
	status TEXT NOT NULL DEFAULT 'running',
	started_at INTEGER NOT NULL,
	finished_at INTEGER,
	fetched INTEGER NOT NULL DEFAULT 0,
	stored INTEGER NOT NULL DEFAULT 0,
	skipped INTEGER NOT NULL DEFAULT 0,
	items_made INTEGER NOT NULL DEFAULT 0,
	files_saved INTEGER NOT NULL DEFAULT 0,
	bytes INTEGER NOT NULL DEFAULT 0,
	error TEXT NOT NULL DEFAULT '',
	cursor TEXT NOT NULL DEFAULT ''
);
CREATE TABLE IF NOT EXISTS raw (
	id INTEGER PRIMARY KEY,
	source TEXT NOT NULL,
	source_id TEXT NOT NULL,
	version_key TEXT NOT NULL,
	thread_key TEXT NOT NULL,
	received_at INTEGER NOT NULL,
	ingested_at INTEGER NOT NULL,
	sync_run_id INTEGER REFERENCES sync_runs(id),
	blob_sha256 TEXT NOT NULL,
	payload TEXT NOT NULL,
	UNIQUE (source, source_id, version_key)
);
CREATE TRIGGER IF NOT EXISTS raw_no_update BEFORE UPDATE ON raw BEGIN SELECT RAISE(ABORT, 'raw rows are append-only'); END;
CREATE TABLE IF NOT EXISTS items (
	id INTEGER PRIMARY KEY,
	number INTEGER NOT NULL UNIQUE,
	kind TEXT NOT NULL,
	subject TEXT NOT NULL,
	stage TEXT NOT NULL,
	needs_claude INTEGER NOT NULL DEFAULT 0,
	pending_since INTEGER,
	first_message_at INTEGER NOT NULL,
	last_message_at INTEGER NOT NULL,
	reviewed_at INTEGER,
	resolved_at INTEGER,
	closed_at INTEGER,
	outcome TEXT NOT NULL DEFAULT '',
	alias_numbers TEXT NOT NULL DEFAULT '',
	moved_at INTEGER NOT NULL DEFAULT 0,
	hidden INTEGER NOT NULL DEFAULT 0,
	merged_into INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS items_last ON items (last_message_at DESC);
CREATE TABLE IF NOT EXISTS item_links (
	id INTEGER PRIMARY KEY,
	item_id INTEGER NOT NULL REFERENCES items(id),
	kind TEXT NOT NULL,
	value TEXT NOT NULL,
	method TEXT NOT NULL,
	linked_by TEXT NOT NULL,
	linked_at INTEGER NOT NULL,
	UNIQUE (kind, value)
);
CREATE TABLE IF NOT EXISTS messages (
	id INTEGER PRIMARY KEY,
	item_id INTEGER NOT NULL REFERENCES items(id),
	raw_id INTEGER NOT NULL REFERENCES raw(id),
	source TEXT NOT NULL,
	source_id TEXT NOT NULL,
	thread_key TEXT NOT NULL,
	network TEXT NOT NULL,
	sender TEXT NOT NULL,
	sender_name TEXT NOT NULL,
	subject TEXT NOT NULL,
	to_addrs TEXT NOT NULL,
	cc_addrs TEXT NOT NULL,
	addressed INTEGER NOT NULL,
	has_attachments INTEGER NOT NULL,
	text TEXT NOT NULL,
	sent_by_me INTEGER NOT NULL,
	received_at INTEGER NOT NULL,
	labels TEXT NOT NULL,
	flags TEXT NOT NULL,
	seen INTEGER NOT NULL,
	starred INTEGER NOT NULL,
	header_id TEXT NOT NULL,
	UNIQUE (source, source_id)
);
CREATE INDEX IF NOT EXISTS messages_item ON messages (item_id, received_at);
CREATE TABLE IF NOT EXISTS events (
	id INTEGER PRIMARY KEY,
	at INTEGER NOT NULL,
	item_id INTEGER REFERENCES items(id),
	agenda_id INTEGER,
	actor TEXT NOT NULL,
	type TEXT NOT NULL,
	data TEXT NOT NULL DEFAULT ''
);
CREATE TRIGGER IF NOT EXISTS events_no_update BEFORE UPDATE ON events BEGIN SELECT RAISE(ABORT, 'events are append-only'); END;
CREATE TABLE IF NOT EXISTS files (
	id INTEGER PRIMARY KEY,
	item_id INTEGER NOT NULL REFERENCES items(id),
	source TEXT NOT NULL,
	message_id INTEGER REFERENCES messages(id),
	event_id INTEGER,
	part TEXT NOT NULL,
	filename TEXT NOT NULL,
	content_type TEXT NOT NULL,
	disposition TEXT NOT NULL,
	content_id TEXT NOT NULL,
	size INTEGER NOT NULL DEFAULT 0,
	sha256 TEXT NOT NULL DEFAULT '',
	path TEXT NOT NULL DEFAULT '',
	status TEXT NOT NULL,
	error TEXT NOT NULL DEFAULT '',
	added_at INTEGER NOT NULL,
	UNIQUE (message_id, part)
);
CREATE INDEX IF NOT EXISTS files_status ON files (status);
CREATE INDEX IF NOT EXISTS files_item ON files (item_id);
CREATE TABLE IF NOT EXISTS rules (
	id INTEGER PRIMARY KEY,
	name TEXT NOT NULL,
	enabled INTEGER NOT NULL DEFAULT 1,
	source TEXT NOT NULL,
	field TEXT NOT NULL,
	op TEXT NOT NULL,
	value TEXT NOT NULL,
	created_at INTEGER NOT NULL,
	created_by TEXT NOT NULL,
	note TEXT NOT NULL DEFAULT ''
);
CREATE TABLE IF NOT EXISTS skipped (
	id INTEGER PRIMARY KEY,
	at INTEGER NOT NULL,
	source TEXT NOT NULL,
	source_id TEXT NOT NULL,
	sender TEXT NOT NULL,
	subject TEXT NOT NULL,
	received_at INTEGER NOT NULL,
	rule_id INTEGER NOT NULL REFERENCES rules(id),
	reimported_at INTEGER,
	UNIQUE (source, source_id)
);
CREATE TABLE IF NOT EXISTS proposals (
	id INTEGER PRIMARY KEY,
	item_id INTEGER NOT NULL REFERENCES items(id),
	action TEXT NOT NULL,
	reason TEXT NOT NULL,
	proposed_by TEXT NOT NULL,
	proposed_at INTEGER NOT NULL,
	base_stage TEXT NOT NULL,
	base_msg_at INTEGER NOT NULL,
	status TEXT NOT NULL DEFAULT 'pending',
	decided_at INTEGER,
	decided_by TEXT NOT NULL DEFAULT '',
	note TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS proposals_item ON proposals (item_id, status);
CREATE TABLE IF NOT EXISTS chat_out (
	id INTEGER PRIMARY KEY,
	item_id INTEGER NOT NULL REFERENCES items(id),
	network TEXT NOT NULL,
	chat_id TEXT NOT NULL,
	grp TEXT NOT NULL,
	seq INTEGER NOT NULL,
	kind TEXT NOT NULL,
	text TEXT NOT NULL DEFAULT '',
	fname TEXT NOT NULL DEFAULT '',
	ctype TEXT NOT NULL DEFAULT '',
	size INTEGER NOT NULL DEFAULT 0,
	sha256 TEXT NOT NULL DEFAULT '',
	path TEXT NOT NULL DEFAULT '',
	file_id INTEGER NOT NULL DEFAULT 0,
	state TEXT NOT NULL,
	error TEXT NOT NULL DEFAULT '',
	ref TEXT NOT NULL DEFAULT '',
	created_at INTEGER NOT NULL,
	handed_at INTEGER NOT NULL DEFAULT 0,
	ended_at INTEGER NOT NULL DEFAULT 0,
	msg_id INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS chat_out_item ON chat_out (item_id, id);
CREATE INDEX IF NOT EXISTS chat_out_state ON chat_out (state, id);
CREATE TABLE IF NOT EXISTS people (
	id INTEGER PRIMARY KEY,
	name TEXT NOT NULL,
	role TEXT NOT NULL DEFAULT '',
	relation TEXT NOT NULL DEFAULT '',
	protected INTEGER NOT NULL DEFAULT 0,
	picture_file_id INTEGER,
	updated_by TEXT NOT NULL,
	updated_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS events_item ON events (item_id, type, id);
CREATE INDEX IF NOT EXISTS messages_sender ON messages (lower(sender));
CREATE TABLE IF NOT EXISTS notes (
	id INTEGER PRIMARY KEY,
	title TEXT NOT NULL,
	body TEXT NOT NULL,
	person_id INTEGER REFERENCES people(id),
	item_id INTEGER REFERENCES items(id),
	created_by TEXT NOT NULL,
	created_at INTEGER NOT NULL,
	updated_by TEXT NOT NULL,
	updated_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS person_pictures (
	person_id INTEGER PRIMARY KEY REFERENCES people(id),
	content_type TEXT NOT NULL,
	data BLOB NOT NULL,
	updated_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS source_pictures (
	source TEXT PRIMARY KEY,
	content_type TEXT NOT NULL,
	data BLOB NOT NULL,
	updated_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS agenda (
	id INTEGER PRIMARY KEY,
	kind TEXT NOT NULL,
	source TEXT NOT NULL DEFAULT '',
	source_id TEXT NOT NULL DEFAULT '',
	cal_key TEXT NOT NULL DEFAULT '',
	calendar TEXT NOT NULL DEFAULT '',
	uid TEXT NOT NULL DEFAULT '',
	title TEXT NOT NULL,
	place TEXT NOT NULL DEFAULT '',
	note TEXT NOT NULL DEFAULT '',
	answers TEXT NOT NULL DEFAULT '',
	start_at INTEGER NOT NULL DEFAULT 0,
	end_at INTEGER NOT NULL DEFAULT 0,
	all_day INTEGER NOT NULL DEFAULT 0,
	start_date TEXT NOT NULL DEFAULT '',
	end_date TEXT NOT NULL DEFAULT '',
	status TEXT NOT NULL DEFAULT 'confirmed',
	state TEXT NOT NULL DEFAULT 'open',
	version INTEGER NOT NULL DEFAULT 1,
	hash TEXT NOT NULL DEFAULT '',
	created_by TEXT NOT NULL DEFAULT '',
	item_id INTEGER REFERENCES items(id),
	created_at INTEGER NOT NULL,
	updated_at INTEGER NOT NULL,
	done_at INTEGER
);
CREATE UNIQUE INDEX IF NOT EXISTS agenda_source ON agenda (source, source_id) WHERE kind = 'event';
CREATE INDEX IF NOT EXISTS agenda_start ON agenda (kind, state, start_at);
CREATE INDEX IF NOT EXISTS agenda_cal ON agenda (cal_key, start_at);
CREATE TABLE IF NOT EXISTS agenda_versions (
	id INTEGER PRIMARY KEY,
	agenda_id INTEGER NOT NULL REFERENCES agenda(id),
	version INTEGER NOT NULL,
	hash TEXT NOT NULL,
	payload TEXT NOT NULL,
	at INTEGER NOT NULL,
	run_id INTEGER,
	UNIQUE (agenda_id, version)
);
CREATE TABLE IF NOT EXISTS person_proposals (
	id INTEGER PRIMARY KEY,
	action TEXT NOT NULL,
	person_id INTEGER NOT NULL,
	other_id INTEGER NOT NULL DEFAULT 0,
	reason TEXT NOT NULL,
	proposed_by TEXT NOT NULL,
	proposed_at INTEGER NOT NULL,
	status TEXT NOT NULL DEFAULT 'pending',
	decided_at INTEGER,
	decided_by TEXT NOT NULL DEFAULT '',
	note TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS person_proposals_status ON person_proposals (status, id);
CREATE TABLE IF NOT EXISTS agenda_proposals (
	id INTEGER PRIMARY KEY,
	action TEXT NOT NULL,
	todo_id INTEGER NOT NULL DEFAULT 0,
	others TEXT NOT NULL DEFAULT '',
	text TEXT NOT NULL DEFAULT '',
	due TEXT NOT NULL DEFAULT '',
	parts TEXT NOT NULL DEFAULT '',
	reason TEXT NOT NULL,
	proposed_by TEXT NOT NULL,
	proposed_at INTEGER NOT NULL,
	status TEXT NOT NULL DEFAULT 'pending',
	decided_at INTEGER,
	decided_by TEXT NOT NULL DEFAULT '',
	note TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS agenda_proposals_status ON agenda_proposals (status, id);
CREATE TABLE IF NOT EXISTS mail_history (
	source TEXT NOT NULL,
	source_id TEXT NOT NULL,
	thread TEXT NOT NULL,
	sender TEXT NOT NULL,
	sender_name TEXT NOT NULL,
	domain TEXT NOT NULL,
	received_at INTEGER NOT NULL,
	seen INTEGER NOT NULL,
	starred INTEGER NOT NULL,
	mine INTEGER NOT NULL,
	answered INTEGER NOT NULL DEFAULT 0,
	state TEXT NOT NULL,
	recorded_at INTEGER NOT NULL,
	PRIMARY KEY (source, source_id)
) WITHOUT ROWID;
CREATE INDEX IF NOT EXISTS mail_history_thread ON mail_history (source, thread, mine, received_at);
CREATE INDEX IF NOT EXISTS mail_history_sender ON mail_history (sender, received_at);
CREATE INDEX IF NOT EXISTS mail_history_domain ON mail_history (domain, received_at);
CREATE TABLE IF NOT EXISTS person_handles (
	person_id INTEGER NOT NULL REFERENCES people(id),
	network TEXT NOT NULL,
	value TEXT NOT NULL,
	PRIMARY KEY (network, value)
);
`

// --- settings and the trial's tests ---

func (q Q) Setting(ctx context.Context, key string) string {
	var v string
	if err := q.x.QueryRowContext(ctx, "SELECT value FROM settings WHERE key = ?", key).Scan(&v); err != nil {
		return ""
	}
	return v
}

func (q Q) SetSetting(ctx context.Context, key, value string) error {
	return q.SetSettingBy(ctx, key, value, "system", time.Now())
}

// SetSettingBy records who changed the setting and when.
func (q Q) SetSettingBy(ctx context.Context, key, value, by string, at time.Time) error {
	_, err := q.x.ExecContext(ctx, `INSERT INTO settings (key, value, updated_by, updated_at) VALUES (?, ?, ?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_by = excluded.updated_by, updated_at = excluded.updated_at`,
		key, value, by, at.Unix())
	return err
}

// SettingAt is when a setting was last written (0 when never).
func (q Q) SettingAt(ctx context.Context, key string) int64 {
	var at int64
	q.x.QueryRowContext(ctx, "SELECT updated_at FROM settings WHERE key = ?", key).Scan(&at)
	return at
}

// Test is the last test of one connector, kept so the page shows it after
// a reload.
type Test struct {
	At     time.Time
	OK     bool
	Result string
}

func (q Q) SaveTest(ctx context.Context, connector string, ok bool, result string, at time.Time) error {
	n := 0
	if ok {
		n = 1
	}
	_, err := q.x.ExecContext(ctx, `INSERT INTO tests (connector, at, ok, result) VALUES (?, ?, ?, ?)
		ON CONFLICT(connector) DO UPDATE SET at = excluded.at, ok = excluded.ok, result = excluded.result`,
		connector, at.Unix(), n, result)
	return err
}

// LastTest returns the connector's last test (ok false when none yet).
func (q Q) LastTest(ctx context.Context, connector string) (Test, bool) {
	var at, ok int64
	var res string
	err := q.x.QueryRowContext(ctx, "SELECT at, ok, result FROM tests WHERE connector = ?", connector).Scan(&at, &ok, &res)
	if err != nil {
		return Test{}, false
	}
	return Test{At: time.Unix(at, 0), OK: ok == 1, Result: res}, true
}

// ForgetTest drops a connector's last test (after Disconnect).
func (q Q) ForgetTest(ctx context.Context, connector string) error {
	_, err := q.x.ExecContext(ctx, "DELETE FROM tests WHERE connector = ?", connector)
	return err
}

// Number shows an item number the way people read it: Q-0001.
func Number(n int64) string { return fmt.Sprintf("Q-%04d", n) }

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

// RepairNames rewrites sender and people names that were stored as undecoded
// mail header words (they start with "=?"). decode is the mail parser's own
// decoder; a name it leaves unchanged is not written. It returns how many
// names it changed.
func (s *Store) RepairNames(ctx context.Context, decode func(string) string) (int, error) {
	changed := 0
	err := s.Update(ctx, func(tx *Tx) error {
		for _, t := range []struct{ table, col string }{{"messages", "sender_name"}, {"people", "name"}} {
			rows, err := tx.x.QueryContext(ctx, "SELECT DISTINCT "+t.col+" FROM "+t.table+" WHERE "+t.col+" LIKE '%=?%'")
			if err != nil {
				return err
			}
			var old []string
			for rows.Next() {
				var v string
				if err := rows.Scan(&v); err != nil {
					rows.Close()
					return err
				}
				old = append(old, v)
			}
			rows.Close()
			if err := rows.Err(); err != nil {
				return err
			}
			for _, v := range old {
				nv := decode(v)
				if nv == v || nv == "" {
					continue
				}
				res, err := tx.x.ExecContext(ctx, "UPDATE "+t.table+" SET "+t.col+"=? WHERE "+t.col+"=?", nv, v)
				if err != nil {
					return err
				}
				n, _ := res.RowsAffected()
				changed += int(n)
			}
		}
		return nil
	})
	return changed, err
}

// ErrCheckTimeout says the integrity check did not finish in the time it was
// given. That is not a failed check: nothing is known.
var ErrCheckTimeout = errors.New("the database check did not finish in time")

// QuickCheck runs SQLite's PRAGMA quick_check on a connection of its own (a
// second, read-only handle on the file), so it never takes one of the
// app's connections nor blocks a writer (the database is in WAL mode). It is
// stopped when ctx ends. It returns nil when the database is sound, an error
// holding the first problems SQLite names when it is not, and ErrCheckTimeout
// when ctx ran out first.
func (s *Store) QuickCheck(ctx context.Context) error {
	db, err := sql.Open("sqlite3", "file:"+s.path+"?mode=ro&_busy_timeout=5000")
	if err != nil {
		return err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	rows, err := db.QueryContext(ctx, "PRAGMA quick_check")
	if err != nil {
		if ctx.Err() != nil {
			return ErrCheckTimeout
		}
		return fmt.Errorf("quick_check: %w", err)
	}
	defer rows.Close()
	var problems []string
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			return err
		}
		if line != "ok" && len(problems) < 5 {
			problems = append(problems, line)
		}
	}
	if err := rows.Err(); err != nil {
		if ctx.Err() != nil {
			return ErrCheckTimeout
		}
		return fmt.Errorf("quick_check: %w", err)
	}
	if len(problems) > 0 {
		return fmt.Errorf("quick_check found: %s", strings.Join(problems, "; "))
	}
	return nil
}
