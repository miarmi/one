package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"lab/apps/one/repo"
	"lab/platform/appkit"
)

// Chats (SPEC 4, 10, 16, 17, 21). The bridge on the owner's Mac brings batches of
// chats and messages; this file is the one path they go through. It follows
// the same table as mail (DESIGN 3.5): unchanged writes nothing, an edit or a
// newly read message is a version, a new message from someone else wakes the
// item, the owner's own never does. What differs from mail is the quiet time: a
// chat that is still going on is shown at once, marked Live, and goes to
// Claude only when nobody has written in it for the chat quiet time.

const (
	keyChatQuiet     = "chat_quiet_minutes"
	keyBridgeSilence = "bridge_silence_minutes"
	chatLinkKind     = "chat"

	maxBatchMessages = 1000
	maxChatName      = 300
	maxChatField     = 300
	maxRawPayload    = 64 << 10
)

var networkRE = regexp.MustCompile(`^[a-z][a-z0-9]{1,19}$`)

// chatQuiet is how long nobody has to write in a chat before Claude sees it.
func (a *app) chatQuiet(ctx context.Context) time.Duration {
	if n, err := strconv.Atoi(a.store.Setting(ctx, keyChatQuiet)); err == nil && n > 0 {
		return time.Duration(n) * time.Minute
	}
	return 30 * time.Minute
}

// bridgeSilence is how long the Mac may stay silent before it is a failed source.
func (a *app) bridgeSilence(ctx context.Context) time.Duration {
	if n, err := strconv.Atoi(a.store.Setting(ctx, keyBridgeSilence)); err == nil && n > 0 {
		return time.Duration(n) * time.Minute
	}
	return 15 * time.Minute
}

// --- what the bridge sends ---

type bridgeReaction struct {
	By    string `json:"by"`
	Emoji string `json:"emoji"`
}

type bridgeAttachment struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Type string `json:"type"`
	Size int64  `json:"size"`
}

type bridgeMessage struct {
	ID          string             `json:"id"`
	Sender      string             `json:"sender"`
	SenderName  string             `json:"sender_name"`
	At          string             `json:"at"`
	Text        string             `json:"text"`
	Own         bool               `json:"own"`
	Read        bool               `json:"read"`
	Reactions   []bridgeReaction   `json:"reactions"`
	ReplyTo     string             `json:"reply_to"`
	Attachments []bridgeAttachment `json:"attachments"`
	EditedAt    string             `json:"edited_at"` // set when the network says the text was edited
	Deleted     bool               `json:"deleted"`   // the network says it was deleted: ONE keeps it, marked
	Raw         json.RawMessage    `json:"raw"`
}

type bridgeChat struct {
	Network      string             `json:"network"`
	ChatID       string             `json:"chat_id"`
	Name         string             `json:"name"`
	Type         string             `json:"type"` // direct or group
	Muted        bool               `json:"muted"`
	Participants []repo.Participant `json:"participants"`
	Messages     []bridgeMessage    `json:"messages"`
}

type bridgeBatch struct {
	// Again says these messages were asked for by the owner (Import again): the
	// import rules are left out for them.
	Again bool `json:"again"`
	// Run names the test run the batch belongs to (SPEC 16): everything it
	// creates is marked with it. The real bridge never sets it.
	Run   string       `json:"run"`
	Chats []bridgeChat `json:"chats"`
}

type missingFile struct {
	Network    string `json:"network"`
	ChatID     string `json:"chat_id"`
	MessageID  string `json:"message_id"`
	Attachment string `json:"attachment"`
}

type batchResult struct {
	Chats     int           `json:"chats"`
	Messages  int           `json:"messages"`
	Stored    int           `json:"stored"`    // messages not seen before
	Versions  int           `json:"versions"`  // changed copies of known messages
	Unchanged int           `json:"unchanged"` // known messages that came again as they were
	Dropped   int           `json:"dropped"`   // messages an import rule dropped
	Missing   []missingFile `json:"missing_files"`
	// Failed counts messages left out because each failed on its own; the bridge
	// is asked for them again through the outbox (softfail.go).
	Failed int `json:"left_out"`
}

