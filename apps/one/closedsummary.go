package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"lab/apps/one/repo"
	"lab/platform/appkit"
)

// Four eyes on what Claude closed (SPEC 27).
//
// Seen marks are events of the owner's (repo/closedseen.go). The summary is ONE
// row of its own kind with ONE to-do that points at it, for as long as
// anything Claude closed is not seen. The row is made at once, with a plain
// list grouped by why things were closed. Once a day, in the evening, it is
// written again when Claude has closed more (or while it is still the plain
// list and a model is available): by the app's own model within the daily
// limit, or as the plain list. The row never waits for Claude by itself; a
// note of the owner's on it does, as on any row. No reply can be proposed on it.

const (
	keyClosedSummary = "closed_summary"
	summaryTodoText  = "Review what Claude closed"
	summaryEvent     = "closed_summary"
	summaryNotice    = "closed_summary_notice"
	summaryHour      = 20 // the evening write, in the app's time zone (the night check's way: a.loc)
	summaryModelMax  = 200
	summaryOutput    = 3000
	summaryMaxChars  = 8000

	introNoKey = "Claude API: Not configured"
	introLimit = "Review paused: today's limit is reached"
	introDown  = "Review paused: Claude cannot be reached"
)

func summarySubject(n int) string { return fmt.Sprintf("Closed by Claude: %d to review", n) }

func closedLinkText(n, unseen int) string {
	if unseen > 0 {
		return fmt.Sprintf("Closed (%d, %d not seen)", n, unseen)
	}
	return fmt.Sprintf("Closed (%d)", n)
}

// summaryOn is the setting `Summary of what Claude closed`.
func (a *app) summaryOn(ctx context.Context) bool {
	d, _ := settingDefByKey(keyClosedSummary)
	return a.currentSetting(ctx, d) != "off"
}

// --- the data ---

type sumItem struct {
	ID     int64    `json:"id"`
	Number string   `json:"number"`
	Who    string   `json:"who"`
	What   string   `json:"what"`
	Why    string   `json:"why,omitempty"` // the item's own reason, kept only when its group does not state one
	Doubt  string   `json:"doubt,omitempty"`
	Kind   string   `json:"kind,omitempty"`
	Score  int      `json:"-"`
	Reason string   `json:"-"`
	Facts  []string `json:"-"` // what was found about the item; the model gets them as data
}

type sumGroup struct {
	Title  string    `json:"title"`
	Reason string    `json:"reason,omitempty"`
	Items  []sumItem `json:"items"`
}

type sumText struct {
	Intro   string     `json:"intro,omitempty"`
	Model   bool       `json:"model"`
	Day     string     `json:"day"`
	Evening bool       `json:"evening"`
	Groups  []sumGroup `json:"groups"`
}

func (t sumText) ids() map[int64]bool {
	out := map[int64]bool{}
	for _, g := range t.Groups {
		for _, it := range g.Items {
			out[it.ID] = true
		}
	}
	return out
}

// gatherClosed reads what the summary says about each id.
func (a *app) gatherClosed(ctx context.Context, ids []int64) []sumItem {
	now := a.now()
	var out []sumItem
	for _, id := range ids {
		lr, ok := a.store.ListRowByID(ctx, id)
		if !ok {
			continue
		}
		v := a.rowFor(lr, now)
		_, reason, _, _ := a.closedBy(ctx, id)
		var d closingData
		if c, ok := a.store.ClosingOf(ctx, id); ok {
			json.Unmarshal([]byte(c.Data), &d)
		}
		score, why, texts := doubtOfFacts(a.factsOf(ctx, lr, d.Kind, d.NoteName != ""))
		it := sumItem{ID: id, Number: v.Number, Who: v.Who, What: whatOf(lr, v.Subject), Reason: oneLine(reason, 200), Kind: skipKindWords[d.Kind], Score: score, Facts: texts}
		if d.NoteName != "" {
			it.Kind = "Skipped by your note " + oneLine(d.NoteName, 60)
		}
		if it.Kind == "" {
			it.Kind = "Closed"
		}
		it.Doubt = why
		out = append(out, it)
	}
	return out
}

func orderGroups(gs []sumGroup) {
	for i := range gs {
		sort.SliceStable(gs[i].Items, func(x, y int) bool { return gs[i].Items[x].Score > gs[i].Items[y].Score })
	}
	top := func(g sumGroup) int {
		m := 0
		for _, it := range g.Items {
			m = max(m, it.Score)
		}
		return m
	}
	sort.SliceStable(gs, func(x, y int) bool { return top(gs[x]) > top(gs[y]) })
}

