package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"lab/apps/one/repo"
	"lab/platform/appkit"
)

// Closed (SPEC 27): the page of everything closed, who closed it and why, and
// the way back. And the one thing the app closes by itself (SPEC 3): a
// Resolved item that has had no news for the quiet time.

const (
	keyQuietDays = "quiet_close_days"
	defQuietDays = 14
	closedPage   = 25 // entries a section shows at first, and how many more each "Show more" adds
)

func (a *app) quietDays(ctx context.Context) int {
	if n, err := strconv.Atoi(a.store.Setting(ctx, keyQuietDays)); err == nil && n > 0 {
		return n
	}
	return defQuietDays
}

func closedAfterText(days int) string {
	if days <= 0 {
		days = defQuietDays
	}
	return fmt.Sprintf("Closed after %s without news.", plural(int64(days), "day"))
}

// closeQuiet closes every Resolved item that has had no news (no message and
// no resolving) for the quiet time. The app's own timer calls it. A new
// message on such an item has already brought it back to New, so nothing
// that has news is closed. The closing is an event by the system.
func (a *app) closeQuiet(ctx context.Context) (int, error) {
	days := a.quietDays(ctx)
	now := a.now().Unix()
	cutoff := now - int64(days)*86400
	items, err := a.store.QuietResolved(ctx, cutoff, 200)
	if err != nil {
		return 0, err
	}
	closed := 0
	for _, it := range items {
		err := a.store.Update(ctx, func(tx *repo.Tx) error {
			cur, ok := tx.ItemByID(ctx, it.ID) // look again inside the transaction
			if !ok || cur.Stage != stageResolved || cur.LastAt >= cutoff {
				return nil
			}
			if err := tx.SetItemStage(ctx, cur.ID, stageClosed, now); err != nil {
				return err
			}
			if _, err := tx.WithdrawPending(ctx, cur.ID, "the item was closed", now); err != nil {
				return err
			}
			closed++
			return tx.InsertEvent(ctx, now, cur.ID, "system", "closed", map[string]any{"auto": true, "days": days, "item": repo.Number(cur.Number)})
		})
		if err != nil {
			return closed, err
		}
	}
	return closed, nil
}

// housekeeping is the app's own timer for what needs no request: closing
// quiet Resolved items and watching the sources.
func (a *app) housekeeping(ctx context.Context) {
	run := func() {
		// Each step is its own unit: one that panics does not stop the others,
		// and the next round comes after the usual minute.
		a.runUnit("close quiet items", func() {
			if _, err := a.closeQuiet(ctx); err != nil && ctx.Err() == nil {
				appkit.Error("closing quiet items", err)
			}
		})
		a.runUnit("quiet-time release", func() { a.releaseChats(ctx) })
		a.runUnit("chat sends in doubt", func() { a.chatSendsInDoubt(ctx) })
		a.runUnit("out in doubt", func() { a.outInDoubt(ctx) })
		a.runUnit("source check", func() { a.checkSources(ctx) })
		a.runUnit("night check timer", func() { a.nightTick(ctx) })
		a.runUnit("summary of closed", func() { a.summaryKick(ctx) })
	}
	run()
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			run()
		}
	}
}

// --- the page ---

type closedEntry struct {
	ID       int64
	Number   string
	Who      string
	Color    string
	Subject  string
	When     string
	WhenFull string
	ISO      string
	By       string
	Reason   string
	Kind     string // what kind of skip, in words; "" when it was not a skip
	BackPath string
	Unseen   bool // marked `Not seen` (Closed by Claude only)
	SeenPath string
}

type closedSection struct {
	Name    string
	Key     string // the section's parameter in the address
	Entries []closedEntry
	MoreURL string // the link that shows more of this section, "" when there is no more
	Unseen  int    // how many of the section are not seen
}

type closedView struct {
	Notice   string
	Sections []closedSection
	Total    int
}

type closingData struct {
	Reason   string `json:"reason"`
	Kind     string `json:"kind"`
	Note     int64  `json:"note"`
	NoteName string `json:"note_title"`
	Proposal int64  `json:"proposal"`
	Auto     bool   `json:"auto"`
	Days     int    `json:"days"`
	SameAs   string `json:"same_as"`
	RuleName string `json:"rule_name"`
}

// closedBy words who closed an item and why from the event that did it.
func (a *app) closedBy(ctx context.Context, itemID int64) (by, reason, kind string, claude bool) {
	c, ok := a.store.ClosingOf(ctx, itemID)
	if !ok {
		return "ONE", "No reason was recorded.", "", false
	}
	var d closingData
	json.Unmarshal([]byte(c.Data), &d)
	by = actorLabel(c.Actor)
	reason = d.Reason
	if c.Type == "skipped" {
		kind = skipKindWords[d.Kind]
		if d.NoteName != "" {
			kind += ", by your note " + d.NoteName
		}
	}
	switch {
	case c.Actor == "rule":
		reason = ruleClosedText(d.RuleName)
		if d.SameAs != "" {
			reason += ". Same mail as " + d.SameAs + "."
		}
	case d.Auto:
		reason = fmt.Sprintf("Resolved, and no news for %s.", plural(int64(max(d.Days, 1)), "day"))
	case d.Proposal != 0:
		if p, found := a.store.ProposalByID(ctx, d.Proposal); found {
			reason = p.Reason
			if p.By != "" {
				reason += " (proposed by " + actorLabel(p.By) + ")"
			}
		}
	}
	if reason == "" {
		reason = "none given"
	}
	return by, reason, kind, c.Type == "skipped"
}

