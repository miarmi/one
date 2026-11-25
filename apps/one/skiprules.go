package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"lab/apps/one/repo"
	"lab/platform/appkit"
)

// Skip rules (SPEC 31): a second kind of rule beside the import rules. An
// import rule drops mail before it is stored; a skip rule closes an item
// that is stored and waits for Claude, without the model. The item goes to
// Closed with the rule as the reason and can be brought back like any
// closed item. A skip rule looks only at plain facts, never at what a text
// means:
//
//   - same_mail: the same mail in both mailboxes, found by its internet
//     message id. The second copy is closed and points to the first.
//   - sender: a sender address or mailing subdomain whose items were skipped
//     again and again and never brought back.
//   - calendar: a notice that an entry was added to a shared calendar,
//     found by its sender and the start of its subject.
//
// A rule has the life of a proposed import rule: it waits, marked Proposal,
// and applies only after the owner presses Approve on the signed-in page. No
// address under /api/ approves one. A rule whose item the owner or the night
// check brings back is paused at once and applies again only when the owner
// switches it on.

const (
	skipSameMail = "same_mail"
	skipSender   = "sender"
	skipCalendar = "calendar"

	keySkipMin     = "claude.skip_min"
	defSkipMin     = 5
	maxSkipPropose = 50
	maxSkipRules   = 500 // rules in all (active and waiting): each new item is compared with every one
	nameSameMail   = "Same mail in both mailboxes"
	nameCalendar   = "Entry added to a shared calendar"
)

// calendarNotice is the marker of a notice that an entry was added to a shared
// calendar: mail from the calendar service's own notification address whose
// subject begins with the words below. It is plain facts in the stored mail
// (the sender and the start of the subject); it is the marker of Google
// Calendar's notification mail and has not been checked against real mail.
var calendarNotice = struct{ Sender, SubjectPrefix string }{"calendar-notification@google.com", "New event:"}

func (a *app) skipMin(ctx context.Context) int {
	if n, err := strconv.Atoi(a.store.Setting(ctx, keySkipMin)); err == nil && n > 0 {
		return n
	}
	return defSkipMin
}

// --- what the app sees when mail is stored ---

// afterMailStored is called once a mail message has been stored on an item:
// it notes the cases a skip rule is about (proposing the rule once, the first
// time) and closes the item when an approved rule says so.
func (a *app) afterMailStored(ctx context.Context, itemID int64) {
	ctx = context.WithoutCancel(ctx)
	if err := a.noticeCases(ctx, itemID); err != nil {
		appkit.Error("noting a case for a skip rule", err)
	}
	if err := a.applySkipRules(ctx, itemID); err != nil {
		appkit.Error("applying skip rules", err)
	}
	a.wakeReview()
}

// noticeCases records the same mail in both mailboxes and proposes the rules
// of the app's own kinds, once.
func (a *app) noticeCases(ctx context.Context, itemID int64) error {
	it, ok := a.store.ItemByID(ctx, itemID)
	if !ok || it.TestRun != "" {
		return nil
	}
	msgs, _, err := a.store.MessagesSince(ctx, itemID, 0)
	if err != nil {
		return err
	}
	now := a.now().Unix()
	for _, m := range msgs {
		if m.SentByMe || m.Network != "mail" {
			continue
		}
		if twin, found := a.store.FindTwin(ctx, repo.TwinOf{HeaderID: m.HeaderID, Source: m.Source, Sender: m.Sender, Subject: m.Subject, ReceivedAt: m.ReceivedAt}, itemID); found {
			first, second := twin, itemID
			if twin > itemID {
				first, second = itemID, twin
			}
			if err := a.recordSameMail(ctx, second, first, now); err != nil {
				return err
			}
		}
		if strings.EqualFold(m.Sender, calendarNotice.Sender) && hasPrefixFold(m.Subject, calendarNotice.SubjectPrefix) {
			if err := a.proposeOwnKind(ctx, skipCalendar, nameCalendar, now); err != nil {
				return err
			}
		}
	}
	return nil
}

