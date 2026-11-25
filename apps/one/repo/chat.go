package repo

import (
	"context"
	"database/sql"
	"encoding/json"
	"strconv"
	"strings"
)

// Chats (SPEC 16, 17). The chat bridge on the Mac brings chats and their
// messages; this file holds what the app keeps about a chat itself, the
// quiet-time rule for a chat that is still going on, and the intents of chat
// replies waiting for the Mac.

const chatSchema = `
CREATE TABLE IF NOT EXISTS chats (
	key TEXT PRIMARY KEY,
	network TEXT NOT NULL,
	chat_id TEXT NOT NULL,
	name TEXT NOT NULL,
	is_group INTEGER NOT NULL,
	muted INTEGER NOT NULL,
	participants TEXT NOT NULL,
	item_id INTEGER REFERENCES items(id),
	created_at INTEGER NOT NULL,
	updated_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS chats_item ON chats (item_id);
`

// ChatKey is how a chat is named inside the app: its network and its id.
func ChatKey(network, chatID string) string { return network + "/" + chatID }

// Participant is one person in a chat, as the bridge describes them.
type Participant struct {
	Handle string `json:"handle"`
	Name   string `json:"name"`
	Self   bool   `json:"self,omitempty"`
}

// Chat is what the app keeps about one chat.
type Chat struct {
	Key          string
	Network      string
	ChatID       string
	Name         string
	Group        bool
	Muted        bool
	Participants []Participant
	ItemID       int64
	UpdatedAt    int64
	TestRun      string // the mark of a chat a test run made ("" for every real one)
}

// Same says whether two descriptions of a chat say the same.
func (c Chat) Same(o Chat) bool {
	a, _ := json.Marshal(c.Participants)
	b, _ := json.Marshal(o.Participants)
	return c.Name == o.Name && c.Group == o.Group && c.Muted == o.Muted && string(a) == string(b)
}

func scanChat(row interface{ Scan(...any) error }) (Chat, error) {
	var c Chat
	var grp, muted int
	var parts string
	var item sql.NullInt64
	err := row.Scan(&c.Key, &c.Network, &c.ChatID, &c.Name, &grp, &muted, &parts, &item, &c.UpdatedAt, &c.TestRun)
	c.Group, c.Muted, c.ItemID = grp == 1, muted == 1, item.Int64
	json.Unmarshal([]byte(parts), &c.Participants)
	return c, err
}

const chatCols = `key, network, chat_id, name, is_group, muted, participants, item_id, updated_at, test_run`

// ChatByKey finds a chat by its network and id.
func (q Q) ChatByKey(ctx context.Context, key string) (Chat, bool) {
	c, err := scanChat(q.x.QueryRowContext(ctx, `SELECT `+chatCols+` FROM chats WHERE key=?`, key))
	return c, err == nil
}

// ChatOfItem is the chat an item is made of. A row that joined two chats
// answers the chat of its newest chat message (the one the owner's own box writes
// to); a row with no message from a chat yet, the chat touched last.
func (q Q) ChatOfItem(ctx context.Context, itemID int64) (Chat, bool) {
	c, err := scanChat(q.x.QueryRowContext(ctx, `SELECT `+chatCols+` FROM chats WHERE item_id=? ORDER BY
		CASE WHEN 'chat:' || key = (SELECT thread_key FROM messages WHERE item_id=? AND network <> 'mail' ORDER BY received_at DESC, id DESC LIMIT 1) THEN 0 ELSE 1 END,
		updated_at DESC LIMIT 1`, itemID, itemID))
	return c, err == nil
}

// ChatOfThread is the chat a chat thread (its thread key, "chat:<key>") of an
// item belongs to.
func (q Q) ChatOfThread(ctx context.Context, itemID int64, threadKey string) (Chat, bool) {
	if !strings.HasPrefix(threadKey, "chat:") {
		return Chat{}, false
	}
	c, err := scanChat(q.x.QueryRowContext(ctx, `SELECT `+chatCols+` FROM chats WHERE item_id=? AND key=?`, itemID, strings.TrimPrefix(threadKey, "chat:")))
	return c, err == nil
}

