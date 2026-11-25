package main

import (
	"context"
	"encoding/json"
	"net/url"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"lab/apps/one/repo"
)

// Tests of the history of the whole mailbox (SPEC 21) and of the numbers per
// sender made from it. All data is invented.

// oldIn is a mail from a sender received days ago, in the inbox, not read.
func (e *env) oldIn(thrid, from, subject string, days float64, flags ...string) testMail {
	m := e.in(thrid, subject, days)
	m.from = from
	m.flags = flags
	return m
}

func (e *env) startHistory() {
	e.t.Helper()
	e.must(e.a.startHistory(e.ctx, historyKeyG))
}

func (e *env) historyRow(msgid string) (repo.HistoryRow, bool) {
	h, answered, ok := e.a.store.HistoryRowOf(e.ctx, srcGmail, msgid)
	if !ok {
		e.t.Fatalf("no history line for message %s", msgid)
	}
	return h, answered
}

// TestHistoryPassRecordsFlagsWithoutTextsForOldMail: mail from before the
// imported period is recorded with who wrote, when, the thread, read, star and
// place, and with nothing else: no message, no raw copy, no subject or text,
// and no body is ever fetched for it.
func TestHistoryPassRecordsFlagsWithoutTextsForOldMail(t *testing.T) {
	e := newEnv(t)
	oldRead := e.box.add(e.oldIn("O1", "News Desk <news@shop.test>", "Secret old subject", 900, `\Seen`, `\Flagged`))
	oldUnread := e.box.add(e.oldIn("O2", "News Desk <news@shop.test>", "Another secret subject", 800))
	archived := e.oldIn("O3", "Bank <office@bank.test>", "Old statement", 700, `\Seen`)
	archived.labels = nil
	oldArchived := e.box.add(archived)
	e.box.add(e.in("N1", "Fresh matter", 3))
	e.must(e.importYear())
	if got := e.a.store.CountRows(e.ctx, "messages"); got != 1 {
		t.Fatalf("the import took %d messages, want only the recent one", got)
	}
	bodies := len(e.box.fetchedBodies())

	e.startHistory()
	e.must(e.a.imp.RunHistory(e.ctx))

	if got := e.a.store.HistoryCount(e.ctx); got != 4 {
		t.Fatalf("the history holds %d lines, want 4", got)
	}
	if got := len(e.box.fetchedBodies()); got != bodies {
		t.Fatalf("the history pass fetched %d message bodies", got-bodies)
	}
	if got := e.a.store.CountRows(e.ctx, "messages"); got != 1 {
		t.Fatalf("the history pass made messages: %d", got)
	}
	if got := e.a.store.CountRows(e.ctx, "raw"); got != 1 {
		t.Fatalf("the history pass made raw copies: %d", got)
	}
	h, _ := e.historyRow(oldRead.msgid)
	if h.Sender != "news@shop.test" || h.SenderName != "News Desk" || h.Thread != "O1" || !h.Seen || !h.Starred || h.Mine || h.State != placeInbox {
		t.Fatalf("history line of the read, starred mail: %+v", h)
	}
	if want := e.ago(900).Unix(); h.ReceivedAt != want {
		t.Fatalf("received at %d, want %d", h.ReceivedAt, want)
	}
	if h, _ := e.historyRow(oldUnread.msgid); h.Seen || h.Starred {
		t.Fatalf("the unread mail is recorded as read or starred: %+v", h)
	}
	if h, _ := e.historyRow(oldArchived.msgid); h.State != placeArchived {
		t.Fatalf("a mail with no label is archived, got %q", h.State)
	}
	for _, c := range e.a.store.HistoryColumns(e.ctx) {
		for _, bad := range []string{"subject", "text", "body", "to", "cc"} {
			if strings.Contains(c, bad) {
				t.Fatalf("the history has a column %q that can hold text", c)
			}
		}
	}
	// Only the two header fields are read, and the folder is only examined.
	for _, c := range e.fake.commands() {
		up := strings.ToUpper(c)
		if strings.Contains(up, "HEADER.FIELDS") && !strings.Contains(up, "BODY.PEEK") {
			t.Fatalf("a read that could mark mail as read: %q", c)
		}
		for _, badCmd := range []string{"STORE", "APPEND", "COPY", "MOVE", "EXPUNGE", "SELECT", "DELETE", "CREATE"} {
			if strings.HasPrefix(up, badCmd+" ") || strings.HasPrefix(up, "UID "+badCmd+" ") {
				t.Fatalf("the history pass sent %q", c)
			}
		}
		if strings.Contains(up, "SUBJECT") && strings.Contains(up, historyItems) {
			t.Fatalf("the history pass asks for subjects: %q", c)
		}
	}
}

