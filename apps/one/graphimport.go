package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"lab/apps/one/repo"
)

// The Office mailbox, read through Microsoft Graph with the sign-in the
// Connectors page keeps. It goes through the same path as Gmail (ingest.go).
// It only reads: no call here changes a message, moves it or deletes it.
//
// A folder is followed with delta queries. A first import of a folder is a
// delta round limited to the messages of the wanted period, newest first;
// when the round ends, Graph's delta link is the folder's position, and the
// timer's sync continues from it. DESIGN.md 3.5, 5.4, 11.4.

const (
	srcGraph       = "outlook" // the name the rules, the data and the page use for it
	graphStateKey  = "sync.outlook"
	keyGraphLogin  = "outlook_needs_login" // "1" while the sign-in has run out
	graphPageSize  = 20                    // messages per page; the app has 256 MB
	graphStreamMax = 30 * time.Minute      // one original message
	graphRetries   = 4
	graphSelect    = "id,conversationId,parentFolderId,receivedDateTime,sentDateTime,isRead,isDraft,flag,categories,from,sender,subject,internetMessageId,hasAttachments,bodyPreview"
)

// graphImmutable asks Graph for ids that never change, even when a message moves.
var graphImmutable = []string{`IdType="ImmutableId"`}

// Folders that are not imported (the owner, SPEC: all but Drafts, Deleted Items and Junk).
var graphSkippedFolders = []string{"drafts", "deleteditems", "junkemail"}

// graphFolder is where one folder stands.
type graphFolder struct {
	Delta string `json:"delta,omitempty"` // the position: where the next sync starts
	Next  string `json:"next,omitempty"`  // a round that was interrupted continues here
	Since int64  `json:"since,omitempty"` // how far back the position reaches
}

type graphCursor struct {
	Next string `json:"next,omitempty"`
	Done bool   `json:"done,omitempty"`
}

// graphBackfill is one import going back in time, folder by folder.
type graphBackfill struct {
	Mode    string                  `json:"mode"` // first, week or month
	Since   int64                   `json:"since"`
	Done    bool                    `json:"done"`
	Folders map[string]*graphCursor `json:"folders,omitempty"`
}

// graphState survives a stop or a crash.
type graphState struct {
	Folders  map[string]*graphFolder `json:"folders,omitempty"`
	Backfill *graphBackfill          `json:"backfill,omitempty"` // the first import
	Manual   *graphBackfill          `json:"manual,omitempty"`   // the last "Import one week" or "Import one month"
}

func (st *graphState) target(mode string) **graphBackfill {
	if mode == "first" {
		return &st.Backfill
	}
	return &st.Manual
}

func (st *graphState) folder(id string) *graphFolder {
	if st.Folders == nil {
		st.Folders = map[string]*graphFolder{}
	}
	f := st.Folders[id]
	if f == nil {
		f = &graphFolder{}
		st.Folders[id] = f
	}
	return f
}

// reach is how far back the folders already followed go: a folder made
// later reaches back as far.
func (st *graphState) reach() (since int64, any bool) {
	for _, f := range st.Folders {
		if f.Delta != "" && (!any || f.Since < since) {
			since, any = f.Since, true
		}
	}
	return
}

type graphImporter struct {
	a        *app
	core     *mailCore
	mu       sync.Mutex // one run at a time
	busy     atomic.Bool
	kick     chan importRequest
	pageSize int
	lastSync time.Time
	log      *slog.Logger
	// holdFirstImport works as for Gmail: the long first import starts only
	// after "Import one month" has finished once.
	holdFirstImport bool
	// Where each well-known folder is, for the history: folder id to place.
	placeMu sync.Mutex
	places  map[string]string
	placesN int // how many well-known folders were looked up already
	// historyPage is how many messages of the history are read at a time (0: the default).
	historyPage int
}

func newGraphImporter(a *app, core *mailCore) *graphImporter {
	return &graphImporter{a: a, core: core, kick: make(chan importRequest, 1), pageSize: graphPageSize, log: core.log, holdFirstImport: true}
}

