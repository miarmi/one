package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"lab/platform/appkit"
)

// SPEC 15, 23: the Office calendar, read through the Microsoft sign-in
// that already reads the mail. The sign-in already holds Calendars.Read;
// nothing more is asked for. It only reads: every call is a GET (graphGet),
// and there is no code in the app that creates, answers or deletes an event.
//
// Changes are followed with Graph's delta feed of the calendar view: the
// first round (and a round after the window has moved to another day, or
// after Graph says the position is gone) reads the whole window, later rounds
// read only what changed, with @removed for what was deleted. The position
// (Graph's delta link, which holds no secret) is kept in the settings.

const (
	graphCalKey      = "outlookcal:main"
	graphCalLabel    = "Outlook · Office"
	graphCalStateKey = "sync.outlookcal"
	outlookCalConn   = "outlookcal"
	graphCalPage     = 50
)

// graphCalState is where the calendar stands: which window the position is
// for, and Graph's delta link.
type graphCalState struct {
	From  int64  `json:"from"`
	To    int64  `json:"to"`
	Delta string `json:"delta,omitempty"`
}

type graphCalWhen struct {
	DateTime string `json:"dateTime"`
	TimeZone string `json:"timeZone"`
}

// graphCalEvent is what the calendar view says about one event or occurrence.
type graphCalEvent struct {
	ID      string `json:"id"`
	Removed *struct {
		Reason string `json:"reason"`
	} `json:"@removed"`
	ICalUID     string       `json:"iCalUId"`
	Subject     string       `json:"subject"`
	BodyPreview string       `json:"bodyPreview"`
	IsAllDay    bool         `json:"isAllDay"`
	IsCancelled bool         `json:"isCancelled"`
	Start       graphCalWhen `json:"start"`
	End         graphCalWhen `json:"end"`
	Location    struct {
		DisplayName string `json:"displayName"`
	} `json:"location"`
	Attendees []struct {
		Status struct {
			Response string `json:"response"`
		} `json:"status"`
		EmailAddress struct {
			Address string `json:"address"`
		} `json:"emailAddress"`
	} `json:"attendees"`
}

type graphCalPageData struct {
	Value []json.RawMessage `json:"value"`
	Next  string            `json:"@odata.nextLink"`
	Delta string            `json:"@odata.deltaLink"`
}

// graphGet is the only call this file makes to Graph: a GET, through the
// importer's way of renewing a refused token and waiting out a throttle.
func (a *app) graphGet(ctx context.Context, path string, out any) error {
	return a.gimp.do(ctx, func(token string) error {
		return a.graph.apiPrefer(ctx, token, "GET", path, append(graphImmutable, fmt.Sprintf("odata.maxpagesize=%d", graphCalPage)), nil, out)
	})
}

func graphWhen(w graphCalWhen) (time.Time, bool) {
	if len(w.DateTime) < 19 {
		return time.Time{}, false
	}
	loc := time.UTC
	if w.TimeZone != "" && !strings.EqualFold(w.TimeZone, "UTC") {
		if z, ok := zoneNamed(w.TimeZone); ok {
			loc = z
		}
	}
	t, err := time.ParseInLocation("2006-01-02T15:04:05", w.DateTime[:19], loc)
	return t.UTC(), err == nil
}

// allDayDate is the day an all-day value stands for in the owner's zone. Graph
// gives an all-day event as midnight in some zone; rounding to the nearest
// midnight of the owner's zone gives the right day whichever zone it was.
func allDayDate(t time.Time, loc *time.Location) string {
	l := t.In(loc)
	if l.Hour() >= 12 {
		l = l.AddDate(0, 0, 1)
	}
	return l.Format(dateLayout)
}

