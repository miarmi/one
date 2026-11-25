package main

import (
	"sort"
	"strconv"
	"strings"
	"time"
	_ "time/tzdata" // zone names work without system zoneinfo
)

// A reader for iCalendar text (RFC 5545), carried over from the CalDAV
// reader of queue v1 and widened for what the agenda needs: recurrence,
// moved and cancelled instances, place, notes and the guests' answers. It
// reads text; it builds nothing that is sent anywhere.

type icalProp struct {
	name   string
	params map[string]string
	value  string
}

func unfoldICal(s string) []string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	var out []string
	for _, l := range strings.Split(s, "\n") {
		if len(l) > 0 && (l[0] == ' ' || l[0] == '\t') && len(out) > 0 {
			out[len(out)-1] += l[1:]
			continue
		}
		out = append(out, l)
	}
	return out
}

// splitOutsideQuotes splits s at sep, ignoring separators inside double quotes.
func splitOutsideQuotes(s string, sep byte) []string {
	var parts []string
	inQ, last := false, 0
	for i := 0; i < len(s); i++ {
		switch {
		case s[i] == '"':
			inQ = !inQ
		case s[i] == sep && !inQ:
			parts = append(parts, s[last:i])
			last = i + 1
		}
	}
	return append(parts, s[last:])
}

func parseICalLine(line string) (icalProp, bool) {
	inQ, colon := false, -1
	for i := 0; i < len(line); i++ {
		if line[i] == '"' {
			inQ = !inQ
		} else if line[i] == ':' && !inQ {
			colon = i
			break
		}
	}
	if colon < 0 {
		return icalProp{}, false
	}
	head := splitOutsideQuotes(line[:colon], ';')
	p := icalProp{name: strings.ToUpper(strings.TrimSpace(head[0])), value: line[colon+1:], params: map[string]string{}}
	for _, kv := range head[1:] {
		if k, v, ok := strings.Cut(kv, "="); ok {
			p.params[strings.ToUpper(strings.TrimSpace(k))] = strings.Trim(v, `"`)
		}
	}
	return p, true
}

func unescapeICalText(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+1 < len(s) {
			i++
			switch s[i] {
			case 'n', 'N':
				b.WriteByte('\n')
			default:
				b.WriteByte(s[i])
			}
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// windowsZones are the names of Microsoft's zones that turn up in calendars
// that came from Outlook.
var windowsZones = map[string]string{
	"w. europe standard time":        "Europe/Berlin",
	"central europe standard time":   "Europe/Budapest",
	"central european standard time": "Europe/Warsaw",
	"romance standard time":          "Europe/Paris",
	"gmt standard time":              "Europe/London",
	"greenwich standard time":        "Atlantic/Reykjavik",
	"utc":                            "UTC",
	"eastern standard time":          "America/New_York",
	"central standard time":          "America/Chicago",
	"pacific standard time":          "America/Los_Angeles",
}

// zoneNamed finds a zone by its name; ok is false for a name nobody knows.
func zoneNamed(name string) (*time.Location, bool) {
	name = strings.Trim(strings.TrimSpace(name), "/")
	if name == "" {
		return nil, false
	}
	if l, err := time.LoadLocation(name); err == nil {
		return l, true
	}
	if w, ok := windowsZones[strings.ToLower(name)]; ok {
		if l, err := time.LoadLocation(w); err == nil {
			return l, true
		}
	}
	return nil, false
}

// icalTime is one date or date-time of a calendar.
type icalTime struct {
	T        time.Time
	Date     bool           // a date without a time: an all-day value
	Loc      *time.Location // the zone it was written in (UTC for a Z time)
	Floating bool
}

func parseICalTime(p icalProp, def *time.Location) (icalTime, bool) {
	v := strings.TrimSpace(p.value)
	if strings.EqualFold(p.params["VALUE"], "DATE") || len(v) == 8 {
		t, err := time.ParseInLocation("20060102", v, def)
		return icalTime{T: t, Date: true, Loc: def}, err == nil
	}
	if strings.HasSuffix(v, "Z") || strings.HasSuffix(v, "z") {
		t, err := time.Parse("20060102T150405Z", strings.ToUpper(v))
		return icalTime{T: t, Loc: time.UTC}, err == nil
	}
	loc, floating := def, true
	if tz := p.params["TZID"]; tz != "" {
		if l, ok := zoneNamed(tz); ok {
			loc, floating = l, false
		}
	}
	t, err := time.ParseInLocation("20060102T150405", v, loc)
	return icalTime{T: t, Loc: loc, Floating: floating}, err == nil
}

// parseICalDuration handles [+-]P nW | nD T nH nM nS.
func parseICalDuration(s string) (time.Duration, bool) {
	s = strings.ToUpper(strings.TrimSpace(s))
	sign := time.Duration(1)
	if strings.HasPrefix(s, "-") {
		sign, s = -1, s[1:]
	} else if strings.HasPrefix(s, "+") {
		s = s[1:]
	}
	if !strings.HasPrefix(s, "P") || len(s) < 2 {
		return 0, false
	}
	s = s[1:]
	var total time.Duration
	inTime := false
	num := ""
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9':
			num += string(r)
		case r == 'T':
			inTime = true
		default:
			n, err := strconv.Atoi(num)
			if err != nil {
				return 0, false
			}
			num = ""
			var unit time.Duration
			switch {
			case r == 'W' && !inTime:
				unit = 7 * 24 * time.Hour
			case r == 'D' && !inTime:
				unit = 24 * time.Hour
			case r == 'H' && inTime:
				unit = time.Hour
			case r == 'M' && inTime:
				unit = time.Minute
			case r == 'S' && inTime:
				unit = time.Second
			default:
				return 0, false
			}
			total += time.Duration(n) * unit
		}
	}
	if num != "" {
		return 0, false
	}
	return sign * total, true
}

