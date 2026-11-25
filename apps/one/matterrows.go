package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"lab/apps/one/repo"
)

// Joining and splitting rows (SPEC 10, 12). Claude can propose to join two
// rows of mail (propose_merge) and to split one that was wrongly joined
// (propose_split). Both are proposals on the row: they do nothing until the owner
// presses Approve, and nothing under /api/ approves. When the owner approves:
//
//   - a joined row's messages, links, files, notes, to-dos and proposals move
//     to the row that stays; the joined row is kept, hidden, and its number
//     leads to the row that stays; its entries show with the row that stays,
//     where a notice says "<agent> joined <number> into this row.";
//   - a split takes one mail thread out of the row into a new row; both rows
//     carry a notice. Nothing is deleted.

const (
	actionMerge = "merge"
	actionSplit = "split"
)

var rowTools = []mcpTool{
	{"propose_merge", "Propose to join two rows",
		"Propose that two rows (mail or chat, in any combination) are one matter: the row id stays, the row join is joined into it. Everything join holds (its messages, files, notes and to-dos) moves to the row that stays and nothing is lost; the number of the joined row then leads to the row that stays. Use it when two rows are plainly the same matter (the same reference number, the same order, the same case), never because the sender or the subject looks alike. A meeting thread (a row tied to a calendar entry, see propose_thread) can be the row that stays, so that related mail or a chat is joined into it; it is never the row that is joined into another. A chat and a mail are joined only by this proposal and only on the owner's Approve, never on their own, not even for the same person; joining rows does not join people. Nothing changes until the owner presses Approve on the row that stays, where the proposal shows with Approve and Dismiss. Give a reason the owner can read. Proposing the same thing twice returns the waiting one.",
		schema(map[string]any{"id": itemT, "join": map[string]any{"type": []string{"string", "integer"}, "description": "The row that is joined into the first one: its number like \"Q-0013\" or 13."}, "reason": strT, "agent": agentT}, "id", "join", "reason"),
		hints("Propose to join two rows", false, false, true)},
	{"propose_split", "Propose to split a row",
		"Propose that one mail thread or chat is taken out of a row that joined several, into a row of its own: give the row (id) and the message_id of any message of the thread to take out (from get_item). Use it when a row holds messages about two different matters, or a chat and a mail that were joined wrongly. The rest of the row stays; nothing is lost. Nothing changes until the owner presses Approve on the row, where the proposal shows with Approve and Dismiss. Give a reason the owner can read. Proposing the same thing twice returns the waiting one.",
		schema(map[string]any{"id": itemT, "message": map[string]any{"type": "integer", "description": "The message_id of a message of the thread to take out."}, "reason": strT, "agent": agentT}, "id", "message", "reason"),
		hints("Propose to split a row", false, false, true)},
}

func init() { mcpTools = append(mcpTools, rowTools...) }

type rowProposalBody struct {
	Agent   string          `json:"agent"`
	Join    json.RawMessage `json:"join"`
	Message int64           `json:"message"`
	Reason  string          `json:"reason"`
}

// stageRank orders the stages from the most open to the least.
func stageRank(stage string) int {
	switch stage {
	case stageNew:
		return 0
	case stageInProgress:
		return 1
	case stagePending:
		return 2
	case stageResolved:
		return 3
	case stageClosed:
		return 4
	}
	return 5
}

// crossChannelJoin is the one switch of the trial "one matter can cross mail
// and chat" (SPEC 10, 2026-11-21). On, propose_merge takes chat rows as well as
// mail rows. Off, a join of a chat row is refused exactly as before the trial.
// Rows that were joined already stay as they are, and splitting them stays
// possible either way: undoing a join is never switched off.
var crossChannelJoin = true

// mailRow says whether a row is made of mail only (a chat is a row of its own).
func mailRow(it repo.Item) bool { return it.Kind == "thread" }