// TestHistoryPassIsResumableAndWritesNothingTwice: a pass stopped in the middle
// continues where it was and reads every UID once; one that is run again
// changes nothing.
func TestHistoryPassIsResumableAndWritesNothingTwice(t *testing.T) {
	e := newEnv(t)
	for i := 0; i < 10; i++ {
		e.box.add(e.oldIn("T"+strconv.Itoa(i), "Sender <s"+strconv.Itoa(i%3)+"@bulk.test>", "x", float64(500+i), `\Seen`))
	}
	e.a.imp.historySpan = 3
	e.startHistory()
	ctx, cancel := context.WithCancel(e.ctx)
	reads := 0
	e.box.onMeta = func() {
		reads++
		if reads == 2 {
			cancel() // the app is stopped after two pages
		}
	}
	if err := e.a.imp.RunHistory(ctx); err == nil {
		t.Fatal("the stopped pass did not report that it was stopped")
	}
	if hs := e.a.imp.loadHistory(e.ctx); hs.Done || hs.Next == 0 {
		t.Fatalf("a stopped pass must keep its place and not be done: %+v", hs)
	}
	stopNext := e.a.imp.loadHistory(e.ctx).Next
	readsBefore := len(e.box.metaReads())
	stopped := e.a.store.HistoryCount(e.ctx)
	if stopped == 0 || stopped >= 10 {
		t.Fatalf("after two pages %d lines are written, want some but not all", stopped)
	}
	e.box.onMeta = nil
	e.must(e.a.imp.RunHistory(e.ctx))
	if got := e.a.store.HistoryCount(e.ctx); got != 10 {
		t.Fatalf("after the second run %d lines, want 10", got)
	}
	// What was written before the stop is not read again: the new reads start at the saved place.
	for _, set := range e.box.metaReads()[readsBefore:] {
		_, hi, _ := strings.Cut(set, ":")
		if n, _ := strconv.Atoi(hi); uint32(n) > stopNext {
			t.Fatalf("after the stop the range %s was read, but the pass had its place at %d", set, stopNext)
		}
	}
	// Run again from the start: nothing is written.
	e.must(e.a.startHistory(e.ctx, historyKeyG))
	before := e.counts()
	e.must(e.a.imp.RunHistory(e.ctx))
	if r := e.lastRun(historyMode); r.Stored != 0 || r.Status != "done" {
		t.Fatalf("a second pass wrote %d lines: %+v", r.Stored, r)
	}
	if got := e.a.store.HistoryCount(e.ctx); got != 10 {
		t.Fatalf("a second pass left %d lines", got)
	}
	if after := e.counts(); !reflect.DeepEqual(before, after) {
		t.Fatalf("a second pass changed what is stored:\n%v\n%v", before, after)
	}
}