// plainSummary is the list without the model: grouped by the kind of closing,
// the least sure first. A group states a reason once when all its items share it.
func plainSummary(items []sumItem, intro string) sumText {
	var gs []sumGroup
	at := map[string]int{}
	for _, it := range items {
		i, ok := at[it.Kind]
		if !ok {
			i = len(gs)
			at[it.Kind] = i
			gs = append(gs, sumGroup{Title: it.Kind})
		}
		gs[i].Items = append(gs[i].Items, it)
	}
	for i := range gs {
		same := true
		for _, it := range gs[i].Items {
			same = same && it.Reason == gs[i].Items[0].Reason
		}
		if same {
			gs[i].Reason = gs[i].Items[0].Reason
		}
		for j := range gs[i].Items {
			if !same {
				gs[i].Items[j].Why = gs[i].Items[j].Reason
			}
		}
	}
	orderGroups(gs)
	return sumText{Intro: intro, Groups: gs}
}

// plainIntro is the first line of a list written without the model: it says
// why, when the reason is that there is no key or today's limit is used up.
func (a *app) plainIntro(ctx context.Context) string {
	switch {
	case !a.claudeConfigured():
		return introNoKey
	case a.limitReached(ctx):
		return introLimit
	}
	return ""
}

// --- the model ---

var numberRE = regexp.MustCompile(`(?i)\bQ-\d+`)

// checkModelSummary is the deterministic check of the model's text. It
// returns the groups in the app's order (least sure first) and, when the text
// is refused, what was wrong in plain words ("" when it passes): an item left
// out, listed twice or not in the list; a number that is in none of the
// listed items; a doubt for an item that has no fact; a number in a doubt
// that is in none of the item's facts; an empty or too long text. The app's
// own words stay for the "what" (the subject) and for the doubt of an item
// the model wrote none for.
func checkModelSummary(raw json.RawMessage, items []sumItem) ([]sumGroup, string) {
	var in struct {
		Groups []struct {
			Title string `json:"title"`
			Items []struct {
				ID    string `json:"id"`
				Doubt string `json:"doubt"`
			} `json:"items"`
		} `json:"groups"`
	}
	if json.Unmarshal(raw, &in) != nil || len(in.Groups) == 0 {
		return nil, "the answer had no groups in the shape asked for"
	}
	byNum := map[string]sumItem{}
	for _, it := range items {
		byNum[strings.ToUpper(it.Number)] = it
	}
	used := map[string]bool{}
	var problems []string
	total := 0
	var out []sumGroup
	for _, g := range in.Groups {
		title := clean(g.Title, 100)
		if title == "" || len(g.Items) == 0 {
			problems = append(problems, "a group had no title or no items")
			continue
		}
		total += len(title)
		og := sumGroup{Title: title}
		texts := []string{g.Title}
		for _, x := range g.Items {
			num := strings.ToUpper(strings.TrimSpace(x.ID))
			it, ok := byNum[num]
			switch {
			case !ok:
				problems = append(problems, "the number "+strings.TrimSpace(x.ID)+" is not in the list")
				continue
			case used[num]:
				problems = append(problems, num+" appears twice")
				continue
			}
			used[num] = true
			doubt := clean(x.Doubt, 160)
			texts = append(texts, x.Doubt)
			switch {
			case doubt != "" && it.Doubt == "":
				problems = append(problems, num+" has no fact that gives a doubt, so its doubt must be empty")
				doubt = ""
			case doubt != "" && doubtInventsNumbers(doubt, it.Facts):
				problems = append(problems, "the doubt of "+num+" has a number that is not in its facts")
				doubt = ""
			}
			total += len(doubt)
			it.Reason, it.Why = "", ""
			if doubt != "" {
				it.Doubt = doubt
			}
			og.Items = append(og.Items, it)
		}
		for _, t := range texts {
			for _, n := range numberRE.FindAllString(t, -1) {
				if _, ok := byNum[strings.ToUpper(n)]; !ok {
					problems = append(problems, "the number "+n+" is not in the list")
				}
			}
		}
		out = append(out, og)
	}
	var missing []string
	for _, it := range items {
		if !used[strings.ToUpper(it.Number)] {
			missing = append(missing, it.Number)
		}
	}
	if len(missing) > 0 {
		problems = append(problems, "left out: "+strings.Join(missing, ", "))
	}
	if total > summaryMaxChars {
		problems = append(problems, "the text is too long")
	}
	if len(problems) > 0 {
		return nil, strings.Join(problems, "; ")
	}
	orderGroups(out)
	return out, ""
}