// joinableRow says whether a row can be joined: a mail row, or (while the
// trial is on) a chat row.
func joinableRow(it repo.Item) bool {
	return mailRow(it) || (crossChannelJoin && it.Kind == "chat")
}

func (a *app) apiProposeMerge(w http.ResponseWriter, r *http.Request) {
	var b rowProposalBody
	if !readJSON(w, r, &b) {
		return
	}
	join, ok := itemRef(b.Join)
	if !ok {
		apiError(w, http.StatusBadRequest, "join must be an item number like Q-0013")
		return
	}
	res, err := a.proposeMerge(r.Context(), agentActor(b.Agent), r.PathValue("id"), join, b.Reason)
	if err != nil {
		answerErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, resultJSON(res))
}

func (a *app) apiProposeSplit(w http.ResponseWriter, r *http.Request) {
	var b rowProposalBody
	if !readJSON(w, r, &b) {
		return
	}
	res, err := a.proposeSplit(r.Context(), agentActor(b.Agent), r.PathValue("id"), b.Message, b.Reason)
	if err != nil {
		answerErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, resultJSON(res))
}

// callRowTool runs propose_merge and propose_split for the connector.
func (a *app) callRowTool(ctx context.Context, name string, args json.RawMessage) (string, bool, bool) {
	var in struct {
		ID      json.RawMessage `json:"id"`
		Join    json.RawMessage `json:"join"`
		Message int64           `json:"message"`
		Reason  string          `json:"reason"`
		Agent   string          `json:"agent"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return "The arguments must be a JSON object.", true, true
	}
	ref, ok := itemRef(in.ID)
	if !ok {
		return "id must be an item number like Q-0012.", true, true
	}
	var text string
	var isErr bool
	if name == "propose_merge" {
		body, _ := json.Marshal(map[string]any{"agent": in.Agent, "join": in.Join, "reason": in.Reason})
		text, isErr = a.callAPI(ctx, "POST", "/api/items/"+ref+"/merge-proposals", body)
	} else {
		body, _ := json.Marshal(map[string]any{"agent": in.Agent, "message": in.Message, "reason": in.Reason})
		text, isErr = a.callAPI(ctx, "POST", "/api/items/"+ref+"/split-proposals", body)
	}
	return text, isErr, true
}

// --- proposing ---

func (a *app) proposeMerge(ctx context.Context, x actor, keepRef, dropRef, reason string) (result, error) {
	var res result
	if err := onlyAgent(x); err != nil {
		return res, a.logRefusal(ctx, x, 0, err)
	}
	reason, err := checkField("reason", reason, maxReason, true)
	if err != nil {
		return res, err
	}
	now := a.now().Unix()
	var itemID int64
	err = a.store.Update(ctx, func(tx *repo.Tx) error {
		keep, err := a.findItemW(ctx, tx.Q, keepRef)
		if err != nil {
			return err
		}
		itemID, res.Item = keep.ID, keep
		drop, err := a.findItemW(ctx, tx.Q, dropRef)
		if err != nil {
			return err
		}
		if msg := mergeMisfit(ctx, tx.Q, keep, drop); msg != "" {
			return conflict(msg)
		}
		for _, p := range pendingOf(ctx, tx.Q, keep.ID, actionMerge) {
			if p.TargetID == drop.ID {
				res.Proposal, res.Same = p.ID, true
				return nil
			}
		}
		if tx.PendingCount(ctx, keep.ID) >= maxPending {
			return conflict("This item already has proposals waiting for the owner.")
		}
		id, err := tx.InsertProposal(ctx, repo.Proposal{ItemID: keep.ID, Action: actionMerge, Text: repo.Number(drop.Number), Reason: reason, By: x.id(), At: now,
			BaseStage: keep.Stage, BaseMsgAt: tx.NewestMessageAt(ctx, keep.ID), TargetID: drop.ID})
		if err != nil {
			return err
		}
		res.Proposal = id
		if err := tx.InsertEvent(ctx, now, keep.ID, x.id(), "proposal_made", map[string]any{"proposal": id, "action": actionMerge, "reason": reason, "join": repo.Number(drop.Number)}); err != nil {
			return err
		}
		// Claude has looked at both rows: neither waits for a first look any more.
		if _, err := a.answer(ctx, tx, x, drop, now); err != nil {
			return err
		}
		res.Revealed, err = a.answer(ctx, tx, x, keep, now)
		return err
	})
	return res, a.logRefusal(ctx, x, itemID, err)
}

// mergeMisfit says in a sentence why two rows cannot be joined ("" when they can).
func mergeMisfit(ctx context.Context, q repo.Q, keep, drop repo.Item) string {
	switch {
	case keep.ID == drop.ID:
		return "These are one row already: that number leads to this row."
	case drop.Kind == meetingKind:
		return "A meeting thread is never joined into another row. Join the mail or chat into the meeting thread instead."
	case !(joinableRow(keep) || keep.Kind == meetingKind) || !joinableRow(drop):
		return "Only rows of mail are joined. A chat is one row already."
	case q.MergedInto(ctx, keep.ID) != 0 || q.MergedInto(ctx, drop.ID) != 0:
		return "One of the rows was joined into another already."
	case keep.TestRun != drop.TestRun:
		return runRefused().Error()
	}
	return ""
}

func pendingOf(ctx context.Context, q repo.Q, itemID int64, action string) []repo.Proposal {
	props, err := q.ProposalsOfItem(ctx, itemID)
	if err != nil {
		return nil
	}
	var out []repo.Proposal
	for _, p := range props {
		if p.Status == "pending" && p.Action == action {
			out = append(out, p)
		}
	}
	return out
}

// threadLink is the link (kind, value) a mail or chat thread has to its row.
func threadLink(threadKey string) (kind, value string, ok bool) {
	if strings.HasPrefix(threadKey, "chat:") && len(threadKey) > len("chat:") {
		return chatLinkKind, strings.TrimPrefix(threadKey, "chat:"), true
	}
	for _, s := range []mailSource{gmailSrc, graphSrc} {
		if strings.HasPrefix(threadKey, s.ThreadPrefix) && len(threadKey) > len(s.ThreadPrefix) {
			return s.LinkKind, strings.TrimPrefix(threadKey, s.ThreadPrefix), true
		}
	}
	return "", "", false
}

func (a *app) proposeSplit(ctx context.Context, x actor, ref string, messageID int64, reason string) (result, error) {
	var res result
	if err := onlyAgent(x); err != nil {
		return res, a.logRefusal(ctx, x, 0, err)
	}
	reason, err := checkField("reason", reason, maxReason, true)
	if err != nil {
		return res, err
	}
	if messageID <= 0 {
		return res, bad("message must be the message_id of a message of the thread to take out")
	}
	now := a.now().Unix()
	var itemID int64
	err = a.store.Update(ctx, func(tx *repo.Tx) error {
		it, err := a.findItemW(ctx, tx.Q, ref)
		if err != nil {
			return err
		}
		itemID, res.Item = it.ID, it
		key, subject, msg := splitMisfit(ctx, tx.Q, it, messageID)
		if msg != "" {
			return conflict(msg)
		}
		for _, p := range pendingOf(ctx, tx.Q, it.ID, actionSplit) {
			if k, _, _, ok := tx.ThreadOfMessage(ctx, it.ID, p.TargetID); ok && k == key {
				res.Proposal, res.Same = p.ID, true
				return nil
			}
		}
		if tx.PendingCount(ctx, it.ID) >= maxPending {
			return conflict("This item already has proposals waiting for the owner.")
		}
		id, err := tx.InsertProposal(ctx, repo.Proposal{ItemID: it.ID, Action: actionSplit, Text: oneLine(subject, 200), Reason: reason, By: x.id(), At: now,
			BaseStage: it.Stage, BaseMsgAt: tx.NewestMessageAt(ctx, it.ID), TargetID: messageID})
		if err != nil {
			return err
		}
		res.Proposal = id
		if err := tx.InsertEvent(ctx, now, it.ID, x.id(), "proposal_made", map[string]any{"proposal": id, "action": actionSplit, "reason": reason, "message": messageID}); err != nil {
			return err
		}
		res.Revealed, err = a.answer(ctx, tx, x, it, now)
		return err
	})
	return res, a.logRefusal(ctx, x, itemID, err)
}

// splitMisfit checks that a thread of a row can be taken out. It answers the
// thread key and subject, or a sentence that says why not.
func splitMisfit(ctx context.Context, q repo.Q, it repo.Item, messageID int64) (key, subject, why string) {
	key, _, subject, ok := q.ThreadOfMessage(ctx, it.ID, messageID)
	switch {
	// A row of one chat is split no more than it was; a row that joined a
	// chat can be split whether or not the trial is on.
	case it.Kind == "chat" && q.ThreadsOfItem(ctx, it.ID) < 2:
		return "", "", "Only rows of mail are split. A chat is one row."
	case q.MergedInto(ctx, it.ID) != 0:
		return "", "", "This row was joined into another already."
	case !ok:
		return "", "", "This row has no message with that message_id."
	case q.ThreadsOfItem(ctx, it.ID) < 2 && it.Kind != meetingKind: // a meeting thread can give up its one joined thread
		return "", "", "This row holds one mail thread only: there is nothing to split."
	}
	if _, _, found := threadLink(key); !found {
		return "", "", "That thread cannot be told apart from the rest of the row."
	}
	return key, subject, ""
}

// --- approving (called by decide, inside its transaction) ---

// withdrawMisfit withdraws a proposal that no longer fits.
func withdrawMisfit(ctx context.Context, tx *repo.Tx, res *result, p repo.Proposal, it repo.Item, now int64) error {
	if _, err := tx.DecideProposal(ctx, p.ID, "withdrawn", "system", "it no longer fits", now); err != nil {
		return err
	}
	res.Status = "withdrawn"
	return tx.InsertEvent(ctx, now, it.ID, "system", "proposal_withdrawn", map[string]any{"proposal": p.ID, "why": "it no longer fits"})
}

func (a *app) approveMerge(ctx context.Context, tx *repo.Tx, res *result, p repo.Proposal, keep repo.Item, now int64) error {
	drop, ok := tx.ItemByID(ctx, p.TargetID)
	if !ok || mergeMisfit(ctx, tx.Q, keep, drop) != "" {
		return withdrawMisfit(ctx, tx, res, p, keep, now)
	}
	if _, err := tx.DecideProposal(ctx, p.ID, "approved", "owner", "", now); err != nil {
		return err
	}
	res.Status = "approved"
	if err := tx.InsertEvent(ctx, now, keep.ID, "owner", "proposal_decided", map[string]any{"proposal": p.ID, "decision": "approved", "action": actionMerge}); err != nil {
		return err
	}
	// What else waited on either row no longer fits.
	for _, id := range []int64{drop.ID, keep.ID} {
		if _, err := tx.WithdrawPending(ctx, id, "the rows were joined", now); err != nil {
			return err
		}
	}
	joined, err := tx.JoinItems(ctx, keep, drop)
	if err != nil {
		return err
	}
	// The row that stays is as open as the more open of the two, and shown.
	if stageRank(drop.Stage) < stageRank(joined.Stage) {
		joined.Stage = drop.Stage
	}
	joined.PendingSince = 0
	if joined.Stage == stagePending {
		joined.PendingSince = min(nonzero(keep.PendingSince, drop.PendingSince), nonzero(drop.PendingSince, keep.PendingSince))
	}
	joined.NeedsClaude = keep.NeedsClaude
	// A chat that was still going on keeps its quiet time in the joined row
	// (JoinItems carried the later of the two times over).
	joined.Hidden = false
	if err := tx.SaveItem(ctx, joined); err != nil {
		return err
	}
	res.Item = joined
	return tx.InsertEvent(ctx, now, keep.ID, p.By, "rows_joined", map[string]any{"number": repo.Number(drop.Number), "proposal": p.ID})
}

func nonzero(a, b int64) int64 {
	if a != 0 {
		return a
	}
	return b
}

func (a *app) approveSplit(ctx context.Context, tx *repo.Tx, res *result, p repo.Proposal, it repo.Item, now int64) error {
	key, _, why := splitMisfit(ctx, tx.Q, it, p.TargetID)
	if why != "" {
		return withdrawMisfit(ctx, tx, res, p, it, now)
	}
	kind, value, _ := threadLink(key)
	if _, err := tx.DecideProposal(ctx, p.ID, "approved", "owner", "", now); err != nil {
		return err
	}
	res.Status = "approved"
	if err := tx.InsertEvent(ctx, now, it.ID, "owner", "proposal_decided", map[string]any{"proposal": p.ID, "decision": "approved", "action": actionSplit}); err != nil {
		return err
	}
	if _, err := tx.WithdrawPending(ctx, it.ID, "the row changed", now); err != nil {
		return err
	}
	// The new row belongs to the same test run as the row it comes from.
	rctx := repo.WithRun(ctx, it.TestRun)
	freshKind := "thread"
	if kind == chatLinkKind {
		freshKind = "chat"
	}
	fresh, err := tx.CreateItemOnly(rctx, repo.Item{Kind: freshKind, Subject: it.Subject, Stage: it.Stage, FirstAt: it.FirstAt, LastAt: it.LastAt, MovedAt: it.MovedAt, PendingSince: it.PendingSince})
	if err != nil {
		return err
	}
	if err := tx.SplitThread(rctx, it.ID, fresh.ID, key, kind, value); err != nil {
		return err
	}
	for _, row := range []*repo.Item{&fresh, &it} {
		first, last, subject := tx.MessageSpan(ctx, row.ID)
		if last > 0 {
			row.FirstAt, row.LastAt = first, last
			if row.Kind != meetingKind {
				row.Subject = subject
			}
		}
		if row.Kind == meetingKind {
			if tx.ChatsOfItem(ctx, row.ID) == 0 {
				row.ChatSince = 0
			}
			if err := tx.SaveItem(rctx, *row); err != nil { // a meeting thread stays one, whatever it holds
				return err
			}
			continue
		}
		// A row is a chat row while it holds a chat, and a chat that is still
		// going on keeps waiting for its quiet time in the row that holds it.
		if tx.ChatsOfItem(ctx, row.ID) > 0 {
			row.Kind = "chat"
		} else {
			row.Kind, row.ChatSince = "thread", 0
		}
		if row == &fresh && row.Kind == "chat" {
			row.ChatSince = it.ChatSince // read before the row it comes from is written
		}
		if err := tx.SetKind(rctx, row.ID, row.Kind); err != nil {
			return err
		}
		if err := tx.SaveItem(rctx, *row); err != nil {
			return err
		}
	}
	res.Item = it
	if err := tx.InsertEvent(ctx, now, it.ID, p.By, "row_split", map[string]any{"number": repo.Number(fresh.Number), "proposal": p.ID}); err != nil {
		return err
	}
	return tx.InsertEvent(ctx, now, fresh.ID, p.By, "row_split_off", map[string]any{"number": repo.Number(it.Number), "proposal": p.ID})
}

// rowNoticeText is the notice a joined or split row shows in its thread.
func rowNoticeText(eventType, actor, number string) string {
	who := actorLabel(actor)
	switch eventType {
	case "rows_joined":
		return fmt.Sprintf("%s joined %s into this row.", who, number)
	case "row_split":
		return fmt.Sprintf("%s split %s off this row.", who, number)
	}
	return fmt.Sprintf("%s split this row off from %s.", who, number)
}
