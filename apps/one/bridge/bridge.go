package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

const version = "0.2"

const (
	maxPages      = 50 // pages read of one list in one round; the rest comes in the next
	chatPages     = 10 // pages of 200 chats
	sweepPages    = 3  // pages looked at again for edits, deletions and reactions
	watchFor      = 30 * time.Minute
	defaultMaxGiB = 256 << 20 // the biggest file taken until ONE says what it takes
)

// Bridge reads Beeper Desktop, tells ONE, and sends what the owner approved or
// wrote himself. It logs counts and fixed words only: never message text,
// names, chat ids, file names or tokens.
type Bridge struct {
	cfg      Config
	st       *state
	beeper   *beeperClient
	one      *http.Client
	log      *log.Logger
	now      func() time.Time
	lastBeat time.Time
	cycles   int
	networks map[string]string  // Beeper account id -> network name
	atts     map[string]attInfo // network|chat|message|attachment -> where the file is
	lastErr  string
	backfill int   // days to go back the first time, as ONE says (0: not told yet)
	maxFile  int64 // the biggest file ONE takes, as it says
}

// attInfo is what the bridge needs to fetch an attachment of Beeper's.
type attInfo struct{ ID, Src string }

func NewBridge(cfg Config, lg *log.Logger) (*Bridge, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	st, err := loadState(cfg.StatePath)
	if err != nil {
		return nil, errors.New("the state file cannot be read")
	}
	return &Bridge{cfg: cfg, st: st, beeper: newBeeper(cfg.BeeperURL, cfg.BeeperToken), one: newHTTPClient(5 * time.Minute),
		log: lg, now: time.Now, networks: map[string]string{}, atts: map[string]attInfo{}, maxFile: defaultMaxGiB}, nil
}

// --- talking to ONE ---

func (b *Bridge) oneReq(ctx context.Context, method, path string, body io.Reader, size int64) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, b.cfg.OneURL+path, body)
	if err != nil {
		return nil, errors.New("bad request to ONE")
	}
	if size >= 0 {
		req.ContentLength = size
	}
	req.Header.Set("Authorization", "Bearer "+b.cfg.OneToken)
	if method == "POST" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := b.one.Do(req)
	if err != nil {
		return nil, errors.New("ONE is not reachable")
	}
	if resp.StatusCode == 401 {
		resp.Body.Close()
		return nil, errors.New("ONE does not accept the bridge token")
	}
	if resp.StatusCode/100 != 2 {
		resp.Body.Close()
		return nil, errStatus(resp.StatusCode)
	}
	return resp, nil
}

func (b *Bridge) oneDo(ctx context.Context, method, path string, body io.Reader, size int64, out any) error {
	resp, err := b.oneReq(ctx, method, path, body, size)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if out != nil {
		if err := json.NewDecoder(io.LimitReader(resp.Body, 16<<20)).Decode(out); err != nil {
			return errors.New("ONE's answer could not be read")
		}
	}
	return nil
}

func (b *Bridge) post(ctx context.Context, path string, in, out any) error {
	j, _ := json.Marshal(in)
	return b.oneDo(ctx, "POST", path, bytes.NewReader(j), int64(len(j)), out)
}

// --- the wire format of ONE's bridge addresses ---

type wNet struct {
	Network    string `json:"network"`
	State      string `json:"state"` // connected, needs_login, error or unknown
	Detail     string `json:"detail,omitempty"`
	Status     string `json:"status,omitempty"`      // Beeper's own word for the account
	StatusText string `json:"status_text,omitempty"` // and its own explanation
}

type wReaction struct {
	By    string `json:"by"`
	Emoji string `json:"emoji"`
}

type wAttachment struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Type string `json:"type"`
	Size int64  `json:"size"`
}

type wMessage struct {
	ID          string        `json:"id"`
	Sender      string        `json:"sender"`
	SenderName  string        `json:"sender_name"`
	At          string        `json:"at"`
	Text        string        `json:"text"`
	Own         bool          `json:"own"`
	Read        bool          `json:"read"`
	Reactions   []wReaction   `json:"reactions,omitempty"`
	ReplyTo     string        `json:"reply_to,omitempty"`
	Attachments []wAttachment `json:"attachments,omitempty"`
	EditedAt    string        `json:"edited_at,omitempty"`
	Deleted     bool          `json:"deleted,omitempty"`
}

type wParticipant struct {
	Handle string `json:"handle"`
	Name   string `json:"name"`
	Self   bool   `json:"self,omitempty"`
}