func hasPrefixFold(s, prefix string) bool {
	s = strings.TrimSpace(s)
	return len(s) >= len(prefix) && strings.EqualFold(s[:len(prefix)], prefix)
}

func (a *app) recordSameMail(ctx context.Context, second, first, now int64) error {
	fresh := false
	err := a.store.Update(ctx, func(tx *repo.Tx) error {
		if _, _, had := tx.SameMailOf(ctx, second); had {
			return nil
		}
		if err := tx.RecordSameMail(ctx, second, first, now); err != nil {
			return err
		}
		fresh = true
		s, _ := tx.ItemByID(ctx, second)
		f, _ := tx.ItemByID(ctx, first)
		if err := tx.InsertEvent(ctx, now, second, "system", "same_mail", map[string]any{"as": repo.Number(f.Number)}); err != nil {
			return err
		}
		return tx.InsertEvent(ctx, now, first, "system", "same_mail", map[string]any{"as": repo.Number(s.Number)})
	})
	if err != nil || !fresh {
		return err
	}
	return a.proposeOwnKind(ctx, skipSameMail, nameSameMail, now)
}

// proposeOwnKind makes the app's own proposal of a kind, the first time only:
// a rule of that kind in any state (a dismissed one too) means it was proposed.
func (a *app) proposeOwnKind(ctx context.Context, kind, name string, now int64) error {
	return a.store.Update(ctx, func(tx *repo.Tx) error {
		if _, known := tx.SkipRuleKnown(ctx, kind, ""); known {
			return nil
		}
		if tx.CountSkipRuleProposals(ctx) >= maxSkipPropose || tx.CountSkipRules(ctx) >= maxSkipRules {
			return nil
		}
		reason := a.seenText(ctx, tx.Q, kind, 1)
		id, err := tx.InsertSkipRule(ctx, repo.SkipRule{Name: name, Kind: kind, Reason: reason, ProposedBy: "system", ProposedAt: now})
		if err != nil {
			return err
		}
		return tx.InsertEvent(ctx, now, 0, "system", "skip_rule_proposed", map[string]any{"skip_rule": id, "name": name, "reason": reason})
	})
}

// seenText is "seen 8 times in the last month" for the two kinds of the app.
func (a *app) seenText(ctx context.Context, q repo.Q, kind string, atLeast int) string {
	since := a.now().Add(-30 * 24 * time.Hour).Unix()
	n := 0
	switch kind {
	case skipSameMail:
		n = q.CountSameMailSince(ctx, since)
	case skipCalendar:
		n = q.CountMailLike(ctx, calendarNotice.Sender, calendarNotice.SubjectPrefix, since)
	}
	n = max(n, atLeast)
	return fmt.Sprintf("Seen %s in the last month.", plural(int64(n), "time"))
}

// --- closing an item by a rule ---

// applySkipRules closes the item when an enabled skip rule says so. It acts
// only on an item that waits for Claude, is in New and has nothing of the owner's
// own in it; the owner's own thread is never closed. A real rule never touches an
// item of a test run, and a rule made under a test run touches only the items
// of that run.
func (a *app) applySkipRules(ctx context.Context, itemID int64) error {
	rules, err := a.store.EnabledSkipRules(ctx)
	if err != nil || len(rules) == 0 {
		return err
	}
	it, ok := a.store.ItemByID(ctx, itemID)
	if !ok || !it.NeedsClaude || it.Stage != stageNew || a.store.SkipRuleProtected(ctx, itemID) {
		return nil // a rule applies only to an item that is new and untouched
	}
	msgs, _, err := a.store.MessagesSince(ctx, itemID, 0)
	if err != nil {
		return err
	}
	var others []repo.Message
	for _, m := range msgs {
		if m.SentByMe {
			return nil
		}
		others = append(others, m)
	}
	if len(others) == 0 || a.store.HasProtectedDirect(ctx, itemID) {
		return nil
	}
	for _, r := range rules {
		if r.TestRun != it.TestRun {
			continue
		}
		same, hit := a.skipRuleHits(ctx, r, it, others)
		if hit {
			return a.closeByRule(ctx, r, it.ID, same, it.LastAt)
		}
	}
	return nil
}

