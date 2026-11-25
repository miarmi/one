package repo

import (
	"context"
	"errors"
	"regexp"
	"sort"
	"strings"
)

// Test runs (SPEC 16, 30). The browser tests run against a preview that holds
// a copy of the owner's real data, and that data is later moved to production, so
// whatever a test makes must be removable completely, and nothing else may
// ever be removed. A test names its run (e2e- and 8 to 40 lower-case letters
// and digits) when it makes something; the server marks every record created
// under that name in a column of its own (test_run), and whatever is later
// attached to a marked record carries its mark. Removing a run is the only
// deletion in the app; every DELETE of it names the mark in its own WHERE.

// RunPattern is the one shape a run name can have.
const RunPattern = `^e2e-[a-z0-9]{8,40}$`

var runRE = regexp.MustCompile(RunPattern)

// ValidRun says whether a string is a run name.
func ValidRun(s string) bool { return runRE.MatchString(s) }

type runCtxKey struct{}

// WithRun says that what is written under ctx belongs to a test run. The name
// must be valid: anything else is ignored here, so the caller checks it first.
func WithRun(ctx context.Context, run string) context.Context {
	if !ValidRun(run) {
		return ctx
	}
	return context.WithValue(ctx, runCtxKey{}, run)
}

// RunOf is the run a context writes for ("" when none).
func RunOf(ctx context.Context) string {
	s, _ := ctx.Value(runCtxKey{}).(string)
	return s
}

// RunError says that a call under a run would have written to a record that
// is not part of that run (a real one, or another run's). Nothing is written.
type RunError struct{}

func (*RunError) Error() string {
	return "This call names a test run, and that record is not part of it, so nothing was written."
}

// IsRunError tells whether err is a RunError.
func IsRunError(err error) bool {
	var re *RunError
	return errors.As(err, &re)
}

// RunTables are the tables whose rows carry a mark.
var RunTables = []string{"raw", "items", "item_links", "messages", "events", "files", "rules", "skipped", "proposals", "chat_out", "people",
	"person_handles", "person_pictures", "notes", "agenda", "agenda_versions", "agenda_proposals", "person_proposals", "mail_history", "chats", "skip_rules", "matter_proposals", "setting_proposals", "pool_marks"}

// markTables are the tables a single row can be looked up in for its mark.
var markTables = map[string]bool{"items": true, "people": true, "notes": true, "rules": true, "skip_rules": true, "matter_proposals": true, "setting_proposals": true, "agenda": true, "agenda_proposals": true, "person_proposals": true, "proposals": true}

// RowRun is the mark of one row ("" for a real one; found is false when the
// row does not exist).
func (q Q) RowRun(ctx context.Context, table string, id int64) (run string, found bool) {
	if !markTables[table] {
		return "", false
	}
	err := q.x.QueryRowContext(ctx, `SELECT test_run FROM `+table+` WHERE id=?`, id).Scan(&run)
	return run, err == nil
}

// CheckRow refuses (RunError) a call under a run that names a row which is not
// part of that run. A call under no run may name any row.
func (q Q) CheckRow(ctx context.Context, table string, id int64) error {
	want := RunOf(ctx)
	if want == "" {
		return nil
	}
	got, found := q.RowRun(ctx, table, id)
	if !found || got != want {
		return &RunError{}
	}
	return nil
}

// runForItem is the mark for a record attached to an item: the item's own
// mark, whoever makes the record. A call under a run may attach only to an
// item of that run.
func (q Q) runForItem(ctx context.Context, itemID int64) (string, error) {
	want := RunOf(ctx)
	if itemID == 0 {
		return want, nil
	}
	got, found := q.RowRun(ctx, "items", itemID)
	if !found {
		return want, nil // the foreign key refuses what is not there
	}
	if want != "" && got != want {
		return "", &RunError{}
	}
	return got, nil
}

// runForRow is the same for a record attached to a person, rule, note or entry.
func (q Q) runForRow(ctx context.Context, table string, id int64) (string, error) {
	want := RunOf(ctx)
	if id == 0 {
		return want, nil
	}
	got, found := q.RowRun(ctx, table, id)
	if !found {
		return want, nil
	}
	if want != "" && got != want {
		return "", &RunError{}
	}
	return got, nil
}

// InsertEventFor writes an event that belongs to no item but to a row (a
// person, a rule, a note, a proposal about people): it carries that row's mark.
func (q Q) InsertEventFor(ctx context.Context, table string, rowID int64, at int64, actor, typ string, data any) error {
	run, err := q.runForRow(ctx, table, rowID)
	if err != nil {
		return err
	}
	return q.insertEvent(ctx, run, at, 0, 0, actor, typ, data)
}

