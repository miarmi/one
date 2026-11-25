package main

import (
	"testing"

	"lab/apps/one/repo"
)

func repoRule(field, op, value string) repo.Rule {
	return repo.Rule{Enabled: true, Source: "gmail", Field: field, Op: op, Value: value}
}

// TestApplyArrivalFollowsTheTable is section 3.5 of the design for a message
// not seen before, as one pure function.
func TestApplyArrivalFollowsTheTable(t *testing.T) {
	const cutoff, now = 1000, 5000
	item := func(stage string, needs bool) repo.Item {
		return repo.Item{Stage: stage, NeedsClaude: needs, FirstAt: 1500, LastAt: 2000, Subject: "old"}
	}
	cases := []struct {
		name       string
		from       string
		stage      string
		needs      bool
		a          arrival
		wantStage  string
		wantNeeds  bool
		wantReopen bool
		wantPend   bool
	}{
		{"someone else on pending", "", stagePending, false, arrival{At: 3000}, stageNew, true, true, false},
		{"someone else on resolved", "", stageResolved, false, arrival{At: 3000}, stageNew, true, true, false},
		{"someone else on closed", "", stageClosed, false, arrival{At: 3000}, stageNew, true, true, false},
		{"someone else on history", "", stageHistory, false, arrival{At: 3000}, stageNew, true, true, false},
		{"someone else on in progress", "", stageInProgress, false, arrival{At: 3000}, stageInProgress, true, false, false},
		{"owner on in progress", "", stageInProgress, false, arrival{At: 3000, SentByMe: true}, stagePending, false, false, true},
		{"owner on resolved", "", stageResolved, false, arrival{At: 3000, SentByMe: true}, stageResolved, false, false, false},
		{"owner on a waiting item leaves it waiting", "", stageNew, true, arrival{At: 3000, SentByMe: true}, stageNew, true, false, false},
		{"an older mail found late changes nothing", "", stageResolved, false, arrival{At: 1800}, stageResolved, false, false, false},
	}
	for _, c := range cases {
		got, ch := applyArrival(item(c.stage, c.needs), c.a, cutoff, now)
		if got.Stage != c.wantStage || got.NeedsClaude != c.wantNeeds || ch.Reopened != c.wantReopen || ch.SetPending != c.wantPend {
			t.Errorf("%s: stage %s needs %v change %+v", c.name, got.Stage, got.NeedsClaude, ch)
		}
	}
	// A newest message that is itself older than the review window is no news.
	it := repo.Item{Stage: stageHistory, FirstAt: 100, LastAt: 200}
	if got, ch := applyArrival(it, arrival{At: 900}, cutoff, now); got.Stage != stageHistory || got.NeedsClaude || ch.Reopened {
		t.Errorf("old mail reopened a history item: %+v %+v", got, ch)
	}
	if it := newItem(arrival{At: 3000}, cutoff); it.Stage != stageNew || !it.NeedsClaude {
		t.Errorf("new item from someone else: %+v", it)
	}
	if it := newItem(arrival{At: 900}, cutoff); it.Stage != stageHistory || it.NeedsClaude {
		t.Errorf("old item: %+v", it)
	}
	if it := newItem(arrival{At: 3000, SentByMe: true}, cutoff); it.Stage != stageHistory || it.NeedsClaude {
		t.Errorf("item from the owner's own mail: %+v", it)
	}
}
