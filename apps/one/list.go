package main

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"lab/apps/one/repo"
	"lab/platform/appkit"
)

// The list page: five stages in the design, four of them on the list (Closed
// has a page of its own, history is not listed). Everything here puts text
// from mail into templates, which escape it; nothing is built as markup.

const (
	groupPage    = 50      // rows a group loads at a time
	maxShownText = 100_000 // bytes of one message's text shown; the original stays in the pool
	maxNote      = 4000
)

var listedGroups = []string{stageNew, stageInProgress, stagePending, stageResolved}

// --- pure rules ---

// ownerAction is what the owner can do to an item from a row or its thread.
type ownerAction struct {
	To    string // stage after
	Event string // event type written
}

// nextForOwner is SPEC 3 as one table. ok is false when the action does not
// fit the item's stage.
func nextForOwner(action, stage string) (ownerAction, bool) {
	switch action {
	case "open": // opening a row in New makes the item In progress
		if stage == stageNew {
			return ownerAction{stageInProgress, "opened"}, true
		}
	case "pending": // Mark pending
		if stage == stageNew || stage == stageInProgress {
			return ownerAction{stagePending, "pending_set"}, true
		}
	case "resolve": // Resolve
		if stage == stageNew || stage == stageInProgress || stage == stagePending {
			return ownerAction{stageResolved, "resolved"}, true
		}
	case "reopen": // Reopen
		switch stage {
		case stagePending:
			return ownerAction{stageInProgress, "pending_cleared"}, true
		case stageResolved:
			return ownerAction{stageInProgress, "unresolved"}, true
		}
	}
	return ownerAction{}, false
}

func plural(n int64, unit string) string {
	if n == 1 {
		return fmt.Sprintf("1 %s", unit)
	}
	return fmt.Sprintf("%d %ss", n, unit)
}

// waited says for how long something has waited: days, or hours under a day.
func waited(since, now int64) string {
	d := now - since
	if d < 0 {
		d = 0
	}
	if d < 86400 {
		return plural(max(d/3600, 1), "hour")
	}
	return plural(d/86400, "day")
}

func headingText(counts map[string]int) string {
	n, m := counts[stageNew], counts[stageInProgress]
	if n == 0 && m == 0 {
		return "Nothing needs you"
	}
	return fmt.Sprintf("%d new, %d in progress", n, m)
}

func newItemsText(n int) string {
	if n == 1 {
		return "1 new item"
	}
	return fmt.Sprintf("%d new items", n)
}

func colorOf(key string) string {
	h := fnv.New32a()
	h.Write([]byte(strings.ToLower(key)))
	return "c" + strconv.Itoa(int(h.Sum32()%12))
}

var sourceNames = map[string]string{
	"gmail": "Gmail", "outlook": "Office mail", "messenger": "Messenger", "whatsapp": "WhatsApp",
	"imessage": "iMessage", "sms": "SMS", "calendar": "Calendar",
}

func sourceLabel(src string) string {
	if n, ok := sourceNames[src]; ok {
		return n
	}
	return src
}

// badge is the two letters shown for a source without a picture.
func badge(label string) string {
	words := strings.Fields(label)
	var r []rune
	switch {
	case len(words) == 0:
		return ""
	case len(words) == 1:
		r = []rune(words[0])
		if len(r) > 2 {
			r = r[:2]
		}
	default:
		r = []rune{[]rune(words[0])[0], []rune(words[1])[0]}
	}
	return strings.ToUpper(string(r))
}

// snippet is the beginning of a text on one line.
func snippet(text string, n int) string {
	text = strings.Join(strings.Fields(text), " ")
	if utf8.RuneCountInString(text) <= n {
		return text
	}
	return string([]rune(text)[:n]) + "…"
}

// --- views ---

type groupView struct {
	Stage   string
	Name    string
	Count   int
	Wait    string // on Pending: "longest wait 3 days"
	Approve string // "2 to approve" when Claude has proposals in the group
}

type listView struct {
	Heading string
	Empty   bool
	Groups  []groupView
	Since   int64
	Waiting string // "3 waiting for Claude", empty when none
	// Proposals is the strip about proposals that concern people (SPEC 25);
	// empty when none waits. Its link leads to where they are decided.
	Proposals string
	// Night is the morning line about the night check (SPEC 31); empty when none or dismissed.
	Night string
	// Todos is the number of open to-dos (SPEC 12 small item); 0 hides the link.
	Todos int
}

// propView is the chip on a closed row: what Claude proposes.
type propView struct {
	ID    int64
	Text  string
	By    string
	Why   string // the reason the agent gave, one line of plain text; empty when there is none
	More  int    // further proposals waiting on the same row
	Reply bool   // a reply is approved from its box, where its text is shown, never from the chip
}

type actionView struct{ Path, Label string }

type rowView struct {
	ID         int64
	Number     string
	Stage      string
	StageLabel string
	Who        string
	Color      string
	Source     string
	Badge      string
	BadgeColor string
	BadgePic   string // the owner's own picture of the source, when he set one (SPEC 23)
	Subject    string
	Snippet    string
	When       string
	WhenFull   string
	ISO        string
	Count      int
	Pending    string
	HasFiles   bool   // the paperclip on the closed row (SPEC 7)
	Pulse      string // what the open row of a chat looks like now: when it changes, the page fetches the row again
	Live       bool   // a chat that is still being written in (SPEC 5, 17)
	Chat       bool   // the row is a chat: an open one asks the server for new messages
	Actions    []actionView
	Quick      *actionView // on a phone a closed row carries one icon: Resolve, or Reopen in Resolved (SPEC 5)
	Open       bool
	Prop       *propView
	Body       *bodyView
}

type fileView struct {
	Name  string
	Size  string
	URL   string
	Image bool
}

type msgView struct {
	Source   string // where the message came from; set only on a row that holds more than one source (SPEC 10)
	Name     string
	Color    string
	Picture  string // the owner's picture of the sender, when he set one
	Subject  string
	When     string
	WhenFull string
	ISO      string
	Text     string
	Edited   bool // the text was edited after it was first seen (SPEC 4)
	Deleted  bool // the network says it was deleted; ONE keeps it, marked (SPEC 30)
	Cut      bool
	Long     bool // more than foldLines lines: shown folded, with Show all
	Files    []fileView
	Pictures []fileView
}

