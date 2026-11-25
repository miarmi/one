package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"lab/apps/one/repo"
	"lab/platform/appkit"
)

// SPEC 15, 20, 23, 24: the Google calendars, read by the app itself through
// each calendar's "Secret address in iCal format" (decided with the owner on
// 2026-11-19). The address can only be read: the one request ever sent to
// Google is a GET of that address. No sign-in, no Google Cloud project.
//
// The address is a secret. It is kept in $DATA_DIR/secrets/googlecal.json
// (0600), never shown again (the page shows the name and the last four
// characters of its secret part), never in a log, never in an error text,
// never through /api/ and never copied into a preview. Everything that is
// not secret (name, choices, validators) is kept in the settings.

const (
	googleSecretFile = "googlecal.json"
	keyGoogleCals    = "google_calendars" // the calendars and the owner's choices, as JSON
	googleConn       = "googlecal"        // the key of its last Test now
	googleHost       = "calendar.google.com"
	googleTimeout    = 3 * time.Minute // one read of all the calendars
	googleFeedTime   = 60 * time.Second
	maxGoogleCals    = 20
	maxGoogleName    = 80
	maxGoogleRedirs  = 3
)

// googleFeedMax is the most one feed may hold. A variable so a test can
// lower it.
var googleFeedMax int64 = 16 << 20

const (
	msgGoogleAddress = "That is not the secret address in iCal format. In Google Calendar open the calendar's settings, find Secret address in iCal format and paste it here. It starts with https://calendar.google.com/calendar/ical/ and ends with basic.ics."
	msgGoogleNet     = "Google could not be reached. ONE tries again at the next read."
)

var (
	errGoogleRefused = errors.New("google refused the calendar")
	errGoogleGone    = errors.New("google does not know the calendar")
	errGoogleTooBig  = errors.New("the calendar is too large")
	errGoogleCut     = errors.New("the calendar answer was cut off")
	errGoogleMoved   = errors.New("google sent the read somewhere else")
	errGoogleBusy    = errors.New("google answered with an error")
	errGoogleNet     = errors.New("google could not be reached")
	errGoogleNoAddr  = errors.New("the address of the calendar is missing")
)

// googleCalErr names the calendar a failure belongs to. It holds the owner's
// name for the calendar and never the address.
type googleCalErr struct {
	Name string
	Err  error
}

func (e *googleCalErr) Error() string { return e.Err.Error() }
func (e *googleCalErr) Unwrap() error { return e.Err }

// plainGoogle words a failed read for the page: what happened and what to do.
// It never holds the address.
func plainGoogle(err error) string {
	var ce *googleCalErr
	name := ""
	if errors.As(err, &ce) {
		name = ce.Name
	}
	q := "the calendar"
	if name != "" {
		q = "the calendar “" + name + "”"
	}
	cap := strings.ToUpper(q[:1]) + q[1:]
	switch {
	case errors.Is(err, errCalOff):
		return "Google Calendar is not connected."
	case errors.Is(err, errGoogleRefused):
		return "Google refused to give " + q + ". Open the calendar's settings in Google, copy its secret address in iCal format, then disconnect the calendar here and connect it again."
	case errors.Is(err, errGoogleGone):
		return "Google no longer knows the address of " + q + ", probably because it was reset. Copy the new secret address from Google, then disconnect the calendar here and connect it again."
	case errors.Is(err, errGoogleTooBig):
		return cap + " is larger than the " + strconv.FormatInt(googleFeedMax>>20, 10) + " MB ONE reads. Disconnect it here, or make a smaller calendar in Google."
	case errors.Is(err, errGoogleCut), errors.Is(err, errCalUnreadable):
		return "Google answered with something ONE could not read for " + q + ". Nothing was removed; ONE tries again at the next read."
	case errors.Is(err, errGoogleMoved):
		return "Google sent ONE to another address for " + q + ", which ONE does not follow. Copy the secret address in iCal format from Google again, then disconnect the calendar here and connect it again."
	case errors.Is(err, errGoogleNoAddr):
		return "The address of " + q + " is missing on the server. Disconnect the calendar here and connect it again."
	case errors.Is(err, errGoogleBusy):
		return "Google answered with an error for " + q + ". ONE tries again at the next read."
	}
	return msgGoogleNet
}

