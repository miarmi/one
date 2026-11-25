package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"unicode"
	"unicode/utf8"

	"lab/apps/one/repo"
)

// Everything an agent or the owner does to an item, as one function each, behind
// REST, MCP and the page alike (DESIGN 4.1: one code path). Who is acting is
// decided by the channel the request came in on, never by what it says:
// handlers under /api/ build an agent with agentActor, and only the signed-in
// pages build ownerActor.

// --- who is acting ---

type actor struct {
	owner bool
	name  string // an agent's display name
}

func ownerActor() actor { return actor{owner: true} }

// reservedNames are names an agent cannot be shown under without a mark,
// so that no entry of an agent looks like one of the owner's.
var reservedNames = map[string]bool{"owner": true, "you": true, "me": true, "system": true, "rule": true, "one": true}

// agentActor is whoever comes in on /api/. The name is what the caller says
// it is, cleaned; it never makes the caller the owner.
func agentActor(claimed string) actor {
	var b strings.Builder
	for _, r := range strings.TrimSpace(claimed) {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r) || r == ' ' || r == '-' || r == '_' || r == '.':
			b.WriteRune(r)
		}
		if utf8.RuneCountInString(b.String()) >= 40 {
			break
		}
	}
	name := strings.Join(strings.Fields(b.String()), " ")
	if name == "" {
		name = "Claude"
	}
	return actor{name: name}
}

// id is what the event log stores: "owner" only for the owner, "claude" for
// Claude and "agent:<name>" for any other agent.
func (x actor) id() string {
	switch {
	case x.owner:
		return "owner"
	case strings.EqualFold(x.name, "claude"):
		return "claude"
	}
	return "agent:" + x.name
}

func (x actor) isClaude() bool { return !x.owner && x.id() == "claude" }

// actorLabel is how a stored actor is signed in the thread.
func actorLabel(id string) string {
	switch {
	case id == "owner":
		return "you"
	case id == "claude":
		return "Claude"
	case id == "system" || id == "rule":
		return "ONE"
	}
	name := strings.TrimPrefix(id, "agent:")
	if reservedNames[strings.ToLower(name)] {
		name += " (agent)"
	}
	return name
}

// --- guards ---

type guardInfo struct {
	Key, Name  string
	Switchable bool
}

// The guards of SPEC 22. Those that cannot be switched off ignore the setting.
var guards = []guardInfo{
	{"reason_required", "Reason required", true},
	{"protected_people", "Protected people", true},
	{"approval_only", "Approval only", false},
	{"exact_text", "Exact text", false},
	{"same_thread", "Same thread", false},
	{"blocked_targets", "Blocked targets", true},
	{"send_once", "Send once", false},
	{"approval_required", "Approval required", false},
	{"actor_by_channel", "Actor by channel", false},
}

func guardByKey(key string) guardInfo {
	for _, g := range guards {
		if g.Key == key {
			return g
		}
	}
	return guardInfo{Key: key, Name: key}
}

// guardOn says whether a guard checks. Settings may switch off only those
// that can be switched off; there is no settings page in this slice, so the
// state is the stored setting "guard.<key>" being "off".
func (a *app) guardOn(ctx context.Context, q repo.Q, key string) bool {
	g := guardByKey(key)
	return !(g.Switchable && q.Setting(ctx, "guard."+g.Key) == "off")
}

// refusal is an action that a guard blocked.
type refusal struct{ Guard, Why string }

// Error is `Refused by guard: <guard name>` and one sentence saying why (SPEC 22).
func (r *refusal) Error() string {
	if r.Why == "" {
		return "Refused by guard: " + r.Guard
	}
	return "Refused by guard: " + r.Guard + ". " + r.Why
}

// userErr is a request that cannot be done, said in a sentence.
type userErr struct {
	Status int
	Msg    string
}

func (e *userErr) Error() string { return e.Msg }

func bad(msg string) error      { return &userErr{http.StatusBadRequest, msg} }
func conflict(msg string) error { return &userErr{http.StatusConflict, msg} }

var errNoProposal = &userErr{http.StatusNotFound, "no such proposal"}