// A message is shown in full, because the owner must read what was written
// before he reads what Claude says about it (SPEC 5, 2026-11-19). Only a text
// of more than foldLines lines is folded and opens with Show all.
const (
	foldLines   = 40
	foldColumns = 80 // about how many characters fit on one line of the text box
)

// isLongText says whether a text runs over foldLines lines as it will be
// shown: every line break counts, and so does every line that wraps.
func isLongText(s string) bool {
	lines := 0
	for _, l := range strings.Split(s, "\n") {
		n := utf8.RuneCountInString(strings.TrimRight(l, "\r"))
		lines += 1 + max(0, n-1)/foldColumns
		if lines > foldLines {
			return true
		}
	}
	return false
}

// replyView is a reply proposal in the thread: the box that shows the exact
// text, and where it goes (SPEC 13).
type replyView struct {
	Label    string // Will be sent · reply to the mail
	Text     string
	To, Cc   []string
	Editable bool // it waits for Approve: the text is in a box the owner can edit
	Hidden   bool // the sent mail came back and is in the thread: its text is not shown twice
}

type entryView struct {
	Reply    *replyView
	Kind     string // note, comment, summary, proposal, result or notice
	Notice   bool
	By       string
	Mark     string // Summary, Proposal, Withdrawn or Result
	Agent    bool
	Text     string
	Attached []string // files this entry added (SPEC 7); they are listed under Attachments
	Reason   string
	PropID   int64
	Waiting  bool  // a proposal that waits for Approve
	Withdraw int64 // a chat reply that waits for the Mac: the button that takes it back
	When     string
	WhenFull string
	ISO      string
}

type bodyView struct {
	Live     bool
	Chat     bool
	ID       int64
	Number   string
	Who      string
	Color    string
	Source   string
	Badge    string
	BadgeC   string
	BadgePic string
	When     string
	Count    int
	Subject  string
	Older    int
	OlderL   string
	Msgs     []msgView
	Entries  []entryView
	Error    string
	// What the owner wrote in this chat and has not yet seen come back, the
	// Attachments of the row (SPEC 7) and the box he writes in (SPEC 17).
	Out         []outView
	Attachments []attView
	Write       *writeView
	Actions     []actionView // the row's own actions: on a phone they are icons in the bar at the bottom (SPEC 5)
	// Meeting is the header of a meeting thread (SPEC 33): its entry's title, time, place and calendar.
	Meeting *meetingView
	// ClosedSummary is the list of the row that sums up what Claude closed (SPEC 27).
	ClosedSummary *closedSummaryView
}

// writeView is the box under the messages of a chat.
type writeView struct {
	Action      string
	Placeholder string
}

func (a *app) whenText(ts int64, now time.Time) (short, full, iso string) {
	t := time.Unix(ts, 0).In(a.loc)
	n := now.In(a.loc)
	switch {
	case t.Year() == n.Year() && t.YearDay() == n.YearDay():
		short = t.Format("15:04")
	case t.Year() == n.Year():
		short = t.Format("2 Jan")
	default:
		short = t.Format("2 Jan 2006")
	}
	return short, t.Format("2 Jan 2006 15:04"), time.Unix(ts, 0).UTC().Format(time.RFC3339)
}

func actionsFor(id int64, stage string, hasTodo bool) []actionView {
	p := "/items/" + strconv.FormatInt(id, 10) + "/"
	var out []actionView
	if _, ok := nextForOwner("pending", stage); ok {
		out = append(out, actionView{p + "pending", "Mark pending"})
	}
	if _, ok := nextForOwner("resolve", stage); ok {
		out = append(out, actionView{p + "resolve", "Resolve"})
	}
	if _, ok := nextForOwner("reopen", stage); ok {
		out = append(out, actionView{p + "reopen", "Reopen"})
	}
	// SPEC 19: a to-do from a row; one that is still open makes the button redundant.
	if !hasTodo && (stage == stageNew || stage == stageInProgress || stage == stagePending || stage == stageResolved) {
		out = append(out, actionView{p + "todo", "To do"})
	}
	return out
}

func (a *app) rowFor(r repo.ListRow, now time.Time) rowView {
	who := strings.TrimSpace(r.SenderName)
	if who == "" {
		who = r.Sender
	}
	if who == "" {
		who = "You"
	}
	key := r.Sender
	if key == "" {
		key = who
	}
	label := sourceLabel(r.Source)
	short, full, iso := a.whenText(r.LastAt, now)
	v := rowView{
		ID: r.ID, Number: repo.Number(r.Number), Stage: r.Stage, StageLabel: stageLabels[r.Stage],
		Who: oneLine(who, 80), Color: colorOf(key), Source: label, Badge: badge(label), BadgeColor: colorOf(r.Source), BadgePic: a.sourcePicURL(r.Source),
		Subject: oneLine(r.Subject, 300), Snippet: snippet(r.Snippet, 120),
		When: short, WhenFull: full, ISO: iso, Count: r.Count, Actions: actionsFor(r.ID, r.Stage, r.HasTodo),
		Live: r.ChatSince > 0, Chat: r.Kind == "chat", HasFiles: r.HasFiles,
	}
	if r.Kind == repo.ClosedSummaryKind { // a row of ONE's own: not from anyone
		v.Who, v.Color, v.Source, v.Badge, v.BadgePic = "ONE", colorOf("one"), "ONE", "ON", ""
	}
	want := "Resolve"
	if r.Stage == stageResolved {
		want = "Reopen"
	}
	for i := range v.Actions {
		if v.Actions[i].Label == want {
			v.Quick = &v.Actions[i]
		}
	}
	if r.Stage == stagePending && r.PendingSince > 0 {
		v.Pending = "waiting " + waited(r.PendingSince, now.Unix())
	}
	if r.ProposalID != 0 {
		v.Prop = &propView{ID: r.ProposalID, Text: a.proposalLabelDue(r.ProposalAction, r.ProposalText, r.ProposalDue), By: actorLabel(r.ProposalBy), More: r.Proposals - 1,
			Why: oneLine(r.ProposalReason, 400), Reply: isReplyAction(r.ProposalAction)}
		if r.ProposalAction == replyChat {
			// The channel the reply goes to, not the one the newest message of a joined row came from.
			net := parseRecipients(r.ProposalTo).Network
			if net == "" {
				net = r.Source
			}
			v.Prop.Text = strings.ToUpper(chatChannel(net)[:1]) + chatChannel(net)[1:]
		}
	}
	return v
}