// --- the address ---

var (
	googlePathRE    = regexp.MustCompile(`^/calendar/ical/[A-Za-z0-9._%@+\-]{1,200}/[A-Za-z0-9._\-]{1,120}/basic\.ics$`)
	googleSegmentRE = regexp.MustCompile(`^[A-Za-z0-9._\-]+$`)
)

// parseGoogleAddress accepts only https://calendar.google.com/calendar/ical/
// <calendar>/<secret part>/basic.ics: that exact host, no port, no sign-in
// data, no query and nothing after the file name. It gives the address in a
// fixed spelling and the last four characters of the secret part.
func parseGoogleAddress(raw string) (addr, tail string, ok bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" || len(raw) > 400 || strings.ContainsAny(raw, " \t\r\n\\") {
		return "", "", false
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Opaque != "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return "", "", false
	}
	if strings.ToLower(u.Host) != googleHost || u.Port() != "" {
		return "", "", false
	}
	p := u.EscapedPath()
	if !googlePathRE.MatchString(p) || strings.Contains(p, "..") {
		return "", "", false
	}
	if lp := strings.ToLower(p); strings.Contains(lp, "%2e") || strings.Contains(lp, "%2f") || strings.Contains(lp, "%5c") {
		return "", "", false // no hidden dots or slashes
	}
	parts := strings.Split(p, "/") // "", calendar, ical, <id>, <secret>, basic.ics
	secret := parts[4]
	if !googleSegmentRE.MatchString(secret) {
		return "", "", false
	}
	tail = secret
	if len(tail) > 4 {
		tail = tail[len(tail)-4:]
	}
	return "https://" + googleHost + p, tail, true
}

// --- what is kept ---

// googleCal is one calendar with the owner's choices. It holds no address.
type googleCal struct {
	Key    string `json:"key"`
	Name   string `json:"name"`
	Tail   string `json:"tail"` // the last four characters of the secret part, to tell calendars apart
	Show   bool   `json:"show"`
	Person int64  `json:"person,omitempty"` // a person of People; 0 is the owner's own
	// What the server last gave for the window From..To, so the next read can
	// ask "unchanged?" (If-None-Match, If-Modified-Since).
	ETag     string `json:"etag,omitempty"`
	Modified string `json:"modified,omitempty"`
	From     int64  `json:"from,omitempty"`
	To       int64  `json:"to,omitempty"`
}

func (c googleCal) label() string { return "Google · " + c.Name }

// googleSecret holds the addresses by calendar key.
type googleSecret struct {
	Addresses map[string]string `json:"addresses"`
}

func (a *app) googleSecret() googleSecret {
	var s googleSecret
	readSecret(a.secretsDir, googleSecretFile, &s)
	if s.Addresses == nil {
		s.Addresses = map[string]string{}
	}
	return s
}

func (a *app) googleCals(ctx context.Context) []googleCal {
	var cals []googleCal
	if v := a.store.Setting(ctx, keyGoogleCals); v != "" {
		json.Unmarshal([]byte(v), &cals)
	}
	return cals
}

// updateGoogleCals changes the list under a lock, so a read that saves its
// validators cannot undo what the owner just added or removed. It writes only
// when the list differs from what is stored.
func (a *app) updateGoogleCals(ctx context.Context, by string, fn func([]googleCal) []googleCal) error {
	a.googleCfgMu.Lock()
	defer a.googleCfgMu.Unlock()
	return a.saveGoogleCalsLocked(ctx, by, fn)
}

func (a *app) saveGoogleCalsLocked(ctx context.Context, by string, fn func([]googleCal) []googleCal) error {
	cals := fn(a.googleCals(ctx))
	if cals == nil {
		cals = []googleCal{}
	}
	b, err := json.Marshal(cals)
	if err != nil {
		return err
	}
	if a.store.Setting(ctx, keyGoogleCals) == string(b) {
		return nil
	}
	return a.store.SetSettingBy(context.WithoutCancel(ctx), keyGoogleCals, string(b), by, a.now())
}

func newCalKey() string {
	var b [8]byte
	rand.Read(b[:])
	return srcGoogleCal + ":" + hex.EncodeToString(b[:])
}

// --- reading one feed ---

type googleFeed struct {
	Body        string
	NotModified bool
	ETag        string
	Modified    string
}

