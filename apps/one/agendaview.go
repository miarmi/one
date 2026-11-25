package main

import (
	"context"
	"errors"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"lab/apps/one/repo"
	"lab/platform/appkit"
)

// The agenda sidebar (SPEC 18, 19): entries of the connected calendars by
// day, the filters, and the to-dos under them. Every word that comes from a
// calendar is put into the page as text by the template.

const earlierMax = 400 // entries "Show earlier" brings at most

type filterOpt struct{ Value, Label string }

type eventView struct {
	ID         int64
	Time       string // 09:00–10:30, or All day
	Until      string // a multi-day entry: until 22 Nov
	Title      string
	Place      string
	Calendars  string // the calendars it is in, as read by a person
	CalKeys    string // for the filter: keys separated by spaces
	PersonKey  string // me, or p<id>
	Whose      string // empty for the owner's own
	Mark       string // Cancelled, Removed or Declined, empty otherwise
	Tentative  bool   // the owner accepted it only tentatively (SPEC 18)
	Past       bool
	CanDone    bool
	StatusCode string
	StartAt    int64 // for the script that keeps the Now line and the running mark current
	EndAt      int64
	Timed      bool // has a clock time (not an all-day entry)
	Running    bool // under way at the time the page was made
	IsNow      bool // not an entry: the line at the present moment, NowText holds its words
	NowText    string
	// SPEC 33: the thread of the entry. ThreadID opens it (the button Thread);
	// CanOpen shows the button Open thread; Props are the proposals to open one.
	ThreadID int64
	CanOpen  bool
	Props    []agendaPropView
}

type dayView struct {
	Heading string
	Today   bool
	Past    bool
	Events  []eventView // today's also holds the Now line, between the entries before and after it
}

type todoView struct {
	ID      int64
	Text    string
	Due     string // its time or due date, as shown
	Number  string // the row it was made from
	ItemID  int64
	Created string
}

type agendaView struct {
	Days       []dayView
	Todos      []todoView
	Proposals  []agendaPropView // what agents propose about the to-dos, waiting for Approve
	Persons    []filterOpt
	Calendars  []filterOpt
	HasEarlier bool
	Earlier    bool   // the earlier entries are shown now
	Empty      bool   // no entry at all in the window
	Zone       string // the owner's time zone, for the script that keeps the Now line current
	Date       string // today, for the head of the agenda pane on a laptop: Fri 20 Nov
}

// calMeta is what the agenda knows about one calendar.
type calMeta struct {
	Label  string
	Person int64
	Show   bool
}

func (a *app) calendarMeta(ctx context.Context) map[string]calMeta {
	out := map[string]calMeta{graphCalKey: {Label: graphCalLabel, Show: true}}
	for _, c := range a.icloudCals(ctx) {
		out[c.Key] = calMeta{Label: c.label(), Person: c.Person, Show: c.Show}
	}
	for _, c := range a.googleCals(ctx) {
		out[c.Key] = calMeta{Label: c.label(), Person: c.Person, Show: c.Show}
	}
	return out
}

func personKey(id int64) string {
	if id == 0 {
		return "me"
	}
	return "p" + strconv.FormatInt(id, 10)
}

// rank orders the copies of one event: a confirmed one beats a cancelled one,
// which beats one that was removed.
func rank(status string) int {
	switch status {
	case "confirmed":
		return 0
	case "cancelled":
		return 1
	}
	return 2
}

type merged struct {
	row     repo.Agenda
	all     []repo.Agenda // every copy of the event, for what only one of them says
	cals    []string      // labels
	calKeys []string
	person  int64
}

