package main

import (
	"sort"
	"strconv"
	"strings"
	"time"
)

// Recurring events (RFC 5545 RRULE) expanded by the app itself, inside the
// window it reads and nowhere else. A repeating event keeps its wall-clock
// time in its own zone, so it stays at 09:00 across a clock change. An
// instance that was moved or cancelled (a component with RECURRENCE-ID), a
// day left out (EXDATE) and a day added (RDATE) are honoured. Not supported,
// and listed as a limit on the Connectors page: RANGE=THISANDFUTURE, and
// rules with BYWEEKNO, BYYEARDAY or BYHOUR and the like (they are read as
// if those parts were absent).

type weekdayRule struct {
	N   int // 0: every such day; 1 first, -1 last
	Day time.Weekday
}

type rrule struct {
	Freq       string
	Interval   int
	Count      int
	Until      time.Time
	HasUntil   bool
	ByDay      []weekdayRule
	ByMonthDay []int
	ByMonth    []int
	BySetPos   []int
	Wkst       time.Weekday
}

var weekdayCodes = map[string]time.Weekday{"SU": time.Sunday, "MO": time.Monday, "TU": time.Tuesday, "WE": time.Wednesday, "TH": time.Thursday, "FR": time.Friday, "SA": time.Saturday}

func ints(s string) []int {
	var out []int
	for _, p := range strings.Split(s, ",") {
		if n, err := strconv.Atoi(strings.TrimSpace(p)); err == nil {
			out = append(out, n)
		}
	}
	return out
}

func parseRRule(s string, loc *time.Location) (rrule, bool) {
	r := rrule{Interval: 1, Wkst: time.Monday}
	for _, part := range strings.Split(s, ";") {
		k, v, ok := strings.Cut(part, "=")
		if !ok {
			continue
		}
		v = strings.TrimSpace(v)
		switch strings.ToUpper(strings.TrimSpace(k)) {
		case "FREQ":
			r.Freq = strings.ToUpper(v)
		case "INTERVAL":
			if n, err := strconv.Atoi(v); err == nil && n > 0 {
				r.Interval = n
			}
		case "COUNT":
			if n, err := strconv.Atoi(v); err == nil && n > 0 {
				r.Count = n
			}
		case "UNTIL":
			t, ok := parseICalTime(icalProp{name: "UNTIL", params: map[string]string{}, value: v}, loc)
			if ok {
				if t.Date {
					t.T = t.T.AddDate(0, 0, 1).Add(-time.Second) // the whole last day
				}
				r.Until, r.HasUntil = t.T, true
			}
		case "BYDAY":
			for _, p := range strings.Split(v, ",") {
				p = strings.ToUpper(strings.TrimSpace(p))
				if len(p) < 2 {
					continue
				}
				day, ok := weekdayCodes[p[len(p)-2:]]
				if !ok {
					continue
				}
				n := 0
				if len(p) > 2 {
					n, _ = strconv.Atoi(p[:len(p)-2])
				}
				r.ByDay = append(r.ByDay, weekdayRule{n, day})
			}
		case "BYMONTHDAY":
			r.ByMonthDay = ints(v)
		case "BYMONTH":
			r.ByMonth = ints(v)
		case "BYSETPOS":
			r.BySetPos = ints(v)
		case "WKST":
			if d, ok := weekdayCodes[strings.ToUpper(v)]; ok {
				r.Wkst = d
			}
		}
	}
	switch r.Freq {
	case "DAILY", "WEEKLY", "MONTHLY", "YEARLY":
		return r, true
	}
	return r, false
}

func daysIn(y int, m time.Month) int {
	return time.Date(y, m+1, 0, 0, 0, 0, 0, time.UTC).Day()
}

func inInts(list []int, n int) bool {
	for _, x := range list {
		if x == n {
			return true
		}
	}
	return false
}

func (r rrule) hasWeekday(d time.Weekday) bool {
	for _, b := range r.ByDay {
		if b.Day == d {
			return true
		}
	}
	return false
}

// dayOK is the filter of the parts that only narrow a day down.
func (r rrule) dayOK(y int, m time.Month, d int) bool {
	if len(r.ByMonth) > 0 && !inInts(r.ByMonth, int(m)) {
		return false
	}
	if len(r.ByMonthDay) > 0 {
		n := daysIn(y, m)
		ok := false
		for _, x := range r.ByMonthDay {
			if x == d || (x < 0 && n+x+1 == d) {
				ok = true
			}
		}
		if !ok {
			return false
		}
	}
	if len(r.ByDay) > 0 {
		wd := time.Date(y, m, d, 0, 0, 0, 0, time.UTC).Weekday()
		if !r.hasWeekday(wd) {
			return false
		}
	}
	return true
}