// googleHTTPClient is the client for one read: a timeout, and a redirect
// rule that follows only a short chain inside calendar.google.com over https
// and answers anything else with the redirect itself (never followed).
func (a *app) googleHTTPClient() *http.Client {
	cl := http.Client{Timeout: googleFeedTime}
	if a.googleHTTP != nil {
		cl = *a.googleHTTP
		if cl.Timeout == 0 {
			cl.Timeout = googleFeedTime
		}
	}
	cl.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) > maxGoogleRedirs || req.URL.Scheme != "https" || strings.ToLower(req.URL.Host) != googleHost || req.URL.User != nil {
			return http.ErrUseLastResponse
		}
		return nil
	}
	return &cl
}

// fetchGoogle makes the one request of this file: a GET of the saved
// address, with a size limit and a timeout. A 304 says nothing changed. A
// body that is cut off, or is not a whole calendar, is an error: the caller
// then removes nothing. No error text holds the address.
func (a *app) fetchGoogle(ctx context.Context, address, etag, modified string) (googleFeed, error) {
	var out googleFeed
	if _, _, ok := parseGoogleAddress(address); !ok {
		return out, errGoogleNoAddr
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
	if err != nil {
		return out, errGoogleNoAddr
	}
	req.Header.Set("Accept", "text/calendar, text/plain;q=0.5")
	req.Header.Set("User-Agent", "ONE")
	if etag != "" {
		req.Header.Set("If-None-Match", etag)
	}
	if modified != "" {
		req.Header.Set("If-Modified-Since", modified)
	}
	resp, err := a.googleHTTPClient().Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return out, ctx.Err()
		}
		return out, errGoogleNet // the error of the client names the address: say nothing of it
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusNotModified && (etag != "" || modified != ""):
		out.NotModified = true
		return out, nil
	case resp.StatusCode == 401 || resp.StatusCode == 403:
		return out, errGoogleRefused
	case resp.StatusCode == 404 || resp.StatusCode == 410:
		return out, errGoogleGone
	case resp.StatusCode >= 300 && resp.StatusCode < 400:
		return out, errGoogleMoved
	case resp.StatusCode != http.StatusOK:
		return out, errGoogleBusy
	}
	if resp.ContentLength > googleFeedMax {
		return out, errGoogleTooBig
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, googleFeedMax+1))
	if err != nil {
		if ctx.Err() != nil {
			return out, ctx.Err()
		}
		return out, errGoogleCut
	}
	if int64(len(data)) > googleFeedMax {
		return out, errGoogleTooBig
	}
	body := strings.TrimPrefix(string(data), "\xef\xbb\xbf")
	if !wholeCalendar(body) {
		return out, errGoogleCut
	}
	out.Body = body
	out.ETag = resp.Header.Get("ETag")
	out.Modified = resp.Header.Get("Last-Modified")
	return out, nil
}

// wholeCalendar says whether the text starts and ends as one iCalendar
// object, so a feed that was cut off is told from a complete one.
func wholeCalendar(s string) bool {
	s = strings.TrimSpace(s)
	return len(s) >= 30 && strings.EqualFold(s[:15], "BEGIN:VCALENDAR") && strings.EqualFold(s[len(s)-13:], "END:VCALENDAR")
}

// feedResources splits a whole feed into one object per event id, each with
// its own components (a repeating event and the changed occurrences go
// together), so every stored version keeps only its own event as payload.
func feedResources(feed string) []calResource {
	type group struct {
		blocks []string
	}
	groups := map[string]*group{}
	var order []string
	var cur []string
	depth := 0
	uid := ""
	n := 0
	for _, line := range unfoldICal(feed) {
		p, ok := parseICalLine(line)
		if !ok {
			continue
		}
		val := strings.ToUpper(strings.TrimSpace(p.value))
		switch {
		case p.name == "BEGIN" && val == "VEVENT":
			depth++
			if depth == 1 {
				cur, uid = nil, ""
			}
		case p.name == "BEGIN" && depth > 0:
			depth++
		case p.name == "END" && depth > 0:
			depth--
			if depth == 0 && val == "VEVENT" {
				cur = append(cur, "END:VEVENT")
				n++
				key := uid
				if key == "" {
					key = "n-" + strconv.Itoa(n)
				}
				g := groups[key]
				if g == nil {
					g = &group{}
					groups[key] = g
					order = append(order, key)
				}
				g.blocks = append(g.blocks, strings.Join(cur, "\r\n"))
				cur = nil
				continue
			}
		case p.name == "UID" && depth == 1:
			uid = strings.TrimSpace(p.value)
		}
		if depth > 0 {
			cur = append(cur, line)
		}
	}
	out := make([]calResource, 0, len(order))
	for _, k := range order {
		data := "BEGIN:VCALENDAR\r\nVERSION:2.0\r\n" + strings.Join(groups[k].blocks, "\r\n") + "\r\nEND:VCALENDAR\r\n"
		out = append(out, calResource{Href: "event-" + k, Data: data})
	}
	return out
}

