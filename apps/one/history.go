package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/mail"
	"net/url"
	"slices"
	"strings"
	"time"

	"lab/apps/one/repo"
)

// The history of what the owner did with his mail, for the whole of each mailbox
// (SPEC 21). Beside the imported year ONE keeps one light line per message:
// who wrote (address and name), when, the thread, whether the owner read it,
// starred it, answered it (a message of his own follows in the thread) and
// where it is kept. For messages before the imported period that is all: no
// subject, no text, no file, never a body. It only reads: Gmail is read with
// EXAMINE and BODY.PEEK of two header fields, Office with a message list
// that selects only the fields below. It is started by the button Read
// history on the Connectors page, goes on in the background when the timer
// comes round, gives way to the regular sync, remembers its place after every
// page and writes nothing twice.

const (
	historyMode    = "history"
	historyKeyG    = "history.gmail"
	historyKeyO    = "history.outlook"
	historyUIDSpan = 200 // Gmail UIDs read at a time
	historyGraphN  = 100 // Graph messages read at a time
	// What the Graph list is asked for: ids, thread, time, flags and sender. No subject, no body.
	historySelect = "id,conversationId,receivedDateTime,sentDateTime,isRead,isDraft,flag,from"
)

// The places a message can be kept.
const (
	placeInbox    = "inbox"
	placeArchived = "archived"
	placeSent     = "sent"
	placeDeleted  = "deleted"
	placeSpam     = "spam"
	placeFolder   = "folder"
)

// historyState is where a history pass stands. It survives a stop or a crash.
type historyState struct {
	Started  int64                    `json:"started"` // when the owner pressed the button; 0 never
	Done     bool                     `json:"done"`
	Validity uint32                   `json:"validity,omitempty"` // Gmail: the numbering the position belongs to
	Next     uint32                   `json:"next,omitempty"`     // Gmail: the highest UID not read yet
	Begun    bool                     `json:"begun,omitempty"`
	Folders  map[string]*historyCurse `json:"folders,omitempty"` // Office: one position per folder
}

type historyCurse struct {
	Next string `json:"next,omitempty"`
	Done bool   `json:"done,omitempty"`
}

// gmailPlace says where Gmail keeps a message, from its labels.
func gmailPlace(labels []string) string {
	has := func(l string) bool {
		return slices.ContainsFunc(labels, func(x string) bool { return strings.EqualFold(x, l) })
	}
	switch {
	case has(`\Trash`):
		return placeDeleted
	case has(`\Spam`) || has(`\Junk`):
		return placeSpam
	case has(`\Inbox`):
		return placeInbox
	case has(`\Sent`):
		return placeSent
	}
	return placeArchived
}

// wkPlace is the place of a well-known Graph folder.
func wkPlace(wk string) string {
	switch wk {
	case "inbox":
		return placeInbox
	case "sentitems":
		return placeSent
	case "deleteditems":
		return placeDeleted
	case "junkemail":
		return placeSpam
	case "archive":
		return placeArchived
	case "drafts":
		return "drafts"
	}
	return placeFolder
}

func (a *app) loadHistoryState(ctx context.Context, key string) historyState {
	var hs historyState
	json.Unmarshal([]byte(a.store.Setting(ctx, key)), &hs)
	return hs
}

func (a *app) saveHistoryState(ctx context.Context, key string, hs historyState) error {
	b, _ := json.Marshal(hs)
	return a.store.SetSettingBy(context.WithoutCancel(ctx), key, string(b), "system", a.now())
}

// startHistory is the button: the pass is marked as started (a finished one
// starts again from the newest message) and the loop is asked to run it.
func (a *app) startHistory(ctx context.Context, key string) error {
	hs := a.loadHistoryState(ctx, key)
	if hs.Started > 0 && !hs.Done {
		return nil // one is going on or waits for the timer: it continues where it is
	}
	return a.saveHistoryState(ctx, key, historyState{Started: a.now().Unix()})
}

// --- Gmail ---

func (im *importer) loadHistory(ctx context.Context) historyState {
	return im.a.loadHistoryState(ctx, historyKeyG)
}

// RequestHistory is the button for Gmail; false when an import is running.
func (im *importer) RequestHistory(ctx context.Context) bool {
	if im.busy.Load() {
		return false
	}
	if err := im.a.startHistory(ctx, historyKeyG); err != nil {
		return false
	}
	select {
	case im.kick <- importRequest{Mode: historyMode}:
		return true
	default:
		return false
	}
}