func errNoSuchItem() error { return &userErr{http.StatusNotFound, "no such item"} }

// logRefusal writes the refusal into the record, in a transaction of its own
// because the one that was refused is rolled back.
func (a *app) logRefusal(ctx context.Context, x actor, itemID int64, err error) error {
	var ref *refusal
	if !errors.As(err, &ref) {
		return err
	}
	a.store.Update(context.WithoutCancel(ctx), func(tx *repo.Tx) error {
		return tx.InsertEvent(ctx, a.now().Unix(), itemID, x.id(), "refused", map[string]any{"guard": ref.Guard, "why": ref.Why})
	})
	return err
}

// --- reading text ---

const (
	maxComment = 8000
	maxReason  = 500
	// A message of this many characters or fewer gets no summary (SPEC 11).
	shortMessage = 300
	maxPending   = 10 // proposals waiting on one item
)

func checkField(field, s string, max int, required bool) (string, error) {
	s = strings.TrimSpace(strings.ReplaceAll(s, "\r\n", "\n"))
	switch {
	case !utf8.ValidString(s):
		return "", bad(field + " is not valid UTF-8")
	case required && s == "":
		return "", bad(field + " is missing")
	case utf8.RuneCountInString(s) > max:
		return "", bad(fmt.Sprintf("%s is longer than %d characters", field, max))
	}
	return s, nil
}

func (a *app) findItem(ctx context.Context, q repo.Q, ref string) (repo.Item, error) {
	n, ok := repo.ParseNumber(ref)
	if !ok {
		return repo.Item{}, bad("the item must be its number, like Q-0012")
	}
	it, found := q.ItemByNumber(ctx, n)
	if !found {
		return repo.Item{}, errNoSuchItem()
	}
	return it, nil
}

// onlyAgent is "Actor by channel" inside the code: these actions exist for
// agents; whoever calls them as anything else is refused.
func onlyAgent(x actor) error {
	if x.owner {
		return refuse("Actor by channel", whyActorAgent)
	}
	return nil
}

// --- what an agent can do without the owner (SPEC 12) ---

type result struct {
	Item     repo.Item
	Revealed bool   // a hidden new item appeared in New
	Same     bool   // nothing was written: it was already so
	Proposal int64  // a proposal that was made, or found waiting
	Status   string // what a decision came to
}

// answer is what a look of Claude's does to an item: it no longer waits, and
// a new item appears in New. Another agent's entry does not count as an answer.
func (a *app) answer(ctx context.Context, tx *repo.Tx, x actor, it repo.Item, now int64) (bool, error) {
	if !x.isClaude() {
		return false, nil
	}
	revealed, err := tx.AnswerReview(ctx, it.ID, now)
	if err != nil {
		return false, err
	}
	if revealed {
		err = tx.InsertEvent(ctx, now, it.ID, "system", "revealed", map[string]any{"item": repo.Number(it.Number)})
	}
	return revealed, err
}

// addComment is a comment or a summary in the thread. A summary takes the
// place of the one before; the old one stays in the record.
func (a *app) addComment(ctx context.Context, x actor, ref, text, kind string) (result, error) {
	var res result
	if err := onlyAgent(x); err != nil {
		return res, a.logRefusal(ctx, x, 0, err)
	}
	if kind == "" {
		kind = "comment"
	}
	if kind != "comment" && kind != "summary" {
		return res, bad(`kind must be "comment" or "summary"`)
	}
	text, err := checkField("text", text, maxComment, true)
	if err != nil {
		return res, err
	}
	now := a.now().Unix()
	err = a.store.Update(ctx, func(tx *repo.Tx) error {
		it, err := a.findItemW(ctx, tx.Q, ref)
		if err != nil {
			return err
		}
		res.Item = it
		if kind == "summary" && a.threadChars(ctx, tx.Q, it.ID) <= shortMessage {
			return conflict(fmt.Sprintf("A message of %d characters or fewer gets no summary. Write a comment, or nothing.", shortMessage))
		}
		if err := tx.InsertEvent(ctx, now, it.ID, x.id(), "commented", map[string]any{"text": text, "kind": kind}); err != nil {
			return err
		}
		res.Revealed, err = a.answer(ctx, tx, x, it, now)
		return err
	})
	return res, err
}