type wChat struct {
	Network      string         `json:"network"`
	ChatID       string         `json:"chat_id"`
	Name         string         `json:"name"`
	Type         string         `json:"type"`
	Muted        bool           `json:"muted"`
	Participants []wParticipant `json:"participants"`
	Messages     []wMessage     `json:"messages"`
}

type wMissing struct {
	Network    string `json:"network"`
	ChatID     string `json:"chat_id"`
	MessageID  string `json:"message_id"`
	Attachment string `json:"attachment"`
}

// wLimits is what ONE tells the bridge with its answers.
type wLimits struct {
	BackfillDays int   `json:"backfill_days"`
	MaxFileBytes int64 `json:"max_file_bytes"`
}

type wBatchAnswer struct {
	wLimits
	Stored  int        `json:"stored"`
	Missing []wMissing `json:"missing_files"`
}

type wReply struct {
	ID      int64  `json:"id"`
	Network string `json:"network"`
	ChatID  string `json:"chat_id"`
	Text    string `json:"text"`
}

// wPost is a message the owner wrote himself: the exact text, or one file.
type wPost struct {
	ID      int64  `json:"id"`
	Group   string `json:"group"`
	Seq     int    `json:"seq"`
	Network string `json:"network"`
	ChatID  string `json:"chat_id"`
	Kind    string `json:"kind"` // text or file
	Text    string `json:"text"`
	File    *struct {
		Name string `json:"name"`
		Type string `json:"type"`
		Size int64  `json:"size"`
	} `json:"file"`
}

type wOutbox struct {
	wLimits
	Replies []wReply   `json:"replies"`
	Posts   []wPost    `json:"posts"`
	Resend  []wMissing `json:"resend"` // network, chat_id, message_id
}

func (b *Bridge) learn(l wLimits) {
	if l.BackfillDays > 0 {
		b.backfill = l.BackfillDays
	}
	if l.MaxFileBytes > 0 {
		b.maxFile = l.MaxFileBytes
	}
}

// backfillDays is how far back a chat is read the first time: as far as ONE
// says, else what was configured (a month).
func (b *Bridge) backfillDays() int {
	if b.backfill > 0 {
		return b.backfill
	}
	return b.cfg.BackfillDays
}

// --- one round ---

// Cycle does one round: say it is alive, take what the owner wrote and the replies,
// send them, report, read Beeper and hand ONE what is new. A non-nil error
// means "back off".
func (b *Bridge) Cycle(ctx context.Context) error {
	b.cycles++
	var errs []error
	if err := b.heartbeat(ctx); err != nil {
		// ONE not answering: nothing else can be told to it either.
		return b.fail([]error{err})
	}
	if err := b.outbox(ctx); err != nil {
		errs = append(errs, err)
	}
	b.watchSends(ctx)
	if err := b.reportAll(ctx); err != nil {
		errs = append(errs, err)
	}
	b.retryFiles(ctx)
	if err := b.scan(ctx); err != nil {
		errs = append(errs, err)
	}
	return b.fail(errs)
}

func (b *Bridge) fail(errs []error) error {
	if len(errs) == 0 {
		return nil
	}
	err := errors.Join(errs...)
	b.log.Printf("cycle failed: %s", strings.ReplaceAll(err.Error(), "\n", "; "))
	return err
}

// netStateOf is how ONE's states name what Beeper's status of an account says.
func netStateOf(status string) string {
	switch {
	case status == "" || status == "connected" || status == "connecting" || status == "backfilling":
		return "connected"
	case needsLogin(status):
		return "needs_login"
	}
	return "unknown" // disabled, or a word this program does not know
}

var netRank = map[string]int{"needs_login": 3, "unknown": 2, "error": 2, "connected": 1}