// parsedMessage is a message of a batch, checked.
type parsedMessage struct {
	bridgeMessage
	at    int64
	sid   string // the app's own id of the message: chat id and message id
	state string // what makes one version of it
}

func chatSourceID(chatID, messageID string) string { return chatID + "|" + messageID }

func splitChatSourceID(sid string) (chatID, messageID string, ok bool) {
	i := strings.LastIndex(sid, "|")
	if i <= 0 || i == len(sid)-1 {
		return "", "", false
	}
	return sid[:i], sid[i+1:], true
}

func cleanHandle(s string) string { return strings.TrimSpace(strings.ToValidUTF8(s, "")) }

func reactionStrings(rs []bridgeReaction) []string {
	out := make([]string, 0, len(rs))
	for _, r := range rs {
		out = append(out, oneLine(r.Emoji, 16)+" "+oneLine(r.By, 80))
	}
	slices.Sort(out)
	return out
}

// messageState is what makes one version of a chat message: its text, its
// reactions and whether it is read. Two copies with the same state are the
// same version, and writing the second changes nothing.
func messageState(m bridgeMessage) string {
	h := sha256.New()
	h.Write([]byte(m.Text))
	h.Write([]byte{0})
	h.Write([]byte(strings.Join(reactionStrings(m.Reactions), "\x01")))
	h.Write([]byte{0})
	if m.Read {
		h.Write([]byte("read"))
	}
	h.Write([]byte{0})
	h.Write([]byte(m.EditedAt))
	if m.Deleted {
		h.Write([]byte("\x00deleted"))
	}
	return hex.EncodeToString(h.Sum(nil)[:8])
}

// checkBatch reads a batch and says in a sentence what is wrong with it.
func (a *app) checkBatch(b *bridgeBatch) (map[int][]parsedMessage, error) {
	if len(b.Chats) == 0 {
		return nil, bad("The batch holds no chat.")
	}
	total := 0
	out := map[int][]parsedMessage{}
	now := a.now().Unix()
	for i := range b.Chats {
		c := &b.Chats[i]
		c.Network = strings.ToLower(strings.TrimSpace(c.Network))
		c.ChatID = cleanHandle(c.ChatID)
		c.Name = oneLine(strings.ToValidUTF8(c.Name, ""), maxChatName)
		if !networkRE.MatchString(c.Network) {
			return nil, bad("A chat has no network, or a network name that is not one word of letters and digits.")
		}
		if c.ChatID == "" || len(c.ChatID) > maxChatField || strings.ContainsAny(c.ChatID, "\n\r\x00") {
			return nil, bad("A chat has no id, or one that is too long.")
		}
		if c.Type != "direct" && c.Type != "group" {
			return nil, bad(`A chat's type must be "direct" or "group".`)
		}
		if len(c.Participants) > 500 {
			return nil, bad("A chat has too many participants.")
		}
		for j := range c.Participants {
			p := &c.Participants[j]
			p.Handle = cleanHandle(p.Handle)
			p.Name = oneLine(strings.ToValidUTF8(p.Name, ""), 200)
			if len(p.Handle) > maxChatField {
				return nil, bad("A participant's handle is too long.")
			}
		}
		seen := map[string]bool{}
		for _, m := range c.Messages {
			total++
			if total > maxBatchMessages {
				return nil, bad(fmt.Sprintf("A batch holds at most %d messages.", maxBatchMessages))
			}
			m.ID = cleanHandle(m.ID)
			if m.ID == "" || len(m.ID) > maxChatField || strings.ContainsAny(m.ID, "\n\r\x00") {
				return nil, bad("A message has no id, or one that is too long.")
			}
			if seen[m.ID] {
				continue // the same message twice in one batch: the first one counts
			}
			seen[m.ID] = true
			at, err := time.Parse(time.RFC3339, m.At)
			if err != nil {
				return nil, bad("A message has no time in RFC 3339 form.")
			}
			ts := at.Unix()
			if ts > now+24*3600 {
				ts = now // a clock that runs ahead does not put a message in the future
			}
			if ts < 0 {
				ts = 0
			}
			if !utf8Valid(m.Text) {
				m.Text = strings.ToValidUTF8(m.Text, "")
			}
			if len(m.Text) > maxStoredText {
				m.Text = strings.ToValidUTF8(m.Text[:maxStoredText], "")
			}
			m.Sender = cleanHandle(m.Sender)
			m.EditedAt = oneLine(m.EditedAt, 40)
			if len(m.Sender) > maxChatField || len(m.Attachments) > 100 {
				return nil, bad("A message's sender or its attachments are too many or too long.")
			}
			for k := range m.Attachments {
				at := &m.Attachments[k]
				at.ID = cleanHandle(at.ID)
				if at.ID == "" || len(at.ID) > maxChatField {
					return nil, bad("An attachment has no id.")
				}
				at.Name = oneLine(strings.ToValidUTF8(at.Name, ""), 200)
				at.Type = strings.ToLower(oneLine(at.Type, 100))
			}
			if len(m.Raw) > maxRawPayload {
				m.Raw = nil
			}
			out[i] = append(out[i], parsedMessage{bridgeMessage: m, at: ts, sid: chatSourceID(c.ChatID, m.ID), state: messageState(m)})
		}
		slices.SortStableFunc(out[i], func(x, y parsedMessage) int {
			switch {
			case x.at < y.at:
				return -1
			case x.at > y.at:
				return 1
			}
			return strings.Compare(x.ID, y.ID)
		})
	}
	return out, nil
}