// threadChars counts what the item's messages say: text without markup,
// quoted messages the thread holds and runs of white space.
func (a *app) threadChars(ctx context.Context, q repo.Q, itemID int64) int {
	msgs, _, err := q.MessagesSince(ctx, itemID, 0)
	if err != nil {
		return 0
	}
	return newThreadTexts(msgs).newChars()
}

// markSeen records that Claude looked and has nothing to add.
func (a *app) markSeen(ctx context.Context, x actor, ref string) (result, error) {
	var res result
	if err := onlyAgent(x); err != nil {
		return res, a.logRefusal(ctx, x, 0, err)
	}
	now := a.now().Unix()
	err := a.store.Update(ctx, func(tx *repo.Tx) error {
		it, err := a.findItemW(ctx, tx.Q, ref)
		if err != nil {
			return err
		}
		res.Item = it
		if !it.NeedsClaude {
			res.Same = true
			return nil
		}
		if !x.isClaude() {
			return conflict("Only Claude's look takes an item off Claude's list.")
		}
		if err := tx.InsertEvent(ctx, now, it.ID, x.id(), "seen", nil); err != nil {
			return err
		}
		res.Revealed, err = a.answer(ctx, tx, x, it, now)
		return err
	})
	return res, err
}

// closable are the stages from which an item can be closed.
func closable(stage string) bool {
	return stage == stageNew || stage == stageInProgress || stage == stagePending || stage == stageResolved
}

// checkCloseGuards is Reason required and Protected people for every close
// by an agent.
func (a *app) checkCloseGuards(ctx context.Context, q repo.Q, it repo.Item, reason string) error {
	if reason == "" && a.guardOn(ctx, q, "reason_required") {
		return refuse("Reason required", whyReasonRequired)
	}
	if a.guardOn(ctx, q, "protected_people") && q.HasProtectedDirect(ctx, it.ID) {
		return refuse("Protected people", whyProtectedClose)
	}
	return nil
}

// skipInput is what a clear skip takes besides the item.
type skipInput struct {
	Kind, Reason string
	NoteID       int64  // the note that lets the agent skip it
	SameAsUsual  string // how this message matches the usual one the note describes
}

// skipItem is a clear skip: noise, or a chat that has finished. It takes
// effect at once, needs a reason, goes to Closed, and only an item still in
// New can be skipped.
func (a *app) skipItem(ctx context.Context, x actor, ref string, in skipInput) (result, error) {
	var res result
	kind, reason := in.Kind, in.Reason
	if err := onlyAgent(x); err != nil {
		return res, a.logRefusal(ctx, x, 0, err)
	}
	if kind == "" {
		kind = "noise"
	}
	if kind != "noise" && kind != "finished" && kind != "old" {
		return res, bad(`kind must be "noise", "finished" or "old"`)
	}
	reason, err := checkField("reason", reason, maxReason, false)
	if err != nil {
		return res, err
	}
	if kind == "old" && reason == "" {
		return res, bad("A reason is required for kind old: say what the date was and that it has passed.")
	}
	same, err := checkField("same_as_usual", in.SameAsUsual, maxReason, false)
	if err != nil {
		return res, err
	}
	if in.NoteID != 0 && kind == "noise" && same == "" {
		return res, bad("same_as_usual is required when a note lets you skip noise: say in a sentence how this message matches the usual one the note describes.")
	}
	if in.NoteID == 0 && same != "" {
		return res, bad("same_as_usual belongs with a note_id.")
	}
	now := a.now().Unix()
	var itemID int64
	err = a.store.Update(ctx, func(tx *repo.Tx) error {
		it, err := a.findItemW(ctx, tx.Q, ref)
		if err != nil {
			return err
		}
		itemID, res.Item = it.ID, it
		if it.Kind == repo.ClosedSummaryKind {
			return conflict("This row is the summary of what Claude closed. Only the owner settles it; write a comment on it instead.")
		}
		if err := a.checkCloseGuards(ctx, tx.Q, it, reason); err != nil {
			return err
		}
		if it.Stage != stageNew {
			return conflict("Only an item in New can be skipped. Propose a change instead.")
		}
		data := map[string]any{"kind": kind, "reason": reason, "item": repo.Number(it.Number)}
		if kind == "old" {
			// SPEC 11: the reason of an old item begins `old:`.
			data["reason"] = "old: " + strings.TrimSpace(strings.TrimPrefix(reason, "old:"))
		}
		if in.NoteID != 0 {
			n, ok := tx.NoteByID(ctx, in.NoteID)
			if !ok {
				return &userErr{http.StatusNotFound, "There is no note with that note_id."}
			}
			if !a.noteApplies(ctx, tx.Q, n, it) {
				return conflict("That note does not apply to this item: it is not attached to it, to its sender's person, to its sender, and does not mention them.")
			}
			data["note"], data["note_title"] = n.ID, n.Title
			if same != "" {
				data["same_as_usual"] = same
			}
		}
		if err := tx.SetItemStage(ctx, it.ID, stageClosed, now); err != nil {
			return err
		}
		if _, err := tx.AnswerReview(ctx, it.ID, now); err != nil {
			return err
		}
		if _, err := tx.WithdrawPending(ctx, it.ID, "the item was closed", now); err != nil {
			return err
		}
		return tx.InsertEvent(ctx, now, it.ID, x.id(), "skipped", data)
	})
	return res, a.logRefusal(ctx, x, itemID, err)
}

