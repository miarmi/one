package main

import (
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"lab/apps/one/repo"
)

// Tests of the Import rules section (SPEC 21) and of the rule proposals of
// agents. All data is invented.

func (e *agentEnv) settingsPage() string {
	e.t.Helper()
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, httptest.NewRequest("GET", "/settings", nil))
	if rec.Code != 200 {
		e.t.Fatalf("/settings: %d", rec.Code)
	}
	return rec.Body.String()
}

func (e *agentEnv) form(path string, f url.Values) *httptest.ResponseRecorder {
	return e.ajaxPost(path, f)
}

func (e *agentEnv) enabledRules(source string) []repo.Rule {
	rs, err := e.a.store.EnabledRules(e.ctx, source)
	if err != nil {
		e.t.Fatal(err)
	}
	return rs
}

func (e *agentEnv) proposeBulk(sender string) map[string]any {
	return e.post("/api/rule-proposals", map[string]any{"field": "sender", "value": sender,
		"reason": "214 mails in three years, none opened, none answered", "agent": "Claude"})
}

// TestRuleProposalShowsWithTheExactWordingAndAppliesOnlyAfterApproveOnThePage.
func TestRuleProposalShowsWithTheExactWordingAndAppliesOnlyAfterApproveOnThePage(t *testing.T) {
	e := newAgentEnv(t)
	e.a.imp.seedRules(e.ctx)
	var rows []repo.HistoryRow
	for i := 0; i < 5; i++ {
		rows = append(rows, e.hrow("bulk@shop.test", float64(100+i), "b"+strconv.Itoa(i), false, false, false))
	}
	e.seedHistory(rows...)
	startRules := len(e.enabledRules(srcGmail))

	res := e.proposeBulk("Bulk@Shop.test")
	if res["ok"] != true || res["waits_for_owner"] != true || res["rule_proposal_id"] == nil {
		t.Fatalf("proposal: %v", res)
	}
	id := int64(res["rule_proposal_id"].(float64))
	if nums, _ := res["history_numbers"].(map[string]any); nums["messages"] != float64(5) || nums["threads_answered"] != float64(0) {
		t.Errorf("the numbers the server found: %v", res["history_numbers"])
	}
	// It applies to nothing yet.
	if got := len(e.enabledRules(srcGmail)); got != startRules {
		t.Fatalf("a proposed rule applies: %d rules on, were %d", got, startRules)
	}
	page := e.settingsPage()
	for _, want := range []string{"Import rules", "Proposal", "Approve", "Dismiss", "Add rule", "Test rule", "GitHub pipeline mails", "Muted group chats",
		"214 mails in three years, none opened, none answered", "bulk@shop.test", "Dropped (0)"} {
		if !strings.Contains(page, want) {
			t.Errorf("the settings page does not say %q", want)
		}
	}
	if !strings.Contains(page, "Enabled") {
		t.Error("no switch Enabled")
	}
	// The strip on the list page counts it.
	state := e.getJSON2("/list/state")
	if !strings.Contains(state, "1 to approve, 1 about rules") {
		t.Errorf("the strip on the list page: %s", state)
	}
	// Approve on the page.
	rec := e.form("/settings/rules/"+strconv.FormatInt(id, 10)+"/decision/approve", url.Values{})
	if rec.Code != 303 {
		t.Fatalf("approve: %d %s", rec.Code, rec.Body.String())
	}
	on := e.enabledRules(srcGmail)
	if len(on) != startRules+1 || on[len(on)-1].Value != "bulk@shop.test" || on[len(on)-1].Field != "sender" {
		t.Fatalf("after Approve the rules on are %+v", on)
	}
	// A second Approve changes nothing and writes nothing.
	n := len(e.ruleEventsOf("rule_approved"))
	e.form("/settings/rules/"+strconv.FormatInt(id, 10)+"/decision/approve", url.Values{})
	if got := len(e.ruleEventsOf("rule_approved")); got != n || n != 1 {
		t.Errorf("rule_approved events: %d then %d", n, got)
	}
	if strings.Contains(e.settingsPage(), "Proposal") && strings.Contains(e.settingsPage(), "Dismiss") {
		t.Error("an approved rule still shows as a proposal")
	}
	// Dismiss is recorded and the rule never applies.
	res2 := e.proposeBulk("other@shop.test")
	id2 := int64(res2["rule_proposal_id"].(float64))
	e.form("/settings/rules/"+strconv.FormatInt(id2, 10)+"/decision/dismiss", url.Values{})
	if r, _ := e.a.store.RuleByID(e.ctx, id2); r.Status != "dismissed" || r.Enabled {
		t.Errorf("a dismissed proposal: %+v", r)
	}
	if got := len(e.enabledRules(srcGmail)); got != startRules+1 {
		t.Errorf("a dismissed proposal applies: %d", got)
	}
	if len(e.ruleEventsOf("rule_dismissed")) != 1 {
		t.Error("the dismissal is not recorded")
	}
	// The same rule twice answers the one that waits.
	res3 := e.proposeBulk("third@shop.test")
	res4 := e.proposeBulk("third@shop.test")
	if res3["rule_proposal_id"] != res4["rule_proposal_id"] || res4["unchanged"] != true {
		t.Errorf("a second identical proposal: %v %v", res3, res4)
	}
}