// vevent is one VEVENT component as written.
type vevent struct {
	UID         string
	Summary     string
	Location    string
	Description string
	Status      string // upper case
	Start       icalTime
	End         icalTime
	HasEnd      bool
	Duration    time.Duration
	HasDuration bool
	RecurID     icalTime
	HasRecurID  bool
	RRule       string
	ExDates     []icalTime
	RDates      []icalTime
	Answers     []string // "address=PARTSTAT", sorted
}

func countVEvent(stack []string) int {
	n := 0
	for _, s := range stack {
		if s == "VEVENT" {
			n++
		}
	}
	return n
}

// parseVEvents returns every VEVENT of an iCalendar text that has a start.
// Alarms inside an event are left out.
func parseVEvents(data string, def *time.Location) []vevent {
	var (
		out   []vevent
		stack []string
		props []icalProp
	)
	for _, line := range unfoldICal(data) {
		p, ok := parseICalLine(line)
		if !ok {
			continue
		}
		switch p.name {
		case "BEGIN":
			name := strings.ToUpper(strings.TrimSpace(p.value))
			stack = append(stack, name)
			if name == "VEVENT" && countVEvent(stack) == 1 {
				props = nil
			}
		case "END":
			name := strings.ToUpper(strings.TrimSpace(p.value))
			if len(stack) > 0 {
				stack = stack[:len(stack)-1]
			}
			if name == "VEVENT" && countVEvent(stack) == 0 {
				if ev, ok := buildVEvent(props, def); ok {
					out = append(out, ev)
				}
				props = nil
			}
		default:
			if len(stack) > 0 && stack[len(stack)-1] == "VEVENT" {
				props = append(props, p)
			}
		}
	}
	return out
}

func buildVEvent(props []icalProp, def *time.Location) (vevent, bool) {
	var ev vevent
	var haveStart bool
	for _, p := range props {
		switch p.name {
		case "UID":
			ev.UID = strings.TrimSpace(p.value)
		case "SUMMARY":
			ev.Summary = unescapeICalText(p.value)
		case "LOCATION":
			ev.Location = unescapeICalText(p.value)
		case "DESCRIPTION":
			ev.Description = unescapeICalText(p.value)
		case "STATUS":
			ev.Status = strings.ToUpper(strings.TrimSpace(p.value))
		case "DTSTART":
			ev.Start, haveStart = parseICalTime(p, def)
		case "DTEND":
			ev.End, ev.HasEnd = parseICalTime(p, def)
		case "DURATION":
			ev.Duration, ev.HasDuration = parseICalDuration(p.value)
		case "RECURRENCE-ID":
			ev.RecurID, ev.HasRecurID = parseICalTime(p, def)
		case "RRULE":
			ev.RRule = strings.TrimSpace(p.value)
		case "EXDATE":
			for _, v := range strings.Split(p.value, ",") {
				q := icalProp{name: p.name, params: p.params, value: v}
				if t, ok := parseICalTime(q, def); ok {
					ev.ExDates = append(ev.ExDates, t)
				}
			}
		case "RDATE":
			if strings.EqualFold(p.params["VALUE"], "PERIOD") {
				continue
			}
			for _, v := range strings.Split(p.value, ",") {
				q := icalProp{name: p.name, params: p.params, value: v}
				if t, ok := parseICalTime(q, def); ok {
					ev.RDates = append(ev.RDates, t)
				}
			}
		case "ATTENDEE":
			who := strings.ToLower(strings.TrimPrefix(strings.TrimSpace(p.value), "mailto:"))
			ev.Answers = append(ev.Answers, who+"="+strings.ToUpper(p.params["PARTSTAT"]))
		}
	}
	sort.Strings(ev.Answers)
	return ev, haveStart
}