// unskip pulls an item back to New, only one that an agent skipped.
func (a *app) unskip(ctx context.Context, x actor, ref string) (result, error) {
	var res result
	if err := onlyAgent(x); err != nil {
		return res, a.logRefusal(ctx, x, 0, err)
	}
	now := a.now().Unix()
	err := a.store.Update(ctx, func(tx *repo.Tx) error {
		it, err := a.findItemW(ctx, tx.Q, ref)
		if err != nil {
			return err
		}
		res.Item = it
		typ, by := tx.LastClosing(ctx, it.ID)
		if it.Stage == stageClosed && typ == "skipped" && by == "rule" {
			return conflict("This item was closed by a skip rule. Only the owner can bring it back.")
		}
		if it.Stage != stageClosed || typ != "skipped" {
			return conflict("Only an item that an agent skipped can be pulled back by an agent.")
		}
		if err := tx.SetItemStage(ctx, it.ID, stageNew, now); err != nil {
			return err
		}
		if err := tx.InsertEvent(ctx, now, it.ID, x.id(), "skip_undone", nil); err != nil {
			return err
		}
		return tx.InsertEvent(ctx, now, it.ID, "system", "revealed", nil)
	})
	return res, err
}

// bringBackPending puts a Pending item in front of the owner again, in New, when
// Claude found something new about it (SPEC 3). It needs no Approve: it only
// shows the item to the owner; it never resolves, approves or sends. The thread
// says `Claude: <reason>`.
func (a *app) bringBackPending(ctx context.Context, x actor, ref, reason string) (result, error) {
	var res result
	if err := onlyAgent(x); err != nil {
		return res, a.logRefusal(ctx, x, 0, err)
	}
	reason, err := checkField("reason", reason, maxReason, true)
	if err != nil {
		return res, err
	}
	now := a.now().Unix()
	err = a.store.Update(ctx, func(tx *repo.Tx) error {
		it, err := a.findItemW(ctx, tx.Q, ref)
		if err != nil {
			return err
		}
		res.Item = it
		if it.Stage != stagePending {
			return conflict("Only an item that is Pending can be brought back. This one is in " + stageLabels[it.Stage] + ".")
		}
		return a.pendingToNew(ctx, tx, it, x.id(), reason, now)
	})
	return res, a.logRefusal(ctx, x, res.Item.ID, err)
}

// pendingToNew is the one way a Pending item returns to New on Claude's word,
// for the connector, the model's review and the night check.
func (a *app) pendingToNew(ctx context.Context, tx *repo.Tx, it repo.Item, actorID, reason string, now int64) error {
	if err := tx.SetItemStage(ctx, it.ID, stageNew, now); err != nil {
		return err
	}
	return tx.InsertEvent(ctx, now, it.ID, actorID, "brought_back", map[string]any{"item": repo.Number(it.Number), "from": stagePending, "to": stageNew, "reason": reason})
}