// skipKindWords says in plain words what kind of clear skip it was.
var skipKindWords = map[string]string{"noise": "Noise", "finished": "A chat that has finished", "old": "Old: its date has passed"}

// closedGroups are the sections of the page in their order.
var closedGroups = []struct{ Key, Name string }{
	{repo.ClosedByClaude, "Closed by Claude"},
	{repo.ClosedByRule, "Closed by a rule"},
	{repo.ClosedElse, "Everything else"},
}

// closedViewFor builds the page. Every section that has entries is on it and
// shows its own newest ones; shown says how many each section shows (a
// section missing from the map shows the first page). Showing more of one
// section never changes another.
func (a *app) closedViewFor(ctx context.Context, shown map[string]int) (closedView, error) {
	v := closedView{Total: a.store.ClosedCount(ctx)}
	now := a.now()
	counts := map[string]int{}
	for _, g := range closedGroups {
		n := closedPage
		if shown[g.Key] > n {
			n = min(shown[g.Key], closedShownMax)
		}
		counts[g.Key] = min(n, closedShownMax)
	}
	for _, g := range closedGroups {
		rows, err := a.store.ClosedList(ctx, g.Key, counts[g.Key]+1, 0)
		if err != nil {
			return v, err
		}
		if len(rows) == 0 {
			continue
		}
		sec := closedSection{Name: g.Name, Key: g.Key}
		if len(rows) > counts[g.Key] {
			rows = rows[:counts[g.Key]]
			if counts[g.Key] < closedShownMax {
				q := url.Values{}
				for _, o := range closedGroups {
					if c := counts[o.Key]; c > closedPage || o.Key == g.Key {
						if o.Key == g.Key {
							c += closedPage
						}
						q.Set(o.Key, strconv.Itoa(c))
					}
				}
				sec.MoreURL = "/closed?" + q.Encode() + "#sec-" + g.Key
			}
		}
		views := make([]rowView, len(rows))
		for i, r := range rows {
			views[i] = a.rowFor(r, now)
		}
		a.dress(ctx, views, rows)
		for i, r := range rows {
			by, reason, kind, _ := a.closedBy(ctx, r.ID)
			ce := closedEntry{ID: r.ID, Number: views[i].Number, Who: views[i].Who, Color: views[i].Color, Subject: views[i].Subject,
				When: views[i].When, WhenFull: views[i].WhenFull, ISO: views[i].ISO, By: by, Reason: reason, Kind: kind,
				BackPath: "/items/" + strconv.FormatInt(r.ID, 10) + "/bring-back"}
			if g.Key == repo.ClosedByClaude && a.store.IsUnseenClosed(ctx, r.ID) {
				ce.Unseen, ce.SeenPath = true, "/items/"+strconv.FormatInt(r.ID, 10)+"/closed-seen"
			}
			sec.Entries = append(sec.Entries, ce)
		}
		if g.Key == repo.ClosedByClaude {
			sec.Unseen = a.store.CountUnseenClosed(ctx)
		}
		v.Sections = append(v.Sections, sec)
	}
	return v, nil
}

// closedShownMax is the most entries one section shows, whatever the address asks for.
var closedShownMax = 500

func (a *app) closedPageHandler(w http.ResponseWriter, r *http.Request) {
	shown := map[string]int{}
	for _, g := range closedGroups {
		if n, err := strconv.Atoi(r.URL.Query().Get(g.Key)); err == nil && n > 0 {
			shown[g.Key] = min(n, 100000)
		}
	}
	v, err := a.closedViewFor(r.Context(), shown)
	if err != nil {
		appkit.Error("building the closed page", err)
		http.Error(w, "The page could not be loaded. Reload it to try again.", http.StatusInternalServerError)
		return
	}
	if r.URL.Query().Get("n") == "back" {
		v.Notice = "You brought this back."
	}
	a.render(w, "closed.html", http.StatusOK, pageData{EnvText: a.envText, Version: a.version, Closed: v})
}

// bringBack is the owner's button on the Closed page: the item goes back to New
// and the thread says so. Nothing else changes.
func (a *app) bringBack(w http.ResponseWriter, r *http.Request) {
	id, ok := itemID(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	ctx := r.Context()
	now := a.now().Unix()
	err := a.store.Update(ctx, func(tx *repo.Tx) error {
		it, found := tx.ItemByID(ctx, id)
		if !found || it.Hidden {
			return errNoItem
		}
		if it.Stage != stageClosed {
			return nil // already back (a second tap, a second device): nothing to write
		}
		// A skip rule whose item the owner brings back is paused at once (SPEC 31).
		if err := a.pauseRuleBehind(ctx, tx, it, "You", "", now); err != nil {
			return err
		}
		if err := tx.SetItemStage(ctx, id, stageNew, now); err != nil {
			return err
		}
		return tx.InsertEvent(ctx, now, id, "owner", "brought_back", map[string]any{"item": repo.Number(it.Number), "from": it.Stage, "to": stageNew})
	})
	if err == errNoItem {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		appkit.Error("bringing an item back", err)
		http.Error(w, "That did not work. Try again.", http.StatusInternalServerError)
		return
	}
	a.closedSummaryRun(ctx, "", false) // what is back is no longer listed in the summary
	if row, err := strconv.ParseInt(r.FormValue("back"), 10, 64); err == nil && row > 0 {
		http.Redirect(w, r, "/items/"+strconv.FormatInt(row, 10), http.StatusSeeOther) // from the summary: back to it
		return
	}
	http.Redirect(w, r, "/closed?n=back", http.StatusSeeOther)
}