// instanceOfGraph turns one event of the calendar view into an entry.
func instanceOfGraph(ev graphCalEvent, raw json.RawMessage, calKey string, loc *time.Location) (calInstance, bool) {
	s, ok1 := graphWhen(ev.Start)
	e, ok2 := graphWhen(ev.End)
	if !ok1 || ev.ID == "" {
		return calInstance{}, false
	}
	if !ok2 || e.Before(s) {
		e = s
	}
	uid := ev.ICalUID
	if uid == "" {
		uid = ev.ID
	}
	in := calInstance{SourceID: calKey + "\x1f" + ev.ID, UID: uid, Title: clip(ev.Subject, 300), Place: clip(ev.Location.DisplayName, 300),
		Note: clip(ev.BodyPreview, 2000), Status: "confirmed", Payload: clipPayload(string(raw))}
	if ev.IsCancelled {
		in.Status = "cancelled"
	}
	var answers []string
	for _, at := range ev.Attendees {
		answers = append(answers, strings.ToLower(at.EmailAddress.Address)+"="+strings.ToUpper(at.Status.Response))
	}
	in.Answers = strings.Join(sortedStrings(answers), ";")
	if ev.IsAllDay {
		in.AllDay = true
		in.StartDate = allDayDate(s, loc)
		in.EndDate = allDayDate(e, loc)
		if in.EndDate <= in.StartDate {
			d, _ := time.ParseInLocation(dateLayout, in.StartDate, loc)
			in.EndDate = d.AddDate(0, 0, 1).Format(dateLayout)
		}
		in.Start, in.End = dayStart(in.StartDate, loc), dayStart(in.EndDate, loc)
		return in, true
	}
	in.Start, in.End = s, e
	return in, true
}

func sortedStrings(in []string) []string {
	out := append([]string(nil), in...)
	sort.Strings(out)
	return out
}

func (a *app) graphCalState(ctx context.Context) graphCalState {
	var st graphCalState
	json.Unmarshal([]byte(a.store.Setting(ctx, graphCalStateKey)), &st)
	return st
}

func (a *app) saveGraphCalState(ctx context.Context, st graphCalState) error {
	b, _ := json.Marshal(st)
	if a.store.Setting(ctx, graphCalStateKey) == string(b) {
		return nil
	}
	return a.store.SetSettingBy(context.WithoutCancel(ctx), graphCalStateKey, string(b), "system", a.now())
}

func graphViewPath(w agendaWindow) string {
	q := url.Values{}
	q.Set("startDateTime", w.From.Format("2006-01-02T15:04:05Z"))
	q.Set("endDateTime", w.To.Format("2006-01-02T15:04:05Z"))
	return "/me/calendarView/delta?" + q.Encode()
}

// plainOutlookCal words a failed read for the page; it never holds a token.
func plainOutlookCal(err error) string {
	switch {
	case errors.Is(err, errNeedsLogin):
		return "The Microsoft sign-in has run out. Sign in again; the calendar is read again by itself."
	case errors.Is(err, errCalOff):
		return "Outlook Calendar is not signed in."
	}
	var me *msError
	if errors.As(err, &me) && me.Status == 429 {
		return "Microsoft asked ONE to slow down and did not let up; the calendar is read again at the next read."
	}
	return plainMS("Outlook Calendar", err)
}

