package main

import (
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"

	"lab/apps/one/repo"
)

// Tests of SPEC 33: a meeting can have a thread. They read a fake iCloud
// calendar through the app's own read, so the thread follows an entry the way
// it does in use.

type meetEnv struct {
	*calEnv
	cal       *davCal
	connected bool
}

func newMeetEnv(t *testing.T) *meetEnv {
	t.Helper()
	e := &meetEnv{calEnv: newCalEnv(t)}
	e.cal = e.dav.calendar("work", "Work")
	return e
}

// put writes one event into the fake calendar and reads the calendar.
func (e *meetEnv) put(name, ics string) {
	e.t.Helper()
	e.dav.put(e.cal, name, ics)
	if !e.connected {
		e.connected = true
		e.connectICloud()
		return
	}
	e.readICloud()
}

func (e *meetEnv) entry(title string) repo.Agenda {
	e.t.Helper()
	rows := e.byTitle(title)
	if len(rows) == 0 {
		e.t.Fatalf("no entry %q", title)
	}
	return rows[0]
}

func (e *meetEnv) threads() []repo.Item {
	e.t.Helper()
	var out []repo.Item
	m, err := e.a.store.MeetingThreads(e.ctx)
	if err != nil {
		e.t.Fatal(err)
	}
	for _, v := range m {
		it, _ := e.a.store.ItemByID(e.ctx, v[0])
		out = append(out, it)
	}
	return out
}

func (e *meetEnv) open(entry repo.Agenda) repo.Item {
	e.t.Helper()
	rec := post(e.h, fmt.Sprintf("/agenda/%d/thread", entry.ID), url.Values{})
	if rec.Code != 303 || !strings.HasPrefix(rec.Header().Get("Location"), "/items/") {
		e.t.Fatalf("Open thread: %d %s", rec.Code, rec.Header().Get("Location"))
	}
	var id int64
	fmt.Sscanf(rec.Header().Get("Location"), "/items/%d", &id)
	return e.item(id)
}

func (e *meetEnv) notices(id int64) []string {
	e.t.Helper()
	var out []string
	for _, n := range e.events(id, meetingNoticeEv) {
		out = append(out, strings.TrimSpace(strings.Split(strings.Split(n.Data, `"text":"`)[1], `"`)[0]))
	}
	return out
}

func (e *meetEnv) threadPage(id int64) string {
	return get(e.h, fmt.Sprintf("/items/%d", id)).Body.String()
}

func meeting(uid, title, start, end string, extra ...string) string {
	return one(uid, title, start, end, extra...)
}

func TestMeetingNoEntryGetsAThreadByItselfAndTheButtonSaysOpenThread(t *testing.T) {
	e := newMeetEnv(t)
	e.put("a.ics", meeting("m1", "Budget review", "20261125T090000Z", "20261125T100000Z", "LOCATION:Room 2"))
	e.put("b.ics", meeting("m2", "Dentist", "20261126T090000Z", "20261126T100000Z"))
	if n := len(e.threads()); n != 0 || e.a.store.CountRows(e.ctx, "items") != 0 {
		t.Fatalf("a thread was made by itself: %d", n)
	}
	page := e.agenda()
	if strings.Count(page, ">Open thread<") != 2 || strings.Contains(page, ">Thread<") {
		t.Errorf("the agenda should offer Open thread on both entries:\n%s", page)
	}
}