// heartbeat tells ONE the bridge is alive, about every minute, with how each
// network looks from here: Beeper's own status and words for it, and whether
// it needs a new login.
func (b *Bridge) heartbeat(ctx context.Context) error {
	if !b.lastBeat.IsZero() && b.now().Sub(b.lastBeat) < 55*time.Second {
		return nil
	}
	nets := []wNet{}
	lastErr := ""
	accounts, err := b.beeper.Accounts(ctx)
	if err != nil {
		var st errStatus
		if errors.As(err, &st) && (st == 401 || st == 403) {
			lastErr = "Beeper does not accept the token the bridge uses."
			b.log.Printf("beeper refuses the token")
		} else {
			lastErr = "Beeper is not answering on the Mac."
			b.log.Printf("beeper not answering")
		}
	} else {
		byName := map[string]int{}
		for _, a := range accounts {
			n := networkName(a)
			b.networks[a.AccountID] = n
			w := wNet{Network: n, State: netStateOf(a.Status), Status: a.Status, StatusText: oneLine(a.StatusText, 200)}
			// Two logins of one network: the one that needs the most attention counts.
			if i, ok := byName[n]; ok {
				if netRank[w.State] > netRank[nets[i].State] {
					nets[i] = w
				}
				continue
			}
			byName[n] = len(nets)
			nets = append(nets, w)
		}
	}
	if lastErr == "" {
		lastErr = b.lastErr
	}
	body := map[string]any{"networks": nets, "version": version, "last_error": lastErr, "files_pending": len(b.st.Files), "files_note": b.filesNote()}
	var ans wLimits
	if err := b.post(ctx, "/api/bridge/alive", body, &ans); err != nil {
		return err
	}
	b.learn(ans)
	b.lastBeat = b.now()
	return nil
}

// filesNote says in a sentence why attachments wait, without naming any.
func (b *Bridge) filesNote() string {
	reasons := map[string]int{}
	for _, f := range b.st.Files {
		reasons[f.Reason]++
	}
	if len(reasons) == 0 {
		return ""
	}
	var rs []string
	for r := range reasons {
		rs = append(rs, r)
	}
	sort.Strings(rs)
	return rs[0]
}

func oneLine(s string, max int) string {
	s = strings.Join(strings.Fields(strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, s)), " ")
	if r := []rune(s); len(r) > max {
		s = string(r[:max])
	}
	return s
}

func (b *Bridge) networkOf(c bChat) string {
	if n, ok := b.networks[c.AccountID]; ok {
		return n
	}
	return shortNetwork(c.Network, c.AccountID)
}

func hashOf(m bMessage) string {
	h := sha256.New()
	fmt.Fprintf(h, "%s\x00%s\x00%s\x00%v\x00%v\x00%s\x00%s\x00%v", m.Text, m.Timestamp, m.LinkedMessageID, m.IsSender, m.IsUnread, m.SenderID, m.EditedTimestamp, m.IsDeleted)
	for _, a := range m.Attachments {
		fmt.Fprintf(h, "\x00a%s:%s:%d", a.ID, a.FileName, a.FileSize)
	}
	rs := make([]string, 0, len(m.Reactions))
	for _, r := range m.Reactions {
		rs = append(rs, r.ParticipantID+r.Key)
	}
	sort.Strings(rs)
	fmt.Fprintf(h, "\x00r%s", strings.Join(rs, ","))
	return hex.EncodeToString(h.Sum(nil))[:16]
}

func (b *Bridge) wireChat(c bChat, msgs []bMessage) wChat {
	net := b.networkOf(c)
	typ := "direct"
	if c.Type == "group" {
		typ = "group"
	}
	w := wChat{Network: net, ChatID: c.ID, Name: c.Title, Type: typ, Muted: c.IsMuted, Participants: []wParticipant{}, Messages: []wMessage{}}
	self := ""
	for _, p := range c.Participants.Items {
		w.Participants = append(w.Participants, wParticipant{Handle: p.ID, Name: p.FullName, Self: p.IsSelf})
		if p.IsSelf {
			self = p.ID
		}
	}
	for _, m := range msgs {
		wm := wMessage{ID: m.ID, Sender: m.SenderID, SenderName: m.SenderName, At: m.Timestamp, Text: m.Text,
			Own: m.IsSender || (self != "" && m.SenderID == self), Read: !m.IsUnread, ReplyTo: m.LinkedMessageID,
			EditedAt: m.EditedTimestamp, Deleted: m.IsDeleted}
		if wm.Own {
			wm.Read = true
		}
		for _, r := range m.Reactions {
			wm.Reactions = append(wm.Reactions, wReaction{By: r.ParticipantID, Emoji: r.Key})
		}
		for _, a := range m.Attachments {
			wm.Attachments = append(wm.Attachments, wAttachment{ID: a.ID, Name: a.FileName, Type: a.MimeType, Size: a.FileSize})
			b.atts[attKey(net, c.ID, m.ID, a.ID)] = attInfo{ID: a.ID, Src: a.SrcURL}
		}
		w.Messages = append(w.Messages, wm)
	}
	return w
}

func attKey(network, chat, msg, att string) string {
	return network + "|" + chat + "|" + msg + "|" + att
}

type pendingMark struct {
	key, activity, cursor string
	hashes                map[string]string
}