const historyItems = "UID X-GM-MSGID X-GM-THRID X-GM-LABELS FLAGS INTERNALDATE BODY.PEEK[HEADER.FIELDS (FROM DATE)]"

// fetchHistory reads the flags, labels, thread and two header fields of the
// messages with UIDs from lo to hi. Nothing in the folder is changed.
func (ic *imapConn) fetchHistory(lo, hi uint32) ([]msgMeta, error) {
	res, err := ic.do(fmt.Sprintf("UID FETCH %d:%d (%s)", lo, hi, historyItems))
	if err != nil {
		return nil, err
	}
	var out []msgMeta
	for _, u := range res.untagged {
		if m, ok := parseMeta(u); ok {
			out = append(out, m)
		}
	}
	return out, nil
}

// RunHistory reads the history of All Mail from the newest UID down.
func (im *importer) RunHistory(ctx context.Context) (err error) {
	im.mu.Lock()
	defer im.mu.Unlock()
	im.busy.Store(true)
	defer im.busy.Store(false)
	sec, ok := im.a.gmailSecret()
	if !ok {
		return errNotConnected
	}
	hs := im.loadHistory(ctx)
	if hs.Started == 0 || hs.Done {
		return nil
	}
	run, err := im.startRun(ctx, historyMode)
	if err != nil {
		return err
	}
	defer func() { im.endRun(ctx, run, err, sec.Password) }()
	sess, err := im.open(ctx, sec)
	if err != nil {
		return err
	}
	defer sess.close()
	if !hs.Begun || hs.Validity != sess.info.UIDValidity {
		// The first page, or Gmail renumbered the folder: start at the newest.
		// Nothing is stored twice: a message is known by Gmail's own id.
		hs.Begun, hs.Validity, hs.Next = true, sess.info.UIDValidity, sess.info.UIDNext-1
		if sess.info.UIDNext == 0 {
			hs.Next = 0
		}
	}
	sc, err := im.scope(ctx, sess, run, sec)
	if err != nil {
		return err
	}
	span := uint32(im.historySpan)
	if span == 0 {
		span = historyUIDSpan
	}
	for hs.Next >= 1 {
		if err = ctx.Err(); err != nil {
			return err
		}
		gst := im.loadState(ctx)
		if err = sc.yieldToTimer(ctx, &gst); err != nil {
			return err
		}
		hi := hs.Next
		lo := uint32(1)
		if hi > span {
			lo = hi - span + 1
		}
		var metas []msgMeta
		if metas, err = sess.ic.fetchHistory(lo, hi); err != nil {
			return err
		}
		if err = im.recordHistory(ctx, sec.Address, metas, run); err != nil {
			return err
		}
		hs.Next = lo - 1
		run.Cursor = fmt.Sprintf(`{"mode":"history","next":%d}`, hs.Next)
		if err = im.a.saveHistoryState(ctx, historyKeyG, hs); err != nil {
			return err
		}
		im.saveRun(ctx, run)
	}
	hs.Done = true
	return im.a.saveHistoryState(ctx, historyKeyG, hs)
}

// recordHistory writes a page of Gmail's messages into the history, in one transaction.
func (im *importer) recordHistory(ctx context.Context, self string, metas []msgMeta, run *repo.Run) error {
	now := im.a.now().Unix()
	self = strings.ToLower(self)
	return im.store().Update(ctx, func(tx *repo.Tx) error {
		for _, m := range metas {
			run.Fetched++
			if m.hasFlag(`\Draft`) || m.hasLabel(`\Draft`) {
				continue
			}
			name, addr := parseAddress(m.Header["from"])
			at := m.Date
			if at.IsZero() {
				at = headerDate(m.Header["date"])
			}
			place := gmailPlace(m.Labels)
			err := tx.Try(ctx, func() error {
				return im.a.tryUnit("history message", func() error {
					changed, err := tx.RecordHistory(ctx, repo.HistoryRow{Source: srcGmail, SourceID: m.MsgID, Thread: m.ThreadID, Sender: addr, SenderName: name,
						ReceivedAt: at.Unix(), Seen: m.hasFlag(`\Seen`), Starred: m.hasFlag(`\Flagged`), Mine: addr == self || place == placeSent, State: place}, now)
					if changed {
						run.Stored++
					}
					return err
				})
			})
			if err != nil {
				if isFatal(ctx, err) {
					return err
				}
				run.Failed++
				im.a.logLeftOut(srcGmail, m.MsgID, "history", 1, err) // only a number of the sender's history is missing
			}
		}
		return nil
	})
}

