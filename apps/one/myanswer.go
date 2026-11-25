package main

import (
	"context"
	"strings"
)

// SPEC 18, last paragraph: an entry says how the owner answered it, where the
// calendar knows. What each source gives, all kept in the entry's answers
// (`address=PARTSTAT;...`, one per guest) as it was read:
//
//   - Outlook (Graph calendarView): each attendee's response (accepted,
//     declined, tentativelyAccepted, notResponded, organizer, none).
//   - iCloud (CalDAV) and Google (the calendar's secret iCal address): the
//     PARTSTAT of each ATTENDEE line (ACCEPTED, DECLINED, TENTATIVE,
//     NEEDS-ACTION, DELEGATED).
//
// The owner's own line is found by his accounts' own addresses and by nothing
// else: the connected Gmail and Office addresses, the addresses ONE has
// remembered from them and from his sent mail, and, for iCloud, the Apple ID
// when it is an address (matched with isOwn, so a +tag or another spelling of
// his Gmail address counts). No guess from a name. When no line is his, when
// his lines disagree, or when the answer is none, not given or needs action,
// the answer is unknown and nothing is marked.
const (
	answerDeclined  = "declined"
	answerTentative = "tentative"
	answerAccepted  = "accepted"
)

func answerWord(partstat string) string {
	switch strings.ToUpper(strings.TrimSpace(partstat)) {
	case "DECLINED":
		return answerDeclined
	case "TENTATIVE", "TENTATIVELYACCEPTED":
		return answerTentative
	case "ACCEPTED":
		return answerAccepted
	}
	return ""
}

// myAnswerOf reads the owner's answer from an entry's answers.
func myAnswerOf(answers string, own []string) string {
	if answers == "" || len(own) == 0 {
		return ""
	}
	found := ""
	for _, part := range strings.Split(answers, ";") {
		i := strings.LastIndexByte(part, '=')
		if i <= 0 || !isOwn(own, part[:i]) {
			continue
		}
		w := answerWord(part[i+1:])
		if w == "" {
			continue
		}
		if found != "" && found != w {
			return "" // his own lines disagree: not known
		}
		found = w
	}
	return found
}

// calendarOwn is the addresses that count as the owner's in a calendar entry.
func (a *app) calendarOwn(ctx context.Context) []string {
	own := a.ownAddresses(ctx, a.store.Q)
	var s icloudSecret
	if readSecret(a.secretsDir, icloudSecretFile, &s) && strings.Contains(s.AppleID, "@") {
		own = append(own, strings.ToLower(strings.TrimSpace(s.AppleID)))
	}
	return own
}

// answerOfGroup is the owner's answer for one shown entry: its own copy's, else
// the first other copy (the same event in another calendar) that knows.
func answerOfGroup(g *merged, own []string) string {
	if w := myAnswerOf(g.row.Answers, own); w != "" {
		return w
	}
	for _, r := range g.all {
		if w := myAnswerOf(r.Answers, own); w != "" {
			return w
		}
	}
	return ""
}