func utf8Valid(s string) bool { return strings.ToValidUTF8(s, "") == s }

// --- the arrival of a chat message at an item ---

// chatSubject is what a chat row is titled: the chat's name; a chat with none
// is named after the people in it who are not the owner.
func chatSubject(c bridgeChat) string {
	if c.Name != "" {
		return c.Name
	}
	var names []string
	for _, p := range c.Participants {
		if p.Self {
			continue
		}
		n := p.Name
		if n == "" {
			n = p.Handle
		}
		if n != "" {
			names = append(names, n)
		}
	}
	if len(names) > 0 {
		return oneLine(strings.Join(names, ", "), maxChatName)
	}
	return c.Network + " chat"
}

// newChatItem is the item the first messages of a chat make. Everything is
// decided from what the batch holds, so a chat that is read oldest message
// first comes out the same as one that arrives newest first.
//
//   - nothing from someone else inside the review window: history, not waiting;
//   - the chat went quiet already: it waits for Claude, hidden until Claude has
//     looked, like mail ("arrived settled");
//   - it is still going on: shown at once, marked Live, and held back until it
//     has been quiet for the chat quiet time.
func newChatItem(subject string, firstAt, lastAt, newestOther int64, hasOther bool, cutoff, now, quiet int64) (it repo.Item, settled bool) {
	it = repo.Item{Kind: "chat", Subject: subject, Stage: stageHistory, FirstAt: firstAt, LastAt: lastAt, MovedAt: lastAt}
	if !hasOther || newestOther < cutoff {
		return it, false
	}
	it.Stage = stageNew
	it.MovedAt = newestOther
	if now-lastAt >= quiet {
		it.NeedsClaude, it.Hidden = true, true
		return it, true
	}
	it.ChatSince = lastAt
	return it, false
}

// chatArrival is applyArrival (DESIGN 3.5) plus the quiet time of a chat. A
// message from someone else inside the window that is already older than the
// quiet time goes to Claude at once; a newer one makes the chat Live and
// shows it. The owner's own message never wakes anything; in a chat that is Live
// it only keeps the chat going. keepClaude is true when the owner has a note on
// the item that Claude has not answered: that request stays queued.
func chatArrival(it repo.Item, a arrival, cutoff, now, quiet int64, keepClaude bool) (repo.Item, itemChange, bool) {
	newest := a.At >= it.LastAt
	next, ch := applyArrival(it, a, cutoff, now)
	if !newest {
		return next, ch, false
	}
	if a.SentByMe {
		if next.ChatSince > 0 && a.At > next.ChatSince {
			next.ChatSince = a.At
		}
		return next, ch, false
	}
	if a.At < cutoff {
		return next, ch, false
	}
	if now-a.At >= quiet {
		next.ChatSince = 0
		return next, ch, !it.NeedsClaude // went to Claude at once
	}
	next.ChatSince = a.At
	next.Hidden = false
	next.NeedsClaude = keepClaude
	return next, ch, false
}

