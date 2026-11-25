package main

import (
	"net/url"
	"strconv"
	"strings"
	"testing"
)

// TestNoRequestThatWritesIsSentToEitherMailbox runs everything this slice
// adds (the history pass, the normal import, a dropped message imported
// again) against both fake mailboxes and looks at every request that was
// sent: Gmail sees only the reading commands, Office only GET.
func TestNoRequestThatWritesIsSentToEitherMailbox(t *testing.T) {
	e := newEnv(t)
	e.a.async = func(f func()) { f() }
	e.box.add(e.oldIn("O1", "News <news@shop.test>", "Old", 900, `\Seen`))
	gh := e.in("G", "Build passed", 1)
	gh.extra = []string{"X-GitHub-Reason: ci_activity"}
	dropped := e.box.add(gh)
	e.box.add(e.in("N1", "Fresh matter", 3))
	e.must(e.importYear())
	e.startHistory()
	e.must(e.a.imp.RunHistory(e.ctx))
	if sk, ok := e.a.store.SkippedBySource(e.ctx, srcGmail, dropped.msgid); ok {
		post(e.h, "/settings/skipped/"+strconv.FormatInt(sk.ID, 10)+"/again", url.Values{})
	} else {
		t.Fatal("nothing was dropped")
	}
	allowed := []string{"CAPABILITY", "LOGIN", "AUTHENTICATE", "ID", "NOOP", "LOGOUT", "EXAMINE", "LIST", "XLIST", "STATUS", "FETCH", "UID FETCH", "UID SEARCH", "SEARCH", "ENABLE", "STARTTLS"}
	for _, c := range e.fake.commands() {
		up := strings.ToUpper(c)
		ok := false
		for _, a := range allowed {
			if up == a || strings.HasPrefix(up, a+" ") {
				ok = true
			}
		}
		if !ok {
			t.Fatalf("Gmail was sent %q", c)
		}
		if strings.Contains(up, "BODY[") && !strings.Contains(up, "PEEK") {
			t.Fatalf("a read that could mark mail as read: %q", c)
		}
	}

	g := newGraphEnv(t)
	g.a.async = func(f func()) { f() }
	g.box.add("F-inbox", g.in("A", "Old", 900))
	gm := g.in("G", "Build passed", 1)
	gm.extra = []string{"X-GitHub-Reason: ci_activity"}
	gd := g.box.add("F-inbox", gm)
	g.box.add("F-inbox", g.in("N", "Fresh", 3))
	g.must(g.importYear())
	g.startHistory()
	g.must(g.a.gimp.RunHistory(g.ctx))
	if sk, ok := g.a.store.SkippedBySource(g.ctx, srcGraph, gd.id); ok {
		post(g.h, "/settings/skipped/"+strconv.FormatInt(sk.ID, 10)+"/again", url.Values{})
	}
	calls := g.box.callList()
	if len(calls) == 0 {
		t.Fatal("no call was made")
	}
	for _, c := range calls {
		if !strings.HasPrefix(c, "GET ") {
			t.Fatalf("Office was sent %q", c)
		}
	}
}