// closeOnNote closes an item when the owner's own newest note on it, which no
// agent has answered yet, asks for it. The server can see that such a note
// exists; that it asks for this is what the agent vouches for, so the thread
// says whose doing it was.
func (a *app) closeOnNote(ctx context.Context, x actor, ref, reason string) (result, error) {
	var res result
	if err := onlyAgent(x); err != nil {
		return res, a.logRefusal(ctx, x, 0, err)
	}
	reason, err := checkField("reason", reason, maxReason, false)
	if err != nil {
		return res, err
	}
	now := a.now().Unix()
	var itemID int64
	err = a.store.Update(ctx, func(tx *repo.Tx) error {
		it, err := a.findItemW(ctx, tx.Q, ref)
		if err != nil {
			return err
		}
		itemID, res.Item = it.ID, it
		if err := a.checkCloseGuards(ctx, tx.Q, it, reason); err != nil {
			return err
		}
		if !closable(it.Stage) || it.Hidden {
			return conflict("This item cannot be closed on a note.")
		}
		if !tx.OpenNote(ctx, it.ID) {
			return conflict("The owner has no note on this item that is waiting for an answer.")
		}
		if err := tx.SetItemStage(ctx, it.ID, stageClosed, now); err != nil {
			return err
		}
		if _, err := a.answer(ctx, tx, x, it, now); err != nil {
			return err
		}
		if _, err := tx.WithdrawPending(ctx, it.ID, "the item was closed", now); err != nil {
			return err
		}
		return tx.InsertEvent(ctx, now, it.ID, x.id(), "closed", map[string]any{"on_note": true, "reason": reason, "item": repo.Number(it.Number)})
	})
	return res, a.logRefusal(ctx, x, itemID, err)
}

// --- proposals ---

// proposalActions are the proposals of this slice: those that change only
// things inside the app.
var proposalActions = map[string]string{"pending": "Mark pending", "resolve": "Resolve", "close": "Close"}

// stageFor is where an approved action takes the item, and whether it fits
// the item's stage now.
func stageFor(action, stage string) (string, bool) {
	switch action {
	case "pending", "resolve":
		if next, ok := nextForOwner(action, stage); ok {
			return next.To, true
		}
	case "close":
		if closable(stage) {
			return stageClosed, true
		}
	}
	return "", false
}

// propose stores what an agent wants done as a proposal. Nothing happens to
// the item until the owner presses Approve (guard Approval required).
func (a *app) propose(ctx context.Context, x actor, ref, action, reason string) (result, error) {
	var res result
	if err := onlyAgent(x); err != nil {
		return res, a.logRefusal(ctx, x, 0, err)
	}
	if _, ok := proposalActions[action]; !ok {
		return res, bad(`action must be "pending", "resolve" or "close"`)
	}
	reason, err := checkField("reason", reason, maxReason, false)
	if err != nil {
		return res, err
	}
	now := a.now().Unix()
	var itemID int64
	err = a.store.Update(ctx, func(tx *repo.Tx) error {
		it, err := a.findItemW(ctx, tx.Q, ref)
		if err != nil {
			return err
		}
		itemID, res.Item = it.ID, it
		if action == "close" {
			if err := a.checkCloseGuards(ctx, tx.Q, it, reason); err != nil {
				return err
			}
		} else if reason == "" {
			return bad("reason is missing")
		}
		if _, fits := stageFor(action, it.Stage); !fits {
			return conflict(fmt.Sprintf("That does not fit an item in %s.", stageLabels[it.Stage]))
		}
		if p, found := tx.PendingProposal(ctx, it.ID, action); found {
			res.Proposal, res.Same = p.ID, true
			return nil
		}
		if tx.PendingCount(ctx, it.ID) >= maxPending {
			return conflict("This item already has proposals waiting for the owner.")
		}
		id, err := tx.InsertProposal(ctx, repo.Proposal{ItemID: it.ID, Action: action, Reason: reason, By: x.id(), At: now,
			BaseStage: it.Stage, BaseMsgAt: tx.NewestMessageAt(ctx, it.ID)})
		if err != nil {
			return err
		}
		res.Proposal = id
		if err := tx.InsertEvent(ctx, now, it.ID, x.id(), "proposal_made", map[string]any{"proposal": id, "action": action, "reason": reason}); err != nil {
			return err
		}
		res.Revealed, err = a.answer(ctx, tx, x, it, now)
		return err
	})
	return res, a.logRefusal(ctx, x, itemID, err)
}

