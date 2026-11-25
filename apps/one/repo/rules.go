package repo

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
)

// The Import rules section of Settings (SPEC 21) and the proposals of agents
// behind it. A rule is Status "active" (it can be switched on or off), "proposed"
// (it waits for the owner and does nothing) or "dismissed".

const ruleCols = `id, name, enabled, source, field, op, value, created_at, created_by, note, status, reason, proposed_by, proposed_at, decided_at, decided_by`

func scanRule(row interface{ Scan(...any) error }) (Rule, int64, error) {
	var r Rule
	var en int
	var created int64
	err := row.Scan(&r.ID, &r.Name, &en, &r.Source, &r.Field, &r.Op, &r.Value, &created, &r.CreatedBy, &r.Note, &r.Status, &r.Reason, &r.ProposedBy, &r.ProposedAt, &r.DecidedAt, &r.DecidedBy)
	r.Enabled = en == 1
	return r, created, err
}

// RuleWith is a rule and when it was made.
type RuleWith struct {
	Rule
	CreatedAt int64
	Dropped   int // how many messages it dropped
}

// ListRules lists every rule that is active or waiting, in the order they
// apply: active ones first by id, then the proposals.
func (q Q) ListRules(ctx context.Context) ([]RuleWith, error) {
	rows, err := q.x.QueryContext(ctx, `SELECT `+ruleCols+`, (SELECT COUNT(*) FROM skipped s WHERE s.rule_id = rules.id) FROM rules
		WHERE status IN ('active', 'proposed') ORDER BY (status = 'proposed'), id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RuleWith
	for rows.Next() {
		var r Rule
		var en int
		var created int64
		var dropped int
		if err := rows.Scan(&r.ID, &r.Name, &en, &r.Source, &r.Field, &r.Op, &r.Value, &created, &r.CreatedBy, &r.Note, &r.Status, &r.Reason, &r.ProposedBy, &r.ProposedAt, &r.DecidedAt, &r.DecidedBy, &dropped); err != nil {
			return nil, err
		}
		r.Enabled = en == 1
		out = append(out, RuleWith{Rule: r, CreatedAt: created, Dropped: dropped})
	}
	return out, rows.Err()
}

func (q Q) RuleByID(ctx context.Context, id int64) (Rule, bool) {
	r, _, err := scanRule(q.x.QueryRowContext(ctx, `SELECT `+ruleCols+` FROM rules WHERE id=?`, id))
	return r, err == nil
}

// SameRule finds a rule (active or waiting) that says the same.
func (q Q) SameRule(ctx context.Context, source, field, op, value string) (Rule, bool) {
	r, _, err := scanRule(q.x.QueryRowContext(ctx, `SELECT `+ruleCols+` FROM rules WHERE status IN ('active','proposed')
		AND source=? AND field=? AND op=? AND lower(value)=lower(?) ORDER BY id LIMIT 1`, source, field, op, value))
	return r, err == nil
}

// InsertRule adds a rule made by the owner (active) or proposed by an agent
// (waiting, switched off).
func (q Q) InsertRule(ctx context.Context, r Rule, now int64) (int64, error) {
	status, enabled := r.Status, b2i(r.Enabled)
	if status == "" {
		status = "active"
	}
	if status == "proposed" {
		enabled = 0
	}
	res, err := q.x.ExecContext(ctx, `INSERT INTO rules (name, enabled, source, field, op, value, created_at, created_by, note, status, reason, proposed_by, proposed_at, test_run)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		r.Name, enabled, r.Source, r.Field, r.Op, r.Value, now, r.CreatedBy, r.Note, status, r.Reason, r.ProposedBy, r.ProposedAt, RunOf(ctx))
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// UpdateRule changes what the owner can edit of an active rule.
func (q Q) UpdateRule(ctx context.Context, r Rule) error {
	res, err := q.x.ExecContext(ctx, `UPDATE rules SET name=?, source=?, field=?, op=?, value=? WHERE id=? AND status='active'`,
		r.Name, r.Source, r.Field, r.Op, r.Value, r.ID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return errors.New("no such rule")
	}
	return nil
}

// SetRuleEnabled switches an active rule. It says false when nothing changed.
func (q Q) SetRuleEnabled(ctx context.Context, id int64, on bool) (bool, error) {
	res, err := q.x.ExecContext(ctx, `UPDATE rules SET enabled=? WHERE id=? AND status='active' AND enabled<>?`, b2i(on), id, b2i(on))
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

// DecideRule is the owner's Approve (the rule becomes active and enabled) or
// Dismiss on a waiting proposal. It changes nothing and says false when the
// rule was no longer waiting, so a second press does nothing.
func (q Q) DecideRule(ctx context.Context, id int64, approve bool, by string, at int64) (bool, error) {
	status, enabled := "dismissed", 0
	if approve {
		status, enabled = "active", 1
	}
	res, err := q.x.ExecContext(ctx, `UPDATE rules SET status=?, enabled=?, decided_at=?, decided_by=? WHERE id=? AND status='proposed'`, status, enabled, at, by, id)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

// CountRuleProposals counts the rules that wait for Approve.
func (q Q) CountRuleProposals(ctx context.Context) int {
	var n int
	q.x.QueryRowContext(ctx, `SELECT COUNT(*) FROM rules WHERE status='proposed'`).Scan(&n)
	return n
}

// SkippedOfRule lists what a rule dropped, newest first.
func (q Q) SkippedOfRule(ctx context.Context, ruleID int64, limit int) ([]Skipped, error) {
	rows, err := q.x.QueryContext(ctx, `SELECT id, at, source, source_id, sender, subject, received_at, rule_id, reimported_at FROM skipped WHERE rule_id=? ORDER BY received_at DESC, id DESC LIMIT ?`, ruleID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Skipped
	for rows.Next() {
		var s Skipped
		var re sql.NullInt64
		if err := rows.Scan(&s.ID, &s.At, &s.Source, &s.SourceID, &s.Sender, &s.Subject, &s.ReceivedAt, &s.RuleID, &re); err != nil {
			return nil, err
		}
		s.Reimported = re.Valid
		out = append(out, s)
	}
	return out, rows.Err()
}

// SkippedSince lists what a rule dropped since a time.
func (q Q) SkippedSince(ctx context.Context, ruleID, since int64, limit int) ([]Skipped, error) {
	rows, err := q.x.QueryContext(ctx, `SELECT id, at, source, source_id, sender, subject, received_at, rule_id, reimported_at FROM skipped
		WHERE rule_id=? AND received_at>=? ORDER BY received_at DESC, id DESC LIMIT ?`, ruleID, since, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Skipped
	for rows.Next() {
		var s Skipped
		var re sql.NullInt64
		if err := rows.Scan(&s.ID, &s.At, &s.Source, &s.SourceID, &s.Sender, &s.Subject, &s.ReceivedAt, &s.RuleID, &re); err != nil {
			return nil, err
		}
		s.Reimported = re.Valid
		out = append(out, s)
	}
	return out, rows.Err()
}

// StoredMail is what a rule test looks at: a message ONE holds, from someone
// else.
type StoredMail struct {
	Source     string
	Sender     string
	Subject    string
	Labels     []string
	ReceivedAt int64
}

// MailSince lists the newest messages from others received since a time, at
// most limit, for testing a rule against what it would have dropped.
func (q Q) MailSince(ctx context.Context, since int64, limit int) ([]StoredMail, error) {
	rows, err := q.x.QueryContext(ctx, `SELECT source, sender, subject, labels, received_at FROM messages
		WHERE sent_by_me=0 AND network='mail' AND received_at>=? ORDER BY received_at DESC LIMIT ?`, since, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []StoredMail
	for rows.Next() {
		var m StoredMail
		var labels string
		if err := rows.Scan(&m.Source, &m.Sender, &m.Subject, &labels, &m.ReceivedAt); err != nil {
			return nil, err
		}
		json.Unmarshal([]byte(labels), &m.Labels)
		out = append(out, m)
	}
	return out, rows.Err()
}

// CountOpenTodos counts the to-dos not yet ticked off.
func (q Q) CountOpenTodos(ctx context.Context) int {
	var n int
	q.x.QueryRowContext(ctx, `SELECT COUNT(*) FROM agenda WHERE kind='todo' AND state='open'`).Scan(&n)
	return n
}

// RuleEvents lists the events about rules of one type, oldest first (they
// belong to no item).
func (q Q) RuleEvents(ctx context.Context, typ string) ([]Event, error) {
	rows, err := q.x.QueryContext(ctx, `SELECT id, at, 0, 0, actor, type, data FROM events WHERE item_id IS NULL AND type=? ORDER BY id`, typ)
	if err != nil {
		return nil, err
	}
	return scanEvents(rows)
}

// CountRules counts the import rules that apply or wait (SPEC 21), so their
// number can be bounded: every message is compared with each of them.
func (q Q) CountRules(ctx context.Context) int {
	var n int
	q.x.QueryRowContext(ctx, `SELECT COUNT(*) FROM rules WHERE status IN ('active','proposed')`).Scan(&n)
	return n
}
