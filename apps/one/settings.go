package main

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"lab/apps/one/repo"
	"lab/platform/appkit"
)

// Settings (SPEC 28). Each setting on the page is read by the code that
// uses it (keyInterval, keyFirstDepth, keyReviewDays in importer.go,
// keyQuietDays in closed.go, the two agenda windows in agenda.go); the page only writes them. A change is made
// only here, by the signed-in the owner, validated on the server, and recorded
// as an event. Nothing under /api/ can reach this file: the agents' address
// refuses anything that looks like a setting (agentapi.go), and there is no
// tool for it.
//
// Chat quiet time (SPEC 17) and bridge silence (SPEC 24) are read by chat.go
// and bridgehealth.go.

type settingOpt struct {
	Value string
	Label string
}

type settingDef struct {
	Key     string
	Label   string
	Hint    string
	Default string
	Options []settingOpt
}

func (d settingDef) label(value string) string {
	for _, o := range d.Options {
		if o.Value == value {
			return o.Label
		}
	}
	return value
}

func days(n int, label string) settingOpt { return settingOpt{strconv.Itoa(n), label} }

var settingDefs = []settingDef{
	{Key: keyInterval, Label: "Sync interval", Hint: "How often mail and calendars are read.", Default: "5", Options: []settingOpt{
		{"1", "1 minute"}, {"2", "2 minutes"}, {"5", "5 minutes"}, {"10", "10 minutes"}, {"15", "15 minutes"}, {"30", "30 minutes"}, {"60", "1 hour"}}},
	{Key: keyFirstDepth, Label: "First import depth", Hint: "How far back a mailbox is first read.", Default: "365", Options: []settingOpt{
		days(30, "1 month"), days(90, "3 months"), days(180, "6 months"), days(365, "1 year"), days(730, "2 years")}},
	{Key: keyReviewDays, Label: "Review window", Hint: "Also a thread's window. Older mail is kept, searchable, and arrives as history.", Default: "30", Options: []settingOpt{
		days(7, "1 week"), days(14, "2 weeks"), days(30, "1 month"), days(60, "2 months"), days(90, "3 months")}},
	{Key: keyChatQuiet, Label: "Chat quiet time", Hint: "A chat goes to Claude once nobody has written in it for this long.", Default: "30", Options: []settingOpt{
		{"5", "5 minutes"}, {"10", "10 minutes"}, {"15", "15 minutes"}, {"30", "30 minutes"}, {"60", "1 hour"}, {"120", "2 hours"}}},
	{Key: keyBridgeSilence, Label: "Bridge silence before a notice", Hint: "How long the Mac may stay silent before the strip and one notice say so.", Default: "15", Options: []settingOpt{
		{"5", "5 minutes"}, {"10", "10 minutes"}, {"15", "15 minutes"}, {"30", "30 minutes"}, {"60", "1 hour"}}},
	{Key: keyQuietDays, Label: "Quiet time before closing", Hint: "A Resolved item with no news for this long is closed.", Default: "14", Options: []settingOpt{
		days(3, "3 days"), days(7, "7 days"), days(14, "14 days"), days(30, "30 days"), days(60, "60 days"), days(90, "90 days")}},
	{Key: keyAgendaBack, Label: "Agenda window back", Hint: "How far back the agenda shows entries; older ones stay under Show earlier.", Default: "30", Options: []settingOpt{
		days(7, "1 week"), days(14, "2 weeks"), days(30, "1 month"), days(60, "2 months"), days(90, "3 months"), days(180, "6 months")}},
	{Key: keyAgendaAhead, Label: "Agenda window ahead", Hint: "How far ahead the agenda shows entries.", Default: "30", Options: []settingOpt{
		days(7, "1 week"), days(14, "2 weeks"), days(30, "1 month"), days(60, "2 months"), days(90, "3 months"), days(180, "6 months")}},
	{Key: keyClosedSummary, Label: "Summary of what Claude closed", Hint: "One row to review, with a to-do, while anything Claude closed is not seen.", Default: "on", Options: []settingOpt{
		{"on", "On"}, {"off", "Off"}}},
}

func settingDefByKey(key string) (settingDef, bool) {
	for _, d := range settingDefs {
		if d.Key == key {
			return d, true
		}
	}
	return settingDef{}, false
}

