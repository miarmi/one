package repo

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// Skip rules, the app's own review by the Claude API and the night check
// (SPEC 31). Skip rules close an item without the model; the review keeps
// one small job row per item; every call to the API leaves one usage row
// with what it cost; the night check keeps its results.
//
// None of these tables holds message text. Only skip_rules carries the mark of
// a test run: a rule made under a run applies to the items of that run alone,
// is counted with that run and goes with it. The review and the night check
// never look at an item that carries a mark.

const claudeSchema = `
CREATE TABLE IF NOT EXISTS skip_rules (
	id INTEGER PRIMARY KEY,
	name TEXT NOT NULL,
	kind TEXT NOT NULL,
	value TEXT NOT NULL DEFAULT '',
	status TEXT NOT NULL DEFAULT 'proposed',
	enabled INTEGER NOT NULL DEFAULT 0,
	reason TEXT NOT NULL DEFAULT '',
	proposed_by TEXT NOT NULL DEFAULT '',
	proposed_at INTEGER NOT NULL DEFAULT 0,
	decided_at INTEGER NOT NULL DEFAULT 0,
	decided_by TEXT NOT NULL DEFAULT '',
	paused_at INTEGER NOT NULL DEFAULT 0,
	paused_why TEXT NOT NULL DEFAULT '',
	paused_item INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS same_mail (
	second_id INTEGER PRIMARY KEY,
	first_id INTEGER NOT NULL,
	at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS same_mail_first ON same_mail (first_id);
CREATE TABLE IF NOT EXISTS review_jobs (
	item_id INTEGER PRIMARY KEY,
	state TEXT NOT NULL,
	tries INTEGER NOT NULL DEFAULT 0,
	msg_at INTEGER NOT NULL DEFAULT 0,
	note_id INTEGER NOT NULL DEFAULT 0,
	next_at INTEGER NOT NULL DEFAULT 0,
	updated_at INTEGER NOT NULL DEFAULT 0,
	last_error TEXT NOT NULL DEFAULT ''
);
CREATE TABLE IF NOT EXISTS claude_usage (
	id INTEGER PRIMARY KEY,
	at INTEGER NOT NULL,
	day TEXT NOT NULL,
	purpose TEXT NOT NULL,
	ref TEXT NOT NULL DEFAULT '',
	model TEXT NOT NULL,
	input_tokens INTEGER NOT NULL DEFAULT 0,
	output_tokens INTEGER NOT NULL DEFAULT 0,
	micro_usd INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS claude_usage_day ON claude_usage (purpose, day);
CREATE TABLE IF NOT EXISTS night_checks (
	id INTEGER PRIMARY KEY,
	day TEXT NOT NULL,
	started_at INTEGER NOT NULL,
	finished_at INTEGER NOT NULL DEFAULT 0,
	status TEXT NOT NULL,
	model TEXT NOT NULL DEFAULT '',
	checked INTEGER NOT NULL DEFAULT 0,
	brought_back INTEGER NOT NULL DEFAULT 0,
	error TEXT NOT NULL DEFAULT '',
	dismissed INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS night_items (
	id INTEGER PRIMARY KEY,
	check_id INTEGER NOT NULL,
	ref TEXT NOT NULL,
	what TEXT NOT NULL,
	reason TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS messages_header ON messages (header_id) WHERE header_id <> '';
CREATE TRIGGER IF NOT EXISTS finder_event_same_ai AFTER INSERT ON events
WHEN new.item_id IS NOT NULL AND new.type IN ('noted', 'commented') AND json_valid(new.data) AND json_extract(new.data, '$.text') IS NOT NULL
	AND (EXISTS (SELECT 1 FROM same_mail s WHERE s.second_id = new.item_id) OR EXISTS (SELECT 1 FROM same_mail s WHERE s.first_id = new.item_id)) BEGIN
	INSERT INTO finder (rowid, title, body, kind, item, ref, at, who)
	SELECT new.id * 4 + 3, '', json_extract(new.data, '$.text'), 'comment', p.other, new.id, new.at, new.actor
	FROM (SELECT first_id AS other FROM same_mail WHERE second_id = new.item_id UNION ALL SELECT second_id FROM same_mail WHERE first_id = new.item_id LIMIT 1) p;
END;
CREATE TRIGGER IF NOT EXISTS finder_event_same_ad AFTER DELETE ON events
WHEN old.item_id IS NOT NULL AND old.type IN ('noted', 'commented') BEGIN
	DELETE FROM finder WHERE rowid = old.id * 4 + 3;
END;
`

