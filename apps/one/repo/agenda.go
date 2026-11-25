package repo

import (
	"context"
	"database/sql"
)

// The agenda (SPEC 18, 19, 20): calendar entries read from the calendars and
// to-dos. An entry is one row that says how it stands now; every changed copy
// of it is kept as a version in agenda_versions. Times are Unix seconds in
// UTC; an all-day entry also holds its dates, which are what is shown.

// Agenda is one entry: an event of a calendar or a to-do.
type Agenda struct {
	ID        int64
	Kind      string // event or todo
	Source    string // icloud or outlookcal, for an event
	SourceID  string // identity inside the source: calendar, id and instance
	CalKey    string // the calendar it comes from
	Calendar  string // that calendar's name as shown
	UID       string // the calendar's own id of the event, to see one event in two calendars
	Title     string
	Place     string
	Note      string
	Answers   string // what the guests answered; kept, not shown
	StartAt   int64
	EndAt     int64
	AllDay    bool
	StartDate string // all-day: 2026-11-20
	EndDate   string // all-day, the day after the last day
	Status    string // confirmed, cancelled or removed (an event)
	State     string // open or done (ticked off)
	Version   int
	Hash      string
	CreatedBy string // a to-do: owner, claude or agent:<name>
	ItemID    int64  // a to-do made from a row
	CreatedAt int64
	UpdatedAt int64
	DoneAt    int64
	TestRun   string // the mark of a test run's entry; "" for every real one
}

const agendaCols = `id, kind, source, source_id, cal_key, calendar, uid, title, place, note, answers, start_at, end_at, all_day, start_date, end_date,
	status, state, version, hash, created_by, COALESCE(item_id,0), created_at, updated_at, COALESCE(done_at,0), test_run`

func scanAgenda(row interface{ Scan(...any) error }) (Agenda, error) {
	var a Agenda
	var allDay int
	err := row.Scan(&a.ID, &a.Kind, &a.Source, &a.SourceID, &a.CalKey, &a.Calendar, &a.UID, &a.Title, &a.Place, &a.Note, &a.Answers, &a.StartAt, &a.EndAt, &allDay,
		&a.StartDate, &a.EndDate, &a.Status, &a.State, &a.Version, &a.Hash, &a.CreatedBy, &a.ItemID, &a.CreatedAt, &a.UpdatedAt, &a.DoneAt, &a.TestRun)
	a.AllDay = allDay == 1
	return a, err
}