// validSetting says whether a value is one the setting can have.
func validSetting(d settingDef, value string) bool {
	for _, o := range d.Options {
		if o.Value == value {
			return true
		}
	}
	return false
}

// currentSetting is the value in force: the stored one when it is a number
// the code accepts, else the default.
func (a *app) currentSetting(ctx context.Context, d settingDef) string {
	v := a.store.Setting(ctx, d.Key)
	if _, err := strconv.Atoi(d.Default); err != nil {
		if validSetting(d, v) {
			return v
		}
		return d.Default
	}
	if n, err := strconv.Atoi(v); err == nil && n > 0 {
		return strconv.Itoa(n)
	}
	return d.Default
}

type settingView struct {
	Key     string
	Label   string
	Hint    string
	Options []settingOpt
	Current string
}

type guardView struct {
	Key        string
	Name       string
	Why        string
	State      string // On, Off or Always on
	Switchable bool
	IsOn       bool
	Refused    int
	// Blocked is the parameter of Blocked targets: its list, one entry a line.
	Blocked     bool
	BlockedList string
}

type waitingView struct {
	ItemID int64
	Number string
	Text   string
	By     string
	Reason string
}

type settingsView struct {
	Notice   string
	Error    string
	Settings []settingView
	Guards   []guardView
	Waiting  []waitingView
	People   personProposalList    // proposals about people, which belong to no item
	Rules    rulesView             // SPEC 21: Import rules, with the proposals of agents
	TestRuns []testRunView         // SPEC 16: what automated tests left behind; shown only when there is some
	Skip     skipRulesView         // SPEC 31: Skip rules
	Claude   claudeSettingsView    // SPEC 31: the model and the night check
	Matter   matterSection         // SPEC 10: matter patterns and the proposals for them
	Changes  []settingProposalView // SPEC 22: proposals to change a setting, a guard or the Blocked targets
	History  historyReviewView     // SPEC 34: History review
}

var guardWhy = map[string]string{
	"exact_text":        "What is sent is the text you saw when you pressed Approve, byte for byte. Only you can edit it, here on the page.",
	"same_thread":       "A reply goes only into the thread it was proposed on, to the people already on it, never to you or to anyone new.",
	"blocked_targets":   "No reply is proposed on mail from a bank, a payment service, the data box or a sender of sign-in codes.",
	"send_once":         "An approved reply is sent one time, whatever happens to the request. A send in doubt is never repeated.",
	"reason_required":   "An agent that closes something has to say why.",
	"protected_people":  "An agent cannot close what someone marked Always show me wrote.",
	"approval_only":     "Only you, signed in, can approve or dismiss.",
	"approval_required": "An agent's change is only a proposal until you approve it.",
	"actor_by_channel":  "Who is acting is decided by the way in, never by the name a caller gives.",
}

// guardsShown are the guards whose checks exist in the code now.
var guardsShown = []string{"reason_required", "protected_people", "approval_only", "exact_text", "same_thread", "blocked_targets", "send_once", "approval_required", "actor_by_channel"}

func (a *app) settingsViewFor(ctx context.Context) settingsView {
	var v settingsView
	for _, d := range settingDefs {
		cur := a.currentSetting(ctx, d)
		sv := settingView{Key: d.Key, Label: d.Label, Hint: d.Hint, Options: append([]settingOpt(nil), d.Options...), Current: cur}
		if !validSetting(d, cur) {
			sv.Options = append(sv.Options, settingOpt{cur, d.label(cur)})
		}
		v.Settings = append(v.Settings, sv)
	}
	refused := a.store.RefusedSince(ctx, a.now().Add(-7*24*time.Hour).Unix())
	for _, key := range guardsShown {
		g := guardByKey(key)
		gv := guardView{Key: g.Key, Name: g.Name, Why: guardWhy[key], Switchable: g.Switchable, Refused: refused[g.Name]}
		gv.IsOn = a.guardOn(ctx, a.store.Q, key)
		if key == "blocked_targets" {
			gv.Blocked = true
			gv.BlockedList = strings.Join(a.blockedTargets(ctx, a.store.Q), "\n")
		}
		switch {
		case !g.Switchable:
			gv.State = "Always on"
		case gv.IsOn:
			gv.State = "On"
		default:
			gv.State = "Off"
		}
		v.Guards = append(v.Guards, gv)
	}
	if ps, items, err := a.store.ProposalsWaiting(ctx, 100); err == nil {
		for _, p := range ps {
			it, ok := items[p.ItemID]
			if !ok || it.Hidden {
				continue
			}
			v.Waiting = append(v.Waiting, waitingView{ItemID: it.ID, Number: repo.Number(it.Number), Text: oneLine(it.Subject, 120) + ": " + a.proposalLabelDue(p.Action, p.Text, p.Due),
				By: actorLabel(p.By), Reason: p.Reason})
		}
	}
	v.People = personProposalList{List: a.personProposalViews(ctx), Back: "settings"}
	v.Rules = a.rulesViewFor(ctx)
	v.TestRuns = a.testRunViews(ctx)
	v.Skip = a.skipRulesViewFor(ctx)
	v.Claude = a.claudeSettingsFor(ctx)
	v.Matter = a.matterSectionFor(ctx)
	v.Changes = a.settingProposalViews(ctx)
	v.History = a.historyReviewFor(ctx)
	return v
}

