package main

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"lab/apps/one/repo"
)

// The block `Connections` at the bottom of the left pane on a laptop (SPEC 1):
// one line for each connected source and one for Claude, each saying when it
// was last read and how much came today. It takes its facts from what the
// Connectors page and the strips already work out; nothing new is read.

// connLine is one line of the block.
type connLine struct {
	Name  string
	Main  string // "3 new today", "stopped", "needs login", "11 waiting"
	Sub   string // "read 2 min ago", "last heard 14:05", "last read 20:51"
	Cls   string // ok, stopped, look or claude: which colour the line has
	Title string // the whole sentence, for the line's title
	Href  string // where the line leads; empty means the Connectors page
	On    bool   // the connection the list is narrowed to
	Alert string // the text of the strip about this source, for screen readers
}

// connNote is a line under the block's lines: a paused review, proposals about
// people, the night check (with its Dismiss).
type connNote struct {
	Text    string
	Href    string
	Cls     string
	Dismiss bool
}

type connBlock struct {
	Lines []connLine
	Notes []connNote
}

// clockShort is a time of day, with the date when it is not today.
func (a *app) clockShort(unix int64, now time.Time) string {
	t := time.Unix(unix, 0).In(a.loc)
	n := now.In(a.loc)
	if t.Year() == n.Year() && t.YearDay() == n.YearDay() {
		return t.Format("15:04")
	}
	return t.Format("2 Jan 15:04")
}

// readText says when something was last read: `read 3 min ago` within the
// hour, `read at 14:05` after that.
func (a *app) readText(unix int64, now time.Time) string {
	if unix <= 0 {
		return "not read yet"
	}
	age := now.Unix() - unix
	if age < 3600 {
		return fmt.Sprintf("read %d min ago", max(age/60, 0))
	}
	return "read at " + a.clockShort(unix, now)
}

func changesText(n int) string { return plural(int64(n), "change") }

// cameToday is how much a source brought since `since` (midnight): items for
// a mailbox or the chat bridge, changes for a calendar (which says so in
// calendar). by is ItemsBySource for the same time. The Connections block
// and the connector's service_status both count it here, so they agree.
func (a *app) cameToday(ctx context.Context, key string, since int64, by map[string]int) (n int, calendar bool) {
	switch key {
	case srcGmail, srcGraph:
		return by[key], false
	case srcBridge:
		for _, c := range []string{"messenger", "whatsapp", "imessage", "sms"} {
			n += by[c]
		}
		return n, false
	}
	return a.store.RunsStoredSince(ctx, key, since), true
}

