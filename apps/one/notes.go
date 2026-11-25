package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"unicode/utf8"

	"lab/apps/one/repo"
	"lab/platform/appkit"
)

// Notes (SPEC 26): what the owner has explained once. The owner writes and changes
// them on the Notes page; agents save them through the connector. A note
// stands alone or belongs to a person or an item. Every save is also an
// event, so earlier versions are kept.

const (
	maxNoteTitle = 200
	maxNoteBody  = 8000
	notesPage    = 50
)

type noteView struct {
	ID       int64
	Title    string
	Body     string
	Preview  string
	Person   string
	Item     string
	ItemID   int64
	By       string
	When     string
	WhenFull string
	ISO      string
	// the form
	ItemField   string
	PersonField string
	SenderField string
	Sender      string
}

type notesView struct {
	Notice  string
	Error   string
	Notes   []noteView
	More    int
	Note    noteView // the one in the form
	Editing bool
	// Choices offered beside the fields "About an item" and "About a person"
	// (the fields stay free text): item numbers with their subject, names as on People.
	ItemChoices   []noteChoice
	PersonChoices []string
}

type noteChoice struct{ Value, Label string }

// noteChoicesFor fills the choices of the note form.
func (a *app) noteChoicesFor(ctx context.Context, v *notesView) {
	seen := map[int64]bool{}
	for _, stage := range []string{stageNew, stageInProgress, stagePending, stageResolved} {
		rows, err := a.store.ListItems(ctx, stage, 40, 0)
		if err != nil {
			continue
		}
		for _, r := range rows {
			if seen[r.ID] {
				continue
			}
			seen[r.ID] = true
			v.ItemChoices = append(v.ItemChoices, noteChoice{Value: repo.Number(r.Number), Label: snippet(r.Subject, 60)})
		}
	}
	if people, err := a.store.ListPeopleNames(ctx, "", 300, 0); err == nil {
		for _, p := range people {
			v.PersonChoices = append(v.PersonChoices, p.Shown())
		}
	}
}

func (a *app) noteViewOf(n repo.Note) noteView {
	short, full, iso := a.whenText(n.UpdatedAt, a.now())
	v := noteView{ID: n.ID, Title: n.Title, Body: n.Body, Preview: snippet(n.Body, 220), Person: n.PersonName, By: actorLabel(n.UpdatedBy),
		When: short, WhenFull: full, ISO: iso, ItemID: n.ItemID}
	if n.ItemNumber > 0 {
		v.Item = repo.Number(n.ItemNumber)
		v.ItemField = v.Item
	}
	v.PersonField = n.PersonName
	v.SenderField, v.Sender = n.Sender, n.Sender
	return v
}

// noteInput is a note as typed, before it is checked.
type noteInput struct {
	Title, Body, Item, Person string
	Sender                    string // a sender address or domain
}

// resolveNote checks a note and finds what it belongs to.
func (a *app) resolveNote(ctx context.Context, q repo.Q, in noteInput) (repo.Note, error) {
	var n repo.Note
	title, err := checkField("The title", in.Title, maxNoteTitle, true)
	if err != nil {
		return n, err
	}
	body, err := checkField("The note", in.Body, maxNoteBody, true)
	if err != nil {
		return n, err
	}
	n.Title, n.Body = oneLine(title, maxNoteTitle), body
	if s := strings.ToLower(strings.TrimSpace(in.Sender)); s != "" {
		s = strings.TrimPrefix(s, "@")
		at := strings.Count(s, "@")
		switch {
		case len(s) > 200 || strings.ContainsAny(s, " \t\r\n<>,;\"") || at > 1:
			return n, bad("The sender must be one address like name@example.com, or a domain like example.com.")
		case at == 1:
			local, dom, _ := strings.Cut(s, "@")
			if local == "" || !strings.Contains(dom, ".") {
				return n, bad("The sender must be one address like name@example.com, or a domain like example.com.")
			}
		case !strings.Contains(s, "."):
			return n, bad("The sender must be one address like name@example.com, or a domain like example.com.")
		}
		n.Sender = s
	}
	if ref := strings.TrimSpace(in.Item); ref != "" {
		num, ok := repo.ParseNumber(ref)
		if !ok {
			return n, bad("The item must be its number, like Q-0012.")
		}
		it, found := q.ItemByNumber(ctx, num)
		if !found {
			return n, bad(fmt.Sprintf("There is no item %s.", repo.Number(num)))
		}
		n.ItemID = it.ID
	}
	if name := strings.TrimSpace(in.Person); name != "" {
		found, err := q.FindPeople(ctx, name, 8)
		if err != nil {
			return n, err
		}
		var exact []repo.Person
		for _, p := range found {
			if strings.EqualFold(p.Name, name) || strings.EqualFold(p.Shown(), name) {
				exact = append(exact, p)
			}
		}
		switch {
		case len(exact) == 1:
			n.PersonID = exact[0].ID
		case len(exact) == 0 && len(found) == 1:
			n.PersonID = found[0].ID
		case len(found) == 0:
			return n, bad(fmt.Sprintf("There is nobody called %q on People.", oneLine(name, 60)))
		default:
			return n, bad(fmt.Sprintf("More than one person matches %q. Write the name in full.", oneLine(name, 60)))
		}
	}
	return n, nil
}