// allChats pages through the chats, newest activity first, as far as the
// first time reads (the cutoff) or the pages allow.
func (b *Bridge) allChats(ctx context.Context, cutoff time.Time) ([]bChat, error) {
	var out []bChat
	cursor, dir := "", ""
	for i := 0; i < chatPages; i++ {
		pg, err := b.beeper.Chats(ctx, cursor, dir)
		if err != nil {
			return nil, err
		}
		out = append(out, pg.Items...)
		if !pg.HasMore || len(pg.Items) == 0 || pg.OldestCursor == "" {
			break
		}
		if t, err := time.Parse(time.RFC3339, pg.Items[len(pg.Items)-1].LastActivity); err == nil && t.Before(cutoff) {
			break
		}
		cursor, dir = pg.OldestCursor, "before"
	}
	return out, nil
}

func stampOf(m bMessage) time.Time {
	t, _ := time.Parse(time.RFC3339, m.Timestamp)
	return t
}

// readChat reads what is new in one chat. A chat the bridge has not read yet is
// read backwards from its newest message as far as the cutoff; a known chat is
// read forwards from the cursor ONE last took. Now and then (sweep) the newest
// pages are read again, because an edit, a deletion or a reaction does not move
// the cursor. It answers the messages (oldest first), the newest cursor, and
// whether more is left for the next round.
func (b *Bridge) readChat(ctx context.Context, c bChat, cs *chatState, cutoff time.Time, sweep bool) (msgs []bMessage, newest string, more bool, err error) {
	seen := map[string]bool{}
	add := func(items []bMessage) {
		for _, m := range items {
			if m.IsHidden || m.Type == "REACTION" || seen[m.ID] {
				continue
			}
			seen[m.ID] = true
			msgs = append(msgs, m)
		}
	}
	back := func(limit time.Time, pages int) error {
		cur, dir := "", ""
		for i := 0; i < pages; i++ {
			pg, err := b.beeper.Messages(ctx, c.ID, cur, dir)
			if err != nil {
				return err
			}
			if i == 0 && newest == "" {
				newest = pg.NewestCursor
			}
			add(pg.Items)
			if !pg.HasMore || len(pg.Items) == 0 || pg.OldestCursor == "" {
				return nil
			}
			if first := stampOf(pg.Items[0]); !first.IsZero() && first.Before(limit) {
				return nil
			}
			cur, dir = pg.OldestCursor, "before"
		}
		return nil
	}
	if cs == nil || cs.Cursor == "" {
		if err = back(cutoff, maxPages); err != nil {
			return nil, "", false, err
		}
	} else {
		cur := cs.Cursor
		newest = cur
		for i := 0; i < maxPages; i++ {
			pg, err := b.beeper.Messages(ctx, c.ID, cur, "after")
			if err != nil {
				return nil, "", false, err
			}
			add(pg.Items)
			if pg.NewestCursor != "" {
				newest, cur = pg.NewestCursor, pg.NewestCursor
			}
			if !pg.HasMore || len(pg.Items) == 0 {
				break
			}
			if i == maxPages-1 {
				more = true
			}
		}
		if sweep && !more {
			sweepSeen := newest
			if err = back(b.now().Add(-48*time.Hour), sweepPages); err != nil {
				return nil, "", false, err
			}
			if newest == "" {
				newest = sweepSeen
			}
		}
	}
	sort.SliceStable(msgs, func(i, j int) bool {
		a, bb := stampOf(msgs[i]), stampOf(msgs[j])
		if !a.Equal(bb) {
			return a.Before(bb)
		}
		return msgs[i].SortKey < msgs[j].SortKey
	})
	return msgs, newest, more, nil
}