func (e *agentEnv) getJSON2(path string) string {
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
	return rec.Body.String()
}

func (e *agentEnv) ruleEventsOf(typ string) []repo.Event {
	evs, err := e.a.store.RuleEvents(e.ctx, typ)
	if err != nil {
		e.t.Fatal(err)
	}
	return evs
}

// TestARuleCannotBeApprovedOrEnabledUnderAPI: nothing under /api/ approves,
// dismisses, enables, changes or tests a rule, and no tool does.
func TestARuleCannotBeApprovedOrEnabledUnderAPI(t *testing.T) {
	e := newAgentEnv(t)
	e.seedHistory(e.hrow("bulk@shop.test", 50, "b1", false, false, false))
	id := int64(e.proposeBulk("bulk@shop.test")["rule_proposal_id"].(float64))
	idS := strconv.FormatInt(id, 10)
	for _, c := range []struct{ method, path string }{
		{"POST", "/api/rule-proposals/" + idS + "/approve"}, {"POST", "/api/rules/" + idS + "/approve"}, {"POST", "/api/rules/" + idS + "/enable"},
		{"POST", "/api/rules/" + idS + "/enabled/on"}, {"PUT", "/api/rules/" + idS}, {"PATCH", "/api/rules/" + idS}, {"DELETE", "/api/rules/" + idS},
		{"POST", "/api/rules"}, {"POST", "/api/settings/rules/" + idS + "/decision/approve"}, {"POST", "/api/rule-proposals/" + idS}, {"GET", "/api/rules"},
	} {
		rec := e.api(c.method, c.path, map[string]any{"enabled": true, "status": "active", "approve": true})
		if rec.Code != 403 && rec.Code != 404 && rec.Code != 405 {
			t.Errorf("%s %s answered %d", c.method, c.path, rec.Code)
		}
		if r, _ := e.a.store.RuleByID(e.ctx, id); r.Status != "proposed" || r.Enabled {
			t.Fatalf("%s %s changed the proposal: %+v", c.method, c.path, r)
		}
	}
	// A body that says enabled, status or actor does not turn a proposal on or into the owner.
	e.post("/api/rule-proposals", map[string]any{"field": "sender", "value": "x@shop.test", "reason": "9 mails", "enabled": true, "status": "active", "created_by": "owner", "agent": "the owner"})
	if r, ok := e.a.store.SameRule(e.ctx, "any", "sender", "equals", "x@shop.test"); !ok || r.Status != "proposed" || r.Enabled || r.ProposedBy == "owner" {
		t.Errorf("a proposal with a bossy body: %+v", r)
	}
	// In code, an agent that reaches the decision is refused by the guard.
	if err := e.a.decideRule(e.ctx, agentActor("Claude"), id, true); err == nil || !isRefusal(err.Error(), "Approval only") {
		t.Errorf("decideRule as an agent: %v", err)
	}
	if r, _ := e.a.store.RuleByID(e.ctx, id); r.Status != "proposed" {
		t.Error("an agent approved a rule in code")
	}
	// No tool but the proposal touches rules.
	for _, tl := range mcpTools {
		n := strings.ToLower(tl.Name)
		if strings.Contains(n, "rule") && tl.Name != "propose_rule" && tl.Name != "propose_skip_rule" {
			t.Errorf("tool %s", tl.Name)
		}
		if tl.Name == "propose_rule" && tl.Annotations["readOnlyHint"] != false {
			t.Error("propose_rule must say that it changes something")
		}
	}
}