func (gi *graphImporter) Busy() bool { return gi.busy.Load() }

// Request asks the loop for an import of the given depth now; false when one is running.
func (gi *graphImporter) Request(mode string, depth time.Duration) bool {
	if gi.busy.Load() {
		return false
	}
	select {
	case gi.kick <- importRequest{Mode: mode, Depth: depth}:
		return true
	default:
		return false
	}
}

func (gi *graphImporter) logErr(err error) {
	if err == nil || errors.Is(err, errNotConnected) || errors.Is(err, errNeedsLogin) || errors.Is(err, context.Canceled) {
		return
	}
	gi.log.Warn("outlook run failed") // the text is on the run and the page, never in the log
}

func (gi *graphImporter) loadState(ctx context.Context) graphState {
	var st graphState
	json.Unmarshal([]byte(gi.a.store.Setting(ctx, graphStateKey)), &st)
	return st
}

func (gi *graphImporter) saveState(ctx context.Context, st graphState) error {
	b, _ := json.Marshal(st)
	return gi.a.store.SetSettingBy(context.WithoutCancel(ctx), graphStateKey, string(b), "system", gi.a.now())
}

// --- the sign-in a run needs ---

func (a *app) graphSigned() bool {
	var sec graphSecret
	return readSecret(a.secretsDir, graphSecretFile, &sec) && sec.RefreshToken != ""
}

func (a *app) graphNeedsLogin(ctx context.Context) bool {
	return a.store.Setting(ctx, keyGraphLogin) == "1"
}

func (a *app) setGraphNeedsLogin(ctx context.Context, on bool) {
	v := ""
	if on {
		v = "1"
	}
	if a.store.Setting(ctx, keyGraphLogin) != v {
		a.store.SetSettingBy(context.WithoutCancel(ctx), keyGraphLogin, v, "system", a.now())
	}
}

const needsLoginText = "The Microsoft sign-in has run out. Press Sign in on this page; the import continues where it stopped."

// plainGraph tells a failed run in words a person can act on.
func plainGraph(err error) string {
	switch {
	case errors.Is(err, errNeedsLogin):
		return needsLoginText
	case errors.Is(err, errNotConnected):
		return "Office mail is not signed in."
	}
	var me *msError
	if errors.As(err, &me) && me.Status == 429 {
		return "Microsoft asked the app to slow down and did not let up; the import continues where it stopped."
	}
	return plainMS("Office mail", err)
}

// do runs one call to Graph with a valid access token. A token Graph
// refuses is renewed once; a sign-in that cannot be renewed is
// errNeedsLogin. A call Graph asks to be repeated later is repeated after
// the time it names. Calls made through do change nothing in the mailbox.
func (gi *graphImporter) do(ctx context.Context, fn func(token string) error) error {
	force := false
	for attempt := 0; ; attempt++ {
		token, _, err := gi.a.graphToken(ctx, force)
		if err != nil {
			return err
		}
		err = fn(token)
		var me *msError
		if err == nil || !errors.As(err, &me) {
			return err
		}
		switch {
		case me.Status == 401 && !force:
			force = true
			continue
		case me.Status == 401:
			return errNeedsLogin
		case (me.Status == 429 || me.Status == 503 || me.Status == 504) && attempt < graphRetries:
			wait := me.RetryAfter
			if gi.a.graph.RetryWait > 0 {
				wait = gi.a.graph.RetryWait
			}
			if wait <= 0 {
				wait = 5 * time.Second
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(min(wait, time.Minute)):
			}
			continue
		}
		return err
	}
}

func (gi *graphImporter) get(ctx context.Context, path string, out any) error {
	return gi.do(ctx, func(token string) error {
		return gi.a.graph.apiPrefer(ctx, token, "GET", path, append(graphImmutable, fmt.Sprintf("odata.maxpagesize=%d", gi.pageSize)), nil, out)
	})
}

// --- folders ---

type graphFolderInfo struct {
	ID      string `json:"id"`
	Name    string `json:"displayName"`
	Subdirs int    `json:"childFolderCount"`
}

