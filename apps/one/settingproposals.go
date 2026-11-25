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
	"time"

	"lab/apps/one/repo"
	"lab/platform/appkit"
)

// An agent proposes a change to a setting (SPEC 22, 12): the value of a setting
// on the Settings page, a guard that can be switched, or an entry of the
// Blocked targets list. The proposal is stored apart, shown in Settings under
// Proposals from Claude with what changes, from what to what, and why, and
// changes nothing until the owner presses Approve on the signed-in page. Approve
// writes through the same functions as Save (writeSettingChange,
// writeClaudeChange, writeGuard, writeBlocked), and writes the same record.
// A proposal whose "from" no longer matches when the owner approves is withdrawn.
// There is no address and no tool that approves a proposal or changes a setting.
//
// Import rules, skip rules and matter patterns have their own tools; this one
// refuses those kinds and names the right tool. Secrets (keys, tokens,
// passwords, calendar addresses) are not settings of this list at all.

const maxSettingProposals = 20

const (
	spSetting = "setting"
	spGuard   = "guard"
	spTarget  = "blocked_target"
)

var settingProposalKinds = []string{spSetting, spGuard, spTarget, "import_rule", "skip_rule", "matter_pattern"}

var settingProposalTools = []mcpTool{
	{"propose_setting", "Propose a change to a setting",
		"Propose a change to something on the owner's Settings page; nothing changes until he presses Approve there, where he sees what changes, from what to what, and your reason. kind setting: name is the setting as list_settings (kind settings) names it, for example Sync interval or Review model, and value is a value it can have (one of its allowed values, as listed); a value the Settings page would refuse is refused here, not adjusted. kind guard: name is Reason required, Protected people or Blocked targets, value is on or off; a guard that is always on is refused. kind blocked_target: name is a sender address or domain, value is add or remove. Read the current value first with list_settings; a proposal for the value it already has is refused, and one whose current value has changed by the time the owner approves is withdrawn. Keys, tokens, passwords and calendar addresses are never settings here and are refused. For import rules, skip rules and matter patterns use propose_rule, propose_skip_rule and propose_pattern. The same change twice returns the one that waits. There is no tool that approves a proposal or changes a setting.",
		schema(map[string]any{"kind": map[string]any{"type": "string", "enum": settingProposalKinds}, "name": strT, "value": strT, "reason": strT, "agent": agentT}, "kind", "name", "value", "reason"),
		hints("Propose a change to a setting", false, false, true)},
}

func init() { mcpTools = append(mcpTools, settingProposalTools...) }

type settingProposalBody struct {
	Agent  string `json:"agent"`
	Kind   string `json:"kind"`
	Name   string `json:"name"`
	Value  string `json:"value"`
	Reason string `json:"reason"`
}

type settingProposalResult struct {
	ID   int64
	Same bool
	P    repo.SettingProposal
}

// --- the settings an agent may propose a value for ---

type settingEntry struct {
	Key, Name, Value, Text string
	Allowed                []string // the values, as the page words them ("5 minutes"); empty for a free value
	Hint                   string
	Def                    *settingDef
}

// settingCatalog lists every setting of the Settings page that has a value an
// agent may propose: the ones of point 28 and the model's (point 31). Prices
// and everything secret are not in it.
func (a *app) settingCatalog(ctx context.Context) []settingEntry {
	var out []settingEntry
	for i := range settingDefs {
		d := settingDefs[i]
		cur := a.currentSetting(ctx, d)
		e := settingEntry{Key: d.Key, Name: d.Label, Value: cur, Text: d.label(cur), Hint: d.Hint, Def: &settingDefs[i]}
		for _, o := range d.Options {
			e.Allowed = append(e.Allowed, o.Label)
		}
		out = append(out, e)
	}
	for _, f := range a.claudeSettingsFor(ctx).Fields {
		out = append(out, settingEntry{Key: f.Key, Name: f.Label, Value: f.Value, Text: f.Value, Hint: f.Hint})
	}
	return out
}

// secretWords: a name that looks like one of these is refused with its own sentence.
var secretWords = []string{"key", "token", "password", "secret", "credential", "calendar", "address", "login", "sign-in", "api"}

func looksSecret(name string) bool {
	low := strings.ToLower(name)
	for _, w := range secretWords {
		if strings.Contains(low, w) {
			return true
		}
	}
	return false
}

