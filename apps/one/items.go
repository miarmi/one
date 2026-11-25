package main

import (
	"strings"

	"lab/apps/one/repo"
)

// The stages of an item. "history" holds items whose last message is older
// than the review window: stored and searchable, not waiting for Claude.
const (
	stageNew        = "new"
	stageInProgress = "in_progress"
	stagePending    = "pending"
	stageResolved   = "resolved"
	stageClosed     = "closed"
	stageHistory    = "history"
)

// arrival is one message the import has not seen before.
type arrival struct {
	At       int64 // when it was received, Unix seconds
	SentByMe bool
	Subject  string
}

// itemChange says what the arrival did to the item, for the event log.
type itemChange struct {
	Reopened   bool // the item came back to New
	SetPending bool // the owner's own message moved it to Pending
}

// newItem is the item a first message makes. Mail from someone else inside
// the review window waits for Claude, hidden from the list until Claude has
// looked; anything older, and anything the owner sent himself, is history.
func newItem(a arrival, cutoff int64) repo.Item {
	it := repo.Item{Kind: "thread", Subject: a.Subject, Stage: stageHistory, FirstAt: a.At, LastAt: a.At, MovedAt: a.At}
	if !a.SentByMe && a.At >= cutoff {
		it.Stage = stageNew
		it.NeedsClaude = true
		it.Hidden = true // SPEC 11: The owner sees it after Claude has looked
	}
	return it
}

// applyArrival is section 3.5 of the design for a message not seen before:
//
//   - a message that is not the newest of its item changes nothing but the
//     item's first-message time (an older mail found late);
//   - from someone else, inside the review window: the item waits for Claude
//     and a Pending, Resolved, Closed or history item comes back to New;
//   - sent by the owner: never waits for Claude, and In progress moves to Pending.
func applyArrival(it repo.Item, a arrival, cutoff, now int64) (repo.Item, itemChange) {
	var ch itemChange
	if a.At < it.FirstAt {
		it.FirstAt = a.At
	}
	if a.At < it.LastAt {
		return it, ch
	}
	it.LastAt = a.At
	it.Subject = a.Subject
	if a.SentByMe {
		if it.Stage == stageInProgress {
			it.Stage = stagePending
			it.PendingSince = now
			it.MovedAt = now
			ch.SetPending = true
		}
		return it, ch
	}
	if a.At < cutoff {
		return it, ch
	}
	// A new message from someone else is the one thing that moves a row.
	it.MovedAt = a.At
	switch it.Stage {
	case stagePending, stageResolved, stageClosed, stageHistory:
		it.Stage = stageNew
		it.PendingSince = 0
		ch.Reopened = true
	}
	it.NeedsClaude = true
	return it, ch
}

// matchesRule says whether a rule drops a message with these facts.
type ruleFacts struct {
	Sender  string
	Subject string
	Labels  []string
	Headers map[string]string // lower-case names, raw values
	// Chats (SPEC 21): the network, the type of chat (direct or group) and
	// whether it is a muted group chat ("true" or "false").
	Network  string
	ChatType string
	Muted    string
}

func ruleMatches(r repo.Rule, f ruleFacts) bool {
	field := strings.ToLower(r.Field)
	var values []string
	switch {
	case field == "sender":
		values = []string{f.Sender}
	case field == "sender_domain":
		if i := strings.LastIndex(f.Sender, "@"); i >= 0 {
			values = []string{f.Sender[i+1:]}
		}
	case field == "subject":
		values = []string{f.Subject}
	case field == "label" || field == "folder":
		values = f.Labels
	case field == "network":
		values = []string{f.Network}
	case field == "chat_type":
		values = []string{f.ChatType}
	case field == "muted":
		values = []string{f.Muted}
	case strings.HasPrefix(field, "header:"):
		if v, ok := f.Headers[strings.TrimPrefix(field, "header:")]; ok {
			values = []string{v}
		}
	}
	want := strings.ToLower(strings.TrimSpace(r.Value))
	for _, v := range values {
		v = strings.ToLower(strings.TrimSpace(boundMatched(v)))
		switch r.Op {
		case "equals":
			if v == want {
				return true
			}
		case "contains":
			if want != "" && strings.Contains(v, want) {
				return true
			}
		case "starts_with":
			if want != "" && strings.HasPrefix(v, want) {
				return true
			}
		case "ends_with":
			if want != "" && strings.HasSuffix(v, want) {
				return true
			}
		}
	}
	return false
}

// firstRuleHit returns the first enabled rule, in order, that matches.
func firstRuleHit(rules []repo.Rule, f ruleFacts) (repo.Rule, bool) {
	for _, r := range rules {
		if r.Enabled && ruleMatches(r, f) {
			return r, true
		}
	}
	return repo.Rule{}, false
}

// startingRules are the rules ONE ships with in this slice.
var startingRules = []repo.Rule{
	{
		Name:      "GitHub pipeline mails",
		Enabled:   true,
		Source:    "any", // both mail sources; chats and calendars carry no such header
		Field:     "header:x-github-reason",
		Op:        "equals",
		Value:     "ci_activity",
		CreatedBy: "system",
		Note:      "Automatic GitHub Actions run notifications.",
	},
	{
		// A muted group chat is dropped when its message arrives from the bridge
		// (SPEC 21). It cannot touch mail. A muted one-to-one chat is not a group
		// chat and is never dropped by it.
		Name:      "Muted group chats",
		Enabled:   true,
		Source:    "chat",
		Field:     "muted",
		Op:        "equals",
		Value:     "true",
		CreatedBy: "system",
		Note:      "Group chats that are muted stay out of the list and are counted here.",
	},
}

// boundMatched keeps what a rule compares within maxMatchedBytes: a longer
// value keeps its beginning and its end, which is where starts_with and
// ends_with look.
func boundMatched(v string) string {
	if len(v) <= maxMatchedBytes {
		return v
	}
	h := maxMatchedBytes / 2
	return strings.ToValidUTF8(v[:h], "") + strings.ToValidUTF8(v[len(v)-h:], "")
}
