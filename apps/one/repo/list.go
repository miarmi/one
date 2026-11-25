package repo

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
)

// --- the list ---

// ListedStages are the stages the list shows, in order. Closed has a page of
// its own and history is not listed.
var ListedStages = []string{"new", "in_progress", "pending", "resolved"}

type phoneKey struct{}

// WithPhone marks a request that comes from a phone: the list then leaves out
// chats whose thread holds nothing but the chat's own messages (SPEC 17).
func WithPhone(ctx context.Context) context.Context { return context.WithValue(ctx, phoneKey{}, true) }

// phoneClause is the part of a list query that a phone adds: a chat is listed
// only when a thread grew from it (a comment or summary, a note, a proposal
// or a to-do made from it). A row that joined a chat with mail counts as a
// thread that grew from the chat (SPEC 10), so it is listed.
func phoneClause(ctx context.Context) string {
	if on, _ := ctx.Value(phoneKey{}).(bool); !on {
		return ""
	}
	return ` AND (i.kind <> 'chat'
		OR EXISTS (SELECT 1 FROM messages mm WHERE mm.item_id = i.id AND mm.network = 'mail')
		OR EXISTS (SELECT 1 FROM events e WHERE e.item_id = i.id AND e.type IN ('commented', 'noted', 'proposal_made', 'todo_made'))
		OR EXISTS (SELECT 1 FROM proposals p WHERE p.item_id = i.id)
		OR EXISTS (SELECT 1 FROM agenda ag WHERE ag.item_id = i.id AND ag.kind = 'todo'))`
}

type onlyKey struct{}

// WithOnly narrows the list queries (ListItems, StageCounts) to the items that
// hold a message from one of the sources: the choice of one connection in the
// Connections pane (SPEC 23). The counts follow because they use the same clause.
func WithOnly(ctx context.Context, sources []string) context.Context {
	return context.WithValue(ctx, onlyKey{}, sources)
}

func onlyClause(ctx context.Context) (string, []any) {
	src, _ := ctx.Value(onlyKey{}).([]string)
	if len(src) == 0 {
		return "", nil
	}
	args := make([]any, len(src))
	for i, s := range src {
		args[i] = s
	}
	return ` AND EXISTS (SELECT 1 FROM messages om WHERE om.item_id = i.id AND om.source IN (?` + strings.Repeat(",?", len(src)-1) + `))`, args
}

// StageCounts counts the items of every stage (on a phone, the items it lists).
func (q Q) StageCounts(ctx context.Context) (map[string]int, error) {
	only, oargs := onlyClause(ctx)
	rows, err := q.x.QueryContext(ctx, `SELECT i.stage, COUNT(*) FROM items i WHERE i.hidden = 0`+phoneClause(ctx)+only+` GROUP BY i.stage`, oargs...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var s string
		var n int
		if err := rows.Scan(&s, &n); err != nil {
			return nil, err
		}
		out[s] = n
	}
	return out, rows.Err()
}

// OldestPending is when the longest-waiting Pending item started to wait
// (0 when none waits).
func (q Q) OldestPending(ctx context.Context) int64 {
	var t sql.NullInt64
	only, oargs := onlyClause(ctx)
	q.x.QueryRowContext(ctx, `SELECT MIN(i.pending_since) FROM items i WHERE i.stage='pending' AND i.hidden = 0 AND i.pending_since IS NOT NULL`+only, oargs...).Scan(&t)
	return t.Int64
}

// ListRow is one line of the list: an item with what its closed line shows.
type ListRow struct {
	Item
	Count      int
	Sender     string // address of the newest message that is not the owner's own
	SenderName string
	Source     string // source of the newest message
	Snippet    string // the beginning of the newest text

	// The first proposal of Claude waiting for Approve, and how many wait.
	ProposalID     int64
	ProposalAction string
	ProposalBy     string
	ProposalText   string // a to-do proposal: what the to-do says
	ProposalDue    string // and its time or due date, if it has one
	ProposalReason string // why, in the words of whoever proposed it (empty in old data)
	ProposalTo     string // a reply proposal: where it goes, as stored (JSON)
	Proposals      int
	HasTodo        bool // a to-do made from this row is still open
	HasFiles       bool // the thread holds a file that is not a picture inside a message (SPEC 7)
}