// summaryLines is what the model is given about the items: one line each, the
// facts found (data it may only rephrase) at the end.
func summaryLines(items []sumItem) string {
	var b strings.Builder
	for _, it := range items {
		facts := strings.Join(it.Facts, "; ")
		if it.Doubt == "" {
			facts = "none" // a weak fact alone gives no doubt
		}
		fmt.Fprintf(&b, "%s | %s | %s | %s | %s | facts: %s\n", it.Number, clean(it.Who, 60), clean(it.What, 120), clean(it.Reason, 200), it.Kind, facts)
	}
	return b.String()
}

// writeSummaryText gives the text of a write: the model's when it can and
// its answer passes the check (once more after a refusal, with what was wrong),
// else the plain list. The plain list's first line says why only when the
// reason is one the app knows for sure: no key, today's limit, or a
// connection that failed. Whatever else goes wrong is logged at error with its
// cause in plain words (never message text or the key), so it shows on the
// Errors page.
func (a *app) writeSummaryText(ctx context.Context, items []sumItem, evening bool) sumText {
	day := a.dayKey(a.now())
	plain := func(intro string) sumText {
		t := plainSummary(items, intro)
		t.Day, t.Evening = day, evening
		return t
	}
	key, ok := a.claudeKey()
	if !ok {
		return plain(introNoKey)
	}
	if a.limitReached(ctx) {
		return plain(introLimit)
	}
	model := a.reviewModel(ctx)
	forModel, rest := items, []sumItem(nil)
	if len(items) > summaryModelMax {
		forModel, rest = items[:summaryModelMax], items[summaryModelMax:]
	}
	lines := summaryLines(forModel)
	spent := a.store.SpentMicro(ctx, "review", a.dayKey(a.now()), "")
	est := (len(closedSummarySystem) + len(lines)) / 3
	if spent+a.priceTable(ctx).costMicro(model, est, summaryOutput) > limitMicro(a.dailyLimit(ctx)) {
		return plain(introLimit)
	}
	prompt, problem := lines, ""
	for attempt := 0; attempt < 2; attempt++ {
		ans, cerr := a.callClaude(ctx, key, claudeRequest{Model: model, MaxTokens: summaryOutput, System: closedSummarySystem,
			Messages: []claudeMsg{{Role: "user", Content: prompt}},
			Tools:    []claudeToolDef{closedSummaryTool}})
		if cerr != nil {
			if !cerr.Stopped {
				a.logger().Error("the summary of what Claude closed: the model call failed", "cause", cerr.Text, "model", model, "status", cerr.Status)
			}
			if cerr.Unreached {
				return plain(introDown) // a real connection failure is the one thing "cannot be reached" says
			}
			return plain("") // refused, invalid or overloaded: no line that would not be true
		}
		a.recordUsage(ctx, "review", "closedsummary", model, ans)
		problem = "the answer did not use the tool write_summary"
		for _, blk := range ans.Content {
			if blk.Type != "tool_use" || blk.Name != "write_summary" {
				continue
			}
			gs, p := checkModelSummary(blk.Input, forModel)
			if p == "" {
				t := sumText{Model: true, Day: day, Evening: evening, Groups: gs}
				if len(rest) > 0 {
					more := plainSummary(rest, "")
					for i := range more.Groups {
						more.Groups[i].Title = "More, not read by the model: " + more.Groups[i].Title
					}
					t.Groups = append(t.Groups, more.Groups...)
				}
				return t
			}
			problem = p
		}
		a.logger().Error("the summary of what Claude closed: the model's text was refused by the check", "cause", problem, "model", model, "attempt", attempt+1)
		prompt = lines + "\nYour previous answer was refused: " + problem + ". Write it again with the tool write_summary: every item exactly once, and a doubt only for an item whose facts are not \"none\", using only what its facts say."
	}
	return plain("") // twice refused: the plain list, no model text
}

// --- the row ---