// skipRuleHits says whether a rule applies to an item whose messages are all
// from others. same is the number of the first copy for the same mail.
func (a *app) skipRuleHits(ctx context.Context, r repo.SkipRule, it repo.Item, others []repo.Message) (same string, hit bool) {
	// The same mail and the calendar notice are about mail. A rule for a sender
	// is about whoever wrote, in mail or in a chat (SPEC 31: "every item whose
	// messages are all from that sender").
	if r.Kind != skipSender {
		for _, m := range others {
			if m.Network != "mail" {
				return "", false
			}
		}
	}
	switch r.Kind {
	case skipSameMail:
		if len(others) != 1 {
			return "", false
		}
		n, second, ok := a.store.SameMailOf(ctx, it.ID)
		if !ok || !second {
			return "", false
		}
		return repo.Number(n), true
	case skipCalendar:
		for _, m := range others {
			if !strings.EqualFold(m.Sender, calendarNotice.Sender) || !hasPrefixFold(m.Subject, calendarNotice.SubjectPrefix) {
				return "", false
			}
		}
		return "", true
	case skipSender:
		by := senderBy(r.Value)
		for _, m := range others {
			if !senderIs(m.Sender, r.Value) {
				return "", false
			}
		}
		// The facts that make a rule unsafe are looked at every time, not only when it was proposed.
		if n, err := a.store.AnsweredThreadsIn(ctx, by, r.Value); err != nil || n > 0 {
			return "", false
		}
		if prot, err := a.store.ProtectedSender(ctx, by, r.Value); err != nil || prot {
			return "", false
		}
		probe := r.Value
		if by == "domain" {
			probe = "x@" + r.Value
		}
		if blockedBy(a.blockedTargets(ctx, a.store.Q), probe) {
			return "", false
		}
		return "", true
	}
	return "", false
}

// senderBy says whether a skip rule's value is an address or a domain.
func senderBy(value string) string {
	if strings.Contains(value, "@") {
		return "address"
	}
	return "domain"
}

func senderIs(sender, value string) bool {
	sender, value = strings.ToLower(strings.TrimSpace(sender)), strings.ToLower(strings.TrimSpace(value))
	if senderBy(value) == "address" {
		return sender == value
	}
	i := strings.LastIndex(sender, "@")
	return i >= 0 && sender[i+1:] == value
}

// closeByRule is the one place a skip rule closes an item. The item goes to
// Closed with `Closed by a rule: <name>`; the event is the app's own ("rule"),
// so it counts in the sender's skipped number and the item can be brought back.
func (a *app) closeByRule(ctx context.Context, r repo.SkipRule, itemID int64, sameAs string, lastAt int64) error {
	now := a.now().Unix()
	return a.store.Update(ctx, func(tx *repo.Tx) error {
		cur, ok := tx.ItemByID(ctx, itemID) // look again inside the transaction
		if !ok || !cur.NeedsClaude || cur.Stage != stageNew || cur.TestRun != r.TestRun {
			return nil
		}
		if cur.LastAt > lastAt || tx.SkipRuleProtected(ctx, cur.ID) {
			return nil // a message came, or the owner acted, since the rule looked
		}
		live, ok := tx.SkipRuleByID(ctx, r.ID)
		if !ok || live.Status != "active" || !live.Enabled {
			return nil
		}
		if err := tx.SetItemStage(ctx, cur.ID, stageClosed, now); err != nil {
			return err
		}
		if _, err := tx.AnswerReview(ctx, cur.ID, now); err != nil {
			return err
		}
		if _, err := tx.WithdrawPending(ctx, cur.ID, "the item was closed", now); err != nil {
			return err
		}
		data := map[string]any{"kind": "rule", "rule": r.ID, "rule_name": r.Name, "reason": ruleClosedText(r.Name), "item": repo.Number(cur.Number)}
		if sameAs != "" {
			data["same_as"] = sameAs
		}
		return tx.InsertEvent(ctx, now, cur.ID, "rule", "skipped", data)
	})
}

func ruleClosedText(name string) string { return "Closed by a rule: " + name }