// connBlockFor works out the block from the sources' states.
func (a *app) connBlockFor(ctx context.Context, states []sourceState) connBlock {
	now := a.now()
	y, m, d := now.In(a.loc).Date()
	since := time.Date(y, m, d, 0, 0, 0, 0, a.loc).Unix()
	by := a.store.ItemsBySource(ctx, since)
	var b connBlock
	for _, s := range states {
		if !s.Configured {
			continue
		}
		l := connLine{Name: s.Label, Cls: "ok"}
		if _, ok := connSources[s.Key]; ok { // choosing it shows only its rows (SPEC 23)
			l.Href, l.On = "/?conn="+s.Key, s.Key == connChosen(ctx)
		}
		last := a.lastActivity(ctx, s.Key)
		today, calendar := a.cameToday(ctx, s.Key, since, by)
		switch s.Status {
		case statusError:
			if s.kind == "silent" { // a source no longer heard from (the Mac bridge): `stopped` with `last heard <time>` (SPEC 1)
				l.Main, l.Cls, l.Alert = "stopped", "stopped", alertText(s)
				l.Sub = lastHeard(a, last, now)
				break
			}
			// reads are tried and fail: `error` with `read <time>` (SPEC 1, 2026-11-24)
			l.Main, l.Cls, l.Alert = "error", "stopped", alertText(s)
			l.Sub = readTime(a, last, now)
		case statusLogin:
			l.Main, l.Cls, l.Alert = "needs login", "stopped", alertText(s)
			l.Sub = lastHeard(a, last, now)
		case statusStale: // Stale on Connectors, in the colour of a stopped source
			l.Cls, l.Alert = "stopped", alertText(s)
			if s.Quiet { // no read and no attempt for longer than allowed: its runs stopped coming
				l.Main, l.Sub = "stopped", lastHeard(a, last, now)
				break
			}
			l.Main, l.Sub = "late", readTime(a, last, now) // still trying, behind its interval
		default:
			l.Sub = a.readText(last, now)
			if last > 0 {
				if calendar {
					l.Main = changesText(today)
				} else {
					l.Main = fmt.Sprintf("%d new today", today)
				}
			}
		}
		l.Title = s.Label + ": " + l.Main
		if l.Main != "" && l.Sub != "" {
			l.Title += ", "
		} else if l.Main == "" {
			l.Title = s.Label + ": "
		}
		l.Title += l.Sub
		if l.Alert != "" {
			l.Title = l.Alert + " " + l.Title
		}
		b.Lines = append(b.Lines, l)
	}
	// Claude: what waits for it and when it last read.
	waiting := a.store.CountWaiting(ctx)
	read := a.store.LastUsageAt(ctx, "review")
	if n, _ := strconv.ParseInt(a.store.Setting(ctx, keyAgentCall), 10, 64); n > read {
		read = n
	}
	c := connLine{Name: "Claude", Main: fmt.Sprintf("%d waiting", waiting), Cls: "ok", Sub: "not read yet"}
	if waiting > 0 {
		c.Cls, c.Alert = "claude", waitingText(waiting)
	}
	if read > 0 {
		c.Sub = "last read " + a.clockShort(read, now)
	}
	c.Href = "/connectors#claude"
	c.Title = "Claude: " + c.Main + ", " + c.Sub
	if c.Alert != "" {
		c.Title = c.Alert + ". " + c.Title
	}
	b.Lines = append(b.Lines, c)
	for _, t := range a.reviewAlerts(ctx) {
		b.Notes = append(b.Notes, connNote{Text: t, Href: "/connectors#claude", Cls: "look"})
	}
	if t := a.peopleStrip(ctx); t != "" {
		b.Notes = append(b.Notes, connNote{Text: t, Href: "/settings#h-proposals", Cls: "claude"})
	}
	if t := a.nightLineText(ctx); t != "" {
		b.Notes = append(b.Notes, connNote{Text: t, Href: "/connectors#claude", Cls: "quiet", Dismiss: true})
	}
	return b
}

// readTime is `read <time>`, the time in the form `read at <time>` uses.
func readTime(a *app, last int64, now time.Time) string {
	if last <= 0 {
		return "not read yet"
	}
	return "read " + a.clockShort(last, now)
}

func lastHeard(a *app, last int64, now time.Time) string {
	if last <= 0 {
		return ""
	}
	return "last heard " + a.clockShort(last, now)
}

// listConnections is the block as a fragment, for the page to put in its place
// when the numbers are renewed.
func (a *app) listConnections(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	a.fragment(w, "connections", http.StatusOK, a.connBlockFor(ctx, a.sourceStates(ctx)))
}

// connSources are the connections whose messages the list can be narrowed to
// (SPEC 23): the two mailboxes and the chat bridge (its four networks). A
// calendar brings no messages and Claude is no connection, so they do not filter.
var connSources = map[string][]string{
	srcGmail:  {srcGmail},
	srcGraph:  {srcGraph},
	srcBridge: {"messenger", "whatsapp", "imessage", "sms"},
}

type chosenKey struct{}

func connChosen(ctx context.Context) string { k, _ := ctx.Value(chosenKey{}).(string); return k }

// withConn reads the query parameter `conn` and narrows the list to that
// connection. The choice lives only in the address: nothing is stored.
func withConn(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if k := r.URL.Query().Get("conn"); k != "" {
			if src, ok := connSources[k]; ok {
				ctx := repo.WithOnly(r.Context(), src)
				r = r.WithContext(context.WithValue(ctx, chosenKey{}, k))
			}
		}
		h(w, r)
	}
}

// onlyView is the bar above the list: `Only <connection>` and `Show all`.
type onlyView struct{ Name, Key string }

func (a *app) onlyFor(ctx context.Context, states []sourceState) *onlyView {
	k := connChosen(ctx)
	if k == "" {
		return nil
	}
	for _, s := range states {
		if s.Key == k {
			return &onlyView{Name: s.Label, Key: k}
		}
	}
	return nil
}