// --- the timer's read ---

// syncGoogleCal reads the shown calendars once, one after the other. A
// calendar that fails keeps what it had and the others go on. Events that are
// gone from a feed are marked removed only after a complete, successful read
// of that feed (storeCalendar with complete set below); a 304, a cut or a
// refused read changes nothing.
func (a *app) syncGoogleCal(ctx context.Context, _ bool) (err error) {
	a.googleMu.Lock()
	defer a.googleMu.Unlock()
	cals := a.googleCals(ctx)
	if len(cals) == 0 {
		return errCalOff
	}
	sec := a.googleSecret()
	core := a.imp.core
	run, err := core.startRun(ctx, googleCalSrc, "sync")
	if err != nil {
		return err
	}
	defer func() {
		core.endRun(ctx, googleCalSrc, run, err, plainGoogle)
		a.checkSource(ctx, googleCalSrc) // SPEC 24: one notice per failure
	}()
	rctx, cancel := context.WithTimeout(ctx, googleTimeout)
	defer cancel()
	w := a.agendaWindow(ctx)
	var total storeStats
	var firstErr error
	fail := func(c googleCal, e error) {
		if firstErr == nil {
			firstErr = &googleCalErr{Name: c.Name, Err: e}
		}
	}
	for _, c := range cals {
		if !c.Show {
			continue
		}
		addr := sec.Addresses[c.Key]
		if addr == "" {
			fail(c, errGoogleNoAddr)
			continue
		}
		etag, mod := "", ""
		if c.From == w.From.Unix() && c.To == w.To.Unix() {
			etag, mod = c.ETag, c.Modified // a window that moved needs a whole read
		}
		fctx, fcancel := context.WithTimeout(rctx, googleFeedTime)
		feed, ferr := a.fetchGoogle(fctx, addr, etag, mod)
		fcancel()
		if ferr != nil {
			if errors.Is(ferr, context.Canceled) || ctx.Err() != nil {
				return ferr
			}
			fail(c, ferr)
			continue
		}
		if feed.NotModified {
			continue
		}
		ins := expandCalendar(feedResources(feed.Body), w, a.loc, c.Key)
		st, serr := a.storeCalendar(ctx, run.ID, srcGoogleCal, c.Key, c.label(), w, ins, true)
		if serr != nil {
			return serr
		}
		total.add(st)
		if a.calRetry(c.Key, st.Failed) {
			continue // an entry failed: keep no ETag, so the next round reads this feed whole again
		}
		key := c.Key
		a.updateGoogleCals(ctx, "system", func(list []googleCal) []googleCal {
			for i := range list {
				if list[i].Key == key {
					list[i].ETag, list[i].Modified, list[i].From, list[i].To = feed.ETag, feed.Modified, w.From.Unix(), w.To.Unix()
				}
			}
			return list
		})
	}
	run.Fetched, run.Stored, run.ItemsMade, run.Failed = total.Read, total.stored(), total.New, total.Failed
	return firstErr
}

// --- the section of the Connectors page ---

type googleCalView struct {
	Key    string
	Name   string
	Tail   string
	Show   bool
	Person int64
	Field  string
}

type googleView struct {
	sectionView
	Calendars []googleCalView
	People    []icloudPersonOpt
	Limits    []string
}

var googleNotices = map[string]string{
	"google":     "Connected.",
	"googlesave": "Saved.",
	"googleoff":  "Disconnected. The address and the events it brought are removed.",
}

