package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"lab/apps/one/repo"
	"lab/platform/appkit"
)

// People (SPEC 25). Claude keeps people: every sender is added on first
// sight and Claude sets what the owner calls them and who they are to him (see
// peopleagent.go). The People page, reached from Settings, is an overview of
// names; a person opens when clicked and only then shows the form to correct
// a mistake. The owner himself can still switch Always show me on and off and
// correct any field: he is the owner, no proposal needed. Names and addresses come
// from mail and are shown as text only. A handle that already belongs to
// someone is never taken over silently.

const (
	maxPersonName  = 120
	maxCallName    = 60
	maxRelation    = 120
	maxHandles     = 30
	peoplePage     = 50
	maxPicture     = 300 << 10
	pictureRefusal = "Use a PNG, JPEG or WebP picture under 300 KB."
)

// knownNetworks are the networks a handle can be on.
var knownNetworks = map[string]bool{"mail": true, "whatsapp": true, "messenger": true, "instagram": true, "telegram": true, "signal": true, "imessage": true, "sms": true}

// parseHandles reads the field "Addresses and handles": one per line, a mail
// address on its own or "network: value".
func parseHandles(text string) ([]repo.Handle, error) { return parseHandlesKeeping(text, nil) }

// phoneLike says a value is a phone number written with the usual spaces,
// dashes and brackets: only then may they be taken out.
func phoneLike(v string) bool {
	digits := 0
	for _, r := range v {
		switch {
		case r >= '0' && r <= '9':
			digits++
		case strings.ContainsRune("+ -()./", r):
		default:
			return false
		}
	}
	return digits >= 5
}

// parseHandlesKeeping is parseHandles for a save: a line that is exactly the
// line of a handle the person already has keeps that handle as it is stored,
// so saving never changes a handle the owner did not edit (SPEC 25).
func parseHandlesKeeping(text string, keep []repo.Handle) ([]repo.Handle, error) {
	var out []repo.Handle
	seen := map[repo.Handle]bool{}
	kept := map[string]repo.Handle{}
	for _, h := range keep {
		kept[handlesText([]repo.Handle{h})] = h
	}
	add := func(h repo.Handle) {
		if !seen[h] {
			seen[h] = true
			out = append(out, h)
		}
	}
	text = strings.ReplaceAll(text, "\r", "\n")
	for _, raw := range strings.Split(text, "\n") {
		if h, ok := kept[strings.TrimSpace(raw)]; ok && strings.TrimSpace(raw) != "" {
			add(h)
			continue
		}
		for _, line := range strings.Split(raw, ",") {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			if h, ok := kept[line]; ok {
				add(h)
				continue
			}
			network, value := "", line
			if i := strings.Index(line, ":"); i > 0 {
				if n := strings.ToLower(strings.TrimSpace(line[:i])); knownNetworks[n] {
					network, value = n, strings.TrimSpace(line[i+1:])
				}
			}
			if network == "" {
				if !strings.Contains(line, "@") {
					return nil, fmt.Errorf(`Write each handle as "network: value", for example "whatsapp: +420600000000"; a mail address needs no network. Networks: mail, whatsapp, messenger, instagram, telegram, signal, imessage, sms.`)
				}
				network = "mail"
			}
			if value == "" || utf8.RuneCountInString(value) > 200 || strings.ContainsFunc(value, unicode.IsControl) {
				return nil, fmt.Errorf("%q is not a handle I can keep.", oneLine(line, 60))
			}
			if network == "mail" && (!strings.Contains(value, "@") || strings.ContainsAny(value, " \t<>")) {
				return nil, fmt.Errorf("%q is not a mail address.", oneLine(value, 60))
			}
			if (network == "whatsapp" || network == "sms") && phoneLike(value) {
				value = strings.NewReplacer(" ", "", "-", "", "(", "", ")", "").Replace(value)
			}
			add(repo.NormalHandle(network, value))
		}
	}
	if len(out) > maxHandles {
		return nil, fmt.Errorf("A person can have at most %d handles.", maxHandles)
	}
	return out, nil
}

// handlesText is the field's content for the handles a person has.
func handlesText(hs []repo.Handle) string {
	lines := make([]string, len(hs))
	for i, h := range hs {
		if h.Network == "mail" {
			lines[i] = h.Value
		} else {
			lines[i] = h.Network + ": " + h.Value
		}
	}
	return strings.Join(lines, "\n")
}

