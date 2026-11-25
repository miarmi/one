package repo

import (
	"context"
	"database/sql"
	"strconv"
	"strings"
)

// Review by Claude and proposals (SPEC 11, 12). Everything an agent or the owner
// changes here is written next to an event, by the caller, in one transaction.

// ItemByNumber finds an item by the number people read (Q-0001 without the prefix).
// The number of a row that was joined into another leads to the row that stays
// (SPEC 10).
func (q Q) ItemByNumber(ctx context.Context, number int64) (Item, bool) {
	it, err := scanItem(q.x.QueryRowContext(ctx, `SELECT `+itemCols+` FROM items WHERE number=?`, number))
	if err != nil {
		return it, false
	}
	for i := 0; i < 5; i++ {
		to := q.MergedInto(ctx, it.ID)
		if to == 0 {
			break
		}
		next, ok := q.ItemByID(ctx, to)
		if !ok {
			break
		}
		it = next
	}
	return it, true
}

// CountWaiting counts the items that wait for Claude, hidden or shown.
func (q Q) CountWaiting(ctx context.Context) int {
	var n int
	q.x.QueryRowContext(ctx, `SELECT COUNT(*) FROM items WHERE needs_claude = 1`).Scan(&n)
	return n
}

// ProposalCounts counts the proposals waiting for Approve in each stage of
// the list.
func (q Q) ProposalCounts(ctx context.Context) map[string]int {
	out := map[string]int{}
	only, oargs := onlyClause(ctx) // under `Only <connection>` only that connection's rows count (SPEC 23)
	rows, err := q.x.QueryContext(ctx, `SELECT i.stage, COUNT(*) FROM proposals p JOIN items i ON i.id = p.item_id
		WHERE p.status = 'pending' AND i.hidden = 0`+only+` GROUP BY i.stage`, oargs...)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var st string
		var n int
		if rows.Scan(&st, &n) == nil {
			out[st] = n
		}
	}
	return out
}

// ReviewQueue lists the items that wait for Claude, oldest first.
func (q Q) ReviewQueue(ctx context.Context, limit int) ([]ListRow, error) {
	return q.listRows(ctx, "i.needs_claude = 1", nil, "ORDER BY i.last_message_at, i.id LIMIT ? OFFSET ?", limit, 0)
}

// ReviewQueuePage lists up to limit items that wait for Claude in the order of
// ReviewQueue, starting after the position (last message time and id) of an
// item. The position needs no item still in the queue, so a page stays the
// same while items leave it.
func (q Q) ReviewQueuePage(ctx context.Context, hasAfter bool, afterAt, afterID int64, limit int) ([]ListRow, error) {
	where, args := "i.needs_claude = 1", []any(nil)
	if hasAfter {
		where += " AND (i.last_message_at > ? OR (i.last_message_at = ? AND i.id > ?))"
		args = []any{afterAt, afterAt, afterID}
	}
	return q.listRows(ctx, where, args, "ORDER BY i.last_message_at, i.id LIMIT ? OFFSET ?", limit, 0)
}

// NewestText is the first n characters of the text of the item's newest message.
func (q Q) NewestText(ctx context.Context, itemID int64, n int) string {
	var s string
	q.x.QueryRowContext(ctx, `SELECT substr(text, 1, ?) FROM messages WHERE item_id = ? ORDER BY received_at DESC, id DESC LIMIT 1`, n, itemID).Scan(&s)
	return s
}

// ListAny lists a page of any stage, shown items only, the order of the list.
func (q Q) ListAny(ctx context.Context, stage string, limit, offset int) ([]ListRow, error) {
	return q.listRows(ctx, "i.stage = ? AND i.hidden = 0", []any{stage}, "ORDER BY i.moved_at DESC, i.id DESC LIMIT ? OFFSET ?", limit, offset)
}

