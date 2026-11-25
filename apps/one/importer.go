package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"lab/apps/one/repo"
	"lab/platform/appkit"
)

// The Gmail import. It reads the All Mail folder over IMAP, newest first,
// in small pages. It follows DESIGN.md sections 3.5, 5.2, 5.4 and 6, and it
// only reads: nothing in the mailbox is ever changed.

const (
	srcGmail      = "gmail"
	stateKey      = "sync.gmail"
	pageSize      = 40 // messages per page; the app has 256 MB
	maxStoredText = 1 << 20
)

const (
	keyInterval   = "sync_interval_minutes"
	keyFirstDepth = "first_import_depth_days"
	keyReviewDays = "review_window_days"
)

// syncSettings are the general settings this slice uses, with the defaults
// of DESIGN.md 11.3.
type syncSettings struct {
	Interval     time.Duration
	FirstDepth   time.Duration
	ReviewWindow time.Duration
}

func (a *app) syncSettings(ctx context.Context) syncSettings {
	days := func(key string, def int) time.Duration {
		if n, err := strconv.Atoi(a.store.Setting(ctx, key)); err == nil && n > 0 {
			return time.Duration(n) * 24 * time.Hour
		}
		return time.Duration(def) * 24 * time.Hour
	}
	iv := 5 * time.Minute
	if n, err := strconv.Atoi(a.store.Setting(ctx, keyInterval)); err == nil && n > 0 {
		iv = time.Duration(n) * time.Minute
	}
	return syncSettings{Interval: iv, FirstDepth: days(keyFirstDepth, 365), ReviewWindow: days(keyReviewDays, 30)}
}

// gmailState is where the import stands: it survives a stop or a crash.
type gmailState struct {
	UIDValidity uint32    `json:"uidvalidity"`
	HighUID     uint32    `json:"high_uid"`           // every message up to here was seen by the timer's pass or started before the import
	ModSeq      uint64    `json:"modseq"`             // changes up to here are followed
	Backfill    *backfill `json:"backfill,omitempty"` // the first import
	Manual      *backfill `json:"manual,omitempty"`   // the last "Import one week" or "Import one month"
}

// target is the import a mode works on: the first import has its own place,
// so a manual import never takes its position away.
func (st *gmailState) target(mode string) **backfill {
	if mode == "first" {
		return &st.Backfill
	}
	return &st.Manual
}

// backfill is one import going back in time, newest first. Every wanted
// message with a UID of Low or more is done; Low 0 means none yet.
type backfill struct {
	Mode  string `json:"mode"` // first, week or month
	Since int64  `json:"since"`
	Low   uint32 `json:"low"`
	Done  bool   `json:"done"`
}

var errNotConnected = errors.New("Gmail is not connected")

type importRequest struct {
	Mode  string
	Depth time.Duration
}

type importer struct {
	a        *app
	core     *mailCore
	pool     pool
	mu       sync.Mutex // one run at a time
	busy     atomic.Bool
	kick     chan importRequest
	rearm    chan struct{} // the sync interval was changed: the timer starts again
	pageSize int
	lastSync time.Time
	log      *slog.Logger
	// historySpan is how many UIDs the history pass reads at a time (0: the default).
	historySpan int
	// holdFirstImport keeps the timer from starting the long first import
	// until the owner has run "Import one month" once: DESIGN.md 14.2 rule 10,
	// the first real import is small and early.
	holdFirstImport bool
}

func newImporter(a *app) *importer {
	p := newPool(a.dataDir + "/files")
	log := appkit.Logger()
	return &importer{
		a:        a,
		core:     &mailCore{a: a, pool: p, log: log},
		pool:     p,
		kick:     make(chan importRequest, 1),
		rearm:    make(chan struct{}, 1),
		pageSize: pageSize,
		log:      log,

		holdFirstImport: true,
	}
}

// Busy says whether a run is going on.
func (im *importer) Busy() bool { return im.busy.Load() }

// Request asks the loop for an import of the given depth now; false when one is running.
func (im *importer) Request(mode string, depth time.Duration) bool {
	if im.busy.Load() {
		return false
	}
	select {
	case im.kick <- importRequest{Mode: mode, Depth: depth}:
		return true
	default:
		return false
	}
}

