package repo

import (
	"context"
	"database/sql"
)

// Matters (SPEC 10): proposals of a matter pattern, and joining or splitting
// rows once the owner approved. A row that was joined into another is kept, hidden,
// with merged_into pointing at the row that stays: its number leads there, and
// everything it held (messages, links, files, notes, to-dos, proposals) belongs
// to the row that stays. Nothing is deleted.

const matterTable = `
CREATE TABLE IF NOT EXISTS matter_proposals (
	id INTEGER PRIMARY KEY,
	name TEXT NOT NULL,
	pattern TEXT NOT NULL,
	reason TEXT NOT NULL DEFAULT '',
	proposed_by TEXT NOT NULL,
	proposed_at INTEGER NOT NULL,
	status TEXT NOT NULL DEFAULT 'proposed',
	decided_at INTEGER NOT NULL DEFAULT 0,
	decided_by TEXT NOT NULL DEFAULT '',
	test_run TEXT NOT NULL DEFAULT ''
);
`

const matterIndex = `CREATE INDEX IF NOT EXISTS matter_proposals_test_run ON matter_proposals (test_run) WHERE test_run <> ''`

// MatterProposal is a pattern an agent proposed. Status is proposed (waits for
// Approve and does nothing), approved or dismissed.
type MatterProposal struct {
	ID         int64
	Name       string
	Pattern    string
	Reason     string
	ProposedBy string
	ProposedAt int64
	Status     string
	DecidedAt  int64
	DecidedBy  string
	TestRun    string
	WouldJoin  int // how many existing rows it would have joined; -1 when not counted
}

const matterProposalCols = `id, name, pattern, reason, proposed_by, proposed_at, status, decided_at, decided_by, test_run, would_join`

func scanMatterProposal(row interface{ Scan(...any) error }) (MatterProposal, error) {
	var p MatterProposal
	err := row.Scan(&p.ID, &p.Name, &p.Pattern, &p.Reason, &p.ProposedBy, &p.ProposedAt, &p.Status, &p.DecidedAt, &p.DecidedBy, &p.TestRun, &p.WouldJoin)
	return p, err
}

