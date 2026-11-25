package main

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"time"
)

// SPEC 15: the Refresh button fetches everything now, with the same sign-ins
// and the same round the timer runs. It costs nothing (no agent, no model).
// Pressing it while a fetch runs, from the button or from the timer, starts
// nothing new.
type refresher struct {
	mu        sync.Mutex
	running   bool
	finished  time.Time
	startedAt time.Time // when the running (or last) round began
	agentAt   time.Time // when an agent last started a round (refresh_now)
}

func (r *refresher) startedUnix() int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.startedAt.Unix()
}

// begin says whether the caller may run a fetch now.
func (r *refresher) begin() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.running {
		return false
	}
	r.running, r.startedAt = true, time.Now()
	return true
}

func (r *refresher) end(at time.Time) {
	r.mu.Lock()
	r.running, r.finished = false, at
	r.mu.Unlock()
}

func (r *refresher) state() (bool, time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.running, r.finished
}

// fetchAll is one round: Gmail, the Office mailbox, then the calendars.
// It returns false when another round was already running.
func (a *app) fetchAll(ctx context.Context) bool {
	if !a.refr.begin() {
		return false
	}
	a.fetchRound(ctx)
	return true
}

// fetchRound runs a round the caller has claimed with begin, and ends it.
func (a *app) fetchRound(ctx context.Context) {
	defer func() { a.refr.end(a.now()) }()
	// Each part is its own unit: one that panics does not keep the others from running.
	a.runUnit("gmail round", func() { a.imp.logErr(a.imp.Tick(ctx)) })
	a.runUnit("office round", func() { a.gimp.logErr(a.gimp.Tick(ctx)) })
	a.syncCalendars(ctx) // the calendars are read after the mail (SPEC 15)
}

type refreshState struct {
	Running  bool   `json:"running"`
	Finished string `json:"finished"` // the time, as the page shows it, once a round has finished
	At       int64  `json:"at"`
	// What the round that finished brought (SPEC 15): the messages new to ONE
	// and each source that did not answer, with its error in plain words.
	New    int          `json:"new"`
	Failed []failedLine `json:"failed"`
}

type failedLine struct {
	Source string `json:"source"`
	Error  string `json:"error"`
}

func (a *app) refreshStateNow(ctx context.Context) refreshState {
	running, fin := a.refr.state()
	st := refreshState{Running: running, Failed: []failedLine{}}
	if !fin.IsZero() {
		st.At = fin.Unix()
		st.Finished = fin.In(a.loc).Format("15:04")
	}
	if !running && !fin.IsZero() {
		round := a.roundOutcome(ctx, a.refr.startedUnix())
		st.New = round.NewMessages
		for _, s := range round.Sources {
			if s.Failed {
				st.Failed = append(st.Failed, failedLine{s.Label, s.Error})
			}
		}
	}
	return st
}

// refreshNow is the page's Refresh: it starts a round in the background (once)
// and answers at once with where things stand.
func (a *app) refreshNow(w http.ResponseWriter, r *http.Request) {
	if a.refr.begin() {
		ctx := context.WithoutCancel(r.Context())
		go a.runUnit("refresh", func() { a.fetchRound(ctx) })
	}
	a.refreshJSON(w, r)
}

func (a *app) refreshStatus(w http.ResponseWriter, r *http.Request) { a.refreshJSON(w, r) }

func (a *app) refreshJSON(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(a.refreshStateNow(r.Context()))
}