// ensureClaude makes the tables of the review and the skip rules. The finder
// must exist first (the triggers write into it).
func ensureClaude(db schemaDB) error {
	if _, err := db.Exec(claudeSchema); err != nil {
		return err
	}
	skp, err := columns(db, "skipped")
	if err != nil {
		return err
	}
	// A skip rule proposed under a test run carries the run's mark (SPEC 31, API.md "Test runs").
	sr, err := columns(db, "skip_rules")
	if err != nil {
		return err
	}
	if !sr["test_run"] {
		if _, err := db.Exec("ALTER TABLE skip_rules ADD COLUMN test_run TEXT NOT NULL DEFAULT ''"); err != nil {
			return err
		}
	}
	if _, err := db.Exec("CREATE INDEX IF NOT EXISTS skip_rules_test_run ON skip_rules (test_run) WHERE test_run <> ''"); err != nil {
		return err
	}
	// SPEC 10: patterns an agent proposed, marked like everything a run makes.
	if _, err := db.Exec(matterTable); err != nil {
		return err
	}
	mp, err := columns(db, "matter_proposals")
	if err != nil {
		return err
	}
	if !mp["test_run"] {
		if _, err := db.Exec("ALTER TABLE matter_proposals ADD COLUMN test_run TEXT NOT NULL DEFAULT ''"); err != nil {
			return err
		}
	}
	if !mp["would_join"] {
		// How many existing rows the pattern would have joined, counted when it was proposed (-1: not counted).
		if _, err := db.Exec("ALTER TABLE matter_proposals ADD COLUMN would_join INTEGER NOT NULL DEFAULT -1"); err != nil {
			return err
		}
	}
	if _, err := db.Exec(matterIndex); err != nil {
		return err
	}
	if _, err := db.Exec(settingProposalTable); err != nil { // SPEC 22: proposals to change a setting
		return err
	}
	sp, err := columns(db, "setting_proposals")
	if err != nil {
		return err
	}
	if !sp["test_run"] {
		if _, err := db.Exec("ALTER TABLE setting_proposals ADD COLUMN test_run TEXT NOT NULL DEFAULT ''"); err != nil {
			return err
		}
	}
	if _, err := db.Exec("CREATE INDEX IF NOT EXISTS setting_proposals_test_run ON setting_proposals (test_run) WHERE test_run <> ''"); err != nil {
		return err
	}
	if !skp["snippet"] {
		// The beginning of the text of a dropped message, kept so the night check can judge it.
		if _, err := db.Exec("ALTER TABLE skipped ADD COLUMN snippet TEXT NOT NULL DEFAULT ''"); err != nil {
			return err
		}
	}
	return nil
}

// --- skip rules ---

// SkipRule is a rule that closes an item without the model. Status is
// proposed (waits for Approve and does nothing), active or dismissed. An
// active rule that is not enabled and has PausedAt set is Paused.
type SkipRule struct {
	ID         int64
	Name       string
	Kind       string // same_mail, sender or calendar
	Value      string // the sender address or domain, for kind sender
	Status     string
	Enabled    bool
	Reason     string
	ProposedBy string
	ProposedAt int64
	DecidedAt  int64
	DecidedBy  string
	PausedAt   int64
	PausedWhy  string
	PausedItem int64
	TestRun    string // the test run that made the rule; "" for a real one
}

const skipRuleCols = `id, name, kind, value, status, enabled, reason, proposed_by, proposed_at, decided_at, decided_by, paused_at, paused_why, paused_item, test_run`

func scanSkipRule(row interface{ Scan(...any) error }) (SkipRule, error) {
	var r SkipRule
	var en int
	err := row.Scan(&r.ID, &r.Name, &r.Kind, &r.Value, &r.Status, &en, &r.Reason, &r.ProposedBy, &r.ProposedAt, &r.DecidedAt, &r.DecidedBy, &r.PausedAt, &r.PausedWhy, &r.PausedItem, &r.TestRun)
	r.Enabled = en == 1
	return r, err
}