// TestARuleProposalIsRefusedForASenderOwnerHasAnswered: a guard in code.
func TestARuleProposalIsRefusedForASenderOwnerHasAnswered(t *testing.T) {
	e := newAgentEnv(t)
	// In the history: The owner answered a thread of ann@club.test; nobody answered bob@club.test.
	e.seedHistory(e.hrow("ann@club.test", 40, "t1", true, false, false), e.hrow("me@example.test", 39, "t1", true, false, true),
		e.hrow("bob@club.test", 40, "t2", false, false, false), e.hrow("spam@loud.test", 30, "t3", false, false, false))
	for _, c := range []struct{ field, value string }{{"sender", "ann@club.test"}, {"sender_domain", "club.test"}} {
		res := e.post("/api/rule-proposals", map[string]any{"field": c.field, "value": c.value, "reason": "12 mails, none read"})
		msg, _ := res["error"].(string)
		if res["_status"] != float64(409) || !strings.Contains(msg, "answered 1 thread") || !strings.Contains(msg, c.value) {
			t.Errorf("%s %s: %v", c.field, c.value, res)
		}
	}
	if _, ok := e.a.store.SameRule(e.ctx, "any", "sender", "equals", "ann@club.test"); ok {
		t.Error("a refused rule was stored")
	}
	if len(e.ruleEventsOf("rule_refused")) != 2 {
		t.Error("the refusals are not in the record")
	}
	// A sender nobody answered passes.
	if res := e.post("/api/rule-proposals", map[string]any{"field": "sender", "value": "bob@club.test", "reason": "1 mail, none read"}); res["ok"] != true {
		t.Errorf("bob@club.test: %v", res)
	}
	if res := e.post("/api/rule-proposals", map[string]any{"field": "sender_domain", "value": "loud.test", "reason": "1 mail, none read"}); res["ok"] != true {
		t.Errorf("loud.test: %v", res)
	}
	// An answer found only in the messages ONE holds in full counts too.
	m := e.mail("Question", 20)
	m.sender = "carl@news.test"
	mine := e.mail("Re: Question", 19)
	mine.mine, mine.sender = true, "me@example.test"
	e.seed(stageResolved, m, mine)
	if res := e.post("/api/rule-proposals", map[string]any{"field": "sender", "value": "carl@news.test", "reason": "3 mails"}); res["_status"] != float64(409) {
		t.Errorf("an answered thread of the imported year: %v", res)
	}
	// A protected person never gets a rule.
	e.a.store.Update(e.ctx, func(tx *repo.Tx) error {
		if _, err := tx.EnsurePerson(e.ctx, "mail", "dad@family.test", "Dad", e.now.Unix()); err != nil {
			return err
		}
		return tx.MarkProtected(e.ctx, "mail", "dad@family.test", true)
	})
	if res := e.post("/api/rule-proposals", map[string]any{"field": "sender", "value": "dad@family.test", "reason": "3 mails"}); res["_status"] != float64(403) || !strings.Contains(res["error"].(string), "Protected people") {
		t.Errorf("a protected person: %v", res)
	}
	// What a rule may look at, and what the reason must hold.
	for _, bad := range []map[string]any{
		{"field": "subject", "value": "sale", "reason": "5 mails"},
		{"field": "sender", "value": "nobody", "reason": "5 mails"},
		{"field": "sender_domain", "value": "x@y.test", "reason": "5 mails"},
		{"field": "sender", "value": "q@w.test", "reason": "none opened"},
		{"field": "sender", "value": "q@w.test", "op": "contains", "reason": "5 mails"},
	} {
		if res := e.post("/api/rule-proposals", bad); res["_status"] != float64(400) {
			t.Errorf("%v was accepted: %v", bad, res)
		}
	}
	if rec := e.call("POST", "/api/rule-proposals", "", map[string]any{"field": "sender", "value": "q@w.test", "reason": "5"}); rec.Code != 401 {
		t.Errorf("without a token: %d", rec.Code)
	}
}