// --- pool files ---

// PoolUsed records that a file of the pool is held by a record of a run. Only
// a file that this run made (created) or that another run made is marked; a
// file that was already in the pool for any other reason is never marked, so
// removing the run can never take it.
func (q Q) PoolUsed(ctx context.Context, path string, created bool, itemID int64) error {
	run, err := q.runForItem(ctx, itemID)
	if err != nil || run == "" || path == "" {
		return err
	}
	if !created {
		var n int
		if err := q.x.QueryRowContext(ctx, `SELECT COUNT(*) FROM pool_marks WHERE path=?`, path).Scan(&n); err != nil || n == 0 {
			return err
		}
	}
	_, err = q.x.ExecContext(ctx, `INSERT INTO pool_marks (path, test_run) VALUES (?, ?) ON CONFLICT DO NOTHING`, path, run)
	return err
}

// --- listing and removing ---

// RunCounts says how many records of each kind a run holds or took away.
type RunCounts map[string]int

// runKinds names the tables in the words of the answer, in the order records
// are removed: children before parents, so no foreign key ever has to be
// switched off.
var runKinds = []struct{ table, kind string }{
	{"events", "events"}, {"files", "files"}, {"chat_out", "outbox"}, {"proposals", "proposals"},
	{"agenda_versions", "agenda_versions"}, {"agenda_proposals", "agenda_proposals"}, {"agenda", "agenda"}, {"notes", "notes"}, {"person_pictures", "pictures"},
	{"person_handles", "handles"}, {"person_proposals", "person_proposals"}, {"messages", "messages"}, {"raw", "raw"},
	{"item_links", "links"}, {"chats", "chats"}, {"skipped", "skipped"}, {"items", "items"}, {"people", "people"},
	{"rules", "rules"}, {"skip_rules", "skip_rules"}, {"matter_proposals", "matter_proposals"}, {"setting_proposals", "setting_proposals"}, {"mail_history", "history"}, {"pool_marks", "pool_marks"},
}

// Kinds lists the kinds of records an answer names, with search_hits and
// pool_files, which are not tables of marked rows.
func Kinds() []string {
	out := []string{}
	for _, k := range runKinds {
		if k.table != "pool_marks" {
			out = append(out, k.kind)
		}
	}
	return append(out, "search_hits", "pool_files")
}

// RunInfo is one run present in the data.
type RunInfo struct {
	Run     string
	FirstAt int64 // the time of its first record (0 when none carries a time)
	Counts  RunCounts
	Total   int
}

// timeColumns is, for the tables that have one, the column that says when a
// record was made.
var timeColumns = map[string]string{"events": "at", "raw": "ingested_at", "notes": "created_at", "rules": "created_at", "skip_rules": "proposed_at", "matter_proposals": "proposed_at", "setting_proposals": "proposed_at", "agenda": "created_at",
	"chat_out": "created_at", "proposals": "proposed_at", "files": "added_at", "chats": "created_at", "skipped": "at", "person_proposals": "proposed_at", "agenda_proposals": "proposed_at",
	"mail_history": "recorded_at"}

// ListRuns lists the runs that have records, oldest first.
func (q Q) ListRuns(ctx context.Context) ([]RunInfo, error) {
	by := map[string]*RunInfo{}
	for _, k := range runKinds {
		if k.table == "pool_marks" {
			continue
		}
		sel := `SELECT test_run, COUNT(*), 0 FROM ` + k.table + ` WHERE test_run <> '' GROUP BY test_run`
		if col, ok := timeColumns[k.table]; ok {
			sel = `SELECT test_run, COUNT(*), MIN(` + col + `) FROM ` + k.table + ` WHERE test_run <> '' GROUP BY test_run`
		}
		rows, err := q.x.QueryContext(ctx, sel)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var run string
			var n int
			var at int64
			if err := rows.Scan(&run, &n, &at); err != nil {
				rows.Close()
				return nil, err
			}
			ri := by[run]
			if ri == nil {
				ri = &RunInfo{Run: run, Counts: RunCounts{}}
				by[run] = ri
			}
			ri.Counts[k.kind] += n
			ri.Total += n
			if at > 0 && (ri.FirstAt == 0 || at < ri.FirstAt) {
				ri.FirstAt = at
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}
	out := make([]RunInfo, 0, len(by))
	for _, ri := range by {
		out = append(out, *ri)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].FirstAt != out[j].FirstAt {
			return out[i].FirstAt < out[j].FirstAt
		}
		return out[i].Run < out[j].Run
	})
	return out, nil
}