// --- the batch ---

// releaseChats sends to Claude the chats that have been quiet long enough.
// It is called by the timer and by everything that shows or reads the queue,
// so a chat goes the moment its quiet time is over.
func (a *app) releaseChats(ctx context.Context) {
	ctx = context.WithoutCancel(ctx)
	now := a.now().Unix()
	quiet := int64(a.chatQuiet(ctx) / time.Second)
	if !a.store.HasQuietChats(ctx, now-quiet) {
		return
	}
	var released []int64
	a.store.Update(ctx, func(tx *repo.Tx) error {
		ids, err := tx.ReleaseQuietChats(ctx, now-quiet)
		if err != nil {
			return err
		}
		released = ids
		for _, id := range ids {
			if err := tx.InsertEvent(ctx, now, id, "system", "chat_released", nil); err != nil {
				return err
			}
		}
		return nil
	})
	for _, id := range released {
		a.chatSettled(ctx, id) // skip rules (SPEC 31)
	}
}

// chatSettled applies the approved skip rules to a chat that now waits for
// Claude, before the model sees it (SPEC 31: the rule is about whoever wrote,
// in a chat too).
func (a *app) chatSettled(ctx context.Context, itemID int64) {
	if err := a.applySkipRules(context.WithoutCancel(ctx), itemID); err != nil {
		appkit.Error("applying skip rules to a chat", err)
	}
}

// ingestBatch is the one path a batch from the bridge goes through. It is
// safe to send twice: what is already stored is not written again.
func (a *app) ingestBatch(ctx context.Context, b bridgeBatch) (batchResult, error) {
	var res batchResult
	parsed, err := a.checkBatch(&b)
	if err != nil {
		return res, err
	}
	// A batch under a run may make chats of its own, and add to them; it never
	// creates or changes a chat that already exists without that run's mark.
	if run := repo.RunOf(ctx); run != "" {
		for _, c := range b.Chats {
			key := repo.ChatKey(c.Network, c.ChatID)
			if old, ok := a.store.ChatByKey(ctx, key); ok && old.TestRun != run {
				return res, conflict("That chat already exists and is not part of this test run, so the batch was refused and nothing was written.")
			}
			if it, ok := a.store.ItemByLink(ctx, chatLinkKind, key); ok && it.TestRun != run {
				return res, conflict("That chat already exists and is not part of this test run, so the batch was refused and nothing was written.")
			}
		}
	}
	for i, c := range b.Chats {
		if err := a.ingestChat(ctx, c, parsed[i], b.Again, &res); err != nil {
			return res, err
		}
	}
	return res, nil
}

