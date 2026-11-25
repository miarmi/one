package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"lab/apps/one/repo"
	"lab/platform/appkit"
)

// SPEC 20: the iCloud calendars, read by the app itself over CalDAV, on the
// timer and when the owner presses Test now. The Apple ID and the app-specific
// password are kept in $DATA_DIR/secrets/icloud.json (0600): never shown
// again, never in a log, never through /api/, never copied into a preview.
// The app only reads (caldav.go has no way to write).

const (
	icloudSecretFile = "icloud.json"
	keyICloudCals    = "icloud_calendars" // the calendars found and the owner's choices, as JSON
	keyICloudLogin   = "icloud_needs_login"
	icloudConn       = "icloud" // the key of its last Test now
	icloudTimeout    = 3 * time.Minute

	msgICloudAuth = "Apple refused the sign-in. Check the Apple ID and the app-specific password."
	msgICloudNet  = "iCloud could not be reached. ONE tries again at the next read."
	msgICloudRead = "iCloud answered with something ONE could not read. ONE tries again at the next read."
)

var errCalOff = errors.New("calendar is not connected")

type icloudSecret struct {
	AppleID  string `json:"apple_id"`
	Password string `json:"password"`
}

// icloudCal is one calendar found in the account, with the owner's choices.
type icloudCal struct {
	Key    string `json:"key"`
	Href   string `json:"href"`
	Name   string `json:"name"`
	Color  string `json:"color,omitempty"`
	Show   bool   `json:"show"`
	Person int64  `json:"person,omitempty"` // a person of People; 0 is the owner's own
}

func (c icloudCal) label() string { return "iCloud · " + c.Name }

// Birthday, holiday and suggestion calendars start switched off (SPEC 20).
var icloudOffByDefault = regexp.MustCompile(`(?i)birthday|geburtstag|narozenin|holiday|feiertag|svátk|svatk|státní|statni|siri|suggest|návrh|navrh|reminder|připomín|pripomin|erinnerung`)

func (a *app) icloudSecret() (icloudSecret, bool) {
	var s icloudSecret
	ok := readSecret(a.secretsDir, icloudSecretFile, &s) && s.AppleID != "" && s.Password != ""
	return s, ok
}

func (a *app) icloudNeedsLogin(ctx context.Context) bool {
	return a.store.Setting(ctx, keyICloudLogin) == "1"
}

func (a *app) setICloudNeedsLogin(ctx context.Context, on bool) {
	v := ""
	if on {
		v = "1"
	}
	if a.store.Setting(ctx, keyICloudLogin) != v {
		a.store.SetSettingBy(context.WithoutCancel(ctx), keyICloudLogin, v, "system", a.now())
	}
}

func (a *app) icloudCals(ctx context.Context) []icloudCal {
	var cals []icloudCal
	if v := a.store.Setting(ctx, keyICloudCals); v != "" {
		json.Unmarshal([]byte(v), &cals)
	}
	return cals
}

// saveICloudCals writes the list only when it differs from what is stored,
// so a read of unchanged calendars writes nothing.
func (a *app) saveICloudCals(ctx context.Context, cals []icloudCal, by string) error {
	b, err := json.Marshal(cals)
	if err != nil {
		return err
	}
	if a.store.Setting(ctx, keyICloudCals) == string(b) {
		return nil
	}
	return a.store.SetSettingBy(ctx, keyICloudCals, string(b), by, a.now())
}

// mergeICloudCals keeps the owner's choices for the calendars still there and
// gives new ones their defaults.
func mergeICloudCals(old []icloudCal, found []calInfo) []icloudCal {
	prev := map[string]icloudCal{}
	for _, c := range old {
		prev[c.Key] = c
	}
	out := []icloudCal{}
	for _, f := range found {
		key := calKeyOf(srcICloud, f.Href)
		c, ok := prev[key]
		if !ok {
			c = icloudCal{Key: key, Show: !icloudOffByDefault.MatchString(f.Name)}
		}
		c.Href, c.Name, c.Color = f.Href, strings.TrimSpace(f.Name), f.Color
		if c.Name == "" {
			c.Name = "Calendar"
		}
		out = append(out, c)
	}
	sort.SliceStable(out, func(i, j int) bool { return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name) })
	return out
}