// scan reads the chats that moved and hands ONE the messages it has not had in
// this form. The place in a chat is written only after ONE accepted the batch.
func (b *Bridge) scan(ctx context.Context) error {
	cutoff := b.now().Add(-time.Duration(b.backfillDays()) * 24 * time.Hour)
	chats, err := b.allChats(ctx, cutoff)
	if err != nil {
		b.lastErr = "Beeper could not be read."
		return err
	}
	b.lastErr = ""
	sweep := b.cycles%15 == 1 // now and then look at recent chats again: edits and reactions do not always move a chat
	var batch []wChat
	var marks []pendingMark
	nmsgs := 0
	for _, c := range chats {
		if c.ID == "" {
			continue
		}
		key := b.networkOf(c) + "/" + c.ID
		cs := b.st.Chats[key]
		if cs == nil {
			if t, err := time.Parse(time.RFC3339, c.LastActivity); err == nil && t.Before(cutoff) {
				continue
			}
		} else if cs.Activity == c.LastActivity && c.LastActivity != "" && !(sweep && recent(c.LastActivity, b.now())) {
			continue
		}
		msgs, newest, more, err := b.readChat(ctx, c, cs, cutoff, sweep)
		if err != nil {
			b.lastErr = "Beeper could not be read."
			return err
		}
		known := map[string]string{}
		if cs != nil {
			known = cs.Msgs
		}
		var fresh []bMessage
		hashes := map[string]string{}
		for _, m := range msgs {
			if cs == nil && !stampOf(m).IsZero() && stampOf(m).Before(cutoff) {
				continue
			}
			h := hashOf(m)
			hashes[m.ID] = h
			if known[m.ID] != h {
				fresh = append(fresh, m)
			}
		}
		activity := c.LastActivity
		if more {
			activity = "" // not all of it was read: the next round looks again
		}
		if len(fresh) == 0 {
			if cs == nil {
				b.st.Chats[key] = &chatState{Activity: activity, Cursor: newest, Msgs: hashes}
			} else {
				cs.Activity = activity
				if newest != "" {
					cs.Cursor = newest
				}
			}
			continue
		}
		batch = append(batch, b.wireChat(c, fresh))
		marks = append(marks, pendingMark{key: key, activity: activity, cursor: newest, hashes: hashes})
		nmsgs += len(fresh)
		if len(batch) >= 20 || nmsgs >= 500 {
			if err := b.send(ctx, batch, marks, false); err != nil {
				return err
			}
			batch, marks, nmsgs = nil, nil, 0
		}
	}
	if len(batch) > 0 {
		if err := b.send(ctx, batch, marks, false); err != nil {
			return err
		}
	}
	return b.st.save()
}

func recent(ts string, now time.Time) bool {
	t, err := time.Parse(time.RFC3339, ts)
	return err == nil && now.Sub(t) < 48*time.Hour
}

// send posts one batch and, once ONE took it, remembers what it took.
func (b *Bridge) send(ctx context.Context, batch []wChat, marks []pendingMark, again bool) error {
	var ans wBatchAnswer
	if err := b.post(ctx, "/api/bridge/batch", map[string]any{"again": again, "chats": batch}, &ans); err != nil {
		return err
	}
	b.learn(ans.wLimits)
	for _, m := range marks {
		cs := b.st.Chats[m.key]
		if cs == nil {
			cs = &chatState{Msgs: map[string]string{}}
			b.st.Chats[m.key] = cs
		}
		cs.Activity = m.activity
		if m.cursor != "" {
			cs.Cursor = m.cursor
		}
		for id, h := range m.hashes {
			cs.Msgs[id] = h
		}
	}
	if err := b.st.save(); err != nil {
		return errors.New("the state file cannot be written")
	}
	b.log.Printf("batch sent chats=%d messages=%d stored=%d", len(batch), countMsgs(batch), ans.Stored)
	b.uploadMissing(ctx, ans.Missing)
	return nil
}

func countMsgs(chats []wChat) int {
	n := 0
	for _, c := range chats {
		n += len(c.Messages)
	}
	return n
}

// --- attachments of Beeper's, brought to ONE ---

// hiddenPath says whether a path runs through a folder whose name begins with
// a dot (~/.ssh, ~/.one-bridge): the bridge never reads from there.
func hiddenPath(p string) bool {
	for _, part := range strings.Split(filepath.ToSlash(p), "/") {
		if strings.HasPrefix(part, ".") && part != "." && part != ".." {
			return true
		}
	}
	return false
}

// localPath turns what Beeper gave (a file URL or a path) into a path.
func localPath(s string) (string, bool) {
	switch {
	case strings.HasPrefix(s, "file://"):
		u, err := url.Parse(s)
		if err != nil || (u.Host != "" && u.Host != "localhost") || u.Path == "" {
			return "", false
		}
		return u.Path, true
	case strings.HasPrefix(s, "/"):
		return s, true
	}
	return "", false
}

func isMedia(s string) bool {
	return strings.HasPrefix(s, "mxc://") || strings.HasPrefix(s, "localmxc://")
}