// googleLimits are the known limitations of this connection, in plain words.
var googleLimits = append([]string{
	"ONE only reads. The secret address can only be read, so ONE can never create, change or delete an event in Google.",
	"The address can lag behind the calendar: a change made in Google may take hours to show up here.",
	"Anyone who has the secret address can read the whole calendar. If it gets out, reset it in Google; ONE then stops reading that calendar until you connect it again with the new address.",
}, calendarLimits[1:]...)

func (c googleCalView) Whose(id int64) bool { return c.Person == id }

func (a *app) googleViewFor(ctx context.Context, notice, errText string) googleView {
	v := googleView{sectionView: sectionView{Notice: googleNotices[notice], Error: errText}, Limits: googleLimits}
	cals := a.googleCals(ctx)
	v.Connected = len(cals) > 0
	v.Test = a.lastView(ctx, googleConn)
	if !v.Connected {
		v.Status, v.StatusCls = statusNotConf, "st-stale"
		return v
	}
	v.Status, v.StatusCls = status(true, v.Test)
	v.applyState(a.sourceStateOf(ctx, googleCalSrc), v.Test)
	if ps, err := a.store.ListPeople(ctx, "", 500, 0); err == nil {
		for _, p := range ps {
			v.People = append(v.People, icloudPersonOpt{ID: p.ID, Label: p.Shown()})
		}
	}
	for i, c := range cals {
		v.Calendars = append(v.Calendars, googleCalView{Key: c.Key, Name: c.Name, Tail: c.Tail, Show: c.Show, Person: c.Person, Field: strconv.Itoa(i)})
	}
	return v
}

// cleanCalName reads the name the owner gives a calendar: one line, not empty.
func cleanCalName(s string) (string, bool) {
	s = strings.Join(strings.Fields(strings.ToValidUTF8(s, "")), " ")
	if s == "" || utf8.RuneCountInString(s) > maxGoogleName {
		return "", false
	}
	return s, true
}

// googleConnect adds one calendar: it checks the address and the name, reads
// the feed once and keeps the address only when Google gives a calendar.
// Nothing typed is shown back on an error, and no error holds the address.
func (a *app) googleConnect(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 8<<10)
	addr, tail, ok := parseGoogleAddress(r.FormValue("address"))
	if !ok {
		a.renderConnectors(w, r, http.StatusBadRequest, "google", "", msgGoogleAddress)
		return
	}
	name, ok := cleanCalName(r.FormValue("name"))
	if !ok {
		a.renderConnectors(w, r, http.StatusBadRequest, "google", "", fmt.Sprintf("Give the calendar a name of up to %d characters.", maxGoogleName))
		return
	}
	a.opMu.Lock()
	defer a.opMu.Unlock()
	ctx := r.Context()
	cals := a.googleCals(ctx)
	sec := a.googleSecret()
	if len(cals) >= maxGoogleCals {
		a.renderConnectors(w, r, http.StatusBadRequest, "google", "", fmt.Sprintf("ONE reads at most %d Google calendars. Disconnect one first.", maxGoogleCals))
		return
	}
	for _, c := range cals {
		if strings.EqualFold(c.Name, name) {
			a.renderConnectors(w, r, http.StatusBadRequest, "google", "", "There is already a calendar with that name. Give this one another name.")
			return
		}
		if sec.Addresses[c.Key] == addr {
			a.renderConnectors(w, r, http.StatusBadRequest, "google", "", "That address is already connected.")
			return
		}
	}
	cctx, cancel := context.WithTimeout(ctx, googleFeedTime+10*time.Second)
	defer cancel()
	if _, err := a.fetchGoogle(cctx, addr, "", ""); err != nil {
		a.renderConnectors(w, r, http.StatusBadRequest, "google", "", plainGoogle(&googleCalErr{Name: name, Err: err}))
		return
	}
	key := newCalKey()
	a.googleCfgMu.Lock()
	sec = a.googleSecret()
	sec.Addresses[key] = addr
	if err := writeSecret(a.secretsDir, googleSecretFile, sec); err != nil {
		a.googleCfgMu.Unlock()
		appkit.Error("saving the Google calendar address", err)
		a.renderConnectors(w, r, http.StatusInternalServerError, "google", "", "The address could not be saved. Try again.")
		return
	}
	err := a.saveGoogleCalsLocked(ctx, "owner", func(list []googleCal) []googleCal {
		return append(list, googleCal{Key: key, Name: name, Tail: tail, Show: true})
	})
	a.googleCfgMu.Unlock()
	if err != nil {
		appkit.Error("saving the Google calendars", err)
		a.renderConnectors(w, r, http.StatusInternalServerError, "google", "", "The calendar could not be saved. Try again.")
		return
	}
	a.store.ForgetTest(ctx, googleConn)
	a.syncGoogleCal(ctx, true) // the events come in at once; a failure is kept on the run
	http.Redirect(w, r, "/connectors?n=google#google", http.StatusSeeOther)
}

