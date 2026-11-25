package repo

import "context"

// What record_feedback reads and keeps (SPEC 29).

// EventByID finds one event of an item.
func (q Q) EventByID(ctx context.Context, id int64) (Event, bool) {
	rows, err := q.x.QueryContext(ctx, `SELECT e.id, e.at, e.item_id, i.number, e.actor, e.type, e.data FROM events e JOIN items i ON i.id = e.item_id WHERE e.id = ?`, id)
	if err != nil {
		return Event{}, false
	}
	evs, err := scanEvents(rows)
	if err != nil || len(evs) == 0 {
		return Event{}, false
	}
	return evs[0], true
}

// FeedbackEvents lists the newest feedback recorded in a run ("" for the real
// records), newest first.
func (q Q) FeedbackEvents(ctx context.Context, run string, limit int) ([]Event, error) {
	rows, err := q.x.QueryContext(ctx, `SELECT e.id, e.at, e.item_id, i.number, e.actor, e.type, e.data FROM events e JOIN items i ON i.id = e.item_id
		WHERE e.type = 'feedback' AND e.test_run = ? ORDER BY e.id DESC LIMIT ?`, run, limit)
	if err != nil {
		return nil, err
	}
	return scanEvents(rows)
}