// summaryWrite stores a text as the row's current summary; the old one stays in the record.
func (a *app) summaryWrite(ctx context.Context, tx *repo.Tx, it repo.Item, t sumText, now int64) error {
	n := 0
	for _, g := range t.Groups {
		n += len(g.Items)
	}
	if err := tx.SetItemSubject(ctx, it.ID, summarySubject(n)); err != nil {
		return err
	}
	return tx.InsertEvent(ctx, now, it.ID, "system", summaryEvent, t)
}

func (a *app) summaryCurrent(ctx context.Context, q repo.Q, itemID int64) (sumText, bool) {
	e, ok := q.NewestEventOf(ctx, itemID, summaryEvent)
	if !ok {
		return sumText{}, false
	}
	var t sumText
	return t, json.Unmarshal([]byte(e.Data), &t) == nil
}

// closeSummaryRow resolves the row and ticks its to-do; what it lists is not touched.
func (a *app) closeSummaryRow(ctx context.Context, tx *repo.Tx, it repo.Item, by string, now int64) error {
	if it.Stage != stageResolved && it.Stage != stageClosed {
		if err := tx.SetItemStage(ctx, it.ID, stageResolved, now); err != nil {
			return err
		}
		if err := tx.InsertEvent(ctx, now, it.ID, by, "resolved", map[string]any{"item": repo.Number(it.Number), "from": it.Stage, "to": stageResolved}); err != nil {
			return err
		}
		if _, err := tx.WithdrawPending(ctx, it.ID, "the summary was settled", now); err != nil {
			return err
		}
	}
	return a.tickSummaryTodo(ctx, tx, it, by, now)
}

func (a *app) tickSummaryTodo(ctx context.Context, tx *repo.Tx, it repo.Item, by string, now int64) error {
	if td, has := tx.OpenTodoOfItem(ctx, it.ID); has {
		if d, err := tx.SetAgendaDone(ctx, td.ID, now); err != nil {
			return err
		} else if d {
			return tx.InsertAgendaEvent(ctx, now, it.ID, td.ID, by, "agenda_closed", map[string]any{"kind": "todo"})
		}
	}
	return nil
}

// settleSummary is the owner's resolving of the row, or ticking of its to-do:
// everything the row lists is marked seen, the row and the to-do are closed.
func (a *app) settleSummary(ctx context.Context, tx *repo.Tx, it repo.Item, now int64) error {
	if t, ok := a.summaryCurrent(ctx, tx.Q, it.ID); ok {
		for _, g := range t.Groups {
			for _, x := range g.Items {
				if tx.IsUnseenClosed(ctx, x.ID) {
					if err := tx.InsertEvent(ctx, now, x.ID, "owner", "closed_seen", map[string]any{"via": "summary"}); err != nil {
						return err
					}
				}
			}
		}
	}
	return a.closeSummaryRow(ctx, tx, it, "owner", now)
}

// closedSummarySync brings the row in step with what is closed and not seen.
// With evening set it may also write the day's text. It reports whether the
// phone should be told (a row was made or written again).
func (a *app) closedSummarySync(ctx context.Context, run string, evening bool) (told bool, err error) {
	return a.closedSummarySyncOpt(ctx, run, evening, false)
}

