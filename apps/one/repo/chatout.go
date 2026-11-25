package repo

import (
	"context"
	"database/sql"
)

// What the owner writes in a chat himself (SPEC 17). Each write is a group of
// parts: the text, then each file as a message of its own, in order. A part
// goes: queued (the intent is written) -> handed (the bridge took it; never
// handed out again) -> sent, failed or unknown. A queued part can be withdrawn;
// a handed one cannot. A sent part that comes back from the network through
// the bridge is matched to its message (msg_id) and is then not shown twice.

// OutPart is one message that the owner's write sends.
type OutPart struct {
	ID        int64
	ItemID    int64
	Network   string
	ChatID    string
	Group     string
	Seq       int
	Kind      string // text or file
	Text      string
	FileName  string
	FileType  string
	Size      int64
	SHA       string
	Path      string // relative to the pool
	FileID    int64  // the row of files that lists it
	State     string // queued, handed, sent, failed, unknown, withdrawn
	Error     string
	Ref       string // the message id the network gave
	CreatedAt int64
	HandedAt  int64
	EndedAt   int64
	MsgID     int64 // the message it came back as
}

const outCols = `id, item_id, network, chat_id, grp, seq, kind, text, fname, ctype, size, sha256, path, file_id, state, error, ref, created_at, handed_at, ended_at, msg_id`

func scanOut(row interface{ Scan(...any) error }) (OutPart, error) {
	var p OutPart
	err := row.Scan(&p.ID, &p.ItemID, &p.Network, &p.ChatID, &p.Group, &p.Seq, &p.Kind, &p.Text, &p.FileName, &p.FileType, &p.Size, &p.SHA, &p.Path, &p.FileID,
		&p.State, &p.Error, &p.Ref, &p.CreatedAt, &p.HandedAt, &p.EndedAt, &p.MsgID)
	return p, err
}