// decide is the owner pressing Approve or Dismiss. Only a signed-in page calls it
// with the owner; any other actor is refused whatever the request says. Approve
// carries the action out once: a proposal that is no longer waiting changes
// nothing, and one that no longer fits is withdrawn.
func (a *app) decide(ctx context.Context, x actor, proposalID int64, approve bool) (result, error) {
	var res result
	var itemID int64
	if !x.owner {
		if p, ok := a.store.ProposalByID(ctx, proposalID); ok {
			itemID = p.ItemID
		}
		return res, a.logRefusal(ctx, x, itemID, refuse("Actor by channel", whyActorOwner))
	}
	now := a.now().Unix()
	err := a.store.Update(ctx, func(tx *repo.Tx) error {
		p, found := tx.ProposalByID(ctx, proposalID)
		if !found {
			return errNoProposal
		}
		it, ok := tx.ItemByID(ctx, p.ItemID)
		if !ok {
			return errNoSuchItem()
		}
		res.Item, res.Proposal = it, p.ID
		if p.Status != "pending" {
			res.Same, res.Status = true, p.Status
			return nil
		}
		if !approve {
			if _, err := tx.DecideProposal(ctx, p.ID, "dismissed", "owner", "", now); err != nil {
				return err
			}
			res.Status = "dismissed"
			return tx.InsertEvent(ctx, now, it.ID, "owner", "proposal_decided", map[string]any{"proposal": p.ID, "decision": "dismissed", "action": p.Action})
		}
		if isReplyAction(p.Action) {
			// A reply is sent from the box that shows its text, never from here.
			return conflict("Open the item and approve the reply in its box, where its text is shown.")
		}
		if p.Action == "todo" {
			return a.approveTodo(ctx, tx, &res, p, it, now)
		}
		switch p.Action {
		case actionMerge:
			return a.approveMerge(ctx, tx, &res, p, it, now)
		case actionSplit:
			return a.approveSplit(ctx, tx, &res, p, it, now)
		}
		to, fits := stageFor(p.Action, it.Stage)
		if !fits || tx.NewestMessageAt(ctx, it.ID) > p.BaseMsgAt {
			if _, err := tx.DecideProposal(ctx, p.ID, "withdrawn", "system", "it no longer fits", now); err != nil {
				return err
			}
			res.Status = "withdrawn"
			return tx.InsertEvent(ctx, now, it.ID, "system", "proposal_withdrawn", map[string]any{"proposal": p.ID, "why": "it no longer fits"})
		}
		if _, err := tx.DecideProposal(ctx, p.ID, "approved", "owner", "", now); err != nil {
			return err
		}
		if err := tx.SetItemStage(ctx, it.ID, to, now); err != nil {
			return err
		}
		res.Status = "approved"
		if err := tx.InsertEvent(ctx, now, it.ID, "owner", "proposal_decided", map[string]any{"proposal": p.ID, "decision": "approved", "action": p.Action}); err != nil {
			return err
		}
		typ := map[string]string{"pending": "pending_set", "resolve": "resolved", "close": "closed"}[p.Action]
		if err := tx.InsertEvent(ctx, now, it.ID, "owner", typ, map[string]any{"item": repo.Number(it.Number), "from": it.Stage, "to": to, "proposal": p.ID}); err != nil {
			return err
		}
		// What else waited on the old stage no longer fits.
		_, err := tx.WithdrawPending(ctx, it.ID, "the item changed", now)
		return err
	})
	return res, err
}