// closedSummarySyncOpt is closedSummarySync; force (for a test run only: the
// test-only path with now=1) writes the model's text again at once, whatever
// the day and whatever was written before.
func (a *app) closedSummarySyncOpt(ctx context.Context, run string, evening, force bool) (told bool, err error) {
	force = force && run != ""
	evening = evening || force
	if run != "" {
		ctx = repo.WithRun(ctx, run)
	}
	now := a.now()
	day := a.dayKey(now)
	for pass := 0; pass < 2; pass++ {
		ids, err := a.store.UnseenClosedIDs(ctx, run)
		if err != nil {
			return false, err
		}
		unseen := map[int64]bool{}
		for _, id := range ids {
			unseen[id] = true
		}
		row, has := a.store.ClosedSummaryRow(ctx, run)
		if !has {
			if len(ids) == 0 {
				return false, nil
			}
			items := a.gatherClosed(ctx, ids)
			var t sumText
			if evening {
				t = a.writeSummaryText(ctx, items, true) // the evening write: the model's text, once
			} else {
				t = plainSummary(items, a.plainIntro(ctx))
			}
			t.Day = day
			if err := a.summaryCreate(ctx, run, t, now.Unix()); err != nil {
				return false, err
			}
			return true, nil
		}
		last, _ := a.summaryCurrent(ctx, a.store.Q, row.ID)
		listed := 0
		newer := false
		lastIDs := last.ids()
		for id := range lastIDs {
			if unseen[id] {
				listed++
			}
		}
		for _, id := range ids {
			newer = newer || !lastIDs[id]
		}
		if listed == 0 {
			if err := a.store.Update(ctx, func(tx *repo.Tx) error { return a.closeSummaryRow(ctx, tx, row, "system", now.Unix()) }); err != nil {
				return false, err
			}
			continue // what was closed meanwhile starts a new row
		}
		due := force || evening && !(last.Evening && last.Day == day) && (newer || (!last.Model && a.claudeConfigured() && !a.limitReached(ctx)))
		if due {
			t := a.writeSummaryText(ctx, a.gatherClosed(ctx, ids), true)
			if err := a.store.Update(ctx, func(tx *repo.Tx) error { return a.summaryWrite(ctx, tx, row, t, now.Unix()) }); err != nil {
				return false, err
			}
			return true, nil
		}
		if want := summarySubject(listed); row.Subject != want {
			return false, a.store.Update(ctx, func(tx *repo.Tx) error { return tx.SetItemSubject(ctx, row.ID, want) })
		}
		return false, nil
	}
	return false, nil
}

func (a *app) summaryCreate(ctx context.Context, run string, t sumText, now int64) error {
	n := 0
	for _, g := range t.Groups {
		n += len(g.Items)
	}
	return a.store.Update(ctx, func(tx *repo.Tx) error {
		it, err := tx.CreateItem(ctx, repo.Item{Kind: repo.ClosedSummaryKind, Subject: summarySubject(n), Stage: stageNew, FirstAt: now, LastAt: now, MovedAt: now},
			repo.ClosedSummaryKind, fmt.Sprintf("%s|%d", run, tx.LastEventID(ctx)+1), "system", now)
		if err != nil {
			return err
		}
		if err := tx.InsertEvent(ctx, now, it.ID, "system", summaryEvent, t); err != nil {
			return err
		}
		id, err := tx.InsertAgenda(ctx, repo.Agenda{Kind: "todo", Title: summaryTodoText, State: "open", Status: "confirmed", CreatedBy: "system", ItemID: it.ID, TestRun: run, CreatedAt: now, UpdatedAt: now})
		if err != nil {
			return err
		}
		return tx.InsertAgendaEvent(ctx, now, it.ID, id, "system", "agenda_posted", map[string]any{"kind": "todo"})
	})
}

// closedSummaryRun is one pass for a run: the row, and the one notice a day.
func (a *app) closedSummaryRun(ctx context.Context, run string, evening bool) {
	told, err := a.closedSummarySync(ctx, run, evening)
	if err != nil {
		appkit.Error("keeping the summary of what Claude closed", err)
		return
	}
	if !told || run != "" {
		return // a test run never reaches the phone
	}
	day := a.dayKey(a.now())
	row, ok := a.store.ClosedSummaryRow(ctx, run)
	if !ok || a.store.DayEventCount(ctx, summaryNotice, day, run) > 0 {
		return
	}
	a.store.Update(ctx, func(tx *repo.Tx) error {
		return tx.InsertEvent(ctx, a.now().Unix(), row.ID, "system", summaryNotice, map[string]any{"day": day})
	})
	link := ""
	if base := os.Getenv("BASE_URL"); base != "" {
		link = strings.TrimRight(base, "/") + "/items/" + strconv.FormatInt(row.ID, 10)
	}
	if err := a.tell("Closed by Claude", row.Subject, link); err != nil {
		appkit.Error("sending a notice about what Claude closed", err)
	}
}

// closedSummaryTick is the housekeeping timer's part: the row is kept in step
// every minute, and the day's text is written from the evening on.
func (a *app) closedSummaryTick(ctx context.Context) {
	if !a.summaryOn(ctx) {
		return
	}
	evening := a.now().In(a.loc).Hour() >= summaryHour
	a.closedSummaryRun(ctx, "", evening)
}

func (a *app) summaryKick(ctx context.Context) {
	if !a.summaryMu.TryLock() {
		return
	}
	go func() {
		defer a.summaryMu.Unlock()
		a.closedSummaryTick(ctx)
	}()
}

// --- the owner's buttons: Seen and Mark all seen ---

