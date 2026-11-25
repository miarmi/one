package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func att(addr, partstat string) string {
	return "ATTENDEE;PARTSTAT=" + partstat + ":mailto:" + addr
}

// SPEC 18: the agenda marks what the owner declined or accepted only tentatively,
// found by his own address on the attendee line; where the calendar does not
// say, nothing is marked.
func TestAgendaMarksDeclinedAndTentativeFromOwnersOwnAttendeeLine(t *testing.T) {
	e := newCalEnv(t)
	cal := e.dav.calendar("work", "Work")
	e.dav.put(cal, "a.ics", one("d1", "Declined thing", "20261119T150000Z", "20261119T160000Z", att("someone@example.test", "ACCEPTED"), att(calUser, "DECLINED")))
	e.dav.put(cal, "b.ics", one("t1", "Maybe thing", "20261119T170000Z", "20261119T180000Z", att(calUser, "TENTATIVE")))
	e.dav.put(cal, "c.ics", one("y1", "Yes thing", "20261120T150000Z", "20261120T160000Z", att(calUser, "ACCEPTED")))
	e.dav.put(cal, "d.ics", one("o1", "Others declined", "20261120T170000Z", "20261120T180000Z", att("boss@example.test", "DECLINED")))
	e.dav.put(cal, "e.ics", one("n1", "Silent thing", "20261120T190000Z", "20261120T200000Z"))
	e.dav.put(cal, "f.ics", one("w1", "Waiting thing", "20261121T150000Z", "20261121T160000Z", att(calUser, "NEEDS-ACTION")))
	e.connectICloud()
	ag := e.agenda()
	if n := strings.Count(ag, ">Declined<"); n != 1 {
		t.Errorf("Declined marks: %d", n)
	}
	if n := strings.Count(ag, ">Tentative<"); n != 1 {
		t.Errorf("Tentative marks: %d", n)
	}
	// the mark sits with its entry and an accepted, silent or unanswered one has none
	li := func(title string) string {
		i := strings.Index(ag, ">"+title+"<")
		if i < 0 {
			t.Fatalf("no entry %q", title)
		}
		j := strings.Index(ag[i:], "</li>")
		return ag[i : i+j]
	}
	for title, want := range map[string]string{"Declined thing": ">Declined<", "Maybe thing": ">Tentative<"} {
		if !strings.Contains(li(title), want) {
			t.Errorf("%s lacks %s", title, want)
		}
	}
	for _, title := range []string{"Yes thing", "Others declined", "Silent thing", "Waiting thing"} {
		if s := li(title); strings.Contains(s, "Declined") || strings.Contains(s, "Tentative") {
			t.Errorf("%s is marked: %s", title, s)
		}
	}
	if !strings.Contains(ag, `ag-ev off`) {
		t.Error("a declined entry is shown as an off one, as a cancelled one is")
	}
	// list_agenda carries the answer, and only where the calendar says
	m := e.toolJSON("list_agenda", map[string]any{"from": "2026-11-18", "to": "2026-11-25"})
	got := map[string]any{}
	for _, x := range m["events"].([]any) {
		ev := x.(map[string]any)
		got[ev["title"].(string)] = ev["my_answer"]
	}
	want := map[string]any{"Declined thing": "declined", "Maybe thing": "tentative", "Yes thing": "accepted", "Others declined": nil, "Silent thing": nil, "Waiting thing": nil}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("my_answer of %s: %v, want %v", k, got[k], v)
		}
	}
}

func TestOwnersAnswerIsReadForEverySourceByHisOwnAddresses(t *testing.T) {
	own := []string{"owner@example.test", "owner.example@gmail.com"}
	// Outlook: the attendee's response as Graph words it
	var ev graphCalEvent
	raw := `{"id":"g1","subject":"X","start":{"dateTime":"2026-11-20T10:00:00","timeZone":"UTC"},"end":{"dateTime":"2026-11-20T11:00:00","timeZone":"UTC"},
	 "attendees":[{"status":{"response":"declined"},"emailAddress":{"address":"owner@example.test"}},{"status":{"response":"accepted"},"emailAddress":{"address":"x@example.test"}}]}`
	if err := json.Unmarshal([]byte(raw), &ev); err != nil {
		t.Fatal(err)
	}
	in, ok := instanceOfGraph(ev, json.RawMessage(raw), "k", time.UTC)
	if !ok || myAnswerOf(in.Answers, own) != "declined" {
		t.Errorf("Outlook: %q -> %q", in.Answers, myAnswerOf(in.Answers, own))
	}
	if got := myAnswerOf(strings.Replace(in.Answers, "DECLINED", "TENTATIVELYACCEPTED", 1), own); got != "tentative" {
		t.Errorf("Outlook tentative: %q", got)
	}
	// Google's iCal feed: Gmail spelled with dots and a tag still is his
	ics := vcal(veventText("UID:g", "SUMMARY:G", "DTSTART:20261120T100000Z", "DTEND:20261120T110000Z", att("owner.Example+cal@googlemail.com", "TENTATIVE")))
	evs := parseVEvents(ics, time.UTC)
	if len(evs) != 1 || myAnswerOf(strings.Join(evs[0].Answers, ";"), own) != "tentative" {
		t.Errorf("Google: %+v", evs)
	}
	// not his: a guest with a similar name, and his own lines disagreeing
	if got := myAnswerOf("ownerx@example.test=DECLINED", own); got != "" {
		t.Errorf("another address: %q", got)
	}
	if got := myAnswerOf("owner@example.test=DECLINED;owner.example@gmail.com=ACCEPTED", own); got != "" {
		t.Errorf("disagreeing lines: %q", got)
	}
	if got := myAnswerOf("owner@example.test=ORGANIZER;owner@example.test=NONE", own); got != "" {
		t.Errorf("organizer or none: %q", got)
	}
	if got := myAnswerOf("a=DECLINED", nil); got != "" {
		t.Errorf("no own address: %q", got)
	}
}