// googleDisconnect removes one calendar: its address, its choices and the
// events it brought.
func (a *app) googleDisconnect(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 8<<10)
	key := r.FormValue("key")
	a.googleMu.Lock() // not while a read of it is running
	defer a.googleMu.Unlock()
	ctx := r.Context()
	found := false
	for _, c := range a.googleCals(ctx) {
		if c.Key == key {
			found = true
		}
	}
	if !found {
		http.Redirect(w, r, "/connectors#google", http.StatusSeeOther)
		return
	}
	a.googleCfgMu.Lock()
	defer a.googleCfgMu.Unlock()
	sec := a.googleSecret()
	delete(sec.Addresses, key)
	var err error
	if len(sec.Addresses) == 0 {
		err = removeSecret(a.secretsDir, googleSecretFile)
	} else {
		err = writeSecret(a.secretsDir, googleSecretFile, sec)
	}
	if err != nil {
		appkit.Error("removing the Google calendar address", err)
		http.Error(w, "could not remove the address", http.StatusInternalServerError)
		return
	}
	err = a.store.Update(ctx, func(tx *repo.Tx) error {
		if err := tx.DeleteEventsOfCalendar(ctx, key); err != nil {
			return err
		}
		return tx.InsertEvent(ctx, a.now().Unix(), 0, "owner", "setting_changed", map[string]any{"connector": "Google Calendar", "to": "a calendar disconnected"})
	})
	if err == nil {
		err = a.saveGoogleCalsLocked(ctx, "owner", func(list []googleCal) []googleCal {
			out := list[:0:0]
			for _, c := range list {
				if c.Key != key {
					out = append(out, c)
				}
			}
			return out
		})
	}
	if err != nil {
		appkit.Error("removing the Google calendar", err)
		http.Error(w, "could not remove the calendar", http.StatusInternalServerError)
		return
	}
	a.store.ForgetTest(ctx, googleConn)
	http.Redirect(w, r, "/connectors?n=googleoff#google", http.StatusSeeOther)
}

// googleSave keeps the owner's choices per calendar, then reads again so the
// agenda follows them.
func (a *app) googleSave(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	ctx := r.Context()
	known := map[int64]bool{}
	if ps, err := a.store.ListPeople(ctx, "", 1000, 0); err == nil {
		for _, p := range ps {
			known[p.ID] = true
		}
	}
	err := a.updateGoogleCals(ctx, "owner", func(cals []googleCal) []googleCal {
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
		return cals
	})
	if err != nil {
		appkit.Error("saving the Google calendars", err)
		http.Error(w, "could not save", http.StatusInternalServerError)
		return
	}
	a.syncGoogleCal(ctx, false)
	http.Redirect(w, r, "/connectors?n=googlesave#google", http.StatusSeeOther)
}

// googleTest is Test now: it reads now and says what came of it.
func (a *app) googleTest(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	cals := a.googleCals(ctx)
	if len(cals) == 0 {
		a.renderConnectors(w, r, http.StatusBadRequest, "google", "", "Connect a Google calendar first.")
		return
	}
	a.opMu.Lock()
	defer a.opMu.Unlock()
	if err := a.syncGoogleCal(ctx, true); err != nil {
		a.record(ctx, googleConn, false, "Could not read: "+plainGoogle(err))
	} else {
		shown := 0
		for _, c := range cals {
			if c.Show {
				shown++
			}
		}
		a.record(ctx, googleConn, true, fmt.Sprintf("Read %s shown in the agenda. %d entries in the agenda now.", plural(int64(shown), "calendar"), a.store.CountEvents(ctx, srcGoogleCal)))
	}
	http.Redirect(w, r, "/connectors#google", http.StatusSeeOther)
}