// mergeCopies shows one event that sits in two calendars once, naming both
// calendars. Both copies stay stored; only the way they are shown is merged.
// Only entries with the same id of the event and the same start are one.
func mergeCopies(rows []repo.Agenda, meta map[string]calMeta) []*merged {
	var out []*merged
	byKey := map[string]*merged{}
	for _, r := range rows {
		m, ok := meta[r.CalKey]
		if !ok || !m.Show {
			continue
		}
		key := ""
		if r.UID != "" {
			key = r.UID + "\x1f" + strconv.FormatInt(r.StartAt, 10) + "\x1f" + strconv.FormatInt(r.EndAt, 10)
		}
		if g, dup := byKey[key]; dup && key != "" {
			g.all = append(g.all, r)
			if rank(r.Status) < rank(g.row.Status) {
				g.row = r
			}
			g.cals = append(g.cals, m.Label)
			g.calKeys = append(g.calKeys, r.CalKey)
			if g.person == 0 {
				g.person = m.Person
			}
			continue
		}
		g := &merged{row: r, all: []repo.Agenda{r}, cals: []string{m.Label}, calKeys: []string{r.CalKey}, person: m.Person}
		if key != "" {
			byKey[key] = g
		}
		out = append(out, g)
	}
	return out
}

func (a *app) dayHeading(date string, today, tomorrow string, nowYear int) string {
	t, err := time.ParseInLocation(dateLayout, date, a.loc)
	if err != nil {
		return date
	}
	layout := "Mon 2 Jan"
	if t.Year() != nowYear {
		layout = "Mon 2 Jan 2006"
	}
	switch date {
	case today:
		return "Today · " + t.Format(layout)
	case tomorrow:
		return "Tomorrow · " + t.Format(layout)
	}
	return t.Format(layout)
}

func (a *app) eventTime(r repo.Agenda) (clock, until string) {
	if r.AllDay {
		if end, err := time.ParseInLocation(dateLayout, r.EndDate, a.loc); err == nil {
			if last := end.AddDate(0, 0, -1); last.Format(dateLayout) > r.StartDate {
				until = "until " + last.Format("2 Jan")
			}
		}
		return "All day", until
	}
	s, e := time.Unix(r.StartAt, 0).In(a.loc), time.Unix(r.EndAt, 0).In(a.loc)
	if r.EndAt <= r.StartAt {
		return s.Format("15:04"), ""
	}
	if e.Format(dateLayout) != s.Format(dateLayout) {
		return s.Format("15:04") + " – " + e.Format("2 Jan 15:04"), ""
	}
	return s.Format("15:04") + "–" + e.Format("15:04"), ""
}

// nowText is the words of the line across the agenda at the present moment.
func (a *app) nowText(now time.Time) string {
	return "Now " + now.In(a.loc).Format("15:04")
}

// running says whether an entry is under way at the time: it has a clock time,
// it began and has not ended, and it is not cancelled or removed.
func running(ev eventView, now int64) bool {
	return ev.Timed && ev.Mark == "" && ev.StartAt <= now && now < ev.EndAt
}

// nowIndex is where the Now line goes among one day's entries, which are in
// order of time with the all-day ones first: after every all-day entry and
// every entry that has begun, before the first that has not.
func nowIndex(evs []eventView, now int64) int {
	for i, ev := range evs {
		if ev.Timed && ev.StartAt > now {
			return i
		}
	}
	return len(evs)
}

// withNowLine puts the Now line into a day's entries.
func withNowLine(evs []eventView, now int64, text string) []eventView {
	i := nowIndex(evs, now)
	out := make([]eventView, 0, len(evs)+1)
	out = append(out, evs[:i]...)
	out = append(out, eventView{IsNow: true, NowText: text, StartAt: now})
	return append(out, evs[i:]...)
}

