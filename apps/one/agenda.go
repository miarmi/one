package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"lab/apps/one/repo"
	"lab/platform/appkit"
)

// The agenda (SPEC 18 to 20, DESIGN 3.3): what the calendars hold inside a
// window around today, and the to-dos. A calendar is read by a small reader
// that gives back the entries inside the window (calInstance); this file
// stores them as versions and never as a second entry. To add a calendar
// (like Google, read from a feed), write a reader that fills calInstance and call
// storeCalendar; nothing else in the agenda changes.

const (
	srcICloud     = "icloud"
	srcOutlookCal = "outlookcal"
	srcGoogleCal  = "googlecal"

	keyAgendaBack  = "agenda_back_days"
	keyAgendaAhead = "agenda_ahead_days"
	defAgendaDays  = 30

	maxTodoText = 300
)

var (
	icloudSrc     = mailSource{Name: srcICloud, Label: "iCloud Calendar"}
	outlookCalSrc = mailSource{Name: srcOutlookCal, Label: "Outlook Calendar"}
	googleCalSrc  = mailSource{Name: srcGoogleCal, Label: "Google Calendar"}
)

// calendarSources are the calendars the app reads itself, in the order of the page.
var calendarSources = []mailSource{outlookCalSrc, icloudSrc, googleCalSrc}

// --- the window ---

// agendaWindow is the time the agenda reads and shows: whole days in the owner's
// zone, from some days back to some days ahead of today.
type agendaWindow struct {
	From, To           time.Time // UTC instants; To is exclusive
	FromDate, ToDate   string    // the same days in the owner's zone
	Back, Ahead        int
	TodayStart, TodayN time.Time
}

func (a *app) agendaDays(ctx context.Context, key string) int {
	if n, err := strconv.Atoi(a.store.Setting(ctx, key)); err == nil && n > 0 {
		return n
	}
	return defAgendaDays
}

func (a *app) agendaWindow(ctx context.Context) agendaWindow {
	back, ahead := a.agendaDays(ctx, keyAgendaBack), a.agendaDays(ctx, keyAgendaAhead)
	n := a.now().In(a.loc)
	y, m, d := n.Date()
	w := agendaWindow{Back: back, Ahead: ahead}
	w.TodayStart = time.Date(y, m, d, 0, 0, 0, 0, a.loc)
	w.TodayN = w.TodayStart.AddDate(0, 0, 1)
	from := time.Date(y, m, d-back, 0, 0, 0, 0, a.loc)
	to := time.Date(y, m, d+ahead+1, 0, 0, 0, 0, a.loc)
	w.From, w.To = from.UTC(), to.UTC()
	w.FromDate, w.ToDate = from.Format(dateLayout), to.Format(dateLayout)
	return w
}

// --- what is stored ---

