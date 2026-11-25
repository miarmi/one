package repo

import (
	"context"
	"database/sql"
)

// Closed items (SPEC 27) and the quiet time before a resolved item closes
// (SPEC 3).

// ClosedCount counts the items in Closed.
func (q Q) ClosedCount(ctx context.Context) int {
	var n int
	q.x.QueryRowContext(ctx, `SELECT COUNT(*) FROM items WHERE stage = 'closed'`).Scan(&n)
	return n
}

// Who closed an item, as the Closed page groups it (SPEC 27).
const (
	ClosedByClaude = "claude" // a clear skip or finished chat by an agent
	ClosedByRule   = "rule"   // a skip by a skip rule or the system
	ClosedElse     = "else"   // what the owner closed or approved to close, and the rest
)

// closingTypeSQL is the type of the newest event that closed the item, and
// closingActorSQL its actor.
const (
	closingTypeSQL  = `COALESCE((SELECT e.type FROM events e WHERE e.item_id = i.id AND e.type IN ('skipped', 'closed') ORDER BY e.id DESC LIMIT 1), '')`
	closingActorSQL = `COALESCE((SELECT e.actor FROM events e WHERE e.item_id = i.id AND e.type IN ('skipped', 'closed') ORDER BY e.id DESC LIMIT 1), '')`
)

// ClosedList lists the closed items of one group, newest first.
func (q Q) ClosedList(ctx context.Context, group string, limit, offset int) ([]ListRow, error) {
	where := "i.stage = 'closed'"
	switch group {
	case ClosedByClaude:
		where += " AND " + closingTypeSQL + " = 'skipped' AND " + closingActorSQL + " NOT IN ('rule', 'system')"
	case ClosedByRule:
		where += " AND " + closingTypeSQL + " = 'skipped' AND " + closingActorSQL + " IN ('rule', 'system')"
	default:
		where += " AND " + closingTypeSQL + " <> 'skipped'"
	}
	return q.listRows(ctx, where, nil, `ORDER BY COALESCE(i.closed_at, i.moved_at) DESC, i.id DESC LIMIT ? OFFSET ?`, limit, offset)
}

// Closing is the newest event that closed an item.
type Closing struct {
	Type  string // skipped or closed
	Actor string
	Data  string
	At    int64
}

// ClosingOf finds who closed an item (ok false when no event says).
func (q Q) ClosingOf(ctx context.Context, itemID int64) (Closing, bool) {
	var c Closing
	err := q.x.QueryRowContext(ctx, `SELECT type, actor, data, at FROM events WHERE item_id = ? AND type IN ('skipped', 'closed') ORDER BY id DESC LIMIT 1`, itemID).Scan(&c.Type, &c.Actor, &c.Data, &c.At)
	return c, err == nil
}

// QuietResolved lists the Resolved items that have had no news since the
// cutoff: neither a message nor the owner's resolving came after it.
func (q Q) QuietResolved(ctx context.Context, cutoff int64, limit int) ([]Item, error) {
	rows, err := q.x.QueryContext(ctx, `SELECT `+itemCols+` FROM items WHERE stage = 'resolved' AND last_message_at < ? AND COALESCE(resolved_at, 0) < ? ORDER BY id LIMIT ?`, cutoff, cutoff, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Item
	for rows.Next() {
		it, err := scanItem(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

// RefusedSince counts how often each guard refused something since a time.
func (q Q) RefusedSince(ctx context.Context, since int64) map[string]int {
	out := map[string]int{}
	rows, err := q.x.QueryContext(ctx, `SELECT json_extract(data, '$.guard'), COUNT(*) FROM events WHERE type = 'refused' AND at >= ? AND json_valid(data) GROUP BY 1`, since)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var g sql.NullString
		var n int
		if rows.Scan(&g, &n) == nil && g.Valid {
			out[g.String] = n
		}
	}
	return out
}

// ProposalsWaiting lists the waiting proposals with their items, oldest first.
func (q Q) ProposalsWaiting(ctx context.Context, limit int) ([]Proposal, map[int64]Item, error) {
	rows, err := q.x.QueryContext(ctx, `SELECT `+proposalCols+` FROM proposals WHERE status = 'pending' ORDER BY id LIMIT ?`, limit)
	if err != nil {
		return nil, nil, err
	}
	var ps []Proposal
	for rows.Next() {
		p, err := scanProposal(rows)
		if err != nil {
			rows.Close()
			return nil, nil, err
		}
		ps = append(ps, p)
	}
	rows.Close()
	items := map[int64]Item{}
	for _, p := range ps {
		if _, ok := items[p.ItemID]; !ok {
			if it, found := q.ItemByID(ctx, p.ItemID); found {
				items[p.ItemID] = it
			}
		}
	}
	return ps, items, rows.Err()
}

// RawEvents lists the data of every event of a type, newest first, whatever
// item it belongs to (tests and the settings record read it).
func (q Q) RawEvents(ctx context.Context, typ string) ([]string, error) {
	rows, err := q.x.QueryContext(ctx, `SELECT data FROM events WHERE type = ? ORDER BY id DESC`, typ)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var d string
		if err := rows.Scan(&d); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// RawEventsFull lists the events of a type, newest first, with who made them
// and whatever item (or none) they belong to. Tests and the people record read it.
func (q Q) RawEventsFull(ctx context.Context, typ string) ([]Event, error) {
	rows, err := q.x.QueryContext(ctx, `SELECT id, at, COALESCE(item_id, 0), actor, type, data FROM events WHERE type = ? ORDER BY id DESC`, typ)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Event
	for rows.Next() {
		var e Event
		if err := rows.Scan(&e.ID, &e.At, &e.Item, &e.Actor, &e.Type, &e.Data); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// SettingBy says who wrote a setting last.
func (q Q) SettingBy(ctx context.Context, key string) string {
	var by string
	q.x.QueryRowContext(ctx, "SELECT updated_by FROM settings WHERE key = ?", key).Scan(&by)
	return by
}

// Exec runs a statement inside the transaction (tests use it to arrange data).
func (t *Tx) Exec(query string, args ...any) (sql.Result, error) {
	return t.tx.Exec(query, args...)
}