// agendaViewFor builds the sidebar: the entries inside the window (and the
// earlier ones when asked), grouped by the day they start in the owner's zone.
func (a *app) agendaViewFor(ctx context.Context, earlier bool) (agendaView, error) {
	var v agendaView
	w := a.agendaWindow(ctx)
	meta := a.calendarMeta(ctx)
	rows, err := a.store.EventsInRange(ctx, w.From.Unix(), w.To.Unix())
	if err != nil {
		return v, err
	}
	if earlier {
		before, err := a.store.EventsBefore(ctx, w.From.Unix(), earlierMax)
		if err != nil {
			return v, err
		}
		rows = append(before, rows...)
		v.Earlier = true
	} else {
		v.HasEarlier = a.store.CountEventsBefore(ctx, w.From.Unix()) > 0
	}
	now := a.now()
	v.Date = now.In(a.loc).Format("Mon 2 Jan")
	nowYear := now.In(a.loc).Year()
	today := w.TodayStart.Format(dateLayout)
	tomorrow := w.TodayStart.AddDate(0, 0, 1).Format(dateLayout)
	names := map[int64]string{}
	personName := func(id int64) string {
		if id == 0 {
			return ""
		}
		if n, ok := names[id]; ok {
			return n
		}
		if p, ok := a.store.PersonByID(ctx, id); ok {
			names[id] = p.Shown()
		} else {
			names[id] = "" // a person who is gone: the entries go under Me
		}
		return names[id]
	}
	threads, _ := a.threadMaps(ctx)
	own := a.calendarOwn(ctx)
	props := a.agendaPropViews(ctx)
	attached := map[int64]bool{}
	days := map[string]*dayView{}
	var dayKeys []string
	persons, cals := map[string]string{}, map[string]string{}
	for _, g := range mergeCopies(rows, meta) {
		r := g.row
		date := r.StartDate
		if !r.AllDay {
			date = time.Unix(r.StartAt, 0).In(a.loc).Format(dateLayout)
		}
		d := days[date]
		if d == nil {
			d = &dayView{Heading: a.dayHeading(date, today, tomorrow, nowYear), Today: date == today, Past: date < today}
			days[date] = d
			dayKeys = append(dayKeys, date)
		}
		whose := personName(g.person)
		pk := "me"
		if whose != "" {
			pk = personKey(g.person)
			persons[pk] = whose
		} else {
			persons["me"] = "Me"
		}
		for i, k := range g.calKeys {
			cals[k] = meta[k].Label
			_ = i
		}
		clock, until := a.eventTime(r)
		ev := eventView{ID: r.ID, Time: clock, Until: until, Title: r.Title, Place: r.Place, Calendars: strings.Join(g.cals, ", "),
			CalKeys: strings.Join(g.calKeys, " "), PersonKey: pk, Whose: whose, StatusCode: r.Status,
			Past: a.eventEnded(r, now.Unix()), StartAt: r.StartAt, EndAt: r.EndAt, Timed: !r.AllDay}
		switch r.Status {
		case "cancelled":
			ev.Mark = "Cancelled"
		case "removed":
			ev.Mark = "Removed"
		default:
			switch answerOfGroup(g, own) {
			case answerDeclined:
				ev.Mark = "Declined"
			case answerTentative:
				ev.Tentative = true
			}
		}
		ev.CanDone = ev.Past
		ev.Running = running(ev, now.Unix())
		key := repo.MeetingKey(r)
		if t, ok := threads[key]; ok {
			ev.ThreadID = t[0]
		} else {
			ev.CanOpen = meetingThreads
		}
		for _, p := range props {
			if p.Entry != 0 && p.EntryKey == key && p.EntryStart == r.StartAt && p.EntryEnd == r.EndAt && !attached[p.ID] {
				ev.Props, attached[p.ID] = append(ev.Props, p), true
			}
		}
		d.Events = append(d.Events, ev)
	}
	v.Empty = len(days) == 0
	// Today is always there, even with no entry, because the Now line sits in it.
	if days[today] == nil {
		days[today] = &dayView{Heading: a.dayHeading(today, today, tomorrow, nowYear), Today: true}
		dayKeys = append(dayKeys, today)
	}
	v.Zone = a.loc.String()
	sort.Strings(dayKeys)
	for _, k := range dayKeys {
		d := days[k]
		sort.SliceStable(d.Events, func(i, j int) bool {
			ai, aj := d.Events[i].Time == "All day", d.Events[j].Time == "All day"
			return ai && !aj // all-day entries first; the rest keeps its order by time
		})
		if d.Today {
			d.Events = withNowLine(d.Events, now.Unix(), a.nowText(now))
		}
		v.Days = append(v.Days, *d)
	}
	v.Persons = []filterOpt{{"", "All"}}
	if _, ok := persons["me"]; ok {
		v.Persons = append(v.Persons, filterOpt{"me", "Me"})
	}
	var pkeys []string
	for k := range persons {
		if k != "me" {
			pkeys = append(pkeys, k)
		}
	}
	sort.Slice(pkeys, func(i, j int) bool { return strings.ToLower(persons[pkeys[i]]) < strings.ToLower(persons[pkeys[j]]) })
	for _, k := range pkeys {
		v.Persons = append(v.Persons, filterOpt{k, persons[k]})
	}
	v.Calendars = []filterOpt{{"", "All"}}
	var ckeys []string
	for k := range cals {
		ckeys = append(ckeys, k)
	}
	sort.Slice(ckeys, func(i, j int) bool { return strings.ToLower(cals[ckeys[i]]) < strings.ToLower(cals[ckeys[j]]) })
	for _, k := range ckeys {
		v.Calendars = append(v.Calendars, filterOpt{k, cals[k]})
	}
	todos, err := a.store.OpenTodos(ctx)
	if err != nil {
		return v, err
	}
	for _, t := range todos {
		tv := todoView{ID: t.ID, Text: t.Title, Due: a.dueShown(a.dueOf(t))}
		if t.ItemID != 0 {
			if it, ok := a.store.ItemByID(ctx, t.ItemID); ok {
				tv.Number, tv.ItemID = repo.Number(it.Number), it.ID
			}
		}
		v.Todos = append(v.Todos, tv)
	}
	for _, p := range props {
		if !attached[p.ID] { // a proposal about an entry that is not shown here stays in the list
			v.Proposals = append(v.Proposals, p)
		}
	}
	return v, nil
}

