package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"lab/apps/one/repo"
	"lab/platform/appkit"
)

// Import rules (SPEC 21). Settings lists the rules that decide what is not
// imported at all. The owner adds, edits, switches and tests rules, sees what each
// dropped and brings one back with Import again. An agent can only propose a
// rule: it shows with Proposal, Approve and Dismiss and does nothing until
// The owner presses Approve on the signed-in page. There is no address and no tool
// under /api/ that approves a rule, enables one, or changes one.

const (
	maxRuleName     = 100
	maxRuleValue    = 200
	maxRuleReason   = 1000
	maxRuleProposed = 50
	maxRules        = 500  // rules in all (active and waiting): each message is compared with every one
	maxMatchedBytes = 4000 // the most of one value (subject, label, header) a rule looks at
	ruleTestDays    = 30
	ruleTestShown   = 25
	ruleDropShown   = 50
)

// The fields and operators the owner can choose for a rule on mail.
var (
	ruleFieldLabels = []settingOpt{{"sender", "Sender"}, {"sender_domain", "Sender domain"}, {"subject", "Subject"}, {"label", "Label or folder"},
		{"network", "Network (chats)"}, {"chat_type", "Chat type: direct or group (chats)"}, {"muted", "Group chat is muted: true (chats)"}}
	ruleOpLabels   = []settingOpt{{"equals", "is"}, {"contains", "contains"}, {"starts_with", "starts with"}, {"ends_with", "ends with"}}
	ruleSourceOpts = []settingOpt{{"any", "Both mailboxes"}, {srcGmail, "Gmail"}, {srcGraph, "Office mail"}, {"chat", "Chats"}}
)

func optLabel(opts []settingOpt, v string) (string, bool) {
	for _, o := range opts {
		if o.Value == v {
			return o.Label, true
		}
	}
	return v, false
}

// describeRule says what a rule does in a line.
func describeRule(r repo.Rule) string {
	field, _ := optLabel(ruleFieldLabels, r.Field)
	switch {
	case strings.HasPrefix(r.Field, "header:"):
		field = "Header " + strings.TrimPrefix(r.Field, "header:")
	case r.Field == "muted":
		field = "Group chat is muted"
	case r.Field == "folder":
		field = "Label or folder"
	}
	op, _ := optLabel(ruleOpLabels, r.Op)
	src, ok := optLabel(ruleSourceOpts, r.Source)
	if !ok {
		src = sourceLabel(r.Source)
	}
	if r.Field == "muted" {
		return fmt.Sprintf("%s (%s)", field, src)
	}
	return fmt.Sprintf("%s %s %q (%s)", field, op, r.Value, src)
}

type droppedView struct {
	ID         int64
	Sender     string
	Subject    string
	When       string
	WhenFull   string
	ISO        string
	Rule       string
	Source     string
	Reimported bool
}

type ruleView struct {
	ID        int64
	Name      string
	Text      string // what the rule says
	Enabled   bool
	Proposal  bool
	Reason    string
	By        string
	Numbers   string // the numbers the history holds about the sender, for a proposal
	Dropped   int
	Drops     []droppedView
	MoreDrops bool
	// the edit form
	Source, Field, Op, Value string
	Editable                 bool
	Test                     *ruleTestView
}

type ruleTestView struct {
	Title string
	Count int
	Lines []droppedView
	More  int
	Note  string
}

type ruleForm struct {
	ID                             int64
	Name, Source, Field, Op, Value string
}

type rulesView struct {
	Rules     []ruleView
	Proposals []ruleView
	Fields    []settingOpt
	Ops       []settingOpt
	Sources   []settingOpt
	Form      ruleForm
	Test      *ruleTestView // the test of the form
	Error     string
}

func (a *app) droppedViews(list []repo.Skipped, ruleName string) []droppedView {
	now := a.now()
	out := make([]droppedView, 0, len(list))
	for _, s := range list {
		d := droppedView{ID: s.ID, Sender: s.Sender, Subject: s.Subject, Rule: ruleName, Source: sourceLabel(s.Source), Reimported: s.Reimported}
		d.When, d.WhenFull, d.ISO = a.whenText(s.ReceivedAt, now)
		out = append(out, d)
	}
	return out
}