// Loop is the timer for the sources: every sync interval it syncs Gmail and
// then the Office mailbox, the first time it starts the long first import
// of each, and then reads the calendars; and it answers the buttons.
func (im *importer) Loop(ctx context.Context) {
	ctxBg := context.WithoutCancel(ctx)
	im.a.runUnit("importer start", func() {
		im.store().Update(ctxBg, func(tx *repo.Tx) error { return tx.CloseStaleRuns(ctxBg, im.a.now().Unix()) })
		im.seedRules(ctx)
	})
	timer := time.NewTimer(3 * time.Second)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case req := <-im.kick:
			im.a.runUnit("gmail import", func() {
				if req.Mode == historyMode {
					im.logErr(im.RunHistory(ctx))
				} else {
					im.logErr(im.RunImport(ctx, req.Mode, req.Depth))
				}
			})
		case req := <-im.a.gimp.kick:
			im.a.runUnit("office import", func() {
				if req.Mode == historyMode {
					im.a.gimp.logErr(im.a.gimp.RunHistory(ctx))
				} else {
					im.a.gimp.logErr(im.a.gimp.RunImport(ctx, req.Mode, req.Depth))
				}
			})
		case <-im.rearm:
			// The owner changed the sync interval: nothing to run, only a new wait.
		case <-timer.C:
			// One source after the other.
			im.a.runUnit("timer round", func() { im.a.fetchAll(ctx) }) // skipped when Refresh is already reading
		}
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		timer.Reset(im.a.syncSettings(ctx).Interval)
	}
}

func (im *importer) logErr(err error) {
	if err == nil || errors.Is(err, errNotConnected) || errors.Is(err, errNeedsLogin) || errors.Is(err, context.Canceled) {
		return
	}
	// The error text is kept on the run and shown on the page; the log gets
	// only that something failed.
	im.log.Warn("gmail run failed")
}

// Tick is one timer round: a continued import when one was left unfinished,
// the first import when none was ever started (once it is allowed), and
// otherwise a plain sync.
func (im *importer) Tick(ctx context.Context) error {
	if _, ok := im.a.gmailSecret(); !ok {
		return errNotConnected
	}
	if im.a.gmailNeedsLogin(ctx) {
		return errNeedsLogin // SPEC 24: paused until the password is entered again
	}
	st := im.loadState(ctx)
	switch {
	case st.Backfill != nil && !st.Backfill.Done:
		return im.RunImport(ctx, "first", 0)
	case st.Manual != nil && !st.Manual.Done:
		return im.RunImport(ctx, st.Manual.Mode, 0)
	case st.Backfill == nil && im.firstImportAllowed(st):
		return im.RunImport(ctx, "first", im.a.syncSettings(ctx).FirstDepth)
	}
	// The history of the whole mailbox (SPEC 21) goes on after the normal
	// imports and gives way to the timer. It starts by itself once the first
	// import has finished: it only reads, and the rules Claude proposes need
	// it. The button starts it again later.
	hs := im.loadHistory(ctx)
	if hs.Started == 0 && st.Backfill != nil && st.Backfill.Done {
		if err := im.a.startHistory(ctx, historyKeyG); err == nil {
			hs = im.loadHistory(ctx)
		}
	}
	if hs.Started > 0 && !hs.Done {
		return im.RunHistory(ctx)
	}
	return im.RunSync(ctx)
}

func (im *importer) firstImportAllowed(st gmailState) bool {
	return !im.holdFirstImport || (st.Manual != nil && st.Manual.Mode == "month" && st.Manual.Done)
}

func (im *importer) store() *store { return im.a.store }

func (im *importer) seedRules(ctx context.Context) {
	for _, r := range startingRules {
		if err := im.store().Update(ctx, func(tx *repo.Tx) error { return tx.EnsureRule(ctx, r, im.a.now().Unix()) }); err != nil {
			im.log.Error("could not add the starting rules")
		}
	}
}

func (im *importer) loadState(ctx context.Context) gmailState {
	var st gmailState
	json.Unmarshal([]byte(im.store().Setting(ctx, stateKey)), &st)
	return st
}

func (im *importer) saveState(ctx context.Context, st gmailState) error {
	b, _ := json.Marshal(st)
	return im.store().SetSettingBy(context.WithoutCancel(ctx), stateKey, string(b), "system", im.a.now())
}