// fetchLocal finds the file of an attachment on this Mac: a media address is
// fetched with Beeper's download call, which answers with the local file; a
// local path is used as it is. The path is a regular file outside any hidden
// folder, and not bigger than ONE takes.
func (b *Bridge) fetchLocal(ctx context.Context, info attInfo) (string, int64, error) {
	var src string
	for _, c := range []string{info.ID, info.Src} {
		if isMedia(c) {
			got, err := b.beeper.Download(ctx, c)
			if err != nil {
				return "", 0, err
			}
			src = got
			break
		}
	}
	if src == "" {
		src = info.Src
	}
	p, ok := localPath(src)
	if !ok {
		return "", 0, errors.New("Beeper gave no file on the Mac for it")
	}
	if real, err := filepath.EvalSymlinks(p); err == nil {
		p = real
	}
	if hiddenPath(p) {
		return "", 0, errors.New("the file is in a folder the bridge does not read")
	}
	st, err := os.Stat(p)
	if err != nil || !st.Mode().IsRegular() {
		return "", 0, errors.New("the file is not on the Mac (any more)")
	}
	if st.Size() > b.maxFile {
		return "", 0, errors.New("the file is larger than ONE takes")
	}
	return p, st.Size(), nil
}

// putFile brings one attachment to ONE. The reason it could not is a sentence
// with no name in it.
func (b *Bridge) putFile(ctx context.Context, w *fileWait) error {
	path, size, err := b.fetchLocal(ctx, attInfo{ID: w.Att, Src: w.Src})
	if err != nil {
		return err
	}
	f, err := os.Open(path)
	if err != nil {
		return errors.New("the file could not be read on the Mac")
	}
	defer f.Close()
	q := url.Values{"network": {w.Network}, "chat_id": {w.Chat}, "message_id": {w.Message}, "attachment": {w.Att}}
	if err := b.oneDo(ctx, "PUT", "/api/bridge/files?"+q.Encode(), f, size, nil); err != nil {
		return errors.New("ONE did not take the file (" + err.Error() + ")")
	}
	return nil
}

// uploadMissing hands ONE the attachments it asked for. One that cannot be
// fetched now is kept as waiting, with why, and tried again every round: it is
// never dropped, and ONE's page says that files wait.
func (b *Bridge) uploadMissing(ctx context.Context, missing []wMissing) {
	done := 0
	for _, m := range missing {
		key := attKey(m.Network, m.ChatID, m.MessageID, m.Attachment)
		info, ok := b.atts[key]
		if w := b.st.Files[key]; w != nil {
			info = attInfo{ID: w.Att, Src: w.Src}
			ok = true
		}
		if !ok {
			continue
		}
		// The id ONE announced the file under is the id Beeper gave it.
		w := &fileWait{Network: m.Network, Chat: m.ChatID, Message: m.MessageID, Att: m.Attachment, Src: info.Src}
		if err := b.putFile(ctx, w); err != nil {
			w.Reason = oneLine(err.Error(), 160)
			b.st.Files[key] = w
			continue
		}
		delete(b.st.Files, key)
		done++
	}
	if len(missing) > 0 {
		b.log.Printf("attachments asked=%d uploaded=%d waiting=%d", len(missing), done, len(b.st.Files))
		b.st.save()
	}
}

