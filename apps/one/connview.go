package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"lab/apps/one/repo"
)

// The rest of the Connections page (SPEC 23): the Notify and Claude rows, the
// source pictures, what each source brought in, the recent fetches, the last
// 14 days and the known limitations, which are all on the page once.

// sourcePicKeys are the sources the owner can give a picture: the sources of the
// rows (point 5) and the connections of the page.
var sourcePicKeys = map[string]string{
	"gmail": "Gmail", "outlook": "Office mail", "messenger": "Messenger", "whatsapp": "WhatsApp",
	"imessage": "iMessage", "sms": "SMS", "calendar": "Calendar",
	"bridge": "Chat bridge", "googlecal": "Google Calendar", "outlookcal": "Outlook Calendar", "icloud": "iCloud Calendar",
}

// srcPics remembers which sources have a picture and when it was set, so the
// rows can show it without a question to the database each.
type srcPics struct {
	mu     sync.RWMutex
	loaded bool
	m      map[string]int64
}

func (a *app) sourcePicURL(key string) string {
	a.pics.mu.RLock()
	loaded := a.pics.loaded
	a.pics.mu.RUnlock()
	if !loaded {
		a.reloadPics()
	}
	a.pics.mu.RLock()
	defer a.pics.mu.RUnlock()
	if at, ok := a.pics.m[key]; ok {
		return "/sources/" + key + "/picture?v=" + strconv.FormatInt(at, 10)
	}
	return ""
}

func (a *app) reloadPics() {
	m := a.store.SourcePictures(context.Background())
	a.pics.mu.Lock()
	a.pics.m, a.pics.loaded = m, true
	a.pics.mu.Unlock()
}

func sourceKey(r *http.Request) (string, bool) {
	k := r.PathValue("key")
	_, ok := sourcePicKeys[k]
	return k, ok
}