// --- the pages' handlers: only the signed-in page does these ---

func (a *app) agendaFragment(w http.ResponseWriter, r *http.Request, earlier bool, code int) {
	v, err := a.agendaViewFor(r.Context(), earlier)
	if err != nil {
		appkit.Error("building the agenda", err)
		http.Error(w, "The agenda could not be loaded. Reload the page to try again.", http.StatusInternalServerError)
		return
	}
	a.fragment(w, "agenda-body", code, v)
}

func (a *app) agendaGet(w http.ResponseWriter, r *http.Request) {
	a.agendaFragment(w, r, r.URL.Query().Get("earlier") == "1", http.StatusOK)
}

func (a *app) agendaAddTodo(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	// SPEC 16: a to-do typed by hand belongs to no item, so it cannot take a
	// mark from one. The page may name a test run for it, in the header and only
	// in the fixed shape of a run name; any other value is ignored. Nothing else
	// the page does takes a run name from the request: its actions on marked
	// data take the mark of that data.
	ctx := r.Context()
	if run := strings.TrimSpace(r.Header.Get(runHeader)); repo.ValidRun(run) {
		ctx = repo.WithRun(ctx, run)
	}
	_, err := a.addTodo(ctx, r.FormValue("text"))
	var ue *userErr
	if errors.As(err, &ue) {
		http.Error(w, ue.Msg, ue.Status)
		return
	}
	if err != nil {
		appkit.Error("adding a to-do", err)
		http.Error(w, "That did not work. Try again.", http.StatusInternalServerError)
		return
	}
	a.agendaAnswer(w, r)
}

func (a *app) agendaDone(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		http.NotFound(w, r)
		return
	}
	_, err = a.tickOff(r.Context(), id)
	var ue *userErr
	if errors.As(err, &ue) {
		http.Error(w, ue.Msg, ue.Status)
		return
	}
	if err != nil {
		appkit.Error("ticking off an entry", err)
		http.Error(w, "That did not work. Try again.", http.StatusInternalServerError)
		return
	}
	a.agendaAnswer(w, r)
}

// agendaAnswer answers a form of the sidebar: the new sidebar for the script,
// the list page for a plain form post.
func (a *app) agendaAnswer(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("X-Requested-With") != "one" {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	a.agendaFragment(w, r, r.FormValue("earlier") == "1", http.StatusOK)
}

// itemTodo is the button "To do" on a row.
func (a *app) itemTodo(w http.ResponseWriter, r *http.Request) {
	id, ok := itemID(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	if _, err := a.todoFromItem(r.Context(), id); err != nil {
		if errors.Is(err, errNoItem) {
			http.NotFound(w, r)
			return
		}
		appkit.Error("making a to-do", err)
		http.Error(w, "That did not work. Try again.", http.StatusInternalServerError)
		return
	}
	if r.Header.Get("X-Requested-With") != "one" {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	a.rowResponse(w, r, id, r.FormValue("open") == "1", false)
}