func (e settingEntry) accepts(val string) (stored string, ok bool) {
	val = strings.TrimSpace(val)
	if e.Def != nil {
		for _, o := range e.Def.Options {
			if strings.EqualFold(o.Value, val) || strings.EqualFold(o.Label, val) {
				return o.Value, true
			}
		}
		return "", false
	}
	return val, validClaudeValue(e.Key, val)
}

func (a *app) findSetting(ctx context.Context, name string) (settingEntry, bool) {
	n := strings.ToLower(strings.TrimSpace(name))
	for _, e := range a.settingCatalog(ctx) {
		if strings.ToLower(e.Name) == n || strings.ToLower(e.Key) == n {
			return e, true
		}
	}
	return settingEntry{}, false
}

// proposeSetting is the one function behind REST and MCP.
func (a *app) proposeSetting(ctx context.Context, x actor, in settingProposalBody) (settingProposalResult, error) {
	var res settingProposalResult
	if err := onlyAgent(x); err != nil {
		return res, a.logRefusal(ctx, x, 0, err)
	}
	reason, err := checkField("reason", in.Reason, maxRuleReason, true)
	if err != nil {
		return res, err
	}
	p := repo.SettingProposal{Kind: in.Kind, ProposedBy: x.id(), ProposedAt: a.now().Unix(), Reason: reason}
	name, value := strings.TrimSpace(in.Name), strings.TrimSpace(in.Value)
	switch in.Kind {
	case "import_rule":
		return res, bad("Import rules are proposed with propose_rule, not with propose_setting.")
	case "skip_rule":
		return res, bad("Skip rules are proposed with propose_skip_rule, not with propose_setting.")
	case "matter_pattern":
		return res, bad("Matter patterns are proposed with propose_pattern, not with propose_setting.")
	case spSetting:
		e, ok := a.findSetting(ctx, name)
		if !ok {
			if looksSecret(name) {
				return res, bad("That is not a setting an agent can propose: keys, tokens, passwords and calendar addresses are never settings here.")
			}
			return res, bad("There is no setting with that name. Read the names with list_settings (kind settings).")
		}
		stored, ok := e.accepts(value)
		if !ok {
			allowed := "a value the Settings page accepts for it"
			if len(e.Allowed) > 0 {
				allowed = "one of: " + strings.Join(e.Allowed, ", ")
			}
			return res, bad(fmt.Sprintf("%s: that is not a value this setting can have; it takes %s. Nothing was proposed.", e.Name, allowed))
		}
		if stored == e.Value {
			return res, conflict(fmt.Sprintf("%s is already %s.", e.Name, e.Text))
		}
		p.Name, p.Label, p.From, p.To, p.FromText = e.Key, e.Name, e.Value, stored, e.Text
		p.ToText = stored
		if e.Def != nil {
			p.ToText = e.Def.label(stored)
		}
	case spGuard:
		g, ok := guardNamed(name)
		if !ok {
			if looksSecret(name) {
				return res, bad("That is not a guard.")
			}
			return res, bad("There is no guard with that name. The guards are listed by list_settings (kind guards).")
		}
		if !g.Switchable {
			return res, bad(g.Name + " is always on and cannot be switched off, so it cannot be proposed.")
		}
		to := strings.ToLower(value)
		if to != "on" && to != "off" {
			return res, bad("For a guard the value is on or off.")
		}
		cur := "off"
		if a.guardOn(ctx, a.store.Q, g.Key) {
			cur = "on"
		}
		if cur == to {
			return res, conflict(fmt.Sprintf("%s is already %s.", g.Name, strings.ToUpper(to[:1])+to[1:]))
		}
		p.Name, p.Label, p.From, p.To = g.Key, g.Name, cur, to
		p.FromText, p.ToText = strings.ToUpper(cur[:1])+cur[1:], strings.ToUpper(to[:1])+to[1:]
	case spTarget:
		entry := strings.ToLower(name)
		if !targetEntryRE.MatchString(entry) {
			return res, bad("A blocked target is a sender address like bank@example.com or a domain like example.com.")
		}
		change := strings.ToLower(value)
		if change != "add" && change != "remove" {
			return res, bad("For a blocked target the value is add or remove.")
		}
		on := slices.Contains(a.blockedTargets(ctx, a.store.Q), entry)
		if change == "add" && on {
			return res, conflict(entry + " is already on the Blocked targets list.")
		}
		if change == "remove" && !on {
			return res, conflict(entry + " is not on the Blocked targets list.")
		}
		p.Name, p.Label = entry, "Blocked targets"
		if change == "add" {
			p.From, p.To, p.FromText, p.ToText = "absent", "present", "not on the list", "on the list"
		} else {
			p.From, p.To, p.FromText, p.ToText = "present", "absent", "on the list", "not on the list"
		}
	default:
		return res, bad("kind must be one of: " + strings.Join(settingProposalKinds, ", "))
	}
	now := p.ProposedAt
	err = a.store.Update(ctx, func(tx *repo.Tx) error {
		if same, ok := tx.SameSettingProposal(ctx, p.Kind, p.Name, p.To); ok {
			res.ID, res.Same, res.P = same.ID, true, same
			return nil
		}
		if tx.CountWaitingSettingProposals(ctx) >= maxSettingProposals {
			return conflict("Many setting proposals already wait for the owner.")
		}
		id, err := tx.InsertSettingProposal(ctx, p)
		if err != nil {
			return err
		}
		res.ID, res.P = id, p
		return tx.InsertEventFor(ctx, "setting_proposals", id, now, x.id(), "setting_proposed", map[string]any{"setting_proposal": id, "kind": p.Kind, "name": p.Label, "from": p.FromText, "to": p.ToText, "reason": reason})
	})
	return res, err
}