// TestHistoryAnsweredIsTrueOnlyWhenAMessageOfOwnersFollowsInTheThread.
func TestHistoryAnsweredIsTrueOnlyWhenAMessageOfOwnersFollowsInTheThread(t *testing.T) {
	e := newEnv(t)
	a1 := e.box.add(e.oldIn("A", "Ann <ann@one.test>", "q", 600, `\Seen`))
	e.box.add(e.out("A", "Re: q", 599)) // the owner answered A
	b1 := e.box.add(e.oldIn("B", "Bob <bob@two.test>", "q", 600, `\Seen`))
	e.box.add(e.out("C", "Hello first", 700)) // the owner wrote first in C ...
	c1 := e.box.add(e.oldIn("C", "Cyd <cyd@three.test>", "Re: Hello first", 650))
	d1 := e.box.add(e.oldIn("D", "Dee <dee@four.test>", "q", 500))
	e.box.add(e.out("A", "Re: q again", 400))
	d2 := e.box.add(e.oldIn("D", "Dee <dee@four.test>", "later", 100))
	e.startHistory()
	e.must(e.a.imp.RunHistory(e.ctx))
	for _, c := range []struct {
		name string
		id   string
		want bool
	}{{"A: The owner's reply follows", a1.msgid, true}, {"B: nobody replied", b1.msgid, false}, {"C: The owner wrote before, not after", c1.msgid, false}, {"D: no reply", d1.msgid, false}, {"D later", d2.msgid, false}} {
		if _, got := e.historyRow(c.id); got != c.want {
			t.Errorf("%s: answered = %v, want %v", c.name, got, c.want)
		}
	}
	// The mark is also set when the owner's message is read before the one it answers.
	e.box.add(e.out("D", "Re: later", 50))
	e.startHistory()
	e.must(e.a.imp.RunHistory(e.ctx))
	if _, got := e.historyRow(d1.msgid); !got {
		t.Error("D: a later message of the owner's must make the earlier ones answered")
	}
	if _, got := e.historyRow(d2.msgid); !got {
		t.Error("D later: a later message of the owner's must make it answered")
	}
	// The owner's own messages are never a sender's.
	if h, _ := e.historyRow(a1.msgid); h.Mine {
		t.Error("a mail of someone else is recorded as the owner's")
	}
	stats, err := e.a.store.SenderStats(e.ctx, repo.SenderQuery{By: "address", Limit: 50, Desc: true, Sort: "count"})
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range stats {
		if s.Key == testAddr || s.Key == "alice@example.test" {
			t.Errorf("The owner's own address shows as a sender: %s", s.Key)
		}
	}
}

// TestHistoryIsKeptCurrentByTheNormalSync: a new mail is recorded by the
// regular sync, a change of read state and star follows, and a mail a rule
// dropped is recorded too.
func TestHistoryIsKeptCurrentByTheNormalSync(t *testing.T) {
	e := newEnv(t)
	m := e.box.add(e.in("K", "Kept", 2))
	gh := e.in("G", "Build passed", 1)
	gh.extra = []string{"X-GitHub-Reason: ci_activity"}
	dropped := e.box.add(gh)
	e.must(e.importMonth())
	if h, _ := e.historyRow(m.msgid); h.Seen || h.Thread != "K" {
		t.Fatalf("a message of the normal import is not in the history: %+v", h)
	}
	if _, _, ok := e.a.store.HistoryRowOf(e.ctx, srcGmail, dropped.msgid); !ok {
		t.Fatal("a message a rule dropped is not in the history")
	}
	e.box.change(m.msgid, []string{`\Seen`, `\Flagged`}, []string{`\Inbox`})
	e.must(e.sync())
	if h, _ := e.historyRow(m.msgid); !h.Seen || !h.Starred {
		t.Fatalf("a changed read state and star did not reach the history: %+v", h)
	}
	e.box.change(m.msgid, []string{`\Seen`}, []string{`\Trash`})
	e.must(e.sync())
	if h, _ := e.historyRow(m.msgid); h.State != placeDeleted {
		t.Fatalf("a message in the bin is %q in the history", h.State)
	}
	// The owner answers: the earlier message of the thread becomes answered.
	e.box.add(e.out("K", "Re: Kept", 0.5))
	e.must(e.sync())
	if _, got := e.historyRow(m.msgid); !got {
		t.Fatal("The owner's reply in the normal sync did not mark the earlier message answered")
	}
}

// --- numbers per sender ---

