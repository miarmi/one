package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"unicode/utf8"

	"lab/apps/one/repo"
)

// An agent proposes an import rule (SPEC 21). The proposal is a rule that is
// stored switched off with the status proposed; it shows in Settings under
// Import rules, marked Proposal, with Approve and Dismiss, and does nothing
// until the owner presses Approve on the signed-in page. There is no address and
// no tool that approves, enables or changes a rule.
//
// A rule is proposed only for mail a machine sends in bulk that the owner has never
// answered. The server refuses a sender or domain where the owner has answered any
// thread, and a sender who is a protected person ("Always show me").

var ruleTools = []mcpTool{
	{"propose_rule", "Propose an import rule",
		"Propose a rule that drops a kind of mail at import: all mail from one sender address (field sender) or from one sender domain (field sender_domain), op equals. Nothing is dropped until the owner presses Approve on the Import rules section of Settings; he sees the rule marked Proposal with your reason. Only for mail a machine sends in bulk that the owner has never answered and that does not matter by its content (a deadline, a payment, a bank, an office, a school, a tenant, a customer): for anything else, and for \"always show mail from this person\" or \"expected every month, skip unless something changed\", save a note with save_note (give sender) instead. Read the numbers first with history_numbers and put them into the reason, for example \"214 mails in three years, none opened, none answered\". The server refuses the rule when the owner has answered any thread of that sender or domain, and says why. The same rule twice returns the one that waits. There is no tool that approves or enables a rule.",
		schema(map[string]any{"field": map[string]any{"type": "string", "enum": []string{"sender", "sender_domain"}}, "value": map[string]any{"type": "string", "description": "The sender address, or the domain without @."},
			"reason": strT, "name": strT, "source": map[string]any{"type": "string", "enum": []string{"any", "gmail", "outlook"}}, "agent": agentT}, "field", "value", "reason"),
		hints("Propose an import rule", false, false, true)},
}

func init() { mcpTools = append(mcpTools, ruleTools...) }

type ruleProposalBody struct {
	Agent  string `json:"agent"`
	Name   string `json:"name"`
	Source string `json:"source"`
	Field  string `json:"field"`
	Op     string `json:"op"`
	Value  string `json:"value"`
	Reason string `json:"reason"`
}

type ruleProposalResult struct {
	ID      int64
	Same    bool
	Numbers map[string]any
}