// AnswerReview is what Claude's look does to an item: it no longer waits for
// Claude, and a new item that was hidden appears in New. revealed says the
// second happened.
func (q Q) AnswerReview(ctx context.Context, id, now int64) (revealed bool, err error) {
	var hid int
	if err = q.x.QueryRowContext(ctx, `SELECT hidden FROM items WHERE id=?`, id).Scan(&hid); err != nil {
		return false, err
	}
	_, err = q.x.ExecContext(ctx, `UPDATE items SET needs_claude=0, hidden=0, reviewed_at=? WHERE id=?`, now, id)
	return hid == 1, err
}

// RevealHidden shows a hidden new item in New without answering it: it keeps
// waiting for an agent on the connector. It is what an item gets when the
// model could not review it (SPEC 31: nothing is skipped because of a failure).
func (q Q) RevealHidden(ctx context.Context, id int64) (bool, error) {
	res, err := q.x.ExecContext(ctx, `UPDATE items SET hidden=0 WHERE id=? AND hidden=1`, id)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

// OpenNote says whether the owner's newest note on the item has had no answer
// from an agent yet.
func (q Q) OpenNote(ctx context.Context, id int64) bool {
	var n int
	q.x.QueryRowContext(ctx, `SELECT COUNT(*) FROM events e WHERE e.item_id=? AND e.type='noted' AND e.actor='owner'
		AND e.id > COALESCE((SELECT MAX(a.id) FROM events a WHERE a.item_id=e.item_id AND a.type IN ('commented','seen','proposal_made','skipped','closed') AND a.actor <> 'owner' AND a.actor <> 'system'), 0)`, id).Scan(&n)
	return n > 0
}

// HasProtectedDirect says whether the item holds a direct message from a
// person the owner marked "Always show me".
func (q Q) HasProtectedDirect(ctx context.Context, id int64) bool {
	var n int
	q.x.QueryRowContext(ctx, `SELECT COUNT(*) FROM messages m
		JOIN person_handles h ON h.network = m.network AND lower(h.value) = lower(m.sender)
		JOIN people p ON p.id = h.person_id
		WHERE m.item_id = ? AND m.sent_by_me = 0 AND m.addressed = 1 AND p.protected = 1`, id).Scan(&n)
	return n > 0
}

// TextLength is the number of characters in the messages of others that the
// item holds (a short matter gets no summary).
func (q Q) TextLength(ctx context.Context, id int64) int {
	var n sql.NullInt64
	q.x.QueryRowContext(ctx, `SELECT SUM(length(text)) FROM messages WHERE item_id=? AND sent_by_me=0`, id).Scan(&n)
	return int(n.Int64)
}

// LastStageEvent is the type and actor of the newest event that closed or
// reopened the item (what pulling back checks).
func (q Q) LastClosing(ctx context.Context, id int64) (typ, actor string) {
	q.x.QueryRowContext(ctx, `SELECT type, actor FROM events WHERE item_id=? AND type IN ('skipped','closed','skip_undone','reopened','unresolved','pending_cleared','opened','brought_back') ORDER BY id DESC LIMIT 1`, id).Scan(&typ, &actor)
	return
}

// --- proposals ---

// Proposal is something an agent wants done that waits for the owner.
type Proposal struct {
	ID        int64
	ItemID    int64
	Action    string // pending, resolve, close or todo
	Text      string // a to-do: what it says
	Reason    string
	By        string // the actor that proposed it
	At        int64
	BaseStage string // the stage the item was in when it was proposed
	BaseMsgAt int64  // its newest message then
	Status    string // pending, approved, dismissed, withdrawn
	DecidedAt int64
	DecidedBy string
	Note      string

	// A reply (SPEC 13) carries more. Text is what Claude proposed; FinalText
	// is what the owner saw in the box when he pressed Approve and what was sent.
	TargetID  int64  // the message that is answered
	Recipient string // JSON: who it goes to, worked out by the server
	FinalText string
	SendState string // "", sending, sent, failed or unknown
	SendAt    int64  // when the send began, then when it ended
	SendRef   string // the Message-ID the app gave the mail (Gmail)
	SendError string
	SentMsgID int64 // the message the import brought back for it

	Due string // a to-do: its time or due date, as the agent gave it ("2026-11-27" or "2026-11-27T14:30"), or ""
}

const proposalCols = `id, item_id, action, reason, proposed_by, proposed_at, base_stage, base_msg_at, status, COALESCE(decided_at,0), decided_by, note, text,
	target_id, recipients, final_text, send_state, send_at, send_ref, send_error, sent_message_id, due`

func scanProposal(row interface{ Scan(...any) error }) (Proposal, error) {
	var p Proposal
	err := row.Scan(&p.ID, &p.ItemID, &p.Action, &p.Reason, &p.By, &p.At, &p.BaseStage, &p.BaseMsgAt, &p.Status, &p.DecidedAt, &p.DecidedBy, &p.Note, &p.Text,
		&p.TargetID, &p.Recipient, &p.FinalText, &p.SendState, &p.SendAt, &p.SendRef, &p.SendError, &p.SentMsgID, &p.Due)
	return p, err
}

func (q Q) InsertProposal(ctx context.Context, p Proposal) (int64, error) {
	mark, err := q.runForItem(ctx, p.ItemID)
	if err != nil {
		return 0, err
	}
	res, err := q.x.ExecContext(ctx, `INSERT INTO proposals (item_id, action, reason, proposed_by, proposed_at, base_stage, base_msg_at, text, target_id, recipients, due, test_run) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		p.ItemID, p.Action, p.Reason, p.By, p.At, p.BaseStage, p.BaseMsgAt, p.Text, p.TargetID, p.Recipient, p.Due, mark)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (q Q) ProposalByID(ctx context.Context, id int64) (Proposal, bool) {
	p, err := scanProposal(q.x.QueryRowContext(ctx, `SELECT `+proposalCols+` FROM proposals WHERE id=?`, id))
	return p, err == nil
}

// ProposalsOfItem lists the item's proposals by id.
func (q Q) ProposalsOfItem(ctx context.Context, itemID int64) (map[int64]Proposal, error) {
	rows, err := q.x.QueryContext(ctx, `SELECT `+proposalCols+` FROM proposals WHERE item_id=? ORDER BY id`, itemID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]Proposal{}
	for rows.Next() {
		p, err := scanProposal(rows)
		if err != nil {
			return nil, err
		}
		out[p.ID] = p
	}
	return out, rows.Err()
}

// PendingProposal finds a waiting proposal of the same action on the item.
func (q Q) PendingProposal(ctx context.Context, itemID int64, action string) (Proposal, bool) {
	p, err := scanProposal(q.x.QueryRowContext(ctx, `SELECT `+proposalCols+` FROM proposals WHERE item_id=? AND action=? AND status='pending' ORDER BY id LIMIT 1`, itemID, action))
	return p, err == nil
}

// PendingTodoProposal finds a waiting to-do proposal with the same text.
func (q Q) PendingTodoProposal(ctx context.Context, itemID int64, text, due string) (Proposal, bool) {
	p, err := scanProposal(q.x.QueryRowContext(ctx, `SELECT `+proposalCols+` FROM proposals WHERE item_id=? AND action='todo' AND text=? AND due=? AND status='pending' ORDER BY id LIMIT 1`, itemID, text, due))
	return p, err == nil
}

// PendingCount counts the proposals waiting on an item.
func (q Q) PendingCount(ctx context.Context, itemID int64) int {
	var n int
	q.x.QueryRowContext(ctx, `SELECT COUNT(*) FROM proposals WHERE item_id=? AND status='pending'`, itemID).Scan(&n)
	return n
}

// DecideProposal moves a waiting proposal to a final status. It changes
// nothing and says false when the proposal was no longer waiting, so a
// second Approve does nothing.
func (q Q) DecideProposal(ctx context.Context, id int64, status, by, note string, at int64) (bool, error) {
	res, err := q.x.ExecContext(ctx, `UPDATE proposals SET status=?, decided_by=?, note=?, decided_at=? WHERE id=? AND status='pending'`, status, by, note, at, id)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

// BeginSend records the intent to send an approved reply: before anything
// leaves the app. It says false when the proposal was already claimed, which
// is how a second Approve, a retry and a second device send nothing.
func (q Q) BeginSend(ctx context.Context, id int64, finalText, ref string, at int64) (bool, error) {
	res, err := q.x.ExecContext(ctx, `UPDATE proposals SET send_state='sending', final_text=?, send_ref=?, send_at=? WHERE id=? AND send_state=''`, finalText, ref, at, id)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

// EndSend records how a send that was begun ended: sent, failed or unknown.
func (q Q) EndSend(ctx context.Context, id int64, state, errText string, at int64) (bool, error) {
	res, err := q.x.ExecContext(ctx, `UPDATE proposals SET send_state=?, send_error=?, send_at=? WHERE id=? AND send_state='sending'`, state, errText, at, id)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

// SendsInFlight lists the replies whose intent was recorded and whose result
// was not (the app stopped in the middle).
func (q Q) SendsInFlight(ctx context.Context) ([]Proposal, error) {
	return q.proposalsWhere(ctx, `send_state='sending'`)
}

// ReplySentFor says whether a reply to this message was already approved and
// sent, is being sent, or may have gone (proposal `except` does not count).
func (q Q) ReplySentFor(ctx context.Context, targetID, except int64) bool {
	var n int
	q.x.QueryRowContext(ctx, `SELECT COUNT(*) FROM proposals WHERE target_id=? AND id<>? AND send_state IN ('sending','queued','handed','sent','unknown')`, targetID, except).Scan(&n)
	return n > 0
}

// UnmatchedSends lists the replies of an item that went out and whose
// message has not come back through the import yet, oldest first.
func (q Q) UnmatchedSends(ctx context.Context, itemID int64) ([]Proposal, error) {
	return q.proposalsWhere(ctx, `item_id=`+strconv.FormatInt(itemID, 10)+` AND send_state IN ('sent','unknown') AND sent_message_id=0`)
}

func (q Q) proposalsWhere(ctx context.Context, where string) ([]Proposal, error) {
	rows, err := q.x.QueryContext(ctx, `SELECT `+proposalCols+` FROM proposals WHERE `+where+` ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Proposal
	for rows.Next() {
		p, err := scanProposal(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// MatchSent ties a reply that went out to the message the import brought
// back; state becomes sent (a mail that "may have gone" is now known to have).
func (q Q) MatchSent(ctx context.Context, id, messageID int64) error {
	_, err := q.x.ExecContext(ctx, `UPDATE proposals SET sent_message_id=?, send_state='sent', send_error='' WHERE id=? AND sent_message_id=0`, messageID, id)
	return err
}

// OwnAddresses lists the addresses the owner's own messages were sent from.
func (q Q) OwnAddresses(ctx context.Context) []string {
	rows, err := q.x.QueryContext(ctx, `SELECT DISTINCT lower(sender) FROM messages WHERE sent_by_me=1 AND sender<>''`)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if rows.Scan(&s) == nil {
			out = append(out, s)
		}
	}
	return out
}

// MessageByID is one message by its number.
func (q Q) MessageByID(ctx context.Context, id int64) (Message, bool) {
	m, err := scanMessage(q.x.QueryRowContext(ctx, `SELECT `+messageCols+` FROM messages WHERE id=?`, id))
	return m, err == nil
}

// NewestMessage is the item's newest message (by time, then number).
func (q Q) NewestMessage(ctx context.Context, itemID int64) (Message, bool) {
	m, err := scanMessage(q.x.QueryRowContext(ctx, `SELECT `+messageCols+` FROM messages WHERE item_id=? ORDER BY received_at DESC, id DESC LIMIT 1`, itemID))
	return m, err == nil
}

// NewestMessageOfThread is the newest message of one thread of an item.
func (q Q) NewestMessageOfThread(ctx context.Context, itemID int64, threadKey string) (Message, bool) {
	m, err := scanMessage(q.x.QueryRowContext(ctx, `SELECT `+messageCols+` FROM messages WHERE item_id=? AND thread_key=? ORDER BY received_at DESC, id DESC LIMIT 1`, itemID, threadKey))
	return m, err == nil
}

// WithdrawPending withdraws every waiting proposal of an item and returns
// their ids.
func (q Q) WithdrawPending(ctx context.Context, itemID int64, note string, at int64) ([]int64, error) {
	rows, err := q.x.QueryContext(ctx, `SELECT id FROM proposals WHERE item_id=? AND status='pending' ORDER BY id`, itemID)
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
		if _, err := q.DecideProposal(ctx, id, "withdrawn", "system", note, at); err != nil {
			return nil, err
		}
		if err := q.InsertEvent(ctx, at, itemID, "system", "proposal_withdrawn", map[string]any{"proposal": id, "why": note}); err != nil {
			return nil, err
		}
	}
	return ids, nil
}

// --- events for agents ---

// Event is one row of an item's record.
type Event struct {
	ID     int64
	At     int64
	Item   int64
	Number int64
	Actor  string
	Type   string
	Data   string
}

// EventsOfItem lists an item's whole record, oldest first, at most limit of
// the newest.
func (q Q) EventsOfItem(ctx context.Context, itemID int64, limit int) ([]Event, error) {
	rows, err := q.x.QueryContext(ctx, `SELECT * FROM (SELECT e.id, e.at, e.item_id, i.number, e.actor, e.type, e.data FROM events e JOIN items i ON i.id = e.item_id
		WHERE (e.item_id=? OR e.item_id IN (SELECT id FROM items WHERE merged_into=?)) ORDER BY e.id DESC LIMIT ?) ORDER BY id`, itemID, itemID, limit)
	if err != nil {
		return nil, err
	}
	return scanEvents(rows)
}

// DecisionTypes are the events that count as a decision of people or agents:
// what Decisions lists (list_recent_decisions) and what get_context gives about
// the same sender or matter.
var DecisionTypes = []string{"proposal_made", "proposal_decided", "proposal_withdrawn", "reply_sent", "reply_failed", "skipped", "skip_undone", "closed", "refused"}

// Decisions lists what people and agents decided since a time, newest first.
func (q Q) Decisions(ctx context.Context, since int64, limit int) ([]Event, error) {
	in := strings.TrimSuffix(strings.Repeat("?,", len(DecisionTypes)), ",")
	args := []any{since}
	for _, t := range DecisionTypes {
		args = append(args, t)
	}
	args = append(args, limit)
	rows, err := q.x.QueryContext(ctx, `SELECT e.id, e.at, e.item_id, i.number, e.actor, e.type, e.data FROM events e JOIN items i ON i.id = e.item_id
		WHERE e.at >= ? AND e.type IN (`+in+`)
		ORDER BY e.id DESC LIMIT ?`, args...)
	if err != nil {
		return nil, err
	}
	return scanEvents(rows)
}

func scanEvents(rows *sql.Rows) ([]Event, error) {
	defer rows.Close()
	var out []Event
	for rows.Next() {
		var e Event
		if err := rows.Scan(&e.ID, &e.At, &e.Item, &e.Number, &e.Actor, &e.Type, &e.Data); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// FileList lists all the files of an item in the order they came.
func (q Q) FileList(ctx context.Context, itemID int64) ([]File, error) {
	by, err := q.FilesOfItem(ctx, itemID)
	if err != nil {
		return nil, err
	}
	var out []File
	for _, fs := range by {
		out = append(out, fs...)
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j].ID < out[j-1].ID; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out, nil
}

// ParseNumber reads "Q-0012" or "12".
func ParseNumber(s string) (int64, bool) {
	s = strings.TrimPrefix(strings.ToUpper(strings.TrimSpace(s)), "Q-")
	n, err := strconv.ParseInt(s, 10, 64)
	return n, err == nil && n > 0
}

// MarkProtected sets "Always show me" on the person who has this handle. The
// People page will call it; the Protected people guard reads it.
func (q Q) MarkProtected(ctx context.Context, network, value string, on bool) error {
	_, err := q.x.ExecContext(ctx, `UPDATE people SET protected=? WHERE id IN (SELECT person_id FROM person_handles WHERE network=? AND lower(value)=lower(?))`, b2i(on), network, value)
	return err
}