func TestMeetingOwnerOpensAThreadFromAnEntryAndItIsARowInProgressWithItsHeader(t *testing.T) {
	e := newMeetEnv(t)
	e.put("a.ics", meeting("m1", "Budget review", "20261125T090000Z", "20261125T100000Z", "LOCATION:Room 2"))
	ent := e.entry("Budget review")
	it := e.open(ent)
	if it.Kind != meetingKind || it.Stage != stageInProgress || it.Number == 0 || it.Hidden || it.NeedsClaude {
		t.Fatalf("thread: %+v", it)
	}
	// Pressing again, or on another device, finds the same thread.
	if again := e.open(ent); again.ID != it.ID || e.a.store.CountRows(e.ctx, "items") != 1 {
		t.Fatalf("a second press made another: %+v", again)
	}
	page := e.threadPage(it.ID)
	for _, want := range []string{"Budget review", "Room 2", "Work", "Wed 25 Nov", repo.Number(it.Number)} {
		if !strings.Contains(page, want) {
			t.Errorf("the thread lacks %q:\n%s", want, page)
		}
	}
	// The entry's button now reads Thread and opens the row.
	ag := e.agenda()
	if strings.Contains(ag, ">Open thread<") || !strings.Contains(ag, fmt.Sprintf(`href="/items/%d"`, it.ID)) || !strings.Contains(ag, ">Thread<") {
		t.Errorf("the agenda after opening:\n%s", ag)
	}
	// It works as any row: a note, a to-do, the list, search, the connector.
	if rec := e.ajaxPost(fmt.Sprintf("/items/%d/notes", it.ID), url.Values{"text": {"Bring the budget papers"}}); rec.Code != 200 {
		t.Fatalf("note: %d %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(e.threadPage(it.ID), "Bring the budget papers") {
		t.Error("the note is not in the thread")
	}
	if rec := e.ajaxPost(fmt.Sprintf("/items/%d/todo", it.ID), url.Values{}); rec.Code != 200 {
		t.Fatalf("to-do: %d", rec.Code)
	}
	if list := get(e.h, "/list/in_progress").Body.String(); !strings.Contains(list, "Budget review") || !strings.Contains(list, "Calendar") {
		t.Errorf("the list lacks the thread:\n%s", list)
	}
	if hits := get(e.h, "/search?q=budget").Body.String(); !strings.Contains(hits, "Event") || !strings.Contains(hits, "Budget review") {
		t.Errorf("search does not find it:\n%s", hits)
	}
	got := e.getJSON("/api/items/" + repo.Number(it.Number))
	item := got["item"].(map[string]any)
	m, _ := item["meeting"].(map[string]any)
	if item["kind"] != "meeting" || m["title"] != "Budget review" || m["place"] != "Room 2" || m["entry_id"] != float64(ent.ID) {
		t.Errorf("get_item: %v", item)
	}
	list := e.getJSON("/api/stage-items?stage=in_progress")
	if !strings.Contains(fmt.Sprint(list), "meeting") {
		t.Errorf("list_items: %v", list)
	}
	// The phone lists it too.
	if rows, err := e.a.store.ListItems(repo.WithPhone(e.ctx), stageInProgress, 10, 0); err != nil || len(rows) != 1 {
		t.Errorf("phone list: %v %v", rows, err)
	}
	// Resolving the thread does not touch the entry, which stays in the agenda.
	before := e.entry("Budget review")
	e.ajaxPost(fmt.Sprintf("/items/%d/resolve", it.ID), url.Values{})
	if e.item(it.ID).Stage != stageResolved {
		t.Fatal("not resolved")
	}
	after := e.entry("Budget review")
	if after.Version != before.Version || after.State != "open" || !strings.Contains(e.agenda(), "Budget review") {
		t.Errorf("the entry changed with the thread: %+v -> %+v", before, after)
	}
}

func TestMeetingFollowsTheEntryWithNoticesAndMovesTheRowAsANewMessageDoes(t *testing.T) {
	e := newMeetEnv(t)
	e.put("a.ics", meeting("m1", "Budget review", "20261125T090000Z", "20261125T100000Z", "LOCATION:Room 2"))
	it := e.open(e.entry("Budget review"))
	e.ajaxPost(fmt.Sprintf("/items/%d/resolve", it.ID), url.Values{})
	base := e.counts()

	// A read that changes nothing writes nothing.
	e.readICloud()
	if e.counts() != base || len(e.notices(it.ID)) != 0 {
		t.Fatal("an unchanged read wrote something")
	}
	// A read that changes only the guests' answers or the note leaves the thread alone.
	e.put("a.ics", meeting("m1", "Budget review", "20261125T090000Z", "20261125T100000Z", "LOCATION:Room 2", "DESCRIPTION:Bring papers"))
	if len(e.notices(it.ID)) != 0 || e.item(it.ID).Stage != stageResolved {
		t.Fatal("a change the thread does not show moved it")
	}

	// A moved time.
	e.now = e.now.Add(3600e9)
	e.put("a.ics", meeting("m1", "Budget review", "20261126T130000Z", "20261126T140000Z", "LOCATION:Room 2", "DESCRIPTION:Bring papers"))
	got := e.notices(it.ID)
	if len(got) != 1 || got[0] != "Moved to Thu 26 Nov 14:00." {
		t.Fatalf("notices: %v", got)
	}
	cur := e.item(it.ID)
	if cur.Stage != stageNew || !cur.NeedsClaude || cur.MovedAt != e.now.Unix() {
		t.Errorf("the row did not move as a new message moves it: %+v", cur)
	}
	if page := e.threadPage(it.ID); !strings.Contains(page, got[0]) {
		t.Errorf("the thread does not show the notice:\n%s", page)
	}
	// A moved place, then a cancellation.
	e.put("a.ics", meeting("m1", "Budget review", "20261126T130000Z", "20261126T140000Z", "LOCATION:Room 9", "DESCRIPTION:Bring papers"))
	e.put("a.ics", meeting("m1", "Budget review", "20261126T130000Z", "20261126T140000Z", "LOCATION:Room 9", "STATUS:CANCELLED"))
	got = e.notices(it.ID)
	if len(got) != 3 || got[1] != "Place changed to Room 9." || got[2] != "The entry was cancelled." {
		t.Fatalf("notices: %v", got)
	}
	// Reading the cancelled entry again says nothing more.
	e.readICloud()
	if len(e.notices(it.ID)) != 3 {
		t.Error("a repeated read wrote another notice")
	}
	// The entry is still in the agenda, marked as before.
	if ag := e.agenda(); !strings.Contains(ag, "Budget review") || !strings.Contains(ag, "Cancelled") {
		t.Errorf("the agenda:\n%s", ag)
	}
	// The agent's thread has the notices too.
	if th := fmt.Sprint(e.getJSON("/api/items/" + repo.Number(it.Number) + "/thread")); !strings.Contains(th, "The entry was cancelled.") {
		t.Errorf("thread for agents: %s", th)
	}
}

func TestMeetingAnEntryThatOnlyDisappearsFromTheFeedIsNotACancellation(t *testing.T) {
	e := newMeetEnv(t)
	e.put("a.ics", meeting("m1", "Budget review", "20261125T090000Z", "20261125T100000Z"))
	it := e.open(e.entry("Budget review"))
	e.dav.drop(e.cal, "a.ics")
	e.readICloud()
	if e.entry("Budget review").Status != "removed" {
		t.Fatal("the entry was not marked removed")
	}
	if len(e.notices(it.ID)) != 0 || e.item(it.ID).Stage != stageInProgress {
		t.Error("a removal was told as a cancellation")
	}
	if !strings.Contains(e.threadPage(it.ID), "Removed") {
		t.Error("the header does not say the entry is removed")
	}
}

func TestMeetingARepeatingEntryHasOneThreadForTheWholeSeries(t *testing.T) {
	e := newMeetEnv(t)
	e.put("s.ics", vcal(veventText("UID:ser", "SUMMARY:Standup", "DTSTART:20261123T080000Z", "DTEND:20261123T081500Z", "RRULE:FREQ=DAILY;COUNT=3")))
	rows := e.byTitle("Standup")
	if len(rows) != 3 {
		t.Fatalf("occurrences: %d", len(rows))
	}
	first := e.open(rows[0])
	if third := e.open(rows[2]); third.ID != first.ID || len(e.threads()) != 1 {
		t.Fatalf("a second occurrence opened another thread")
	}
	if ag := e.agenda(); strings.Contains(ag, ">Open thread<") || strings.Count(ag, ">Thread<") != 3 {
		t.Errorf("every occurrence should say Thread:\n%s", ag)
	}
	// One occurrence moves: the series' thread says so, once.
	e.put("s.ics", vcal(
		veventText("UID:ser", "SUMMARY:Standup", "DTSTART:20261123T080000Z", "DTEND:20261123T081500Z", "RRULE:FREQ=DAILY;COUNT=3"),
		veventText("UID:ser", "SUMMARY:Standup", "RECURRENCE-ID:20261124T080000Z", "DTSTART:20261124T150000Z", "DTEND:20261124T151500Z")))
	got := e.notices(first.ID)
	if len(got) != 1 || !strings.HasPrefix(got[0], "Moved to Tue 24 Nov ") {
		t.Fatalf("notices: %v", got)
	}
}

func TestMeetingAnAgentProposesAThreadAndItStartsInNewOnlyAfterApprove(t *testing.T) {
	e := newMeetEnv(t)
	e.put("a.ics", meeting("m1", "Budget review", "20261125T090000Z", "20261125T100000Z"))
	ent := e.entry("Budget review")
	m := e.toolJSON("propose_thread", map[string]any{"id": ent.ID, "reason": "Papers will be sent before it.", "agent": "Claude"})
	id := m["proposal_id"]
	if m["ok"] != true || id == nil || len(e.threads()) != 0 {
		t.Fatalf("propose_thread: %v", m)
	}
	page := e.agenda()
	for _, want := range []string{"Proposal", "Claude", "Open thread", "Budget review", "Papers will be sent before it.", "Approve", "Dismiss"} {
		if !strings.Contains(page, want) {
			t.Errorf("the agenda lacks %q:\n%s", want, page)
		}
	}
	// The card is on the entry; list_agenda says a proposal waits.
	if !strings.Contains(fmt.Sprint(e.getJSON("/api/agenda")), "thread_proposed:true") {
		t.Errorf("list_agenda: %v", e.getJSON("/api/agenda"))
	}
	if again := e.toolJSON("propose_thread", map[string]any{"id": ent.ID, "reason": "Again.", "agent": "Claude"}); again["unchanged"] != true || again["proposal_id"] != id {
		t.Errorf("twice: %v", again)
	}
	if code := e.decide(int(id.(float64)), "approve"); code != 200 {
		t.Fatalf("approve: %d", code)
	}
	ths := e.threads()
	if len(ths) != 1 || ths[0].Kind != meetingKind || ths[0].Stage != stageNew || ths[0].Hidden {
		t.Fatalf("threads: %+v", ths)
	}
	// With a thread the entry is refused a second proposal, and the button says Thread.
	if text, bad := e.tool("propose_thread", map[string]any{"id": ent.ID, "reason": "Again."}); !bad || !strings.Contains(text, repo.Number(ths[0].Number)) {
		t.Errorf("a second proposal: %v %s", bad, text)
	}
	if strings.Contains(e.agenda(), ">Open thread<") {
		t.Error("Open thread is still offered")
	}
	// A second Approve does nothing.
	e.decide(int(id.(float64)), "approve")
	if len(e.threads()) != 1 {
		t.Error("a second Approve made another thread")
	}
}

func TestMeetingDismissedProposalMakesNothingAndAToDoIsRefused(t *testing.T) {
	e := newMeetEnv(t)
	e.put("a.ics", meeting("m1", "Budget review", "20261125T090000Z", "20261125T100000Z"))
	m := e.toolJSON("propose_thread", map[string]any{"id": e.entry("Budget review").ID, "reason": "Because.", "agent": "Claude"})
	e.decide(int(m["proposal_id"].(float64)), "dismiss")
	if len(e.threads()) != 0 || strings.Contains(e.agenda(), "Because.") {
		t.Error("a dismissed proposal did something")
	}
	todo := e.addTodo("Buy milk")
	if text, bad := e.tool("propose_thread", map[string]any{"id": todo.ID, "reason": "No."}); !bad || !strings.Contains(text, "not a calendar entry") {
		t.Errorf("a to-do: %v %s", bad, text)
	}
	if text, bad := e.tool("propose_thread", map[string]any{"id": 99999, "reason": "No."}); !bad {
		t.Errorf("no entry: %s", text)
	}
	if _, bad := e.tool("propose_thread", map[string]any{"id": e.entry("Budget review").ID}); !bad {
		t.Error("a reason is required")
	}
}

func TestMeetingOpeningTheThreadYourselfWithdrawsTheWaitingProposal(t *testing.T) {
	e := newMeetEnv(t)
	e.put("a.ics", meeting("m1", "Budget review", "20261125T090000Z", "20261125T100000Z"))
	ent := e.entry("Budget review")
	m := e.toolJSON("propose_thread", map[string]any{"id": ent.ID, "reason": "Because.", "agent": "Claude"})
	e.open(ent)
	p, _ := e.a.store.AgendaProposalByID(e.ctx, int64(m["proposal_id"].(float64)))
	if p.Status != "withdrawn" || strings.Contains(e.agenda(), "Because.") {
		t.Errorf("the proposal: %+v", p)
	}
}

func TestMeetingTheSwitchHidesTheButtonAndRefusesProposalsButKeepsThreadsReadable(t *testing.T) {
	e := newMeetEnv(t)
	e.put("a.ics", meeting("m1", "Budget review", "20261125T090000Z", "20261125T100000Z"))
	e.put("b.ics", meeting("m2", "Dentist", "20261126T090000Z", "20261126T100000Z"))
	it := e.open(e.entry("Budget review"))
	meetingThreads = false
	defer func() { meetingThreads = true }()
	ag := e.agenda()
	if strings.Contains(ag, ">Open thread<") || !strings.Contains(ag, ">Thread<") {
		t.Errorf("the agenda with the switch off:\n%s", ag)
	}
	if text, bad := e.tool("propose_thread", map[string]any{"id": e.entry("Dentist").ID, "reason": "x"}); !bad || !strings.Contains(text, "switched off") {
		t.Errorf("propose_thread: %v %s", bad, text)
	}
	if rec := post(e.h, fmt.Sprintf("/agenda/%d/thread", e.entry("Dentist").ID), url.Values{}); rec.Code == 303 {
		t.Error("the button still works")
	}
	if !strings.Contains(e.threadPage(it.ID), "Budget review") {
		t.Error("an existing thread is not readable")
	}
	// It still follows its entry.
	e.put("a.ics", meeting("m1", "Budget review", "20261125T090000Z", "20261125T100000Z", "LOCATION:Room 3"))
	if got := e.notices(it.ID); len(got) != 1 || got[0] != "Place changed to Room 3." {
		t.Errorf("notices: %v", got)
	}
}

func TestMeetingMailAndChatsJoinOnlyByProposalAndASplitTakesThemOutAgain(t *testing.T) {
	e := newMeetEnv(t)
	e.put("a.ics", meeting("m1", "Budget review", "20261125T090000Z", "20261125T100000Z"))
	th := e.open(e.entry("Budget review"))
	mail := e.newItem(stageNew, false, "Budget papers", "Here are the papers.")
	if th2 := e.item(th.ID); th2.Kind != meetingKind {
		t.Fatal("kind")
	}
	// Nothing joins by itself.
	if e.a.store.MailOfItem(e.ctx, th.ID) != 0 {
		t.Fatal("mail joined on its own")
	}
	// A meeting thread is never joined into another row.
	if text, bad := e.tool("propose_merge", map[string]any{"id": mail.Number, "join": th.Number, "reason": "Same meeting."}); !bad || !strings.Contains(text, "never joined") {
		t.Errorf("joining the thread into a mail row: %v %s", bad, text)
	}
	m := e.toolJSON("propose_merge", map[string]any{"id": th.Number, "join": mail.Number, "reason": "The papers are for this meeting."})
	if m["ok"] != true || e.a.store.MailOfItem(e.ctx, th.ID) != 0 {
		t.Fatalf("propose_merge: %v", m)
	}
	rec := e.decideAsOwner(int64(m["proposal_id"].(float64)), "approve")
	if rec.Code != 200 {
		t.Fatalf("approve: %d %s", rec.Code, rec.Body.String())
	}
	cur := e.item(th.ID)
	if cur.Kind != meetingKind || cur.Subject != "Budget review" || e.a.store.MailOfItem(e.ctx, th.ID) != 1 {
		t.Fatalf("after the join: %+v", cur)
	}
	if page := e.threadPage(th.ID); !strings.Contains(page, "Here are the papers.") || !strings.Contains(page, "Budget review") {
		t.Errorf("the thread after the join:\n%s", page)
	}
	// The mail's own number leads to the thread, and the entry's button still says Thread.
	if rec := get(e.h, fmt.Sprintf("/items/%d", mail.ID)); rec.Code != 303 {
		t.Errorf("the joined number: %d", rec.Code)
	}
	// Split it off again, by proposal.
	msgs, _, _ := e.a.store.MessagesSince(e.ctx, th.ID, 0)
	s := e.toolJSON("propose_split", map[string]any{"id": th.Number, "message": msgs[0].ID, "reason": "It was about something else."})
	if rec := e.decideAsOwner(int64(s["proposal_id"].(float64)), "approve"); rec.Code != 200 {
		t.Fatalf("split: %d %s", rec.Code, rec.Body.String())
	}
	cur = e.item(th.ID)
	if cur.Kind != meetingKind || cur.Subject != "Budget review" || e.a.store.MailOfItem(e.ctx, th.ID) != 0 {
		t.Errorf("after the split: %+v", cur)
	}
	if len(e.threads()) != 1 {
		t.Error("the thread is gone")
	}
}

func TestMeetingNoReplyIsProposedOnAThreadThatHoldsNoMessage(t *testing.T) {
	e := newMeetEnv(t)
	e.put("a.ics", meeting("m1", "Budget review", "20261125T090000Z", "20261125T100000Z"))
	th := e.open(e.entry("Budget review"))
	text, bad := e.tool("propose_reply", map[string]any{"id": th.Number, "text": "Hello.", "reason": "Because."})
	if !bad || !strings.Contains(text, "no message to answer") {
		t.Errorf("propose_reply: %v %s", bad, text)
	}
	if ps, _ := e.a.store.ProposalsOfItem(e.ctx, th.ID); len(ps) != 0 {
		t.Errorf("a proposal was stored: %+v", ps)
	}
}

func TestMeetingATestRunsThreadHangsOnTheRunsOwnEntryOnly(t *testing.T) {
	e := newMeetEnv(t)
	run := "e2e-meeting01"
	e.put("a.ics", meeting("m1", "Real meeting", "20261125T090000Z", "20261125T100000Z"))
	real := e.entry("Real meeting")
	var test repo.Agenda
	rctx := repo.WithRun(e.ctx, run)
	err := e.a.store.Update(rctx, func(tx *repo.Tx) error {
		id, err := tx.InsertAgenda(rctx, repo.Agenda{Kind: "event", Source: srcICloud, SourceID: "t1", CalKey: real.CalKey, Calendar: "Work", UID: "m1", Title: "Test meeting",
			StartAt: real.StartAt, EndAt: real.EndAt, Status: "confirmed", State: "open", Version: 1, Hash: "h", CreatedAt: 1, UpdatedAt: 1})
		test, _ = tx.AgendaByID(rctx, id)
		return err
	})
	if err != nil || test.TestRun != run {
		t.Fatalf("test entry: %v %+v", err, test)
	}
	// Under the run, the real entry is out of reach.
	if _, _, err := e.a.openMeeting(rctx, ownerActor(), real.ID, false); !repo.IsRunError(err) {
		t.Errorf("a run opened a thread on a real entry: %v", err)
	}
	if text, bad := e.tool("propose_thread", map[string]any{"id": real.ID, "reason": "x", "run": run}); !bad || !strings.Contains(text, "test run") {
		t.Errorf("propose_thread under a run on a real entry: %v %s", bad, text)
	}
	// A test entry has a thread of the run's mark, apart from the real thread of the same event id.
	realThread := e.open(real)
	it, made, err := e.a.openMeeting(e.ctx, ownerActor(), test.ID, false)
	if err != nil || !made || it.TestRun != run || it.ID == realThread.ID {
		t.Fatalf("the test thread: %+v %v %v", it, made, err)
	}
	if realThread.TestRun != "" {
		t.Error("the real thread carries a mark")
	}
	// Removing the run takes its thread and nothing else.
	if err := e.a.store.Update(e.ctx, func(tx *repo.Tx) error { _, _, err := tx.RemoveRun(e.ctx, run); return err }); err != nil {
		t.Fatal(err)
	}
	if got := e.threads(); len(got) != 1 || got[0].ID != realThread.ID {
		t.Errorf("threads after removing the run: %+v", got)
	}
}

func TestMeetingNoCodePathOfTheFeatureWritesToACalendar(t *testing.T) {
	e := newMeetEnv(t)
	e.signInMicrosoft()
	e.gc.events = []map[string]any{gev("g1", "gu1", "Board", "2026-11-24T10:00:00.0000000", "2026-11-24T11:00:00.0000000", false)}
	e.readOutlook()
	e.put("a.ics", meeting("m1", "Budget review", "20261125T090000Z", "20261125T100000Z", "LOCATION:Room 2"))
	e.toolJSON("propose_thread", map[string]any{"id": e.entry("Board").ID, "reason": "x"})
	for _, title := range []string{"Budget review", "Board"} {
		th := e.open(e.entry(title))
		e.ajaxPost(fmt.Sprintf("/items/%d/notes", th.ID), url.Values{"text": {"note"}})
		e.ajaxPost(fmt.Sprintf("/items/%d/resolve", th.ID), url.Values{})
	}
	e.put("a.ics", meeting("m1", "Budget review", "20261126T090000Z", "20261126T100000Z", "LOCATION:Room 5"))
	e.readOutlook()
	for m := range e.dav.methods() {
		if m != "PROPFIND" && m != "REPORT" && m != "GET" && m != "OPTIONS" {
			t.Errorf("the feature sent %s to the calendar server", m)
		}
	}
	for _, r := range e.gc.reqs {
		if !strings.HasPrefix(r, "GET ") {
			t.Errorf("the feature sent %q to the calendar API", r)
		}
	}
	// And no code of the feature has a way to: it holds no client of its own.
	src, err := os.ReadFile("meeting.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"http.NewRequest", "http.Client", "http.Post", "http.Get", ".Do(", "PUT", "PATCH", "MKCALENDAR", "caldav", "graphCalPost"} {
		if strings.Contains(string(src), bad) {
			t.Errorf("meeting.go mentions %q", bad)
		}
	}
}