// session is one signed-in connection with All Mail open.
type session struct {
	ic     *imapConn
	folder string
	info   mailboxInfo
	done   chan struct{}
	once   sync.Once
}

func (im *importer) open(ctx context.Context, sec gmailSecret) (*session, error) {
	cfg := im.a.gmail
	ic, err := dialIMAP(cfg.IMAPAddr, cfg.TLS, cfg.Timeout)
	if err != nil {
		return nil, err
	}
	s := &session{ic: ic, done: make(chan struct{})}
	// A stop of the app closes the connection, which ends a read in progress.
	go func() {
		select {
		case <-ctx.Done():
			ic.Close()
		case <-s.done:
		}
	}()
	if _, err := ic.do("LOGIN " + quote(sec.Address) + " " + quote(sec.Password)); err != nil {
		s.close()
		var ref *errIMAPRefused
		if errors.As(err, &ref) && !imapTransient(ref.Text) {
			return nil, &gmailLoginErr{loginError(err)}
		}
		return nil, loginError(err)
	}
	lst, err := ic.do(`LIST "" "*"`)
	if err != nil {
		s.close()
		return nil, err
	}
	s.folder = folderWithAttr(lst.untagged, `\All`)
	if s.folder == "" {
		s.folder = "[Gmail]/All Mail"
	}
	if s.info, err = ic.examine(s.folder); err != nil {
		s.close()
		return nil, err
	}
	return s, nil
}

func (s *session) close() {
	s.once.Do(func() { close(s.done) })
	s.ic.do("LOGOUT")
	s.ic.Close()
}

// refresh reads the folder's numbers again, for a sync in the middle of a long import.
func (s *session) refresh() error {
	info, err := s.ic.examine(s.folder)
	if err != nil {
		return err
	}
	s.info = info
	return nil
}

// runScope is the shared scope of a run plus Gmail's open connection.
type runScope struct {
	*mailScope
	im     *importer
	sess   *session
	secret string
}

func (im *importer) startRun(ctx context.Context, mode string) (*repo.Run, error) {
	return im.core.startRun(ctx, gmailSrc, mode)
}

func (im *importer) saveRun(ctx context.Context, run *repo.Run) { im.core.saveRun(ctx, run) }

// endRun records how a run ended, in plain words when it failed.
func (im *importer) endRun(ctx context.Context, run *repo.Run, err error, secret string) {
	im.core.endRun(ctx, gmailSrc, run, err, func(err error) string {
		var gl *gmailLoginErr
		switch {
		case errors.Is(err, errNotConnected):
			return "Gmail is not connected."
		case errors.As(err, &gl):
			return gmailLoginText
		}
		return plainError("Gmail", err, secret)
	})
	// SPEC 24: a refused password pauses Gmail until it is entered again; a
	// run that worked ends the pause.
	var gl *gmailLoginErr
	switch {
	case err == nil:
		im.a.setGmailNeedsLogin(ctx, false)
	case errors.As(err, &gl):
		im.a.setGmailNeedsLogin(ctx, true)
	}
	im.a.checkSource(ctx, gmailSrc)
}

func (im *importer) scope(ctx context.Context, sess *session, run *repo.Run, sec gmailSecret) (*runScope, error) {
	ms, err := im.core.scope(ctx, gmailSrc, run, sec.Address)
	if err != nil {
		return nil, err
	}
	return &runScope{mailScope: ms, im: im, sess: sess, secret: sec.Password}, nil
}

// RunSync is the regular pass: new messages first, then changed flags and labels.
func (im *importer) RunSync(ctx context.Context) error {
	im.mu.Lock()
	defer im.mu.Unlock()
	im.busy.Store(true)
	defer im.busy.Store(false)
	sec, ok := im.a.gmailSecret()
	if !ok {
		return errNotConnected
	}
	sess, err := im.open(ctx, sec)
	if err != nil {
		run, rerr := im.startRun(ctx, "sync")
		if rerr == nil {
			im.endRun(ctx, run, err, sec.Password)
		}
		return err
	}
	defer sess.close()
	st := im.loadState(ctx)
	return im.syncOn(ctx, sess, sec, &st)
}

