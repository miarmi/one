package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"log/slog"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"

	"lab/apps/one/repo"
)

// The one path both mail sources use once a message is known: deciding
// whether it is new, a new version or nothing, making items, saving files,
// applying the import rules and keeping the run records. What differs
// between Gmail and the Office mailbox (how messages are listed and
// fetched) lives in importer.go and graphimport.go. DESIGN.md 3.5, 5.2, 6.

// mailSource says how one source is named in the data.
type mailSource struct {
	Name         string // raw.source, messages.source, runs and rules: "gmail" or "outlook"
	Label        string // in plain words for the page
	LinkKind     string // how its threads link to items
	ThreadPrefix string // in front of the thread id in raw.thread_key
}

var (
	gmailSrc = mailSource{Name: srcGmail, Label: "Gmail", LinkKind: "gmail_thread", ThreadPrefix: "gmail:"}
	graphSrc = mailSource{Name: srcGraph, Label: "Office mail", LinkKind: "outlook_conversation", ThreadPrefix: "outlook:"}
)

// errKnown ends the work on one record that is already stored.
var errKnown = errors.New("already stored")

// mailRecord is one message as a source describes it, before its content
// has been read. Flags use Gmail's words (\Seen, \Flagged) for both sources.
type mailRecord struct {
	SourceID string
	ThreadID string
	Received time.Time
	Labels   []string
	Flags    []string
	Payload  string // the source's own description of the message, kept as the raw payload
	Facts    ruleFacts
	// Place says where the source keeps the message, for the history (SPEC 21):
	// inbox, archived, sent, deleted, spam or folder.
	Place string
	// StateExtra is more that makes up a version of the message (a folder's id).
	StateExtra []string
	// LateHeaders reads the headers when a rule needs them and the source
	// did not send them with the message.
	LateHeaders func() (map[string]string, error)
	// Peek gives the beginning of the text of a message an import rule drops
	// (SPEC 31, for the night check). It is called only for dropped mail, may
	// return nothing, and is best effort.
	Peek func() string
}

func (r mailRecord) hasFlag(f string) bool {
	for _, x := range r.Flags {
		if strings.EqualFold(x, f) {
			return true
		}
	}
	return false
}

func (r mailRecord) state() string {
	return stateHash(r.Flags, append(slices.Clone(r.Labels), r.StateExtra...))
}

// stateHash is what a message's mutable state looks like: read, starred,
// labels. Two states with the same hash are the same version.
func stateHash(flags, labels []string) string {
	f := slices.Clone(flags)
	l := slices.Clone(labels)
	for i := range f {
		f[i] = strings.ToLower(f[i])
	}
	slices.Sort(f)
	slices.Sort(l)
	sum := sha256.Sum256([]byte(strings.Join(f, "\x00") + "\x01" + strings.Join(l, "\x00")))
	return hex.EncodeToString(sum[:8])
}

// mailCore is what both importers share: the app, the file pool and the log.
type mailCore struct {
	a    *app
	pool pool
	log  *slog.Logger
}

func (c *mailCore) store() *store { return c.a.store }

func (c *mailCore) startRun(ctx context.Context, src mailSource, mode string) (*repo.Run, error) {
	run := &repo.Run{Source: src.Name, Mode: mode}
	err := c.store().Update(ctx, func(tx *repo.Tx) error {
		id, err := tx.StartRun(ctx, src.Name, mode, c.a.now().Unix())
		run.ID = id
		return err
	})
	return run, err
}

func (c *mailCore) saveRun(ctx context.Context, run *repo.Run) {
	c.store().Update(context.WithoutCancel(ctx), func(tx *repo.Tx) error { return tx.SaveRun(ctx, *run) })
}

// endRun records how a run ended. A failure keeps its position, which is
// already in the saved state, and says what happened in plain words: plain
// turns the error into the sentence for the page.
func (c *mailCore) endRun(ctx context.Context, src mailSource, run *repo.Run, err error, plain func(error) string) {
	status, text := "done", ""
	switch {
	case err == nil:
	case errors.Is(err, context.Canceled) || ctx.Err() != nil:
		status, text = "interrupted", "Stopped before it finished; it continues where it left off."
	default:
		status, text = "failed", plain(err)
	}
	bg := context.WithoutCancel(ctx)
	if e := c.store().Update(bg, func(tx *repo.Tx) error {
		return tx.FinishRun(bg, *run, status, text, c.a.now().Unix())
	}); e != nil {
		c.log.Error("could not record the end of a run")
	}
	// Memory the app holds from the system, so a run close to the lab's limit
	// (256 MB unless app.json says otherwise) shows in the logs.
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	c.log.Info(src.Name+" run ended", "run", run.ID, "mode", run.Mode, "status", status, "detail", text,
		"read", run.Fetched, "stored", run.Stored, "left_out", run.Failed, "skipped", run.Skipped, "items", run.ItemsMade, "files", run.FilesSaved, "bytes", run.Bytes,
		"mem_mb", ms.Sys>>20)
}