// skipRulesPass applies the skip rules to every item that waits for Claude,
// so a rule the owner has just approved reaches what already waits.
func (a *app) skipRulesPass(ctx context.Context) {
	rules, err := a.store.EnabledSkipRules(ctx)
	if err != nil || len(rules) == 0 {
		return
	}
	rows, err := a.store.ReviewQueue(ctx, 200)
	if err != nil {
		return
	}
	for _, r := range rows {
		if ctx.Err() != nil {
			return
		}
		if err := a.applySkipRules(ctx, r.ID); err != nil {
			appkit.Error("applying skip rules", err)
		}
	}
}

// pauseRuleBehind pauses the skip rule that closed an item, because the owner or
// the night check brought it back. It runs inside the transaction that brings
// the item back; by names who did it.
func (a *app) pauseRuleBehind(ctx context.Context, tx *repo.Tx, it repo.Item, by, reason string, now int64) error {
	c, ok := tx.ClosingOf(ctx, it.ID)
	if !ok || c.Type != "skipped" || c.Actor != "rule" {
		return nil
	}
	var d struct {
		Rule int64 `json:"rule"`
	}
	json.Unmarshal([]byte(c.Data), &d)
	rule, found := tx.SkipRuleByID(ctx, d.Rule)
	if !found {
		return nil
	}
	why := fmt.Sprintf("%s brought back %s.", by, repo.Number(it.Number))
	if reason != "" {
		why = fmt.Sprintf("%s brought back %s: %s", by, repo.Number(it.Number), strings.TrimSuffix(reason, "."))
		why += "."
	}
	changed, err := tx.PauseSkipRule(ctx, rule.ID, now, why, it.ID)
	if err != nil || !changed {
		return err
	}
	return tx.InsertEventFor(ctx, "skip_rules", rule.ID, now, "system", "skip_rule_paused", map[string]any{"skip_rule": rule.ID, "name": rule.Name, "why": why})
}

// --- the proposal of an agent or of the app's own model (kind sender) ---

var skipRuleTools = []mcpTool{
	{"propose_skip_rule", "Propose a skip rule for a sender",
		"Propose a skip rule: from now on ONE closes, without anyone looking, every item whose messages are all from one sender address (field sender) or one mailing domain (field sender_domain, written like news.shop.test). It does nothing until the owner presses Approve on the Skip rules section of Settings; he sees it marked Proposal with your reason and the numbers. Only for a sender whose items agents skipped again and again and the owner never brought back. Read the numbers first with history_numbers (agent_skipped and brought_back) and put them in the reason, for example \"skipped 12 times, brought back 0 times\". The server refuses the rule, and says why, when the owner has ever answered that sender, when any item of that sender was ever brought back, when any item of that sender ever got a comment, a summary, a note of the owner's or a proposal (it has mattered at least once), or when fewer than the set number of items of that sender were skipped (5 to begin with). It also refuses a sender who is a protected person or on the list of Blocked targets. Never propose it for a sender that can also send something that matters by its content: a bank, an office, a school, a tenant, a carrier, a shop that also sends invoices. The same rule twice returns the one that waits. A skip rule is paused at once when an item it closed is brought back.",
		schema(map[string]any{"field": map[string]any{"type": "string", "enum": []string{"sender", "sender_domain"}}, "value": map[string]any{"type": "string", "description": "The sender address, or the mailing domain without @."},
			"reason": strT, "name": strT, "agent": agentT}, "field", "value", "reason"),
		hints("Propose a skip rule for a sender", false, false, true)},
}

func init() { mcpTools = append(mcpTools, skipRuleTools...) }

type skipProposalBody struct {
	Agent  string `json:"agent"`
	Name   string `json:"name"`
	Field  string `json:"field"`
	Value  string `json:"value"`
	Reason string `json:"reason"`
}

type skipProposalResult struct {
	ID      int64
	Same    bool
	Numbers map[string]any
}

// refuseSkip is a skip rule the server will not make, with its reason.
func refuseSkip(format string, args ...any) error {
	return &userErr{http.StatusConflict, "Refused: " + fmt.Sprintf(format, args...)}
}