func (a *app) icloudClient(sec icloudSecret) *calDAVClient {
	return &calDAVClient{BaseURL: a.icloudBase, User: sec.AppleID, Pass: sec.Password}
}

// plainICloud words a failed read for the page. It never holds the password.
func plainICloud(err error) string {
	switch {
	case errors.Is(err, errCalAuth):
		return msgICloudAuth
	case errors.Is(err, errCalUnreadable):
		return msgICloudRead
	case errors.Is(err, errCalOff):
		return "iCloud Calendar is not connected."
	}
	return msgICloudNet
}

// clipPayload keeps at most 64 KB of what a calendar delivered with a version.
func clipPayload(s string) string {
	if len(s) > 64<<10 {
		return strings.ToValidUTF8(s[:64<<10], "")
	}
	return s
}

// expandCalendar turns the objects of one calendar into the entries inside
// the window: it groups the components by event id, expands repeating events
// and names every entry in the calendar by its own address.
func expandCalendar(res []calResource, w agendaWindow, loc *time.Location, calKey string) []calInstance {
	type group struct {
		evs     []vevent
		payload string
	}
	groups := map[string]*group{}
	var order []string
	for _, r := range res {
		for _, ev := range parseVEvents(r.Data, loc) {
			uid := ev.UID
			if uid == "" {
				uid = "href-" + path.Base(strings.TrimRight(r.Href, "/"))
			}
			ev.UID = uid
			g := groups[uid]
			if g == nil {
				g = &group{payload: clipPayload(r.Data)}
				groups[uid] = g
				order = append(order, uid)
			}
			g.evs = append(g.evs, ev)
		}
	}
	var out []calInstance
	for _, uid := range order {
		g := groups[uid]
		for _, in := range expandEvents(g.evs, w.From, w.To, loc, g.payload) {
			in.SourceID = calKey + "\x1f" + uid + "\x1f" + in.Instance
			out = append(out, in)
		}
	}
	return out
}

// syncICloud reads the shown calendars once. force reads even while the
// sign-in is marked as refused (Test now, to see whether it works again).
func (a *app) syncICloud(ctx context.Context, force bool) (err error) {
	a.icloudMu.Lock()
	defer a.icloudMu.Unlock()
	sec, ok := a.icloudSecret()
	if !ok {
		return errCalOff
	}
	if !force && a.icloudNeedsLogin(ctx) {
		return errNeedsLogin // SPEC 24: paused until the password is entered again
	}
	core := a.imp.core
	run, err := core.startRun(ctx, icloudSrc, "sync")
	if err != nil {
		return err
	}
	defer func() {
		core.endRun(ctx, icloudSrc, run, err, plainICloud)
		switch {
		case err == nil:
			a.setICloudNeedsLogin(ctx, false)
		case errors.Is(err, errCalAuth):
			a.setICloudNeedsLogin(ctx, true)
		}
		a.checkSource(ctx, icloudSrc) // SPEC 24: one notice per failure
	}()
	rctx, cancel := context.WithTimeout(ctx, icloudTimeout)
	defer cancel()
	cl := a.icloudClient(sec)
	found, err := cl.Calendars(rctx)
	if err != nil {
		return err
	}
	cals := mergeICloudCals(a.icloudCals(ctx), found)
	if err = a.saveICloudCals(ctx, cals, "system"); err != nil {
		return err
	}
	w := a.agendaWindow(ctx)
	var total storeStats
	var firstErr error
	for _, c := range cals {
		if !c.Show {
			continue
		}
		res, rerr := cl.Resources(rctx, c.Href, w.From, w.To)
		if rerr != nil {
			if errors.Is(rerr, errCalAuth) {
				return rerr
			}
			if firstErr == nil {
				firstErr = rerr
			}
			continue // this calendar keeps what it had; the others go on
		}
		st, serr := a.storeCalendar(ctx, run.ID, srcICloud, c.Key, c.label(), w, expandCalendar(res, w, a.loc, c.Key), true)
		if serr != nil {
			return serr
		}
		total.add(st)
	}
	run.Fetched, run.Stored, run.ItemsMade, run.Failed = total.Read, total.stored(), total.New, total.Failed
	return firstErr
}