// TestTestRuleListsWhatARuleWouldHaveDropped.
func TestTestRuleListsWhatARuleWouldHaveDropped(t *testing.T) {
	e := newAgentEnv(t)
	hit := func(sub string, days float64) seedMsg {
		m := e.mail(sub, days)
		m.sender = "ads@loud.test"
		return m
	}
	e.seed(stageResolved, hit("Big sale today", 3))
	e.seed(stageResolved, hit("Another sale", 10))
	e.seed(stageResolved, hit("Ancient sale", 45)) // older than 30 days
	other := e.mail("Your invoice", 2)
	e.seed(stageResolved, other)
	mine := e.mail("From me", 1)
	mine.sender, mine.mine = "ads@loud.test", true
	e.seed(stageResolved, mine)

	rec := e.form("/settings/rules/test", url.Values{"name": {"Loud"}, "source": {"any"}, "field": {"sender_domain"}, "op": {"equals"}, "value": {"@Loud.test"}})
	body := rec.Body.String()
	if rec.Code != 200 || !strings.Contains(body, "would have dropped 2 messages") {
		t.Fatalf("test of a domain rule: %d %s", rec.Code, textOf(body))
	}
	for _, want := range []string{"Big sale today", "Another sale", "ads@loud.test"} {
		if !strings.Contains(body, want) {
			t.Errorf("the test does not list %q", want)
		}
	}
	for _, not := range []string{"Ancient sale", "Your invoice", "From me"} {
		if strings.Contains(body, not) {
			t.Errorf("the test lists %q", not)
		}
	}
	// Nothing is added by testing.
	if n := e.a.store.CountRows(e.ctx, "rules"); n != 0 {
		t.Errorf("testing added %d rules", n)
	}
	// A subject rule, contains.
	rec = e.form("/settings/rules/test", url.Values{"source": {"gmail"}, "field": {"subject"}, "op": {"contains"}, "value": {"invoice"}})
	if !strings.Contains(rec.Body.String(), "would have dropped 1 message.") {
		t.Errorf("subject test: %s", textOf(rec.Body.String()))
	}
	// A rule that is too wide is refused.
	rec = e.form("/settings/rules/test", url.Values{"source": {"any"}, "field": {"subject"}, "op": {"contains"}, "value": {"a"}})
	if rec.Code != 400 || !strings.Contains(rec.Body.String(), "at least 3 characters") {
		t.Errorf("a too wide rule: %d", rec.Code)
	}
}

// TestOwnerAddsEditsAndSwitchesARule: Add rule, the switch Enabled, Dropped (n).
func TestOwnerAddsEditsAndSwitchesARule(t *testing.T) {
	e := newAgentEnv(t)
	rec := e.form("/settings/rules", url.Values{"name": {"Loud shop"}, "source": {"gmail"}, "field": {"sender"}, "op": {"equals"}, "value": {"Ads@Loud.test"}})
	if rec.Code != 303 {
		t.Fatalf("add: %d %s", rec.Code, textOf(rec.Body.String()))
	}
	rs := e.enabledRules(srcGmail)
	if len(rs) != 1 || rs[0].Value != "ads@loud.test" || rs[0].CreatedBy != "owner" || rs[0].Name != "Loud shop" {
		t.Fatalf("rules: %+v", rs)
	}
	if rec := e.form("/settings/rules", url.Values{"source": {"gmail"}, "field": {"sender"}, "op": {"equals"}, "value": {"ads@loud.test"}}); rec.Code != 400 {
		t.Errorf("the same rule twice: %d", rec.Code)
	}
	id := strconv.FormatInt(rs[0].ID, 10)
	e.form("/settings/rules/"+id+"/enabled/off", url.Values{})
	if len(e.enabledRules(srcGmail)) != 0 {
		t.Error("a rule that is off still applies")
	}
	if !strings.Contains(e.settingsPage(), "Off") {
		t.Error("the page does not show the rule as off")
	}
	e.form("/settings/rules/"+id+"/enabled/on", url.Values{})
	if len(e.enabledRules(srcGmail)) != 1 {
		t.Error("a rule that is switched on again does not apply")
	}
	e.form("/settings/rules/"+id, url.Values{"name": {"Loud shop"}, "source": {"any"}, "field": {"sender_domain"}, "op": {"equals"}, "value": {"loud.test"}})
	rs = e.enabledRules(srcGraph)
	if len(rs) != 1 || rs[0].Field != "sender_domain" || rs[0].Value != "loud.test" {
		t.Errorf("after the edit: %+v", rs)
	}
	// Only the owner's page: the paths are not under /api/.
	for _, ev := range []string{"rule_added", "rule_switched", "rule_changed"} {
		if len(e.ruleEventsOf(ev)) == 0 {
			t.Errorf("no %s event", ev)
		}
	}
}

