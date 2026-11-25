package repo

import (
	"context"
	"strings"
)

// Meeting threads (SPEC 33): a row of kind "meeting" tied to a calendar entry,
// or to the whole series of a repeating one. The tie is a link (kind
// "meeting") whose value names the series, so one series has one thread. The
// app never writes to a calendar; nothing here touches an entry.

// MeetingKind is the kind of a row that is a meeting thread.
const MeetingKind = "meeting"

// MeetingLinkKind is the kind of the link between a meeting thread and its entry.
const MeetingLinkKind = "meeting"

// MeetingKey is what ties a thread to the series of an entry: the calendar's
// own id of the event (shared by every occurrence, and by the copies of one
// event in two calendars). An entry that has none is its own series. A test
// run's entry keeps the run's mark in the key, so a run never meets a real
// thread.
func MeetingKey(e Agenda) string {
	id := e.UID
	if id == "" {
		id = "entry:" + e.Source + "\x1f" + e.SourceID
	}
	if e.TestRun != "" {
		return e.TestRun + "\x1f" + id
	}
	return id
}

// splitKey takes a series key apart: the test run it belongs to ("" for a real
// one) and the calendar's id of the event.
func splitKey(key string) (run, id string) {
	if i := strings.Index(key, "\x1f"); i > 0 && ValidRun(key[:i]) {
		return key[:i], key[i+1:]
	}
	return "", key
}

// EventsOfSeries lists the entries of one series (every occurrence, in every
// calendar it is in), by time. A key that was made from an entry without an
// id of the event is found by its source id.
func (q Q) EventsOfSeries(ctx context.Context, key string) ([]Agenda, error) {
	run, id := splitKey(key)
	if rest, ok := strings.CutPrefix(id, "entry:"); ok {
		src, sid, _ := strings.Cut(rest, "\x1f")
		return q.agendaRows(ctx, `kind = 'event' AND uid = '' AND source = ? AND source_id = ? AND test_run = ? ORDER BY start_at, id`, src, sid, run)
	}
	return q.agendaRows(ctx, `kind = 'event' AND uid = ? AND test_run = ? ORDER BY start_at, id`, id, run)
}

// MeetingThreads says, for every thread that stands (one that was not joined
// into another row), its item id and number by series key.
func (q Q) MeetingThreads(ctx context.Context) (map[string][2]int64, error) {
	rows, err := q.x.QueryContext(ctx, `SELECT l.value, i.id, i.number FROM item_links l JOIN items i ON i.id = l.item_id
		WHERE l.kind = ? AND i.merged_into = 0`, MeetingLinkKind)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][2]int64{}
	for rows.Next() {
		var k string
		var v [2]int64
		if err := rows.Scan(&k, &v[0], &v[1]); err != nil {
			return nil, err
		}
		out[k] = v
	}
	return out, rows.Err()
}

// MeetingKeyOfItem is the series key of a meeting thread ("" when it has none).
func (q Q) MeetingKeyOfItem(ctx context.Context, itemID int64) string {
	var k string
	q.x.QueryRowContext(ctx, `SELECT value FROM item_links WHERE item_id = ? AND kind = ? ORDER BY id LIMIT 1`, itemID, MeetingLinkKind).Scan(&k)
	return k
}

// IndexMeeting puts the words of a thread's entry into the search index (SPEC
// 26, results marked Event), in place of what was there. The index is a copy
// made for searching; a trigger removes it with its item.
func (q Q) IndexMeeting(ctx context.Context, itemID int64, title, body string, at int64) error {
	if _, err := q.x.ExecContext(ctx, `DELETE FROM finder WHERE rowid = ?`, itemID*4+3); err != nil {
		return err
	}
	_, err := q.x.ExecContext(ctx, `INSERT INTO finder (rowid, title, body, kind, item, ref, at, who) VALUES (?, ?, ?, 'event', ?, ?, ?, '')`,
		itemID*4+3, title, body, itemID, itemID, at)
	return err
}