// retryFiles tries again the attachments that waited. A few each round.
func (b *Bridge) retryFiles(ctx context.Context) {
	if len(b.st.Files) == 0 {
		return
	}
	keys := make([]string, 0, len(b.st.Files))
	for k := range b.st.Files {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if len(keys) > 10 {
		keys = keys[:10]
	}
	changed := false
	for _, k := range keys {
		w := b.st.Files[k]
		if err := b.putFile(ctx, w); err != nil {
			if r := oneLine(err.Error(), 160); r != w.Reason {
				w.Reason, changed = r, true
			}
			continue
		}
		delete(b.st.Files, k)
		changed = true
	}
	if changed {
		b.st.save()
	}
}

// --- what the owner approved and what he wrote himself ---

func (b *Bridge) outbox(ctx context.Context) error {
	var out wOutbox
	if err := b.post(ctx, "/api/bridge/outbox", map[string]any{}, &out); err != nil {
		return err
	}
	b.learn(out.wLimits)
	for _, r := range out.Replies {
		b.sendReply(ctx, r)
	}
	for _, p := range out.Posts {
		b.sendPost(ctx, p)
	}
	if len(out.Resend) > 0 {
		return b.resend(ctx, out.Resend)
	}
	return nil
}

// begin writes the intent to disk before anything leaves, so a crash in
// between is reported as "may have gone" and never as a reason to send again.
// It says false when this one was handled before.
func (b *Bridge) begin(key string) (*replyState, bool) {
	if _, done := b.st.Replies[key]; done {
		return nil, false
	}
	rs := &replyState{State: "sending"}
	b.st.Replies[key] = rs
	if err := b.st.save(); err != nil {
		// Without a note on disk nothing is sent.
		rs.State, rs.Error = "failed", "The Mac could not keep its place, so nothing was sent."
		return rs, false
	}
	return rs, true
}

// sendReply sends one approved reply, at most once.
func (b *Bridge) sendReply(ctx context.Context, r wReply) {
	key := strconv.FormatInt(r.ID, 10)
	if _, done := b.st.Replies[key]; done {
		return
	}
	if r.Text == "" || b.st.Chats[r.Network+"/"+r.ChatID] == nil {
		b.st.Replies[key] = &replyState{State: "failed", Error: "The Mac does not know this chat, so nothing was sent."}
		b.st.save()
		return
	}
	rs, ok := b.begin(key)
	if !ok {
		return
	}
	b.transmit(ctx, rs, r.ChatID, r.Text, nil, "")
	b.st.save()
	b.log.Printf("reply handled state=%s", rs.State)
}

// sendPost sends one message the owner wrote himself: his exact text, or one file,
// as a message of its own. If a message before it in the same write did not go,
// this one does not.
func (b *Bridge) sendPost(ctx context.Context, p wPost) {
	key := "p" + strconv.FormatInt(p.ID, 10)
	if _, done := b.st.Replies[key]; done {
		return
	}
	switch {
	case b.st.FailedGroups[p.Group]:
		b.st.Replies[key] = &replyState{State: "failed", Error: "Not sent: the message before it did not go."}
		b.st.save()
		return
	case b.st.Chats[p.Network+"/"+p.ChatID] == nil || (p.Kind == "text" && p.Text == "") || (p.Kind == "file" && p.File == nil) || (p.Kind != "text" && p.Kind != "file"):
		b.st.Replies[key] = &replyState{State: "failed", Error: "The Mac does not know this chat, so nothing was sent."}
		b.st.FailedGroups[p.Group] = true
		b.st.save()
		return
	}
	rs, ok := b.begin(key)
	if !ok {
		return
	}
	if p.Kind == "file" {
		b.transmit(ctx, rs, p.ChatID, "", &wFileOut{Name: p.File.Name, Type: p.File.Type, Size: p.File.Size}, "/api/bridge/posts/"+strconv.FormatInt(p.ID, 10)+"/file")
	} else {
		b.transmit(ctx, rs, p.ChatID, p.Text, nil, "")
	}
	if rs.State == "failed" || rs.State == "unknown" {
		b.st.FailedGroups[p.Group] = true
	}
	b.st.save()
	b.log.Printf("post handled state=%s", rs.State)
}

type wFileOut struct {
	Name string
	Type string
	Size int64
}

// transmit does the sending once the intent is on disk: a file is first
// handed to Beeper's upload call (streamed from ONE), then the message is sent;
// Beeper answers with a provisional id and the chat it really went to, and the
// message is then watched until Beeper says it went or that it cannot.
func (b *Bridge) transmit(ctx context.Context, rs *replyState, chatID, text string, file *wFileOut, fileURL string) {
	var att *sendAttachment
	if file != nil {
		body, err := b.oneReq(ctx, "GET", fileURL, nil, -1)
		if err != nil {
			rs.State, rs.Error = "failed", "The file could not be fetched from ONE ("+err.Error()+")."
			return
		}
		id, err := b.beeper.Upload(ctx, file.Name, file.Type, body.Body)
		body.Body.Close()
		if err != nil {
			rs.State, rs.Error = "failed", "Beeper did not take the file ("+err.Error()+")."
			return
		}
		att = &sendAttachment{UploadID: id, FileName: file.Name, MimeType: file.Type, Type: attachmentType(file.Type)}
	}
	pending, chat, definite, err := b.beeper.Send(ctx, chatID, text, att)
	switch {
	case err == nil:
		rs.State, rs.Message, rs.Chat, rs.Since = "watching", pending, chat, b.now().Unix()
		b.st.save() // accepted: from now on it is only watched, never sent again
		b.resolve(ctx, rs)
	case definite:
		rs.State, rs.Error = "failed", "Beeper refused the message ("+err.Error()+")."
	default:
		rs.State, rs.Error = "unknown", "Beeper did not answer; the message may have gone."
	}
}

// resolve reads a message Beeper accepted and says what became of it: sent
// (SUCCESS), failed with its reason (FAIL_PERMANENT), or still going out
// (PENDING, FAIL_RETRIABLE: Beeper tries again by itself, the bridge never does).
func (b *Bridge) resolve(ctx context.Context, rs *replyState) {
	m, err := b.beeper.Message(ctx, rs.Chat, rs.Message)
	var st errStatus
	switch {
	case errors.As(err, &st) && int(st) == 404:
		// The provisional id is gone: Beeper has the message under its final id.
		rs.State = "sent"
		return
	case err != nil:
		if b.now().Unix()-rs.Since > int64(watchFor/time.Second) {
			rs.State, rs.Error = "unknown", "Beeper did not say whether the message went."
		}
		return
	}
	switch s := m.SendStatus; {
	case s == nil || s.Status == "SUCCESS" || s.Status == "":
		rs.State = "sent"
		if m.ID != "" {
			rs.Message = m.ID
		}
	case s.Status == "FAIL_PERMANENT":
		why := s.Reason
		if why == "" {
			why = s.Message
		}
		if why == "" {
			why = "Beeper could not send it"
		}
		rs.State, rs.Error = "failed", "Beeper could not send it: "+oneLine(why, 160)+"."
	default: // PENDING or FAIL_RETRIABLE
		if b.now().Unix()-rs.Since > int64(watchFor/time.Second) {
			rs.State, rs.Error = "unknown", "Beeper has not said whether the message went."
		}
	}
}

// watchSends looks again at the messages Beeper accepted and did not yet say
// went.
func (b *Bridge) watchSends(ctx context.Context) {
	changed := false
	keys := make([]string, 0)
	for k, rs := range b.st.Replies {
		if rs.State == "watching" {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	for _, k := range keys {
		rs := b.st.Replies[k]
		before := rs.State
		b.resolve(ctx, rs)
		if rs.State != before {
			changed = true
		}
	}
	if changed {
		b.st.save()
	}
}

// reportAll tells ONE what became of every reply and every message of the owner's
// that it has not yet been told.
func (b *Bridge) reportAll(ctx context.Context) error {
	ids := make([]string, 0)
	for id, rs := range b.st.Replies {
		if rs.Reported || rs.State == "watching" {
			continue
		}
		if rs.State == "sending" { // we stopped in the middle: it may have gone, never send it again
			rs.State, rs.Error = "unknown", "The bridge stopped while sending; the message may have gone."
		}
		ids = append(ids, id)
	}
	sort.Strings(ids)
	var first error
	for _, id := range ids {
		rs := b.st.Replies[id]
		body := map[string]any{"state": rs.State, "error": rs.Error, "message_id": rs.Message}
		path := "/api/bridge/outbox/" + id + "/result"
		if strings.HasPrefix(id, "p") {
			path = "/api/bridge/posts/" + strings.TrimPrefix(id, "p") + "/result"
		}
		err := b.post(ctx, path, body, nil)
		var st errStatus
		if err == nil || (errors.As(err, &st) && (int(st) == 404 || int(st) == 409)) {
			// ONE has it, or will never want it: do not tell again.
			rs.Reported = true
			continue
		}
		if first == nil {
			first = err
		}
	}
	if len(ids) > 0 {
		b.st.save()
	}
	return first
}

// resend brings back messages the owner asked to import again.
func (b *Bridge) resend(ctx context.Context, asks []wMissing) error {
	byChat := map[string][]string{}
	for _, a := range asks {
		k := a.Network + "\x00" + a.ChatID
		byChat[k] = append(byChat[k], a.MessageID)
	}
	chats, err := b.allChats(ctx, time.Time{})
	if err != nil {
		return err
	}
	var batch []wChat
	for _, c := range chats {
		ids := byChat[b.networkOf(c)+"\x00"+c.ID]
		if len(ids) == 0 {
			continue
		}
		var pick []bMessage
		for _, id := range ids {
			m, err := b.beeper.Message(ctx, c.ID, id)
			if err != nil {
				var st errStatus
				if errors.As(err, &st) && int(st) == 404 {
					continue
				}
				return err
			}
			pick = append(pick, m)
		}
		if len(pick) > 0 {
			batch = append(batch, b.wireChat(c, pick))
		}
	}
	if len(batch) == 0 {
		return nil
	}
	return b.send(ctx, batch, nil, true)
}

// Run does rounds until the context ends, backing off on errors.
func (b *Bridge) Run(ctx context.Context) {
	failures := 0
	for {
		err := b.Cycle(ctx)
		wait := b.cfg.Poll
		if err != nil {
			failures++
			wait = backoff(b.cfg.Poll, failures)
		} else {
			failures = 0
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}

func backoff(base time.Duration, failures int) time.Duration {
	d := base
	for i := 1; i < failures && d < 5*time.Minute; i++ {
		d *= 2
	}
	if d > 5*time.Minute {
		d = 5 * time.Minute
	}
	return d
}
