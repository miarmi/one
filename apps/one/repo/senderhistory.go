package repo

import "context"

// SenderHistory is what ONE knows about a sender before one message: how many
// messages of theirs came earlier (items and the history of the mailbox
// together, so a double count only ever errs against "first message"), and
// whether the owner ever wrote to them.
type SenderHistory struct {
	Earlier    int
	OwnerWrote bool
}

// SenderHistoryOf looks a sender address (lower case) up. msgID and at are the
// message in question: it and anything not older than it do not count as earlier.
func (q Q) SenderHistoryOf(ctx context.Context, sender string, msgID, at int64) SenderHistory {
	var h SenderHistory
	if sender == "" {
		return h
	}
	var n, m int
	q.x.QueryRowContext(ctx, `SELECT COUNT(*) FROM messages WHERE sender = ? AND sent_by_me = 0 AND id <> ? AND received_at < ?`, sender, msgID, at).Scan(&n)
	q.x.QueryRowContext(ctx, `SELECT COUNT(*) FROM mail_history WHERE sender = ? AND mine = 0 AND received_at < ?`, sender, at).Scan(&m)
	h.Earlier = n + m
	var w int
	q.x.QueryRowContext(ctx, `SELECT COUNT(*) FROM messages WHERE sent_by_me = 1 AND instr(lower(to_addrs), ?) > 0`, sender).Scan(&w)
	if w == 0 {
		q.x.QueryRowContext(ctx, `SELECT COUNT(*) FROM mail_history WHERE sender = ? AND answered = 1`, sender).Scan(&w)
	}
	h.OwnerWrote = w > 0
	return h
}