type personView struct {
	ID        int64
	Name      string
	Shown     string // what the owner calls them, else their name
	CallName  string
	Relation  string
	Handles   string
	Protected bool
	Picture   string // address of the owner's picture, empty when none
	Badge     string
	Color     string
}

type peopleView struct {
	Notice    string
	Error     string
	Filter    string
	Total     int
	People    []personView
	Open      *personView // the person that is opened, nil on the overview
	Proposals personProposalList
	More      int
	Prev      int
	HasPrev   bool
	From, To  int // the numbers of the first and last person on this page
}

func (a *app) personViewOf(p repo.Person) personView {
	v := personView{ID: p.ID, Name: p.Name, Shown: p.Shown(), CallName: p.CallName, Relation: p.Relation, Handles: handlesText(p.Handles), Protected: p.Protected,
		Badge: badge(p.Shown()), Color: colorOf("person" + strconv.FormatInt(p.ID, 10))}
	if p.HasPicture {
		v.Picture = "/people/" + strconv.FormatInt(p.ID, 10) + "/picture"
	}
	return v
}

// peopleViewFor is the overview: one page of names, never the forms.
func (a *app) peopleViewFor(ctx context.Context, filter string, offset int) (peopleView, error) {
	v := peopleView{Filter: filter, Total: a.store.CountPeople(ctx, filter)}
	list, err := a.store.ListPeopleNames(ctx, filter, peoplePage+1, offset)
	if err != nil {
		return v, err
	}
	if len(list) > peoplePage {
		list, v.More = list[:peoplePage], offset+peoplePage
	}
	for _, p := range list {
		v.People = append(v.People, a.personViewOf(p))
	}
	if len(list) > 0 {
		v.From, v.To = offset+1, offset+len(list)
	}
	if offset > 0 {
		v.HasPrev, v.Prev = true, max(offset-peoplePage, 0)
	}
	return v, nil
}

// renderPeople shows the overview, or with open set that one person's page.
// form is what the owner typed when a save was refused.
func (a *app) renderPeople(w http.ResponseWriter, r *http.Request, code int, notice, errText string, open int64, form *personView) {
	var v peopleView
	if open == 0 {
		offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
		var err error
		v, err = a.peopleViewFor(r.Context(), strings.TrimSpace(r.URL.Query().Get("q")), max(offset, 0))
		if err != nil {
			appkit.Error("building the people page", err)
			http.Error(w, "The page could not be loaded. Reload it to try again.", http.StatusInternalServerError)
			return
		}
		v.Proposals = personProposalList{List: a.personProposalViews(r.Context()), Back: "people"}
	} else {
		p, ok := a.store.PersonByID(r.Context(), open)
		if !ok {
			http.NotFound(w, r)
			return
		}
		pv := a.personViewOf(p)
		if form != nil {
			pv.Name, pv.CallName, pv.Relation, pv.Handles, pv.Protected = form.Name, form.CallName, form.Relation, form.Handles, form.Protected
		}
		v.Open = &pv
	}
	switch notice {
	case "saved":
		v.Notice = "Saved."
	case "picture":
		v.Notice = "The picture is set."
	case "nopicture":
		v.Notice = "The picture is removed."
	case "proposal":
		v.Notice = "You approved this proposal."
	case "dismissed":
		v.Notice = "You dismissed this proposal."
	case "withdrawn":
		v.Notice = "This proposal no longer fits and was withdrawn."
	}
	v.Error = errText
	a.render(w, "people.html", code, pageData{EnvText: a.envText, Version: a.version, People: v})
}

func (a *app) peoplePage(w http.ResponseWriter, r *http.Request) {
	a.renderPeople(w, r, http.StatusOK, r.URL.Query().Get("n"), "", 0, nil)
}