// proposeSkipRule is the one function behind REST and MCP.
func (a *app) proposeSkipRule(ctx context.Context, x actor, in skipProposalBody) (skipProposalResult, error) {
	var res skipProposalResult
	if err := onlyAgent(x); err != nil {
		return res, a.logRefusal(ctx, x, 0, err)
	}
	if in.Field != "sender" && in.Field != "sender_domain" {
		return res, bad("field must be sender or sender_domain")
	}
	value := strings.ToLower(strings.TrimSpace(strings.ToValidUTF8(in.Value, "")))
	by := "address"
	switch {
	case in.Field == "sender":
		if !strings.Contains(value, "@") || strings.ContainsAny(value, " ") {
			return res, bad("a sender is an address like bulk@shop.test; for a whole mailing domain use field sender_domain")
		}
	default:
		value = strings.TrimPrefix(value, "@")
		by = "domain"
		if !strings.Contains(value, ".") || strings.ContainsAny(value, "@ ") {
			return res, bad("a sender domain is written like news.shop.test, without @")
		}
	}
	if len(value) > maxRuleValue {
		return res, bad("the value is too long")
	}
	reason, err := checkField("reason", in.Reason, maxRuleReason, true)
	if err != nil {
		return res, err
	}
	if !strings.ContainsAny(reason, "0123456789") {
		return res, bad("the reason must carry the numbers behind the rule (how many items were skipped, how many brought back): read them with history_numbers")
	}
	name, err := checkField("name", in.Name, maxRuleName, false)
	if err != nil {
		return res, err
	}
	if name == "" {
		name = "Skip mail from " + value
	}
	now := a.now().Unix()
	min := a.skipMin(ctx)
	err = a.store.Update(ctx, func(tx *repo.Tx) error {
		if same, ok := tx.SkipRuleLive(ctx, skipSender, value); ok {
			res.ID, res.Same = same.ID, true
			return nil
		}
		// The refusals, in the order the spec lists them. Each says why.
		if n, err := tx.AnsweredThreadsIn(ctx, by, value); err != nil {
			return err
		} else if n > 0 {
			return refuseSkip("The owner has answered %s from %s. A skip rule never closes mail from someone he has answered.", plural(int64(n), "thread"), value)
		}
		if n, err := tx.SenderBroughtBack(ctx, by, value); err != nil {
			return err
		} else if n > 0 {
			return refuseSkip("%s from %s %s brought back by the owner or the night check, so it has mattered.", plural(int64(n), "item"), value, pluralVerb(n, "was", "were"))
		}
		if n, err := tx.SenderMattered(ctx, by, value); err != nil {
			return err
		} else if n > 0 {
			return refuseSkip("%s from %s got a comment, a summary, a note or a proposal that was not a skip: it has mattered at least once.", plural(int64(n), "item"), value)
		}
		skipped, _, err := tx.AgentSkips(ctx, by, value)
		if err != nil {
			return err
		}
		if skipped < min {
			return refuseSkip("only %s from %s %s skipped; at least %d are needed before a skip rule is proposed.", plural(int64(skipped), "item"), value, pluralVerb(skipped, "was", "were"), min)
		}
		if prot, err := tx.ProtectedSender(ctx, by, value); err != nil {
			return err
		} else if prot {
			return refuse("Protected people", whyProtectedRule)
		}
		probe := value
		if by == "domain" {
			probe = "x@" + value
		}
		if blockedBy(a.blockedTargets(ctx, tx.Q), probe) {
			return refuse("Blocked targets", whyBlockedSkip)
		}
		if tx.CountSkipRuleProposals(ctx) >= maxSkipPropose {
			return conflict("Many skip rule proposals already wait for the owner.")
		}
		if tx.CountSkipRules(ctx) >= maxSkipRules {
			return conflict("There are already as many skip rules as the app keeps.")
		}
		id, err := tx.InsertSkipRule(ctx, repo.SkipRule{Name: name, Kind: skipSender, Value: value, Reason: reason, ProposedBy: x.id(), ProposedAt: now})
		if err != nil {
			return err
		}
		res.ID = id
		return tx.InsertEvent(ctx, now, 0, x.id(), "skip_rule_proposed", map[string]any{"skip_rule": id, "name": name, "reason": reason})
	})
	if err != nil {
		var ref *refusal
		if errors.As(err, &ref) {
			return res, a.logRefusal(ctx, x, 0, err)
		}
		var ue *userErr
		if errors.As(err, &ue) && strings.HasPrefix(ue.Msg, "Refused:") {
			a.store.Update(context.WithoutCancel(ctx), func(tx *repo.Tx) error {
				return tx.InsertEvent(ctx, now, 0, x.id(), "skip_rule_refused", map[string]any{"name": name, "why": ue.Msg})
			})
		}
		return res, err
	}
	sk, back, _ := a.store.AgentSkips(ctx, by, value)
	res.Numbers = map[string]any{"skipped": sk, "brought_back": back}
	return res, nil
}