// RemoveRun deletes, in this transaction, every record that carries the mark
// of the run and nothing else. Every statement names the mark in its own
// WHERE; nothing is deleted by a list of ids, and nothing cascades. It returns
// how many records of each kind went, and the files of the pool that no record
// holds any more and that only this run ever made (the caller removes those
// from the disk after the transaction has been committed).
func (tx *Tx) RemoveRun(ctx context.Context, run string) (RunCounts, []string, error) {
	counts := RunCounts{}
	for _, k := range Kinds() {
		counts[k] = 0
	}
	if !ValidRun(run) {
		return counts, nil, errors.New("not a run name")
	}
	var marked []string
	rows, err := tx.x.QueryContext(ctx, `SELECT path FROM pool_marks WHERE test_run <> '' AND test_run = ?`, run)
	if err != nil {
		return counts, nil, err
	}
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			rows.Close()
			return counts, nil, err
		}
		marked = append(marked, p)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return counts, nil, err
	}
	var hitsBefore int
	tx.x.QueryRowContext(ctx, `SELECT COUNT(*) FROM finder`).Scan(&hitsBefore)
	for _, k := range runKinds {
		res, err := tx.x.ExecContext(ctx, `DELETE FROM `+k.table+` WHERE test_run <> '' AND test_run = ?`, run)
		if err != nil {
			return counts, nil, err
		}
		n, _ := res.RowsAffected()
		if k.table != "pool_marks" {
			counts[k.kind] += int(n)
		}
	}
	var hitsAfter int
	tx.x.QueryRowContext(ctx, `SELECT COUNT(*) FROM finder`).Scan(&hitsAfter)
	counts["search_hits"] = max(hitsBefore-hitsAfter, 0)
	var gone []string
	for _, p := range marked {
		var held int
		sha := p[strings.LastIndex(p, "/")+1:]
		if err := tx.x.QueryRowContext(ctx, `SELECT (SELECT COUNT(*) FROM files WHERE path=?) + (SELECT COUNT(*) FROM chat_out WHERE path=?)
			+ (SELECT COUNT(*) FROM raw WHERE blob_sha256=?) + (SELECT COUNT(*) FROM pool_marks WHERE path=?)`, p, p, sha, p).Scan(&held); err != nil {
			return counts, nil, err
		}
		if held == 0 {
			gone = append(gone, p)
		}
	}
	counts["pool_files"] = len(gone)
	return counts, gone, nil
}

// ensureTestRuns gives every table that can hold a test's records its mark
// column (empty for everything real) and makes the two append-only tables
// refuse deleting any row that does not carry a mark, in the trigger itself.
func ensureTestRuns(db schemaDB) error {
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS pool_marks (
		path TEXT NOT NULL,
		test_run TEXT NOT NULL,
		PRIMARY KEY (path, test_run)
	)`); err != nil {
		return err
	}
	for _, t := range RunTables {
		if t == "pool_marks" || t == "skip_rules" || t == "matter_proposals" || t == "setting_proposals" {
			continue // these are made later, with their mark column, in ensureClaude
		}
		have, err := columns(db, t)
		if err != nil {
			return err
		}
		if !have["test_run"] {
			if _, err := db.Exec("ALTER TABLE " + t + " ADD COLUMN test_run TEXT NOT NULL DEFAULT ''"); err != nil {
				return err
			}
		}
		if _, err := db.Exec("CREATE INDEX IF NOT EXISTS " + t + "_test_run ON " + t + " (test_run) WHERE test_run <> ''"); err != nil {
			return err
		}
	}
	// Raw rows and events stay append-only: only a row that carries a run's
	// mark may be deleted, and only by the removal of that run.
	for _, t := range []struct{ table, name, what string }{{"raw", "raw_no_delete", "raw rows are append-only"}, {"events", "events_no_delete", "events are append-only"}} {
		if _, err := db.Exec("DROP TRIGGER IF EXISTS " + t.name); err != nil {
			return err
		}
		if _, err := db.Exec("CREATE TRIGGER " + t.name + " BEFORE DELETE ON " + t.table + " WHEN old.test_run NOT GLOB 'e2e-*' BEGIN SELECT RAISE(ABORT, '" + t.what + "'); END"); err != nil {
			return err
		}
	}
	return nil
}
