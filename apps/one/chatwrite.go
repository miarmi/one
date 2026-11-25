package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"path"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"lab/apps/one/repo"
	"lab/platform/appkit"
)

// The owner writes in a chat himself (SPEC 17, second paragraph). What he writes
// is his own, by channel: it is accepted only on the signed-in page (the cross-
// site guard of the whole app refuses a post from another site), it has no
// address under /api/, no agent tool and no proposal, and nothing an agent
// does can create, change or trigger it. It needs no Approve.
//
// A write is a text and up to five files. Each is a part: the text first, then
// each file as a message of its own, in order. The parts wait in chat_out for
// the bridge, which is handed each once and reports each once; what happens to
// a part never repeats the send. Guards: Same thread (the chat is the item's
// own, never taken from the request), Exact text (what is typed is what goes),
// Send once, Actor by channel. Blocked targets is a guard on what an agent may
// propose; it does not stop the owner writing in a chat himself.

const (
	maxWriteFiles = 5
	maxWriteFile  = 20 << 20 // SPEC 7: 20 MB each
	maxWriteText  = 8000     // characters
	// macHeardWithin is how recently the Mac must have been heard from for a
	// message to say it is being sent, and not that it waits for the Mac.
	macHeardWithin = 2 * time.Minute
)

// writePart is a file of a write, read and kept.
type writePart struct {
	Kind string
	Text string
	Name string
	Type string
	Size int64
	SHA  string
	Path string
	// Created says this write put the bytes into the pool; false when they
	// were there already (a test run's removal never takes those).
	Created bool
}