// monthDays are the days of one month a monthly or yearly rule picks.
func (r rrule) monthDays(y int, m time.Month, startDay int) []int {
	n := daysIn(y, m)
	var days []int
	switch {
	case len(r.ByMonthDay) > 0:
		for d := 1; d <= n; d++ {
			if !r.dayOK(y, m, d) {
				continue
			}
			days = append(days, d)
		}
	case len(r.ByDay) > 0:
		for _, b := range r.ByDay {
			var same []int
			for d := 1; d <= n; d++ {
				if time.Date(y, m, d, 0, 0, 0, 0, time.UTC).Weekday() == b.Day {
					same = append(same, d)
				}
			}
			switch {
			case b.N == 0:
				days = append(days, same...)
			case b.N > 0 && b.N <= len(same):
				days = append(days, same[b.N-1])
			case b.N < 0 && -b.N <= len(same):
				days = append(days, same[len(same)+b.N])
			}
		}
		sort.Ints(days)
	default:
		if startDay <= n {
			days = []int{startDay}
		}
	}
	return days
}

func setPos(days []time.Time, pos []int) []time.Time {
	if len(pos) == 0 {
		return days
	}
	var out []time.Time
	for _, p := range pos {
		switch {
		case p > 0 && p <= len(days):
			out = append(out, days[p-1])
		case p < 0 && -p <= len(days):
			out = append(out, days[len(days)+p])
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Before(out[j]) })
	return out
}

const maxPeriods = 200000

// occurrences lists the starts of a rule from the first one up to limit,
// at most as many as COUNT allows, in the zone and at the time of day of
// start. The first start is always one of them.
func (r rrule) occurrences(start, limit time.Time) []time.Time {
	loc := start.Location()
	h, mi, s := start.Clock()
	at := func(y int, mo time.Month, d int) time.Time { return time.Date(y, mo, d, h, mi, s, 0, loc) }
	out := []time.Time{start}
	n := 1
	done := func() bool { return r.Count > 0 && n >= r.Count }
	// take adds the candidates of one period; false ends the walk.
	take := func(c []time.Time) bool {
		for _, t := range c {
			if !t.After(start) {
				continue
			}
			if r.HasUntil && t.After(r.Until) {
				return false
			}
			if t.After(limit) || done() {
				return false
			}
			out = append(out, t)
			n++
		}
		return !done()
	}
	if r.HasUntil && start.After(r.Until) {
		return out
	}
	y0, m0, d0 := start.Date()
	for i := 0; i < maxPeriods && !done(); i++ {
		var cands []time.Time
		var periodStart time.Time
		switch r.Freq {
		case "DAILY":
			d := time.Date(y0, m0, d0+i*r.Interval, 0, 0, 0, 0, loc)
			periodStart = d
			y, m, dd := d.Date()
			if r.dayOK(y, m, dd) {
				cands = []time.Time{at(y, m, dd)}
			}
		case "WEEKLY":
			back := (int(start.Weekday()) - int(r.Wkst) + 7) % 7
			ws := time.Date(y0, m0, d0-back+7*r.Interval*i, 0, 0, 0, 0, loc)
			periodStart = ws
			for k := 0; k < 7; k++ {
				y, m, dd := time.Date(ws.Year(), ws.Month(), ws.Day()+k, 0, 0, 0, 0, loc).Date()
				var ok bool
				if len(r.ByDay) > 0 {
					ok = r.hasWeekday(time.Date(y, m, dd, 0, 0, 0, 0, time.UTC).Weekday())
				} else {
					ok = time.Date(y, m, dd, 0, 0, 0, 0, time.UTC).Weekday() == start.Weekday()
				}
				if ok && (len(r.ByMonth) == 0 || inInts(r.ByMonth, int(m))) {
					cands = append(cands, at(y, m, dd))
				}
			}
			cands = setPos(cands, r.BySetPos)
		case "MONTHLY":
			first := time.Date(y0, m0+time.Month(i*r.Interval), 1, 0, 0, 0, 0, loc)
			periodStart = first
			y, m := first.Year(), first.Month()
			if len(r.ByMonth) > 0 && !inInts(r.ByMonth, int(m)) {
				break
			}
			for _, d := range r.monthDays(y, m, d0) {
				cands = append(cands, at(y, m, d))
			}
			cands = setPos(cands, r.BySetPos)
		case "YEARLY":
			y := y0 + i*r.Interval
			periodStart = time.Date(y, 1, 1, 0, 0, 0, 0, loc)
			months := r.ByMonth
			if len(months) == 0 {
				months = []int{int(m0)}
			}
			sort.Ints(months)
			for _, mo := range months {
				if mo < 1 || mo > 12 {
					continue
				}
				for _, d := range r.monthDays(y, time.Month(mo), d0) {
					cands = append(cands, at(y, time.Month(mo), d))
				}
			}
			cands = setPos(cands, r.BySetPos)
		default:
			return out
		}
		if periodStart.After(limit) {
			break
		}
		if !take(cands) {
			break
		}
	}
	return out
}

// --- the instances of one event (one UID) inside a window ---

// calInstance is one entry the calendar holds inside the window: a single
// event, or one occurrence of a repeating one.
type calInstance struct {
	SourceID  string // the identity in the source, set by the reader
	UID       string
	Instance  string // "" for a single event; the original start of an occurrence
	Title     string
	Place     string
	Note      string
	Answers   string
	Start     time.Time // UTC
	End       time.Time
	AllDay    bool
	StartDate string // all-day: the first day
	EndDate   string // all-day: the day after the last day
	Status    string // confirmed or cancelled
	Payload   string // what the calendar delivered, kept with the version
}