func headerDate(s string) time.Time {
	t, err := mail.ParseDate(strings.TrimSpace(unfold(s)))
	if err != nil {
		return time.Time{}
	}
	return t
}

// --- Office ---

func (gi *graphImporter) loadHistory(ctx context.Context) historyState {
	return gi.a.loadHistoryState(ctx, historyKeyO)
}

// RequestHistory is the button for the Office mailbox.
func (gi *graphImporter) RequestHistory(ctx context.Context) bool {
	if gi.busy.Load() {
		return false
	}
	if err := gi.a.startHistory(ctx, historyKeyO); err != nil {
		return false
	}
	select {
	case gi.kick <- importRequest{Mode: historyMode}:
		return true
	default:
		return false
	}
}

func (gi *graphImporter) rememberPlace(id, place string) {
	gi.placeMu.Lock()
	defer gi.placeMu.Unlock()
	if gi.places == nil {
		gi.places = map[string]string{}
	}
	gi.places[id] = place
}

// placeOf says what a folder is: the well-known ones by their id, any other
// folder is just a folder.
func (gi *graphImporter) placeOf(folderID string) string {
	gi.placeMu.Lock()
	defer gi.placeMu.Unlock()
	if p, ok := gi.places[folderID]; ok {
		return p
	}
	return placeFolder
}

// loadPlaces looks up, once, the folders Inbox, Sent Items and Archive that
// the list of folders to read does not ask for.
func (gi *graphImporter) loadPlaces(ctx context.Context) {
	gi.placeMu.Lock()
	done := gi.placesN > 0
	gi.placeMu.Unlock()
	if done {
		return
	}
	for _, wk := range []string{"inbox", "sentitems", "archive"} {
		var f graphFolderInfo
		if err := gi.get(ctx, "/me/mailFolders/"+wk+"?%24select=id", &f); err == nil && f.ID != "" {
			gi.rememberPlace(f.ID, wkPlace(wk))
		}
	}
	gi.placeMu.Lock()
	gi.placesN = 1
	gi.placeMu.Unlock()
}

type historyFolder struct {
	graphFolderInfo
	Place string
}