// SaveChat writes what is known of a chat. It writes nothing when the chat is
// already stored the same way, and says whether it wrote.
func (q Q) SaveChat(ctx context.Context, c Chat, now int64) (bool, error) {
	if old, ok := q.ChatByKey(ctx, c.Key); ok {
		if old.Same(c) && (c.ItemID == 0 || old.ItemID == c.ItemID) {
			return false, nil
		}
		if c.ItemID == 0 {
			c.ItemID = old.ItemID
		}
		parts, _ := json.Marshal(c.Participants)
		var item any
		if c.ItemID != 0 {
			item = c.ItemID
		}
		_, err := q.x.ExecContext(ctx, `UPDATE chats SET name=?, is_group=?, muted=?, participants=?, item_id=?, updated_at=? WHERE key=?`,
			c.Name, b2i(c.Group), b2i(c.Muted), string(parts), item, now, c.Key)
		return err == nil, err
	}
	parts, _ := json.Marshal(c.Participants)
	var item any
	if c.ItemID != 0 {
		item = c.ItemID
	}
	_, err := q.x.ExecContext(ctx, `INSERT INTO chats (key, network, chat_id, name, is_group, muted, participants, item_id, created_at, updated_at, test_run) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		c.Key, c.Network, c.ChatID, c.Name, b2i(c.Group), b2i(c.Muted), string(parts), item, now, now, RunOf(ctx))
	return err == nil, err
}

// CountChats counts the chats the bridge has brought.
func (q Q) CountChats(ctx context.Context) int {
	var n int
	q.x.QueryRowContext(ctx, "SELECT COUNT(*) FROM chats WHERE test_run = ''").Scan(&n)
	return n
}

// CountChatMessages counts the chat messages ONE holds.
func (q Q) CountChatMessages(ctx context.Context) int {
	var n int
	q.x.QueryRowContext(ctx, "SELECT COUNT(*) FROM messages WHERE test_run = '' AND network <> 'mail'").Scan(&n)
	return n
}

// ChatMessagesSince counts the chat messages that first arrived after a time.
func (q Q) ChatMessagesSince(ctx context.Context, since int64) int {
	var n int
	q.x.QueryRowContext(ctx, `SELECT COUNT(*) FROM raw WHERE test_run = '' AND source NOT IN ('gmail', 'outlook') AND version_key LIKE '%@0' AND version_key NOT LIKE 'removed@%' AND ingested_at > ?`, since).Scan(&n)
	return n
}

// ChatItemsSince counts the chat items made after a time.
func (q Q) ChatItemsSince(ctx context.Context, since int64) int {
	var n int
	q.x.QueryRowContext(ctx, `SELECT COUNT(*) FROM chats WHERE test_run = '' AND created_at > ?`, since).Scan(&n)
	return n
}

// --- the quiet time (SPEC 17) ---

// HasQuietChats says whether a chat is ready to go to Claude, without taking
// the write lock.
func (q Q) HasQuietChats(ctx context.Context, before int64) bool {
	var n int
	q.x.QueryRowContext(ctx, `SELECT COUNT(*) FROM items WHERE chat_since > 0 AND chat_since <= ?`, before).Scan(&n)
	return n > 0
}

// ReleaseQuietChats sends to Claude every chat nobody has written in since
// the given time, once for the whole exchange. It returns the items it
// released.
func (q Q) ReleaseQuietChats(ctx context.Context, before int64) ([]int64, error) {
	rows, err := q.x.QueryContext(ctx, `SELECT id FROM items WHERE chat_since > 0 AND chat_since <= ? ORDER BY id`, before)
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
		if _, err := q.x.ExecContext(ctx, `UPDATE items SET needs_claude=1, chat_since=0 WHERE id=? AND chat_since > 0`, id); err != nil {
			return nil, err
		}
	}
	return ids, nil
}

// UpdateChatMessage follows an edit, a reaction or a read mark onto the one
// message, pointing it at the newest raw version.
func (q Q) UpdateChatMessage(ctx context.Context, id, rawID int64, text string, edited bool, reactions []string, seen bool) error {
	l, _ := json.Marshal(reactions)
	_, err := q.x.ExecContext(ctx, `UPDATE messages SET raw_id=?, text=?, edited=?, labels=?, seen=? WHERE id=?`, rawID, text, b2i(edited), string(l), b2i(seen), id)
	return err
}

// --- skipped chat messages: Import again (SPEC 21) ---

// RequestAgain notes that the owner asked for a dropped chat message to be
// imported again; the bridge is told on its next visit.
func (q Q) RequestAgain(ctx context.Context, id, now int64) error {
	_, err := q.x.ExecContext(ctx, `UPDATE skipped SET again_at=? WHERE id=? AND reimported_at IS NULL`, now, id)
	return err
}

// AgainRequests lists the dropped chat messages the owner wants back.
func (q Q) AgainRequests(ctx context.Context, limit int) ([]Skipped, error) {
	rows, err := q.x.QueryContext(ctx, `SELECT id, at, source, source_id, sender, subject, received_at, rule_id, reimported_at FROM skipped
		WHERE again_at > 0 AND reimported_at IS NULL AND source NOT IN ('gmail', 'outlook') ORDER BY again_at, id LIMIT ?`, limit)
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

// --- chat replies waiting for the Mac (SPEC 13) ---
//
// The send state of a chat reply goes: queued (the owner approved; the intent is
// written) -> handed (the bridge took it; never handed out again) -> sent,
// failed or unknown. A queued reply can be withdrawn; a handed one cannot.

const ChatReply = "reply_chat"

// QueueSend turns a reply whose intent was just written into one that waits
// for the bridge.
func (q Q) QueueSend(ctx context.Context, id int64) (bool, error) {
	res, err := q.x.ExecContext(ctx, `UPDATE proposals SET send_state='queued' WHERE id=? AND action=? AND send_state='sending'`, id, ChatReply)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

// ClaimQueued hands out the chat replies that wait, oldest first. Each is
// handed out once: the claim is one conditional write, so a reply that was
// withdrawn meanwhile, or that another request took, is not in the answer.
func (q Q) ClaimQueued(ctx context.Context, limit int, now int64) ([]Proposal, error) {
	cands, err := q.proposalsWhere(ctx, `action='reply_chat' AND send_state='queued'`)
	if err != nil {
		return nil, err
	}
	var out []Proposal
	for _, p := range cands {
		if len(out) >= limit {
			break
		}
		res, err := q.x.ExecContext(ctx, `UPDATE proposals SET send_state='handed', send_at=? WHERE id=? AND send_state='queued'`, now, p.ID)
		if err != nil {
			return nil, err
		}
		if n, _ := res.RowsAffected(); n == 1 {
			p.SendState, p.SendAt = "handed", now
			out = append(out, p)
		}
	}
	return out, nil
}

// WithdrawQueued takes back a chat reply that has not been handed out. It
// says false when it was already handed out (or is not a queued chat reply).
func (q Q) WithdrawQueued(ctx context.Context, id int64, now int64) (bool, error) {
	res, err := q.x.ExecContext(ctx, `UPDATE proposals SET status='withdrawn', send_state='withdrawn', decided_at=?, note='you withdrew it' WHERE id=? AND action=? AND send_state='queued'`, now, id, ChatReply)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

// EndChatSend records what the bridge reported for a handed-out reply. A
// report for a reply that was not handed out is refused (false); a second
// report changes nothing. A reply marked unknown (the Mac took it and was not
// heard from) takes a late, definite report.
func (q Q) EndChatSend(ctx context.Context, id int64, state, errText, ref string, at int64) (bool, error) {
	res, err := q.x.ExecContext(ctx, `UPDATE proposals SET send_state=?, send_error=?, send_at=?, send_ref=CASE WHEN ?<>'' THEN ? ELSE send_ref END
		WHERE id=? AND action=? AND (send_state='handed' OR (send_state='unknown' AND ?<>'unknown'))`, state, errText, at, ref, ref, id, ChatReply, state)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

// HandedBefore lists the chat replies handed out before a time whose result
// never came.
func (q Q) HandedBefore(ctx context.Context, before int64) ([]Proposal, error) {
	return q.proposalsWhere(ctx, `action='reply_chat' AND send_state='handed' AND send_at < `+strconv.FormatInt(before, 10))
}

// ChatSendOf is the proposal of a chat reply by its id.
func (q Q) ChatSendOf(ctx context.Context, id int64) (Proposal, bool) {
	p, ok := q.ProposalByID(ctx, id)
	if !ok || p.Action != ChatReply {
		return Proposal{}, false
	}
	return p, true
}

// ItemPulse is a short text that changes whenever the messages or the thread
// of an item change: an open chat asks for it and fetches the row again when
// it is not what the page holds.
func (q Q) ItemPulse(ctx context.Context, itemID int64) string {
	var n, maxRaw, maxEvent, live int64
	var stage string
	q.x.QueryRowContext(ctx, `SELECT (SELECT COUNT(*) FROM messages WHERE item_id=i.id), COALESCE((SELECT MAX(raw_id) FROM messages WHERE item_id=i.id), 0),
		COALESCE((SELECT MAX(id) FROM events WHERE item_id=i.id), 0), i.chat_since > 0, i.stage FROM items i WHERE i.id=?`, itemID).Scan(&n, &maxRaw, &maxEvent, &live, &stage)
	return strconv.FormatInt(n, 10) + "." + strconv.FormatInt(maxRaw, 10) + "." + strconv.FormatInt(maxEvent, 10) + "." + strconv.FormatInt(live, 10) + "." + stage
}
