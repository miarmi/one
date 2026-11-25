package repo

import (
	"context"
)

// --- the owner's picture for a source (SPEC 5, 23) ---

// SetSourcePicture keeps the owner's own picture for a source (replacing the old one).
func (q Q) SetSourcePicture(ctx context.Context, source, contentType string, data []byte, at int64) error {
	_, err := q.x.ExecContext(ctx, `INSERT INTO source_pictures (source, content_type, data, updated_at) VALUES (?, ?, ?, ?)
		ON CONFLICT(source) DO UPDATE SET content_type = excluded.content_type, data = excluded.data, updated_at = excluded.updated_at`, source, contentType, data, at)
	return err
}

func (q Q) RemoveSourcePicture(ctx context.Context, source string) error {
	_, err := q.x.ExecContext(ctx, `DELETE FROM source_pictures WHERE source = ?`, source)
	return err
}

func (q Q) SourcePicture(ctx context.Context, source string) (contentType string, data []byte, at int64, ok bool) {
	err := q.x.QueryRowContext(ctx, `SELECT content_type, data, updated_at FROM source_pictures WHERE source = ?`, source).Scan(&contentType, &data, &at)
	return contentType, data, at, err == nil
}

// SourcePictures lists which sources have a picture, with when it was set.
func (q Q) SourcePictures(ctx context.Context) map[string]int64 {
	out := map[string]int64{}
	rows, err := q.x.QueryContext(ctx, `SELECT source, updated_at FROM source_pictures`)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var s string
		var at int64
		if rows.Scan(&s, &at) == nil {
			out[s] = at
		}
	}
	return out
}

// --- what each source brought in (SPEC 23) ---

// ItemsBySource counts, per message source, the items that got a message stored
// since the time.
func (q Q) ItemsBySource(ctx context.Context, since int64) map[string]int {
	out := map[string]int{}
	rows, err := q.x.QueryContext(ctx, `SELECT m.source, COUNT(DISTINCT m.item_id) FROM messages m JOIN raw r ON r.id = m.raw_id
		WHERE m.test_run = '' AND r.ingested_at >= ? GROUP BY m.source`, since)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var s string
		var n int
		if rows.Scan(&s, &n) == nil {
			out[s] = n
		}
	}
	return out
}

// MessagesBySource counts, per message source, the messages (not the items)
// stored since the time.
func (q Q) MessagesBySource(ctx context.Context, since int64) map[string]int {
	out := map[string]int{}
	rows, err := q.x.QueryContext(ctx, `SELECT m.source, COUNT(*) FROM messages m JOIN raw r ON r.id = m.raw_id
		WHERE m.test_run = '' AND r.ingested_at >= ? GROUP BY m.source`, since)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var s string
		var n int
		if rows.Scan(&s, &n) == nil {
			out[s] = n
		}
	}
	return out
}

// CalendarNewSince sums the calendar entries that were new to the agenda in
// the runs of one source since the time (a calendar run keeps them in items_made).
func (q Q) CalendarNewSince(ctx context.Context, source string, since int64) int {
	var n int
	q.x.QueryRowContext(ctx, `SELECT COALESCE(SUM(items_made), 0) FROM sync_runs WHERE source = ? AND status = 'done' AND started_at >= ?`, source, since).Scan(&n)
	return n
}

// LatestRunSince is the newest run of the source that began at or after the
// time, of any mode.
func (q Q) LatestRunSince(ctx context.Context, source string, since int64) (Run, bool) {
	r, err := scanRun(q.x.QueryRowContext(ctx, `SELECT `+runCols+` FROM sync_runs WHERE source=? AND started_at >= ? ORDER BY id DESC LIMIT 1`, source, since))
	return r, err == nil
}

// DayCount is the items one source brought in on one day.
type DayCount struct {
	Day    int64 // days since 1970 in the zone of the offset given
	Source string
	N      int
}

// ItemsPerDay counts, per day and message source, the items that got a message
// stored since the time. offset is the zone's offset from UTC in seconds.
func (q Q) ItemsPerDay(ctx context.Context, since, offset int64) []DayCount {
	rows, err := q.x.QueryContext(ctx, `SELECT (r.ingested_at + ?) / 86400, m.source, COUNT(DISTINCT m.item_id) FROM messages m JOIN raw r ON r.id = m.raw_id
		WHERE r.ingested_at >= ? GROUP BY 1, 2 ORDER BY 1 DESC`, offset, since)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []DayCount
	for rows.Next() {
		var d DayCount
		if rows.Scan(&d.Day, &d.Source, &d.N) == nil {
			out = append(out, d)
		}
	}
	return out
}

// RunsStoredSince sums what the runs of one source stored (calendar entries)
// or made into items since the time.
func (q Q) RunsStoredSince(ctx context.Context, source string, since int64) int {
	var n int
	q.x.QueryRowContext(ctx, `SELECT COALESCE(SUM(MAX(items_made, stored)), 0) FROM sync_runs WHERE source = ? AND status = 'done' AND started_at >= ?`, source, since).Scan(&n)
	return n
}

// RunsPerDay is RunsStoredSince by day.
func (q Q) RunsPerDay(ctx context.Context, source string, since, offset int64) map[int64]int {
	out := map[int64]int{}
	rows, err := q.x.QueryContext(ctx, `SELECT (started_at + ?) / 86400, COALESCE(SUM(MAX(items_made, stored)), 0) FROM sync_runs WHERE source = ? AND status = 'done' AND started_at >= ? GROUP BY 1`, offset, source, since)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var d int64
		var n int
		if rows.Scan(&d, &n) == nil {
			out[d] = n
		}
	}
	return out
}

// RecentFetches lists the newest runs of every source that ended as done or failed.
func (q Q) RecentFetches(ctx context.Context, limit int) ([]Run, error) {
	rows, err := q.x.QueryContext(ctx, `SELECT `+runCols+` FROM sync_runs WHERE status IN ('done', 'failed') ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Run
	for rows.Next() {
		r, err := scanRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
