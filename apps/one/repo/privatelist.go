package repo

import "context"

// What the private list for Work tool reads (SPEC 32). Every query here takes
// real records only (test_run = ''): what a test run made never reaches the
// list. History items, hidden items and rows joined into another are left
// out. With a run name (automated tests only) the same queries read that
// run's records and nothing real; without one, only real records.

// PrivateWaitingItems lists the items of New and In progress that Claude has
// looked at (they no longer wait for Claude) and that are listed for the owner.
func (q Q) PrivateWaitingItems(ctx context.Context, run string) ([]Item, error) {
	rows, err := q.x.QueryContext(ctx, `SELECT `+itemCols+` FROM items
		WHERE stage IN ('new', 'in_progress') AND hidden = 0 AND needs_claude = 0 AND merged_into = 0 AND test_run = ?
		ORDER BY moved_at, id`, run)
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

// PrivateOpenTodos lists the open to-dos that are real.
func (q Q) PrivateOpenTodos(ctx context.Context, run string) ([]Agenda, error) {
	return q.agendaRows(ctx, `kind = 'todo' AND state = 'open' AND test_run = ? ORDER BY created_at, id`, run)
}

// PrivateEvents lists the open calendar entries that start in [from, to),
// real ones only.
func (q Q) PrivateEvents(ctx context.Context, from, to int64, run string) ([]Agenda, error) {
	return q.agendaRows(ctx, `kind = 'event' AND state = 'open' AND status = 'confirmed' AND test_run = ? AND start_at >= ? AND start_at < ? ORDER BY start_at, end_at, id`, run, from, to)
}

// PrivateAgendaProposals lists the waiting proposals about to-dos that are real.
func (q Q) PrivateAgendaProposals(ctx context.Context, limit int, run string) ([]AgendaProposal, error) {
	rows, err := q.x.QueryContext(ctx, `SELECT `+agendaProposalCols+` FROM agenda_proposals WHERE status = 'pending' AND test_run = ? ORDER BY id LIMIT ?`, run, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AgendaProposal
	for rows.Next() {
		p, err := scanAgendaProposal(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}