func guardNamed(name string) (guardInfo, bool) {
	n := strings.ToLower(strings.TrimSpace(name))
	for _, k := range guardsShown {
		g := guardByKey(k)
		if strings.ToLower(g.Name) == n || g.Key == n {
			return g, true
		}
	}
	return guardInfo{}, false
}

func (a *app) apiProposeSetting(w http.ResponseWriter, r *http.Request) {
	var b settingProposalBody
	if !readJSON(w, r, &b) {
		return
	}
	res, err := a.proposeSetting(r.Context(), agentActor(b.Agent), b)
	if err != nil {
		answerErr(w, err)
		return
	}
	out := map[string]any{"ok": true, "setting_proposal_id": res.ID, "waits_for_owner": true,
		"changes": map[string]any{"kind": res.P.Kind, "name": res.P.Label, "from": res.P.FromText, "to": res.P.ToText}}
	if res.Same {
		out["unchanged"] = true
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *app) callSettingProposalTool(ctx context.Context, name string, args json.RawMessage) (text string, isErr, known bool) {
	if name != "propose_setting" {
		return "", false, false
	}
	var in settingProposalBody
	if err := json.Unmarshal(args, &in); err != nil {
		return "The arguments must be a JSON object.", true, true
	}
	body, _ := json.Marshal(in)
	text, isErr = a.callAPI(ctx, "POST", "/api/setting-proposals", body)
	return text, isErr, true
}

// --- Settings: the section, Approve and Dismiss (the signed-in page; the owner is the actor) ---

type settingProposalView struct {
	ID       int64
	Kind     string
	Label    string
	FromText string
	ToText   string
	Reason   string
	By       string
}

func (a *app) settingProposalViews(ctx context.Context) []settingProposalView {
	ps, err := a.store.WaitingSettingProposals(ctx)
	if err != nil {
		appkit.Error("listing setting proposals", err)
		return nil
	}
	var out []settingProposalView
	for _, p := range ps {
		label := p.Label
		if p.Kind == spTarget {
			label += ": " + p.Name
		}
		out = append(out, settingProposalView{ID: p.ID, Kind: p.Kind, Label: label, FromText: p.FromText, ToText: p.ToText, Reason: p.Reason, By: actorLabel(p.ProposedBy)})
	}
	return out
}

func (a *app) decideSettingProposalHandler(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	decision := r.PathValue("decision")
	if err != nil || id <= 0 || (decision != "approve" && decision != "dismiss") {
		http.NotFound(w, r)
		return
	}
	n := "proposal"
	switch res, err := a.decideSettingProposal(r.Context(), ownerActor(), id, decision == "approve"); {
	case err == nil && res == "withdrawn":
		n = "withdrawn"
	case err == nil:
		if decision == "dismiss" {
			n = "dismissed"
		}
	case isUserErr(err):
		var ue *userErr
		errors.As(err, &ue)
		a.renderSettings(w, r, ue.Status, "", ue.Msg)
		return
	default:
		appkit.Error("deciding a setting proposal", err)
		http.Error(w, "That did not work. Try again.", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/settings?n="+n+"#h-proposals", http.StatusSeeOther)
}

// currentOf is the value in force now, worded as the proposal's "from" is.
func (a *app) currentOf(ctx context.Context, p repo.SettingProposal) (string, bool) {
	switch p.Kind {
	case spSetting:
		e, ok := a.findSetting(ctx, p.Name)
		return e.Value, ok
	case spGuard:
		g := guardByKey(p.Name)
		if a.guardOn(ctx, a.store.Q, g.Key) {
			return "on", true
		}
		return "off", true
	case spTarget:
		if slices.Contains(a.blockedTargets(ctx, a.store.Q), p.Name) {
			return "present", true
		}
		return "absent", true
	}
	return "", false
}

// applySettingProposal writes the proposed change through the very functions
// Save uses. It says false when the change cannot be written any more.
func (a *app) applySettingProposal(ctx context.Context, tx *repo.Tx, p repo.SettingProposal, now time.Time) (bool, error) {
	switch p.Kind {
	case spSetting:
		e, ok := a.findSetting(ctx, p.Name)
		if !ok {
			return false, nil
		}
		if e.Def != nil {
			if !validSetting(*e.Def, p.To) {
				return false, nil
			}
			return true, writeSettingChange(ctx, tx, *e.Def, p.From, p.To, now)
		}
		if !validClaudeValue(e.Key, p.To) {
			return false, nil
		}
		return true, writeClaudeChange(ctx, tx, e.Key, e.Name, p.From, p.To, now)
	case spGuard:
		g, ok := guardNamed(p.Name)
		if !ok || !g.Switchable || (p.To != "on" && p.To != "off") {
			return false, nil
		}
		return true, writeGuard(ctx, tx, g.Key, g, p.To, now)
	case spTarget:
		if !targetEntryRE.MatchString(p.Name) {
			return false, nil
		}
		list := a.blockedTargets(ctx, tx.Q)
		if p.To == "present" {
			list = append(slices.Clone(list), p.Name)
		} else {
			list = slices.DeleteFunc(slices.Clone(list), func(s string) bool { return s == p.Name })
		}
		return true, a.writeBlocked(ctx, tx, list, now)
	}
	return false, nil
}

// decideSettingProposal is the one function that applies a proposed change or
// dismisses it. Only the owner, on the signed-in page, can call it. A proposal made
// under a test run is decided and changes nothing real. It answers
// "withdrawn" when the value is no longer the one the proposal started from.
func (a *app) decideSettingProposal(ctx context.Context, x actor, id int64, approve bool) (string, error) {
	if !x.owner {
		return "", a.logRefusal(ctx, x, 0, refuse("Approval only", whyApprovalOnly))
	}
	now := a.now()
	p0, found := a.store.SettingProposalByID(ctx, id)
	if !found {
		return "", &userErr{http.StatusNotFound, "no such proposal"}
	}
	cur, curOK := a.currentOf(ctx, p0)
	result := ""
	err := a.store.Update(ctx, func(tx *repo.Tx) error {
		p, found := tx.SettingProposalByID(ctx, id)
		if !found || p.Status != "proposed" {
			return nil // a second tap, a second device: nothing to write
		}
		record := func(typ, by string) error {
			return tx.InsertEventFor(ctx, "setting_proposals", id, now.Unix(), by, typ, map[string]any{"setting_proposal": id, "kind": p.Kind, "name": p.Label, "from": p.FromText, "to": p.ToText, "proposed_by": p.ProposedBy})
		}
		if !approve {
			if _, err := tx.DecideSettingProposal(ctx, id, "dismissed", x.id(), now.Unix()); err != nil {
				return err
			}
			return record("setting_proposal_dismissed", x.id())
		}
		if p.TestRun == "" {
			applied := false
			if curOK && cur == p.From {
				var err error
				if applied, err = a.applySettingProposal(ctx, tx, p, now); err != nil {
					return err
				}
			}
			if !applied { // the owner changed it himself meanwhile (SPEC 12): withdrawn
				if _, err := tx.DecideSettingProposal(ctx, id, "withdrawn", "system", now.Unix()); err != nil {
					return err
				}
				result = "withdrawn"
				return record("setting_proposal_withdrawn", "system")
			}
		}
		if _, err := tx.DecideSettingProposal(ctx, id, "approved", x.id(), now.Unix()); err != nil {
			return err
		}
		if p.TestRun == "" {
			result = "applied"
		}
		return record("setting_proposal_approved", x.id())
	})
	if err == nil && result == "applied" {
		a.settingsChanged()
		a.wakeReview()
	}
	return result, err
}