type graphFolderPage struct {
	Value []graphFolderInfo `json:"value"`
	Next  string            `json:"@odata.nextLink"`
}

// folders lists the folders to read: every folder with its subfolders,
// except Drafts, Deleted Items and Junk and whatever is inside them.
func (gi *graphImporter) folders(ctx context.Context) ([]graphFolderInfo, error) {
	skip := map[string]bool{}
	for _, wk := range graphSkippedFolders {
		var f graphFolderInfo
		err := gi.get(ctx, "/me/mailFolders/"+wk+"?%24select=id", &f)
		var me *msError
		if errors.As(err, &me) && me.Status == 404 {
			continue // this mailbox has no such folder
		}
		if err != nil {
			return nil, err
		}
		skip[f.ID] = true
		gi.rememberPlace(f.ID, wkPlace(wk))
	}
	var out []graphFolderInfo
	var walk func(path string, depth int) error
	walk = func(path string, depth int) error {
		for path != "" {
			var page graphFolderPage
			if err := gi.get(ctx, path, &page); err != nil {
				return err
			}
			for _, f := range page.Value {
				if skip[f.ID] {
					continue
				}
				out = append(out, f)
				if f.Subdirs > 0 && depth < 8 {
					if err := walk("/me/mailFolders/"+url.PathEscape(f.ID)+"/childFolders?%24select=id,displayName,childFolderCount&%24top=100", depth+1); err != nil {
						return err
					}
				}
			}
			path = ""
			if page.Next != "" {
				p, err := gi.a.graph.pathOfLink(page.Next)
				if err != nil {
					return err
				}
				path = p
			}
		}
		return nil
	}
	if err := walk("/me/mailFolders?%24select=id,displayName,childFolderCount&%24top=100", 0); err != nil {
		return nil, err
	}
	return out, nil
}

// --- messages ---

type graphAddr struct {
	E struct {
		Name    string `json:"name"`
		Address string `json:"address"`
	} `json:"emailAddress"`
}

// graphMessage is what a delta page says about one message.
type graphMessage struct {
	ID      string `json:"id"`
	Removed *struct {
		Reason string `json:"reason"`
	} `json:"@removed"`
	ConversationID   string    `json:"conversationId"`
	ReceivedDateTime string    `json:"receivedDateTime"`
	SentDateTime     string    `json:"sentDateTime"`
	IsRead           bool      `json:"isRead"`
	IsDraft          bool      `json:"isDraft"`
	Categories       []string  `json:"categories"`
	From             graphAddr `json:"from"`
	Sender           graphAddr `json:"sender"`
	Subject          string    `json:"subject"`
	InternetID       string    `json:"internetMessageId"`
	BodyPreview      string    `json:"bodyPreview"`
	Flag             struct {
		Status string `json:"flagStatus"`
	} `json:"flag"`
}

type graphPage struct {
	Value []json.RawMessage `json:"value"`
	Next  string            `json:"@odata.nextLink"`
	Delta string            `json:"@odata.deltaLink"`
}

func graphTime(s string) time.Time {
	t, ok := parseGraphTime(s)
	if !ok {
		return time.Time{}
	}
	return t
}

// firstPath starts a delta round over one folder: the messages received
// since the given time, newest first.
func firstPath(folderID string, since int64) string {
	q := url.Values{}
	q.Set("$select", graphSelect)
	q.Set("$filter", "receivedDateTime ge "+time.Unix(since, 0).UTC().Format("2006-01-02T15:04:05Z"))
	q.Set("$orderby", "receivedDateTime desc")
	return "/me/mailFolders/" + url.PathEscape(folderID) + "/messages/delta?" + strings.ReplaceAll(q.Encode(), "+", "%20")
}

// graphScope is a run of the Office mailbox.
type graphScope struct {
	*mailScope
	gi      *graphImporter
	headers bool            // an enabled rule looks at a header, so headers are read when a rule is checked
	read    map[string]bool // the folders being read
}