// instanceHash is what makes an entry a new version: the words and times the owner
// sees, its state in the calendar and what the guests answered. The calendar
// it is in is not part of it.
func instanceHash(in calInstance) string {
	h := sha256.New()
	for _, f := range []string{in.UID, in.Instance, in.Title, in.Place, in.Note, in.Answers, in.Status, in.StartDate, in.EndDate,
		strconv.FormatInt(in.Start.Unix(), 10), strconv.FormatInt(in.End.Unix(), 10), strconv.FormatBool(in.AllDay)} {
		h.Write([]byte(f))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

func rowFrom(source, calKey, calName string, in calInstance, now int64) repo.Agenda {
	title := strings.TrimSpace(in.Title)
	if title == "" {
		title = "(no title)"
	}
	return repo.Agenda{
		Kind: "event", Source: source, SourceID: in.SourceID, CalKey: calKey, Calendar: calName, UID: in.UID,
		Title: title, Place: in.Place, Note: in.Note, Answers: in.Answers,
		StartAt: in.Start.Unix(), EndAt: in.End.Unix(), AllDay: in.AllDay, StartDate: in.StartDate, EndDate: in.EndDate,
		Status: in.Status, State: "open", Version: 1, Hash: instanceHash(in), CreatedAt: now, UpdatedAt: now,
	}
}

// storeStats says what a read of one calendar changed.
type storeStats struct {
	Read, New, Changed, Removed int
	Failed                      int // entries left out because each failed on its own
}

func (s *storeStats) add(o storeStats) {
	s.Read += o.Read
	s.New += o.New
	s.Changed += o.Changed
	s.Removed += o.Removed
	s.Failed += o.Failed
}

func (s storeStats) stored() int { return s.New + s.Changed + s.Removed }

// storeCalendar puts what one calendar holds inside the window into the
// agenda, in one transaction. An entry that is new is added; one that
// changed (or came back, or was cancelled) is one entry with a new version;
// one that is not in the calendar any more, though it lies inside the
// window, is marked removed and kept. Reading the same calendar again
// writes nothing. complete says the read covered the whole window, so what
// is missing can be marked removed.
func (a *app) storeCalendar(ctx context.Context, runID int64, source, calKey, calName string, w agendaWindow, ins []calInstance, complete bool) (storeStats, error) {
	seen := map[string]bool{}
	st, err := a.storeInstances(ctx, runID, source, calKey, calName, ins, seen)
	if err != nil || !complete {
		return st, err
	}
	n, err := a.sweepRemoved(ctx, runID, calKey, w, seen)
	st.Removed = n
	return st, err
}

// storeInstances stores entries in one transaction and notes which it saw.
func (a *app) storeInstances(ctx context.Context, runID int64, source, calKey, calName string, ins []calInstance, seen map[string]bool) (storeStats, error) {
	var st storeStats
	now := a.now().Unix()
	err := a.store.Update(ctx, func(tx *repo.Tx) error {
		st = storeStats{}
		for _, in := range ins {
			if in.SourceID == "" {
				continue
			}
			st.Read++
			seen[in.SourceID] = true
			var n string
			err := tx.Try(ctx, func() error {
				return a.tryUnit("calendar event", func() (err error) {
					n, err = a.upsertEvent(ctx, tx, source, calKey, calName, in, runID, now)
					return err
				})
			})
			if err != nil {
				if isFatal(ctx, err) {
					return err
				}
				// One entry that cannot be stored is left out; it stays "seen"
				// so the sweep does not mark it removed, and the next read of
				// the calendar tries it again.
				st.Failed++
				a.logLeftOut(source, in.SourceID, "event", 1, err)
				continue
			}
			switch n {
			case "new":
				st.New++
			case "changed":
				st.Changed++
			}
		}
		return nil
	})
	return st, err
}

// sweepRemoved marks the entries of a calendar that lie inside the window
// and were not seen in a complete read of it.
func (a *app) sweepRemoved(ctx context.Context, runID int64, calKey string, w agendaWindow, seen map[string]bool) (int, error) {
	now := a.now().Unix()
	removed := 0
	err := a.store.Update(ctx, func(tx *repo.Tx) error {
		removed = 0
		gone, err := tx.EventsOverlapping(ctx, calKey, w.From.Unix(), w.To.Unix())
		if err != nil {
			return err
		}
		for _, ex := range gone {
			if seen[ex.SourceID] {
				continue
			}
			if err := a.markRemoved(ctx, tx, ex, runID, now); err != nil {
				return err
			}
			removed++
		}
		return nil
	})
	return removed, err
}

// upsertEvent stores one entry; it says "new", "changed" or "" (nothing to write).
func (a *app) upsertEvent(ctx context.Context, tx *repo.Tx, source, calKey, calName string, in calInstance, runID, now int64) (string, error) {
	row := rowFrom(source, calKey, calName, in, now)
	ex, found := tx.AgendaBySource(ctx, source, in.SourceID)
	if !found {
		id, err := tx.InsertAgenda(ctx, row)
		if err != nil {
			return "", err
		}
		if err := tx.InsertAgendaVersion(ctx, id, 1, row.Hash, in.Payload, now, runID); err != nil {
			return "", err
		}
		return "new", tx.InsertAgendaEvent(ctx, now, 0, id, "system", "agenda_posted", map[string]any{"source": source, "version": 1, "status": row.Status})
	}
	if ex.Hash == row.Hash && ex.Status == row.Status {
		return "", nil // the same copy again: nothing is written
	}
	row.ID, row.Version, row.CreatedAt = ex.ID, ex.Version+1, ex.CreatedAt
	if err := tx.UpdateAgendaEvent(ctx, row); err != nil {
		return "", err
	}
	if err := tx.InsertAgendaVersion(ctx, ex.ID, row.Version, row.Hash, in.Payload, now, runID); err != nil {
		return "", err
	}
	if err := tx.InsertAgendaEvent(ctx, now, 0, ex.ID, "system", "agenda_changed", map[string]any{"source": source, "version": row.Version, "status": row.Status}); err != nil {
		return "", err
	}
	// SPEC 33: the thread of the entry, if it has one, hears what changed.
	return "changed", a.meetingFollow(ctx, tx, ex, row, now)
}

// markRemoved marks an entry that the calendar no longer holds. It stays.
func (a *app) markRemoved(ctx context.Context, tx *repo.Tx, ex repo.Agenda, runID, now int64) error {
	ex.Status, ex.Version, ex.UpdatedAt = "removed", ex.Version+1, now
	if err := tx.UpdateAgendaEvent(ctx, ex); err != nil {
		return err
	}
	if err := tx.InsertAgendaVersion(ctx, ex.ID, ex.Version, ex.Hash, `{"removed":true}`, now, runID); err != nil {
		return err
	}
	return tx.InsertAgendaEvent(ctx, now, 0, ex.ID, "system", "agenda_changed", map[string]any{"source": ex.Source, "version": ex.Version, "status": "removed"})
}

// markRemovedByID is a calendar saying that an entry was deleted (Graph's
// delta feed). Unknown entries are ignored.
func (a *app) markRemovedByID(ctx context.Context, runID int64, source, sourceID string) (bool, error) {
	now := a.now().Unix()
	done := false
	err := a.store.Update(ctx, func(tx *repo.Tx) error {
		ex, found := tx.AgendaBySource(ctx, source, sourceID)
		if !found || ex.Status == "removed" {
			return nil
		}
		done = true
		return a.markRemoved(ctx, tx, ex, runID, now)
	})
	return done, err
}

// calKeyOf makes the stable name of a calendar from its source and address.
func calKeyOf(source, id string) string {
	sum := sha256.Sum256([]byte(id))
	return source + ":" + hex.EncodeToString(sum[:6])
}

// --- to-dos (SPEC 19) ---

const todoMade = "You made this a to-do."

// cleanTodoText reads the text of a to-do: one line, not empty.
func cleanTodoText(s string) (string, error) {
	s = strings.Join(strings.Fields(strings.ToValidUTF8(s, "")), " ")
	switch {
	case s == "":
		return "", bad("Write what there is to do first.")
	case utf8.RuneCountInString(s) > maxTodoText:
		return "", bad(fmt.Sprintf("A to-do can have at most %d characters.", maxTodoText))
	}
	return s, nil
}

// addTodo is the owner typing a to-do into the box. An empty box adds nothing.
func (a *app) addTodo(ctx context.Context, text string) (added bool, err error) {
	if strings.TrimSpace(text) == "" {
		return false, nil
	}
	text, err = cleanTodoText(text)
	if err != nil {
		return false, err
	}
	now := a.now().Unix()
	err = a.store.Update(ctx, func(tx *repo.Tx) error {
		id, err := tx.InsertAgenda(ctx, repo.Agenda{Kind: "todo", Title: text, State: "open", Status: "confirmed", CreatedBy: "owner", CreatedAt: now, UpdatedAt: now})
		if err != nil {
			return err
		}
		added = true
		return tx.InsertAgendaEvent(ctx, now, 0, id, "owner", "agenda_posted", map[string]any{"kind": "todo"})
	})
	return added, err
}

// todoFromItem is the button "To do" on a row: a to-do that shows the row's
// number and opens it. A row that already has an open to-do gets no second one
// (a second tap, a second device). Closing or resolving the item does not
// touch the to-do, and ticking it off does not touch the item.
func (a *app) todoFromItem(ctx context.Context, itemID int64) (made bool, err error) {
	now := a.now().Unix()
	err = a.store.Update(ctx, func(tx *repo.Tx) error {
		it, ok := tx.ItemByID(ctx, itemID)
		if !ok || it.Hidden {
			return errNoItem
		}
		if _, has := tx.OpenTodoOfItem(ctx, itemID); has {
			return nil
		}
		id, err := tx.InsertAgenda(ctx, repo.Agenda{Kind: "todo", Title: oneLine(it.Subject, maxTodoText), State: "open", Status: "confirmed",
			CreatedBy: "owner", ItemID: itemID, CreatedAt: now, UpdatedAt: now})
		if err != nil {
			return err
		}
		made = true
		return tx.InsertAgendaEvent(ctx, now, itemID, id, "owner", "todo_made", map[string]any{"item": repo.Number(it.Number)})
	})
	return made, err
}

// tickOff is Done on a to-do or on a past event. A second tap does nothing.
func (a *app) tickOff(ctx context.Context, id int64) (bool, error) {
	now := a.now().Unix()
	var did bool
	err := a.store.Update(ctx, func(tx *repo.Tx) error {
		ex, ok := tx.AgendaByID(ctx, id)
		if !ok {
			return &userErr{404, "no such entry"}
		}
		if ex.Kind == "event" && !a.eventEnded(ex, now) {
			return conflict("An event can be ticked off once it is over.")
		}
		d, err := tx.SetAgendaDone(ctx, id, now)
		if err != nil || !d {
			return err
		}
		did = true
		if err := tx.InsertAgendaEvent(ctx, now, ex.ItemID, id, "owner", "agenda_closed", map[string]any{"kind": ex.Kind}); err != nil {
			return err
		}
		if ex.ItemID != 0 {
			if row, ok := tx.ItemByID(ctx, ex.ItemID); ok && row.Kind == repo.ClosedSummaryKind && row.Stage != stageResolved && row.Stage != stageClosed {
				if err := a.settleSummary(ctx, tx, row, now); err != nil { // SPEC 27: ticking the to-do settles the summary
					return err
				}
			}
		}
		// What waited to change a to-do that is ticked off no longer fits.
		return a.withdrawAgendaProposals(ctx, tx, []int64{id}, 0, "the to-do was ticked off", now)
	})
	return did, err
}

// eventEnded says whether an event is over: its end has passed, or for an
// all-day event its last day has.
func (a *app) eventEnded(ex repo.Agenda, now int64) bool {
	if ex.AllDay {
		return ex.EndAt <= now
	}
	end := ex.EndAt
	if end < ex.StartAt {
		end = ex.StartAt
	}
	return end <= now
}

// syncCalendars is the calendars' part of the timer: the Office calendar,
// then iCloud, then Google. A calendar that is not connected, or is paused because its
// sign-in ran out, is skipped; a failure is kept on the run and shown by the
// strip, the dot and the notice (SPEC 24). The log gets only that it failed.
func (a *app) syncCalendars(ctx context.Context) {
	for i, run := range []func(context.Context, bool) error{a.syncOutlookCal, a.syncICloud, a.syncGoogleCal} {
		err := a.tryUnit([]string{"outlook calendar", "icloud calendar", "google calendar"}[i], func() error { return run(ctx, false) })
		if err == nil || errors.Is(err, errCalOff) || errors.Is(err, errNeedsLogin) || errors.Is(err, context.Canceled) {
			continue
		}
		appkit.Logger().Warn("calendar run failed")
	}
}
