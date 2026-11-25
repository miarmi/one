package repo

import (
	"context"
)

// Proposals to change a setting (SPEC 22, 12): the value of a setting on the
// Settings page, a guard that can be switched, an entry of the Blocked
// targets list. Stored apart from the settings, they do nothing until the owner
// presses Approve on the signed-in page. Marked like everything a test run
// makes: a run's proposal never changes a real setting.

const settingProposalTable = `
CREATE TABLE IF NOT EXISTS setting_proposals (
	id INTEGER PRIMARY KEY,
	kind TEXT NOT NULL,
	name TEXT NOT NULL,
	label TEXT NOT NULL DEFAULT '',
	from_value TEXT NOT NULL DEFAULT '',
	to_value TEXT NOT NULL DEFAULT '',
	from_text TEXT NOT NULL DEFAULT '',
	to_text TEXT NOT NULL DEFAULT '',
	reason TEXT NOT NULL DEFAULT '',
	proposed_by TEXT NOT NULL,
	proposed_at INTEGER NOT NULL,
	status TEXT NOT NULL DEFAULT 'proposed',
	decided_at INTEGER NOT NULL DEFAULT 0,
	decided_by TEXT NOT NULL DEFAULT '',
	test_run TEXT NOT NULL DEFAULT ''
);
`

// SettingProposal is a change an agent proposed. Kind is setting, guard or
// blocked_target; Name is the setting's key, the guard's key or the entry.
// Status is proposed, approved, dismissed or withdrawn.
type SettingProposal struct {
	ID         int64
	Kind       string
	Name       string
	Label      string
	From, To   string // the values, as stored
	FromText   string // in words, as the page shows them
	ToText     string
	Reason     string
	ProposedBy string
	ProposedAt int64
	Status     string
	DecidedAt  int64
	DecidedBy  string
	TestRun    string
}

const settingProposalCols = `id, kind, name, label, from_value, to_value, from_text, to_text, reason, proposed_by, proposed_at, status, decided_at, decided_by, test_run`

func scanSettingProposal(row interface{ Scan(...any) error }) (SettingProposal, error) {
	var p SettingProposal
	err := row.Scan(&p.ID, &p.Kind, &p.Name, &p.Label, &p.From, &p.To, &p.FromText, &p.ToText, &p.Reason, &p.ProposedBy, &p.ProposedAt, &p.Status, &p.DecidedAt, &p.DecidedBy, &p.TestRun)
	return p, err
}

func (q Q) InsertSettingProposal(ctx context.Context, p SettingProposal) (int64, error) {
	res, err := q.x.ExecContext(ctx, `INSERT INTO setting_proposals (kind, name, label, from_value, to_value, from_text, to_text, reason, proposed_by, proposed_at, test_run) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		p.Kind, p.Name, p.Label, p.From, p.To, p.FromText, p.ToText, p.Reason, p.ProposedBy, p.ProposedAt, RunOf(ctx))
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (q Q) SettingProposalByID(ctx context.Context, id int64) (SettingProposal, bool) {
	p, err := scanSettingProposal(q.x.QueryRowContext(ctx, `SELECT `+settingProposalCols+` FROM setting_proposals WHERE id=?`, id))
	return p, err == nil
}

// WaitingSettingProposals lists the proposals that wait for Approve, oldest first.
func (q Q) WaitingSettingProposals(ctx context.Context) ([]SettingProposal, error) {
	rows, err := q.x.QueryContext(ctx, `SELECT `+settingProposalCols+` FROM setting_proposals WHERE status='proposed' ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SettingProposal
	for rows.Next() {
		p, err := scanSettingProposal(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// SameSettingProposal finds a waiting proposal of the same change made under
// the same run (or none).
func (q Q) SameSettingProposal(ctx context.Context, kind, name, to string) (SettingProposal, bool) {
	p, err := scanSettingProposal(q.x.QueryRowContext(ctx, `SELECT `+settingProposalCols+` FROM setting_proposals WHERE status='proposed' AND kind=? AND name=? AND to_value=? AND test_run=? ORDER BY id LIMIT 1`,
		kind, name, to, RunOf(ctx)))
	return p, err == nil
}

// CountSettingProposals counts the real proposals that wait (a run's are not the owner's).
func (q Q) CountSettingProposals(ctx context.Context) int {
	var n int
	q.x.QueryRowContext(ctx, `SELECT COUNT(*) FROM setting_proposals WHERE status='proposed' AND test_run=''`).Scan(&n)
	return n
}

// CountWaitingSettingProposals counts the proposals that wait under the run of ctx.
func (q Q) CountWaitingSettingProposals(ctx context.Context) int {
	var n int
	q.x.QueryRowContext(ctx, `SELECT COUNT(*) FROM setting_proposals WHERE status='proposed' AND test_run=?`, RunOf(ctx)).Scan(&n)
	return n
}

// DecideSettingProposal sets the status (approved, dismissed or withdrawn);
// it says false when the proposal no longer waits.
func (q Q) DecideSettingProposal(ctx context.Context, id int64, status, by string, at int64) (bool, error) {
	res, err := q.x.ExecContext(ctx, `UPDATE setting_proposals SET status=?, decided_at=?, decided_by=? WHERE id=? AND status='proposed'`, status, at, by, id)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

// LastSettingProposal is the newest decided proposal for a setting, kind and
// name (list_settings says who proposed a value and when the owner approved it).
func (q Q) LastApprovedSettingProposal(ctx context.Context, kind, name string) (SettingProposal, bool) {
	p, err := scanSettingProposal(q.x.QueryRowContext(ctx, `SELECT `+settingProposalCols+` FROM setting_proposals WHERE status='approved' AND kind=? AND name=? AND test_run='' ORDER BY id DESC LIMIT 1`, kind, name))
	return p, err == nil
}