func (a *app) settingsPage(w http.ResponseWriter, r *http.Request) {
	a.renderSettings(w, r, http.StatusOK, r.URL.Query().Get("n"), "")
}

func (a *app) renderSettings(w http.ResponseWriter, r *http.Request, code int, notice, errText string) {
	v := a.settingsViewFor(r.Context())
	switch notice {
	case "saved":
		v.Notice = "Saved."
	case "guard":
		v.Notice = "The guard is changed."
	case "blocked":
		v.Notice = "The list of blocked targets is saved."
	case "proposal":
		v.Notice = "You approved this proposal."
	case "dismissed":
		v.Notice = "You dismissed this proposal."
	case "withdrawn":
		v.Notice = "This proposal no longer fits and was withdrawn."
	case "again":
		v.Notice = "The message is being imported again. It shows in the list when it is done."
	case "skipapproved":
		v.Notice = "You approved this skip rule."
	case "skipdismissed":
		v.Notice = "You dismissed this skip rule."
	case "skipon":
		v.Notice = "The skip rule is switched on."
	case "skipoff":
		v.Notice = "The skip rule is switched off."
	case "matteradded":
		v.Notice = "The pattern is added."
	case "patternapproved":
		v.Notice = "You approved this pattern. Mail that arrives from now on joins by it."
	case "patterndismissed":
		v.Notice = "You dismissed this pattern."
	case "matterremoved":
		v.Notice = "The pattern is removed."
	case "prices":
		v.Notice = "The prices are saved."
	case "testremoved":
		v.Notice = "Removed."
	case "againchat":
		v.Notice = "The Mac brings this message the next time it checks in. It shows in the list when it is done."
	}
	v.Error = errText
	a.render(w, "settings.html", code, pageData{EnvText: a.envText, Version: a.version, Settings: v})
}