// numbersText says what the history holds of a sender in a line, for a proposal.
func (a *app) numbersText(ctx context.Context, field, value string) string {
	by := "address"
	if field == "sender_domain" {
		by = "domain"
	} else if field != "sender" {
		return ""
	}
	s, ok := a.store.SenderStatOf(ctx, by, value)
	if !ok {
		return "The history holds no mail from " + value + " yet."
	}
	from := time.Unix(s.FirstAt, 0).In(a.loc).Format("2 Jan 2006")
	to := time.Unix(s.LastAt, 0).In(a.loc).Format("2 Jan 2006")
	return fmt.Sprintf("History: %s from %s to %s; you read %d, answered %s, starred %d.", plural(int64(s.Messages), "message"), from, to, s.Read, plural(int64(s.AnsweredThreads), "thread"), s.Starred)
}

func (a *app) rulesViewFor(ctx context.Context) rulesView {
	v := rulesView{Fields: ruleFieldLabels, Ops: ruleOpLabels, Sources: ruleSourceOpts, Form: ruleForm{Source: "any", Field: "sender", Op: "equals"}}
	rules, err := a.store.ListRules(ctx)
	if err != nil {
		appkit.Error("listing rules", err)
		return v
	}
	for _, r := range rules {
		rv := ruleView{ID: r.ID, Name: r.Name, Text: describeRule(r.Rule), Enabled: r.Enabled, Dropped: r.Dropped,
			Source: r.Source, Field: r.Field, Op: r.Op, Value: r.Value,
			Editable: !strings.HasPrefix(r.Field, "header:") && r.Field != "muted"}
		if r.Status == "proposed" {
			rv.Proposal, rv.Reason, rv.By = true, r.Reason, actorLabel(r.ProposedBy)
			rv.Numbers = a.numbersText(ctx, r.Field, r.Value)
			v.Proposals = append(v.Proposals, rv)
			continue
		}
		if r.Dropped > 0 {
			list, _ := a.store.SkippedOfRule(ctx, r.ID, ruleDropShown+1)
			if len(list) > ruleDropShown {
				list, rv.MoreDrops = list[:ruleDropShown], true
			}
			rv.Drops = a.droppedViews(list, r.Name)
		}
		v.Rules = append(v.Rules, rv)
	}
	return v
}

// --- what a rule would have dropped ---

// testRule lists what the rule would have dropped in the last 30 days: the
// messages ONE holds that it matches, and what it already dropped.
func (a *app) testRule(ctx context.Context, r repo.Rule) (*ruleTestView, error) {
	t := &ruleTestView{Title: fmt.Sprintf("Test of %q", r.Name)}
	if r.Name == "" {
		t.Title = "Test"
	}
	if strings.HasPrefix(r.Field, "header:") || r.Field == "muted" || r.Field == "network" || r.Field == "chat_type" {
		t.Note = "This rule looks at something ONE does not keep for a message, so it cannot be tested."
		return t, nil
	}
	since := a.now().Add(-ruleTestDays * 24 * time.Hour).Unix()
	var lines []repo.Skipped
	if r.ID != 0 {
		if dropped, err := a.store.SkippedSince(ctx, r.ID, since, 500); err == nil {
			lines = append(lines, dropped...)
		}
	}
	mails, err := a.store.MailSince(ctx, since, 5000)
	if err != nil {
		return nil, err
	}
	for _, m := range mails {
		if r.Source != "any" && r.Source != m.Source {
			continue
		}
		if ruleMatches(r, ruleFacts{Sender: m.Sender, Subject: m.Subject, Labels: m.Labels}) {
			lines = append(lines, repo.Skipped{Source: m.Source, Sender: m.Sender, Subject: m.Subject, ReceivedAt: m.ReceivedAt})
		}
	}
	t.Count = len(lines)
	if len(lines) > ruleTestShown {
		t.More = len(lines) - ruleTestShown
		lines = lines[:ruleTestShown]
	}
	t.Lines = a.droppedViews(lines, r.Name)
	return t, nil
}

// --- reading what the owner typed ---