// mailScope holds what one run of one source needs while it works.
type mailScope struct {
	c           *mailCore
	src         mailSource
	run         *repo.Run
	self        string
	rules       []repo.Rule
	ignoreRules bool
	strict      bool // one message asked for by hand: its failure is returned, never left out
	consec      int  // records in a row that failed on their own
}

func (c *mailCore) scope(ctx context.Context, src mailSource, run *repo.Run, self string) (*mailScope, error) {
	rules, err := c.store().EnabledRules(ctx, src.Name)
	if err != nil {
		return nil, err
	}
	return &mailScope{c: c, src: src, run: run, self: strings.ToLower(self), rules: rules}, nil
}

// ruleHit says whether an import rule drops the message.
func (s *mailScope) ruleHit(rec mailRecord) (repo.Rule, bool, error) {
	if s.ignoreRules {
		return repo.Rule{}, false, nil
	}
	if rec.LateHeaders != nil {
		h, err := rec.LateHeaders()
		if err != nil {
			return repo.Rule{}, false, conn(err)
		}
		rec.Facts.Headers = h
	}
	r, hit := firstRuleHit(s.rules, rec.Facts)
	return r, hit, nil
}

// skip keeps one metadata line for a message a rule dropped.
func (s *mailScope) skip(ctx context.Context, rec mailRecord, r repo.Rule) error {
	at := rec.Received.Unix()
	snippet := ""
	if rec.Peek != nil {
		snippet = snippetOf(rec.Peek(), 400)
	}
	var inserted bool
	err := s.c.store().Update(ctx, func(tx *repo.Tx) error {
		var err error
		inserted, err = tx.InsertSkipped(ctx, repo.Skipped{At: s.c.a.now().Unix(), Source: s.src.Name, SourceID: rec.SourceID,
			Sender: rec.Facts.Sender, Subject: oneLine(rec.Facts.Subject, 300), ReceivedAt: at, RuleID: r.ID, Snippet: snippet})
		if err != nil {
			return err
		}
		// A dropped message is still mail the owner has or has not read.
		_, err = tx.RecordHistory(ctx, repo.HistoryRow{Source: s.src.Name, SourceID: rec.SourceID, Thread: rec.ThreadID, Sender: rec.Facts.Sender,
			ReceivedAt: at, Seen: rec.hasFlag(`\Seen`), Starred: rec.hasFlag(`\Flagged`), Mine: rec.Facts.Sender == s.self || rec.Place == placeSent, State: rec.Place}, s.c.a.now().Unix())
		return err
	})
	if inserted {
		s.run.Skipped++
	}
	return err
}

// newVersion keeps a changed read state, label, star or folder as a new raw
// version of the one message. No message is added and no item is touched.
func (s *mailScope) newVersion(ctx context.Context, rec mailRecord, prev repo.Raw, state string) error {
	return s.c.store().Update(ctx, func(tx *repo.Tx) error {
		msg, ok := tx.MessageBySource(ctx, s.src.Name, rec.SourceID)
		if !ok {
			return nil
		}
		id, inserted, err := tx.InsertRaw(ctx, repo.Raw{
			Source: s.src.Name, SourceID: rec.SourceID, VersionKey: state + "@" + strconv.FormatInt(prev.ID, 10), ThreadKey: prev.ThreadKey,
			ReceivedAt: prev.ReceivedAt, IngestedAt: s.c.a.now().Unix(), RunID: s.run.ID, BlobSHA: prev.BlobSHA,
			Payload: rec.Payload,
		})
		if err != nil || !inserted {
			return err // already there: stop for this record
		}
		if err := tx.UpdateMessageState(ctx, msg.ID, id, rec.Labels, strings.Join(rec.Flags, " "), rec.hasFlag(`\Seen`), rec.hasFlag(`\Flagged`)); err != nil {
			return err
		}
		return tx.UpdateHistoryState(ctx, s.src.Name, rec.SourceID, rec.hasFlag(`\Seen`), rec.hasFlag(`\Flagged`), rec.Place)
	})
}

