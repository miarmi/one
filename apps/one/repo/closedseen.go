package repo

import (
	"context"
	"strconv"
)

// Four eyes on what Claude closed (SPEC 27). Seen is an event of the owner's
// (`closed_seen`) newer than the event that closed the item: closing it again
// later makes a new closing event, so it is not seen again until he looks.
// What was closed before the feature existed counts as seen: the floor is the
// newest event id on the day the app first started with this feature.

// ClosedSummaryKind is the kind of the one row that sums up what Claude closed.
const ClosedSummaryKind = "closedsummary"

// SeenFloorKey is the setting that holds the floor.
const SeenFloorKey = "closed.seen_floor"

const (
	closingIDSQL = `COALESCE((SELECT e.id FROM events e WHERE e.item_id = i.id AND e.type IN ('skipped', 'closed') ORDER BY e.id DESC LIMIT 1), 0)`
	claudeClosed = "i.stage = 'closed' AND " + closingTypeSQL + " = 'skipped' AND " + closingActorSQL + " NOT IN ('rule', 'system')"
	unseenSQL    = closingIDSQL + ` > ? AND NOT EXISTS (SELECT 1 FROM events s WHERE s.item_id = i.id AND s.type = 'closed_seen' AND s.id > ` + closingIDSQL + `)`
)

// ensureSeenFloor sets the floor once, on the first start with this feature.
func ensureSeenFloor(db schemaDB) error {
	_, err := db.Exec(`INSERT OR IGNORE INTO settings (key, value, updated_by, updated_at)
		SELECT ?, CAST(COALESCE(MAX(id), 0) AS TEXT), 'system', strftime('%s','now') FROM events`, SeenFloorKey)
	return err
}

// SeenFloor is the newest event id that counts as already seen.
func (q Q) SeenFloor(ctx context.Context) int64 {
	n, _ := strconv.ParseInt(q.Setting(ctx, SeenFloorKey), 10, 64)
	return n
}

// CountUnseenClosed counts what Claude closed and the owner has not looked at.
func (q Q) CountUnseenClosed(ctx context.Context) int {
	var n int
	q.x.QueryRowContext(ctx, `SELECT COUNT(*) FROM items i WHERE `+claudeClosed+` AND `+unseenSQL, q.SeenFloor(ctx)).Scan(&n)
	return n
}

// UnseenClosedIDs lists the ids of what Claude closed and the owner has not looked
// at, of one run (empty: the real ones), newest closing first.
func (q Q) UnseenClosedIDs(ctx context.Context, run string) ([]int64, error) {
	rows, err := q.x.QueryContext(ctx, `SELECT i.id FROM items i WHERE `+claudeClosed+` AND `+unseenSQL+` AND (? = '*' OR i.test_run = ?) ORDER BY `+closingIDSQL+` DESC`, q.SeenFloor(ctx), run, run)
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

// IsUnseenClosed says whether one item is closed by Claude and not seen.
func (q Q) IsUnseenClosed(ctx context.Context, id int64) bool {
	var n int
	q.x.QueryRowContext(ctx, `SELECT COUNT(*) FROM items i WHERE i.id = ? AND `+claudeClosed+` AND `+unseenSQL, id, q.SeenFloor(ctx)).Scan(&n)
	return n > 0
}

// ClosedSummaryRow finds the open summary row of a run (empty: the real one).
func (q Q) ClosedSummaryRow(ctx context.Context, run string) (Item, bool) {
	rows, err := q.x.QueryContext(ctx, `SELECT `+itemCols+` FROM items WHERE kind = ? AND stage IN ('new', 'in_progress', 'pending') AND test_run = ? ORDER BY id DESC LIMIT 1`, ClosedSummaryKind, run)
	if err != nil {
		return Item{}, false
	}
	defer rows.Close()
	if rows.Next() {
		it, err := scanItem(rows)
		return it, err == nil
	}
	return Item{}, false
}

// NewestEventOf is the newest event of a type on an item.
func (q Q) NewestEventOf(ctx context.Context, itemID int64, typ string) (Event, bool) {
	var e Event
	err := q.x.QueryRowContext(ctx, `SELECT id, at, COALESCE(item_id, 0), actor, type, data FROM events WHERE item_id = ? AND type = ? ORDER BY id DESC LIMIT 1`, itemID, typ).
		Scan(&e.ID, &e.At, &e.Item, &e.Actor, &e.Type, &e.Data)
	return e, err == nil
}

// DayEventCount counts the events of a type of one run that name a day.
func (q Q) DayEventCount(ctx context.Context, typ, day, run string) int {
	var n int
	q.x.QueryRowContext(ctx, `SELECT COUNT(*) FROM events WHERE type = ? AND test_run = ? AND json_valid(data) AND json_extract(data, '$.day') = ?`, typ, run, day).Scan(&n)
	return n
}

// SetItemSubject changes the subject of an item (the summary row's count).
func (t *Tx) SetItemSubject(ctx context.Context, id int64, subject string) error {
	_, err := t.x.ExecContext(ctx, `UPDATE items SET subject = ? WHERE id = ?`, subject, id)
	return err
}

// CountKind counts the items of a kind (tests use it).
func (q Q) CountKind(ctx context.Context, kind string) int {
	var n int
	q.x.QueryRowContext(ctx, `SELECT COUNT(*) FROM items WHERE kind = ?`, kind).Scan(&n)
	return n
}