// ruleFromForm checks a rule as typed and says what is wrong in a sentence.
func ruleFromForm(f ruleForm) (repo.Rule, error) {
	var r repo.Rule
	if _, ok := optLabel(ruleSourceOpts, f.Source); !ok {
		return r, errors.New("Choose which mailbox the rule is for.")
	}
	if _, ok := optLabel(ruleFieldLabels, f.Field); !ok {
		return r, errors.New("Choose what the rule looks at.")
	}
	if _, ok := optLabel(ruleOpLabels, f.Op); !ok {
		return r, errors.New("Choose how the rule compares.")
	}
	value := strings.TrimSpace(strings.ToValidUTF8(f.Value, ""))
	if f.Field == "sender" || f.Field == "sender_domain" {
		value = strings.ToLower(value)
	}
	if f.Field == "sender_domain" {
		value = strings.TrimPrefix(value, "@")
	}
	switch {
	case value == "":
		return r, errors.New("Write what the rule looks for.")
	case utf8.RuneCountInString(value) > maxRuleValue:
		return r, fmt.Errorf("What the rule looks for can have at most %d characters.", maxRuleValue)
	case f.Op != "equals" && utf8.RuneCountInString(value) < 3:
		return r, errors.New("A rule that is not exact needs at least 3 characters, or it would drop far too much.")
	}
	name := strings.TrimSpace(strings.Join(strings.Fields(f.Name), " "))
	if utf8.RuneCountInString(name) > maxRuleName {
		return r, fmt.Errorf("The name can have at most %d characters.", maxRuleName)
	}
	r = repo.Rule{Name: name, Source: f.Source, Field: f.Field, Op: f.Op, Value: value}
	if r.Name == "" {
		r.Name = describeRule(r)
	}
	return r, nil
}

func formOf(r *http.Request) ruleForm {
	return ruleForm{Name: r.FormValue("name"), Source: r.FormValue("source"), Field: r.FormValue("field"), Op: r.FormValue("op"), Value: r.FormValue("value")}
}

// --- handlers (the signed-in page; the owner is the actor by channel) ---

func (a *app) renderRules(w http.ResponseWriter, r *http.Request, code int, notice string, mod func(*settingsView)) {
	v := a.settingsViewFor(r.Context())
	switch notice {
	case "ruleadded":
		v.Notice = "The rule is added."
	case "rulesaved":
		v.Notice = "The rule is saved."
	case "ruleon":
		v.Notice = "The rule is on."
	case "ruleoff":
		v.Notice = "The rule is off."
	case "ruleapproved":
		v.Notice = "You approved this proposal. The rule is on."
	case "ruledismissed":
		v.Notice = "You dismissed this proposal."
	case "again":
		v.Notice = "The message is being imported again. It shows in the list when it is done."
	}
	if mod != nil {
		mod(&v)
	}
	a.render(w, "settings.html", code, pageData{EnvText: a.envText, Version: a.version, Settings: v})
}

func (a *app) addRule(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 8<<10)
	f := formOf(r)
	rule, err := ruleFromForm(f)
	if err == nil {
		rule.Enabled, rule.CreatedBy = true, "owner"
		err = a.store.Update(r.Context(), func(tx *repo.Tx) error {
			if _, same := tx.SameRule(r.Context(), rule.Source, rule.Field, rule.Op, rule.Value); same {
				return errors.New("There is already a rule that says the same.")
			}
			if tx.CountRules(r.Context()) >= maxRules {
				return fmt.Errorf("There are already %d rules. Remove one first.", maxRules)
			}
			id, err := tx.InsertRule(r.Context(), rule, a.now().Unix())
			if err != nil {
				return err
			}
			return tx.InsertEventFor(r.Context(), "rules", id, a.now().Unix(), "owner", "rule_added", map[string]any{"rule": id, "name": rule.Name})
		})
	}
	if err != nil {
		a.renderRules(w, r, http.StatusBadRequest, "", func(v *settingsView) { v.Rules.Form, v.Rules.Error = f, err.Error() })
		return
	}
	http.Redirect(w, r, "/settings?n=ruleadded#h-rules", http.StatusSeeOther)
}