// removedKey marks the raw version that says a message is gone from its mailbox.
const removedKey = "removed@"

// markRemoved records that the source no longer has a message. The message
// stays in the app (SPEC 30); the removal is one more raw version. accept
// decides, from the newest version stored, whether this removal counts, so
// a message that was only moved is not recorded as gone.
func (s *mailScope) markRemoved(ctx context.Context, sourceID, payload string, accept func(repo.Raw) (bool, error)) error {
	prev, ok := s.c.store().LatestRaw(ctx, s.src.Name, sourceID)
	if !ok || strings.HasPrefix(prev.VersionKey, removedKey) {
		return nil
	}
	if counts, err := accept(prev); err != nil || !counts {
		return err
	}
	return s.c.store().Update(ctx, func(tx *repo.Tx) error {
		msg, ok := tx.MessageBySource(ctx, s.src.Name, sourceID)
		if !ok {
			return nil
		}
		id, inserted, err := tx.InsertRaw(ctx, repo.Raw{
			Source: s.src.Name, SourceID: sourceID, VersionKey: removedKey + strconv.FormatInt(prev.ID, 10), ThreadKey: prev.ThreadKey,
			ReceivedAt: prev.ReceivedAt, IngestedAt: s.c.a.now().Unix(), RunID: s.run.ID, BlobSHA: prev.BlobSHA, Payload: payload,
		})
		if err != nil || !inserted {
			return err
		}
		flags := strings.TrimSpace(strings.Replace(msg.Flags, "removed", "", 1) + " removed")
		if err := tx.UpdateMessageState(ctx, msg.ID, id, msg.Labels, flags, msg.Seen, msg.Starred); err != nil {
			return err
		}
		return tx.UpdateHistoryState(ctx, s.src.Name, sourceID, msg.Seen, msg.Starred, placeDeleted)
	})
}