func (q Q) InsertMatterProposal(ctx context.Context, p MatterProposal) (int64, error) {
	res, err := q.x.ExecContext(ctx, `INSERT INTO matter_proposals (name, pattern, reason, proposed_by, proposed_at, test_run, would_join) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		p.Name, p.Pattern, p.Reason, p.ProposedBy, p.ProposedAt, RunOf(ctx), p.WouldJoin)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (q Q) MatterProposalByID(ctx context.Context, id int64) (MatterProposal, bool) {
	p, err := scanMatterProposal(q.x.QueryRowContext(ctx, `SELECT `+matterProposalCols+` FROM matter_proposals WHERE id=?`, id))
	return p, err == nil
}

// WaitingMatterProposals lists the proposals that wait for Approve, oldest first.
func (q Q) WaitingMatterProposals(ctx context.Context) ([]MatterProposal, error) {
	rows, err := q.x.QueryContext(ctx, `SELECT `+matterProposalCols+` FROM matter_proposals WHERE status='proposed' ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MatterProposal
	for rows.Next() {
		p, err := scanMatterProposal(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// SameMatterProposal finds a waiting proposal of the same pattern made under
// the same run (or none): a run never meets the real one, nor another run's.
func (q Q) SameMatterProposal(ctx context.Context, pattern string) (MatterProposal, bool) {
	p, err := scanMatterProposal(q.x.QueryRowContext(ctx, `SELECT `+matterProposalCols+` FROM matter_proposals WHERE status='proposed' AND pattern=? AND test_run=? ORDER BY id LIMIT 1`,
		pattern, RunOf(ctx)))
	return p, err == nil
}

// CountMatterProposals counts the waiting proposals of the run (or the real ones).
func (q Q) CountMatterProposals(ctx context.Context) int {
	var n int
	q.x.QueryRowContext(ctx, `SELECT COUNT(*) FROM matter_proposals WHERE status='proposed' AND test_run=?`, RunOf(ctx)).Scan(&n)
	return n
}

// DecideMatterProposal says false when the proposal no longer waits.
func (q Q) DecideMatterProposal(ctx context.Context, id int64, approve bool, by string, at int64) (bool, error) {
	status := "dismissed"
	if approve {
		status = "approved"
	}
	res, err := q.x.ExecContext(ctx, `UPDATE matter_proposals SET status=?, decided_at=?, decided_by=? WHERE id=? AND status='proposed'`, status, at, by, id)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

// ApprovedRunPatterns are the patterns the owner approved from a proposal made
// under a run. They apply to that run's mail only and go with the run.
func (q Q) ApprovedRunPatterns(ctx context.Context) ([]MatterProposal, error) {
	run := RunOf(ctx)
	if run == "" {
		return nil, nil
	}
	rows, err := q.x.QueryContext(ctx, `SELECT `+matterProposalCols+` FROM matter_proposals WHERE status='approved' AND test_run=? ORDER BY id`, run)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MatterProposal
	for rows.Next() {
		p, err := scanMatterProposal(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// --- rows that were joined ---

// MergedInto is the row an item was joined into (0 when it was not).
func (q Q) MergedInto(ctx context.Context, id int64) int64 {
	var n int64
	q.x.QueryRowContext(ctx, `SELECT merged_into FROM items WHERE id=?`, id).Scan(&n)
	return n
}

// JoinItems moves everything the dropped row holds into the row that stays and
// leaves the dropped row hidden, pointing at the row that stays. Events cannot
// be moved (they are append-only); the row that stays shows them with its own.
// The caller checked both rows and wrote the entries.
func (q Q) JoinItems(ctx context.Context, keep, drop Item) (Item, error) {
	for _, table := range []string{"messages", "item_links", "files", "proposals", "notes", "agenda", "chat_out", "chats"} {
		if _, err := q.x.ExecContext(ctx, `UPDATE `+table+` SET item_id=? WHERE item_id=?`, keep.ID, drop.ID); err != nil {
			return keep, err
		}
	}
	// chat_since goes too: a hidden joined row must never be released to Claude.
	if _, err := q.x.ExecContext(ctx, `UPDATE items SET merged_into=?, hidden=1, needs_claude=0, chat_since=0 WHERE id=?`, keep.ID, drop.ID); err != nil {
		return keep, err
	}
	// Comments of the joined row stay on its events; search finds them on the row that stays.
	if _, err := q.x.ExecContext(ctx, `UPDATE finder SET item=? WHERE item=? AND kind='comment'`, keep.ID, drop.ID); err != nil {
		return keep, err
	}
	// A row that holds a chat is a chat row (it has the box to write in, and a phone lists it).
	// A meeting thread stays one (SPEC 33): joined mail or a chat does not change its kind.
	if drop.Kind == "chat" && keep.Kind != "chat" && keep.Kind != MeetingKind {
		keep.Kind = "chat"
		if _, err := q.x.ExecContext(ctx, `UPDATE items SET kind='chat' WHERE id=?`, keep.ID); err != nil {
			return keep, err
		}
	}
	if drop.ChatSince > keep.ChatSince {
		keep.ChatSince = drop.ChatSince
	}
	if drop.FirstAt < keep.FirstAt {
		keep.FirstAt = drop.FirstAt
	}
	if drop.LastAt >= keep.LastAt {
		keep.LastAt = drop.LastAt
		if keep.Kind != MeetingKind { // a meeting thread keeps its entry's title
			keep.Subject = drop.Subject
		}
	}
	if drop.MovedAt > keep.MovedAt {
		keep.MovedAt = drop.MovedAt
	}
	return keep, nil
}

// SplitThread takes the messages of one thread (its thread key) and its links
// out of an item into another item that already exists. A chat thread takes
// its chat, and what the owner wrote in it, along.
func (q Q) SplitThread(ctx context.Context, from, to int64, threadKey, linkKind, linkValue string) error {
	if _, err := q.x.ExecContext(ctx, `UPDATE messages SET item_id=? WHERE item_id=? AND thread_key=?`, to, from, threadKey); err != nil {
		return err
	}
	if _, err := q.x.ExecContext(ctx, `UPDATE files SET item_id=? WHERE item_id=? AND message_id IN (SELECT id FROM messages WHERE item_id=?)`, to, from, to); err != nil {
		return err
	}
	if linkKind == "chat" {
		if _, err := q.x.ExecContext(ctx, `UPDATE chats SET item_id=? WHERE item_id=? AND key=?`, to, from, linkValue); err != nil {
			return err
		}
		if _, err := q.x.ExecContext(ctx, `UPDATE chat_out SET item_id=? WHERE item_id=? AND network || '/' || chat_id = ?`, to, from, linkValue); err != nil {
			return err
		}
		if _, err := q.x.ExecContext(ctx, `UPDATE files SET item_id=? WHERE item_id=? AND id IN (SELECT file_id FROM chat_out WHERE item_id=? AND file_id<>0)`, to, from, to); err != nil {
			return err
		}
	}
	_, err := q.x.ExecContext(ctx, `UPDATE item_links SET item_id=? WHERE item_id=? AND kind=? AND value=?`, to, from, linkKind, linkValue)
	return err
}

// SetKind says whether a row is a chat row (it holds a chat) or a mail row.
func (q Q) SetKind(ctx context.Context, id int64, kind string) error {
	_, err := q.x.ExecContext(ctx, `UPDATE items SET kind=? WHERE id=?`, kind, id)
	return err
}

// ChatsOfItem counts the chats an item holds.
func (q Q) ChatsOfItem(ctx context.Context, itemID int64) int {
	var n int
	q.x.QueryRowContext(ctx, `SELECT COUNT(*) FROM chats WHERE item_id=?`, itemID).Scan(&n)
	return n
}

// MailOfItem counts the mail messages an item holds.
func (q Q) MailOfItem(ctx context.Context, itemID int64) int {
	var n int
	q.x.QueryRowContext(ctx, `SELECT COUNT(*) FROM messages WHERE item_id=? AND network='mail'`, itemID).Scan(&n)
	return n
}

// MessageSpan is the oldest and newest message time and the subject of the
// newest message of an item (zero when it has none).
func (q Q) MessageSpan(ctx context.Context, itemID int64) (first, last int64, subject string) {
	var f, l sql.NullInt64
	q.x.QueryRowContext(ctx, `SELECT MIN(received_at), MAX(received_at) FROM messages WHERE item_id=?`, itemID).Scan(&f, &l)
	q.x.QueryRowContext(ctx, `SELECT subject FROM messages WHERE item_id=? ORDER BY received_at DESC, id DESC LIMIT 1`, itemID).Scan(&subject)
	return f.Int64, l.Int64, subject
}

// ThreadKeyOfMessage is the thread of a message of an item ("" when none).
func (q Q) ThreadOfMessage(ctx context.Context, itemID, messageID int64) (key, network, subject string, ok bool) {
	err := q.x.QueryRowContext(ctx, `SELECT thread_key, network, subject FROM messages WHERE id=? AND item_id=?`, messageID, itemID).Scan(&key, &network, &subject)
	return key, network, subject, err == nil
}

// ThreadsOfItem counts the distinct mail threads of an item.
func (q Q) ThreadsOfItem(ctx context.Context, itemID int64) int {
	var n int
	q.x.QueryRowContext(ctx, `SELECT COUNT(DISTINCT thread_key) FROM messages WHERE item_id=?`, itemID).Scan(&n)
	return n
}

// LinksOfItem lists an item's (kind, value) links.
func (q Q) LinksOfItem(ctx context.Context, itemID int64) ([][2]string, error) {
	rows, err := q.x.QueryContext(ctx, `SELECT kind, value FROM item_links WHERE item_id=? ORDER BY id`, itemID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out [][2]string
	for rows.Next() {
		var kv [2]string
		if err := rows.Scan(&kv[0], &kv[1]); err != nil {
			return nil, err
		}
		out = append(out, kv)
	}
	return out, rows.Err()
}

// PatternSample is one message as a pattern is tried on it.
type PatternSample struct {
	ItemID        int64
	Subject, Text string
}

// MailForPatterns lists the newest mail of others since a time, of the run (or
// the real mail), for counting what a pattern would have joined.
func (q Q) MailForPatterns(ctx context.Context, since int64, limit int) ([]PatternSample, error) {
	rows, err := q.x.QueryContext(ctx, `SELECT item_id, subject, substr(text, 1, 4000) FROM messages WHERE sent_by_me=0 AND network='mail' AND received_at >= ? AND test_run=? ORDER BY received_at DESC, id DESC LIMIT ?`,
		since, RunOf(ctx), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PatternSample
	for rows.Next() {
		var m PatternSample
		if err := rows.Scan(&m.ItemID, &m.Subject, &m.Text); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// ApprovedMatterProposal finds the proposal the owner approved for this pattern,
// made under the same run (or none); the newest one when there are several.
func (q Q) ApprovedMatterProposal(ctx context.Context, pattern string) (MatterProposal, bool) {
	p, err := scanMatterProposal(q.x.QueryRowContext(ctx, `SELECT `+matterProposalCols+` FROM matter_proposals WHERE status='approved' AND pattern=? AND test_run=? ORDER BY id DESC LIMIT 1`,
		pattern, RunOf(ctx)))
	return p, err == nil
}