func textOf(html string) string {
	return strings.Join(strings.Fields(strings.NewReplacer("<", " <", ">", "> ").Replace(html)), " ")
}

// TestDroppedListAndImportAgain: each rule shows Dropped (n), what it dropped
// and a button that imports one message again (Gmail and Office).
func TestDroppedListAndImportAgain(t *testing.T) {
	e := newEnv(t)
	e.a.async = func(f func()) { f() }
	gh := e.in("G", "Build passed", 1)
	gh.extra = []string{"X-GitHub-Reason: ci_activity"}
	dropped := e.box.add(gh)
	e.must(e.importMonth())
	if e.a.store.CountMessages(e.ctx, srcGmail) != 0 {
		t.Fatal("the rule did not drop the message")
	}
	page := get(e.h, "/settings").Body.String()
	for _, want := range []string{"Dropped (1)", "Build passed", "alice@example.test", "Import again", "GitHub pipeline mails"} {
		if !strings.Contains(page, want) {
			t.Errorf("the page does not say %q", want)
		}
	}
	sk, ok := e.a.store.SkippedBySource(e.ctx, srcGmail, dropped.msgid)
	if !ok {
		t.Fatal("no skipped line")
	}
	rec := post(e.h, "/settings/skipped/"+strconv.FormatInt(sk.ID, 10)+"/again", url.Values{})
	if rec.Code != 303 {
		t.Fatalf("import again: %d", rec.Code)
	}
	if e.a.store.CountMessages(e.ctx, srcGmail) != 1 {
		t.Fatal("the message was not imported again")
	}
	if after, _ := e.a.store.SkippedByID(e.ctx, sk.ID); !after.Reimported {
		t.Error("the dropped line does not say it was imported again")
	}
	if !strings.Contains(get(e.h, "/settings").Body.String(), "Imported again") {
		t.Error("the page does not say it was imported again")
	}
	for _, c := range e.fake.commands() {
		up := strings.ToUpper(c)
		for _, bad := range []string{"STORE", "APPEND", "COPY", "MOVE", "EXPUNGE", "SELECT", "DELETE", "CREATE"} {
			if strings.HasPrefix(up, bad+" ") || strings.HasPrefix(up, "UID "+bad+" ") {
				t.Fatalf("sent %q", c)
			}
		}
	}
}

func TestGraphDroppedMessageImportsAgain(t *testing.T) {
	e := newGraphEnv(t)
	e.a.async = func(f func()) { f() }
	gh := e.in("G", "Build passed", 1)
	gh.extra = []string{"X-GitHub-Reason: ci_activity"}
	m := e.box.add("F-inbox", gh)
	e.must(e.importMonth())
	if e.a.store.CountMessages(e.ctx, srcGraph) != 0 {
		t.Fatal("the rule did not drop the message")
	}
	sk, ok := e.a.store.SkippedBySource(e.ctx, srcGraph, m.id)
	if !ok {
		t.Fatal("no skipped line")
	}
	rec := post(e.h, "/settings/skipped/"+strconv.FormatInt(sk.ID, 10)+"/again", url.Values{})
	if rec.Code != 303 {
		t.Fatalf("import again: %d", rec.Code)
	}
	if e.a.store.CountMessages(e.ctx, srcGraph) != 1 {
		t.Fatal("the message was not imported again")
	}
	if after, _ := e.a.store.SkippedByID(e.ctx, sk.ID); !after.Reimported {
		t.Error("not marked as imported again")
	}
	for _, c := range e.box.callList() {
		if !strings.HasPrefix(c, "GET ") {
			t.Fatalf("a call that is not a read: %s", c)
		}
	}
}