// storeNew writes a message not seen before: raw row, message, item, people,
// event and the rows for its files, all in one transaction.
func (s *mailScope) storeNew(ctx context.Context, rec mailRecord, pm parsedMail, blob, state string) error {
	c := s.c
	now := c.a.now().Unix()
	set := c.a.syncSettings(ctx)
	cutoff := c.a.now().Add(-set.ReviewWindow).Unix()
	received := rec.Received.Unix()
	if rec.Received.IsZero() {
		received = pm.Date.Unix()
		if pm.Date.IsZero() {
			received = now
		}
	}
	mine := pm.FromAddr != "" && pm.FromAddr == s.self
	subject := oneLine(pm.Subject, 500)
	if c.a.beforeStore != nil {
		c.a.beforeStore(subject)
	}
	text := pm.Text
	if len(text) > maxStoredText {
		text = strings.ToValidUTF8(text[:maxStoredText], "")
	}
	addressed := slices.Contains(pm.To, s.self)
	threadKey := s.src.ThreadPrefix + rec.ThreadID
	var made bool
	var messageID int64
	// SPEC 10: the matter of the mail is worked out before the write
	// transaction opens, so a pattern never costs time with the lock held.
	matter := ""
	if ps := c.a.matterPatternsFor(ctx, c.store().Q); len(ps) > 0 {
		matter = matterKey(ps, subject, text)
	}
	err := c.store().Update(ctx, func(tx *repo.Tx) error {
		rawID, inserted, err := tx.InsertRaw(ctx, repo.Raw{
			Source: s.src.Name, SourceID: rec.SourceID, VersionKey: state + "@0", ThreadKey: threadKey,
			ReceivedAt: received, IngestedAt: now, RunID: s.run.ID, BlobSHA: blob, Payload: rec.Payload,
		})
		if err != nil {
			return err
		}
		if !inserted {
			return errKnown
		}
		a := arrival{At: received, SentByMe: mine, Subject: subject}
		item, found := tx.ItemByLink(ctx, s.src.LinkKind, rec.ThreadID)
		// SPEC 10: mail about the same matter joins one row, when the owner set a
		// pattern for it. The thread then belongs to that row from now on.
		if !found && matter != "" {
			if it, ok := tx.ItemByLink(ctx, matterLinkKind, matter); ok && it.TestRun == repo.RunOf(ctx) {
				item, found = it, true
				if err := tx.AddLink(ctx, it.ID, s.src.LinkKind, rec.ThreadID, "system", now); err != nil {
					return err
				}
			}
		}
		var ch itemChange
		if found {
			item, ch = applyArrival(item, a, cutoff, now)
			if err := tx.SaveItem(ctx, item); err != nil {
				return err
			}
			if item.NeedsClaude && !mine {
				// The review is keyed on last_message_at, which a mail of the
				// same second does not move: start the item's review afresh.
				if err := tx.ResetReview(ctx, item.ID); err != nil {
					return err
				}
			}
		} else {
			item, err = tx.CreateItem(ctx, newItem(a, cutoff), s.src.LinkKind, rec.ThreadID, "system", now)
			if err != nil {
				return err
			}
			made = true
		}
		if matter != "" {
			if err := tx.AddLink(ctx, item.ID, matterLinkKind, matter, "system", now); err != nil {
				return err
			}
		}
		stored := repo.Message{
			ItemID: item.ID, RawID: rawID, Source: s.src.Name, SourceID: rec.SourceID, ThreadKey: threadKey, Network: "mail",
			Sender: pm.FromAddr, SenderName: pm.FromName, Subject: subject, To: strings.Join(pm.To, ", "), Cc: strings.Join(pm.Cc, ", "),
			Addressed: addressed, HasAttachments: pm.hasFiles(), Text: text, SentByMe: mine, ReceivedAt: received,
			Labels: rec.Labels, Flags: strings.Join(rec.Flags, " "), Seen: rec.hasFlag(`\Seen`), Starred: rec.hasFlag(`\Flagged`), HeaderID: pm.MessageID,
		}
		messageID, inserted, err = tx.InsertMessage(ctx, stored)
		if err != nil {
			return err
		}
		if !inserted {
			return errKnown
		}
		if mine && found {
			// A mail ONE sent for the owner (SPEC 13) comes back here: it is the message
			// already in the thread, not a second one.
			if err := matchSentReply(ctx, tx, item.ID, stored, messageID, now); err != nil {
				return err
			}
		}
		if _, err := tx.RecordHistory(ctx, repo.HistoryRow{Source: s.src.Name, SourceID: rec.SourceID, Thread: rec.ThreadID, Sender: pm.FromAddr, SenderName: pm.FromName,
			ReceivedAt: received, Seen: rec.hasFlag(`\Seen`), Starred: rec.hasFlag(`\Flagged`), Mine: mine || rec.Place == placeSent, State: rec.Place}, now); err != nil {
			return err
		}
		if pm.FromAddr != "" && !mine {
			if _, err := tx.EnsurePerson(ctx, "mail", pm.FromAddr, pm.FromName, now); err != nil {
				return err
			}
		}
		for _, p := range pm.Parts {
			if _, err := tx.InsertFile(ctx, repo.File{ItemID: item.ID, Source: s.src.Name, MessageID: messageID, Part: p.Part, Filename: p.Filename,
				ContentType: p.ContentType, Disposition: p.Disposition, ContentID: p.ContentID, Size: p.Size, Status: "pending", AddedAt: now}); err != nil {
				return err
			}
		}
		if err := tx.InsertEvent(ctx, now, item.ID, "system", "ingested", map[string]any{"message_id": messageID, "item": repo.Number(item.Number), "new_item": made}); err != nil {
			return err
		}
		if found {
			// SPEC 12: a proposal that no longer fits because a message arrived is withdrawn.
			if _, err := tx.WithdrawPending(ctx, item.ID, "a new message arrived", now); err != nil {
				return err
			}
		}
		if ch.Reopened {
			if err := tx.InsertEvent(ctx, now, item.ID, "system", "reopened", map[string]any{"message_id": messageID}); err != nil {
				return err
			}
		}
		if ch.SetPending {
			if err := tx.InsertEvent(ctx, now, item.ID, "system", "pending_set", map[string]any{"message_id": messageID}); err != nil {
				return err
			}
		}
		return nil
	})
	if errors.Is(err, errKnown) {
		return nil
	}
	if err != nil {
		return err
	}
	s.run.Stored++
	if made {
		s.run.ItemsMade++
	}
	if msg, ok := s.c.store().MessageBySource(ctx, s.src.Name, rec.SourceID); ok && !msg.SentByMe {
		c.a.afterMailStored(ctx, msg.ItemID) // skip rules (SPEC 31)
	}
	// The message is stored whole; its files are a second pass. A file that
	// cannot be saved now stays pending and is tried again by finishPending.
	if err := s.saveFiles(ctx, messageID); err != nil {
		return s.filesLeft(ctx, messageID, err)
	}
	return nil
}