// --- the section of the Connectors page ---

type icloudCalView struct {
	Key    string
	Name   string
	Show   bool
	Person int64
	Field  string
}

type icloudPersonOpt struct {
	ID    int64
	Label string
}

type icloudView struct {
	sectionView
	AppleID   string
	Calendars []icloudCalView
	People    []icloudPersonOpt
	Limits    []string
}

var icloudNotices = map[string]string{
	"icloud":     "Connected.",
	"icloudsave": "Saved.",
	"icloudoff":  "Disconnected. The password and the events it brought are removed.",
}

var calendarLimits = []string{
	"ONE only reads calendars. It never creates, changes or deletes an event.",
	"Only events are read, not reminders or tasks.",
	"Events outside the agenda window are not read; a repeating event is shown for the days inside it.",
	"An event that was moved outside the window shows as removed until it is back inside it.",
	"Repeating events that change from one occurrence on (this and all following) are not followed.",
}

func (a *app) icloudViewFor(ctx context.Context, notice, errText string) icloudView {
	v := icloudView{sectionView: sectionView{Notice: icloudNotices[notice], Error: errText}, Limits: calendarLimits}
	sec, ok := a.icloudSecret()
	v.Connected, v.AppleID = ok, sec.AppleID
	v.Test = a.lastView(ctx, icloudConn)
	if !ok {
		v.Status, v.StatusCls = statusNotConf, "st-stale"
		return v
	}
	v.Status, v.StatusCls = status(true, v.Test)
	v.applyState(a.sourceStateOf(ctx, icloudSrc), v.Test)
	if ps, err := a.store.ListPeople(ctx, "", 500, 0); err == nil {
		for _, p := range ps {
			v.People = append(v.People, icloudPersonOpt{ID: p.ID, Label: p.Shown()})
		}
	}
	for i, c := range a.icloudCals(ctx) {
		v.Calendars = append(v.Calendars, icloudCalView{Key: c.Key, Name: c.Name, Show: c.Show, Person: c.Person, Field: strconv.Itoa(i)})
	}
	return v
}

// Whose says whether this person is the calendar's chosen one (template).
func (c icloudCalView) Whose(id int64) bool { return c.Person == id }

// icloudConnect checks the Apple ID and the app-specific password with
// Apple and keeps them only when Apple lets them in. Nothing typed is shown
// back on an error.
func (a *app) icloudConnect(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 8<<10)
	id := strings.TrimSpace(r.FormValue("apple_id"))
	pass := strings.TrimSpace(r.FormValue("password"))
	if id == "" || pass == "" || len(id) > 200 || len(pass) > 200 {
		a.renderConnectors(w, r, http.StatusBadRequest, "icloud", "", "Enter the Apple ID and an app-specific password.")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), icloudTimeout)
	defer cancel()
	a.opMu.Lock()
	defer a.opMu.Unlock()
	sec := icloudSecret{AppleID: id, Password: pass}
	if _, err := a.icloudClient(sec).Calendars(ctx); err != nil {
		a.renderConnectors(w, r, http.StatusBadRequest, "icloud", "", plainICloud(err))
		return
	}
	if err := writeSecret(a.secretsDir, icloudSecretFile, sec); err != nil {
		appkit.Error("saving the iCloud secret", err)
		a.renderConnectors(w, r, http.StatusInternalServerError, "icloud", "", "The password could not be saved. Try again.")
		return
	}
	a.store.ForgetTest(r.Context(), icloudConn)
	a.setICloudNeedsLogin(r.Context(), false) // SPEC 24: fetching resumes by itself
	a.syncICloud(r.Context(), true)           // the calendars are listed at once; the events follow the timer's way
	http.Redirect(w, r, "/connectors?n=icloud#icloud", http.StatusSeeOther)
}