// saveNote is the one function behind the Notes page and the connector: it
// makes a note, or changes the one with the given id, and writes the event.
// An agent may change only a note an agent made.
func (a *app) saveNote(ctx context.Context, x actor, id int64, in noteInput) (repo.Note, bool, error) {
	var saved repo.Note
	created := id == 0
	now := a.now().Unix()
	err := a.store.Update(ctx, func(tx *repo.Tx) error {
		n, err := a.resolveNote(ctx, tx.Q, in)
		if err != nil {
			return err
		}
		if created {
			n.CreatedBy = x.id()
			if n.ID, err = tx.InsertNote(ctx, n, now); err != nil {
				return err
			}
		} else {
			old, ok := tx.NoteByID(ctx, id)
			if !ok {
				return &userErr{http.StatusNotFound, "no such note"}
			}
			if !x.owner && old.CreatedBy == "owner" {
				return &userErr{http.StatusForbidden, "That note is the owner's. Only the owner can change it."}
			}
			n.ID = id
			if err := tx.UpdateNote(ctx, n, x.id(), now); err != nil {
				return err
			}
		}
		saved = n
		data := map[string]any{"note": n.ID, "title": n.Title, "text": n.Body, "created": created}
		if n.ItemID == 0 {
			return tx.InsertEventFor(ctx, "notes", n.ID, now, x.id(), "note_saved", data)
		}
		return tx.InsertEvent(ctx, now, n.ItemID, x.id(), "note_saved", data)
	})
	return saved, created, err
}

func (a *app) notesPageHandler(w http.ResponseWriter, r *http.Request) {
	offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
	offset = max(offset, 0)
	list, err := a.store.ListNotes(r.Context(), notesPage+1, offset)
	if err != nil {
		appkit.Error("listing notes", err)
		http.Error(w, "The page could not be loaded. Reload it to try again.", http.StatusInternalServerError)
		return
	}
	v := notesView{}
	if len(list) > notesPage {
		list, v.More = list[:notesPage], offset+notesPage
	}
	for _, n := range list {
		v.Notes = append(v.Notes, a.noteViewOf(n))
	}
	if r.URL.Query().Get("n") == "saved" {
		v.Notice = "Saved."
	}
	a.render(w, "notes.html", http.StatusOK, pageData{EnvText: a.envText, Version: a.version, Notes: v})
}

func (a *app) renderNoteForm(w http.ResponseWriter, code int, v notesView) {
	a.noteChoicesFor(context.Background(), &v)
	a.render(w, "note.html", code, pageData{EnvText: a.envText, Version: a.version, Notes: v})
}

func (a *app) newNoteForm(w http.ResponseWriter, r *http.Request) {
	a.renderNoteForm(w, http.StatusOK, notesView{})
}

func (a *app) editNoteForm(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	n, ok := a.store.NoteByID(r.Context(), id)
	if err != nil || !ok {
		http.NotFound(w, r)
		return
	}
	a.renderNoteForm(w, http.StatusOK, notesView{Note: a.noteViewOf(n), Editing: true})
}

func (a *app) postNote(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	var id int64
	editing := false
	if s := r.PathValue("id"); s != "" {
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil || n <= 0 {
			http.NotFound(w, r)
			return
		}
		id, editing = n, true
	}
	in := noteInput{Title: r.FormValue("title"), Body: r.FormValue("body"), Item: r.FormValue("item"), Person: r.FormValue("person"), Sender: r.FormValue("sender")}
	_, _, err := a.saveNote(r.Context(), ownerActor(), id, in)
	if err != nil {
		var ue *userErr
		msg := "The note could not be saved. Try again."
		code := http.StatusInternalServerError
		if errors.As(err, &ue) {
			msg, code = ue.Msg, ue.Status
			if code == http.StatusNotFound {
				http.NotFound(w, r)
				return
			}
		} else {
			appkit.Error("saving a note", err)
		}
		a.renderNoteForm(w, code, notesView{Error: msg, Editing: editing, Note: noteView{ID: id, Title: in.Title, Body: in.Body, ItemField: in.Item, PersonField: in.Person, SenderField: in.Sender}})
		return
	}
	http.Redirect(w, r, "/notes?n=saved", http.StatusSeeOther)
}

var _ = utf8.RuneCountInString