// testRuleForm is Test rule on a rule that is typed but not added.
func (a *app) testRuleForm(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 8<<10)
	f := formOf(r)
	rule, err := ruleFromForm(f)
	var t *ruleTestView
	if err == nil {
		t, err = a.testRule(r.Context(), rule)
	}
	code := http.StatusOK
	if err != nil {
		code = http.StatusBadRequest
	}
	a.renderRules(w, r, code, "", func(v *settingsView) {
		v.Rules.Form, v.Rules.Test = f, t
		if err != nil {
			v.Rules.Error = err.Error()
		}
	})
}

func ruleID(r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	return id, err == nil && id > 0
}

// testRuleOf is Test rule on a rule of the list.
func (a *app) testRuleOf(w http.ResponseWriter, r *http.Request) {
	id, ok := ruleID(r)
	rule, found := repo.Rule{}, false
	if ok {
		rule, found = a.store.RuleByID(r.Context(), id)
	}
	if !found || rule.Status == "dismissed" {
		http.NotFound(w, r)
		return
	}
	t, err := a.testRule(r.Context(), rule)
	if err != nil {
		appkit.Error("testing a rule", err)
		http.Error(w, "The test did not work. Try again.", http.StatusInternalServerError)
		return
	}
	a.renderRules(w, r, http.StatusOK, "", func(v *settingsView) {
		for i := range v.Rules.Rules {
			if v.Rules.Rules[i].ID == id {
				v.Rules.Rules[i].Test = t
			}
		}
		for i := range v.Rules.Proposals {
			if v.Rules.Proposals[i].ID == id {
				v.Rules.Proposals[i].Test = t
			}
		}
	})
}

func (a *app) saveRule(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 8<<10)
	id, ok := ruleID(r)
	old, found := repo.Rule{}, false
	if ok {
		old, found = a.store.RuleByID(r.Context(), id)
	}
	if !found || old.Status != "active" || strings.HasPrefix(old.Field, "header:") || old.Field == "muted" {
		http.NotFound(w, r)
		return
	}
	f := formOf(r)
	rule, err := ruleFromForm(f)
	if err == nil {
		rule.ID = id
		err = a.store.Update(r.Context(), func(tx *repo.Tx) error {
			if same, dup := tx.SameRule(r.Context(), rule.Source, rule.Field, rule.Op, rule.Value); dup && same.ID != id {
				return errors.New("There is already a rule that says the same.")
			}
			if err := tx.UpdateRule(r.Context(), rule); err != nil {
				return err
			}
			return tx.InsertEventFor(r.Context(), "rules", id, a.now().Unix(), "owner", "rule_changed", map[string]any{"rule": id, "name": rule.Name,
				"from": describeRule(old), "to": describeRule(rule)})
		})
	}
	if err != nil {
		a.renderRules(w, r, http.StatusBadRequest, "", func(v *settingsView) { v.Rules.Error = err.Error() })
		return
	}
	http.Redirect(w, r, "/settings?n=rulesaved#h-rules", http.StatusSeeOther)
}