// personPage opens one person: the form to correct a mistake.
func (a *app) personPage(w http.ResponseWriter, r *http.Request) {
	id, ok := personID(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	a.renderPeople(w, r, http.StatusOK, r.URL.Query().Get("n"), "", id, nil)
}

// personForm reads and checks the fields of one person.
func personForm(r *http.Request, keep []repo.Handle) (repo.Person, []repo.Handle, error) {
	field := func(name string, max int) string {
		return strings.TrimSpace(strings.ToValidUTF8(strings.Join(strings.Fields(r.FormValue(name)), " "), ""))
	}
	p := repo.Person{Name: field("name", maxPersonName), CallName: field("call_name", maxCallName), Relation: field("relation", maxRelation), Protected: r.FormValue("protected") == "1"}
	switch {
	case p.Name == "":
		return p, nil, errors.New("Write a name first.")
	case utf8.RuneCountInString(p.Name) > maxPersonName:
		return p, nil, fmt.Errorf("A name can have at most %d characters.", maxPersonName)
	case utf8.RuneCountInString(p.CallName) > maxCallName:
		return p, nil, fmt.Errorf("What you call them can have at most %d characters.", maxCallName)
	case utf8.RuneCountInString(p.Relation) > maxRelation:
		return p, nil, fmt.Errorf("Who they are to you can have at most %d characters.", maxRelation)
	}
	hs, err := parseHandlesKeeping(r.FormValue("handles"), keep)
	return p, hs, err
}

// savePerson writes what the owner corrected on a person and the handles, in one
// transaction.
func (a *app) savePerson(ctx context.Context, id int64, p repo.Person, hs []repo.Handle) ([]string, error) {
	now := a.now().Unix()
	var changed []string
	err := a.store.Update(ctx, func(tx *repo.Tx) error {
		old, ok := tx.PersonByID(ctx, id)
		if !ok {
			return errNoItem
		}
		p.ID = id
		if err := tx.UpdatePerson(ctx, p, "owner", now); err != nil {
			return err
		}
		for _, c := range []struct {
			field string
			diff  bool
		}{{"name", old.Name != p.Name}, {"call_name", old.CallName != p.CallName}, {"relation", old.Relation != p.Relation}, {"protected", old.Protected != p.Protected}} {
			if c.diff {
				changed = append(changed, c.field)
			}
		}
		if err := tx.SetHandles(ctx, id, hs); err != nil {
			return err
		}
		if len(changed) == 0 {
			if cur, ok := tx.PersonByID(ctx, id); ok && handlesText(cur.Handles) != handlesText(old.Handles) {
				changed = append(changed, "handles")
			}
		}
		if len(changed) == 0 {
			return nil
		}
		return tx.InsertEventFor(ctx, "people", id, now, "owner", "setting_changed", map[string]any{"person": id, "fields": changed, "protected": p.Protected})
	})
	return changed, err
}

func (a *app) peopleError(w http.ResponseWriter, r *http.Request, err error, id int64, form *personView) {
	var taken *repo.ErrHandleTaken
	if errors.As(err, &taken) {
		a.renderPeople(w, r, http.StatusConflict, "", fmt.Sprintf("%s already belongs to %s. Take it away from them first, then add it here.", taken.Handle.Value, oneLine(taken.Owner, 80)), id, form)
		return
	}
	if errors.Is(err, errNoItem) {
		http.NotFound(w, r)
		return
	}
	appkit.Error("saving a person", err)
	http.Error(w, "That did not work. Try again.", http.StatusInternalServerError)
}

func personID(r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	return id, err == nil && id > 0
}

func (a *app) savePersonHandler(w http.ResponseWriter, r *http.Request) {
	id, ok := personID(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 32<<10)
	var keep []repo.Handle
	if cur, found := a.store.PersonByID(r.Context(), id); found {
		keep = cur.Handles
	}
	p, hs, err := personForm(r, keep)
	form := &personView{Name: p.Name, CallName: p.CallName, Relation: p.Relation, Handles: r.FormValue("handles"), Protected: p.Protected}
	if err != nil {
		a.renderPeople(w, r, http.StatusBadRequest, "", err.Error(), id, form)
		return
	}
	if _, err = a.savePerson(r.Context(), id, p, hs); err != nil {
		a.peopleError(w, r, err, id, form)
		return
	}
	http.Redirect(w, r, "/people/"+strconv.FormatInt(id, 10)+"?n=saved", http.StatusSeeOther)
}

// --- pictures ---

// pictureType says what an upload is by its first bytes, never by what its
// name or header claims. Only PNG, JPEG and WebP are kept.
func pictureType(b []byte) (string, bool) {
	ct := http.DetectContentType(b)
	switch ct {
	case "image/png", "image/jpeg", "image/webp":
		return ct, true
	}
	return "", false
}

func (a *app) setPicture(w http.ResponseWriter, r *http.Request) {
	id, ok := personID(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxPicture+64<<10)
	if err := r.ParseMultipartForm(maxPicture); err != nil {
		a.renderPeople(w, r, http.StatusBadRequest, "", pictureRefusal, id, nil)
		return
	}
	f, _, err := r.FormFile("picture")
	if err != nil {
		a.renderPeople(w, r, http.StatusBadRequest, "", pictureRefusal, id, nil)
		return
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxPicture+1))
	ct, good := pictureType(data)
	if err != nil || len(data) == 0 || len(data) > maxPicture || !good {
		a.renderPeople(w, r, http.StatusBadRequest, "", pictureRefusal, id, nil)
		return
	}
	ctx := r.Context()
	now := a.now().Unix()
	err = a.store.Update(ctx, func(tx *repo.Tx) error {
		if _, found := tx.PersonByID(ctx, id); !found {
			return errNoItem
		}
		if err := tx.SetPicture(ctx, id, ct, data, now); err != nil {
			return err
		}
		return tx.InsertEventFor(ctx, "people", id, now, "owner", "setting_changed", map[string]any{"person": id, "fields": []string{"picture"}})
	})
	if err != nil {
		a.peopleError(w, r, err, id, nil)
		return
	}
	http.Redirect(w, r, "/people/"+strconv.FormatInt(id, 10)+"?n=picture", http.StatusSeeOther)
}