func (a *app) serveSourcePicture(w http.ResponseWriter, r *http.Request) {
	k, ok := sourceKey(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	ct, data, at, found := a.store.SourcePicture(r.Context(), k)
	if !found {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", ct)
	w.Header().Set("Cache-Control", "private, no-cache")
	w.Header().Set("Content-Disposition", "inline")
	http.ServeContent(w, r, "", time.Unix(at, 0), bytes.NewReader(data))
}

func (a *app) setSourcePicture(w http.ResponseWriter, r *http.Request) {
	k, ok := sourceKey(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxPicture+64<<10)
	if err := r.ParseMultipartForm(maxPicture); err != nil {
		a.renderConnectors(w, r, http.StatusBadRequest, "picture-error", "", pictureRefusal)
		return
	}
	f, _, err := r.FormFile("picture")
	if err != nil {
		a.renderConnectors(w, r, http.StatusBadRequest, "picture-error", "", pictureRefusal)
		return
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxPicture+1))
	ct, good := pictureType(data)
	if err != nil || len(data) == 0 || len(data) > maxPicture || !good {
		a.renderConnectors(w, r, http.StatusBadRequest, "picture-error", "", pictureRefusal)
		return
	}
	ctx := r.Context()
	err = a.store.Update(ctx, func(tx *repo.Tx) error {
		return tx.SetSourcePicture(ctx, k, ct, data, a.now().Unix())
	})
	if err != nil {
		http.Error(w, "The picture could not be saved. Try again.", http.StatusInternalServerError)
		return
	}
	a.reloadPics()
	http.Redirect(w, r, "/connectors?n=picture", http.StatusSeeOther)
}

func (a *app) removeSourcePicture(w http.ResponseWriter, r *http.Request) {
	k, ok := sourceKey(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	ctx := r.Context()
	err := a.store.Update(ctx, func(tx *repo.Tx) error { return tx.RemoveSourcePicture(ctx, k) })
	if err != nil {
		http.Error(w, "That did not work. Try again.", http.StatusInternalServerError)
		return
	}
	a.reloadPics()
	http.Redirect(w, r, "/connectors?n=nopicture", http.StatusSeeOther)
}

// --- Notify and Claude (SPEC 23) ---

const (
	keyNotifyLog   = "notify.log"      // JSON: the times of the notices sent, newest last
	keyNotifyErr   = "notify.err"      // the last failure to hand a notice over
	keyNotifyErrAt = "notify.err_at"   //
	keyAgentCall   = "agent.last_call" // when an agent last called with the token
)

// tell sends a notice to the owner's phone and keeps the time, so the Connections
// page can say when one last went and how many in the last day.
func (a *app) tell(title, body, link string) error {
	err := a.notify(title, body, link)
	ctx := context.Background()
	now := a.now()
	a.store.Update(ctx, func(tx *repo.Tx) error {
		if err != nil {
			tx.SetSettingBy(ctx, keyNotifyErr, oneLine(err.Error(), 200), "system", now)
			return tx.SetSettingBy(ctx, keyNotifyErrAt, strconv.FormatInt(now.Unix(), 10), "system", now)
		}
		var log []int64
		json.Unmarshal([]byte(tx.Setting(ctx, keyNotifyLog)), &log)
		log = append(log, now.Unix())
		if len(log) > 100 {
			log = log[len(log)-100:]
		}
		b, _ := json.Marshal(log)
		tx.SetSettingBy(ctx, keyNotifyErr, "", "system", now)
		return tx.SetSettingBy(ctx, keyNotifyLog, string(b), "system", now)
	})
	return err
}

// noteAgentCall keeps when an agent last called, at most once in half a minute.
func (a *app) noteAgentCall() {
	now := a.now().Unix()
	last := a.agentSeen.Load()
	if now-last < 30 || !a.agentSeen.CompareAndSwap(last, now) {
		return
	}
	ctx := context.Background()
	a.store.Update(ctx, func(tx *repo.Tx) error {
		return tx.SetSettingBy(ctx, keyAgentCall, strconv.FormatInt(now, 10), "system", a.now())
	})
}

type notifyView struct {
	Status, Cls string
	Last        string
	Count24     int
	Error       string
	ErrorAt     string
	Note        string
	Test        *lastView
	Notice      string
}

type claudeView struct {
	Status, Cls string
	LastCall    string
	Mode        string   // which of the two is at work
	Spend       string   // today's spend against the limit
	LastRan     string   // when the model last read an item
	NightRan    string   // when the night check last called the model
	Nights      []string // the last night checks
}

func (a *app) notifyViewFor(ctx context.Context) notifyView {
	v := notifyView{Status: statusOK, Cls: "st-ok"}
	if a.envText != "production" {
		// Previews never notify, and a local run only logs (SPEC 24).
		v.Status, v.Cls = statusNotConf, "st-stale"
		v.Note = "Notices are sent only from production; here they are dropped."
	}
	var log []int64
	json.Unmarshal([]byte(a.store.Setting(ctx, keyNotifyLog)), &log)
	since := a.now().Add(-24 * time.Hour).Unix()
	for _, t := range log {
		if t >= since {
			v.Count24++
		}
	}
	if len(log) > 0 {
		v.Last = a.clock(log[len(log)-1])
	}
	if e := a.store.Setting(ctx, keyNotifyErr); e != "" {
		at, _ := strconv.ParseInt(a.store.Setting(ctx, keyNotifyErrAt), 10, 64)
		v.Error, v.ErrorAt = e, a.clock(at)
		v.Status, v.Cls = statusError, "st-error"
	}
	if t, ok := a.store.LastTest(ctx, "notify"); ok {
		v.Test = &lastView{When: a.clock(t.At.Unix()), OK: t.OK, Result: strings.Split(t.Result, "\n")}
	}
	return v
}

func (a *app) claudeViewFor(ctx context.Context) claudeView {
	v := claudeView{Status: statusNotConf, Cls: "st-stale"}
	if n, _ := strconv.ParseInt(a.store.Setting(ctx, keyAgentCall), 10, 64); n > 0 {
		v.Status, v.Cls, v.LastCall = statusOK, "st-ok", a.clock(n)
	}
	a.claudeRowFor(ctx, &v)
	return v
}

// notifyTest is Test now on the Notify row: one notice to the phone.
func (a *app) notifyTest(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	err := a.tell("ONE: test notice", "This is a test of the notice to your phone.", "")
	a.record(ctx, "notify", err == nil, map[bool]string{true: "A test notice was handed over.", false: "The notice could not be handed over."}[err == nil])
	if err != nil {
		a.renderConnectors(w, r, http.StatusBadGateway, "notify", "", "The notice could not be handed over.")
		return
	}
	http.Redirect(w, r, "/connectors?n=notify", http.StatusSeeOther)
}

// --- what the page shows beside the rows ---

type picView struct {
	Key, Label, Img, Badge, Color string
	Has                           bool
}

type fetchRow struct {
	When, Source, Mode, Result, Counts, Error string
	Failed                                    bool
}

type dayRowView struct {
	Day   string
	Cells []int
}

type limitGroup struct {
	Name  string
	Items []string
}

type connExtra struct {
	Notify    notifyView
	Claude    claudeView
	Pics      map[string]picView
	Items24   map[string]int
	Fetches   []fetchRow
	DayCols   []string
	Days      []dayRowView
	Limits    []limitGroup
	PicNotice string
	PicError  string
	Removed   map[string]bool // stale sources taken off the page until they are read again
}

// mailLimits are the known limitations both mailboxes share (SPEC 23).
var mailLimits = []string{
	"Mail deleted before the first import cannot be imported.",
	"Only the notification mails are seen. ONE never signs in to the data box, and a notification is not the delivery itself.",
}

var notifyLimits = []string{"A notice reaches the phone only from production; previews never send one."}
var claudeLimits = []string{"An agent can read, comment and propose; it cannot approve, send or change a setting."}

var runLabels = map[string]string{
	"gmail": "Gmail", "outlook": "Office mail", "icloud": "iCloud Calendar", "outlookcal": "Outlook Calendar", "googlecal": "Google Calendar",
}

func (a *app) picFor(key string) picView {
	label := sourcePicKeys[key]
	v := picView{Key: key, Label: label, Badge: badge(label), Color: colorOf(key)}
	if u := a.sourcePicURL(key); u != "" {
		v.Img, v.Has = u, true
	}
	return v
}

// connExtraFor builds what the page needs beyond its rows.
func (a *app) connExtraFor(ctx context.Context, d *pageData) connExtra {
	x := connExtra{Notify: a.notifyViewFor(ctx), Claude: a.claudeViewFor(ctx), Pics: map[string]picView{}, Items24: map[string]int{}, Removed: map[string]bool{}}
	for _, src := range healthSources {
		x.Removed[src.Name] = a.sourceRemoved(ctx, src.Name)
	}
	for k := range sourcePicKeys {
		x.Pics[k] = a.picFor(k)
	}
	now := a.now()
	since24 := now.Add(-24 * time.Hour).Unix()
	by := a.store.ItemsBySource(ctx, since24)
	x.Items24["gmail"], x.Items24["outlook"] = by["gmail"], by["outlook"]
	for _, n := range []string{"messenger", "whatsapp", "imessage", "sms"} {
		x.Items24["bridge"] += by[n]
	}
	for _, k := range []string{"icloud", "outlookcal", "googlecal"} {
		x.Items24[k] = a.store.RunsStoredSince(ctx, k, since24)
	}
	// Recent fetches: the newest runs of every source.
	if runs, err := a.store.RecentFetches(ctx, 15); err == nil {
		for _, r := range runs {
			f := fetchRow{When: a.clock(r.StartedAt), Source: runLabels[r.Source], Mode: r.Mode, Result: "OK",
				Counts: strconv.Itoa(r.Fetched) + " read, " + strconv.Itoa(r.Stored) + " new", Error: r.Error}
			if f.Source == "" {
				f.Source = r.Source
			}
			if r.Status == "failed" {
				f.Result, f.Failed = "Failed", true
			}
			x.Fetches = append(x.Fetches, f)
		}
	}
	// The last 14 days: items per day and source.
	_, off := now.In(a.loc).Zone()
	today := (now.Unix() + int64(off)) / 86400
	first := today - 13
	startAt := first*86400 - int64(off)
	cols := []string{"gmail", "outlook"}
	cell := map[int64]map[string]int{}
	add := func(day int64, src string, n int) {
		if day < first || day > today || n == 0 {
			return
		}
		if cell[day] == nil {
			cell[day] = map[string]int{}
		}
		cell[day][src] += n
		if !slices.Contains(cols, src) {
			cols = append(cols, src)
		}
	}
	for _, c := range a.store.ItemsPerDay(ctx, startAt, int64(off)) {
		add(c.Day, c.Source, c.N)
	}
	for _, k := range []string{"icloud", "outlookcal", "googlecal"} {
		for day, n := range a.store.RunsPerDay(ctx, k, startAt, int64(off)) {
			add(day, k, n)
		}
	}
	for _, c := range cols {
		l := runLabels[c]
		if l == "" {
			l = sourceLabel(c)
		}
		x.DayCols = append(x.DayCols, l)
	}
	for day := today; day >= first; day-- {
		row := dayRowView{Day: time.Unix(day*86400, 0).UTC().Format("Mon 2 Jan")}
		for _, c := range cols {
			row.Cells = append(row.Cells, cell[day][c])
		}
		x.Days = append(x.Days, row)
	}
	// The known limitations, each sentence once on the page. What every
	// calendar shares is listed once, under "All calendars".
	own := func(l []string) []string {
		var out []string
		for _, s := range l {
			if !slices.Contains(calendarLimits, s) {
				out = append(out, s)
			}
		}
		return out
	}
	groups := []limitGroup{
		{"Gmail and Office mail", mailLimits},
		{"Chat bridge", d.Bridge.Limits},
		{"Google Calendar", own(d.Google.Limits)},
		{"Outlook Calendar", own(d.OutCal.Limits)},
		{"iCloud Calendar", own(d.ICloud.Limits)},
		{"All calendars", calendarLimits},
		{"Notify", notifyLimits},
		{"Claude", claudeLimits},
	}
	for _, g := range groups {
		if len(g.Items) > 0 {
			x.Limits = append(x.Limits, g)
		}
	}
	return x
}

// --- a stale source taken off the page (SPEC 23) ---

const keyRemovedPrefix = "health.removed."

// lastActivity is when a source was last read (or heard from).
func (a *app) lastActivity(ctx context.Context, src string) int64 {
	if src == srcBridge {
		return a.bridgeHeardAt(ctx)
	}
	return a.store.LastRunTime(ctx, src)
}

// sourceRemoved says whether the owner took a stale source off the page and it has
// not been read since: it is then not shown, not counted by the dot and not in
// the strip. A new read brings it back by itself.
func (a *app) sourceRemoved(ctx context.Context, src string) bool {
	at, _ := strconv.ParseInt(a.store.Setting(ctx, keyRemovedPrefix+src), 10, 64)
	return at > 0 && a.lastActivity(ctx, src) <= at
}

func (a *app) removeSource(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")
	known := false
	for _, s := range healthSources {
		known = known || s.Name == key
	}
	if !known {
		http.NotFound(w, r)
		return
	}
	ctx := r.Context()
	// Only a stale source can be removed: one that works or has failed stays in sight.
	for _, st := range a.sourceStatesAll(ctx) {
		if st.Key == key && st.Status != statusStale {
			http.Error(w, "Only a stale source can be removed.", http.StatusConflict)
			return
		}
	}
	now := a.now()
	err := a.store.Update(ctx, func(tx *repo.Tx) error {
		return tx.SetSettingBy(ctx, keyRemovedPrefix+key, strconv.FormatInt(max(now.Unix(), a.lastActivity(ctx, key)), 10), "owner", now)
	})
	if err != nil {
		http.Error(w, "That did not work. Try again.", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/connectors", http.StatusSeeOther)
}