// syncOn runs the regular pass on an open session as its own run.
func (im *importer) syncOn(ctx context.Context, sess *session, sec gmailSecret, st *gmailState) (err error) {
	run, err := im.startRun(ctx, "sync")
	if err != nil {
		return err
	}
	defer func() { im.endRun(ctx, run, err, sec.Password) }()
	if err = sess.refresh(); err != nil {
		return err
	}
	im.resetOnNewValidity(sess, st)
	sc, err := im.scope(ctx, sess, run, sec)
	if err != nil {
		return err
	}
	if err = sc.finishPending(ctx); err != nil {
		return err
	}
	if err = sc.retryFailed(ctx); err != nil {
		return err
	}
	if err = sc.syncNew(ctx, st); err != nil {
		return err
	}
	if err = sc.syncChanged(ctx, st); err != nil {
		return err
	}
	im.lastSync = im.a.now()
	return nil
}

// resetOnNewValidity starts the numbering over when Gmail says the old UIDs
// no longer mean anything. Nothing is lost: a message is known by Gmail's
// own message id, so nothing is stored twice.
func (im *importer) resetOnNewValidity(sess *session, st *gmailState) {
	if st.UIDValidity == sess.info.UIDValidity {
		return
	}
	st.UIDValidity = sess.info.UIDValidity
	st.HighUID = sess.info.UIDNext - 1
	st.ModSeq = sess.info.HighestModSeq
	st.Backfill, st.Manual = nil, nil
}

// syncNew takes the messages that arrived after the last pass, oldest first,
// and keeps its place after every page.
func (sc *runScope) syncNew(ctx context.Context, st *gmailState) error {
	uids, err := sc.sess.ic.uidSearch(fmt.Sprintf("UID %d:*", st.HighUID+1))
	if err != nil {
		return err
	}
	uids = slices.DeleteFunc(uids, func(u uint32) bool { return u <= st.HighUID })
	for i := 0; i < len(uids); i += sc.im.pageSize {
		page := uids[i:min(i+sc.im.pageSize, len(uids))]
		if err := sc.page(ctx, page, false); err != nil {
			return err
		}
		st.HighUID = page[len(page)-1]
		if err := sc.im.saveState(ctx, *st); err != nil {
			return err
		}
	}
	return nil
}

// syncChanged follows read state, labels and stars of known messages
// (CONDSTORE): each change is a new raw version of the one message.
func (sc *runScope) syncChanged(ctx context.Context, st *gmailState) error {
	now := sc.sess.info.HighestModSeq
	if now == 0 {
		return nil // the server keeps no change counter
	}
	if st.ModSeq == 0 {
		st.ModSeq = now
		return sc.im.saveState(ctx, *st)
	}
	if now > st.ModSeq {
		err := sc.sess.ic.fetchChanged(st.ModSeq, func(m msgMeta) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			return sc.tolerate(ctx, m.MsgID, sc.unit(func() error { return sc.changed(ctx, m) }))
		})
		if err != nil {
			return err
		}
	}
	st.ModSeq = now
	return sc.im.saveState(ctx, *st)
}

// RunImport goes back in time from now: newest first, in pages, resumable.
// An import already started with this mode is continued; depth 0 means so.
func (im *importer) RunImport(ctx context.Context, mode string, depth time.Duration) (err error) {
	im.mu.Lock()
	defer im.mu.Unlock()
	im.busy.Store(true)
	defer im.busy.Store(false)
	sec, ok := im.a.gmailSecret()
	if !ok {
		return errNotConnected
	}
	run, err := im.startRun(ctx, mode)
	if err != nil {
		return err
	}
	defer func() { im.endRun(ctx, run, err, sec.Password) }()
	sess, err := im.open(ctx, sec)
	if err != nil {
		return err
	}
	defer sess.close()
	st := im.loadState(ctx)
	im.resetOnNewValidity(sess, &st)
	slot := st.target(mode)
	if *slot == nil || (*slot).Done || (*slot).Mode != mode {
		if depth <= 0 {
			depth = im.a.syncSettings(ctx).FirstDepth
		}
		*slot = &backfill{Mode: mode, Since: im.a.now().Add(-depth).Unix()}
	}
	if err = im.saveState(ctx, st); err != nil {
		return err
	}
	sc, err := im.scope(ctx, sess, run, sec)
	if err != nil {
		return err
	}
	if err = sc.finishPending(ctx); err != nil {
		return err
	}
	if err = sc.retryFailed(ctx); err != nil {
		return err
	}
	return sc.backfill(ctx, &st, *slot)
}