func (gi *graphImporter) scope(ctx context.Context, run *repo.Run, self string, folders []graphFolderInfo) (*graphScope, error) {
	ms, err := gi.core.scope(ctx, graphSrc, run, self)
	if err != nil {
		return nil, err
	}
	gi.loadPlaces(ctx)
	gs := &graphScope{mailScope: ms, gi: gi, read: map[string]bool{}}
	for _, f := range folders {
		gs.read[f.ID] = true
	}
	for _, r := range ms.rules {
		if strings.HasPrefix(strings.ToLower(r.Field), "header:") {
			gs.headers = true
		}
	}
	return gs, nil
}

func (gs *graphScope) record(ctx context.Context, m graphMessage, f graphFolderInfo, raw json.RawMessage) mailRecord {
	var flags []string
	if m.IsRead {
		flags = append(flags, `\Seen`)
	}
	if strings.EqualFold(m.Flag.Status, "flagged") {
		flags = append(flags, `\Flagged`)
	}
	labels := append([]string{f.Name}, m.Categories...)
	received := graphTime(m.ReceivedDateTime)
	if received.IsZero() {
		received = graphTime(m.SentDateTime)
	}
	payload, _ := json.Marshal(map[string]any{"message": raw, "folder_id": f.ID, "folder": f.Name})
	rec := mailRecord{
		SourceID: m.ID, ThreadID: m.ConversationID, Received: received, Labels: labels, Flags: flags,
		Payload:    string(payload),
		StateExtra: []string{"folder-id:" + f.ID},
		Place:      gs.gi.placeOf(f.ID),
		Facts:      ruleFacts{Sender: strings.ToLower(m.From.E.Address), Subject: m.Subject, Labels: labels},
	}
	if m.BodyPreview != "" {
		preview := m.BodyPreview
		rec.Peek = func() string { return preview }
	}
	if gs.headers {
		rec.LateHeaders = func() (map[string]string, error) { return gs.readHeaders(ctx, m.ID) }
	}
	return rec
}

// readHeaders reads the headers of one message, for the rules that look at them.
func (gs *graphScope) readHeaders(ctx context.Context, id string) (map[string]string, error) {
	var out struct {
		Headers []struct {
			Name  string `json:"name"`
			Value string `json:"value"`
		} `json:"internetMessageHeaders"`
	}
	if err := gs.gi.get(ctx, "/me/messages/"+url.PathEscape(id)+"?%24select=internetMessageHeaders", &out); err != nil {
		return nil, err
	}
	h := map[string]string{}
	for _, x := range out.Headers {
		k := strings.ToLower(x.Name)
		if _, ok := h[k]; !ok {
			h[k] = x.Value
		}
	}
	return h, nil
}