func (a *app) listViewFor(ctx context.Context) (listView, error) {
	a.releaseChats(ctx) // a chat whose quiet time is over goes to Claude now
	counts, err := a.store.StageCounts(ctx)
	if err != nil {
		return listView{}, err
	}
	v := listView{Heading: headingText(counts), Since: a.store.LastEventID(ctx), Waiting: waitingText(a.store.CountWaiting(ctx)), Proposals: a.peopleStrip(ctx), Night: a.nightLineText(ctx), Todos: a.store.CountOpenTodos(ctx)}
	for _, st := range listedGroups {
		v.Groups = append(v.Groups, a.groupFor(ctx, st, counts))
	}
	total := 0
	for _, g := range v.Groups {
		total += g.Count
	}
	v.Empty = total == 0
	return v, nil
}

// todoText is the link in the top line: how many to-dos are open; nothing
// with none. It opens the agenda at the to-dos.
func todoText(n int) string {
	if n <= 0 {
		return ""
	}
	return fmt.Sprintf("%d to do", n)
}

// waitingText is the strip about what waits for Claude (SPEC 11).
func waitingText(n int) string {
	if n <= 0 {
		return ""
	}
	return fmt.Sprintf("%d waiting for Claude", n)
}

// peopleStrip says that proposals about people wait for the owner (join two
// people, stop always showing someone), which no group header can show
// because they belong to no item. It counts them with the proposals the group
// headers show: "3 to approve, 1 about people, 1 about rules". Empty when none
// about people or rules waits.
func (a *app) peopleStrip(ctx context.Context) string {
	p := len(a.personProposalViews(ctx))
	r := a.store.CountRuleProposals(ctx) + a.store.CountSkipRuleProposals(ctx) // SPEC 21, 31: proposed rules belong to no item either
	st := a.store.CountSettingProposals(ctx)                                   // SPEC 22: so do proposed changes of a setting
	if p+r+st == 0 {
		return ""
	}
	total := p + r + st
	for _, n := range a.store.ProposalCounts(ctx) {
		total += n
	}
	s := fmt.Sprintf("%d to approve", total)
	if p > 0 {
		s += fmt.Sprintf(", %d about people", p)
	}
	if r > 0 {
		s += fmt.Sprintf(", %d about rules", r)
	}
	if st > 0 {
		s += fmt.Sprintf(", %d about settings", st)
	}
	return s
}

func approveText(n int) string {
	if n <= 0 {
		return ""
	}
	return fmt.Sprintf("%d to approve", n)
}

func (a *app) groupFor(ctx context.Context, stage string, counts map[string]int) groupView {
	g := groupView{Stage: stage, Name: stageLabels[stage], Count: counts[stage], Approve: approveText(a.store.ProposalCounts(ctx)[stage])}
	if stage == stagePending && g.Count > 0 {
		if since := a.store.OldestPending(ctx); since > 0 {
			g.Wait = "longest wait " + waited(since, a.now().Unix())
		}
	}
	return g
}

// bodyFor builds the open part of a row: the messages of the last month (or
// all of them), and the thread of the owner's notes and notices.
func (a *app) bodyFor(ctx context.Context, r repo.ListRow, all bool) (*bodyView, error) {
	now := a.now()
	row := a.rowFor(r, now)
	rv := []rowView{row}
	a.dress(ctx, rv, []repo.ListRow{r})
	row = rv[0]
	b := &bodyView{Live: row.Live, Chat: row.Chat, ID: r.ID, Number: row.Number, Who: row.Who, Color: row.Color, Source: row.Source, Badge: row.Badge,
		BadgeC: row.BadgeColor, BadgePic: row.BadgePic, When: row.WhenFull, Count: r.Count, Subject: row.Subject, Actions: row.Actions}
	var since int64
	if !all {
		// The window counts back from the newest message, so a chat that went
		// quiet still shows its last month.
		if newest := a.store.NewestMessageAt(ctx, r.ID); newest > 0 {
			since = newest - int64(a.syncSettings(ctx).ReviewWindow/time.Second)
		}
	}
	msgs, older, err := a.store.MessagesSince(ctx, r.ID, since)
	if err != nil {
		return nil, err
	}
	files, err := a.store.FilesOfItem(ctx, r.ID)
	if err != nil {
		return nil, err
	}
	deleted, err := a.store.DeletedMessages(ctx, r.ID)
	if err != nil {
		return nil, err
	}
	b.Older = older
	b.OlderL = plural(int64(older), "older message")
	var hs []repo.Handle
	for _, m := range msgs {
		if m.Sender != "" {
			hs = append(hs, repo.Handle{Network: m.Network, Value: strings.ToLower(m.Sender)})
		}
	}
	people, _ := a.store.PeopleByHandles(ctx, hs)
	prevSubject := row.Subject // the row line above says it already
	sources := map[string]bool{}
	for _, m := range msgs {
		sources[m.Source] = true
	}
	for _, m := range msgs {
		name := strings.TrimSpace(m.SenderName)
		if name == "" {
			name = m.Sender
		}
		short, full, iso := a.whenText(m.ReceivedAt, now)
		mv := msgView{Name: oneLine(name, 120), Color: colorOf(m.Sender + name), When: short, WhenFull: full, ISO: iso}
		if len(sources) > 1 {
			mv.Source = sourceLabel(m.Source)
		}
		// SPEC 25: a person's messages show the name the owner calls them, one
		// colour and the picture he set.
		if p, ok := people[repo.Handle{Network: m.Network, Value: strings.ToLower(m.Sender)}]; ok && !m.SentByMe {
			mv.Name, mv.Color = oneLine(p.Shown, 120), colorOf("person"+strconv.FormatInt(p.ID, 10))
			if p.HasPicture {
				mv.Picture = "/people/" + strconv.FormatInt(p.ID, 10) + "/picture"
			}
		}
		if s := oneLine(m.Subject, 300); s != prevSubject {
			mv.Subject = s
			prevSubject = s
		}
		mv.Text = m.Text
		mv.Edited = m.Edited
		mv.Deleted = deleted[m.ID]
		mv.Long = isLongText(mv.Text)
		if len(mv.Text) > maxShownText {
			mv.Text = strings.ToValidUTF8(mv.Text[:maxShownText], "")
			mv.Cut = true
		}
		for _, f := range files[m.ID] {
			fv := fileView{Name: oneLine(f.Filename, 200), Size: fileSize(f.Size), URL: "/files/" + strconv.FormatInt(f.ID, 10)}
			if fv.Name == "" {
				fv.Name = "file"
			}
			if f.Status != "saved" {
				fv.URL = "" // not downloaded yet: named, not linked
			}
			if fv.URL != "" && isPicture(f) {
				fv.Image = true
				mv.Pictures = append(mv.Pictures, fv)
				continue
			}
			mv.Files = append(mv.Files, fv)
		}
		b.Msgs = append(b.Msgs, mv)
	}
	entries, err := a.threadEntries(ctx, r.ID, now)
	if err != nil {
		return nil, err
	}
	b.Entries = entries
	if row.Chat {
		if chat, ok := a.store.ChatOfItem(ctx, r.ID); ok {
			b.Write = &writeView{Action: "/items/" + strconv.FormatInt(r.ID, 10) + "/write", Placeholder: "Write in " + sourceLabel(chat.Network) + "…"}
		}
		b.Out = a.outViews(ctx, r.ID, now)
	}
	b.Attachments = a.attachmentsOf(ctx, r.ID, files, msgs, now)
	if r.Kind == meetingKind {
		b.Meeting = a.meetingViewFor(ctx, r.ID)
	}
	if r.Kind == repo.ClosedSummaryKind {
		b.ClosedSummary = a.closedSummaryViewFor(ctx, r.ID)
	}
	return b, nil
}

