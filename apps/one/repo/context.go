package repo

import (
	"context"
	"strings"
)

// What get_context and record_feedback read and keep (SPEC 29).

// RelatedRow is another row that shares something with the one asked about.
type RelatedRow struct {
	ID      int64
	Number  int64
	Subject string
	Stage   string
	LastAt  int64
	Shared  string // the sender address it shares
}

func inList(n int) string { return strings.TrimSuffix(strings.Repeat("?,", n), ",") }

// RelatedBySenders lists the newest rows of the same test run (run "" for the
// real ones) that hold a message from one of the addresses, with the address.
func (q Q) RelatedBySenders(ctx context.Context, itemID int64, run string, addrs []string, limit int) ([]RelatedRow, error) {
	if len(addrs) == 0 {
		return nil, nil
	}
	args := []any{itemID, run}
	for _, a := range addrs {
		args = append(args, strings.ToLower(a))
	}
	args = append(args, limit)
	rows, err := q.x.QueryContext(ctx, `SELECT i.id, i.number, i.subject, i.stage, i.last_message_at, MAX(lower(m.sender))
		FROM items i JOIN messages m ON m.item_id = i.id
		WHERE i.id <> ? AND i.test_run = ? AND i.merged_into = 0 AND m.sent_by_me = 0 AND lower(m.sender) IN (`+inList(len(addrs))+`)
		GROUP BY i.id ORDER BY i.last_message_at DESC, i.id DESC LIMIT ?`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RelatedRow
	for rows.Next() {
		var r RelatedRow
		if err := rows.Scan(&r.ID, &r.Number, &r.Subject, &r.Stage, &r.LastAt, &r.Shared); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// RecentRows lists the newest other rows of the same run, for working out
// which are about the same matter.
func (q Q) RecentRows(ctx context.Context, itemID int64, run string, limit int) ([]RelatedRow, error) {
	rows, err := q.x.QueryContext(ctx, `SELECT id, number, subject, stage, last_message_at FROM items
		WHERE id <> ? AND test_run = ? AND merged_into = 0 AND kind = 'thread' ORDER BY last_message_at DESC, id DESC LIMIT ?`, itemID, run, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RelatedRow
	for rows.Next() {
		var r RelatedRow
		if err := rows.Scan(&r.ID, &r.Number, &r.Subject, &r.Stage, &r.LastAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// PersonMailHandles lists every mail address of the people.
func (q Q) PersonMailHandles(ctx context.Context, ids []int64) ([]string, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	rows, err := q.x.QueryContext(ctx, `SELECT DISTINCT lower(value) FROM person_handles WHERE network='mail' AND person_id IN (`+inList(len(ids))+`)`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}
