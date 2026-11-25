package repo

import (
	"context"
	"database/sql"
	"errors"
)

// Proposals about people (SPEC 25). They belong to no item, so they have a
// table of their own. Only two things need them: joining two people into one
// ("join") and switching Always show me off ("unprotect").

// PersonProposal is something an agent wants done to People that waits for the owner.
type PersonProposal struct {
	ID        int64
	Action    string // join or unprotect
	PersonID  int64
	OtherID   int64 // join: the second person
	Reason    string
	By        string
	At        int64
	Status    string // pending, approved, dismissed, withdrawn
	DecidedAt int64
	DecidedBy string
	Note      string
}

const personProposalCols = `id, action, person_id, other_id, reason, proposed_by, proposed_at, status, COALESCE(decided_at,0), decided_by, note`

func scanPersonProposal(row interface{ Scan(...any) error }) (PersonProposal, error) {
	var p PersonProposal
	err := row.Scan(&p.ID, &p.Action, &p.PersonID, &p.OtherID, &p.Reason, &p.By, &p.At, &p.Status, &p.DecidedAt, &p.DecidedBy, &p.Note)
	return p, err
}

func (q Q) InsertPersonProposal(ctx context.Context, p PersonProposal) (int64, error) {
	// The proposal belongs to the people it names, who must all be of one mark.
	mark, err := q.runForRow(ctx, "people", p.PersonID)
	if err != nil {
		return 0, err
	}
	if p.OtherID != 0 {
		other, err := q.runForRow(ctx, "people", p.OtherID)
		if err != nil {
			return 0, err
		}
		if other != mark {
			return 0, &RunError{}
		}
	}
	res, err := q.x.ExecContext(ctx, `INSERT INTO person_proposals (action, person_id, other_id, reason, proposed_by, proposed_at, test_run) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		p.Action, p.PersonID, p.OtherID, p.Reason, p.By, p.At, mark)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (q Q) PersonProposalByID(ctx context.Context, id int64) (PersonProposal, bool) {
	p, err := scanPersonProposal(q.x.QueryRowContext(ctx, `SELECT `+personProposalCols+` FROM person_proposals WHERE id = ?`, id))
	return p, err == nil
}

// PendingPersonProposal finds a waiting proposal of the same kind about the
// same person (a join in either order).
func (q Q) PendingPersonProposal(ctx context.Context, action string, a, b int64) (PersonProposal, bool) {
	p, err := scanPersonProposal(q.x.QueryRowContext(ctx, `SELECT `+personProposalCols+` FROM person_proposals
		WHERE status = 'pending' AND action = ? AND ((person_id = ? AND other_id = ?) OR (person_id = ? AND other_id = ?)) ORDER BY id LIMIT 1`, action, a, b, b, a))
	return p, err == nil
}

// PersonProposalsWaiting lists the waiting proposals, oldest first.
func (q Q) PersonProposalsWaiting(ctx context.Context, limit int) ([]PersonProposal, error) {
	rows, err := q.x.QueryContext(ctx, `SELECT `+personProposalCols+` FROM person_proposals WHERE status = 'pending' ORDER BY id LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PersonProposal
	for rows.Next() {
		p, err := scanPersonProposal(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// CountPersonProposals counts the waiting proposals.
func (q Q) CountPersonProposals(ctx context.Context) int {
	var n int
	q.x.QueryRowContext(ctx, `SELECT COUNT(*) FROM person_proposals WHERE status = 'pending'`).Scan(&n)
	return n
}

// DecidePersonProposal moves a waiting proposal to a final status. It changes
// nothing and says false when the proposal was no longer waiting, so a second
// Approve does nothing.
func (q Q) DecidePersonProposal(ctx context.Context, id int64, status, by, note string, at int64) (bool, error) {
	res, err := q.x.ExecContext(ctx, `UPDATE person_proposals SET status = ?, decided_by = ?, note = ?, decided_at = ? WHERE id = ? AND status = 'pending'`, status, by, note, at, id)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

// WithdrawPersonProposalsFor withdraws every waiting proposal that names the
// person and returns their ids.
func (q Q) WithdrawPersonProposalsFor(ctx context.Context, personID int64, note string, at int64) ([]int64, error) {
	rows, err := q.x.QueryContext(ctx, `SELECT id FROM person_proposals WHERE status = 'pending' AND (person_id = ? OR other_id = ?) ORDER BY id`, personID, personID)
	if err != nil {
		return nil, err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	for _, id := range ids {
		if _, err := q.DecidePersonProposal(ctx, id, "withdrawn", "system", note, at); err != nil {
			return nil, err
		}
	}
	return ids, nil
}

// MergePeople joins person drop into person keep: the handles, the notes and,
// when keep has none, the picture move to keep; what keep leaves empty is
// filled from drop; Always show me stays on if either had it. drop is then
// removed. Calling it with the same person twice, or with one that is gone,
// is an error and changes nothing.
func (q Q) MergePeople(ctx context.Context, keep, drop int64, at int64) error {
	if keep == drop {
		return errors.New("a person cannot be joined with themselves")
	}
	// A real person and a test run's person are never joined: the joined
	// person would carry what the other is removed with.
	kr, _ := q.RowRun(ctx, "people", keep)
	dr, _ := q.RowRun(ctx, "people", drop)
	if kr != dr {
		return &RunError{}
	}
	k, ok := q.PersonByID(ctx, keep)
	if !ok {
		return sql.ErrNoRows
	}
	d, ok := q.PersonByID(ctx, drop)
	if !ok {
		return sql.ErrNoRows
	}
	if _, err := q.x.ExecContext(ctx, `UPDATE person_handles SET person_id = ? WHERE person_id = ?`, keep, drop); err != nil {
		return err
	}
	if _, err := q.x.ExecContext(ctx, `UPDATE notes SET person_id = ? WHERE person_id = ?`, keep, drop); err != nil {
		return err
	}
	if k.HasPicture {
		if _, err := q.x.ExecContext(ctx, `DELETE FROM person_pictures WHERE person_id = ?`, drop); err != nil {
			return err
		}
	} else if _, err := q.x.ExecContext(ctx, `UPDATE person_pictures SET person_id = ? WHERE person_id = ?`, keep, drop); err != nil {
		return err
	}
	call, rel := k.CallName, k.Relation
	if call == "" {
		call = d.CallName
	}
	if rel == "" {
		rel = d.Relation
	}
	if _, err := q.x.ExecContext(ctx, `UPDATE people SET call_name = ?, relation = ?, protected = ?, updated_by = 'owner', updated_at = ? WHERE id = ?`,
		call, rel, b2i(k.Protected || d.Protected), at, keep); err != nil {
		return err
	}
	_, err := q.x.ExecContext(ctx, `DELETE FROM people WHERE id = ?`, drop)
	return err
}

// ListPeopleNames is ListPeople for an overview: no handles are loaded.
func (q Q) ListPeopleNames(ctx context.Context, filter string, limit, offset int) ([]Person, error) {
	return q.listPeople(ctx, filter, limit, offset, false)
}