func (a *app) ingestChat(ctx context.Context, c bridgeChat, msgs []parsedMessage, again bool, res *batchResult) error {
	now := a.now().Unix()
	cutoff := a.now().Add(-a.syncSettings(ctx).ReviewWindow).Unix()
	quiet := int64(a.chatQuiet(ctx) / time.Second)
	key := repo.ChatKey(c.Network, c.ChatID)
	rules, err := a.store.EnabledRules(ctx, "chat")
	if err != nil {
		return err
	}
	res.Chats++
	res.Messages += len(msgs)
	desc := repo.Chat{Key: key, Network: c.Network, ChatID: c.ChatID, Name: c.Name, Group: c.Type == "group", Muted: c.Muted, Participants: c.Participants}
	facts := ruleFacts{Network: c.Network, ChatType: c.Type, Muted: "false"}
	if desc.Group && desc.Muted {
		facts.Muted = "true" // the rule says muted group chats; a muted one-to-one chat is never dropped
	}
	var pendingFiles []int64 // messages whose attachments are still to come
	// One message that cannot be stored is left out, not the whole chat: each
	// message is one step (a savepoint) of the chat's transaction.
	var left []leftOutMsg
	chatMsgID := func(sid string) string { return c.Network + "\x1f" + sid }
	soft := func(tx *repo.Tx, sid string, fn func() error) error {
		err := tx.Try(ctx, func() error { return a.tryUnit("chat message", fn) })
		if err == nil || isFatal(ctx, err) {
			return err
		}
		left = append(left, leftOutMsg{chatMsgID(sid), err})
		return errLeftOut
	}
	err = a.store.Update(ctx, func(tx *repo.Tx) error {
		pendingFiles, left = nil, nil
		item, linked := tx.ItemByLink(ctx, chatLinkKind, key)
		var fresh []parsedMessage
		type edit struct {
			m   parsedMessage
			raw repo.Raw
		}
		var edits []edit
		for _, m := range msgs {
			raw, known := tx.LatestRaw(ctx, c.Network, m.sid)
			if run := repo.RunOf(ctx); run != "" && known && raw.TestRun != run {
				return &repo.RunError{} // a message that is real, or another run's, is never touched
			}
			switch {
			case known && strings.HasPrefix(raw.VersionKey, m.state+"@"):
				res.Unchanged++
				if msg, ok := tx.MessageBySource(ctx, c.Network, m.sid); ok && msg.HasAttachments {
					pendingFiles = append(pendingFiles, msg.ID)
				}
			case known:
				edits = append(edits, edit{m, raw})
			default:
				// A message that failed before and comes again at the bridge's
				// resend is still judged by the rules like any other.
				againM := again && !a.failHas(ctx, "chat", chatMsgID(m.sid))
				if !againM {
					f := facts
					f.Sender = strings.ToLower(m.Sender)
					f.Subject = desc.Name
					if r, hit := firstRuleHit(rules, f); hit {
						var inserted bool
						err := soft(tx, m.sid, func() (err error) {
							inserted, err = tx.InsertSkipped(ctx, repo.Skipped{At: now, Source: c.Network, SourceID: m.sid, Sender: oneLine(senderLabel(m, desc), 120),
								Subject: oneLine(chatSubject(c), 300), ReceivedAt: m.at, RuleID: r.ID, Snippet: snippetOf(m.Text, 400)})
							return err
						})
						if err == errLeftOut {
							continue
						} else if err != nil {
							return err
						}
						if inserted {
							res.Dropped++
						}
						continue
					}
				} else if sk, ok := tx.SkippedBySource(ctx, c.Network, m.sid); ok && !sk.Reimported {
					// Import again: the dropped line is answered by this message.
					if err := tx.MarkReimported(ctx, sk.ID, now); err != nil {
						return err
					}
				}
				fresh = append(fresh, m)
			}
		}
		if len(fresh) > 0 || linked {
			// The chat itself is kept once it has an item.
			if len(fresh) > 0 && !linked {
				first, last, newestOther := fresh[0].at, fresh[0].at, int64(0)
				hasOther := false
				for _, m := range fresh {
					first, last = min(first, m.at), max(last, m.at)
					if !m.Own && m.at >= newestOther {
						newestOther, hasOther = m.at, true
					}
				}
				var settled bool
				stored := 0
				item, settled = newChatItem(chatSubject(c), first, last, newestOther, hasOther, cutoff, now, quiet)
				var err error
				item, err = tx.CreateItem(ctx, item, chatLinkKind, key, "system", now)
				if err != nil {
					return err
				}
				linked = true
				if settled {
					if err := tx.InsertEvent(ctx, now, item.ID, "system", "chat_settled", nil); err != nil {
						return err
					}
				}
				for _, m := range fresh {
					var id int64
					err := soft(tx, m.sid, func() (err error) {
						id, err = a.storeChatMessage(ctx, tx, item, desc, m, now, true)
						return err
					})
					if err == errLeftOut {
						continue
					} else if err != nil {
						return err
					}
					if id != 0 {
						res.Stored++
						pendingFiles = append(pendingFiles, id)
					}
					stored++
				}
				if stored == 0 {
					// Nothing of a new chat could be stored: its item is not kept
					// without a message, and every message of the chat waits.
					return errNoneStored
				}
			} else {
				keep := tx.OpenNote(ctx, item.ID)
				for _, m := range fresh {
					var id int64
					err := soft(tx, m.sid, func() error {
						cur, _ := tx.ItemByID(ctx, item.ID)
						next, ch, settled := chatArrival(cur, arrival{At: m.at, SentByMe: m.Own, Subject: chatSubject(c)}, cutoff, now, quiet, keep)
						if err := tx.SaveItem(ctx, next); err != nil {
							return err
						}
						var err error
						id, err = a.storeChatMessage(ctx, tx, next, desc, m, now, false)
						if err != nil || id == 0 {
							return err
						}
						if _, err := tx.WithdrawPending(ctx, next.ID, "a new message arrived", now); err != nil {
							return err
						}
						if settled {
							if err := tx.InsertEvent(ctx, now, next.ID, "system", "chat_settled", nil); err != nil {
								return err
							}
						}
						if ch.Reopened {
							if err := tx.InsertEvent(ctx, now, next.ID, "system", "reopened", map[string]any{"message_id": id}); err != nil {
								return err
							}
						}
						if ch.SetPending {
							if err := tx.InsertEvent(ctx, now, next.ID, "system", "pending_set", map[string]any{"message_id": id}); err != nil {
								return err
							}
						}
						return nil
					})
					if err == errLeftOut {
						continue
					} else if err != nil {
						return err
					}
					if id != 0 {
						res.Stored++
						pendingFiles = append(pendingFiles, id)
					}
				}
				item, _ = tx.ItemByID(ctx, item.ID)
			}
		}
		for _, e := range edits {
			var changed bool
			err := soft(tx, e.m.sid, func() (err error) {
				changed, err = a.chatVersion(ctx, tx, e.m, e.raw, desc, now)
				return err
			})
			if err == errLeftOut {
				continue
			} else if err != nil {
				return err
			}
			if changed {
				res.Versions++
			}
		}
		if linked {
			desc.ItemID = item.ID
			if wrote, err := tx.SaveChat(ctx, desc, now); err != nil {
				return err
			} else if wrote {
				if err := a.rememberParticipants(ctx, tx, desc, now); err != nil {
					return err
				}
				// A chat that was renamed is titled by its new name.
				if sub := chatSubject(c); sub != item.Subject && c.Name != "" {
					item.Subject = sub
					if err := tx.SaveItem(ctx, item); err != nil {
						return err
					}
				}
			}
		}
		return nil
	})
	if errors.Is(err, errNoneStored) {
		// The whole chat's step was taken back; every message of it waits.
		for _, m := range msgs {
			if !slices.ContainsFunc(left, func(l leftOutMsg) bool { return l.id == chatMsgID(m.sid) }) {
				left = append(left, leftOutMsg{chatMsgID(m.sid), err})
			}
		}
		a.leftOutChat(ctx, c.Network, left, res)
		return nil
	}
	if err != nil {
		return err
	}
	a.leftOutChat(ctx, c.Network, left, res)
	// A message that went through leaves the ledger (it may be one the bridge sent again).
	for _, m := range msgs {
		if !slices.ContainsFunc(left, func(l leftOutMsg) bool { return l.id == chatMsgID(m.sid) }) {
			a.failClear(ctx, "chat", chatMsgID(m.sid))
		}
	}
	if it, ok := a.store.ItemByLink(ctx, chatLinkKind, key); ok && it.NeedsClaude {
		a.chatSettled(ctx, it.ID)
	}
	// The attachments the bridge still has to upload.
	for _, id := range pendingFiles {
		files, err := a.store.FilesOfMessage(ctx, id)
		if err != nil {
			return err
		}
		msg, ok := a.store.MessageByID(ctx, id)
		if !ok {
			continue
		}
		for _, f := range files {
			if f.Status == "pending" {
				chatID, msgID, ok := splitChatSourceID(msg.SourceID)
				if ok {
					res.Missing = append(res.Missing, missingFile{Network: msg.Source, ChatID: chatID, MessageID: msgID, Attachment: f.Part})
				}
			}
		}
	}
	return nil
}