// proposeRule is the one function behind REST and MCP.
func (a *app) proposeRule(ctx context.Context, x actor, in ruleProposalBody) (ruleProposalResult, error) {
	var res ruleProposalResult
	if err := onlyAgent(x); err != nil {
		return res, a.logRefusal(ctx, x, 0, err)
	}
	if in.Source == "" {
		in.Source = "any"
	}
	if in.Op == "" {
		in.Op = "equals"
	}
	if in.Field != "sender" && in.Field != "sender_domain" {
		return res, bad("field must be sender or sender_domain: an agent can propose a rule only for a sender address or a sender domain")
	}
	if in.Op != "equals" {
		return res, bad("op must be equals")
	}
	rule, err := ruleFromForm(ruleForm{Name: in.Name, Source: in.Source, Field: in.Field, Op: in.Op, Value: in.Value})
	if err != nil {
		return res, bad(err.Error())
	}
	if in.Field == "sender" && !strings.Contains(rule.Value, "@") {
		return res, bad("a sender is an address like bulk@shop.test; for a whole domain use field sender_domain")
	}
	if in.Field == "sender_domain" && (!strings.Contains(rule.Value, ".") || strings.ContainsAny(rule.Value, "@ ")) {
		return res, bad("a sender domain is written like shop.test, without @")
	}
	reason, err := checkField("reason", in.Reason, maxRuleReason, true)
	if err != nil {
		return res, err
	}
	if !strings.ContainsAny(reason, "0123456789") {
		return res, bad("the reason must carry the numbers behind the rule (how many messages, over which period, how many read, how many answered): read them with history_numbers")
	}
	by := "address"
	if in.Field == "sender_domain" {
		by = "domain"
	}
	now := a.now().Unix()
	err = a.store.Update(ctx, func(tx *repo.Tx) error {
		if same, ok := tx.SameRule(ctx, rule.Source, rule.Field, rule.Op, rule.Value); ok {
			// A test run never answers with a real rule, nor with another run's.
			if err := checkRunRow(ctx, tx.Q, "rules", same.ID); err != nil {
				return err
			}
			res.ID, res.Same = same.ID, true
			return nil
		}
		// The guard in code: nothing the owner has ever answered can get a rule.
		n, err := tx.AnsweredThreadsIn(ctx, by, rule.Value)
		if err != nil {
			return err
		}
		if n > 0 {
			return &userErr{http.StatusConflict, fmt.Sprintf("Refused: The owner has answered %s from %s. A rule never drops mail from someone he has answered. Save a note instead (save_note with sender %s) so your review follows it.",
				plural(int64(n), "thread"), rule.Value, rule.Value)}
		}
		if protected, err := tx.ProtectedSender(ctx, by, rule.Value); err != nil {
			return err
		} else if protected {
			return refuse("Protected people", whyProtectedRule)
		}
		if tx.CountRules(ctx) >= maxRules {
			return conflict("There are already as many rules as the app keeps.")
		}
		if tx.CountRuleProposals(ctx) >= maxRuleProposed {
			return conflict("Many rule proposals already wait for the owner.")
		}
		rule.CreatedBy, rule.Status, rule.Reason, rule.ProposedBy, rule.ProposedAt = x.id(), "proposed", reason, x.id(), now
		rule.Note = "Proposed by " + actorLabel(x.id()) + "."
		id, err := tx.InsertRule(ctx, rule, now)
		if err != nil {
			return err
		}
		res.ID = id
		return tx.InsertEventFor(ctx, "rules", id, now, x.id(), "rule_proposed", map[string]any{"rule": id, "name": rule.Name, "text": describeRule(rule), "reason": reason})
	})
	if err != nil {
		var ref *refusal
		if asRef(err, &ref) {
			return res, a.logRefusal(ctx, x, 0, err)
		}
		a.store.Update(context.WithoutCancel(ctx), func(tx *repo.Tx) error {
			if ue, ok := err.(*userErr); ok && ue.Status == http.StatusConflict && strings.HasPrefix(ue.Msg, "Refused:") {
				return tx.InsertEvent(ctx, now, 0, x.id(), "rule_refused", map[string]any{"name": rule.Name, "text": describeRule(rule), "why": ue.Msg})
			}
			return nil
		})
		return res, err
	}
	if s, ok := a.store.SenderStatOf(ctx, by, rule.Value); ok {
		res.Numbers = map[string]any{"messages": s.Messages, "read": s.Read, "threads_answered": s.AnsweredThreads, "starred": s.Starred,
			"first_at": isoTime(s.FirstAt), "last_at": isoTime(s.LastAt)}
	}
	return res, nil
}

func asRef(err error, target **refusal) bool {
	r, ok := err.(*refusal)
	if ok {
		*target = r
	}
	return ok
}

func (a *app) apiProposeRule(w http.ResponseWriter, r *http.Request) {
	var b ruleProposalBody
	if !readJSON(w, r, &b) {
		return
	}
	res, err := a.proposeRule(r.Context(), agentActor(b.Agent), b)
	if err != nil {
		answerErr(w, err)
		return
	}
	out := map[string]any{"ok": true, "rule_proposal_id": res.ID, "waits_for_owner": true}
	if res.Same {
		out["unchanged"] = true
	}
	if res.Numbers != nil {
		out["history_numbers"] = res.Numbers
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *app) callRuleTool(ctx context.Context, name string, args json.RawMessage) (text string, isErr, known bool) {
	if name != "propose_rule" {
		return "", false, false
	}
	var in ruleProposalBody
	if err := json.Unmarshal(args, &in); err != nil {
		return "The arguments must be a JSON object.", true, true
	}
	body, _ := json.Marshal(in)
	text, isErr = a.callAPI(ctx, "POST", "/api/rule-proposals", body)
	return text, isErr, true
}

var _ = utf8.RuneCountInString