// attachmentsOf lists every file that belongs to a row in one place, oldest
// first so the newest is last, each with who added it (SPEC 7): the files that
// came with any message of the conversation and the files the owner attached
// himself. A file the owner sent that has come back as a message is listed once,
// as the message's.
func (a *app) attachmentsOf(ctx context.Context, itemID int64, files map[int64][]repo.File, shown []repo.Message, now time.Time) []attView {
	names := map[int64]string{}
	sent := map[int64]int64{}
	for _, m := range shown {
		n := strings.TrimSpace(m.SenderName)
		if n == "" {
			n = m.Sender
		}
		if m.SentByMe {
			n = "You"
		}
		names[m.ID] = n
		sent[m.ID] = m.ReceivedAt
	}
	back := map[int64]bool{} // files of the owner's that came back as a message
	if outs, err := a.store.OutOfItem(ctx, itemID); err == nil {
		for _, p := range outs {
			if p.FileID != 0 && p.MsgID != 0 {
				back[p.FileID] = true
			}
		}
	}
	type one struct {
		f  repo.File
		by string
		at int64 // when it was sent (a file of a message) or attached: never when ONE saved it
	}
	var all []one
	for mid, fs := range files {
		for _, f := range fs {
			if back[f.ID] {
				continue
			}
			at := f.AddedAt
			by := "You"
			if mid == 0 && f.Source != "owner" && f.Source != "" {
				by = actorLabel(f.Source) // a file an agent added to the item
			}
			if mid != 0 {
				n, ok := names[mid]
				if !ok {
					if m, found := a.store.MessageByID(ctx, mid); found {
						sent[mid] = m.ReceivedAt
						n = strings.TrimSpace(m.SenderName)
						if n == "" {
							n = m.Sender
						}
						if m.SentByMe {
							n = "You"
						}
					}
				}
				by = n
				if t, ok := sent[mid]; ok {
					at = t
				}
			}
			all = append(all, one{f, oneLine(by, 120), at})
		}
	}
	slices.SortFunc(all, func(x, y one) int {
		if c := cmp.Compare(x.at, y.at); c != 0 {
			return c
		}
		return cmp.Compare(x.f.ID, y.f.ID)
	})
	var out []attView
	for _, o := range all {
		v := attView{Name: oneLine(o.f.Filename, 200), Size: fileSize(o.f.Size), By: o.by, At: o.at}
		v.When, v.WhenFull, v.ISO = a.whenText(o.at, now)
		v.Dated = "attached" // by the owner or an agent in the thread
		if o.f.MessageID != 0 {
			v.Dated = "sent" // it came with a message
		}
		if v.Name == "" {
			v.Name = "file"
		}
		if o.f.Status == "saved" {
			v.URL = "/files/" + strconv.FormatInt(o.f.ID, 10)
		}
		out = append(out, v)
	}
	return out
}

// threadTypes: the events the thread shows, oldest first.
var threadEventTypes = []string{"feedback", "noted", "commented", "file_added", "proposal_made", "proposal_decided", "pending_set", "resolved", "unresolved", "pending_cleared", "closed", "brought_back", "history_reviewed", "history_attention", "todo_made", "skipped", "reply_sent", "reply_failed", "chat_settled", "send_withdrawn", "write_withdrawn", "same_mail", "model_failed", "rows_joined", "row_split", "row_split_off", meetingNoticeEv}

type entryData struct {
	Text     string   `json:"text"`
	Kind     string   `json:"kind"`
	Reason   string   `json:"reason"`
	Proposal int64    `json:"proposal"`
	Decision string   `json:"decision"`
	Action   string   `json:"action"`
	OnNote   bool     `json:"on_note"`
	Auto     bool     `json:"auto"`
	Days     int      `json:"days"`
	NoteName string   `json:"note_title"`
	Files    []string `json:"files"`
	RuleName string   `json:"rule_name"`
	Number   string   `json:"number"`
	SameAs   string   `json:"same_as"`
	As       string   `json:"as"`
	Why      string   `json:"why"`
	Summary  string   `json:"summary"`
}