// switchRule is the switch Enabled. Only this signed-in page does it.
func (a *app) switchRule(w http.ResponseWriter, r *http.Request) {
	id, ok := ruleID(r)
	state := r.PathValue("state")
	if !ok || (state != "on" && state != "off") {
		http.NotFound(w, r)
		return
	}
	rule, found := a.store.RuleByID(r.Context(), id)
	if !found || rule.Status != "active" {
		http.NotFound(w, r)
		return
	}
	err := a.store.Update(r.Context(), func(tx *repo.Tx) error {
		changed, err := tx.SetRuleEnabled(r.Context(), id, state == "on")
		if err != nil || !changed {
			return err
		}
		return tx.InsertEventFor(r.Context(), "rules", id, a.now().Unix(), "owner", "rule_switched", map[string]any{"rule": id, "name": rule.Name, "to": state})
	})
	if err != nil {
		appkit.Error("switching a rule", err)
		http.Error(w, "That did not work. Try again.", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/settings?n=rule"+state+"#h-rules", http.StatusSeeOther)
}

// decideRuleHandler is Approve or Dismiss on a proposed rule: the one place a
// proposed rule starts to apply, and the actor is the owner because of the channel.
func (a *app) decideRuleHandler(w http.ResponseWriter, r *http.Request) {
	id, ok := ruleID(r)
	decision := r.PathValue("decision")
	if !ok || (decision != "approve" && decision != "dismiss") {
		http.NotFound(w, r)
		return
	}
	err := a.decideRule(r.Context(), ownerActor(), id, decision == "approve")
	var ue *userErr
	switch {
	case errors.As(err, &ue):
		http.Error(w, ue.Msg, ue.Status)
		return
	case err != nil:
		appkit.Error("deciding a rule proposal", err)
		http.Error(w, "That did not work. Try again.", http.StatusInternalServerError)
		return
	}
	n := "ruledismissed"
	if decision == "approve" {
		n = "ruleapproved"
	}
	http.Redirect(w, r, "/settings?n="+n+"#h-rules", http.StatusSeeOther)
}

// decideRule is the one function that turns a proposed rule on or dismisses
// it. Only the owner, on the signed-in page, can call it; an agent that gets into
// it is refused by the guard Approval only. A second press does nothing.
func (a *app) decideRule(ctx context.Context, x actor, id int64, approve bool) error {
	if !x.owner {
		return a.logRefusal(ctx, x, 0, refuse("Approval only", whyApprovalOnly))
	}
	now := a.now().Unix()
	return a.store.Update(ctx, func(tx *repo.Tx) error {
		rule, found := tx.RuleByID(ctx, id)
		if !found || rule.ProposedBy == "" { // only a rule an agent proposed is decided here
			return &userErr{http.StatusNotFound, "no such proposal"}
		}
		changed, err := tx.DecideRule(ctx, id, approve, x.id(), now)
		if err != nil || !changed {
			return err
		}
		typ := "rule_dismissed"
		if approve {
			typ = "rule_approved"
		}
		return tx.InsertEventFor(ctx, "rules", id, now, x.id(), typ, map[string]any{"rule": id, "name": rule.Name, "proposed_by": rule.ProposedBy, "text": describeRule(rule)})
	})
}

// importAgain is the button on a dropped message: it is imported again with
// the rules left out for it. It reaches into the mailbox, so it runs behind the
// page and the list shows it when it is done.
func (a *app) importAgainHandler(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	sk, ok := repo.Skipped{}, false
	if err == nil {
		sk, ok = a.store.SkippedByID(r.Context(), id)
	}
	if !ok {
		http.NotFound(w, r)
		return
	}
	if !sk.Reimported && sk.Source != srcGmail && sk.Source != srcGraph {
		// A dropped chat message: the Mac has the chat, ONE has only the line. The
		// request waits for the bridge's next visit, which brings the message again
		// with the rules left out.
		if err := a.store.Update(r.Context(), func(tx *repo.Tx) error { return tx.RequestAgain(r.Context(), sk.ID, a.now().Unix()) }); err != nil {
			appkit.Error("asking for a chat message again", err)
			http.Error(w, "That did not work. Try again.", http.StatusInternalServerError)
			return
		}
		http.Redirect(w, r, "/settings?n=againchat#h-rules", http.StatusSeeOther)
		return
	} else if !sk.Reimported {
		a.async(func() {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
			defer cancel()
			var err error
			switch sk.Source {
			case srcGmail:
				err = a.imp.Reimport(ctx, sk.ID)
			case srcGraph:
				err = a.gimp.Reimport(ctx, sk.ID)
			default:
				err = errors.New("unknown source")
			}
			if err != nil {
				a.imp.log.Warn("import again failed", "source", sk.Source)
				return
			}
			// What the owner brought back is never closed by a skip rule again.
			a.store.Update(ctx, func(tx *repo.Tx) error {
				it, found := tx.MessageItem(ctx, sk.Source, sk.SourceID)
				if !found || it.TestRun != "" {
					return nil
				}
				return tx.InsertEvent(ctx, a.now().Unix(), it.ID, "owner", "brought_back", map[string]any{"item": repo.Number(it.Number), "from": "dropped", "to": it.Stage})
			})
		})
	}
	http.Redirect(w, r, "/settings?n=again#h-rules", http.StatusSeeOther)
}

var _ = url.Values{}