// filesLeft handles a failure of the files of one stored message: the run
// ends for a fatal one, otherwise the message waits in the files ledger.
func (s *mailScope) filesLeft(ctx context.Context, messageID int64, err error) error {
	if s.strict || isFatal(ctx, err) {
		return err
	}
	a := s.c.a
	id := strconv.FormatInt(messageID, 10)
	s.run.Failed++
	a.logLeftOut(s.src.Name, id, "files", a.failNote(ctx, s.src.Name+"-files", id), err)
	return nil
}

// readStored parses an original that is already in the pool.
func (c *mailCore) readStored(rel string) (parsedMail, error) {
	f, err := c.pool.open(rel)
	if err != nil {
		return parsedMail{}, err
	}
	defer f.Close()
	return parseMail(f, countingSink{})
}

// finishPending saves the files of messages stored by an earlier run that
// stopped before it finished them.
func (s *mailScope) finishPending(ctx context.Context) error {
	ids, err := s.c.store().MessagesWithPendingFiles(ctx, 500)
	if err != nil {
		return err
	}
	files := s.src.Name + "-files"
	for _, id := range ids {
		key := strconv.FormatInt(id, 10)
		// A message whose files failed is tried again with growing waits and
		// given up after maxFailTries; one never seen to fail has no line.
		if !s.c.a.failIsDue(ctx, files, key) {
			continue
		}
		if err := s.unit(func() error { return s.saveFiles(ctx, id) }); err != nil {
			if err := s.filesLeft(ctx, id, err); err != nil {
				return err
			}
			continue
		}
		s.c.a.failClear(ctx, files, key)
	}
	return nil
}

// saveFiles is the second pass over a stored message: every file part is
// streamed from the stored original into the pool, once by hash.
func (s *mailScope) saveFiles(ctx context.Context, messageID int64) error {
	st := s.c.store()
	files, err := st.FilesOfMessage(ctx, messageID)
	if err != nil {
		return err
	}
	pending := map[string]int64{}
	for _, f := range files {
		if f.Status == "pending" {
			pending[f.Part] = f.ID
		}
	}
	if len(pending) == 0 {
		return nil
	}
	blob, err := st.MessageBlob(ctx, messageID)
	if err != nil {
		return err
	}
	if len(blob) < 2 {
		return nil // no stored original (a chat message): its files come another way
	}
	src, err := s.c.pool.open(relPath(blob))
	if err != nil {
		return err
	}
	defer src.Close()
	sink := &poolSink{p: s.c.pool}
	pm, perr := parseMail(src, sink)
	err = st.Update(ctx, func(tx *repo.Tx) error {
		for _, p := range pm.Parts {
			id, ok := pending[p.Part]
			if !ok || p.SHA == "" {
				continue
			}
			if err := tx.MarkFileSaved(ctx, id, p.Size, p.SHA, p.Path); err != nil {
				return err
			}
			delete(pending, p.Part)
			s.run.FilesSaved++
		}
		if perr != nil {
			for _, id := range pending {
				if err := tx.MarkFileFailed(ctx, id, "Could not be written to disk; it will be tried again."); err != nil {
					return err
				}
			}
		}
		return nil
	})
	s.run.Bytes += sink.bytes
	if err != nil {
		return err
	}
	return perr
}

// handle is the decision every message goes through once its description is
// known: drop it by a rule, record a change, or read its original and store
// it. fetch writes the original into w and says how many bytes it wrote;
// fallback describes the message when the original cannot be parsed.
func (s *mailScope) handle(ctx context.Context, rec mailRecord, fetch func(w *poolWriter) error, fallback func() parsedMail) error {
	st := s.c.store()
	state := rec.state()
	if raw, ok := st.LatestRaw(ctx, s.src.Name, rec.SourceID); ok {
		if strings.HasPrefix(raw.VersionKey, state+"@") {
			return nil // the same message, nothing changed: nothing is written
		}
		return s.newVersion(ctx, rec, raw, state)
	}
	r, hit, err := s.ruleHit(rec)
	if err != nil {
		return err
	}
	if hit {
		return s.skip(ctx, rec, r)
	}
	// Download the original bytes into the pool, a buffer at a time.
	w, err := s.c.pool.newWriter()
	if err != nil {
		return err
	}
	if err := fetch(w); err != nil {
		w.abort()
		return conn(err) // the connection or the whole fetch: it ends the run
	}
	sha, rel, size, created, err := w.finish()
	if err != nil {
		return err
	}
	if created {
		s.run.Bytes += size
	}
	pm, perr := s.c.readStored(rel)
	if perr != nil {
		pm = fallback()
	}
	return s.storeNew(ctx, rec, pm, sha, state)
}
