package repo

import (
	"context"
	"strings"
)

// History in batches (SPEC 34). The old threads are items in the stage
// history (stored with their texts, searchable, never waiting for Claude). A
// thread is done when it has a history_reviewed event; nothing else is kept
// about it here, so a test run's threads and records carry the run's mark like
// every other event and item.

// HistoryScope says which history threads a call looks at.
type HistoryScope struct {
	Run      string // the test run ("" for the owner's real data)
	Before   int64  // only threads whose newest message is older than this (exclusive)
	From     int64  // and not older than this (inclusive); 0 for no limit
	OnlyMine bool   // only threads the owner took part in
}

// HistoryThread is one old thread, as an agent is handed it.
type HistoryThread struct {
	ID       int64
	Number   int64
	Subject  string
	FirstAt  int64
	LastAt   int64
	Messages int
	Chars    int
	Mine     bool // the owner wrote at least one message
	Preview  string
}

const historyEligible = `i.stage = 'history' AND i.kind = 'thread' AND i.hidden = 0 AND i.merged_into = 0
	AND COALESCE(i.test_run, '') = ? AND i.last_message_at < ?
	AND NOT EXISTS (SELECT 1 FROM events e WHERE e.item_id = i.id AND e.type = 'history_reviewed')`

func (sc HistoryScope) where() (string, []any) {
	w, args := historyEligible, []any{sc.Run, sc.Before}
	if sc.From > 0 {
		w += ` AND i.last_message_at >= ?`
		args = append(args, sc.From)
	}
	if sc.OnlyMine {
		w += ` AND EXISTS (SELECT 1 FROM messages m WHERE m.item_id = i.id AND m.sent_by_me = 1)`
	}
	return w, args
}

// HistorySize counts the threads of a scope that are not done yet and the
// characters of their messages.
func (q Q) HistorySize(ctx context.Context, sc HistoryScope) (threads, chars int, err error) {
	w, args := sc.where()
	err = q.x.QueryRowContext(ctx, `SELECT COUNT(*), COALESCE(SUM((SELECT COALESCE(SUM(length(m.text)), 0) FROM messages m WHERE m.item_id = i.id)), 0)
		FROM items i WHERE `+w, args...).Scan(&threads, &chars)
	return
}

// HistoryThreads lists up to limit threads of a scope that are not done,
// newest first (so a batch goes backward in time), after the position of the
// last thread of the page before.
func (q Q) HistoryThreads(ctx context.Context, sc HistoryScope, hasAfter bool, afterAt, afterID int64, limit int) ([]HistoryThread, error) {
	w, args := sc.where()
	if hasAfter {
		w += ` AND (i.last_message_at < ? OR (i.last_message_at = ? AND i.id < ?))`
		args = append(args, afterAt, afterAt, afterID)
	}
	args = append(args, limit)
	rows, err := q.x.QueryContext(ctx, `SELECT i.id, i.number, i.subject, i.first_message_at, i.last_message_at,
		(SELECT COUNT(*) FROM messages m WHERE m.item_id = i.id),
		(SELECT COALESCE(SUM(length(m.text)), 0) FROM messages m WHERE m.item_id = i.id),
		EXISTS (SELECT 1 FROM messages m WHERE m.item_id = i.id AND m.sent_by_me = 1),
		COALESCE((SELECT substr(m.text, 1, 400) FROM messages m WHERE m.item_id = i.id ORDER BY m.received_at DESC, m.id DESC LIMIT 1), '')
		FROM items i WHERE `+w+` ORDER BY i.last_message_at DESC, i.id DESC LIMIT ?`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []HistoryThread
	for rows.Next() {
		var t HistoryThread
		var mine int
		if err := rows.Scan(&t.ID, &t.Number, &t.Subject, &t.FirstAt, &t.LastAt, &t.Messages, &t.Chars, &mine, &t.Preview); err != nil {
			return nil, err
		}
		t.Mine = mine == 1
		out = append(out, t)
	}
	return out, rows.Err()
}

// HistoryPeople lists who wrote in a thread, oldest writer first: a name with
// the address, and "(the owner)" for the messages the owner wrote.
func (q Q) HistoryPeople(ctx context.Context, itemID int64, max int) []string {
	rows, err := q.x.QueryContext(ctx, `SELECT sender, sender_name, MAX(sent_by_me) FROM messages WHERE item_id = ? GROUP BY sender, sender_name ORDER BY MIN(received_at) LIMIT ?`, itemID, max)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var addr, name string
		var mine int
		if rows.Scan(&addr, &name, &mine) != nil {
			continue
		}
		s := strings.TrimSpace(addr)
		if n := strings.TrimSpace(name); n != "" && n != addr {
			s = n + " <" + addr + ">"
		}
		if mine == 1 {
			s += " (the owner)"
		}
		out = append(out, s)
	}
	return out
}

// HistoryPoint is how far back the review has reached: the time of the
// oldest thread recorded as done (false before the first one).
func (q Q) HistoryPoint(ctx context.Context, run string) (int64, bool) {
	var at *int64
	q.x.QueryRowContext(ctx, `SELECT MIN(json_extract(e.data, '$.last_at')) FROM events e WHERE e.type = 'history_reviewed' AND e.test_run = ? AND json_valid(e.data)`, run).Scan(&at)
	if at == nil {
		return 0, false
	}
	return *at, true
}

// HistoryReviewed says whether a thread has been recorded as done.
func (q Q) HistoryReviewed(ctx context.Context, itemID int64) bool {
	var n int
	q.x.QueryRowContext(ctx, `SELECT COUNT(*) FROM events WHERE item_id = ? AND type = 'history_reviewed'`, itemID).Scan(&n)
	return n > 0
}

// HistoryReviewOf is what was recorded for a thread, if anything.
func (q Q) HistoryReviewOf(ctx context.Context, itemID int64) (data string, found bool) {
	err := q.x.QueryRowContext(ctx, `SELECT data FROM events WHERE item_id = ? AND type = 'history_reviewed' ORDER BY id LIMIT 1`, itemID).Scan(&data)
	return data, err == nil
}