// historyFolders lists every folder but Drafts, with where it keeps mail: the
// subfolders of Deleted Items and Junk keep it the same way.
func (gi *graphImporter) historyFolders(ctx context.Context) ([]historyFolder, error) {
	drafts := ""
	var f graphFolderInfo
	if err := gi.get(ctx, "/me/mailFolders/drafts?%24select=id", &f); err == nil {
		drafts = f.ID
	}
	var out []historyFolder
	var walk func(path, inherit string, depth int) error
	walk = func(path, inherit string, depth int) error {
		for path != "" {
			var page graphFolderPage
			if err := gi.get(ctx, path, &page); err != nil {
				return err
			}
			for _, f := range page.Value {
				if f.ID == drafts && drafts != "" {
					continue
				}
				place := gi.placeOf(f.ID)
				if place == placeFolder && (inherit == placeDeleted || inherit == placeSpam) {
					place = inherit
				}
				out = append(out, historyFolder{f, place})
				if f.Subdirs > 0 && depth < 8 {
					if err := walk("/me/mailFolders/"+url.PathEscape(f.ID)+"/childFolders?%24select=id,displayName,childFolderCount&%24top=100", place, depth+1); err != nil {
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
	if err := walk("/me/mailFolders?%24select=id,displayName,childFolderCount&%24top=100", "", 0); err != nil {
		return nil, err
	}
	return out, nil
}

func historyPath(folderID string) string {
	q := url.Values{}
	q.Set("$select", historySelect)
	q.Set("$orderby", "receivedDateTime desc")
	return "/me/mailFolders/" + url.PathEscape(folderID) + "/messages?" + strings.ReplaceAll(q.Encode(), "+", "%20")
}

// RunHistory reads the list of every folder, newest first, a page at a time.
func (gi *graphImporter) RunHistory(ctx context.Context) (err error) {
	gi.mu.Lock()
	defer gi.mu.Unlock()
	gi.busy.Store(true)
	defer gi.busy.Store(false)
	if !gi.a.graphSigned() {
		return errNotConnected
	}
	hs := gi.loadHistory(ctx)
	if hs.Started == 0 || hs.Done {
		return nil
	}
	run, err := gi.startRun(ctx, historyMode)
	if err != nil {
		return err
	}
	defer func() { gi.finish(ctx, run, err) }()
	_, sec, err := gi.a.graphAccess(ctx)
	if err != nil {
		return err
	}
	gi.loadPlaces(ctx)
	folders, err := gi.historyFolders(ctx)
	if err != nil {
		return err
	}
	if hs.Folders == nil {
		hs.Folders = map[string]*historyCurse{}
	}
	self := strings.ToLower(sec.Address)
	for i, f := range folders {
		cur := hs.Folders[f.ID]
		if cur == nil {
			cur = &historyCurse{}
			hs.Folders[f.ID] = cur
		}
		if cur.Done {
			continue
		}
		run.Cursor = fmt.Sprintf(`{"mode":"history","folder":%d,"of":%d}`, i+1, len(folders))
		path := historyPath(f.ID)
		if cur.Next != "" {
			if path, err = gi.a.graph.pathOfLink(cur.Next); err != nil {
				return err
			}
		}
		for path != "" {
			if err = ctx.Err(); err != nil {
				return err
			}
			if err = gi.yieldHistory(ctx); err != nil {
				return err
			}
			var page struct {
				Value []graphMessage `json:"value"`
				Next  string         `json:"@odata.nextLink"`
			}
			size := gi.historyPage
			if size == 0 {
				size = historyGraphN
			}
			if err = gi.getSized(ctx, path, size, &page); err != nil {
				if isResync(err) && cur.Next != "" { // the saved place is no longer good: this folder starts again
					cur.Next, path = "", historyPath(f.ID)
					err = nil
					continue
				}
				return err
			}
			if err = gi.recordHistory(ctx, self, f.Place, page.Value, run); err != nil {
				return err
			}
			path = ""
			if page.Next != "" {
				if path, err = gi.a.graph.pathOfLink(page.Next); err != nil {
					return err
				}
				cur.Next = page.Next
			} else {
				cur.Next, cur.Done = "", true
			}
			if err = gi.a.saveHistoryState(ctx, historyKeyO, hs); err != nil {
				return err
			}
			gi.core.saveRun(ctx, run)
		}
	}
	hs.Done = true
	return gi.a.saveHistoryState(ctx, historyKeyO, hs)
}

// yieldHistory runs the regular sync inside a long history pass when its time has come.
func (gi *graphImporter) yieldHistory(ctx context.Context) error {
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
	st := gi.loadState(ctx)
	return gi.syncOn(ctx, &st)
}

// getSized is get with the size of a page the caller wants.
func (gi *graphImporter) getSized(ctx context.Context, path string, size int, out any) error {
	return gi.do(ctx, func(token string) error {
		return gi.a.graph.apiPrefer(ctx, token, "GET", path, append(graphImmutable, fmt.Sprintf("odata.maxpagesize=%d", size)), nil, out)
	})
}

func (gi *graphImporter) recordHistory(ctx context.Context, self, place string, msgs []graphMessage, run *repo.Run) error {
	now := gi.a.now().Unix()
	return gi.a.store.Update(ctx, func(tx *repo.Tx) error {
		for _, m := range msgs {
			run.Fetched++
			if m.ID == "" || m.IsDraft || m.Removed != nil {
				continue
			}
			addr, ok := cleanAddress(m.From.E.Address)
			if !ok {
				addr = strings.ToLower(strings.TrimSpace(m.From.E.Address))
			}
			addr = strings.ToLower(addr)
			err := tx.Try(ctx, func() error {
				return gi.a.tryUnit("history message", func() error {
					changed, err := tx.RecordHistory(ctx, repo.HistoryRow{Source: srcGraph, SourceID: m.ID, Thread: m.ConversationID, Sender: addr, SenderName: m.From.E.Name,
						ReceivedAt: receivedOf(m).Unix(), Seen: m.IsRead, Starred: strings.EqualFold(m.Flag.Status, "flagged"), Mine: addr == self || place == placeSent, State: place}, now)
					if changed {
						run.Stored++
					}
					return err
				})
			})
			if err != nil {
				if isFatal(ctx, err) {
					return err
				}
				run.Failed++
				gi.a.logLeftOut(srcGraph, m.ID, "history", 1, err)
			}
		}
		return nil
	})
}

var _ = errors.New
