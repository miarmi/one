package repo

import (
	"context"
	"database/sql"
	"errors"
)

// Notes (SPEC 26): what the owner has explained once. The table holds the
// current text; every save is also written as an event by the caller, so
// earlier versions are kept there.

// Note is one saved note. PersonID and ItemID are 0 for a note that stands alone.
type Note struct {
	ID         int64
	Title      string
	Body       string
	PersonID   int64
	ItemID     int64
	Sender     string // a sender address or domain the note is about, lower case; "" for none
	CreatedBy  string
	CreatedAt  int64
	UpdatedBy  string
	UpdatedAt  int64
	PersonName string // joined
	ItemNumber int64  // joined
}

const noteCols = `n.id, n.title, n.body, COALESCE(n.person_id, 0), COALESCE(n.item_id, 0), n.sender, n.created_by, n.created_at, n.updated_by, n.updated_at,
	COALESCE((SELECT COALESCE(NULLIF(p.call_name, ''), p.name) FROM people p WHERE p.id = n.person_id), ''), COALESCE((SELECT i.number FROM items i WHERE i.id = n.item_id), 0)`

func scanNote(row interface{ Scan(...any) error }) (Note, error) {
	var n Note
	err := row.Scan(&n.ID, &n.Title, &n.Body, &n.PersonID, &n.ItemID, &n.Sender, &n.CreatedBy, &n.CreatedAt, &n.UpdatedBy, &n.UpdatedAt, &n.PersonName, &n.ItemNumber)
	return n, err
}

func nullID(id int64) any {
	if id == 0 {
		return nil
	}
	return id
}

// InsertNote saves a new note made by n.CreatedBy at the time at.
func (q Q) InsertNote(ctx context.Context, n Note, at int64) (int64, error) {
	// A note carries the mark of the item it is attached to, else of the person,
	// else of the call that makes it. A call under a run may attach it only to
	// an item or a person of that run.
	mark, err := q.runForItem(ctx, n.ItemID)
	if err != nil {
		return 0, err
	}
	pmark, err := q.runForRow(ctx, "people", n.PersonID)
	if err != nil {
		return 0, err
	}
	if n.ItemID == 0 {
		mark = pmark
	}
	res, err := q.x.ExecContext(ctx, `INSERT INTO notes (title, body, person_id, item_id, sender, created_by, created_at, updated_by, updated_at, test_run) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		n.Title, n.Body, nullID(n.PersonID), nullID(n.ItemID), n.Sender, n.CreatedBy, at, n.CreatedBy, at, mark)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// UpdateNote changes a note's text and what it belongs to.
func (q Q) UpdateNote(ctx context.Context, n Note, by string, at int64) error {
	if err := q.CheckRow(ctx, "notes", n.ID); err != nil {
		return err
	}
	if _, err := q.runForItem(ctx, n.ItemID); err != nil {
		return err
	}
	if _, err := q.runForRow(ctx, "people", n.PersonID); err != nil {
		return err
	}
	res, err := q.x.ExecContext(ctx, `UPDATE notes SET title=?, body=?, person_id=?, item_id=?, sender=?, updated_by=?, updated_at=? WHERE id=?`,
		n.Title, n.Body, nullID(n.PersonID), nullID(n.ItemID), n.Sender, by, at, n.ID)
	if err != nil {
		return err
	}
	if c, _ := res.RowsAffected(); c == 0 {
		return errors.New("no such note")
	}
	return nil
}

func (q Q) NoteByID(ctx context.Context, id int64) (Note, bool) {
	n, err := scanNote(q.x.QueryRowContext(ctx, `SELECT `+noteCols+` FROM notes n WHERE n.id = ?`, id))
	return n, err == nil
}

// ListNotes lists notes, the one changed last first.
func (q Q) ListNotes(ctx context.Context, limit, offset int) ([]Note, error) {
	rows, err := q.x.QueryContext(ctx, `SELECT `+noteCols+` FROM notes n ORDER BY n.updated_at DESC, n.id DESC LIMIT ? OFFSET ?`, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Note
	for rows.Next() {
		n, err := scanNote(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// NotesOfItem and NotesOfPerson list the notes attached to one of them.
func (q Q) NotesOf(ctx context.Context, column string, id int64) ([]Note, error) {
	if column != "item_id" && column != "person_id" {
		return nil, errors.New("unknown column")
	}
	rows, err := q.x.QueryContext(ctx, `SELECT `+noteCols+` FROM notes n WHERE n.`+column+` = ? ORDER BY n.updated_at DESC, n.id DESC LIMIT 100`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Note
	for rows.Next() {
		n, err := scanNote(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// AllNotes lists up to limit notes, the newest first (what the notes that
// apply to an item are picked from).
func (q Q) AllNotes(ctx context.Context, limit int) ([]Note, error) {
	return q.ListNotes(ctx, limit, 0)
}

// ItemSender is one sender of an item's messages from others.
type ItemSender struct {
	Address string // lower case
	Name    string
}

// SendersOfItem lists the distinct senders of the messages from others that
// the item holds.
func (q Q) SendersOfItem(ctx context.Context, itemID int64) ([]ItemSender, error) {
	rows, err := q.x.QueryContext(ctx, `SELECT lower(sender), MAX(sender_name) FROM messages WHERE item_id=? AND sent_by_me=0 AND sender<>'' GROUP BY lower(sender)`, itemID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ItemSender
	for rows.Next() {
		var s ItemSender
		if err := rows.Scan(&s.Address, &s.Name); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// SendersOfThread is SendersOfItem for one thread of the item (its thread key):
// the guards of a reply look at the thread the reply goes to.
func (q Q) SendersOfThread(ctx context.Context, itemID int64, threadKey string) ([]ItemSender, error) {
	rows, err := q.x.QueryContext(ctx, `SELECT lower(sender), MAX(sender_name) FROM messages WHERE item_id=? AND thread_key=? AND sent_by_me=0 AND sender<>'' GROUP BY lower(sender)`, itemID, threadKey)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ItemSender
	for rows.Next() {
		var s ItemSender
		if err := rows.Scan(&s.Address, &s.Name); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// PersonIDsOfAddresses finds the people who have one of these mail addresses.
func (q Q) PersonIDsOfAddresses(ctx context.Context, addrs []string) ([]int64, error) {
	var out []int64
	seen := map[int64]bool{}
	for _, a := range addrs {
		rows, err := q.x.QueryContext(ctx, `SELECT person_id FROM person_handles WHERE network='mail' AND lower(value)=?`, a)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var id int64
			if rows.Scan(&id) == nil && !seen[id] {
				seen[id] = true
				out = append(out, id)
			}
		}
		rows.Close()
	}
	return out, nil
}

var _ = sql.ErrNoRows
