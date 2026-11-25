package repo

import (
	"context"
	"sort"
	"strings"
)

// The history of what the owner did with his mail (SPEC 21): one light line per
// message of a whole mailbox, whatever its age. Who wrote, when, the thread,
// whether the owner read it, answered it, starred it, and where it is kept. No
// subject and no text is ever put here: for the years before the import this
// is all ONE knows of a message.

// HistoryRow is one message as the history keeps it.
type HistoryRow struct {
	Source     string // gmail or outlook
	SourceID   string // the source's own id of the message
	Thread     string // the source's thread (Gmail) or conversation (Graph) id
	Sender     string // address, lower case
	SenderName string
	ReceivedAt int64
	Seen       bool
	Starred    bool
	Mine       bool   // the owner wrote it
	State      string // inbox, archived, sent, deleted, spam, folder
}

// DomainOf is the part of an address after the @, lower case.
func DomainOf(addr string) string {
	addr = strings.ToLower(strings.TrimSpace(addr))
	if i := strings.LastIndex(addr, "@"); i >= 0 {
		return addr[i+1:]
	}
	return ""
}

// RecordHistory writes one line. A line that is already there and says the
// same is not written again; a line whose read state, star or place changed is
// brought up to date. The owner answered a message when a message of his own
// follows it in the same thread: the answered mark is kept up to date from
// both sides. changed says whether anything was written.
func (q Q) RecordHistory(ctx context.Context, h HistoryRow, now int64) (changed bool, err error) {
	h.Sender = strings.ToLower(strings.TrimSpace(h.Sender))
	answered := 0
	if !h.Mine && h.Thread != "" {
		err = q.x.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM mail_history WHERE source=? AND thread=? AND mine=1 AND received_at > ?)`,
			h.Source, h.Thread, h.ReceivedAt).Scan(&answered)
		if err != nil {
			return false, err
		}
	}
	res, err := q.x.ExecContext(ctx, `INSERT INTO mail_history (source, source_id, thread, sender, sender_name, domain, received_at, seen, starred, mine, answered, state, recorded_at, test_run)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (source, source_id) DO UPDATE SET seen=excluded.seen, starred=excluded.starred, state=excluded.state
		WHERE mail_history.seen<>excluded.seen OR mail_history.starred<>excluded.starred OR mail_history.state<>excluded.state`,
		h.Source, h.SourceID, h.Thread, h.Sender, h.SenderName, DomainOf(h.Sender), h.ReceivedAt, b2i(h.Seen), b2i(h.Starred), b2i(h.Mine), answered, h.State, now, RunOf(ctx))
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	changed = n > 0
	if h.Mine && h.Thread != "" {
		res, err := q.x.ExecContext(ctx, `UPDATE mail_history SET answered=1 WHERE source=? AND thread=? AND mine=0 AND answered=0 AND received_at < ?`,
			h.Source, h.Thread, h.ReceivedAt)
		if err != nil {
			return changed, err
		}
		if k, _ := res.RowsAffected(); k > 0 {
			changed = true
		}
	}
	return changed, nil
}

// UpdateHistoryState follows a change of read state, star or place of a
// message the history already holds.
func (q Q) UpdateHistoryState(ctx context.Context, source, sourceID string, seen, starred bool, state string) error {
	_, err := q.x.ExecContext(ctx, `UPDATE mail_history SET seen=?, starred=?, state=? WHERE source=? AND source_id=?
		AND (seen<>? OR starred<>? OR state<>?)`, b2i(seen), b2i(starred), state, source, sourceID, b2i(seen), b2i(starred), state)
	return err
}

// HistoryTotals says how many messages the history holds for a source and
// the time of the oldest.
func (q Q) HistoryTotals(ctx context.Context, source string) (n int, oldest int64) {
	var o *int64
	q.x.QueryRowContext(ctx, `SELECT COUNT(*), MIN(received_at) FROM mail_history WHERE source=?`, source).Scan(&n, &o)
	if o != nil {
		oldest = *o
	}
	return
}

// HistoryCount counts all the lines of the history (tests and the page).
func (q Q) HistoryCount(ctx context.Context) int {
	var n int
	q.x.QueryRowContext(ctx, `SELECT COUNT(*) FROM mail_history`).Scan(&n)
	return n
}

// HistoryColumns lists the columns of the history table (a test checks that
// none of them can hold a subject or a text).
func (q Q) HistoryColumns(ctx context.Context) []string {
	rows, err := q.x.QueryContext(ctx, `SELECT name FROM pragma_table_info('mail_history')`)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var c string
		if rows.Scan(&c) == nil {
			out = append(out, c)
		}
	}
	return out
}

// --- numbers per sender ---

// SenderQuery asks for numbers per sender address or per sender domain.
type SenderQuery struct {
	By     string // "address" or "domain"
	Source string // "" for both mailboxes
	Like   string // a part of the address or domain, case-insensitive
	Filter string // "", "never_read_never_answered" or "answered"
	Sort   string // "count", "last" or "first"
	Desc   bool
	Limit  int
	Offset int
	Since  int64 // the start of "the last 12 months"
}

// SenderStat is what the history says about one sender.
type SenderStat struct {
	Key             string
	Messages        int
	FirstAt, LastAt int64
	Read, Starred   int
	Recent          int // messages since SenderQuery.Since
	AnsweredThreads int // threads with a message of this sender that the owner answered
}

func senderKey(by string) string {
	if by == "domain" {
		return "domain"
	}
	return "sender"
}

// SenderStats answers per sender. The owner's own messages are not counted as a
// sender's. No text of any message is read.
func (q Q) SenderStats(ctx context.Context, sq SenderQuery) ([]SenderStat, error) {
	key := senderKey(sq.By)
	where := "mine=0 AND " + key + "<>''"
	var args []any
	args = append(args, sq.Since)
	if sq.Source != "" {
		where += " AND source=?"
		args = append(args, sq.Source)
	}
	if sq.Like != "" {
		where += " AND instr(" + key + ", ?) > 0"
		args = append(args, strings.ToLower(sq.Like))
	}
	having := ""
	switch sq.Filter {
	case "never_read_never_answered":
		having = " HAVING SUM(seen)=0 AND COUNT(DISTINCT CASE WHEN answered=1 THEN source||'|'||thread END)=0"
	case "answered":
		having = " HAVING COUNT(DISTINCT CASE WHEN answered=1 THEN source||'|'||thread END)>=1"
	}
	order := "COUNT(*)"
	switch sq.Sort {
	case "last":
		order = "MAX(received_at)"
	case "first":
		order = "MIN(received_at)"
	}
	dir := "ASC"
	if sq.Desc {
		dir = "DESC"
	}
	args = append(args, sq.Limit, sq.Offset)
	rows, err := q.x.QueryContext(ctx, `SELECT `+key+`, COUNT(*), MIN(received_at), MAX(received_at), SUM(seen), SUM(starred),
		SUM(CASE WHEN received_at >= ? THEN 1 ELSE 0 END),
		COUNT(DISTINCT CASE WHEN answered=1 THEN source||'|'||thread END)
		FROM mail_history WHERE `+where+` GROUP BY `+key+having+` ORDER BY `+order+` `+dir+`, `+key+` LIMIT ? OFFSET ?`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SenderStat
	for rows.Next() {
		var s SenderStat
		if err := rows.Scan(&s.Key, &s.Messages, &s.FirstAt, &s.LastAt, &s.Read, &s.Starred, &s.Recent, &s.AnsweredThreads); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// SenderStatOf is the numbers of one address or domain (empty when none).
func (q Q) SenderStatOf(ctx context.Context, by, key string) (SenderStat, bool) {
	col := senderKey(by)
	var s SenderStat
	err := q.x.QueryRowContext(ctx, `SELECT COUNT(*), COALESCE(MIN(received_at),0), COALESCE(MAX(received_at),0), COALESCE(SUM(seen),0), COALESCE(SUM(starred),0),
		COUNT(DISTINCT CASE WHEN answered=1 THEN source||'|'||thread END)
		FROM mail_history WHERE mine=0 AND `+col+`=?`, strings.ToLower(key)).
		Scan(&s.Messages, &s.FirstAt, &s.LastAt, &s.Read, &s.Starred, &s.AnsweredThreads)
	s.Key = strings.ToLower(key)
	return s, err == nil && s.Messages > 0
}

// SenderTimes lists the times a sender wrote, oldest first, at most limit of
// the newest (what the usual gap is worked out from).
func (q Q) SenderTimes(ctx context.Context, by, key, source string, limit int) ([]int64, error) {
	where, args := "mine=0 AND "+senderKey(by)+"=?", []any{key}
	if source != "" {
		where += " AND source=?"
		args = append(args, source)
	}
	args = append(args, limit)
	rows, err := q.x.QueryContext(ctx, `SELECT received_at FROM mail_history WHERE `+where+` ORDER BY received_at DESC LIMIT ?`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var t int64
		if err := rows.Scan(&t); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out, rows.Err()
}

// SenderLatestName is the name the address was last written with.
func (q Q) SenderLatestName(ctx context.Context, addr string) string {
	var n string
	q.x.QueryRowContext(ctx, `SELECT sender_name FROM mail_history WHERE sender=? AND mine=0 AND sender_name<>'' ORDER BY received_at DESC LIMIT 1`, strings.ToLower(addr)).Scan(&n)
	return n
}

// senderMatch is the condition for a message of the items table's messages
// whose sender is the address or in the domain.
// itemRunIs is true for a message whose item belongs to the run the context
// writes for (a real item when there is none): the numbers behind a skip rule
// never mix real items with a test run's.
func itemRunIs(alias string) string {
	return "(SELECT i.test_run FROM items i WHERE i.id=" + alias + ".item_id) = ?"
}

func senderMatch(by, alias string) string {
	if by == "domain" {
		return "substr(lower(" + alias + ".sender), instr(" + alias + ".sender, '@') + 1) = ?"
	}
	return "lower(" + alias + ".sender) = ?"
}

// AnsweredThreadsIn counts the threads of this sender or domain that the owner
// answered, in the messages ONE holds in full and in the history together.
// A rule is never proposed for a sender where this is not zero.
func (q Q) AnsweredThreadsIn(ctx context.Context, by, key string) (int, error) {
	key = strings.ToLower(strings.TrimSpace(key))
	var inHistory, inItems int
	if err := q.x.QueryRowContext(ctx, `SELECT COUNT(DISTINCT source||'|'||thread) FROM mail_history WHERE mine=0 AND answered=1 AND test_run=? AND `+senderKey(by)+`=?`, RunOf(ctx), key).Scan(&inHistory); err != nil {
		return 0, err
	}
	err := q.x.QueryRowContext(ctx, `SELECT COUNT(DISTINCT m.item_id) FROM messages m WHERE m.sent_by_me=0 AND `+senderMatch(by, "m")+` AND `+itemRunIs("m")+`
		AND EXISTS (SELECT 1 FROM messages p WHERE p.item_id=m.item_id AND p.sent_by_me=1 AND p.received_at > m.received_at)`, key, RunOf(ctx)).Scan(&inItems)
	return max(inHistory, inItems), err
}

// AgentSkips counts the items of a sender or domain that agents skipped, and
// how many of those the owner brought back.
func (q Q) AgentSkips(ctx context.Context, by, key string) (skipped, broughtBack int, err error) {
	err = q.x.QueryRowContext(ctx, `SELECT COUNT(DISTINCT e.item_id),
		COUNT(DISTINCT CASE WHEN EXISTS (SELECT 1 FROM events b WHERE b.item_id=e.item_id AND b.type='brought_back' AND b.actor IN ('owner','nightcheck') AND b.id > e.id) THEN e.item_id END)
		FROM events e WHERE e.type='skipped' AND e.actor<>'owner'
		AND EXISTS (SELECT 1 FROM messages m WHERE m.item_id=e.item_id AND m.sent_by_me=0 AND `+senderMatch(by, "m")+` AND `+itemRunIs("m")+`)`, strings.ToLower(key), RunOf(ctx)).Scan(&skipped, &broughtBack)
	return
}

// HistoryRowOf returns the line the history holds of one message, and
// whether the thread's later message of the owner's marks it answered.
func (q Q) HistoryRowOf(ctx context.Context, source, sourceID string) (h HistoryRow, answered bool, ok bool) {
	var seen, starred, mine, ans int
	err := q.x.QueryRowContext(ctx, `SELECT thread, sender, sender_name, received_at, seen, starred, mine, answered, state FROM mail_history WHERE source=? AND source_id=?`, source, sourceID).
		Scan(&h.Thread, &h.Sender, &h.SenderName, &h.ReceivedAt, &seen, &starred, &mine, &ans, &h.State)
	h.Source, h.SourceID, h.Seen, h.Starred, h.Mine = source, sourceID, seen == 1, starred == 1, mine == 1
	return h, ans == 1, err == nil
}

// ProtectedSender says whether the address, or someone at the domain, is a
// person the owner marked "Always show me", on any network: a chat handle that
// looks like an address counts like a mail address.
func (q Q) ProtectedSender(ctx context.Context, by, key string) (bool, error) {
	key = strings.ToLower(strings.TrimSpace(key))
	cond := "lower(h.value) = ?"
	if by == "domain" {
		cond = "substr(lower(h.value), instr(h.value, '@') + 1) = ?"
	}
	var n int
	err := q.x.QueryRowContext(ctx, `SELECT COUNT(*) FROM person_handles h JOIN people p ON p.id = h.person_id WHERE p.protected=1 AND `+cond, key).Scan(&n)
	return n > 0, err
}