const dateLayout = "2006-01-02"

// instanceKey names an occurrence by its original start.
func instanceKey(t icalTime) string {
	if t.Date {
		return t.T.Format("20060102")
	}
	return t.T.UTC().Format("20060102T150405Z")
}

// dayStart is midnight of a date in the zone, as an instant.
func dayStart(date string, loc *time.Location) time.Time {
	t, err := time.ParseInLocation(dateLayout, date, loc)
	if err != nil {
		return time.Time{}
	}
	return t.UTC()
}

func clip(s string, n int) string {
	r := []rune(strings.TrimSpace(s))
	if len(r) > n {
		return string(r[:n])
	}
	return string(r)
}

// instanceOf turns one component and the start it takes place at into an entry.
func instanceOf(ev vevent, start icalTime, loc *time.Location, payload, key string) calInstance {
	in := calInstance{UID: ev.UID, Instance: key, Title: clip(ev.Summary, 300), Place: clip(ev.Location, 300), Note: clip(ev.Description, 2000),
		Answers: strings.Join(ev.Answers, ";"), Status: "confirmed", Payload: payload}
	if ev.Status == "CANCELLED" {
		in.Status = "cancelled"
	}
	if start.Date {
		in.AllDay = true
		in.StartDate = start.T.In(loc).Format(dateLayout)
		days := 1
		switch {
		case ev.HasEnd && ev.End.Date:
			if d := int(ev.End.T.Sub(ev.Start.T).Round(24*time.Hour) / (24 * time.Hour)); d > 0 {
				days = d
			}
		case ev.HasDuration && ev.Duration >= 24*time.Hour:
			days = int(ev.Duration / (24 * time.Hour))
		}
		in.EndDate = start.T.In(loc).AddDate(0, 0, days).Format(dateLayout)
		in.Start, in.End = dayStart(in.StartDate, loc), dayStart(in.EndDate, loc)
		return in
	}
	in.Start = start.T.UTC()
	switch {
	case ev.HasEnd && !ev.End.Date:
		in.End = in.Start.Add(ev.End.T.Sub(ev.Start.T))
	case ev.HasDuration:
		in.End = in.Start.Add(ev.Duration)
	default:
		in.End = in.Start
	}
	if in.End.Before(in.Start) {
		in.End = in.Start
	}
	return in
}

func overlaps(in calInstance, w0, w1 time.Time) bool {
	if !in.Start.Before(w1) {
		return false
	}
	return in.End.After(w0) || (!in.End.After(in.Start) && !in.Start.Before(w0))
}

// expandEvents turns the components of one event (its main component and
// any that change single occurrences) into the entries that fall inside the
// window [w0, w1). payload is kept with each entry's version.
func expandEvents(evs []vevent, w0, w1 time.Time, loc *time.Location, payload string) []calInstance {
	var master *vevent
	overrides := map[string]vevent{}
	for i := range evs {
		ev := evs[i]
		switch {
		case ev.HasRecurID:
			overrides[instanceKey(ev.RecurID)] = ev
		case master == nil:
			master = &evs[i]
		}
	}
	var out []calInstance
	used := map[string]bool{}
	if master != nil {
		recurring := master.RRule != "" || len(master.RDates) > 0
		if !recurring {
			in := instanceOf(*master, master.Start, loc, payload, "")
			if overlaps(in, w0, w1) {
				out = append(out, in)
			}
		} else {
			var starts []icalTime
			if rule, ok := parseRRule(master.RRule, master.Start.Loc); ok {
				for _, t := range rule.occurrences(master.Start.T, w1) {
					starts = append(starts, icalTime{T: t, Date: master.Start.Date, Loc: master.Start.Loc})
				}
			} else {
				starts = append(starts, master.Start)
			}
			for _, rd := range master.RDates {
				starts = append(starts, icalTime{T: rd.T, Date: master.Start.Date, Loc: master.Start.Loc})
			}
			gone := map[string]bool{}
			for _, ex := range master.ExDates {
				gone[instanceKey(ex)] = true
			}
			seen := map[string]bool{}
			for _, st := range starts {
				key := instanceKey(st)
				if gone[key] || seen[key] {
					continue
				}
				seen[key] = true
				var in calInstance
				if ov, ok := overrides[key]; ok {
					used[key] = true
					in = instanceOf(ov, ov.Start, loc, payload, key)
				} else {
					// the other occurrences are as long as the main one
					in = instanceOf(*master, st, loc, payload, key)
				}
				if overlaps(in, w0, w1) {
					out = append(out, in)
				}
			}
		}
	}
	for key, ov := range overrides {
		if used[key] {
			continue
		}
		in := instanceOf(ov, ov.Start, loc, payload, key)
		if overlaps(in, w0, w1) {
			out = append(out, in)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if !out[i].Start.Equal(out[j].Start) {
			return out[i].Start.Before(out[j].Start)
		}
		return out[i].Instance < out[j].Instance
	})
	return out
}
