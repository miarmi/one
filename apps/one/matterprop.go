package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"lab/apps/one/repo"
	"lab/platform/appkit"
)

// An agent proposes a matter pattern (SPEC 10, 12). The proposal is stored
// apart from the owner's patterns and shown in Settings under Matter patterns,
// marked Proposal, with Approve and Dismiss. It joins nothing until the owner
// presses Approve on the signed-in page. There is no address and no tool that
// approves, adds or removes a pattern.

const maxMatterProposals = 10

var matterPatternTools = []mcpTool{
	{"propose_pattern", "Propose a matter pattern",
		"Propose a pattern that finds the reference of a matter (an order, parcel, invoice or file number) in the subject or the text of mail, so that mail about the same reference joins one row even when it arrives as separate mails. pattern is a regular expression; if it has a group in brackets, the group is the reference, otherwise the whole match (capital letters do not matter), for example INV-([0-9]+). It must not match an empty text. Give a reason the owner can read, with a real example of the reference from his mail. Similar sender or subject alone never joins rows: only the same reference does. Nothing joins until the owner presses Approve on the Matter patterns section of Settings, where he sees the proposal marked Proposal. The same pattern twice returns the one that waits. There is no tool that approves, adds or removes a pattern.",
		schema(map[string]any{"pattern": strT, "name": strT, "reason": strT, "agent": agentT}, "pattern", "reason"),
		hints("Propose a matter pattern", false, false, true)},
}

func init() { mcpTools = append(mcpTools, matterPatternTools...) }

type patternProposalBody struct {
	Agent   string `json:"agent"`
	Name    string `json:"name"`
	Pattern string `json:"pattern"`
	Reason  string `json:"reason"`
}

type patternProposalResult struct {
	ID        int64
	Same      bool
	WouldJoin int
}

// proposeMatterPattern is the one function behind REST and MCP.
func (a *app) proposeMatterPattern(ctx context.Context, x actor, in patternProposalBody) (patternProposalResult, error) {
	var res patternProposalResult
	if err := onlyAgent(x); err != nil {
		return res, a.logRefusal(ctx, x, 0, err)
	}
	re := strings.TrimSpace(strings.ToValidUTF8(in.Pattern, ""))
	if msg := checkMatterRegex(re); msg != "" {
		return res, bad(msg)
	}
	name := oneLine(strings.TrimSpace(strings.ToValidUTF8(in.Name, "")), maxMatterName)
	if name == "" {
		name = oneLine(re, maxMatterName)
	}
	reason, err := checkField("reason", in.Reason, maxRuleReason, true)
	if err != nil {
		return res, err
	}
	now := a.now().Unix()
	// Counted before the write transaction opens (it only reads).
	joins := a.countMatterJoins(ctx, name, re)
	err = a.store.Update(ctx, func(tx *repo.Tx) error {
		if same, ok := tx.SameMatterProposal(ctx, re); ok {
			res.ID, res.Same = same.ID, true
			return nil
		}
		if slices.ContainsFunc(a.matterPatternsFor(ctx, tx.Q), func(p matterPattern) bool { return p.Re == re }) {
			return conflict("The owner already has a pattern that says the same.")
		}
		if tx.CountMatterProposals(ctx) >= maxMatterProposals {
			return conflict("Many pattern proposals already wait for the owner.")
		}
		id, err := tx.InsertMatterProposal(ctx, repo.MatterProposal{Name: name, Pattern: re, Reason: reason, ProposedBy: x.id(), ProposedAt: now, WouldJoin: joins})
		if err != nil {
			return err
		}
		res.ID, res.WouldJoin = id, joins
		return tx.InsertEventFor(ctx, "matter_proposals", id, now, x.id(), "pattern_proposed", map[string]any{"pattern_proposal": id, "name": name, "pattern": re, "reason": reason, "would_join": joins})
	})
	return res, err
}