func pluralVerb(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

func (a *app) apiProposeSkipRule(w http.ResponseWriter, r *http.Request) {
	var b skipProposalBody
	if !readJSON(w, r, &b) {
		return
	}
	res, err := a.proposeSkipRule(r.Context(), agentActor(b.Agent), b)
	if err != nil {
		answerErr(w, err)
		return
	}
	out := map[string]any{"ok": true, "skip_rule_proposal_id": res.ID, "waits_for_owner": true}
	if res.Same {
		out["unchanged"] = true
	}
	if res.Numbers != nil {
		out["numbers"] = res.Numbers
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *app) callSkipRuleTool(ctx context.Context, name string, args json.RawMessage) (text string, isErr, known bool) {
	if name != "propose_skip_rule" {
		return "", false, false
	}
	var in skipProposalBody
	if err := json.Unmarshal(args, &in); err != nil {
		return "The arguments must be a JSON object.", true, true
	}
	body, _ := json.Marshal(in)
	text, isErr = a.callAPI(ctx, "POST", "/api/skip-rule-proposals", body)
	return text, isErr, true
}

// --- Settings: the section Skip rules (the signed-in page; the owner is the actor) ---

type skipRuleView struct {
	ID        int64
	Name      string
	What      string // in a sentence, what the rule looks at
	Reason    string
	Numbers   string
	By        string
	Proposal  bool
	Enabled   bool
	Paused    bool
	PausedWhy string
	Closed    int
}

type skipRulesView struct {
	Proposals []skipRuleView
	Rules     []skipRuleView
}

func (a *app) skipWhat(r repo.SkipRule) string {
	switch r.Kind {
	case skipSameMail:
		return "Closes the second copy of a mail that arrived in both mailboxes, found by its internet message id. The first copy stays; the second says which one it is the same as."
	case skipCalendar:
		return fmt.Sprintf("Closes a notice that an entry was added to a shared calendar: mail from %s whose subject begins with %q.", calendarNotice.Sender, calendarNotice.SubjectPrefix)
	}
	if senderBy(r.Value) == "domain" {
		return "Closes an item whose messages are all from the domain " + r.Value + "."
	}
	return "Closes an item whose messages are all from " + r.Value + "."
}

func (a *app) skipNumbers(ctx context.Context, r repo.SkipRule) string {
	switch r.Kind {
	case skipSender:
		sk, back, err := a.store.AgentSkips(ctx, senderBy(r.Value), r.Value)
		if err != nil {
			return ""
		}
		return fmt.Sprintf("Skipped %s, brought back %s.", plural(int64(sk), "time"), plural(int64(back), "time"))
	}
	return a.seenText(ctx, a.store.Q, r.Kind, 0)
}

func (a *app) skipRulesViewFor(ctx context.Context) skipRulesView {
	var v skipRulesView
	rules, err := a.store.ListSkipRules(ctx)
	if err != nil {
		appkit.Error("listing skip rules", err)
		return v
	}
	for _, r := range rules {
		sv := skipRuleView{ID: r.ID, Name: r.Name, What: a.skipWhat(r), Reason: r.Reason, Numbers: a.skipNumbers(ctx, r), By: actorLabel(r.ProposedBy),
			Enabled: r.Enabled, Closed: a.store.SkipRuleClosed(ctx, r.ID)}
		if r.Status == "proposed" {
			sv.Proposal = true
			if r.Kind != skipSender {
				sv.Reason = "" // the number says it
			}
			v.Proposals = append(v.Proposals, sv)
			continue
		}
		if r.PausedAt > 0 && !r.Enabled {
			sv.Paused = true
			sv.PausedWhy = r.PausedWhy
		}
		v.Rules = append(v.Rules, sv)
	}
	return v
}

func skipRuleID(r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	return id, err == nil && id > 0
}

// decideSkipRuleHandler is Approve or Dismiss on a proposed skip rule: the one
// place such a rule starts to apply. The actor is the owner because of the channel.
func (a *app) decideSkipRuleHandler(w http.ResponseWriter, r *http.Request) {
	id, ok := skipRuleID(r)
	decision := r.PathValue("decision")
	if !ok || (decision != "approve" && decision != "dismiss") {
		http.NotFound(w, r)
		return
	}
	err := a.decideSkipRule(r.Context(), ownerActor(), id, decision == "approve")
	var ue *userErr
	switch {
	case errors.As(err, &ue):
		http.Error(w, ue.Msg, ue.Status)
		return
	case err != nil:
		appkit.Error("deciding a skip rule proposal", err)
		http.Error(w, "That did not work. Try again.", http.StatusInternalServerError)
		return
	}
	n := "skipdismissed"
	if decision == "approve" {
		n = "skipapproved"
		a.async(func() { a.skipRulesPass(context.Background()) }) // what waits and fits is closed now
	}
	http.Redirect(w, r, "/settings?n="+n+"#h-skiprules", http.StatusSeeOther)
}

// decideSkipRule is the one function that turns a proposed skip rule on or
// dismisses it. Only the owner, on the signed-in page, can call it.
func (a *app) decideSkipRule(ctx context.Context, x actor, id int64, approve bool) error {
	if !x.owner {
		return a.logRefusal(ctx, x, 0, refuse("Approval only", whyApprovalOnly))
	}
	now := a.now().Unix()
	return a.store.Update(ctx, func(tx *repo.Tx) error {
		rule, found := tx.SkipRuleByID(ctx, id)
		if !found {
			return &userErr{http.StatusNotFound, "no such proposal"}
		}
		changed, err := tx.DecideSkipRule(ctx, id, approve, x.id(), now)
		if err != nil || !changed {
			return err
		}
		typ := "skip_rule_dismissed"
		if approve {
			typ = "skip_rule_approved"
		}
		return tx.InsertEventFor(ctx, "skip_rules", id, now, x.id(), typ, map[string]any{"skip_rule": id, "name": rule.Name, "proposed_by": rule.ProposedBy})
	})
}

// switchSkipRule is the switch Enabled. Switching a paused rule on is the one
// way it applies again.
func (a *app) switchSkipRule(w http.ResponseWriter, r *http.Request) {
	id, ok := skipRuleID(r)
	state := r.PathValue("state")
	if !ok || (state != "on" && state != "off") {
		http.NotFound(w, r)
		return
	}
	rule, found := a.store.SkipRuleByID(r.Context(), id)
	if !found || rule.Status != "active" {
		http.NotFound(w, r)
		return
	}
	now := a.now().Unix()
	err := a.store.Update(r.Context(), func(tx *repo.Tx) error {
		changed, err := tx.SetSkipRuleEnabled(r.Context(), id, state == "on")
		if err != nil || !changed {
			return err
		}
		return tx.InsertEventFor(r.Context(), "skip_rules", id, now, "owner", "skip_rule_switched", map[string]any{"skip_rule": id, "name": rule.Name, "to": state})
	})
	if err != nil {
		appkit.Error("switching a skip rule", err)
		http.Error(w, "That did not work. Try again.", http.StatusInternalServerError)
		return
	}
	if state == "on" {
		a.async(func() { a.skipRulesPass(context.Background()) })
	}
	http.Redirect(w, r, "/settings?n=skip"+state+"#h-skiprules", http.StatusSeeOther)
}