func (a *app) markSeenHandler(w http.ResponseWriter, r *http.Request) {
	id, ok := itemID(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	ctx := r.Context()
	var run string
	err := a.store.Update(ctx, func(tx *repo.Tx) error {
		it, found := tx.ItemByID(ctx, id)
		if !found || it.Hidden {
			return errNoItem
		}
		run = it.TestRun
		if !tx.IsUnseenClosed(ctx, id) {
			return nil // a second tap, a second device
		}
		return tx.InsertEvent(ctx, a.now().Unix(), id, "owner", "closed_seen", map[string]any{})
	})
	a.afterSeen(w, r, run, err, "/closed#closed-"+strconv.FormatInt(id, 10))
}

func (a *app) markAllSeenHandler(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	runs := map[string]bool{"": true}
	err := a.store.Update(ctx, func(tx *repo.Tx) error {
		ids, err := tx.UnseenClosedIDs(ctx, "*")
		if err != nil {
			return err
		}
		for _, id := range ids {
			if it, ok := tx.ItemByID(ctx, id); ok {
				runs[it.TestRun] = true
			}
			if err := tx.InsertEvent(ctx, a.now().Unix(), id, "owner", "closed_seen", map[string]any{"all": true}); err != nil {
				return err
			}
		}
		return nil
	})
	for run := range runs {
		if err == nil {
			_, err = a.closedSummarySync(ctx, run, false)
		}
	}
	a.afterSeen(w, r, "", err, "/closed#sec-claude")
}

func (a *app) afterSeen(w http.ResponseWriter, r *http.Request, run string, err error, to string) {
	if err == errNoItem {
		http.NotFound(w, r)
		return
	}
	if err == nil && run != "-" {
		_, err = a.closedSummarySync(r.Context(), run, false)
	}
	if err != nil {
		appkit.Error("marking what Claude closed as seen", err)
		http.Error(w, "That did not work. Try again.", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, to, http.StatusSeeOther)
}

// --- what the owner sees in the row ---

type sumItemView struct {
	ID       int64
	Number   string
	Who      string
	What     string
	Why      string
	Doubt    string
	Open     string
	BackPath string
	Back     bool // still closed: the button Bring back
}

type sumGroupView struct {
	Title  string
	Reason string
	Items  []sumItemView
}

type closedSummaryView struct {
	Intro  string
	Groups []sumGroupView
	Count  int
	Row    int64
}

func (a *app) closedSummaryViewFor(ctx context.Context, rowID int64) *closedSummaryView {
	t, ok := a.summaryCurrent(ctx, a.store.Q, rowID)
	if !ok {
		return nil
	}
	v := &closedSummaryView{Intro: t.Intro, Row: rowID}
	for _, g := range t.Groups {
		gv := sumGroupView{Title: g.Title, Reason: g.Reason}
		for _, x := range g.Items {
			if !a.store.IsUnseenClosed(ctx, x.ID) {
				continue // seen, or brought back: it leaves the list
			}
			sid := strconv.FormatInt(x.ID, 10)
			gv.Items = append(gv.Items, sumItemView{ID: x.ID, Number: x.Number, Who: x.Who, What: x.What, Why: x.Why, Doubt: x.Doubt,
				Open: "/items/" + sid, BackPath: "/items/" + sid + "/bring-back", Back: true})
			v.Count++
		}
		if len(gv.Items) > 0 {
			v.Groups = append(v.Groups, gv)
		}
	}
	return v
}

// summaryJSON is what an agent reading the row sees: the current list.
func (a *app) summaryJSON(ctx context.Context, itemID int64) map[string]any {
	t, ok := a.summaryCurrent(ctx, a.store.Q, itemID)
	if !ok {
		return nil
	}
	var groups []map[string]any
	for _, g := range t.Groups {
		var items []map[string]any
		for _, x := range g.Items {
			if a.store.IsUnseenClosed(ctx, x.ID) {
				e := map[string]any{"id": x.Number, "from": x.Who, "what": x.What}
				if x.Doubt != "" {
					e["might_matter_because"] = x.Doubt
				}
				if x.Why != "" {
					e["reason"] = x.Why
				}
				items = append(items, e)
			}
		}
		if len(items) > 0 {
			groups = append(groups, map[string]any{"why_closed": g.Title, "reason": g.Reason, "items": items})
		}
	}
	return map[string]any{"intro": t.Intro, "groups": groups}
}