func newGroupID() string {
	b := make([]byte, 8)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func cleanFileName(s string) string {
	s = strings.ToValidUTF8(s, "")
	s = strings.ReplaceAll(s, "\\", "/")
	s = path.Base(s)
	if s == "." || s == "/" {
		s = ""
	}
	return oneLine(s, 200)
}

// chatWrite is the page's own address: The owner writes a message, with files.
func (a *app) chatWrite(w http.ResponseWriter, r *http.Request) {
	id, ok := itemID(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	ctx := r.Context()
	extendRead(w, 5*time.Minute)
	r.Body = http.MaxBytesReader(w, r.Body, maxWriteFiles*maxWriteFile+1<<20)
	text, files, err := a.readWrite(r)
	if err != nil {
		var ue *userErr
		if errors.As(err, &ue) {
			http.Error(w, ue.Msg, ue.Status)
		} else {
			http.Error(w, "The message could not be read. Try again.", http.StatusBadRequest)
		}
		return
	}
	if err := a.queueWrite(ctx, id, text, files); err != nil {
		var ue *userErr
		switch {
		case errors.Is(err, errNoItem):
			http.NotFound(w, r)
		case errors.As(err, &ue):
			http.Error(w, ue.Msg, ue.Status)
		default:
			appkit.Error("queueing a message the owner wrote in a chat", err)
			http.Error(w, "That did not work. Try again.", http.StatusInternalServerError)
		}
		return
	}
	if r.Header.Get("X-Requested-With") != "one" {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	a.rowResponse(w, r, id, true, false)
}

// readWrite reads the text and the files of a write. The files are streamed
// into the pool, once by hash; none is held in memory as a whole. A sixth
// file, or one over the limit, refuses the whole write with a sentence.
func (a *app) readWrite(r *http.Request) (string, []writePart, error) {
	var text string
	var files []writePart
	ct, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if ct != "multipart/form-data" {
		if err := r.ParseForm(); err != nil {
			return "", nil, bad("The message was too large.")
		}
		return cleanWriteText(r.PostForm.Get("text")), nil, nil
	}
	mr, err := r.MultipartReader()
	if err != nil {
		return "", nil, bad("The message could not be read.")
	}
	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", nil, bad("The files were too large to take in one go.")
		}
		switch part.FormName() {
		case "text":
			b, _ := io.ReadAll(io.LimitReader(part, 64<<10))
			text = cleanWriteText(string(b))
		case "file":
			name := cleanFileName(part.FileName())
			if name == "" && part.FileName() == "" {
				part.Close()
				continue // an empty file field: nothing was chosen
			}
			if name == "" {
				name = "file"
			}
			if len(files) >= maxWriteFiles {
				return "", nil, bad(fmt.Sprintf("At most %d files can be sent at once.", maxWriteFiles))
			}
			sha, rel, size, created, err := a.imp.pool.write(io.LimitReader(part, maxWriteFile+1))
			if err != nil {
				return "", nil, bad("The file " + name + " could not be read to its end.")
			}
			if size > maxWriteFile {
				return "", nil, bad(name + " is larger than 20 MB.")
			}
			if size == 0 {
				return "", nil, bad(name + " is empty.")
			}
			ctype := "application/octet-stream"
			if m, _, err := mime.ParseMediaType(part.Header.Get("Content-Type")); err == nil && m != "" && len(m) <= 100 {
				ctype = strings.ToLower(m)
			}
			files = append(files, writePart{Kind: "file", Name: name, Type: ctype, Size: size, SHA: sha, Path: rel, Created: created})
		}
		part.Close()
	}
	return text, files, nil
}

func cleanWriteText(s string) string {
	s = strings.TrimSpace(strings.ToValidUTF8(s, ""))
	return strings.ReplaceAll(s, "\r\n", "\n")
}

// queueWrite stores the intent: the text and the files go in as parts, in
// order, in one transaction, before anything leaves the app.
func (a *app) queueWrite(ctx context.Context, itemID int64, text string, files []writePart) error {
	if text == "" && len(files) == 0 {
		return bad("Write something first.")
	}
	if utf8.RuneCountInString(text) > maxWriteText {
		return bad(fmt.Sprintf("A message can have at most %d characters.", maxWriteText))
	}
	now := a.now().Unix()
	group := newGroupID()
	return a.store.Update(ctx, func(tx *repo.Tx) error {
		it, found := tx.ItemByID(ctx, itemID)
		if !found || it.Hidden {
			return errNoItem
		}
		chat, ok := tx.ChatOfItem(ctx, it.ID)
		if !ok {
			return bad("Only a chat takes a message here. Mail is answered through a reply.")
		}
		var parts []repo.OutPart
		seq := 0
		if text != "" {
			parts = append(parts, repo.OutPart{ItemID: it.ID, Network: chat.Network, ChatID: chat.ChatID, Group: group, Seq: seq, Kind: "text", Text: text, CreatedAt: now})
			seq++
		}
		for _, f := range files {
			fid, err := tx.InsertOwnFile(ctx, repo.File{ItemID: it.ID, Source: "owner", Part: fmt.Sprintf("out-%s-%d", group, seq), Filename: f.Name,
				ContentType: f.Type, Size: f.Size, SHA: f.SHA, Path: f.Path, AddedAt: now})
			if err != nil {
				return err
			}
			if err := tx.PoolUsed(ctx, f.Path, f.Created, it.ID); err != nil {
				return err
			}
			parts = append(parts, repo.OutPart{ItemID: it.ID, Network: chat.Network, ChatID: chat.ChatID, Group: group, Seq: seq, Kind: "file", FileName: f.Name,
				FileType: f.Type, Size: f.Size, SHA: f.SHA, Path: f.Path, FileID: fid, CreatedAt: now})
			seq++
		}
		ids, err := tx.InsertOut(ctx, parts)
		if err != nil {
			return err
		}
		// The record holds who and when, and how many files; the text is in the
		// part itself, exactly as it was typed.
		return tx.InsertEvent(ctx, now, it.ID, "owner", "write_queued", map[string]any{"group": group, "parts": ids, "files": len(files)})
	})
}

// withdrawWrite is Withdraw on a write that waits for the Mac. Only the signed-in
// page does it; a part the Mac already took cannot be taken back.
func (a *app) withdrawWrite(w http.ResponseWriter, r *http.Request) {
	group := r.PathValue("group")
	if len(group) != 16 || strings.Trim(group, "0123456789abcdef") != "" {
		http.NotFound(w, r)
		return
	}
	ctx := r.Context()
	now := a.now().Unix()
	var itemID int64
	var withdrew bool
	err := a.store.Update(ctx, func(tx *repo.Tx) error {
		parts, err := tx.OutByGroup(ctx, group)
		if err != nil || len(parts) == 0 {
			if err == nil {
				err = errNoItem
			}
			return err
		}
		itemID = parts[0].ItemID
		withdrew, err = tx.WithdrawOut(ctx, group, now)
		if err != nil || !withdrew {
			return err
		}
		return tx.InsertEvent(ctx, now, itemID, "owner", "write_withdrawn", map[string]any{"group": group})
	})
	switch {
	case errors.Is(err, errNoItem):
		http.NotFound(w, r)
		return
	case err != nil:
		appkit.Error("withdrawing a message the owner wrote", err)
		http.Error(w, "That did not work. Try again.", http.StatusInternalServerError)
		return
	case !withdrew:
		http.Error(w, "The Mac has already taken this message, so it cannot be withdrawn.", http.StatusConflict)
		return
	}
	if r.Header.Get("X-Requested-With") != "one" {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	a.rowResponse(w, r, itemID, true, false)
}

// finishOut writes how one part ended. A part that went out takes an item In
// progress to Pending (the table for the owner's own message, DESIGN 3.5) and
// withdraws what Claude had proposed; one that did not go leaves the item with
// The owner, stops the parts after it, and sends one notice with nothing from the
// chat in it. It returns the state written.
func (a *app) finishOut(ctx context.Context, p repo.OutPart, state, reason, ref string) string {
	if state != "sent" && reason == "" {
		reason = "The Mac could not send it."
	}
	if state == "unknown" {
		reason = reason + " It may have gone: check the chat before you try again."
	}
	if p.Kind == "file" && state != "sent" {
		reason = p.FileName + ": " + reason
	}
	reason = oneLine(reason, 400)
	var it repo.Item
	wrote := false
	told := false // an earlier message of the same write already told the owner it did not go
	write := func() error {
		return a.store.Update(ctx, func(tx *repo.Tx) error {
			now := a.now().Unix()
			var ok bool
			it, ok = tx.ItemByID(ctx, p.ItemID)
			if !ok {
				return errNoSuchItem()
			}
			if state != "sent" {
				same, err := tx.OutByGroup(ctx, p.Group)
				if err != nil {
					return err
				}
				for _, q := range same {
					if q.ID != p.ID && (q.State == "failed" || q.State == "unknown") {
						told = true
					}
				}
			}
			done, err := tx.EndOut(ctx, p.ID, state, reason, ref, now)
			if err != nil || !done {
				return err
			}
			wrote = true
			if err := tx.InsertEvent(ctx, now, it.ID, "system", "write_"+state, map[string]any{"part": p.ID, "kind": p.Kind}); err != nil {
				return err
			}
			if state == "sent" {
				if it.Stage == stageInProgress {
					if err := tx.SetItemStage(ctx, it.ID, stagePending, now); err != nil {
						return err
					}
					if err := tx.InsertEvent(ctx, now, it.ID, "owner", "pending_set", map[string]any{"item": repo.Number(it.Number), "from": it.Stage, "to": stagePending}); err != nil {
						return err
					}
				}
				_, err := tx.WithdrawPending(ctx, it.ID, "you wrote in the chat", now)
				return err
			}
			// The parts after it in the same write do not go: what follows a
			// message that failed would arrive out of order.
			rest, err := tx.OutByGroup(ctx, p.Group)
			if err != nil {
				return err
			}
			for _, q := range rest {
				if q.Seq > p.Seq && q.State == "queued" {
					if _, err := tx.CancelOutPart(ctx, q.ID, "Not sent: the message before it did not go.", now); err != nil {
						return err
					}
				}
			}
			if it.Stage != stageInProgress && it.Stage != stageNew {
				return tx.SetItemStage(ctx, it.ID, stageInProgress, now)
			}
			return nil
		})
	}
	err := write()
	if err != nil {
		err = write()
	}
	if err != nil {
		appkit.Error("recording how a message the owner wrote ended", err)
		return state
	}
	if wrote {
		appkit.Logger().Info("chat write ended", "part", p.ID, "state", state)
		if state != "sent" && !told {
			a.noticeSendFailed(it)
		}
	}
	return state
}

// outInDoubt marks the parts the Mac took and never reported: they are not
// handed out again, and the chat says they may have gone.
func (a *app) outInDoubt(ctx context.Context) {
	ctx = context.WithoutCancel(ctx)
	before := a.now().Add(-a.bridgeSilence(ctx)).Unix()
	ps, err := a.store.OutHandedBefore(ctx, before)
	if err != nil {
		appkit.Error("looking for messages in doubt", err)
		return
	}
	for _, p := range ps {
		a.finishOut(ctx, p, "unknown", "The Mac took this message and did not say whether it went.", "")
	}
}

// --- what the page shows of a write ---

type outView struct {
	Text     string
	File     *fileView
	When     string
	WhenFull string
	ISO      string
	Status   string
	Failed   bool
	Group    string // set when Withdraw is offered
}

// outViews are the parts the owner wrote that have not yet come back as messages,
// oldest first. A part that came back is the message itself and is not shown
// twice; a withdrawn one is gone from the chat.
func (a *app) outViews(ctx context.Context, itemID int64, now time.Time) []outView {
	parts, err := a.store.OutOfItem(ctx, itemID)
	if err != nil {
		appkit.Error("loading what the owner wrote", err)
		return nil
	}
	heard := a.bridgeHeardAt(ctx)
	macUp := heard > 0 && now.Sub(time.Unix(heard, 0)) <= macHeardWithin
	var out []outView
	for _, p := range parts {
		if p.MsgID != 0 || p.State == "withdrawn" {
			continue
		}
		v := outView{Text: p.Text}
		v.When, v.WhenFull, v.ISO = a.whenText(p.CreatedAt, now)
		if p.Kind == "file" {
			v.File = &fileView{Name: p.FileName, Size: fileSize(p.Size), URL: "/files/" + strconv.FormatInt(p.FileID, 10)}
		}
		switch p.State {
		case "queued":
			if macUp {
				v.Status = "Sending…"
			} else {
				v.Status, v.Group = "Waiting for the Mac to send this.", p.Group
			}
		case "handed":
			v.Status = "Sending…"
		case "sent":
			v.Status = "Sent " + time.Unix(p.EndedAt, 0).In(a.loc).Format("15:04") + "."
		default: // failed, unknown
			v.Status, v.Failed = "Could not send: "+p.Error, true
		}
		out = append(out, v)
	}
	return out
}

// attView is one chip of the Attachments list of a row (SPEC 7).
type attView struct {
	Name string
	Size string
	URL  string
	By   string
	At   int64 // when it was sent or attached
	// The date as the thread shows it (whenText).
	When, WhenFull, ISO string
	Dated               string // "sent" for a file of a message, "attached" for one attached in the thread
}

// matchOutPart is called inside the import's transaction for a message of
// The owner's own that was not stored before. When it is something he wrote in ONE
// (by the id the Mac reported, else by the chat, the time and the text, or for
// a file by its name), the part points at it and the chat shows it once. The
// file the network sends back is the file ONE already holds.
func matchOutPart(ctx context.Context, tx *repo.Tx, itemID int64, m repo.Message, messageID int64, now int64) error {
	parts, err := tx.UnmatchedOut(ctx, itemID)
	if err != nil || len(parts) == 0 {
		return err
	}
	files, err := tx.FilesOfMessage(ctx, messageID)
	if err != nil {
		return err
	}
	for _, p := range parts {
		if m.ReceivedAt < p.CreatedAt-600 {
			continue
		}
		byRef := p.Ref != "" && strings.EqualFold(strings.TrimSpace(m.HeaderID), p.Ref)
		var file *repo.File
		if p.Kind == "file" {
			for i := range files {
				if strings.EqualFold(files[i].Filename, p.FileName) {
					file = &files[i]
					break
				}
			}
		}
		var hit bool
		if p.Kind == "file" {
			hit = file != nil && (byRef || m.Text == "" || normalText(m.Text) == normalText(p.FileName))
		} else {
			sent, got := normalText(p.Text), normalText(m.Text)
			hit = byRef || (sent != "" && got == sent && len(files) == 0)
		}
		if !hit {
			continue
		}
		if err := tx.MatchOut(ctx, p.ID, messageID); err != nil {
			return err
		}
		if file != nil && file.Status != "saved" {
			if err := tx.MarkFileSaved(ctx, file.ID, p.Size, p.SHA, p.Path); err != nil {
				return err
			}
		}
		if p.State == "unknown" {
			// It did go after all: the doubt is over.
			if _, err := tx.EndOut(ctx, p.ID, "sent", "", "", now); err != nil {
				return err
			}
			if err := tx.InsertEvent(ctx, now, itemID, "system", "write_sent", map[string]any{"part": p.ID, "kind": p.Kind, "confirmed": true}); err != nil {
				return err
			}
		}
		return tx.InsertEvent(ctx, now, itemID, "system", "write_matched", map[string]any{"part": p.ID, "message_id": messageID})
	}
	return nil
}