// saveSettings is the Save button. Every value is checked first; when one is
// not valid nothing is saved. Only what changed is written and recorded.
func (a *app) saveSettings(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 8<<10)
	ctx := r.Context()
	type change struct {
		def      settingDef
		from, to string
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "The form could not be read.", http.StatusBadRequest)
		return
	}
	var changes []change
	for _, d := range settingDefs {
		raw := r.PostForm[d.Key]
		if len(raw) == 0 {
			continue
		}
		val := strings.TrimSpace(raw[0])
		cur := a.currentSetting(ctx, d)
		if val == cur {
			continue
		}
		if !validSetting(d, val) {
			a.renderSettings(w, r, http.StatusBadRequest, "", fmt.Sprintf("%s: that is not a value this setting can have. Nothing was saved.", d.Label))
			return
		}
		changes = append(changes, change{d, cur, val})
	}
	now := a.now()
	err := a.store.Update(ctx, func(tx *repo.Tx) error {
		for _, c := range changes {
			if err := writeSettingChange(ctx, tx, c.def, c.from, c.to, now); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		appkit.Error("saving settings", err)
		http.Error(w, "The settings could not be saved. Try again.", http.StatusInternalServerError)
		return
	}
	if len(changes) > 0 {
		a.settingsChanged() // the timer takes the new interval at once
	}
	http.Redirect(w, r, "/settings?n=saved", http.StatusSeeOther)
}

// writeSettingChange is the one way a value on the Settings page is written
// and recorded: Save and the Approve of a proposal both end here.
func writeSettingChange(ctx context.Context, tx *repo.Tx, d settingDef, from, to string, now time.Time) error {
	if err := tx.SetSettingBy(ctx, d.Key, to, "owner", now); err != nil {
		return err
	}
	return tx.InsertEvent(ctx, now.Unix(), 0, "owner", "setting_changed", map[string]any{
		"setting": d.Key, "label": d.Label, "from": from, "to": to, "from_text": d.label(from), "to_text": d.label(to)})
}

// settingsChanged wakes the timer so a new sync interval counts now.
func (a *app) settingsChanged() {
	select {
	case a.imp.rearm <- struct{}{}:
	default:
	}
}

// switchGuard turns a guard on or off. Only a guard that can be switched off
// is switched; the others are refused. Only this signed-in page does it.
func (a *app) switchGuard(w http.ResponseWriter, r *http.Request) {
	key, state := r.PathValue("key"), r.PathValue("state")
	g := guardByKey(key)
	known := false
	for _, k := range guardsShown {
		known = known || k == key
	}
	if !known || (state != "on" && state != "off") {
		http.NotFound(w, r)
		return
	}
	ctx := r.Context()
	if !g.Switchable {
		a.renderSettings(w, r, http.StatusBadRequest, "", g.Name+" is always on and cannot be switched off.")
		return
	}
	was := a.guardOn(ctx, a.store.Q, key)
	if was == (state == "on") {
		http.Redirect(w, r, "/settings", http.StatusSeeOther)
		return
	}
	now := a.now()
	err := a.store.Update(ctx, func(tx *repo.Tx) error { return writeGuard(ctx, tx, key, g, state, now) })
	if err != nil {
		appkit.Error("switching a guard", err)
		http.Error(w, "That did not work. Try again.", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/settings?n=guard", http.StatusSeeOther)
}

// writeGuard is the one way a guard is switched and recorded: the switch on the
// page and the Approve of a proposal both end here.
func writeGuard(ctx context.Context, tx *repo.Tx, key string, g guardInfo, state string, now time.Time) error {
	val := ""
	if state == "off" {
		val = "off"
	}
	if err := tx.SetSettingBy(ctx, "guard."+key, val, "owner", now); err != nil {
		return err
	}
	return tx.InsertEvent(ctx, now.Unix(), 0, "owner", "setting_changed", map[string]any{"guard": g.Name, "to": state})
}

// writeBlocked is the one way the Blocked targets list is written and
// recorded: Save and the Approve of a proposal both end here.
func (a *app) writeBlocked(ctx context.Context, tx *repo.Tx, list []string, now time.Time) error {
	stored := strings.Join(list, "\n")
	if stored == "" {
		stored = "-"
	}
	before := a.blockedTargets(ctx, tx.Q)
	if err := tx.SetSettingBy(ctx, keyBlockedList, stored, "owner", now); err != nil {
		return err
	}
	return tx.InsertEvent(ctx, now.Unix(), 0, "owner", "setting_changed", map[string]any{"guard": "Blocked targets", "list_before": len(before), "list_after": len(list)})
}

// saveBlockedTargets saves the list of Blocked targets: one sender address or
// domain a line. Only the signed-in page does it; an entry that is neither an
// address nor a domain stops the whole save.
func (a *app) saveBlockedTargets(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "The form could not be read.", http.StatusBadRequest)
		return
	}
	list := parseTargetList(r.PostForm.Get("list"))
	for _, e := range list {
		if !targetEntryRE.MatchString(e) {
			a.renderSettings(w, r, http.StatusBadRequest, "", fmt.Sprintf("%q is not an address or a domain. Nothing was saved.", oneLine(e, 60)))
			return
		}
	}
	ctx := r.Context()
	now := a.now()
	err := a.store.Update(ctx, func(tx *repo.Tx) error { return a.writeBlocked(ctx, tx, list, now) })
	if err != nil {
		appkit.Error("saving the blocked targets", err)
		http.Error(w, "That did not work. Try again.", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/settings?n=blocked#h-guards", http.StatusSeeOther)
}