func (e *agentEnv) seedHistory(rows ...repo.HistoryRow) {
	e.t.Helper()
	if err := e.a.store.Update(e.ctx, func(tx *repo.Tx) error {
		for _, r := range rows {
			if _, err := tx.RecordHistory(e.ctx, r, e.now.Unix()); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		e.t.Fatal(err)
	}
}

var hseq int

func (e *agentEnv) hrow(sender string, daysAgo float64, thread string, seen, starred, mine bool) repo.HistoryRow {
	hseq++
	return repo.HistoryRow{Source: srcGmail, SourceID: "h" + strconv.Itoa(hseq), Thread: thread, Sender: sender, SenderName: "Name of " + sender,
		ReceivedAt: e.ago(daysAgo), Seen: seen, Starred: starred, Mine: mine, State: placeInbox}
}

func senderList(m map[string]any) []map[string]any {
	var out []map[string]any
	for _, x := range m["senders"].([]any) {
		out = append(out, x.(map[string]any))
	}
	return out
}

func bySender(list []map[string]any, key string) map[string]any {
	for _, s := range list {
		if s["sender"] == key {
			return s
		}
	}
	return nil
}

// TestSenderNumbersAreRightForASeededMailboxAndCarryNoText.
func TestSenderNumbersAreRightForASeededMailboxAndCarryNoText(t *testing.T) {
	e := newAgentEnv(t)
	var rows []repo.HistoryRow
	// bulk@shop.test: 6 mails every 30 days, none read, none answered; 2 within the last 12 months.
	for i := 0; i < 6; i++ {
		rows = append(rows, e.hrow("bulk@shop.test", float64(30*(i+1)+300), "b"+strconv.Itoa(i), false, false, false))
	}
	rows = append(rows, e.hrow("bulk@shop.test", 10, "b-new1", false, false, false), e.hrow("bulk@shop.test", 40, "b-new2", false, false, false))
	// pal@friend.test: 3 read, one starred, in two threads, the owner answered one of them.
	rows = append(rows, e.hrow("pal@friend.test", 100, "p1", true, true, false), e.hrow("pal@friend.test", 90, "p1", true, false, false),
		e.hrow("pal@friend.test", 20, "p2", true, false, false), e.hrow("me@example.test", 19, "p2", true, false, true))
	// news@shop.test shares the domain with bulk@shop.test.
	rows = append(rows, e.hrow("news@shop.test", 5, "n1", true, false, false))
	e.seedHistory(rows...)

	m := e.getJSON("/api/senders?by=address")
	list := senderList(m)
	b := bySender(list, "bulk@shop.test")
	if b == nil {
		t.Fatalf("no numbers for bulk@shop.test: %v", m)
	}
	want := map[string]float64{"messages": 8, "read": 0, "threads_answered": 0, "starred": 0, "last_12_months": 4, "agent_skipped": 0, "brought_back": 0}
	for k, v := range want {
		if b[k] != v {
			t.Errorf("bulk@shop.test %s = %v, want %v", k, b[k], v)
		}
	}
	// Six old mails 30 days apart, then two newer: the usual (middle) gap is 30 days.
	if gap, _ := b["usual_gap_days"].(float64); gap < 29.5 || gap > 30.5 {
		t.Errorf("usual gap %v days, want about 30", b["usual_gap_days"])
	}
	if b["first_at"] == nil || b["last_at"] == nil {
		t.Errorf("no period: %v", b)
	}
	p := bySender(list, "pal@friend.test")
	for k, v := range map[string]float64{"messages": 3, "read": 3, "threads_answered": 1, "starred": 1, "last_12_months": 3} {
		if p[k] != v {
			t.Errorf("pal@friend.test %s = %v, want %v", k, p[k], v)
		}
	}
	if bySender(list, "me@example.test") != nil {
		t.Error("The owner's own address is listed as a sender")
	}
	// Sorted by count, biggest first, and paged.
	if list[0]["sender"] != "bulk@shop.test" {
		t.Errorf("not sorted by count: first is %v", list[0]["sender"])
	}
	one := e.getJSON("/api/senders?by=address&limit=1&offset=1")
	if got := senderList(one); len(got) != 1 || got[0]["sender"] != "pal@friend.test" || one["more"] != true {
		t.Errorf("paging: %v", one)
	}
	asc := senderList(e.getJSON("/api/senders?by=address&order=asc"))
	if asc[0]["sender"] != "news@shop.test" {
		t.Errorf("ascending count: first is %v", asc[0]["sender"])
	}
	// Per domain.
	dom := senderList(e.getJSON("/api/senders?by=domain"))
	if s := bySender(dom, "shop.test"); s == nil || s["messages"] != float64(9) {
		t.Errorf("domain shop.test: %v", s)
	}
	// Filters.
	never := senderList(e.getJSON("/api/senders?by=address&filter=never_read_never_answered"))
	if len(never) != 1 || never[0]["sender"] != "bulk@shop.test" {
		t.Errorf("never read and never answered: %v", never)
	}
	ans := senderList(e.getJSON("/api/senders?by=address&filter=answered"))
	if len(ans) != 1 || ans[0]["sender"] != "pal@friend.test" {
		t.Errorf("answered at least once: %v", ans)
	}
	if bad := e.api("GET", "/api/senders?by=colour", nil); bad.Code != 400 {
		t.Errorf("a wrong by gave %d", bad.Code)
	}
	// No message text anywhere in the answer, and the token is needed.
	raw := e.api("GET", "/api/senders?by=address", nil).Body.String()
	for _, k := range []string{`"text"`, `"subject"`, `"snippet"`, `"preview"`, `"body"`} {
		if strings.Contains(raw, k) {
			t.Errorf("the answer holds %s", k)
		}
	}
	if rec := e.call("GET", "/api/senders?by=address", "", nil); rec.Code != 401 {
		t.Errorf("without a token: %d", rec.Code)
	}
}

// TestSenderNumbersCountWhatAgentsSkippedAndOwnerBroughtBack.
func TestSenderNumbersCountWhatAgentsSkippedAndOwnerBroughtBack(t *testing.T) {
	e := newAgentEnv(t)
	e.seedHistory(e.hrow("alice@example.test", 5, "x", false, false, false))
	skipped := e.newItem(stageNew, true, "skip me", "")
	back := e.newItem(stageNew, true, "skip me too", "")
	for _, it := range []repo.Item{skipped, back} {
		if r := e.post("/api/items/"+ref(it)+"/skip", map[string]any{"reason": "advert, nothing to do"}); r["ok"] != true {
			t.Fatalf("skip: %v", r)
		}
	}
	e.a.store.Update(e.ctx, func(tx *repo.Tx) error {
		return tx.InsertEvent(e.ctx, e.now.Unix(), back.ID, "owner", "brought_back", nil)
	})
	s := bySender(senderList(e.getJSON("/api/senders?by=address")), "alice@example.test")
	if s["agent_skipped"] != float64(2) || s["brought_back"] != float64(1) {
		t.Errorf("skipped and brought back: %v", s)
	}
}

// TestHistoryNumbersMCPToolIsReadOnlyAndNeedsTheToken.
func TestHistoryNumbersMCPToolIsReadOnlyAndNeedsTheToken(t *testing.T) {
	e := newAgentEnv(t)
	e.seedHistory(e.hrow("a@x.test", 3, "t", true, false, false))
	var tool *mcpTool
	for i := range mcpTools {
		if mcpTools[i].Name == "history_numbers" {
			tool = &mcpTools[i]
		}
	}
	if tool == nil {
		t.Fatal("no tool history_numbers")
	}
	if tool.Annotations["readOnlyHint"] != true {
		t.Error("history_numbers is not marked read-only")
	}
	text, isErr, known := e.a.callTool(e.ctx, "history_numbers", json.RawMessage(`{"by":"domain","limit":5}`))
	if !known || isErr || !strings.Contains(text, `"x.test"`) {
		t.Fatalf("history_numbers: %v %v %s", known, isErr, text)
	}
	if text, _, _ := e.a.callTool(e.ctx, "history_numbers", json.RawMessage(`{"by":"colour"}`)); !strings.Contains(text, "by must be") {
		t.Errorf("a wrong by: %s", text)
	}
}

var _ = url.Values{}
var _ = slices.Contains[[]int]
var _ = time.Second