// icloudDisconnect removes the password, the choices and the events it brought.
func (a *app) icloudDisconnect(w http.ResponseWriter, r *http.Request) {
	a.icloudMu.Lock()
	defer a.icloudMu.Unlock()
	ctx := r.Context()
	if err := removeSecret(a.secretsDir, icloudSecretFile); err != nil {
		appkit.Error("removing the iCloud secret", err)
		http.Error(w, "could not remove the password", http.StatusInternalServerError)
		return
	}
	err := a.store.Update(ctx, func(tx *repo.Tx) error {
		n, err := tx.DeleteEventsOf(ctx, srcICloud)
		if err != nil {
			return err
		}
		if err := tx.SetSettingBy(ctx, keyICloudCals, "", "owner", a.now()); err != nil {
			return err
		}
		return tx.InsertEvent(ctx, a.now().Unix(), 0, "owner", "setting_changed", map[string]any{"connector": "iCloud Calendar", "to": "disconnected", "events_removed": n})
	})
	if err != nil {
		appkit.Error("removing the iCloud events", err)
		http.Error(w, "could not remove the events", http.StatusInternalServerError)
		return
	}
	a.store.ForgetTest(ctx, icloudConn)
	a.setICloudNeedsLogin(ctx, false)
	http.Redirect(w, r, "/connectors?n=icloudoff#icloud", http.StatusSeeOther)
}

// icloudSave keeps the owner's choices per calendar, then reads again so the
// agenda follows them.
func (a *app) icloudSave(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	ctx := r.Context()
	cals := a.icloudCals(ctx)
	known := map[int64]bool{}
	if ps, err := a.store.ListPeople(ctx, "", 1000, 0); err == nil {
		for _, p := range ps {
			known[p.ID] = true
		}
	}
	for i := range cals {
		f := strconv.Itoa(i)
		// The calendar is named by its key too, so a list that changed in
		// between cannot mix up the choices.
		if r.PostFormValue("key"+f) != cals[i].Key {
			continue
		}
		cals[i].Show = r.PostFormValue("show"+f) == "1"
		cals[i].Person = 0
		if p, err := strconv.ParseInt(r.PostFormValue("whose"+f), 10, 64); err == nil && known[p] {
			cals[i].Person = p
		}
	}
	if err := a.saveICloudCals(ctx, cals, "owner"); err != nil {
		appkit.Error("saving the iCloud calendars", err)
		http.Error(w, "could not save", http.StatusInternalServerError)
		return
	}
	a.syncICloud(ctx, false)
	http.Redirect(w, r, "/connectors?n=icloudsave#icloud", http.StatusSeeOther)
}

// icloudTest is Test now: it reads now and says what came of it.
func (a *app) icloudTest(w http.ResponseWriter, r *http.Request) {
	if _, ok := a.icloudSecret(); !ok {
		a.renderConnectors(w, r, http.StatusBadRequest, "icloud", "", "Connect iCloud Calendar first.")
		return
	}
	ctx := r.Context()
	err := a.syncICloud(ctx, true)
	if err != nil {
		a.record(ctx, icloudConn, false, "Could not read: "+plainICloud(err))
	} else {
		shown := 0
		for _, c := range a.icloudCals(ctx) {
			if c.Show {
				shown++
			}
		}
		a.record(ctx, icloudConn, true, fmt.Sprintf("Read %s shown in the agenda. %d entries in the agenda now.", plural(int64(shown), "calendar"), a.store.CountEvents(ctx, srcICloud)))
	}
	http.Redirect(w, r, "/connectors#icloud", http.StatusSeeOther)
}