func (q Q) outWhere(ctx context.Context, where string, args ...any) ([]OutPart, error) {
	rows, err := q.x.QueryContext(ctx, `SELECT `+outCols+` FROM chat_out WHERE `+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []OutPart
	for rows.Next() {
		p, err := scanOut(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// InsertOut writes the parts of one write, queued, in order.
func (q Q) InsertOut(ctx context.Context, parts []OutPart) ([]int64, error) {
	ids := make([]int64, 0, len(parts))
	for _, p := range parts {
		mark, err := q.runForItem(ctx, p.ItemID)
		if err != nil {
			return nil, err
		}
		res, err := q.x.ExecContext(ctx, `INSERT INTO chat_out (item_id, network, chat_id, grp, seq, kind, text, fname, ctype, size, sha256, path, file_id, state, created_at, test_run)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'queued', ?, ?)`,
			p.ItemID, p.Network, p.ChatID, p.Group, p.Seq, p.Kind, p.Text, p.FileName, p.FileType, p.Size, p.SHA, p.Path, p.FileID, p.CreatedAt, mark)
		if err != nil {
			return nil, err
		}
		id, _ := res.LastInsertId()
		ids = append(ids, id)
	}
	return ids, nil
}

// OutOfItem lists the parts the owner wrote in an item's chat, oldest first.
func (q Q) OutOfItem(ctx context.Context, itemID int64) ([]OutPart, error) {
	return q.outWhere(ctx, `item_id=? ORDER BY id`, itemID)
}

// OutByID is one part.
func (q Q) OutByID(ctx context.Context, id int64) (OutPart, bool) {
	p, err := scanOut(q.x.QueryRowContext(ctx, `SELECT `+outCols+` FROM chat_out WHERE id=?`, id))
	return p, err == nil
}

// ClaimOut hands out the queued parts, oldest first. Each is handed out once:
// the claim is one conditional write, so a part that was withdrawn meanwhile,
// or that another request took, is not in the answer. A write is handed out
// whole, so its parts stay in order.
func (q Q) ClaimOut(ctx context.Context, limit int, now int64) ([]OutPart, error) {
	cands, err := q.outWhere(ctx, `state='queued' ORDER BY id`)
	if err != nil {
		return nil, err
	}
	var out []OutPart
	for _, p := range cands {
		if len(out) >= limit {
			break
		}
		res, err := q.x.ExecContext(ctx, `UPDATE chat_out SET state='handed', handed_at=? WHERE id=? AND state='queued'`, now, p.ID)
		if err != nil {
			return nil, err
		}
		if n, _ := res.RowsAffected(); n == 1 {
			p.State, p.HandedAt = "handed", now
			out = append(out, p)
		}
	}
	return out, nil
}

// WithdrawOut takes back every part of a write that has not been handed out.
// It says false when no part was still queued.
func (q Q) WithdrawOut(ctx context.Context, group string, now int64) (bool, error) {
	res, err := q.x.ExecContext(ctx, `UPDATE chat_out SET state='withdrawn', ended_at=? WHERE grp=? AND state='queued'`, now, group)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// EndOut records what the bridge reported for a handed-out part. A report for
// a part that was not handed out is refused (false); a second report changes
// nothing. A part marked unknown takes a late, definite report.
func (q Q) EndOut(ctx context.Context, id int64, state, errText, ref string, at int64) (bool, error) {
	res, err := q.x.ExecContext(ctx, `UPDATE chat_out SET state=?, error=?, ended_at=?, ref=CASE WHEN ?<>'' THEN ? ELSE ref END
		WHERE id=? AND (state='handed' OR (state='unknown' AND ?<>'unknown'))`, state, errText, at, ref, ref, id, state)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

// OutHandedBefore lists the parts handed out before a time whose result never came.
func (q Q) OutHandedBefore(ctx context.Context, before int64) ([]OutPart, error) {
	return q.outWhere(ctx, `state='handed' AND handed_at < ? ORDER BY id`, before)
}

// UnmatchedOut lists the parts that went (or may have gone) of an item that have not
// yet come back as a message, oldest first.
func (q Q) UnmatchedOut(ctx context.Context, itemID int64) ([]OutPart, error) {
	return q.outWhere(ctx, `item_id=? AND state IN ('handed','sent','unknown') AND msg_id=0 ORDER BY id`, itemID)
}

// MatchOut points a part at the message it came back as.
func (q Q) MatchOut(ctx context.Context, id, messageID int64) error {
	_, err := q.x.ExecContext(ctx, `UPDATE chat_out SET msg_id=? WHERE id=? AND msg_id=0`, messageID, id)
	return err
}

// OutFileOf is a file part by id, for the bridge's fetch: only a part that is
// a file and that has been handed out and not yet reported.
func (q Q) OutFileOf(ctx context.Context, id int64) (OutPart, bool) {
	p, err := scanOut(q.x.QueryRowContext(ctx, `SELECT `+outCols+` FROM chat_out WHERE id=? AND kind='file' AND state='handed'`, id))
	return p, err == nil
}

// OutWaiting counts the parts that are queued or handed out.
func (q Q) OutWaiting(ctx context.Context) int {
	var n int
	q.x.QueryRowContext(ctx, `SELECT COUNT(*) FROM chat_out WHERE state IN ('queued','handed')`).Scan(&n)
	return n
}

// MarkMessageDeleted marks a chat message as deleted in the network. Its text
// is kept (SPEC 30).
func (q Q) MarkMessageDeleted(ctx context.Context, id int64) error {
	_, err := q.x.ExecContext(ctx, `UPDATE messages SET deleted=1 WHERE id=?`, id)
	return err
}

// DeletedMessages says which of an item's messages were deleted in the network.
func (q Q) DeletedMessages(ctx context.Context, itemID int64) (map[int64]bool, error) {
	rows, err := q.x.QueryContext(ctx, `SELECT id FROM messages WHERE item_id=? AND deleted=1`, itemID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]bool{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = true
	}
	return out, rows.Err()
}

// MessageDeleted says whether one message is marked deleted.
func (q Q) MessageDeleted(ctx context.Context, id int64) bool {
	var d sql.NullInt64
	q.x.QueryRowContext(ctx, `SELECT deleted FROM messages WHERE id=?`, id).Scan(&d)
	return d.Valid && d.Int64 == 1
}

// SaveFileFrom points a pending file at bytes the pool already holds.
func (q Q) SaveFileFrom(ctx context.Context, fileID, size int64, sha, path string) error {
	return q.MarkFileSaved(ctx, fileID, size, sha, path)
}

// InsertOwnFile lists a file the owner attached to what he writes. It belongs to
// the item and to no message of it; it is saved already.
func (q Q) InsertOwnFile(ctx context.Context, f File) (int64, error) {
	mark, err := q.runForItem(ctx, f.ItemID)
	if err != nil {
		return 0, err
	}
	res, err := q.x.ExecContext(ctx, `INSERT INTO files (item_id, source, message_id, part, filename, content_type, disposition, content_id, size, sha256, path, status, error, added_at, test_run)
		VALUES (?, ?, NULL, ?, ?, ?, 'attachment', '', ?, ?, ?, 'saved', '', ?, ?)`,
		f.ItemID, f.Source, f.Part, f.Filename, f.ContentType, f.Size, f.SHA, f.Path, f.AddedAt, mark)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// OutByGroup lists the parts of one write, in order.
func (q Q) OutByGroup(ctx context.Context, group string) ([]OutPart, error) {
	return q.outWhere(ctx, `grp=? ORDER BY seq, id`, group)
}

// CancelOutPart ends a part that was never handed out because the one before
// it did not go.
func (q Q) CancelOutPart(ctx context.Context, id int64, why string, at int64) (bool, error) {
	res, err := q.x.ExecContext(ctx, `UPDATE chat_out SET state='failed', error=?, ended_at=? WHERE id=? AND state='queued'`, why, at, id)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}
