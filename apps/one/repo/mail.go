package repo

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
)

// --- sync runs ---

// Run is one sync or import of one source.
type Run struct {
	ID         int64
	Source     string
	Mode       string
	Status     string // running, done, failed, interrupted
	StartedAt  int64
	FinishedAt int64
	Fetched    int
	Stored     int
	Skipped    int
	ItemsMade  int
	FilesSaved int
	Bytes      int64
	Error      string
	Cursor     string
	// Failed counts the records of this run that were left out because each
	// failed on its own; it is not stored, only logged when the run ends.
	Failed int
}

func (q Q) StartRun(ctx context.Context, source, mode string, now int64) (int64, error) {
	res, err := q.x.ExecContext(ctx, "INSERT INTO sync_runs (source, mode, started_at) VALUES (?, ?, ?)", source, mode, now)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// SaveRun writes the counters and the position of a running run.
func (q Q) SaveRun(ctx context.Context, r Run) error {
	_, err := q.x.ExecContext(ctx, `UPDATE sync_runs SET fetched=?, stored=?, skipped=?, items_made=?, files_saved=?, bytes=?, cursor=? WHERE id=?`,
		r.Fetched, r.Stored, r.Skipped, r.ItemsMade, r.FilesSaved, r.Bytes, r.Cursor, r.ID)
	return err
}

// FinishRun ends a run as done or failed, with the error in plain words.
func (q Q) FinishRun(ctx context.Context, r Run, status, errText string, now int64) error {
	_, err := q.x.ExecContext(ctx, `UPDATE sync_runs SET status=?, error=?, finished_at=?, fetched=?, stored=?, skipped=?, items_made=?, files_saved=?, bytes=?, cursor=? WHERE id=?`,
		status, errText, now, r.Fetched, r.Stored, r.Skipped, r.ItemsMade, r.FilesSaved, r.Bytes, r.Cursor, r.ID)
	return err
}

// CloseStaleRuns marks runs left "running" by a stop or crash.
func (q Q) CloseStaleRuns(ctx context.Context, now int64) error {
	_, err := q.x.ExecContext(ctx, `UPDATE sync_runs SET status='interrupted', finished_at=?, error='Stopped before it finished; it continues where it left off.' WHERE status='running'`, now)
	return err
}

const runCols = `id, source, mode, status, started_at, COALESCE(finished_at,0), fetched, stored, skipped, items_made, files_saved, bytes, error, cursor`

func scanRun(row interface{ Scan(...any) error }) (Run, error) {
	var r Run
	err := row.Scan(&r.ID, &r.Source, &r.Mode, &r.Status, &r.StartedAt, &r.FinishedAt, &r.Fetched, &r.Stored, &r.Skipped, &r.ItemsMade, &r.FilesSaved, &r.Bytes, &r.Error, &r.Cursor)
	return r, err
}

// LatestRun returns the newest run of the source whose mode is in modes.
func (q Q) LatestRun(ctx context.Context, source string, modes ...string) (Run, bool) {
	args := []any{source}
	in := make([]string, len(modes))
	for i, m := range modes {
		in[i] = "?"
		args = append(args, m)
	}
	r, err := scanRun(q.x.QueryRowContext(ctx, `SELECT `+runCols+` FROM sync_runs WHERE source=? AND mode IN (`+strings.Join(in, ",")+`) ORDER BY id DESC LIMIT 1`, args...))
	return r, err == nil
}

// LastFinishedRun returns the newest finished run of the source and mode that was not a failure.
func (q Q) LastFinishedRun(ctx context.Context, source, mode string) (Run, bool) {
	r, err := scanRun(q.x.QueryRowContext(ctx, `SELECT `+runCols+` FROM sync_runs WHERE source=? AND mode=? AND status='done' ORDER BY id DESC LIMIT 1`, source, mode))
	return r, err == nil
}

// RecentRuns lists the newest runs of a source that ended as done or failed,
// newest first.
func (q Q) RecentRuns(ctx context.Context, source string, limit int) ([]Run, error) {
	rows, err := q.x.QueryContext(ctx, `SELECT `+runCols+` FROM sync_runs WHERE source=? AND status IN ('done', 'failed') ORDER BY id DESC LIMIT ?`, source, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Run
	for rows.Next() {
		r, err := scanRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// LastRunTime is when the source last finished a fetch without failing; when
// it never did, when its first fetch began (0 when it never ran).
func (q Q) LastRunTime(ctx context.Context, source string) int64 {
	var t sql.NullInt64
	q.x.QueryRowContext(ctx, `SELECT COALESCE((SELECT MAX(finished_at) FROM sync_runs WHERE source=? AND status='done'), (SELECT MIN(started_at) FROM sync_runs WHERE source=?))`, source, source).Scan(&t)
	return t.Int64
}

// --- raw ---

// Raw is one stored version of a message as the source delivered it.
type Raw struct {
	ID         int64
	Source     string
	SourceID   string
	VersionKey string
	ThreadKey  string
	ReceivedAt int64
	IngestedAt int64
	RunID      int64
	BlobSHA    string
	Payload    string
	// TestRun is the mark of a record made by a test run ("" for everything real).
	// A new version of a message carries the mark of the version before it.
	TestRun string
}

// InsertRaw stores one version. inserted is false when that version is
// already there; the id is then 0 and the caller must stop for this record.
// It never asks the database for "the last inserted id" after an ignored
// insert: that id belongs to some other row.
func (q Q) InsertRaw(ctx context.Context, r Raw) (id int64, inserted bool, err error) {
	var run any
	if r.RunID != 0 {
		run = r.RunID
	}
	mark := r.TestRun
	if want := RunOf(ctx); mark == "" {
		mark = want
	} else if want != "" && want != mark {
		return 0, false, &RunError{}
	}
	res, err := q.x.ExecContext(ctx, `INSERT INTO raw (source, source_id, version_key, thread_key, received_at, ingested_at, sync_run_id, blob_sha256, payload, test_run)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT (source, source_id, version_key) DO NOTHING`,
		r.Source, r.SourceID, r.VersionKey, r.ThreadKey, r.ReceivedAt, r.IngestedAt, run, r.BlobSHA, r.Payload, mark)
	if err != nil {
		return 0, false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, false, err
	}
	if n == 0 {
		return 0, false, nil
	}
	id, err = res.LastInsertId()
	return id, err == nil, err
}

// LatestRaw returns the newest stored version of a source's message.
func (q Q) LatestRaw(ctx context.Context, source, sourceID string) (Raw, bool) {
	var r Raw
	var run sql.NullInt64
	err := q.x.QueryRowContext(ctx, `SELECT id, source, source_id, version_key, thread_key, received_at, ingested_at, sync_run_id, blob_sha256, payload, test_run
		FROM raw WHERE source=? AND source_id=? ORDER BY id DESC LIMIT 1`, source, sourceID).
		Scan(&r.ID, &r.Source, &r.SourceID, &r.VersionKey, &r.ThreadKey, &r.ReceivedAt, &r.IngestedAt, &run, &r.BlobSHA, &r.Payload, &r.TestRun)
	r.RunID = run.Int64
	return r, err == nil
}

// Count of the rows in one of the tables below; used by tests and the page.
func (q Q) count(ctx context.Context, table string) int {
	var n int
	q.x.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+table).Scan(&n)
	return n
}

// CountRows counts the rows of a known table.
func (q Q) CountRows(ctx context.Context, table string) int {
	switch table {
	case "raw", "messages", "items", "events", "files", "chat_out", "skipped", "people", "person_handles", "item_links", "rules", "sync_runs", "agenda", "agenda_versions", "mail_history":
		return q.count(ctx, table)
	}
	return -1
}

// --- items ---

// Item is one matter.
type Item struct {
	ID           int64
	Number       int64
	Kind         string
	Subject      string
	Stage        string // new, in_progress, pending, resolved, closed, history
	NeedsClaude  bool
	PendingSince int64
	FirstAt      int64
	LastAt       int64
	MovedAt      int64 // when the item last took its place in its group; the order of a group
	Hidden       bool  // a new item waits for Claude's first look and is not in the owner's list (SPEC 11)
	// ChatSince is, for a chat that is still being written in (SPEC 17), the time
	// of the last writing; the chat goes to Claude once it is older than the chat
	// quiet time. 0: the chat is not live.
	ChatSince int64
	// TestRun is the mark of an item a test run made ("" for every real one).
	TestRun string
}

const itemCols = `id, number, kind, subject, stage, needs_claude, COALESCE(pending_since,0), first_message_at, last_message_at, moved_at, hidden, chat_since, test_run`

func scanItem(row interface{ Scan(...any) error }) (Item, error) {
	var it Item
	var nc, hid int
	err := row.Scan(&it.ID, &it.Number, &it.Kind, &it.Subject, &it.Stage, &nc, &it.PendingSince, &it.FirstAt, &it.LastAt, &it.MovedAt, &hid, &it.ChatSince, &it.TestRun)
	it.NeedsClaude, it.Hidden = nc == 1, hid == 1
	return it, err
}

func (q Q) ItemByLink(ctx context.Context, kind, value string) (Item, bool) {
	it, err := scanItem(q.x.QueryRowContext(ctx, `SELECT `+prefixed("i.", itemCols)+` FROM items i JOIN item_links l ON l.item_id = i.id WHERE l.kind=? AND l.value=?`, kind, value))
	return it, err == nil
}

func (q Q) ItemByID(ctx context.Context, id int64) (Item, bool) {
	it, err := scanItem(q.x.QueryRowContext(ctx, `SELECT `+itemCols+` FROM items WHERE id=?`, id))
	return it, err == nil
}

func prefixed(p, cols string) string {
	parts := strings.Split(cols, ", ")
	for i, c := range parts {
		if strings.HasPrefix(c, "COALESCE(") {
			parts[i] = strings.Replace(c, "COALESCE(", "COALESCE("+p, 1)
		} else {
			parts[i] = p + c
		}
	}
	return strings.Join(parts, ", ")
}

// CreateItem adds an item with the next number and links it by (kind, value).
func (q Q) CreateItem(ctx context.Context, it Item, linkKind, linkValue, by string, now int64) (Item, error) {
	it, err := q.CreateItemOnly(ctx, it)
	if err != nil {
		return it, err
	}
	_, err = q.x.ExecContext(ctx, `INSERT INTO item_links (item_id, kind, value, method, linked_by, linked_at, test_run) VALUES (?, ?, ?, 'auto', ?, ?, ?)`,
		it.ID, linkKind, linkValue, by, now, it.TestRun)
	return it, err
}

// CreateItemOnly adds an item with the next number and no link.
func (q Q) CreateItemOnly(ctx context.Context, it Item) (Item, error) {
	var next int64
	if err := q.x.QueryRowContext(ctx, "SELECT COALESCE(MAX(number), 0) + 1 FROM items").Scan(&next); err != nil {
		return it, err
	}
	var pend any
	if it.PendingSince != 0 {
		pend = it.PendingSince
	}
	if it.MovedAt == 0 {
		it.MovedAt = it.LastAt
	}
	it.TestRun = RunOf(ctx)
	res, err := q.x.ExecContext(ctx, `INSERT INTO items (number, kind, subject, stage, needs_claude, pending_since, first_message_at, last_message_at, moved_at, hidden, chat_since, test_run)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, next, it.Kind, it.Subject, it.Stage, b2i(it.NeedsClaude), pend, it.FirstAt, it.LastAt, it.MovedAt, b2i(it.Hidden), it.ChatSince, it.TestRun)
	if err != nil {
		return it, err
	}
	it.ID, err = res.LastInsertId()
	if err != nil {
		return it, err
	}
	it.Number = next
	return it, nil
}

// AddLink links one more (kind, value) to an item; a link that exists stays.
func (q Q) AddLink(ctx context.Context, itemID int64, kind, value, by string, now int64) error {
	_, err := q.x.ExecContext(ctx, `INSERT OR IGNORE INTO item_links (item_id, kind, value, method, linked_by, linked_at, test_run) VALUES (?, ?, ?, 'auto', ?, ?, ?)`,
		itemID, kind, value, by, now, RunOf(ctx))
	return err
}

// SaveItem writes the parts of an item that the import changes.
func (q Q) SaveItem(ctx context.Context, it Item) error {
	if err := q.CheckRow(ctx, "items", it.ID); err != nil {
		return err
	}
	var pend any
	if it.PendingSince != 0 {
		pend = it.PendingSince
	}
	_, err := q.x.ExecContext(ctx, `UPDATE items SET subject=?, stage=?, needs_claude=?, pending_since=?, first_message_at=?, last_message_at=?, moved_at=?, hidden=?, chat_since=? WHERE id=?`,
		it.Subject, it.Stage, b2i(it.NeedsClaude), pend, it.FirstAt, it.LastAt, it.MovedAt, b2i(it.Hidden), it.ChatSince, it.ID)
	return err
}

// --- messages ---

// Message is the normalized view of a raw record.
type Message struct {
	ID             int64
	ItemID         int64
	RawID          int64
	Source         string
	SourceID       string
	ThreadKey      string
	Network        string
	Sender         string // address, lower case
	SenderName     string
	Subject        string
	To, Cc         string
	Addressed      bool
	HasAttachments bool
	Text           string
	SentByMe       bool
	ReceivedAt     int64
	Labels         []string
	Flags          string
	Seen, Starred  bool
	HeaderID       string
	Edited         bool // a chat message whose text was edited after it was first seen (SPEC 4)
}

// InsertMessage adds a message; inserted is false when the source's message
// is already there.
func (q Q) InsertMessage(ctx context.Context, m Message) (id int64, inserted bool, err error) {
	labels, _ := json.Marshal(m.Labels)
	mark, err := q.runForItem(ctx, m.ItemID)
	if err != nil {
		return 0, false, err
	}
	res, err := q.x.ExecContext(ctx, `INSERT INTO messages (item_id, raw_id, source, source_id, thread_key, network, sender, sender_name, subject, to_addrs, cc_addrs,
		addressed, has_attachments, text, sent_by_me, received_at, labels, flags, seen, starred, header_id, edited, test_run)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT (source, source_id) DO NOTHING`,
		m.ItemID, m.RawID, m.Source, m.SourceID, m.ThreadKey, m.Network, m.Sender, m.SenderName, m.Subject, m.To, m.Cc,
		b2i(m.Addressed), b2i(m.HasAttachments), m.Text, b2i(m.SentByMe), m.ReceivedAt, string(labels), m.Flags, b2i(m.Seen), b2i(m.Starred), m.HeaderID, b2i(m.Edited), mark)
	if err != nil {
		return 0, false, err
	}
	n, err := res.RowsAffected()
	if err != nil || n == 0 {
		return 0, false, err
	}
	id, err = res.LastInsertId()
	return id, err == nil, err
}

const messageCols = `id, item_id, raw_id, source, source_id, thread_key, network, sender, sender_name, subject, to_addrs, cc_addrs,
	addressed, has_attachments, text, sent_by_me, received_at, labels, flags, seen, starred, header_id, edited`

func scanMessage(row interface{ Scan(...any) error }) (Message, error) {
	var m Message
	var labels string
	var addr, att, mine, seen, star, edited int
	err := row.Scan(&m.ID, &m.ItemID, &m.RawID, &m.Source, &m.SourceID, &m.ThreadKey, &m.Network, &m.Sender, &m.SenderName, &m.Subject, &m.To, &m.Cc,
		&addr, &att, &m.Text, &mine, &m.ReceivedAt, &labels, &m.Flags, &seen, &star, &m.HeaderID, &edited)
	if err != nil {
		return m, err
	}
	m.Addressed, m.HasAttachments, m.SentByMe, m.Seen, m.Starred, m.Edited = addr == 1, att == 1, mine == 1, seen == 1, star == 1, edited == 1
	json.Unmarshal([]byte(labels), &m.Labels)
	return m, nil
}

// MessageBySource finds a message by the source's own id.
func (q Q) MessageBySource(ctx context.Context, source, sourceID string) (Message, bool) {
	m, err := scanMessage(q.x.QueryRowContext(ctx, `SELECT `+messageCols+` FROM messages WHERE source=? AND source_id=?`, source, sourceID))
	return m, err == nil
}

// UpdateMessageState follows a changed read state, label, star or folder
// onto the one message, pointing it at the newest raw version.
func (q Q) UpdateMessageState(ctx context.Context, id, rawID int64, labels []string, flags string, seen, starred bool) error {
	l, _ := json.Marshal(labels)
	_, err := q.x.ExecContext(ctx, `UPDATE messages SET raw_id=?, labels=?, flags=?, seen=?, starred=? WHERE id=?`, rawID, string(l), flags, b2i(seen), b2i(starred), id)
	return err
}

// --- events ---

func (q Q) InsertEvent(ctx context.Context, at int64, itemID int64, actor, typ string, data any) error {
	run, err := q.runForItem(ctx, itemID)
	if err != nil {
		return err
	}
	return q.insertEvent(ctx, run, at, itemID, 0, actor, typ, data)
}

func (q Q) insertEvent(ctx context.Context, run string, at int64, itemID, agendaID int64, actor, typ string, data any) error {
	d := ""
	if data != nil {
		b, err := json.Marshal(data)
		if err != nil {
			return err
		}
		d = string(b)
	}
	var item, ag any
	if itemID != 0 {
		item = itemID
	}
	if agendaID != 0 {
		ag = agendaID
	}
	_, err := q.x.ExecContext(ctx, `INSERT INTO events (at, item_id, agenda_id, actor, type, data, test_run) VALUES (?, ?, ?, ?, ?, ?, ?)`, at, item, ag, actor, typ, d, run)
	return err
}

// InsertAgendaEvent writes an event of the record that belongs to one agenda
// entry (and to an item, when it has one).
func (q Q) InsertAgendaEvent(ctx context.Context, at int64, itemID, agendaID int64, actor, typ string, data any) error {
	run, err := q.runForItem(ctx, itemID)
	if err != nil {
		return err
	}
	if itemID == 0 {
		if run, err = q.runForRow(ctx, "agenda", agendaID); err != nil {
			return err
		}
	}
	return q.insertEvent(ctx, run, at, itemID, agendaID, actor, typ, data)
}

// EventTypes lists the types of an item's events, oldest first.
func (q Q) EventTypes(ctx context.Context, itemID int64) []string {
	rows, err := q.x.QueryContext(ctx, `SELECT type FROM events WHERE item_id=? ORDER BY id`, itemID)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var t string
		if rows.Scan(&t) == nil {
			out = append(out, t)
		}
	}
	return out
}

// --- files ---

// File is one file of the pool, as one message or event holds it.
type File struct {
	ID          int64
	ItemID      int64
	Source      string
	MessageID   int64
	Part        string
	Filename    string
	ContentType string
	Disposition string // attachment or inline
	ContentID   string
	Size        int64
	SHA         string
	Path        string // relative to the pool
	Status      string // pending, saved, failed
	Error       string
	AddedAt     int64
}

func (q Q) InsertFile(ctx context.Context, f File) (int64, error) {
	mark, err := q.runForItem(ctx, f.ItemID)
	if err != nil {
		return 0, err
	}
	res, err := q.x.ExecContext(ctx, `INSERT INTO files (item_id, source, message_id, part, filename, content_type, disposition, content_id, size, sha256, path, status, error, added_at, test_run)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		f.ItemID, f.Source, f.MessageID, f.Part, f.Filename, f.ContentType, f.Disposition, f.ContentID, f.Size, f.SHA, f.Path, f.Status, f.Error, f.AddedAt, mark)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// MarkFileSaved records where a pending file ended up.
func (q Q) MarkFileSaved(ctx context.Context, id, size int64, sha, path string) error {
	_, err := q.x.ExecContext(ctx, `UPDATE files SET size=?, sha256=?, path=?, status='saved', error='' WHERE id=?`, size, sha, path, id)
	return err
}

// MarkFileFailed keeps a file pending but remembers why the last try failed.
func (q Q) MarkFileFailed(ctx context.Context, id int64, why string) error {
	_, err := q.x.ExecContext(ctx, `UPDATE files SET error=? WHERE id=?`, why, id)
	return err
}

// MessagesWithPendingFiles lists up to limit message ids that still have files
// to save out of their stored original. A chat message has no stored original
// (the bridge brings its files), so it is never listed here.
func (q Q) MessagesWithPendingFiles(ctx context.Context, limit int) ([]int64, error) {
	rows, err := q.x.QueryContext(ctx, `SELECT DISTINCT f.message_id FROM files f JOIN messages m ON m.id = f.message_id JOIN raw r ON r.id = m.raw_id
		WHERE f.status='pending' AND r.blob_sha256 <> '' ORDER BY f.message_id LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// FilesOfMessage lists a message's files in part order.
func (q Q) FilesOfMessage(ctx context.Context, messageID int64) ([]File, error) {
	rows, err := q.x.QueryContext(ctx, `SELECT id, item_id, source, message_id, part, filename, content_type, disposition, content_id, size, sha256, path, status, error, added_at
		FROM files WHERE message_id=? ORDER BY id`, messageID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []File
	for rows.Next() {
		var f File
		if err := rows.Scan(&f.ID, &f.ItemID, &f.Source, &f.MessageID, &f.Part, &f.Filename, &f.ContentType, &f.Disposition, &f.ContentID, &f.Size, &f.SHA, &f.Path, &f.Status, &f.Error, &f.AddedAt); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// MessageRaw returns the blob hash of the message's newest raw version.
func (q Q) MessageBlob(ctx context.Context, messageID int64) (string, error) {
	var sha string
	err := q.x.QueryRowContext(ctx, `SELECT r.blob_sha256 FROM messages m JOIN raw r ON r.id = m.raw_id WHERE m.id=?`, messageID).Scan(&sha)
	return sha, err
}

func (q Q) PendingFileCount(ctx context.Context) int {
	var n int
	q.x.QueryRowContext(ctx, "SELECT COUNT(*) FROM files WHERE status='pending'").Scan(&n)
	return n
}

// --- rules and skipped ---

// Rule decides what is not imported at all.
type Rule struct {
	ID        int64
	Name      string
	Enabled   bool
	Source    string // gmail, outlook, chat, calendar or any
	Field     string
	Op        string
	Value     string
	CreatedBy string
	Note      string
	// SPEC 21: a rule an agent proposed is Status "proposed" and does nothing
	// until the owner approves it (then "active"); one he dismissed stays as "dismissed".
	Status     string
	Reason     string
	ProposedBy string
	ProposedAt int64
	DecidedAt  int64
	DecidedBy  string
}

// EnsureRule adds a rule with this name unless one is already there; the owner's
// later edits of a rule are never overwritten.
func (q Q) EnsureRule(ctx context.Context, r Rule, now int64) error {
	var id int64
	err := q.x.QueryRowContext(ctx, "SELECT id FROM rules WHERE name=?", r.Name).Scan(&id)
	if err == nil {
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	_, err = q.x.ExecContext(ctx, `INSERT INTO rules (name, enabled, source, field, op, value, created_at, created_by, note, test_run) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		r.Name, b2i(r.Enabled), r.Source, r.Field, r.Op, r.Value, now, r.CreatedBy, r.Note, RunOf(ctx))
	return err
}

// EnabledRules lists the enabled rules for a source in the order they apply.
func (q Q) EnabledRules(ctx context.Context, source string) ([]Rule, error) {
	rows, err := q.x.QueryContext(ctx, `SELECT id, name, enabled, source, field, op, value, created_by, note FROM rules
		WHERE enabled=1 AND status='active' AND (source=? OR source='any') ORDER BY id`, source)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Rule
	for rows.Next() {
		var r Rule
		var en int
		if err := rows.Scan(&r.ID, &r.Name, &en, &r.Source, &r.Field, &r.Op, &r.Value, &r.CreatedBy, &r.Note); err != nil {
			return nil, err
		}
		r.Enabled = en == 1
		out = append(out, r)
	}
	return out, rows.Err()
}

// RuleByName finds a rule by its name.
func (q Q) RuleByName(ctx context.Context, name string) (Rule, bool) {
	var r Rule
	var en int
	err := q.x.QueryRowContext(ctx, `SELECT id, name, enabled, source, field, op, value, created_by, note FROM rules WHERE name=?`, name).
		Scan(&r.ID, &r.Name, &en, &r.Source, &r.Field, &r.Op, &r.Value, &r.CreatedBy, &r.Note)
	r.Enabled = en == 1
	return r, err == nil
}

// Skipped is the metadata kept of a message a rule dropped.
type Skipped struct {
	ID         int64
	At         int64
	Source     string
	SourceID   string
	Sender     string
	Subject    string
	ReceivedAt int64
	RuleID     int64
	Reimported bool
	Snippet    string // the beginning of the text, kept at the drop for the night check
}

// InsertSkipped keeps one metadata line per dropped message; inserted is
// false when the line is already there.
func (q Q) InsertSkipped(ctx context.Context, s Skipped) (bool, error) {
	res, err := q.x.ExecContext(ctx, `INSERT INTO skipped (at, source, source_id, sender, subject, received_at, rule_id, test_run, snippet) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (source, source_id) DO NOTHING`, s.At, s.Source, s.SourceID, s.Sender, s.Subject, s.ReceivedAt, s.RuleID, RunOf(ctx), s.Snippet)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

func (q Q) SkippedByID(ctx context.Context, id int64) (Skipped, bool) {
	var s Skipped
	var re sql.NullInt64
	err := q.x.QueryRowContext(ctx, `SELECT id, at, source, source_id, sender, subject, received_at, rule_id, reimported_at FROM skipped WHERE id=?`, id).
		Scan(&s.ID, &s.At, &s.Source, &s.SourceID, &s.Sender, &s.Subject, &s.ReceivedAt, &s.RuleID, &re)
	s.Reimported = re.Valid
	return s, err == nil
}

func (q Q) SkippedBySource(ctx context.Context, source, sourceID string) (Skipped, bool) {
	var id int64
	if err := q.x.QueryRowContext(ctx, `SELECT id FROM skipped WHERE source=? AND source_id=?`, source, sourceID).Scan(&id); err != nil {
		return Skipped{}, false
	}
	return q.SkippedByID(ctx, id)
}

func (q Q) MarkReimported(ctx context.Context, id, now int64) error {
	_, err := q.x.ExecContext(ctx, `UPDATE skipped SET reimported_at=? WHERE id=?`, now, id)
	return err
}

// --- people ---

// EnsurePerson adds a person with one handle on first sight: name and
// address only. It changes nothing for a handle that is already known.
func (q Q) EnsurePerson(ctx context.Context, network, value, name string, now int64) (created bool, err error) {
	var pid int64
	err = q.x.QueryRowContext(ctx, "SELECT person_id FROM person_handles WHERE network=? AND lower(value)=lower(?)", network, value).Scan(&pid)
	if err == nil {
		return false, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return false, err
	}
	if name == "" {
		name = value
	}
	res, err := q.x.ExecContext(ctx, `INSERT INTO people (name, updated_by, updated_at, test_run) VALUES (?, 'system', ?, ?)`, name, now, RunOf(ctx))
	if err != nil {
		return false, err
	}
	pid, err = res.LastInsertId()
	if err != nil {
		return false, err
	}
	_, err = q.x.ExecContext(ctx, `INSERT INTO person_handles (person_id, network, value, test_run) VALUES (?, ?, ?, ?)`, pid, network, value, RunOf(ctx))
	return err == nil, err
}

// CountMessages counts the messages one source brought in.
func (q Q) CountMessages(ctx context.Context, source string) int {
	var n int
	q.x.QueryRowContext(ctx, "SELECT COUNT(*) FROM messages WHERE source=?", source).Scan(&n)
	return n
}

// CountItemsOf counts the items that hold a message of the source. Items of
// two sources never join, so each item counts for one source only.
func (q Q) CountItemsOf(ctx context.Context, source string) int {
	var n int
	q.x.QueryRowContext(ctx, "SELECT COUNT(DISTINCT item_id) FROM messages WHERE source=?", source).Scan(&n)
	return n
}

// PendingFileCountOf counts the files of one source still to be saved.
func (q Q) PendingFileCountOf(ctx context.Context, source string) int {
	var n int
	q.x.QueryRowContext(ctx, "SELECT COUNT(*) FROM files WHERE status='pending' AND source=?", source).Scan(&n)
	return n
}