func (q Q) InsertSkipRule(ctx context.Context, r SkipRule) (int64, error) {
	res, err := q.x.ExecContext(ctx, `INSERT INTO skip_rules (name, kind, value, status, enabled, reason, proposed_by, proposed_at, test_run) VALUES (?, ?, ?, 'proposed', 0, ?, ?, ?, ?)`,
		r.Name, r.Kind, strings.ToLower(r.Value), r.Reason, r.ProposedBy, r.ProposedAt, RunOf(ctx))
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (q Q) SkipRuleByID(ctx context.Context, id int64) (SkipRule, bool) {
	r, err := scanSkipRule(q.x.QueryRowContext(ctx, `SELECT `+skipRuleCols+` FROM skip_rules WHERE id=?`, id))
	return r, err == nil
}

// ListSkipRules lists the rules that are active or wait: active ones first.
func (q Q) ListSkipRules(ctx context.Context) ([]SkipRule, error) {
	rows, err := q.x.QueryContext(ctx, `SELECT `+skipRuleCols+` FROM skip_rules WHERE status IN ('active','proposed') ORDER BY (status = 'proposed'), id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SkipRule
	for rows.Next() {
		r, err := scanSkipRule(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// EnabledSkipRules are the rules that apply now.
func (q Q) EnabledSkipRules(ctx context.Context) ([]SkipRule, error) {
	rows, err := q.x.QueryContext(ctx, `SELECT `+skipRuleCols+` FROM skip_rules WHERE status='active' AND enabled=1 ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SkipRule
	for rows.Next() {
		r, err := scanSkipRule(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// SkipRuleKnown finds a rule of this kind and value in any state, a dismissed
// one included: the app proposes a rule of its own kinds only once.
func (q Q) SkipRuleKnown(ctx context.Context, kind, value string) (SkipRule, bool) {
	r, err := scanSkipRule(q.x.QueryRowContext(ctx, `SELECT `+skipRuleCols+` FROM skip_rules WHERE kind=? AND lower(value)=lower(?) AND test_run=? ORDER BY id LIMIT 1`, kind, value, RunOf(ctx)))
	return r, err == nil
}

// SkipRuleLive finds a rule of this kind and value that is active or waits.
func (q Q) SkipRuleLive(ctx context.Context, kind, value string) (SkipRule, bool) {
	r, err := scanSkipRule(q.x.QueryRowContext(ctx, `SELECT `+skipRuleCols+` FROM skip_rules WHERE kind=? AND lower(value)=lower(?) AND status IN ('active','proposed') AND test_run=? ORDER BY id LIMIT 1`, kind, value, RunOf(ctx)))
	return r, err == nil
}

func (q Q) CountSkipRuleProposals(ctx context.Context) int {
	var n int
	q.x.QueryRowContext(ctx, `SELECT COUNT(*) FROM skip_rules WHERE status='proposed' AND test_run=?`, RunOf(ctx)).Scan(&n)
	return n
}

// DecideSkipRule is the owner's Approve or Dismiss on a waiting proposal; false
// when it was no longer waiting.
func (q Q) DecideSkipRule(ctx context.Context, id int64, approve bool, by string, at int64) (bool, error) {
	status, enabled := "dismissed", 0
	if approve {
		status, enabled = "active", 1
	}
	res, err := q.x.ExecContext(ctx, `UPDATE skip_rules SET status=?, enabled=?, decided_at=?, decided_by=? WHERE id=? AND status='proposed'`, status, enabled, at, by, id)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

// SetSkipRuleEnabled is the switch. Switching on clears a pause.
func (q Q) SetSkipRuleEnabled(ctx context.Context, id int64, on bool) (bool, error) {
	res, err := q.x.ExecContext(ctx, `UPDATE skip_rules SET enabled=?, paused_at=CASE WHEN ? THEN 0 ELSE paused_at END WHERE id=? AND status='active' AND (enabled<>? OR (? AND paused_at<>0))`,
		b2i(on), on, id, b2i(on), on)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

// PauseSkipRule switches a rule off at once and keeps what happened.
func (q Q) PauseSkipRule(ctx context.Context, id, at int64, why string, itemID int64) (bool, error) {
	res, err := q.x.ExecContext(ctx, `UPDATE skip_rules SET enabled=0, paused_at=?, paused_why=?, paused_item=? WHERE id=? AND status='active' AND enabled=1`, at, why, itemID, id)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

// SkipRuleClosed counts the items a rule closed.
func (q Q) SkipRuleClosed(ctx context.Context, id int64) int {
	var n int
	q.x.QueryRowContext(ctx, `SELECT COUNT(*) FROM events WHERE type='skipped' AND actor='rule' AND json_extract(data, '$.rule') = ?`, id).Scan(&n)
	return n
}

// --- the same mail in both mailboxes ---

// twinWindow is how far apart the two copies of one mail may have arrived.
const twinWindow = 3 * 24 * 3600

// TwinOf describes the mail whose twin is looked for.
type TwinOf struct {
	HeaderID, Source, Sender, Subject string
	ReceivedAt                        int64
}

// FindTwin finds the earliest stored message of another source that is the
// same mail as m: the same internet message id AND the same sender address
// (and the same subject, apart from Re: and Fwd:, received within a few days).
// The id alone is chosen by the sender and proves nothing. Messages of a test
// run never count.
func (q Q) FindTwin(ctx context.Context, m TwinOf, notItem int64) (itemID int64, ok bool) {
	id := strings.TrimSpace(m.HeaderID)
	if id == "" {
		return 0, false
	}
	rows, err := q.x.QueryContext(ctx, `SELECT m.item_id, m.sender, m.subject, m.received_at FROM messages m JOIN items i ON i.id = m.item_id
		WHERE lower(m.header_id)=lower(?) AND m.source<>? AND m.sent_by_me=0 AND m.item_id<>? AND m.test_run='' AND i.test_run='' ORDER BY m.id LIMIT 20`,
		id, m.Source, notItem)
	if err != nil {
		return 0, false
	}
	defer rows.Close()
	for rows.Next() {
		var item, at int64
		var sender, subject string
		if rows.Scan(&item, &sender, &subject, &at) != nil {
			continue
		}
		d := at - m.ReceivedAt
		if d < 0 {
			d = -d
		}
		if strings.EqualFold(strings.TrimSpace(sender), strings.TrimSpace(m.Sender)) && sameSubject(subject, m.Subject) && d <= twinWindow {
			return item, true
		}
	}
	return 0, false
}

// sameSubject compares two subjects without case, spacing, Re: and Fwd:.
func sameSubject(a, b string) bool { return baseSubject(a) == baseSubject(b) }

func baseSubject(s string) string {
	s = strings.ToLower(strings.Join(strings.Fields(s), " "))
	for {
		t := s
		for _, p := range []string{"re:", "fwd:", "fw:"} {
			t = strings.TrimSpace(strings.TrimPrefix(t, p))
		}
		if t == s {
			return s
		}
		s = t
	}
}

// RecordSameMail notes that the second item holds the same mail as the first,
// and lets what was written on either before be found from both.
func (q Q) RecordSameMail(ctx context.Context, second, first, at int64) error {
	res, err := q.x.ExecContext(ctx, `INSERT INTO same_mail (second_id, first_id, at) VALUES (?, ?, ?) ON CONFLICT DO NOTHING`, second, first, at)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil
	}
	for _, p := range [][2]int64{{second, first}, {first, second}} {
		if _, err := q.x.ExecContext(ctx, `INSERT INTO finder (rowid, title, body, kind, item, ref, at, who)
			SELECT e.id * 4 + 3, '', json_extract(e.data, '$.text'), 'comment', ?, e.id, e.at, e.actor FROM events e
			WHERE e.item_id = ? AND e.type IN ('noted', 'commented') AND json_valid(e.data) AND json_extract(e.data, '$.text') IS NOT NULL`, p[1], p[0]); err != nil {
			return err
		}
	}
	return nil
}

// SameMailOf says which item holds the same mail as this one. second is true
// when this item is the later copy.
func (q Q) SameMailOf(ctx context.Context, itemID int64) (otherNumber int64, second, ok bool) {
	err := q.x.QueryRowContext(ctx, `SELECT i.number FROM same_mail s JOIN items i ON i.id = s.first_id WHERE s.second_id = ?`, itemID).Scan(&otherNumber)
	if err == nil {
		return otherNumber, true, true
	}
	err = q.x.QueryRowContext(ctx, `SELECT i.number FROM same_mail s JOIN items i ON i.id = s.second_id WHERE s.first_id = ?`, itemID).Scan(&otherNumber)
	return otherNumber, false, err == nil
}

func (q Q) CountSameMailSince(ctx context.Context, since int64) int {
	var n int
	q.x.QueryRowContext(ctx, `SELECT COUNT(*) FROM same_mail s JOIN items i ON i.id = s.second_id WHERE s.at >= ? AND i.test_run = ''`, since).Scan(&n)
	return n
}

// CountMailLike counts the mail of others since a time from one sender whose
// subject starts with a text (the marker of a calendar notice).
func (q Q) CountMailLike(ctx context.Context, sender, subjectPrefix string, since int64) int {
	var n int
	q.x.QueryRowContext(ctx, `SELECT COUNT(*) FROM messages WHERE sent_by_me=0 AND network='mail' AND lower(sender)=lower(?) AND lower(substr(subject, 1, ?)) = lower(?) AND received_at >= ? AND test_run = ''`,
		sender, len([]rune(subjectPrefix)), subjectPrefix, since).Scan(&n)
	return n
}

// SkipRuleProtected says whether a skip rule must leave the item alone for
// good: The owner has done something with it (a note, Pending, In progress,
// Resolved, a decision), it has a proposal waiting, or it was brought back (by
// The owner, by the night check, or by importing a dropped message again).
func (q Q) SkipRuleProtected(ctx context.Context, itemID int64) bool {
	var n int
	q.x.QueryRowContext(ctx, `SELECT
		EXISTS (SELECT 1 FROM events WHERE item_id=?1 AND ((actor='owner' AND type<>'setting_changed') OR type='brought_back'))
		OR EXISTS (SELECT 1 FROM proposals WHERE item_id=?1 AND status='pending')
		OR EXISTS (SELECT 1 FROM messages m JOIN skipped s ON s.source=m.source AND s.source_id=m.source_id WHERE m.item_id=?1)`, itemID).Scan(&n)
	return n == 1
}

// --- what a sender's history says about a skip rule ---

// SenderBroughtBack counts the items of a sender or domain that the owner or the
// night check ever brought back.
func (q Q) SenderBroughtBack(ctx context.Context, by, key string) (int, error) {
	var n int
	err := q.x.QueryRowContext(ctx, `SELECT COUNT(DISTINCT e.item_id) FROM events e WHERE e.type='brought_back' AND e.actor IN ('owner','nightcheck')
		AND EXISTS (SELECT 1 FROM messages m WHERE m.item_id=e.item_id AND m.sent_by_me=0 AND `+senderMatch(by, "m")+` AND `+itemRunIs("m")+`)`, strings.ToLower(key), RunOf(ctx)).Scan(&n)
	return n, err
}

// SenderMattered counts the items of a sender or domain that got a comment,
// a summary, a note of the owner's or a proposal: that is, an answer that was not
// a skip, so the mail has mattered at least once.
func (q Q) SenderMattered(ctx context.Context, by, key string) (int, error) {
	var n int
	err := q.x.QueryRowContext(ctx, `SELECT COUNT(DISTINCT e.item_id) FROM events e WHERE e.type IN ('commented','proposal_made','noted','history_attention')
		AND EXISTS (SELECT 1 FROM messages m WHERE m.item_id=e.item_id AND m.sent_by_me=0 AND `+senderMatch(by, "m")+` AND `+itemRunIs("m")+`)`, strings.ToLower(key), RunOf(ctx)).Scan(&n)
	return n, err
}

// --- the review by the app's own model ---

// ReviewCandidate is the next item for the model: waiting for Claude, made by
// no test run, in order of arrival, not already handled for this arrival.
func (q Q) NextReviewItem(ctx context.Context, now int64) (Item, bool) {
	it, err := scanItem(q.x.QueryRowContext(ctx, `SELECT `+prefixed("i.", itemCols)+` FROM items i LEFT JOIN review_jobs j ON j.item_id = i.id
		WHERE i.needs_claude = 1 AND i.test_run = '' AND (j.item_id IS NULL OR j.msg_at < i.last_message_at
			OR j.note_id < COALESCE((SELECT MAX(e.id) FROM events e WHERE e.item_id = i.id AND e.type = 'noted'), 0)
			OR (j.state = 'retry' AND j.next_at <= ?))
		ORDER BY i.last_message_at, i.id LIMIT 1`, now))
	return it, err == nil
}

// WaitingItem is an item waiting for Claude that the review does not take
// now, with the plain reason (numbers and states only).
type WaitingItem struct {
	Number int64
	Why    string
}

// WaitingNotDue lists up to limit items that wait for Claude (outside test
// runs) and that NextReviewItem would not return, oldest first.
func (q Q) WaitingNotDue(ctx context.Context, now int64, limit int) []WaitingItem {
	rows, err := q.x.QueryContext(ctx, `SELECT i.number, COALESCE(j.state, 'no job'), COALESCE(j.msg_at, 0), i.last_message_at, COALESCE(j.next_at, 0), i.hidden, i.stage
		FROM items i LEFT JOIN review_jobs j ON j.item_id = i.id
		WHERE i.needs_claude = 1 AND i.test_run = ''
		AND NOT (j.item_id IS NULL OR j.msg_at < i.last_message_at
			OR j.note_id < COALESCE((SELECT MAX(e.id) FROM events e WHERE e.item_id = i.id AND e.type = 'noted'), 0)
			OR (j.state = 'retry' AND j.next_at <= ?))
		ORDER BY i.last_message_at, i.id LIMIT ?`, now, limit)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []WaitingItem
	for rows.Next() {
		var w WaitingItem
		var state, stage string
		var msgAt, lastAt, nextAt int64
		var hidden int
		if rows.Scan(&w.Number, &state, &msgAt, &lastAt, &nextAt, &hidden, &stage) != nil {
			return out
		}
		w.Why = fmt.Sprintf("job %s, msg_at %d, last_message_at %d, next_at %d, hidden %d, stage %s", state, msgAt, lastAt, nextAt, hidden, stage)
		out = append(out, w)
	}
	return out
}

// ReviewJob is what the app keeps of its own review of one item.
type ReviewJob struct {
	ItemID    int64
	State     string // running, retry, failed or done
	Tries     int
	MsgAt     int64
	NoteID    int64
	NextAt    int64
	LastError string
}

func (q Q) ReviewJobOf(ctx context.Context, itemID int64) (ReviewJob, bool) {
	var j ReviewJob
	err := q.x.QueryRowContext(ctx, `SELECT item_id, state, tries, msg_at, note_id, next_at, last_error FROM review_jobs WHERE item_id=?`, itemID).
		Scan(&j.ItemID, &j.State, &j.Tries, &j.MsgAt, &j.NoteID, &j.NextAt, &j.LastError)
	return j, err == nil
}

// NewestNoteID is the newest note of the owner's on the item (0 when none).
func (q Q) NewestNoteID(ctx context.Context, itemID int64) int64 {
	var n int64
	q.x.QueryRowContext(ctx, `SELECT COALESCE(MAX(id),0) FROM events WHERE item_id=? AND type='noted'`, itemID).Scan(&n)
	return n
}

// ClaimReview marks an item as being handed to the model and says how many
// tries it has already had for this arrival.
func (q Q) ClaimReview(ctx context.Context, it Item, now int64) (tries int, err error) {
	note := q.NewestNoteID(ctx, it.ID)
	j, found := q.ReviewJobOf(ctx, it.ID)
	if found && j.MsgAt >= it.LastAt && j.NoteID >= note {
		tries = j.Tries
	}
	_, err = q.x.ExecContext(ctx, `INSERT INTO review_jobs (item_id, state, tries, msg_at, note_id, next_at, updated_at, last_error) VALUES (?, 'running', ?, ?, ?, 0, ?, '')
		ON CONFLICT(item_id) DO UPDATE SET state='running', tries=excluded.tries, msg_at=excluded.msg_at, note_id=excluded.note_id, next_at=0, updated_at=excluded.updated_at, last_error=''`,
		it.ID, tries, it.LastAt, note, now)
	return tries, err
}

// ResetReview forgets what the app's review knows of an item, so it counts as
// new for the model: a new message from someone else came on it.
func (q Q) ResetReview(ctx context.Context, itemID int64) error {
	_, err := q.x.ExecContext(ctx, `DELETE FROM review_jobs WHERE item_id=?`, itemID)
	return err
}

// EndReview records how the model's turn on an item ended.
func (q Q) EndReview(ctx context.Context, itemID int64, state string, tries int, nextAt int64, errText string, now int64) error {
	_, err := q.x.ExecContext(ctx, `UPDATE review_jobs SET state=?, tries=?, next_at=?, last_error=?, updated_at=? WHERE item_id=?`, state, tries, nextAt, errText, now, itemID)
	return err
}

// RecoverReviews makes an item that was being handled when the app stopped
// ready for another try: a restart resumes the queue.
func (q Q) RecoverReviews(ctx context.Context, now int64) error {
	_, err := q.x.ExecContext(ctx, `UPDATE review_jobs SET state='retry', next_at=?, updated_at=? WHERE state='running'`, now, now)
	return err
}

// --- what the calls cost ---

// AddUsage keeps one call: the tokens it used and what it cost, in
// millionths of a US dollar. ref names the night a night check call belongs to.
func (q Q) AddUsage(ctx context.Context, at int64, day, purpose, ref, model string, in, out int, micro int64) error {
	_, err := q.x.ExecContext(ctx, `INSERT INTO claude_usage (at, day, purpose, ref, model, input_tokens, output_tokens, micro_usd) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		at, day, purpose, ref, model, in, out, micro)
	return err
}

// SpentMicro is what the calls of one purpose cost on a day (review) or in
// one night (night check: ref).
func (q Q) SpentMicro(ctx context.Context, purpose, day, ref string) int64 {
	var n sql.NullInt64
	if purpose == "night" {
		q.x.QueryRowContext(ctx, `SELECT SUM(micro_usd) FROM claude_usage WHERE purpose='night' AND ref=?`, ref).Scan(&n)
	} else {
		q.x.QueryRowContext(ctx, `SELECT SUM(micro_usd) FROM claude_usage WHERE purpose=? AND day=?`, purpose, day).Scan(&n)
	}
	return n.Int64
}

// LastUsageAt is when the last call of a purpose was made (0: never).
func (q Q) LastUsageAt(ctx context.Context, purpose string) int64 {
	var n sql.NullInt64
	q.x.QueryRowContext(ctx, `SELECT MAX(at) FROM claude_usage WHERE purpose=?`, purpose).Scan(&n)
	return n.Int64
}

// --- the night check ---

// NightCheck is one night's result.
type NightCheck struct {
	ID          int64
	Day         string
	StartedAt   int64
	FinishedAt  int64
	Status      string // running, done or failed
	Model       string
	Checked     int
	BroughtBack int
	Error       string
	Dismissed   bool
}

const nightCols = `id, day, started_at, finished_at, status, model, checked, brought_back, error, dismissed`

func scanNight(row interface{ Scan(...any) error }) (NightCheck, error) {
	var n NightCheck
	var d int
	err := row.Scan(&n.ID, &n.Day, &n.StartedAt, &n.FinishedAt, &n.Status, &n.Model, &n.Checked, &n.BroughtBack, &n.Error, &d)
	n.Dismissed = d == 1
	return n, err
}

func (q Q) InsertNightCheck(ctx context.Context, day, model string, at int64) (int64, error) {
	res, err := q.x.ExecContext(ctx, `INSERT INTO night_checks (day, started_at, status, model) VALUES (?, ?, 'running', ?)`, day, at, model)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (q Q) FinishNightCheck(ctx context.Context, n NightCheck) error {
	_, err := q.x.ExecContext(ctx, `UPDATE night_checks SET finished_at=?, status=?, checked=?, brought_back=?, error=? WHERE id=?`, n.FinishedAt, n.Status, n.Checked, n.BroughtBack, n.Error, n.ID)
	return err
}

func (q Q) NightCheckOfDay(ctx context.Context, day string) (NightCheck, bool) {
	n, err := scanNight(q.x.QueryRowContext(ctx, `SELECT `+nightCols+` FROM night_checks WHERE day=? ORDER BY id DESC LIMIT 1`, day))
	return n, err == nil
}

// NightChecksOfDay lists every row of a night, oldest first.
func (q Q) NightChecksOfDay(ctx context.Context, day string) []NightCheck {
	rows, err := q.x.QueryContext(ctx, `SELECT `+nightCols+` FROM night_checks WHERE day=? ORDER BY id`, day)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []NightCheck
	for rows.Next() {
		if n, err := scanNight(rows); err == nil {
			out = append(out, n)
		}
	}
	return out
}

// FailRunningNights ends the rows a restart left `running`: nothing is at work on them.
func (q Q) FailRunningNights(ctx context.Context, now int64, why string) error {
	_, err := q.x.ExecContext(ctx, `UPDATE night_checks SET status='failed', finished_at=?, error=? WHERE status='running'`, now, why)
	return err
}

// LatestNightCheck is the newest night check that has ended.
func (q Q) LatestNightCheck(ctx context.Context) (NightCheck, bool) {
	n, err := scanNight(q.x.QueryRowContext(ctx, `SELECT `+nightCols+` FROM night_checks WHERE status<>'running' ORDER BY id DESC LIMIT 1`))
	return n, err == nil
}

func (q Q) RecentNightChecks(ctx context.Context, limit int) ([]NightCheck, error) {
	rows, err := q.x.QueryContext(ctx, `SELECT `+nightCols+` FROM night_checks WHERE status<>'running' ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []NightCheck
	for rows.Next() {
		n, err := scanNight(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

func (q Q) DismissNightCheck(ctx context.Context, id int64) error {
	_, err := q.x.ExecContext(ctx, `UPDATE night_checks SET dismissed=1 WHERE id=?`, id)
	return err
}

func (q Q) InsertNightItem(ctx context.Context, checkID int64, ref, what, reason string) error {
	_, err := q.x.ExecContext(ctx, `INSERT INTO night_items (check_id, ref, what, reason) VALUES (?, ?, ?, ?)`, checkID, ref, what, reason)
	return err
}

type NightItem struct{ Ref, What, Reason string }

func (q Q) NightItems(ctx context.Context, checkID int64) []NightItem {
	rows, err := q.x.QueryContext(ctx, `SELECT ref, what, reason FROM night_items WHERE check_id=? ORDER BY id`, checkID)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []NightItem
	for rows.Next() {
		var n NightItem
		if rows.Scan(&n.Ref, &n.What, &n.Reason) == nil {
			out = append(out, n)
		}
	}
	return out
}

// NightMessage is one message of the last 24 hours as the night check sees it.
type NightMessage struct {
	ID         int64
	ItemID     int64
	Number     int64
	Source     string
	Network    string
	Sender     string
	Subject    string
	At         int64
	Seen       bool
	Text       string // the beginning, for the caller to clean
	Waiting    bool   // the item still waits for Claude
	Stage      string
	SourceID   string
	DroppedID  int64  // for a dropped message: its line in skipped; 0 otherwise
	RuleName   string // for a dropped message: the import rule
	Reimported bool
}

// NightMessages lists the messages of others received since a time that ONE
// stored, oldest first. Items of a test run are left out.
func (q Q) NightMessages(ctx context.Context, since int64, limit int) ([]NightMessage, error) {
	rows, err := q.x.QueryContext(ctx, `SELECT m.id, m.item_id, i.number, m.source, m.network, m.sender, m.subject, m.received_at, m.seen, substr(m.text, 1, 1500), i.needs_claude, i.stage
		FROM messages m JOIN items i ON i.id = m.item_id
		WHERE m.sent_by_me = 0 AND m.received_at >= ? AND m.test_run = '' AND i.test_run = '' ORDER BY m.received_at, m.id LIMIT ?`, since, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []NightMessage
	for rows.Next() {
		var m NightMessage
		var seen, waiting int
		if err := rows.Scan(&m.ID, &m.ItemID, &m.Number, &m.Source, &m.Network, &m.Sender, &m.Subject, &m.At, &seen, &m.Text, &waiting, &m.Stage); err != nil {
			return nil, err
		}
		m.Seen, m.Waiting = seen == 1, waiting == 1
		out = append(out, m)
	}
	return out, rows.Err()
}

// NightDropped lists the messages that import rules dropped since a time.
func (q Q) NightDropped(ctx context.Context, since int64, limit int) ([]NightMessage, error) {
	rows, err := q.x.QueryContext(ctx, `SELECT s.id, s.source, s.source_id, s.sender, s.subject, s.received_at, s.snippet, r.name, s.reimported_at IS NOT NULL
		FROM skipped s JOIN rules r ON r.id = s.rule_id WHERE s.received_at >= ? AND s.test_run = '' ORDER BY s.received_at, s.id LIMIT ?`, since, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []NightMessage
	for rows.Next() {
		var m NightMessage
		if err := rows.Scan(&m.DroppedID, &m.Source, &m.SourceID, &m.Sender, &m.Subject, &m.At, &m.Text, &m.RuleName, &m.Reimported); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// ItemEvents lists events of many items, oldest first, of the given types.
func (q Q) ItemEvents(ctx context.Context, ids []int64, types ...string) (map[int64][]Event, error) {
	out := map[int64][]Event{}
	if len(ids) == 0 || len(types) == 0 {
		return out, nil
	}
	tin := strings.TrimSuffix(strings.Repeat("?,", len(types)), ",")
	for start := 0; start < len(ids); start += 400 {
		chunk := ids[start:min(start+400, len(ids))]
		in := strings.TrimSuffix(strings.Repeat("?,", len(chunk)), ",")
		args := []any{}
		for _, id := range chunk {
			args = append(args, id)
		}
		for _, t := range types {
			args = append(args, t)
		}
		rows, err := q.x.QueryContext(ctx, `SELECT e.id, e.at, e.item_id, 0, e.actor, e.type, e.data FROM events e WHERE e.item_id IN (`+in+`) AND e.type IN (`+tin+`) ORDER BY e.id`, args...)
		if err != nil {
			return nil, err
		}
		evs, err := scanEvents(rows)
		if err != nil {
			return nil, err
		}
		for _, e := range evs {
			out[e.Item] = append(out[e.Item], e)
		}
	}
	return out, nil
}

// SentAfter says, for each item, the time of the owner's newest own message.
func (q Q) OwnerWroteAt(ctx context.Context, ids []int64) (map[int64]int64, error) {
	out := map[int64]int64{}
	for start := 0; start < len(ids); start += 400 {
		chunk := ids[start:min(start+400, len(ids))]
		in := strings.TrimSuffix(strings.Repeat("?,", len(chunk)), ",")
		args := []any{}
		for _, id := range chunk {
			args = append(args, id)
		}
		rows, err := q.x.QueryContext(ctx, `SELECT item_id, MAX(received_at) FROM messages WHERE sent_by_me=1 AND item_id IN (`+in+`) GROUP BY item_id`, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var id, at int64
			if err := rows.Scan(&id, &at); err != nil {
				rows.Close()
				return nil, err
			}
			out[id] = at
		}
		rows.Close()
	}
	return out, nil
}

// SetSnippet keeps the beginning of the text of a dropped message.
func (q Q) SetSkippedSnippet(ctx context.Context, sourceID, source, snippet string) error {
	_, err := q.x.ExecContext(ctx, `UPDATE skipped SET snippet=? WHERE source=? AND source_id=? AND snippet=''`, snippet, source, sourceID)
	return err
}

// MessageItem finds the item a stored message belongs to.
func (q Q) MessageItem(ctx context.Context, source, sourceID string) (Item, bool) {
	var id int64
	if err := q.x.QueryRowContext(ctx, `SELECT item_id FROM messages WHERE source=? AND source_id=?`, source, sourceID).Scan(&id); err != nil {
		return Item{}, false
	}
	return q.ItemByID(ctx, id)
}

// CountSkipRules counts the skip rules that apply or wait, so their number
// can be bounded.
func (q Q) CountSkipRules(ctx context.Context) int {
	var n int
	q.x.QueryRowContext(ctx, `SELECT COUNT(*) FROM skip_rules WHERE status IN ('active','proposed')`).Scan(&n)
	return n
}