// backfill works through the wanted UIDs from the newest down.
func (sc *runScope) backfill(ctx context.Context, st *gmailState, bf *backfill) error {
	since := time.Unix(bf.Since, 0).UTC().Format("2-Jan-2006")
	uids, err := sc.sess.ic.uidSearch("SINCE " + since)
	if err != nil {
		return err
	}
	slices.Reverse(uids)
	if bf.Low != 0 {
		uids = slices.DeleteFunc(uids, func(u uint32) bool { return u >= bf.Low })
	}
	for i := 0; i < len(uids); i += sc.im.pageSize {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := sc.yieldToTimer(ctx, st); err != nil {
			return err
		}
		page := uids[i:min(i+sc.im.pageSize, len(uids))]
		if err := sc.page(ctx, page, true); err != nil {
			return err
		}
		bf.Low = page[len(page)-1]
		sc.run.Cursor = fmt.Sprintf(`{"mode":%q,"low":%d}`, bf.Mode, bf.Low)
		if err := sc.im.saveState(ctx, *st); err != nil {
			return err
		}
	}
	bf.Done = true
	return sc.im.saveState(ctx, *st)
}

// yieldToTimer runs the regular sync inside a long import when its time has
// come, so the import never starves it.
func (sc *runScope) yieldToTimer(ctx context.Context, st *gmailState) error {
	im := sc.im
	if im.lastSync.IsZero() {
		if r, ok := im.store().LastFinishedRun(ctx, srcGmail, "sync"); ok {
			im.lastSync = time.Unix(r.FinishedAt, 0)
		} else {
			im.lastSync = im.a.now()
		}
	}
	if im.a.now().Sub(im.lastSync) < im.a.syncSettings(ctx).Interval {
		return nil
	}
	sec, ok := im.a.gmailSecret()
	if !ok {
		return errNotConnected
	}
	validity := st.UIDValidity
	if err := im.syncOn(ctx, sc.sess, sec, st); err != nil {
		return err
	}
	if st.UIDValidity != validity {
		return errors.New("Gmail renumbered the folder; the import starts again at the next round")
	}
	return nil
}

// page reads a page of messages by UID and stores each. Newest first for an
// import (descending=true), oldest first for the regular pass.
func (sc *runScope) page(ctx context.Context, uids []uint32, descending bool) error {
	metas, err := sc.sess.ic.fetchMeta(uids)
	if err != nil {
		return err
	}
	slices.SortFunc(metas, func(a, b msgMeta) int {
		if descending {
			return int(int64(b.UID) - int64(a.UID))
		}
		return int(int64(a.UID) - int64(b.UID))
	})
	for _, m := range metas {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := sc.tolerate(ctx, m.MsgID, sc.unit(func() error { return sc.message(ctx, m) })); err != nil {
			return err
		}
		sc.im.saveRun(ctx, sc.run)
	}
	return nil
}

func (m msgMeta) facts() ruleFacts {
	_, addr := parseAddress(m.Header["from"])
	return ruleFacts{Sender: addr, Subject: decodeHeaderText(m.Header["subject"]), Labels: m.Labels, Headers: m.Header}
}

func payloadOf(m msgMeta, validity uint32) string {
	b, _ := json.Marshal(map[string]any{
		"msgid": m.MsgID, "thrid": m.ThreadID, "uid": m.UID, "uidvalidity": validity,
		"labels": m.Labels, "flags": m.Flags, "internaldate": m.Date.Unix(), "size": m.Size, "modseq": m.ModSeq,
	})
	return string(b)
}

// record is what the shared path needs to know of a Gmail message.
func (sc *runScope) record(m msgMeta) mailRecord {
	return mailRecord{SourceID: m.MsgID, ThreadID: m.ThreadID, Received: m.Date, Labels: m.Labels, Flags: m.Flags,
		Payload: payloadOf(m, sc.sess.info.UIDValidity), Facts: m.facts(), Place: gmailPlace(m.Labels),
		Peek: func() string {
			var b bytes.Buffer
			if _, err := sc.sess.ic.fetchPrefix(m.UID, 6000, &b); err != nil {
				return ""
			}
			return textOfPrefix(b.Bytes())
		}}
}