// ListItems lists one page of a stage, the item that took its place last
// first.
func (q Q) ListItems(ctx context.Context, stage string, limit, offset int) ([]ListRow, error) {
	only, oargs := onlyClause(ctx)
	return q.listRows(ctx, "i.stage = ? AND i.hidden = 0"+phoneClause(ctx)+only, append([]any{stage}, oargs...), "ORDER BY i.moved_at DESC, i.id DESC LIMIT ? OFFSET ?", limit, offset)
}

// ListRowByID is the list line of one item.
func (q Q) ListRowByID(ctx context.Context, id int64) (ListRow, bool) {
	rows, err := q.listRows(ctx, "i.id = ?", []any{id}, "LIMIT ? OFFSET ?", 1, 0)
	if err != nil || len(rows) == 0 {
		return ListRow{}, false
	}
	return rows[0], true
}

func (q Q) listRows(ctx context.Context, where string, args []any, tail string, limit, offset int) ([]ListRow, error) {
	rows, err := q.x.QueryContext(ctx, `SELECT `+prefixed("i.", itemCols)+`,
		(SELECT COUNT(*) FROM messages m WHERE m.item_id = i.id),
		COALESCE((SELECT m.sender FROM messages m WHERE m.item_id = i.id ORDER BY m.sent_by_me, m.received_at DESC, m.id DESC LIMIT 1), ''),
		COALESCE((SELECT m.sender_name FROM messages m WHERE m.item_id = i.id ORDER BY m.sent_by_me, m.received_at DESC, m.id DESC LIMIT 1), ''),
		COALESCE((SELECT m.source FROM messages m WHERE m.item_id = i.id ORDER BY m.received_at DESC, m.id DESC LIMIT 1), ''),
		COALESCE((SELECT substr(m.text, 1, 400) FROM messages m WHERE m.item_id = i.id ORDER BY m.received_at DESC, m.id DESC LIMIT 1), ''),
		COALESCE((SELECT p.id FROM proposals p WHERE p.item_id = i.id AND p.status = 'pending' ORDER BY p.id LIMIT 1), 0),
		COALESCE((SELECT p.action FROM proposals p WHERE p.item_id = i.id AND p.status = 'pending' ORDER BY p.id LIMIT 1), ''),
		COALESCE((SELECT p.proposed_by FROM proposals p WHERE p.item_id = i.id AND p.status = 'pending' ORDER BY p.id LIMIT 1), ''),
		COALESCE((SELECT p.text FROM proposals p WHERE p.item_id = i.id AND p.status = 'pending' ORDER BY p.id LIMIT 1), ''),
		COALESCE((SELECT p.due FROM proposals p WHERE p.item_id = i.id AND p.status = 'pending' ORDER BY p.id LIMIT 1), ''),
		COALESCE((SELECT p.reason FROM proposals p WHERE p.item_id = i.id AND p.status = 'pending' ORDER BY p.id LIMIT 1), ''),
		COALESCE((SELECT p.recipients FROM proposals p WHERE p.item_id = i.id AND p.status = 'pending' ORDER BY p.id LIMIT 1), ''),
		(SELECT COUNT(*) FROM proposals p WHERE p.item_id = i.id AND p.status = 'pending'),
		EXISTS (SELECT 1 FROM agenda a WHERE a.item_id = i.id AND a.kind = 'todo' AND a.state = 'open'),
		EXISTS (SELECT 1 FROM files f WHERE f.item_id = i.id AND f.disposition <> 'inline')
		FROM items i WHERE `+where+` `+tail, append(args, limit, offset)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ListRow
	for rows.Next() {
		var r ListRow
		var nc, hid, todo, fil int
		if err := rows.Scan(&r.ID, &r.Number, &r.Kind, &r.Subject, &r.Stage, &nc, &r.PendingSince, &r.FirstAt, &r.LastAt, &r.MovedAt, &hid, &r.ChatSince, &r.TestRun,
			&r.Count, &r.Sender, &r.SenderName, &r.Source, &r.Snippet, &r.ProposalID, &r.ProposalAction, &r.ProposalBy, &r.ProposalText, &r.ProposalDue, &r.ProposalReason, &r.ProposalTo, &r.Proposals, &todo, &fil); err != nil {
			return nil, err
		}
		r.NeedsClaude, r.Hidden, r.HasTodo, r.HasFiles = nc == 1, hid == 1, todo == 1, fil == 1
		out = append(out, r)
	}
	return out, rows.Err()
}

// LastEventID is the mark a page takes when it is drawn: what comes after it is
// new to that page, even within the same second (SPEC 2).
func (q Q) LastEventID(ctx context.Context) int64 {
	var n sql.NullInt64
	q.x.QueryRowContext(ctx, `SELECT MAX(id) FROM events`).Scan(&n)
	return n.Int64
}

// ArrivedSince counts the listed items that got a message, or came back, after
// the page's mark (a LastEventID; 0 for a page drawn on an empty app). It
// decides whether the page offers the button for new items.
func (q Q) ArrivedSince(ctx context.Context, since int64) int {
	var n int
	only, oargs := onlyClause(ctx)
	q.x.QueryRowContext(ctx, `SELECT COUNT(DISTINCT e.item_id) FROM events e JOIN items i ON i.id = e.item_id
		WHERE e.id > ? AND i.hidden = 0 AND e.type IN ('ingested', 'reopened', 'revealed') AND i.stage IN ('new', 'in_progress', 'pending', 'resolved')`+only, append([]any{since}, oargs...)...).Scan(&n)
	return n
}

// --- one item: its messages, files and thread ---

// NewestMessageAt is the time of the item's newest message (0 when none).
func (q Q) NewestMessageAt(ctx context.Context, itemID int64) int64 {
	var t sql.NullInt64
	q.x.QueryRowContext(ctx, `SELECT MAX(received_at) FROM messages WHERE item_id=?`, itemID).Scan(&t)
	return t.Int64
}

// MessagesSince lists the item's messages received at or after the time,
// newest first (SPEC 9), and says how many older ones there are.
func (q Q) MessagesSince(ctx context.Context, itemID, since int64) (msgs []Message, older int, err error) {
	if err = q.x.QueryRowContext(ctx, `SELECT COUNT(*) FROM messages WHERE item_id=? AND received_at < ?`, itemID, since).Scan(&older); err != nil {
		return nil, 0, err
	}
	rows, err := q.x.QueryContext(ctx, `SELECT id, item_id, raw_id, source, source_id, thread_key, network, sender, sender_name, subject, to_addrs, cc_addrs,
		addressed, has_attachments, text, sent_by_me, received_at, labels, flags, seen, starred, header_id, edited
		FROM messages WHERE item_id=? AND received_at >= ? ORDER BY received_at DESC, id DESC`, itemID, since)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	for rows.Next() {
		var m Message
		var labels string
		var addr, att, mine, seen, star, edited int
		if err = rows.Scan(&m.ID, &m.ItemID, &m.RawID, &m.Source, &m.SourceID, &m.ThreadKey, &m.Network, &m.Sender, &m.SenderName, &m.Subject, &m.To, &m.Cc,
			&addr, &att, &m.Text, &mine, &m.ReceivedAt, &labels, &m.Flags, &seen, &star, &m.HeaderID, &edited); err != nil {
			return nil, 0, err
		}
		m.Addressed, m.HasAttachments, m.SentByMe, m.Seen, m.Starred, m.Edited = addr == 1, att == 1, mine == 1, seen == 1, star == 1, edited == 1
		json.Unmarshal([]byte(labels), &m.Labels)
		msgs = append(msgs, m)
	}
	return msgs, older, rows.Err()
}

// FilesOfItem lists the files of an item by message, in the order they came.
func (q Q) FilesOfItem(ctx context.Context, itemID int64) (map[int64][]File, error) {
	rows, err := q.x.QueryContext(ctx, `SELECT id, item_id, source, COALESCE(message_id, 0), part, filename, content_type, disposition, content_id, size, sha256, path, status, error, added_at
		FROM files WHERE item_id=? ORDER BY id`, itemID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64][]File{}
	for rows.Next() {
		var f File
		if err := rows.Scan(&f.ID, &f.ItemID, &f.Source, &f.MessageID, &f.Part, &f.Filename, &f.ContentType, &f.Disposition, &f.ContentID, &f.Size, &f.SHA, &f.Path, &f.Status, &f.Error, &f.AddedAt); err != nil {
			return nil, err
		}
		out[f.MessageID] = append(out[f.MessageID], f)
	}
	return out, rows.Err()
}

// FileByID finds a file of the pool that is saved and can be served.
func (q Q) FileByID(ctx context.Context, id int64) (File, bool) {
	var f File
	var mid sql.NullInt64
	err := q.x.QueryRowContext(ctx, `SELECT id, item_id, source, message_id, part, filename, content_type, disposition, content_id, size, sha256, path, status, error, added_at
		FROM files WHERE id=? AND status='saved' AND path <> ''`, id).
		Scan(&f.ID, &f.ItemID, &f.Source, &mid, &f.Part, &f.Filename, &f.ContentType, &f.Disposition, &f.ContentID, &f.Size, &f.SHA, &f.Path, &f.Status, &f.Error, &f.AddedAt)
	f.MessageID = mid.Int64
	return f, err == nil
}

// Entry is one event of an item that the thread shows.
type Entry struct {
	ID    int64
	At    int64
	Actor string
	Type  string
	Data  string
}

// EntriesOfItem lists the item's events of the given types, oldest first.
func (q Q) EntriesOfItem(ctx context.Context, itemID int64, types ...string) ([]Entry, error) {
	if len(types) == 0 {
		return nil, nil
	}
	in := strings.TrimSuffix(strings.Repeat("?,", len(types)), ",")
	args := []any{itemID, itemID}
	for _, t := range types {
		args = append(args, t)
	}
	rows, err := q.x.QueryContext(ctx, `SELECT id, at, actor, type, data FROM events WHERE (item_id=? OR item_id IN (SELECT id FROM items WHERE merged_into=?)) AND type IN (`+in+`) ORDER BY id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Entry
	for rows.Next() {
		var e Entry
		if err := rows.Scan(&e.ID, &e.At, &e.Actor, &e.Type, &e.Data); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// SetItemStage moves an item to a stage and puts it first in that group. A
// Pending item remembers when it started to wait; a Resolved one when it was
// resolved.
func (q Q) SetItemStage(ctx context.Context, id int64, stage string, now int64) error {
	var pend, resolved, closed any
	switch stage {
	case "pending":
		pend = now
	case "resolved":
		resolved = now
	case "closed":
		closed = now
	}
	res, err := q.x.ExecContext(ctx, `UPDATE items SET stage=?, moved_at=?, pending_since=?, resolved_at=?, closed_at=? WHERE id=?`, stage, now, pend, resolved, closed, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return errors.New("no such item")
	}
	return nil
}

// QueueForClaude marks an item as waiting for Claude (a note from the owner is a
// request, queued once).
func (q Q) QueueForClaude(ctx context.Context, id int64) error {
	_, err := q.x.ExecContext(ctx, `UPDATE items SET needs_claude=1 WHERE id=?`, id)
	return err
}
