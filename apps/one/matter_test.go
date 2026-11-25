package main

import (
	"net/url"
	"strings"
	"testing"
)

// SPEC 10 (invented data): the Settings section Matter patterns, and mail of
// one matter joining one row.

func TestSpec10SettingsHasMatterPatternsAndAddPattern(t *testing.T) {
	e := newAgentEnv(t)
	page := get(e.h, "/settings").Body.String()
	for _, want := range []string{">Matter patterns</h2>", ">Add pattern</button>", "No patterns yet."} {
		if !strings.Contains(page, want) {
			t.Errorf("the page lacks %q", want)
		}
	}
	if rec := post(e.h, "/settings/matter-patterns", url.Values{"name": {"Invoice"}, "pattern": {`INV-([0-9]+)`}}); rec.Code != 303 {
		t.Fatalf("add: %d %s", rec.Code, rec.Body.String())
	}
	page = get(e.h, "/settings").Body.String()
	if !strings.Contains(page, "Invoice") || !strings.Contains(page, "INV-([0-9]&#43;)") || strings.Contains(page, "No patterns yet.") {
		t.Errorf("the pattern is not listed:\n%s", page)
	}
	for _, bad := range []url.Values{{"pattern": {""}}, {"pattern": {"(["}}, {"pattern": {"x*"}}, {"pattern": {`INV-([0-9]+)`}}} {
		if rec := post(e.h, "/settings/matter-patterns", bad); rec.Code != 400 {
			t.Errorf("%v answered %d, want 400", bad, rec.Code)
		}
	}
	if rec := post(e.h, "/settings/matter-patterns/0/remove", nil); rec.Code != 303 {
		t.Fatalf("remove: %d", rec.Code)
	}
	if page = get(e.h, "/settings").Body.String(); !strings.Contains(page, "No patterns yet.") {
		t.Error("the pattern was not removed")
	}
}

func TestSpec10MailOfOneMatterJoinsOneRowAndOtherMailDoesNot(t *testing.T) {
	e := newEnv(t)
	post(e.h, "/settings/matter-patterns", url.Values{"name": {"Invoice"}, "pattern": {`INV-([0-9]+)`}})
	e.box.add(e.in("100", "Invoice INV-4711 is ready", 4))
	e.box.add(e.in("200", "Reminder: inv-4711 unpaid", 3))
	e.box.add(e.in("300", "Invoice INV-9999 is ready", 2))
	e.box.add(e.in("400", "Invoice is ready", 1))
	e.must(e.importMonth())
	if c := e.counts(); c["items"] != 3 || c["messages"] != 4 {
		t.Fatalf("counts %v: want 3 rows for 4 mails", c)
	}
	if a, b := e.item("100"), e.item("200"); a.ID != b.ID {
		t.Error("the two mails about INV-4711 are not one row")
	}
	if e.item("300").ID == e.item("100").ID || e.item("400").ID == e.item("100").ID {
		t.Error("another matter, or no matter, joined the row")
	}
}

func TestSpec10WithoutPatternsNothingJoins(t *testing.T) {
	e := newEnv(t)
	e.box.add(e.in("100", "Invoice INV-4711 is ready", 4))
	e.box.add(e.in("200", "Reminder INV-4711 unpaid", 3))
	e.must(e.importMonth())
	if c := e.counts(); c["items"] != 2 {
		t.Fatalf("counts %v", c)
	}
}
