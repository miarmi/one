package main

import (
	"context"
	"strings"
	"testing"
)

// The history of the Office mailbox (SPEC 21): the Graph message list with
// the needed fields only, every folder but Drafts, resumable, read-only.

func (g *genv) startHistory() {
	g.t.Helper()
	g.must(g.a.startHistory(g.ctx, historyKeyO))
}

func (g *genv) histOf(id string) (placeAnswered struct {
	State    string
	Answered bool
	Mine     bool
	Seen     bool
	Starred  bool
	Sender   string
}) {
	g.t.Helper()
	h, answered, ok := g.a.store.HistoryRowOf(g.ctx, srcGraph, id)
	if !ok {
		g.t.Fatalf("no history line for %s", id)
	}
	placeAnswered.State, placeAnswered.Answered, placeAnswered.Mine, placeAnswered.Seen, placeAnswered.Starred, placeAnswered.Sender = h.State, answered, h.Mine, h.Seen, h.Starred, h.Sender
	return
}

func TestGraphHistoryPassRecordsFlagsWithoutTextsForOldMail(t *testing.T) {
	e := newGraphEnv(t)
	oldIn := e.box.add("F-inbox", e.in("A", "Old secret subject", 900))
	e.box.set(oldIn.id, func(m *gmMsg) { m.read, m.flagged = true, true })
	e.box.add("F-sent", e.out("A", "Re: Old secret subject", 899))
	oldBin := e.box.add("F-deleted", e.in("B", "Thrown away", 800))
	oldJunk := e.box.add("F-junk", e.in("C", "Junk subject", 700))
	oldSub := e.box.add("F-trashsub", e.in("D", "In a subfolder of the bin", 600))
	oldArch := e.box.add("F-archive", e.in("E", "Archived", 500))
	draft := e.box.add("F-drafts", e.in("X", "A draft", 400))
	draft.draft = true
	e.box.add("F-inbox", e.in("N", "Fresh", 3))
	e.must(e.importYear())
	bodies := len(e.box.mimeFetched())
	messages := e.a.store.CountRows(e.ctx, "messages")

	e.startHistory()
	e.must(e.a.gimp.RunHistory(e.ctx))

	if got := e.a.store.HistoryCount(e.ctx); got != 7 { // six old ones (no draft) and the fresh one
		t.Fatalf("the history holds %d lines, want 7", got)
	}
	if got := len(e.box.mimeFetched()); got != bodies {
		t.Fatalf("the history pass downloaded %d originals", got-bodies)
	}
	if got := e.a.store.CountRows(e.ctx, "messages"); got != messages {
		t.Fatalf("the history pass made messages: %d", got-messages)
	}
	if h := e.histOf(oldIn.id); h.State != placeInbox || !h.Seen || !h.Starred || h.Mine || !h.Answered || h.Sender != "jana@example.test" {
		t.Fatalf("old inbox mail: %+v", h)
	}
	for id, want := range map[string]string{oldBin.id: placeDeleted, oldJunk.id: placeSpam, oldSub.id: placeDeleted, oldArch.id: placeArchived} {
		if h := e.histOf(id); h.State != want {
			t.Errorf("%s: place %q, want %q", id, h.State, want)
		}
	}
	if _, _, ok := e.a.store.HistoryRowOf(e.ctx, srcGraph, draft.id); ok {
		t.Error("a draft is in the history")
	}
	// Only the needed fields are asked for.
	for _, sel := range e.box.listSelects {
		if sel != historySelect {
			t.Fatalf("the list asked for %q", sel)
		}
		for _, bad := range []string{"subject", "body", "bodyPreview", "toRecipients", "internetMessageId"} {
			if strings.Contains(sel, bad) {
				t.Fatalf("the list asks for %s", bad)
			}
		}
	}
	for _, c := range e.box.callList() {
		if !strings.HasPrefix(c, "GET ") {
			t.Fatalf("the history pass made a call that is not a read: %s", c)
		}
	}
}

func TestGraphHistoryPassIsResumableAndWritesNothingTwice(t *testing.T) {
	e := newGraphEnv(t)
	for i := 0; i < 9; i++ {
		e.box.add("F-inbox", e.in("T"+string(rune('a'+i)), "x", float64(500+i)))
	}
	e.box.add("F-archive", e.in("Z", "y", 400))
	e.startHistory()
	ctx, cancel := context.WithCancel(e.ctx)
	pages := 0
	e.box.onList = func() {
		pages++
		if pages == 2 {
			cancel()
		}
	}
	// Pages of 3: use the importer's own size for the test.
	e.a.gimp.historyPage = 3
	if err := e.a.gimp.RunHistory(ctx); err == nil {
		t.Fatal("a stopped pass did not say so")
	}
	if hs := e.a.gimp.loadHistory(e.ctx); hs.Done {
		t.Fatal("a stopped pass is marked done")
	}
	mid := e.a.store.HistoryCount(e.ctx)
	if mid == 0 || mid >= 10 {
		t.Fatalf("after the stop %d lines, want some but not all", mid)
	}
	e.box.onList = nil
	before := e.box.listPages
	e.must(e.a.gimp.RunHistory(e.ctx))
	if got := e.a.store.HistoryCount(e.ctx); got != 10 {
		t.Fatalf("after resuming %d lines, want 10", got)
	}
	// A whole pass reads 9 pages (three of the inbox, one of every other folder but Drafts); the first inbox page is done.
	if read := e.box.listPages - before; read >= 9 {
		t.Fatalf("the resumed pass read %d pages: it did not continue where it stopped", read)
	}
	e.must(e.a.startHistory(e.ctx, historyKeyO))
	e.must(e.a.gimp.RunHistory(e.ctx))
	if r := e.lastRun(historyMode); r.Stored != 0 || r.Status != "done" {
		t.Fatalf("a second pass wrote lines: %+v", r)
	}
	if got := e.a.store.HistoryCount(e.ctx); got != 10 {
		t.Fatalf("a second pass left %d lines", got)
	}
}
