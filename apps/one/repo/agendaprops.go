package repo

import (
	"context"
	"encoding/json"
)

// Proposals about the to-dos of the agenda (SPEC 19): a to-do that belongs to
// no item, or a change to a to-do that is there. They belong to no item, so
// they have a table of their own, like the proposals about people. Nothing
// here ever touches a calendar entry.

type AgendaProposal struct {
	ID        int64
	Action    string // add, edit, split, join, done or drop
	TodoID    int64  // the to-do it is about (the one that stays, for join); 0 for add
	Others    []int64
	Text      string // the new words (add, edit, join); "" leaves them as they are
	Due       string // the new time or due date; "" leaves it, "none" takes it away
	Parts     []AgendaPart
	Reason    string
	By        string
	At        int64
	Status    string // pending, approved, dismissed, withdrawn
	DecidedAt int64
	DecidedBy string
	Note      string
}

// AgendaPart is one of the to-dos a split makes.
type AgendaPart struct {
	Text string `json:"text"`
	Due  string `json:"due,omitempty"`
}

const agendaProposalCols = `id, action, todo_id, others, text, due, parts, reason, proposed_by, proposed_at, status, COALESCE(decided_at,0), decided_by, note`

func scanAgendaProposal(row interface{ Scan(...any) error }) (AgendaProposal, error) {
	var p AgendaProposal
	var others, parts string
	err := row.Scan(&p.ID, &p.Action, &p.TodoID, &others, &p.Text, &p.Due, &parts, &p.Reason, &p.By, &p.At, &p.Status, &p.DecidedAt, &p.DecidedBy, &p.Note)
	if others != "" {
		json.Unmarshal([]byte(others), &p.Others)
	}
	if parts != "" {
		json.Unmarshal([]byte(parts), &p.Parts)
	}
	return p, err
}

// InsertAgendaProposal stores a proposal. Every to-do it names must be of one
// mark (a real one or one run's), which is the proposal's mark.
func (q Q) InsertAgendaProposal(ctx context.Context, p AgendaProposal) (int64, error) {
	mark := RunOf(ctx)
	for _, id := range append([]int64{p.TodoID}, p.Others...) {
		if id == 0 {
			continue
		}
		m, err := q.runForRow(ctx, "agenda", id)
		if err != nil {
			return 0, err
		}
		if m != mark {
			return 0, &RunError{}
		}
	}
	others, parts := "", ""
	if len(p.Others) > 0 {
		b, _ := json.Marshal(p.Others)
		others = string(b)
	}
	if len(p.Parts) > 0 {
		b, _ := json.Marshal(p.Parts)
		parts = string(b)
	}
	res, err := q.x.ExecContext(ctx, `INSERT INTO agenda_proposals (action, todo_id, others, text, due, parts, reason, proposed_by, proposed_at, test_run) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		p.Action, p.TodoID, others, p.Text, p.Due, parts, p.Reason, p.By, p.At, mark)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (q Q) AgendaProposalByID(ctx context.Context, id int64) (AgendaProposal, bool) {
	p, err := scanAgendaProposal(q.x.QueryRowContext(ctx, `SELECT `+agendaProposalCols+` FROM agenda_proposals WHERE id = ?`, id))
	return p, err == nil
}

// AgendaProposalsWaiting lists the waiting proposals, oldest first.
func (q Q) AgendaProposalsWaiting(ctx context.Context, limit int) ([]AgendaProposal, error) {
	rows, err := q.x.QueryContext(ctx, `SELECT `+agendaProposalCols+` FROM agenda_proposals WHERE status = 'pending' ORDER BY id LIMIT ?`, limit)
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

// CountAgendaProposals counts the waiting proposals of the real records, or of
// the run when the context names one.
func (q Q) CountAgendaProposals(ctx context.Context) int {
	var n int
	q.x.QueryRowContext(ctx, `SELECT COUNT(*) FROM agenda_proposals WHERE status = 'pending' AND test_run = ?`, RunOf(ctx)).Scan(&n)
	return n
}

// DecideAgendaProposal moves a waiting proposal to a final status. It says
// false when the proposal was no longer waiting, so a second Approve does
// nothing.
func (q Q) DecideAgendaProposal(ctx context.Context, id int64, status, by, note string, at int64) (bool, error) {
	res, err := q.x.ExecContext(ctx, `UPDATE agenda_proposals SET status = ?, decided_by = ?, note = ?, decided_at = ? WHERE id = ? AND status = 'pending'`, status, by, note, at, id)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}