var resultText = map[string]string{"pending": "Marked as pending.", "resolve": "Marked as resolved.", "close": "Closed.", "todo": "Added to the to-dos."}

// proposalLabel says what a proposal asks for: the action, and for a to-do its words.
func proposalLabel(action, text string) string {
	if isReplyAction(action) {
		return replyChipLabel(action)
	}
	if action == "todo" {
		return "To do: " + oneLine(text, 200)
	}
	switch action {
	case actionMerge:
		return "Join " + text + " into this row"
	case actionSplit:
		return "Split off: " + oneLine(text, 120)
	}
	return proposalActions[action]
}

// proposalLabelDue is proposalLabel with the time or due date of a to-do.
func (a *app) proposalLabelDue(action, text, due string) string {
	l := proposalLabel(action, text)
	if action == "todo" && due != "" {
		if d := a.dueShown(due); d != "" {
			l += " · " + d
		}
	}
	return l
}

// threadEntries builds what the thread shows: The owner's notes, comments and
// summaries (only the newest summary), proposals with their state, results
// and notices. Everything is plain text for the template to escape.
func (a *app) threadEntries(ctx context.Context, itemID int64, now time.Time) ([]entryView, error) {
	events, err := a.store.EntriesOfItem(ctx, itemID, threadEventTypes...)
	if err != nil {
		return nil, err
	}
	props, err := a.store.ProposalsOfItem(ctx, itemID)
	if err != nil {
		return nil, err
	}
	datas := make([]entryData, len(events))
	var lastSummary int64
	for i, e := range events {
		json.Unmarshal([]byte(e.Data), &datas[i])
		if e.Type == "commented" && datas[i].Kind == "summary" {
			lastSummary = e.ID // one current summary; the old ones stay in the record
		}
	}
	var out []entryView
	for i, e := range events {
		d := datas[i]
		ev := entryView{By: actorLabel(e.Actor), Agent: e.Actor != "owner"}
		ev.When, ev.WhenFull, ev.ISO = a.whenText(e.At, now)
		switch e.Type {
		case "feedback":
			// SPEC 29: The owner's feedback, in his words, under "Feedback from the owner",
			// with who recorded it (the name before the mark). It is his word, so
			// it is drawn like his own entries, not like an agent's comment.
			ev.Kind, ev.Text, ev.Mark, ev.Agent = "feedback", d.Text, "Feedback from the owner", false
		case "noted":
			ev.Kind, ev.Text, ev.Attached = "note", d.Text, d.Files
		case "file_added":
			ev.Kind, ev.Attached = "comment", d.Files
		case "commented":
			ev.Kind, ev.Text = "comment", d.Text
			if d.Kind == "summary" {
				if e.ID != lastSummary {
					continue
				}
				ev.Kind, ev.Mark = "summary", "Summary"
			}
		case "history_reviewed":
			// SPEC 34: the summary of an old thread, kept as its one summary.
			ev.Kind, ev.Mark, ev.Text = "summary", "Summary", d.Summary
		case "history_attention":
			ev.Notice, ev.Kind, ev.By, ev.Agent = true, "notice", "Claude", true
			ev.Text = historyNotice(d.Reason)
		case "proposal_made":
			p, ok := props[d.Proposal]
			if !ok {
				continue
			}
			ev.Kind, ev.Mark, ev.Text, ev.Reason, ev.PropID = "proposal", "Proposal", a.proposalLabelDue(p.Action, p.Text, p.Due), p.Reason, p.ID
			if isReplyAction(p.Action) {
				ev.Reply = replyViewOf(p)
				ev.Text = ""
			}
			switch p.Status {
			case "pending":
				ev.Waiting = true
			case "withdrawn":
				ev.Mark = "Withdrawn"
			}
		case "proposal_decided":
			ev.Notice = true
			ev.Kind = "notice"
			if d.Decision == "approved" {
				ev.Text = "You approved this proposal."
				out = append(out, ev)
				if isReplyAction(d.Action) {
					// What became of the send is told by its own entries; a chat reply
					// that has not gone yet says who it waits for.
					if p, ok := props[d.Proposal]; ok && p.Action == replyChat {
						if w, ok := chatWaitEntry(p, ev); ok {
							out = append(out, w)
						}
					}
					continue
				}
				if d.Action == actionMerge || d.Action == actionSplit {
					continue // the notice of the row says what was done
				}
				res := ev
				res.Notice, res.Kind, res.Mark, res.By, res.Agent = false, "result", "Result", "ONE", false
				res.Text = resultText[d.Action]
				ev = res
			} else {
				ev.Text = "You dismissed this proposal."
			}
		case "rows_joined", "row_split", "row_split_off":
			ev.Notice, ev.Kind, ev.By, ev.Agent = true, "notice", "ONE", false
			ev.Text = rowNoticeText(e.Type, e.Actor, d.Number)
		case meetingNoticeEv:
			ev.Notice, ev.Kind, ev.By, ev.Agent = true, "notice", "ONE", false
			ev.Text = d.Text // SPEC 33: the notice of a changed entry, in its exact words
		case "chat_settled":
			ev.Notice, ev.Kind, ev.By, ev.Agent = true, "notice", "ONE", false
			ev.Text = "Arrived settled."
		case "send_withdrawn":
			ev.Notice, ev.Kind, ev.Agent = true, "notice", false
			ev.Text = "You withdrew this reply."
		case "write_withdrawn":
			ev.Notice, ev.Kind, ev.Agent = true, "notice", false
			ev.Text = "You withdrew a message you wrote."
		case "reply_sent":
			ev.Text = "Sent " + time.Unix(e.At, 0).In(a.loc).Format("15:04") + "."
			ev.Kind, ev.Mark, ev.By, ev.Agent = "result", "Result", "ONE", false
		case "reply_failed":
			ev.Kind, ev.Mark, ev.By, ev.Agent = "result", "Result", "ONE", false
			ev.Text = "Could not send: " + d.Reason
		case "closed":
			if e.Actor == "owner" {
				continue // the result of an approved proposal says it
			}
			ev.Notice, ev.Kind = true, "notice"
			if d.Auto {
				ev.Text = closedAfterText(d.Days) // the app's own timer (SPEC 3)
				break
			}
			ev.Text = actorLabel(e.Actor) + " closed this on your note."
		case "same_mail":
			ev.Notice, ev.Kind, ev.By, ev.Agent = true, "notice", "ONE", false
			ev.Text = "Same mail as " + d.As + "."
		case "model_failed":
			ev.Notice, ev.Kind, ev.By, ev.Agent = true, "notice", "ONE", false
			ev.Text = d.Why
		case "skipped":
			if e.Actor == "rule" {
				ev.Notice, ev.Kind, ev.By, ev.Agent = true, "notice", "ONE", false
				ev.Text = ruleClosedText(d.RuleName)
				if d.SameAs != "" {
					ev.Text += ". Same mail as " + d.SameAs + "."
				}
				break
			}
			if d.NoteName == "" {
				continue // a plain skip shows on the Closed page, not in the thread
			}
			ev.Notice, ev.Kind = true, "notice"
			ev.Text = "Skipped by your note " + d.NoteName + "."
		default: // the owner's own marks; those of an approved proposal are told by its result
			if e.Type == "brought_back" && e.Actor == "nightcheck" {
				ev.Notice, ev.Kind, ev.By, ev.Agent = true, "notice", "ONE", false
				ev.Text = nightNotice(d.Reason)
				break
			}
			if e.Type == "brought_back" && e.Actor != "owner" && d.Reason != "" {
				ev.Notice, ev.Kind, ev.By, ev.Agent = true, "notice", "Claude", true
				ev.Text = "Claude: " + d.Reason
				break
			}
			if e.Actor != "owner" || d.Proposal != 0 {
				continue
			}
			ev.Notice, ev.Kind = true, "notice"
			ev.Text = map[string]string{
				"pending_set":     "You marked this pending.",
				"resolved":        "You resolved this.",
				"unresolved":      "You reopened this.",
				"pending_cleared": "You reopened this.",
				"brought_back":    "You brought this back.",
				"todo_made":       todoMade,
			}[e.Type]
		}
		out = append(out, ev)
	}
	return out, nil
}