// message is the whole pipeline for one message of Gmail.
func (sc *runScope) message(ctx context.Context, m msgMeta) error {
	sc.run.Fetched++
	if m.hasFlag(`\Draft`) || m.hasLabel(`\Draft`) {
		return nil
	}
	return sc.handle(ctx, sc.record(m),
		func(w *poolWriter) error { _, err := sc.sess.ic.fetchBody(m.UID, w); return err },
		func() parsedMail { return parsedFromMeta(m) })
}

func parsedFromMeta(m msgMeta) parsedMail {
	pm := parsedMail{Subject: decodeHeaderText(m.Header["subject"]), MessageID: m.Header["message-id"]}
	pm.FromName, pm.FromAddr = parseAddress(m.Header["from"])
	pm.To = parseAddressList(m.Header["to"])
	pm.Cc = parseAddressList(m.Header["cc"])
	return pm
}

// changed handles one line of a CONDSTORE answer: a known message whose state moved.
func (sc *runScope) changed(ctx context.Context, m msgMeta) error {
	raw, ok := sc.im.store().LatestRaw(ctx, srcGmail, m.MsgID)
	if !ok {
		return nil // not imported (older than the import went); nothing to follow
	}
	rec := sc.record(m)
	state := rec.state()
	if strings.HasPrefix(raw.VersionKey, state+"@") {
		return nil
	}
	return sc.newVersion(ctx, rec, raw, state)
}

// Reimport brings back one message a rule dropped, by importing it again
// with the rules left out for this message only.
func (im *importer) Reimport(ctx context.Context, skippedID int64) (err error) {
	im.mu.Lock()
	defer im.mu.Unlock()
	im.busy.Store(true)
	defer im.busy.Store(false)
	sec, ok := im.a.gmailSecret()
	if !ok {
		return errNotConnected
	}
	sk, ok := im.store().SkippedByID(ctx, skippedID)
	if !ok || sk.Source != srcGmail {
		return errors.New("there is no such skipped message")
	}
	run, err := im.startRun(ctx, "reimport")
	if err != nil {
		return err
	}
	defer func() { im.endRun(ctx, run, err, sec.Password) }()
	sess, err := im.open(ctx, sec)
	if err != nil {
		return err
	}
	defer sess.close()
	sc, err := im.scope(ctx, sess, run, sec)
	if err != nil {
		return err
	}
	sc.ignoreRules = true
	sc.strict = true // the owner asked for this one: a failure is told to him, not left out
	uids, err := sess.ic.uidSearch("X-GM-MSGID " + sk.SourceID)
	if err != nil {
		return err
	}
	if len(uids) == 0 {
		err = errors.New("Gmail no longer has that message")
		return err
	}
	if err = sc.page(ctx, uids, true); err != nil {
		return err
	}
	return im.store().Update(ctx, func(tx *repo.Tx) error { return tx.MarkReimported(ctx, sk.ID, im.a.now().Unix()) })
}

// retryFailed tries again the messages that failed on their own in an earlier
// run (the ledger of softfail.go): each is found by Gmail's own message id,
// so a renumbered folder does not matter, and one that Gmail no longer has
// leaves the ledger. A failure of the connection ends the run as always.
func (sc *runScope) retryFailed(ctx context.Context) error {
	a := sc.im.a
	for _, id := range a.failDue(ctx, srcGmail, pageSize) {
		uids, err := sc.sess.ic.uidSearch("X-GM-MSGID " + id)
		if err != nil {
			return err
		}
		if len(uids) == 0 {
			a.failClear(ctx, srcGmail, id)
			continue
		}
		metas, err := sc.sess.ic.fetchMeta(uids[:1])
		if err != nil {
			return err
		}
		for _, m := range metas {
			if err := sc.tolerate(ctx, id, sc.unit(func() error { return sc.message(ctx, m) })); err != nil {
				return err
			}
		}
	}
	return nil
}

// imapTransient says whether a refused LOGIN is the server being busy or
// unwell (Gmail's UNAVAILABLE, THROTTLED, "too many simultaneous
// connections") and not the password being refused: the first must not pause
// fetching until the owner signs in again, it is tried again at the next read and
// counts as a failed fetch like any other.
func imapTransient(text string) bool {
	t := strings.ToLower(text)
	for _, w := range []string{"unavailable", "throttled", "too many", "temporar", "try again", "system error", "overloaded"} {
		if strings.Contains(t, w) {
			return true
		}
	}
	return false
}