func (q Q) agendaRows(ctx context.Context, where string, args ...any) ([]Agenda, error) {
	rows, err := q.x.QueryContext(ctx, `SELECT `+agendaCols+` FROM agenda WHERE `+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Agenda
	for rows.Next() {
		a, err := scanAgenda(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// AgendaByID finds one entry.
func (q Q) AgendaByID(ctx context.Context, id int64) (Agenda, bool) {
	a, err := scanAgenda(q.x.QueryRowContext(ctx, `SELECT `+agendaCols+` FROM agenda WHERE id = ?`, id))
	return a, err == nil
}

// AgendaBySource finds the entry of an event by its identity in the source.
func (q Q) AgendaBySource(ctx context.Context, source, sourceID string) (Agenda, bool) {
	a, err := scanAgenda(q.x.QueryRowContext(ctx, `SELECT `+agendaCols+` FROM agenda WHERE kind = 'event' AND source = ? AND source_id = ?`, source, sourceID))
	return a, err == nil
}

// InsertAgenda adds an entry and its first version (events only have one).
func (q Q) InsertAgenda(ctx context.Context, a Agenda) (int64, error) {
	mark, err := q.runForItem(ctx, a.ItemID)
	if err != nil {
		return 0, err
	}
	var item any
	if a.ItemID != 0 {
		item = a.ItemID
	}
	res, err := q.x.ExecContext(ctx, `INSERT INTO agenda (kind, source, source_id, cal_key, calendar, uid, title, place, note, answers, start_at, end_at, all_day, start_date, end_date,
		status, state, version, hash, created_by, item_id, created_at, updated_at, test_run) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		a.Kind, a.Source, a.SourceID, a.CalKey, a.Calendar, a.UID, a.Title, a.Place, a.Note, a.Answers, a.StartAt, a.EndAt, b2i(a.AllDay), a.StartDate, a.EndDate,
		a.Status, orOpen(a.State), max(a.Version, 1), a.Hash, a.CreatedBy, item, a.CreatedAt, a.UpdatedAt, mark)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func orOpen(s string) string {
	if s == "" {
		return "open"
	}
	return s
}

// UpdateAgendaEvent writes a new version of an event's content. Whether it
// is ticked off is not part of it and stays as it was.
func (q Q) UpdateAgendaEvent(ctx context.Context, a Agenda) error {
	_, err := q.x.ExecContext(ctx, `UPDATE agenda SET calendar=?, uid=?, title=?, place=?, note=?, answers=?, start_at=?, end_at=?, all_day=?, start_date=?, end_date=?,
		status=?, version=?, hash=?, updated_at=? WHERE id=?`,
		a.Calendar, a.UID, a.Title, a.Place, a.Note, a.Answers, a.StartAt, a.EndAt, b2i(a.AllDay), a.StartDate, a.EndDate, a.Status, a.Version, a.Hash, a.UpdatedAt, a.ID)
	return err
}

// InsertAgendaVersion keeps one version of an entry, with what the source
// delivered.
func (q Q) InsertAgendaVersion(ctx context.Context, id int64, version int, hash, payload string, at, runID int64) error {
	var run any
	if runID != 0 {
		run = runID
	}
	mark, err := q.runForRow(ctx, "agenda", id)
	if err != nil {
		return err
	}
	_, err = q.x.ExecContext(ctx, `INSERT INTO agenda_versions (agenda_id, version, hash, payload, at, run_id, test_run) VALUES (?, ?, ?, ?, ?, ?, ?)`, id, version, hash, payload, at, run, mark)
	return err
}

// AgendaVersions counts the versions an entry has.
func (q Q) AgendaVersions(ctx context.Context, id int64) int {
	var n int
	q.x.QueryRowContext(ctx, `SELECT COUNT(*) FROM agenda_versions WHERE agenda_id = ?`, id).Scan(&n)
	return n
}

// EventsOverlapping lists the events of one calendar that are not removed
// and overlap the time range [from, to).
func (q Q) EventsOverlapping(ctx context.Context, calKey string, from, to int64) ([]Agenda, error) {
	return q.agendaRows(ctx, `kind = 'event' AND cal_key = ? AND status <> 'removed' AND start_at < ? AND (end_at > ? OR (end_at <= start_at AND start_at >= ?))`,
		calKey, to, from, from)
}

// SetAgendaStatus changes how an event stands (confirmed, cancelled, removed).
func (q Q) SetAgendaStatus(ctx context.Context, id int64, status string, now int64) error {
	_, err := q.x.ExecContext(ctx, `UPDATE agenda SET status = ?, updated_at = ? WHERE id = ?`, status, now, id)
	return err
}

// EventsInRange lists the open events that start in [from, to), by time.
func (q Q) EventsInRange(ctx context.Context, from, to int64) ([]Agenda, error) {
	return q.EventsBetween(ctx, from, to, false)
}

// EventsBetween lists the events that start in [from, to), by time; the ones
// ticked off too when includeDone.
func (q Q) EventsBetween(ctx context.Context, from, to int64, includeDone bool) ([]Agenda, error) {
	return q.agendaRows(ctx, `kind = 'event' AND (state = 'open' OR ?) AND start_at >= ? AND start_at < ? ORDER BY start_at, end_at, id`, b2i(includeDone), from, to)
}

// EventsBefore lists the newest open events that start before the time, by time.
func (q Q) EventsBefore(ctx context.Context, before int64, limit int) ([]Agenda, error) {
	rows, err := q.agendaRows(ctx, `id IN (SELECT id FROM agenda WHERE kind = 'event' AND state = 'open' AND start_at < ? ORDER BY start_at DESC, id DESC LIMIT ?) ORDER BY start_at, end_at, id`, before, limit)
	return rows, err
}

// CountEventsBefore counts the open events that start before the time.
func (q Q) CountEventsBefore(ctx context.Context, before int64) int {
	var n int
	q.x.QueryRowContext(ctx, `SELECT COUNT(*) FROM agenda WHERE kind = 'event' AND state = 'open' AND start_at < ?`, before).Scan(&n)
	return n
}

// CountEvents counts the events of a source (all of them when source is empty).
func (q Q) CountEvents(ctx context.Context, source string) int {
	var n int
	q.x.QueryRowContext(ctx, `SELECT COUNT(*) FROM agenda WHERE kind = 'event' AND (? = '' OR source = ?)`, source, source).Scan(&n)
	return n
}

// OpenTodos lists the to-dos not yet ticked off, oldest first.
func (q Q) OpenTodos(ctx context.Context) ([]Agenda, error) {
	return q.agendaRows(ctx, `kind = 'todo' AND state = 'open' ORDER BY created_at, id`)
}

// AllTodos lists every to-do, ticked off or not, oldest first. A to-do that
// was dropped (SPEC 19) is kept in the record but is not listed.
func (q Q) AllTodos(ctx context.Context) ([]Agenda, error) {
	return q.agendaRows(ctx, `kind = 'todo' AND state <> 'dropped' ORDER BY created_at, id`)
}

// UpdateAgendaTodo changes the words and the time of a to-do that is open.
func (q Q) UpdateAgendaTodo(ctx context.Context, id int64, title string, startAt int64, allDay bool, startDate string, now int64) error {
	_, err := q.x.ExecContext(ctx, `UPDATE agenda SET title=?, start_at=?, all_day=?, start_date=?, updated_at=? WHERE id=? AND kind='todo' AND state='open'`,
		title, startAt, b2i(allDay), startDate, now, id)
	return err
}

// DropAgendaTodo takes an open to-do off the list without ticking it off. It
// stays in the record. It says false when the to-do was not open.
func (q Q) DropAgendaTodo(ctx context.Context, id, now int64) (bool, error) {
	res, err := q.x.ExecContext(ctx, `UPDATE agenda SET state='dropped', updated_at=? WHERE id=? AND kind='todo' AND state='open'`, now, id)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

// OpenTodoOfItem finds the to-do made from a row that is not yet ticked off.
func (q Q) OpenTodoOfItem(ctx context.Context, itemID int64) (Agenda, bool) {
	rows, err := q.agendaRows(ctx, `kind = 'todo' AND state = 'open' AND item_id = ? ORDER BY id LIMIT 1`, itemID)
	if err != nil || len(rows) == 0 {
		return Agenda{}, false
	}
	return rows[0], true
}

// SetAgendaDone ticks an entry off. It changes nothing and says false when
// the entry was not open, so a second tap does nothing.
func (q Q) SetAgendaDone(ctx context.Context, id, now int64) (bool, error) {
	res, err := q.x.ExecContext(ctx, `UPDATE agenda SET state = 'done', done_at = ?, updated_at = ? WHERE id = ? AND state = 'open'`, now, now, id)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

// DeleteEventsOf removes the events a source brought, with their versions
// (Disconnect, SPEC 20). It returns how many events went.
func (q Q) DeleteEventsOf(ctx context.Context, source string) (int, error) {
	if _, err := q.x.ExecContext(ctx, `DELETE FROM agenda_versions WHERE agenda_id IN (SELECT id FROM agenda WHERE kind = 'event' AND source = ?)`, source); err != nil {
		return 0, err
	}
	res, err := q.x.ExecContext(ctx, `DELETE FROM agenda WHERE kind = 'event' AND source = ?`, source)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// DeleteEventsOfCalendar removes the events of one calendar (used when the
// calendar is gone from the account).
func (q Q) DeleteEventsOfCalendar(ctx context.Context, calKey string) error {
	if _, err := q.x.ExecContext(ctx, `DELETE FROM agenda_versions WHERE agenda_id IN (SELECT id FROM agenda WHERE kind = 'event' AND cal_key = ?)`, calKey); err != nil {
		return err
	}
	_, err := q.x.ExecContext(ctx, `DELETE FROM agenda WHERE kind = 'event' AND cal_key = ?`, calKey)
	return err
}

// ItemsWithOpenTodo says which of the items have a to-do that is still open.
func (q Q) ItemsWithOpenTodo(ctx context.Context) map[int64]bool {
	out := map[int64]bool{}
	rows, err := q.x.QueryContext(ctx, `SELECT DISTINCT item_id FROM agenda WHERE kind = 'todo' AND state = 'open' AND item_id IS NOT NULL`)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var id sql.NullInt64
		if rows.Scan(&id) == nil && id.Valid {
			out[id.Int64] = true
		}
	}
	return out
}