// chatWaitEntry says where a chat reply stands while it has not gone: it waits
// for the Mac, with Withdraw, or the Mac has it and is sending it.
func chatWaitEntry(p repo.Proposal, base entryView) (entryView, bool) {
	e := base
	e.Notice, e.Kind, e.Mark, e.By, e.Agent, e.Reason = false, "result", "", "ONE", false, ""
	switch p.SendState {
	case "queued":
		e.Text, e.Withdraw, e.PropID = "Waiting for the Mac to send this.", p.ID, p.ID
	case "handed":
		e.Text = "The Mac has this reply and is sending it."
	default:
		return e, false
	}
	return e, true
}

// fileSize is a size for a file chip: bytes, KB or MB.
func fileSize(n int64) string {
	switch {
	case n < 1024:
		return fmt.Sprintf("%d B", max(n, 0))
	case n < 1<<20:
		return fmt.Sprintf("%.0f KB", float64(n)/1024)
	}
	return humanBytes(uint64(n))
}

// isPicture: only PNG, JPEG and GIF files of the pool are shown as pictures.
func isPicture(f repo.File) bool {
	switch strings.ToLower(f.ContentType) {
	case "image/png", "image/jpeg", "image/gif":
		return true
	}
	return false
}

// --- fragments ---

func (a *app) fragment(w http.ResponseWriter, name string, status int, data any) {
	var buf bytes.Buffer
	if err := a.pages.ExecuteTemplate(&buf, name, data); err != nil {
		appkit.Error("rendering a fragment", err)
		http.Error(w, "template error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	w.Write(buf.Bytes())
}

func itemID(r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	return id, err == nil && id > 0
}

// rowResponse writes one row, open or closed.
func (a *app) rowResponse(w http.ResponseWriter, r *http.Request, id int64, open, all bool) {
	ctx := r.Context()
	lr, ok := a.store.ListRowByID(ctx, id)
	if !ok || lr.Hidden { // a new item waits for Claude before the owner sees it
		http.NotFound(w, r)
		return
	}
	v := a.rowFor(lr, a.now())
	one := []rowView{v}
	a.dress(ctx, one, []repo.ListRow{lr})
	v = one[0]
	if open {
		body, err := a.bodyFor(ctx, lr, all)
		if err != nil {
			appkit.Error("building a row", err)
			http.Error(w, "The row could not be loaded.", http.StatusInternalServerError)
			return
		}
		v.Open, v.Body = true, body
	}
	a.fragment(w, "row", http.StatusOK, v)
}

// --- handlers ---

func (a *app) listGroup(w http.ResponseWriter, r *http.Request) {
	stage := r.PathValue("stage")
	known := false
	for _, s := range listedGroups {
		known = known || s == stage
	}
	if !known {
		http.NotFound(w, r)
		return
	}
	offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
	offset = max(offset, 0)
	rows, err := a.store.ListItems(r.Context(), stage, groupPage+1, offset)
	if err != nil {
		appkit.Error("listing a group", err)
		http.Error(w, "The list could not be loaded.", http.StatusInternalServerError)
		return
	}
	more := len(rows) > groupPage
	if more {
		rows = rows[:groupPage]
	}
	now := a.now()
	data := struct {
		Name  string
		Rows  []rowView
		First bool
		Next  int
		More  bool
	}{Name: stageLabels[stage], First: offset == 0, Next: offset + groupPage, More: more}
	for _, lr := range rows {
		data.Rows = append(data.Rows, a.rowFor(lr, now))
	}
	a.dress(r.Context(), data.Rows, rows)
	a.fragment(w, "group", http.StatusOK, data)
}

// listState is what the page needs to keep its numbers right and to offer
// the button for new items: no rows, so nothing can move.
func (a *app) listState(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	a.releaseChats(ctx)
	counts, err := a.store.StageCounts(ctx)
	if err != nil {
		http.Error(w, "unavailable", http.StatusInternalServerError)
		return
	}
	out := struct {
		Heading string            `json:"heading"`
		Counts  map[string]int    `json:"counts"`
		Wait    string            `json:"wait"`
		Waiting int               `json:"waiting"`
		Button  string            `json:"button"`
		Claude  string            `json:"claude"`
		Approve map[string]string `json:"approve"`
		Alerts  []string          `json:"alerts"`
		Closed  int               `json:"closed"`
		ClosedT string            `json:"closed_text"`
		People  string            `json:"people"`
		Night   string            `json:"night"`
		Todos   int               `json:"todos"`
		Todo    string            `json:"todo"`
	}{Heading: headingText(counts), Counts: map[string]int{}, Approve: map[string]string{}, Alerts: []string{}}
	out.Claude = waitingText(a.store.CountWaiting(ctx))
	out.Alerts = a.allAlerts(ctx, a.sourceStates(ctx))
	out.Night, out.People = a.nightLineText(ctx), a.peopleStrip(ctx) // every strip is sent; the page fits them to one line (see "+n more")
	out.Closed = a.store.ClosedCount(ctx)
	out.ClosedT = closedLinkText(out.Closed, a.store.CountUnseenClosed(ctx))
	out.Todos = a.store.CountOpenTodos(ctx)
	out.Todo = todoText(out.Todos)
	pc := a.store.ProposalCounts(ctx)
	for _, st := range listedGroups {
		out.Counts[st] = counts[st]
		out.Approve[st] = approveText(pc[st])
	}
	out.Wait = a.groupFor(ctx, stagePending, counts).Wait
	if since, err := strconv.ParseInt(r.URL.Query().Get("since"), 10, 64); err == nil && since >= 0 {
		out.Waiting = a.store.ArrivedSince(ctx, since)
		if out.Waiting > 0 {
			out.Button = newItemsText(out.Waiting)
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(out)
}

func (a *app) itemRow(w http.ResponseWriter, r *http.Request) {
	id, ok := itemID(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	a.rowResponse(w, r, id, r.URL.Query().Get("open") == "1", false)
}

// itemPulse answers what an open chat asks every few seconds (SPEC 17): one
// short text that changes when its messages or its thread change. A page that
// holds another one fetches the row again. No long-lived connection is used,
// so the lab's proxy and sandbox need to allow nothing special.
func (a *app) itemPulse(w http.ResponseWriter, r *http.Request) {
	id, ok := itemID(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	it, found := a.store.ItemByID(r.Context(), id)
	if !found || it.Hidden {
		http.NotFound(w, r)
		return
	}
	a.releaseChats(r.Context())
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	io.WriteString(w, a.store.ItemPulse(r.Context(), id))
}

// itemMessages is the "Show older" button: the same open row, all messages.
func (a *app) itemMessages(w http.ResponseWriter, r *http.Request) {
	id, ok := itemID(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	ctx := r.Context()
	lr, found := a.store.ListRowByID(ctx, id)
	if !found || lr.Hidden {
		http.NotFound(w, r)
		return
	}
	body, err := a.bodyFor(ctx, lr, true)
	if err != nil {
		appkit.Error("loading older messages", err)
		http.Error(w, "The messages could not be loaded.", http.StatusInternalServerError)
		return
	}
	a.fragment(w, "msgs", http.StatusOK, body)
}

// ownerActs is the one place where a click of the owner's on a row changes an
// item: this page is the signed-in dashboard, so the actor is `owner`.
func (a *app) ownerActs(w http.ResponseWriter, r *http.Request) {
	id, ok := itemID(r)
	action := r.PathValue("action")
	if !ok {
		http.NotFound(w, r)
		return
	}
	switch action {
	case "open", "pending", "resolve", "reopen", "undo":
	default:
		http.NotFound(w, r)
		return
	}
	ctx := r.Context()
	now := a.now().Unix()
	changed := false
	err := a.store.Update(ctx, func(tx *repo.Tx) error {
		it, found := tx.ItemByID(ctx, id)
		if !found || it.Hidden {
			return errNoItem
		}
		next, fits := nextForOwner(action, it.Stage)
		if action == "undo" {
			next, fits = a.undoResolve(ctx, tx.Q, it)
		}
		if !fits {
			return nil // already there (a second tap, a second device): nothing to write
		}
		if err := tx.SetItemStage(ctx, id, next.To, now); err != nil {
			return err
		}
		changed = true
		data := map[string]any{"item": repo.Number(it.Number), "from": it.Stage, "to": next.To}
		if action == "undo" {
			data["undo"] = true // an undo is not a move that can be undone again
		}
		if err := tx.InsertEvent(ctx, now, id, "owner", next.Event, data); err != nil {
			return err
		}
		if it.Kind == repo.ClosedSummaryKind && next.To == stageResolved {
			if err := a.settleSummary(ctx, tx, it, now); err != nil { // SPEC 27: what the row lists is marked seen
				return err
			}
		}
		if action == "open" {
			return nil // looking at a proposal is not acting on the item
		}
		// SPEC 12: what Claude proposed no longer fits once the owner acted himself.
		_, err := tx.WithdrawPending(ctx, id, "you changed this yourself", now)
		return err
	})
	if err == errNoItem {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		appkit.Error("changing an item", err)
		http.Error(w, "That did not work. Try again.", http.StatusInternalServerError)
		return
	}
	if r.Header.Get("X-Requested-With") != "one" {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	if !changed && action != "open" {
		w.Header().Set("X-One-Unchanged", "1")
	}
	a.rowResponse(w, r, id, r.FormValue("open") == "1" || action == "open", false)
}

var errNoItem = errors.New("no such item")

// undoResolve is the phone's Undo after a one-tap resolve or reopen (SPEC 5).
// After a resolve the item goes back to the stage it held immediately before,
// and only when its own record says that its latest move was the owner's resolve
// from there. After a reopen from Resolved it goes back to Resolved, and only
// when its latest move was the owner's reopen of a resolved item. Anything else
// (it moved since, or the owner never made that move) fits nothing.
func (a *app) undoResolve(ctx context.Context, q repo.Q, it repo.Item) (ownerAction, bool) {
	if it.Stage != stageResolved && it.Stage != stageInProgress {
		return ownerAction{}, false
	}
	evs, err := q.EventsOfItem(ctx, it.ID, 50)
	if err != nil {
		return ownerAction{}, false
	}
	for i := len(evs) - 1; i >= 0; i-- {
		e := evs[i]
		if e.Item != it.ID {
			continue
		}
		switch e.Type {
		case "opened", "pending_set", "pending_cleared", "unresolved", "closed", "brought_back", "resolved":
		default:
			continue // a note or a proposal is not a move
		}
		var d struct {
			From, To string
			Undo     bool
		}
		if e.Actor != "owner" || json.Unmarshal([]byte(e.Data), &d) != nil || d.Undo {
			return ownerAction{}, false
		}
		switch {
		case it.Stage == stageResolved && e.Type == "resolved":
			if _, ok := nextForOwner("resolve", d.From); !ok {
				return ownerAction{}, false
			}
			return ownerAction{d.From, "unresolved"}, true
		case it.Stage == stageInProgress && e.Type == "unresolved" && d.From == stageResolved && d.To == stageInProgress:
			return ownerAction{stageResolved, "resolved"}, true
		}
		return ownerAction{}, false
	}
	return ownerAction{}, false
}

// addNote stores a note of the owner's as an event and queues the item for Claude.
func (a *app) addNote(w http.ResponseWriter, r *http.Request) {
	id, ok := itemID(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	extendRead(w, 5*time.Minute)
	r.Body = http.MaxBytesReader(w, r.Body, maxWriteFiles*maxWriteFile+1<<20)
	text, files, err := a.readWrite(r)
	if err != nil {
		var ue *userErr
		if errors.As(err, &ue) {
			http.Error(w, ue.Msg, ue.Status)
		} else {
			http.Error(w, "The note could not be read. Try again.", http.StatusBadRequest)
		}
		return
	}
	if text == "" && len(files) == 0 {
		http.Error(w, "Write something first.", http.StatusBadRequest)
		return
	}
	if utf8.RuneCountInString(text) > maxNote {
		http.Error(w, fmt.Sprintf("A note can have at most %d characters.", maxNote), http.StatusBadRequest)
		return
	}
	ctx := r.Context()
	now := a.now().Unix()
	group := newGroupID()
	err = a.store.Update(ctx, func(tx *repo.Tx) error {
		if it, found := tx.ItemByID(ctx, id); !found || it.Hidden {
			return errNoItem
		}
		// SPEC 7: a file attached to a note belongs to the row, and the entry says so.
		var names []string
		for i, f := range files {
			if _, err := tx.InsertOwnFile(ctx, repo.File{ItemID: id, Source: "owner", Part: fmt.Sprintf("note-%s-%d", group, i), Filename: f.Name,
				ContentType: f.Type, Size: f.Size, SHA: f.SHA, Path: f.Path, AddedAt: now}); err != nil {
				return err
			}
			if err := tx.PoolUsed(ctx, f.Path, f.Created, id); err != nil {
				return err
			}
			names = append(names, f.Name)
		}
		data := map[string]any{"text": text}
		if len(names) > 0 {
			data["files"] = names
		}
		if err := tx.InsertEvent(ctx, now, id, "owner", "noted", data); err != nil {
			return err
		}
		return tx.QueueForClaude(ctx, id)
	})
	if err == errNoItem {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		appkit.Error("saving a note", err)
		http.Error(w, "The note could not be saved. Try again.", http.StatusInternalServerError)
		return
	}
	if r.Header.Get("X-Requested-With") != "one" {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	if len(files) > 0 {
		// The list of attachments is in the other part of the row: answer with the whole row.
		a.rowResponse(w, r, id, true, false)
		return
	}
	lr, _ := a.store.ListRowByID(ctx, id)
	body, err := a.bodyFor(ctx, lr, false)
	if err != nil {
		http.Error(w, "The thread could not be loaded.", http.StatusInternalServerError)
		return
	}
	a.fragment(w, "thread", http.StatusOK, body)
}

// decideProposal is Approve or Dismiss on the signed-in page: the one place
// where a decision is made, and the actor is the owner because of the channel.
func (a *app) decideProposal(w http.ResponseWriter, r *http.Request) {
	pid, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	decision := r.PathValue("decision")
	if err != nil || pid <= 0 || (decision != "approve" && decision != "dismiss") {
		http.NotFound(w, r)
		return
	}
	var res result
	if p, found := a.store.ProposalByID(r.Context(), pid); found && isReplyAction(p.Action) && decision == "approve" {
		// A reply is approved only from its box: the text that goes out is the text
		// the form carries, the text the owner saw at that moment.
		r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
		r.ParseForm()
		shown, has := r.PostForm["text"]
		if !has || len(shown) != 1 {
			http.Error(w, "Open the item and approve the reply in its box, where its text is shown.", http.StatusBadRequest)
			return
		}
		out, aerr := a.approveReply(r.Context(), ownerActor(), pid, shown[0])
		res.Item, err = out.Item, aerr
	} else {
		res, err = a.decide(r.Context(), ownerActor(), pid, decision == "approve")
	}
	var ue *userErr
	var rf *refusal
	switch {
	case errors.As(err, &ue):
		http.Error(w, ue.Msg, ue.Status)
		return
	case errors.As(err, &rf):
		http.Error(w, rf.Error(), http.StatusForbidden)
		return
	case err != nil:
		appkit.Error("deciding a proposal", err)
		http.Error(w, "That did not work. Try again.", http.StatusInternalServerError)
		return
	}
	if r.Header.Get("X-Requested-With") != "one" {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	a.rowResponse(w, r, res.Item.ID, r.FormValue("open") == "1", false)
}

// replyViewOf is how the thread shows a reply proposal: its label, its text
// (the text that was sent once it was approved), and who it goes to.
func replyViewOf(p repo.Proposal) *replyView {
	rt := parseRecipients(p.Recipient)
	v := &replyView{Label: replyBoxLabel(p.Action), Text: p.Text, To: rt.To, Cc: rt.Cc, Editable: p.Status == "pending"}
	if p.Action == replyChat {
		v.Label = "Will be sent · " + chatChannel(rt.Network)
	}
	if p.FinalText != "" {
		v.Text = p.FinalText
	}
	v.Hidden = p.SentMsgID != 0
	return v
}