func senderLabel(m parsedMessage, c repo.Chat) string {
	if m.SenderName != "" {
		return m.SenderName
	}
	return m.Sender
}

// rememberParticipants adds every participant of a chat to the people ONE
// knows, on first sight.
func (a *app) rememberParticipants(ctx context.Context, tx *repo.Tx, c repo.Chat, now int64) error {
	for _, p := range c.Participants {
		if p.Self || p.Handle == "" {
			continue
		}
		if _, err := tx.EnsurePerson(ctx, c.Network, p.Handle, p.Name, now); err != nil {
			return err
		}
	}
	return nil
}

// storeChatMessage writes a message not seen before: its raw row, the message,
// its files, the person who wrote it and the record. It returns the message's
// number, or 0 when it was already there.
func (a *app) storeChatMessage(ctx context.Context, tx *repo.Tx, item repo.Item, c repo.Chat, m parsedMessage, now int64, newItem bool) (int64, error) {
	payload := m.Raw
	if len(payload) == 0 {
		b, _ := json.Marshal(map[string]any{"chat": map[string]any{"network": c.Network, "chat_id": c.ChatID, "name": c.Name, "group": c.Group, "muted": c.Muted}, "message": m.bridgeMessage})
		payload = b
	}
	rawID, inserted, err := tx.InsertRaw(ctx, repo.Raw{Source: c.Network, SourceID: m.sid, VersionKey: m.state + "@0", ThreadKey: "chat:" + c.Key,
		ReceivedAt: m.at, IngestedAt: now, Payload: string(payload), TestRun: item.TestRun})
	if err != nil {
		return 0, err
	}
	if !inserted {
		return 0, nil
	}
	sender, name := strings.TrimSpace(m.Sender), strings.TrimSpace(m.SenderName)
	if m.Own {
		name = "You"
	} else if name == "" {
		for _, p := range c.Participants {
			if p.Handle == sender {
				name = p.Name
			}
		}
	}
	if name == "" {
		name = sender
	}
	msg := repo.Message{ItemID: item.ID, RawID: rawID, Source: c.Network, SourceID: m.sid, ThreadKey: "chat:" + c.Key, Network: c.Network,
		Sender: sender, SenderName: oneLine(name, 200), Subject: oneLine(c.Name, 300), Addressed: !c.Group && !m.Own,
		HasAttachments: len(m.Attachments) > 0, Text: m.Text, SentByMe: m.Own, ReceivedAt: m.at, Labels: reactionStrings(m.Reactions),
		Seen: m.Read, HeaderID: m.ID, Edited: m.EditedAt != ""}
	if m.Read {
		msg.Flags = "read"
	}
	id, ok, err := tx.InsertMessage(ctx, msg)
	if err != nil || !ok {
		return 0, err
	}
	if m.Deleted {
		if err := tx.MarkMessageDeleted(ctx, id); err != nil {
			return 0, err
		}
	}
	for _, at := range m.Attachments {
		ct := at.Type
		if ct == "" {
			ct = "application/octet-stream"
		}
		if _, err := tx.InsertFile(ctx, repo.File{ItemID: item.ID, Source: c.Network, MessageID: id, Part: at.ID, Filename: at.Name, ContentType: ct,
			Disposition: "attachment", Size: at.Size, Status: "pending", AddedAt: now}); err != nil {
			return 0, err
		}
	}
	if m.Own {
		if err := matchSentReply(ctx, tx, item.ID, msg, id, now); err != nil {
			return 0, err
		}
		if err := matchOutPart(ctx, tx, item.ID, msg, id, now); err != nil {
			return 0, err
		}
	} else if sender != "" {
		if _, err := tx.EnsurePerson(ctx, c.Network, sender, name, now); err != nil {
			return 0, err
		}
	}
	if err := tx.InsertEvent(ctx, now, item.ID, "system", "ingested", map[string]any{"message_id": id, "item": repo.Number(item.Number), "new_item": newItem}); err != nil {
		return 0, err
	}
	return id, nil
}