func (a *app) apiProposePattern(w http.ResponseWriter, r *http.Request) {
	var b patternProposalBody
	if !readJSON(w, r, &b) {
		return
	}
	res, err := a.proposeMatterPattern(r.Context(), agentActor(b.Agent), b)
	if err != nil {
		answerErr(w, err)
		return
	}
	out := map[string]any{"ok": true, "pattern_proposal_id": res.ID, "waits_for_owner": true}
	if !res.Same {
		out["would_join_rows"] = res.WouldJoin
	}
	if res.Same {
		out["unchanged"] = true
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *app) callMatterTool(ctx context.Context, name string, args json.RawMessage) (text string, isErr, known bool) {
	switch name {
	case "propose_pattern":
		var in patternProposalBody
		if err := json.Unmarshal(args, &in); err != nil {
			return "The arguments must be a JSON object.", true, true
		}
		body, _ := json.Marshal(in)
		text, isErr = a.callAPI(ctx, "POST", "/api/pattern-proposals", body)
		return text, isErr, true
	case "propose_merge", "propose_split":
		return a.callRowTool(ctx, name, args)
	}
	return "", false, false
}

// --- Settings: Approve and Dismiss (the signed-in page; the owner is the actor) ---

type matterProposalView struct {
	ID     int64
	Name   string
	Re     string
	Reason string
	By     string
	Joins  string // what the pattern would have joined, in words; "" when not counted
}

type matterSection struct {
	Patterns  []matterView
	Proposals []matterProposalView
}

func (a *app) matterSectionFor(ctx context.Context) matterSection {
	v := matterSection{Patterns: a.matterViewsFor(ctx)}
	ps, err := a.store.WaitingMatterProposals(ctx)
	if err != nil {
		appkit.Error("listing matter proposals", err)
		return v
	}
	for _, p := range ps {
		v.Proposals = append(v.Proposals, matterProposalView{ID: p.ID, Name: p.Name, Re: p.Pattern, Reason: p.Reason, By: actorLabel(p.ProposedBy), Joins: joinsText(p.WouldJoin)})
	}
	return v
}

func (a *app) decideMatterProposalHandler(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	decision := r.PathValue("decision")
	if err != nil || id <= 0 || (decision != "approve" && decision != "dismiss") {
		http.NotFound(w, r)
		return
	}
	switch err := a.decideMatterProposal(r.Context(), ownerActor(), id, decision == "approve"); {
	case err == nil:
	case isUserErr(err):
		var ue *userErr
		errors.As(err, &ue)
		a.renderSettings(w, r, ue.Status, "", ue.Msg)
		return
	default:
		appkit.Error("deciding a pattern proposal", err)
		http.Error(w, "That did not work. Try again.", http.StatusInternalServerError)
		return
	}
	n := "patterndismissed"
	if decision == "approve" {
		n = "patternapproved"
	}
	http.Redirect(w, r, "/settings?n="+n+"#h-matter", http.StatusSeeOther)
}

func isUserErr(err error) bool {
	var ue *userErr
	return errors.As(err, &ue)
}

// decideMatterProposal is the one function that turns a proposed pattern into
// one of the owner's or dismisses it. Only the owner, on the signed-in page, can call
// it. A proposal made under a test run becomes a pattern of that run alone
// (it stays with the run's records); a real one is added to the owner's patterns.
func (a *app) decideMatterProposal(ctx context.Context, x actor, id int64, approve bool) error {
	if !x.owner {
		return a.logRefusal(ctx, x, 0, refuse("Approval only", whyApprovalOnly))
	}
	now := a.now()
	return a.store.Update(ctx, func(tx *repo.Tx) error {
		p, found := tx.MatterProposalByID(ctx, id)
		if !found {
			return &userErr{http.StatusNotFound, "no such proposal"}
		}
		if p.Status != "proposed" {
			return nil // a second tap, a second device: nothing to write
		}
		if approve && p.TestRun == "" {
			ps := a.matterPatterns(ctx, tx.Q)
			if !slices.ContainsFunc(ps, func(m matterPattern) bool { return m.Re == p.Pattern }) {
				if len(ps) >= maxMatterPatterns {
					return conflict(fmt.Sprintf("There are already %d patterns. Remove one first.", maxMatterPatterns))
				}
				ps = append(ps, matterPattern{Name: p.Name, Re: p.Pattern})
				if err := tx.SetSettingBy(ctx, keyMatterPatterns, encodeMatterPatterns(ps), "owner", now); err != nil {
					return err
				}
				if err := tx.InsertEvent(ctx, now.Unix(), 0, "owner", "setting_changed", map[string]any{"matter_patterns": len(ps)}); err != nil {
					return err
				}
			}
		}
		if _, err := tx.DecideMatterProposal(ctx, id, approve, x.id(), now.Unix()); err != nil {
			return err
		}
		typ := "pattern_dismissed"
		if approve {
			typ = "pattern_approved"
		}
		return tx.InsertEventFor(ctx, "matter_proposals", id, now.Unix(), x.id(), typ, map[string]any{"pattern_proposal": id, "name": p.Name, "proposed_by": p.ProposedBy})
	})
}

// joinsText says in a sentence what a proposed pattern would have joined.
func joinsText(n int) string {
	switch {
	case n < 0:
		return ""
	case n == 0:
		return "On your mail of the last 90 days it would have joined no rows."
	}
	return fmt.Sprintf("On your mail of the last 90 days it would have joined %s.", plural(int64(n), "row"))
}