// syncOutlookCal reads the calendar once. force reads even while the sign-in
// is marked as run out (Test now).
func (a *app) syncOutlookCal(ctx context.Context, force bool) (err error) {
	a.outlookCalMu.Lock()
	defer a.outlookCalMu.Unlock()
	if !a.graphSigned() {
		return errCalOff
	}
	if !force && a.graphNeedsLogin(ctx) {
		return errNeedsLogin
	}
	core := a.imp.core
	run, err := core.startRun(ctx, outlookCalSrc, "sync")
	if err != nil {
		return err
	}
	defer func() {
		core.endRun(ctx, outlookCalSrc, run, err, plainOutlookCal)
		switch {
		case err == nil:
			a.setGraphNeedsLogin(ctx, false)
		case errors.Is(err, errNeedsLogin):
			a.setGraphNeedsLogin(ctx, true)
		}
		a.checkSource(ctx, outlookCalSrc)
		if errors.Is(err, errNeedsLogin) {
			a.checkSource(ctx, graphSrc) // one sign-in, one notice: the mail's
		}
	}()
	rctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	w := a.agendaWindow(ctx)
	var total storeStats
	for attempt := 0; attempt < 2; attempt++ {
		st := a.graphCalState(ctx)
		full := st.Delta == "" || st.From != w.From.Unix() || st.To != w.To.Unix()
		path := graphViewPath(w)
		if !full {
			if path, err = a.graph.pathOfLink(st.Delta); err != nil {
				return err
			}
		}
		seen := map[string]bool{}
		total = storeStats{}
		var delta string
		for path != "" {
			var page graphCalPageData
			if err = a.graphGet(rctx, path, &page); err != nil {
				var me *msError
				if errors.As(err, &me) && me.Status == 410 && !full {
					// Graph no longer knows the position: read the whole window again.
					a.saveGraphCalState(ctx, graphCalState{})
					err = nil
					break
				}
				return err
			}
			var ins []calInstance
			var gone []string
			for _, raw := range page.Value {
				var ev graphCalEvent
				if json.Unmarshal(raw, &ev) != nil {
					continue
				}
				if ev.Removed != nil {
					gone = append(gone, graphCalKey+"\x1f"+ev.ID)
					continue
				}
				if in, ok := instanceOfGraph(ev, raw, graphCalKey, a.loc); ok {
					ins = append(ins, in)
				}
			}
			stats, serr := a.storeInstances(rctx, run.ID, srcOutlookCal, graphCalKey, graphCalLabel, ins, seen)
			if serr != nil {
				return serr
			}
			total.add(stats)
			for _, id := range gone {
				if done, derr := a.markRemovedByID(rctx, run.ID, srcOutlookCal, id); derr != nil {
					return derr
				} else if done {
					total.Removed++
				}
			}
			path, delta = "", page.Delta
			if page.Next != "" {
				if path, err = a.graph.pathOfLink(page.Next); err != nil {
					return err
				}
			}
		}
		if delta == "" {
			if err != nil {
				return err
			}
			continue // the position was gone: go round again as a full read
		}
		if full {
			n, serr := a.sweepRemoved(rctx, run.ID, graphCalKey, w, seen)
			if serr != nil {
				return serr
			}
			total.Removed += n
		}
		run.Fetched, run.Stored, run.ItemsMade, run.Failed = total.Read, total.stored(), total.New, total.Failed
		if a.calRetry(graphCalKey, total.Failed) {
			// An entry failed: the position is not kept, so the next round reads
			// the whole window again and meets it once more.
			return a.saveGraphCalState(ctx, graphCalState{})
		}
		return a.saveGraphCalState(ctx, graphCalState{From: w.From.Unix(), To: w.To.Unix(), Delta: delta})
	}
	return errors.New("Graph did not give a position after two tries")
}

// --- the section of the Connectors page ---

type outlookCalView struct {
	sectionView
	Signed bool
	Limits []string
}

func (a *app) outlookCalViewFor(ctx context.Context, notice, errText string) outlookCalView {
	v := outlookCalView{sectionView: sectionView{Notice: notice, Error: errText}, Limits: append([]string{
		"Only the main calendar of the Office mailbox is read, not other calendars of the account.",
	}, calendarLimits...)}
	v.Signed = a.graphSigned()
	v.Test = a.lastView(ctx, outlookCalConn)
	if !v.Signed {
		v.Status, v.StatusCls = statusNotConf, "st-stale"
		return v
	}
	v.Status, v.StatusCls = status(true, v.Test)
	v.applyState(a.sourceStateOf(ctx, outlookCalSrc), v.Test)
	return v
}

// outlookCalTest is Test now: it reads now and says what came of it.
func (a *app) outlookCalTest(w http.ResponseWriter, r *http.Request) {
	if !a.graphSigned() {
		a.renderConnectors(w, r, http.StatusBadRequest, "outlookcal", "", "Sign in to Office mail first; the calendar uses the same sign-in.")
		return
	}
	ctx := r.Context()
	a.opMu.Lock()
	defer a.opMu.Unlock()
	if err := a.syncOutlookCal(ctx, true); err != nil {
		appkit.Logger().Warn("outlook calendar test failed")
		a.record(ctx, outlookCalConn, false, "Could not read: "+plainOutlookCal(err))
	} else {
		a.record(ctx, outlookCalConn, true, fmt.Sprintf("Read the calendar. %d entries in the agenda now.", a.store.CountEvents(ctx, srcOutlookCal)))
	}
	http.Redirect(w, r, "/connectors#outlookcal", http.StatusSeeOther)
}