// chatVersion keeps a changed copy of a known message: a reaction or a read
// mark updates the one message and wakes nothing; an edit shows the new text
// marked edited and brings the chat back to Claude after its quiet time.
func (a *app) chatVersion(ctx context.Context, tx *repo.Tx, m parsedMessage, prev repo.Raw, c repo.Chat, now int64) (bool, error) {
	msg, ok := tx.MessageBySource(ctx, c.Network, m.sid)
	if !ok {
		return false, nil
	}
	payload := m.Raw
	if len(payload) == 0 {
		b, _ := json.Marshal(map[string]any{"chat": map[string]any{"network": c.Network, "chat_id": c.ChatID, "name": c.Name, "group": c.Group, "muted": c.Muted}, "message": m.bridgeMessage})
		payload = b
	}
	rawID, inserted, err := tx.InsertRaw(ctx, repo.Raw{Source: c.Network, SourceID: m.sid, VersionKey: m.state + "@" + strconv.FormatInt(prev.ID, 10), ThreadKey: prev.ThreadKey,
		ReceivedAt: prev.ReceivedAt, IngestedAt: now, Payload: string(payload), TestRun: prev.TestRun})
	if err != nil || !inserted {
		return false, err
	}
	// A deleted message keeps the text ONE had: the network may blank it, and
	// ONE deletes nothing (SPEC 30). It is marked, and wakes nothing.
	text := m.Text
	if m.Deleted {
		text = msg.Text
		if err := tx.MarkMessageDeleted(ctx, msg.ID); err != nil {
			return false, err
		}
	}
	// An edit is a changed text, or the network's first mark that the message
	// was edited (SPEC 4). A reaction or a read mark is neither.
	textChanged := text != msg.Text || (!m.Deleted && !msg.Edited && m.EditedAt != "")
	if err := tx.UpdateChatMessage(ctx, msg.ID, rawID, text, msg.Edited || textChanged || m.EditedAt != "", reactionStrings(m.Reactions), m.Read); err != nil {
		return false, err
	}
	if !textChanged || msg.SentByMe {
		return true, nil
	}
	it, ok := tx.ItemByID(ctx, msg.ItemID)
	if !ok {
		return true, nil
	}
	if err := tx.InsertEvent(ctx, now, it.ID, "system", "chat_edited", map[string]any{"message_id": msg.ID}); err != nil {
		return false, err
	}
	// The reply that was written for the old text no longer fits.
	if _, err := tx.WithdrawPending(ctx, it.ID, "a message was edited", now); err != nil {
		return false, err
	}
	// SPEC 4: an edit "reopens" like a message from someone else: a Pending,
	// Resolved, Closed or history item comes back to New, and the chat goes to
	// Claude once it has been quiet (SPEC 17). The row stays where it is.
	changed := false
	switch it.Stage {
	case stagePending, stageResolved, stageClosed, stageHistory:
		it.Stage = stageNew
		it.PendingSince = 0
		it.Hidden = false
		changed = true
		if err := tx.InsertEvent(ctx, now, it.ID, "system", "reopened", map[string]any{"message_id": msg.ID}); err != nil {
			return false, err
		}
	}
	// An edit is not somebody writing in the chat: the quiet time (SPEC 17)
	// counts from the chat's newest message. A chat that has been quiet
	// already goes to Claude at once; one still being written in waits.
	if !it.NeedsClaude {
		quiet := int64(a.chatQuiet(ctx) / time.Second)
		if now-it.LastAt >= quiet {
			it.NeedsClaude, it.ChatSince = true, 0
		} else if it.ChatSince == 0 {
			it.ChatSince = it.LastAt
		}
		changed = true
	}
	if changed {
		if err := tx.SaveItem(ctx, it); err != nil {
			return false, err
		}
	}
	return true, nil
}

var (
	errLeftOut    = errors.New("message left out")
	errNoneStored = errors.New("no message of a new chat could be stored")
)

type leftOutMsg struct {
	id  string // network, then the app's own id of the message
	err error
}

// leftOutChat logs and remembers the messages of one chat that failed on
// their own. They reach the bridge again through the outbox's resend list.
func (a *app) leftOutChat(ctx context.Context, network string, left []leftOutMsg, res *batchResult) {
	for _, l := range left {
		res.Failed++
		a.logLeftOut(network, l.id, "message", a.failNoteNew(ctx, "chat", l.id), l.err)
	}
}