func (a *app) removePicture(w http.ResponseWriter, r *http.Request) {
	id, ok := personID(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	ctx := r.Context()
	now := a.now().Unix()
	err := a.store.Update(ctx, func(tx *repo.Tx) error {
		if _, found := tx.PersonByID(ctx, id); !found {
			return errNoItem
		}
		if err := tx.RemovePicture(ctx, id); err != nil {
			return err
		}
		return tx.InsertEventFor(ctx, "people", id, now, "owner", "setting_changed", map[string]any{"person": id, "fields": []string{"picture removed"}})
	})
	if err != nil {
		a.peopleError(w, r, err, id, nil)
		return
	}
	http.Redirect(w, r, "/people/"+strconv.FormatInt(id, 10)+"?n=nopicture", http.StatusSeeOther)
}

func (a *app) servePicture(w http.ResponseWriter, r *http.Request) {
	id, ok := personID(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	ct, data, at, found := a.store.Picture(r.Context(), id)
	if !found {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", ct)
	w.Header().Set("Cache-Control", "private, no-cache")
	w.Header().Set("Content-Disposition", "inline")
	http.ServeContent(w, r, "", time.Unix(at, 0), bytes.NewReader(data))
}

// --- people in rows and messages ---

// dress gives each row the name the owner calls the person who wrote its newest
// message, and that person's colour and picture.
func (a *app) dress(ctx context.Context, views []rowView, rows []repo.ListRow) {
	defer a.dressMeeting(ctx, views, rows) // SPEC 33: a meeting thread with no message is named by its calendar
	for i, r := range rows {
		if r.Kind == "chat" {
			views[i].Pulse = a.store.ItemPulse(ctx, r.ID)
		}
	}
	var hs []repo.Handle
	for _, r := range rows {
		if r.Sender != "" {
			hs = append(hs, repo.Handle{Network: networkOfSource(r.Source), Value: strings.ToLower(r.Sender)})
		}
	}
	people, err := a.store.PeopleByHandles(ctx, hs)
	if err != nil || len(people) == 0 {
		return
	}
	for i, r := range rows {
		if p, ok := people[repo.Handle{Network: networkOfSource(r.Source), Value: strings.ToLower(r.Sender)}]; ok {
			views[i].Who = oneLine(p.Shown, 80)
			views[i].Color = colorOf("person" + strconv.FormatInt(p.ID, 10))
		}
	}
}

// networkOfSource is the network a person's handle is on for messages that came
// from a source: mail for both mailboxes, the chat network itself otherwise.
func networkOfSource(src string) string {
	if src == srcGmail || src == srcGraph || src == "" {
		return "mail"
	}
	return src
}