// leftForGood asks Graph where a message that left a folder is now.
func (gs *graphScope) leftForGood(ctx context.Context, id string) (bool, error) {
	var now struct {
		Parent string `json:"parentFolderId"`
	}
	err := gs.gi.get(ctx, "/me/messages/"+url.PathEscape(id)+"?%24select=parentFolderId", &now)
	var me *msError
	if errors.As(err, &me) && me.Status == 404 {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	return !gs.read[now.Parent], nil
}

// entry is the whole pipeline for one line of a delta page.
func (gs *graphScope) entry(ctx context.Context, f graphFolderInfo, raw json.RawMessage) error {
	var m graphMessage
	if err := json.Unmarshal(raw, &m); err != nil || m.ID == "" {
		return nil // a line that is not a message
	}
	gs.run.Fetched++
	return gs.tolerate(ctx, m.ID, gs.unit(func() error { return gs.entryOf(ctx, f, m, raw) }))
}

// entryOf is the work on one message; its failures are told apart by entry.
func (gs *graphScope) entryOf(ctx context.Context, f graphFolderInfo, m graphMessage, raw json.RawMessage) error {
	if m.Removed != nil {
		payload, _ := json.Marshal(map[string]any{"removed": true, "reason": m.Removed.Reason, "folder_id": f.ID, "folder": f.Name})
		return conn(gs.markRemoved(ctx, m.ID, string(payload), func(prev repo.Raw) (bool, error) {
			// A message that left a folder may only have been moved to another
			// folder that is read; there it shows up as a change. It is gone
			// when Graph no longer has it or it now sits in a folder that is
			// not read (Deleted Items, Junk, Drafts).
			var p struct {
				Folder string `json:"folder_id"`
			}
			json.Unmarshal([]byte(prev.Payload), &p)
			if p.Folder != "" && p.Folder != f.ID {
				return false, nil // the message was already seen elsewhere
			}
			return gs.leftForGood(ctx, m.ID)
		}))
	}
	if m.IsDraft {
		return nil
	}
	rec := gs.record(ctx, m, f, raw)
	return gs.handle(ctx, rec,
		func(w *poolWriter) error {
			cctx, cancel := context.WithTimeout(ctx, graphStreamMax)
			defer cancel()
			return gs.gi.do(cctx, func(token string) error {
				_, err := gs.gi.a.graph.apiStream(cctx, token, "/me/messages/"+url.PathEscape(m.ID)+"/$value", graphImmutable, w)
				return err
			})
		},
		func() parsedMail {
			pm := parsedMail{Subject: m.Subject, MessageID: m.InternetID, Date: receivedOf(m)}
			addr, ok := cleanAddress(m.From.E.Address)
			if ok {
				pm.FromAddr = strings.ToLower(addr)
			}
			pm.FromName = m.From.E.Name
			return pm
		})
}

func receivedOf(m graphMessage) time.Time {
	if t := graphTime(m.ReceivedDateTime); !t.IsZero() {
		return t
	}
	return graphTime(m.SentDateTime)
}

// isResync says Graph no longer knows a saved position.
func isResync(err error) bool {
	var me *msError
	if !errors.As(err, &me) {
		return false
	}
	switch strings.ToLower(me.Code) {
	case "syncstatenotfound", "resyncrequired", "syncstateinvalid":
		return true
	}
	return me.Status == 410
}

// walk reads a folder from the first of the given starting points Graph
// still accepts, page by page, and returns the delta link that ends the
// round. progress is told, after each page is stored, where to continue.
func (gs *graphScope) walk(ctx context.Context, f graphFolderInfo, starts []string, progress func(next string) error, between func() error) (string, error) {
	var lastErr error
	for _, start := range starts {
		path := start
		if strings.HasPrefix(path, "http") {
			p, err := gs.gi.a.graph.pathOfLink(path)
			if err != nil {
				lastErr = err
				continue
			}
			path = p
		}
		delta, err := gs.walkFrom(ctx, f, path, progress, between)
		if err == nil {
			return delta, nil
		}
		if !isResync(err) {
			return "", err
		}
		lastErr = err
	}
	if lastErr == nil {
		lastErr = errors.New("there is no place to read from")
	}
	return "", lastErr
}

func (gs *graphScope) walkFrom(ctx context.Context, f graphFolderInfo, path string, progress func(next string) error, between func() error) (string, error) {
	for {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		var page graphPage
		if err := gs.gi.get(ctx, path, &page); err != nil {
			return "", err
		}
		for _, raw := range page.Value {
			if err := ctx.Err(); err != nil {
				return "", err
			}
			if err := gs.entry(ctx, f, raw); err != nil {
				return "", err
			}
			gs.gi.core.saveRun(ctx, gs.run)
		}
		switch {
		case page.Next != "":
			p, err := gs.gi.a.graph.pathOfLink(page.Next)
			if err != nil {
				return "", err
			}
			if err := progress(page.Next); err != nil {
				return "", err
			}
			if between != nil {
				if err := between(); err != nil {
					return "", err
				}
			}
			path = p
		case page.Delta != "":
			return page.Delta, nil
		default:
			return "", errors.New("Microsoft Graph ended the list without a position to continue from")
		}
	}
}

// --- runs ---

func (gi *graphImporter) startRun(ctx context.Context, mode string) (*repo.Run, error) {
	return gi.core.startRun(ctx, graphSrc, mode)
}

// finish records the end of a run and keeps track of whether the sign-in has run out.
func (gi *graphImporter) finish(ctx context.Context, run *repo.Run, err error) {
	gi.core.endRun(ctx, graphSrc, run, err, plainGraph)
	switch {
	case err == nil:
		gi.a.setGraphNeedsLogin(ctx, false)
	case errors.Is(err, errNeedsLogin):
		gi.a.setGraphNeedsLogin(ctx, true)
	}
	gi.a.checkSource(ctx, graphSrc) // SPEC 24: one notice per failure
}

// Tick is one timer round, as for Gmail: a continued import when one was
// left unfinished, the first import when none was ever started (once it is
// allowed), and otherwise a plain sync. A sign-in that has run out pauses it.
func (gi *graphImporter) Tick(ctx context.Context) error {
	if !gi.a.graphSigned() {
		return errNotConnected
	}
	if gi.a.graphNeedsLogin(ctx) {
		return errNeedsLogin
	}
	st := gi.loadState(ctx)
	switch {
	case st.Backfill != nil && !st.Backfill.Done:
		return gi.RunImport(ctx, "first", 0)
	case st.Manual != nil && !st.Manual.Done:
		return gi.RunImport(ctx, st.Manual.Mode, 0)
	case st.Backfill == nil && gi.firstImportAllowed(st):
		return gi.RunImport(ctx, "first", gi.a.syncSettings(ctx).FirstDepth)
	}
	hs := gi.loadHistory(ctx)
	if hs.Started == 0 && st.Backfill != nil && st.Backfill.Done {
		if err := gi.a.startHistory(ctx, historyKeyO); err == nil {
			hs = gi.loadHistory(ctx)
		}
	}
	if hs.Started > 0 && !hs.Done {
		return gi.RunHistory(ctx)
	}
	return gi.RunSync(ctx)
}

func (gi *graphImporter) firstImportAllowed(st graphState) bool {
	return !gi.holdFirstImport || (st.Manual != nil && st.Manual.Mode == "month" && st.Manual.Done)
}

// RunSync is the regular pass: the changes of every folder since its position.
func (gi *graphImporter) RunSync(ctx context.Context) error {
	gi.mu.Lock()
	defer gi.mu.Unlock()
	gi.busy.Store(true)
	defer gi.busy.Store(false)
	if !gi.a.graphSigned() {
		return errNotConnected
	}
	st := gi.loadState(ctx)
	return gi.syncOn(ctx, &st)
}

// syncOn runs the regular pass as its own run.
func (gi *graphImporter) syncOn(ctx context.Context, st *graphState) (err error) {
	run, err := gi.startRun(ctx, "sync")
	if err != nil {
		return err
	}
	defer func() { gi.finish(ctx, run, err) }()
	_, sec, err := gi.a.graphAccess(ctx)
	if err != nil {
		return err
	}
	folders, err := gi.folders(ctx)
	if err != nil {
		return err
	}
	gs, err := gi.scope(ctx, run, sec.Address, folders)
	if err != nil {
		return err
	}
	if err = gs.finishPending(ctx); err != nil {
		return err
	}
	if err = gs.retryFailed(ctx); err != nil {
		return err
	}
	for _, f := range folders {
		if err = gs.syncFolder(ctx, st, f); err != nil {
			return err
		}
	}
	gi.lastSync = gi.a.now()
	return nil
}

func (gs *graphScope) syncFolder(ctx context.Context, st *graphState, f graphFolderInfo) error {
	fs, known := st.Folders[f.ID]
	if !known || (fs.Delta == "" && fs.Next == "") {
		since, any := st.reach()
		if !any {
			return nil // nothing was imported yet: the first import will bring it
		}
		// A folder made after the import reaches back as far as the others.
		fs = st.folder(f.ID)
		fs.Since = since
	}
	var starts []string
	for _, s := range []string{fs.Next, fs.Delta} {
		if s != "" {
			starts = append(starts, s)
		}
	}
	if fs.Since > 0 {
		starts = append(starts, firstPath(f.ID, fs.Since))
	}
	delta, err := gs.walk(ctx, f, starts, func(next string) error {
		fs.Next = next
		return gs.gi.saveState(ctx, *st)
	}, nil)
	if err != nil {
		return err
	}
	fs.Delta, fs.Next = delta, ""
	return gs.gi.saveState(ctx, *st)
}

// RunImport goes back in time from now, folder by folder, newest first, in
// pages, resumable. An import already started with this mode is continued;
// depth 0 means so.
func (gi *graphImporter) RunImport(ctx context.Context, mode string, depth time.Duration) (err error) {
	gi.mu.Lock()
	defer gi.mu.Unlock()
	gi.busy.Store(true)
	defer gi.busy.Store(false)
	if !gi.a.graphSigned() {
		return errNotConnected
	}
	run, err := gi.startRun(ctx, mode)
	if err != nil {
		return err
	}
	defer func() { gi.finish(ctx, run, err) }()
	_, sec, err := gi.a.graphAccess(ctx)
	if err != nil {
		return err
	}
	st := gi.loadState(ctx)
	slot := st.target(mode)
	if *slot == nil || (*slot).Done || (*slot).Mode != mode {
		if depth <= 0 {
			depth = gi.a.syncSettings(ctx).FirstDepth
		}
		*slot = &graphBackfill{Mode: mode, Since: gi.a.now().Add(-depth).Unix(), Folders: map[string]*graphCursor{}}
	}
	if err = gi.saveState(ctx, st); err != nil {
		return err
	}
	folders, err := gi.folders(ctx)
	if err != nil {
		return err
	}
	gs, err := gi.scope(ctx, run, sec.Address, folders)
	if err != nil {
		return err
	}
	if err = gs.finishPending(ctx); err != nil {
		return err
	}
	if err = gs.retryFailed(ctx); err != nil {
		return err
	}
	return gs.backfill(ctx, &st, *slot, folders)
}

func (gs *graphScope) backfill(ctx context.Context, st *graphState, bf *graphBackfill, folders []graphFolderInfo) error {
	gi := gs.gi
	if bf.Folders == nil {
		bf.Folders = map[string]*graphCursor{}
	}
	for i, f := range folders {
		cur := bf.Folders[f.ID]
		if cur == nil {
			cur = &graphCursor{}
			bf.Folders[f.ID] = cur
		}
		if cur.Done {
			continue
		}
		var starts []string
		if cur.Next != "" {
			starts = append(starts, cur.Next)
		}
		starts = append(starts, firstPath(f.ID, bf.Since))
		gs.run.Cursor = fmt.Sprintf(`{"mode":%q,"folder":%d,"of":%d}`, bf.Mode, i+1, len(folders))
		delta, err := gs.walk(ctx, f, starts, func(next string) error {
			cur.Next = next
			return gi.saveState(ctx, *st)
		}, func() error { return gs.yieldToTimer(ctx, st) })
		if err != nil {
			return err
		}
		cur.Done, cur.Next = true, ""
		// The round's delta link is the folder's position when it reaches back
		// at least as far as the one the folder has.
		fs := st.folder(f.ID)
		if fs.Delta == "" || bf.Since <= fs.Since {
			fs.Delta, fs.Next, fs.Since = delta, "", bf.Since
		}
		if err := gi.saveState(ctx, *st); err != nil {
			return err
		}
	}
	bf.Done = true
	return gi.saveState(ctx, *st)
}

// yieldToTimer runs the regular sync inside a long import when its time has
// come, so the import never starves it.
func (gs *graphScope) yieldToTimer(ctx context.Context, st *graphState) error {
	gi := gs.gi
	if gi.lastSync.IsZero() {
		if r, ok := gi.a.store.LastFinishedRun(ctx, srcGraph, "sync"); ok {
			gi.lastSync = time.Unix(r.FinishedAt, 0)
		} else {
			gi.lastSync = gi.a.now()
		}
	}
	if gi.a.now().Sub(gi.lastSync) < gi.a.syncSettings(ctx).Interval {
		return nil
	}
	return gi.syncOn(ctx, st)
}
