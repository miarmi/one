package main

import (
	"net/http"
	"strconv"
	"strings"

	"lab/apps/one/repo"
	"lab/platform/appkit"
)

// Search (SPEC 26): one search over messages, comments and notes, newest
// first, with the person as a filter. It looks in every stage, closed and
// history items included, and in messages older than the thread window. The
// index is an FTS4 table kept by triggers (repo/search.go). A result text
// comes from other people: it is shown through the template, which escapes
// it; the marked words are split out by this code, never inserted as markup.

const (
	searchPage = 30
	markOpen   = "\x01"
	markClose  = "\x02"
)

var hitLabels = map[string]string{"mail": "Mail", "chat": "Chat", "event": "Event", "comment": "Comment", "note": "Note"}

type snippetPart struct {
	Text string
	Hit  bool
}

// snippetParts splits a snippet at the marks the index put around matching words.
func snippetParts(s string) []snippetPart {
	var out []snippetPart
	for len(s) > 0 {
		i := strings.Index(s, markOpen)
		if i < 0 {
			out = append(out, snippetPart{Text: stripMarks(s)})
			break
		}
		if i > 0 {
			out = append(out, snippetPart{Text: stripMarks(s[:i])})
		}
		s = s[i+len(markOpen):]
		j := strings.Index(s, markClose)
		if j < 0 {
			out = append(out, snippetPart{Text: stripMarks(s), Hit: true})
			break
		}
		out = append(out, snippetPart{Text: stripMarks(s[:j]), Hit: true})
		s = s[j+len(markClose):]
	}
	return out
}

func stripMarks(s string) string {
	return strings.NewReplacer(markOpen, "", markClose, "", "\r", " ", "\n", " ").Replace(s)
}

type hitView struct {
	Mark     string
	Link     string
	Title    string
	Subject  string
	Parts    []snippetPart
	Who      string
	Stage    string
	When     string
	WhenFull string
	ISO      string
}

type searchView struct {
	Query   string
	Person  string
	Done    bool // a search was made
	Hits    []hitView
	More    int
	Error   string
	Matched string // who the person filter found, in words
}

// searchHits runs a search for the owner's page or an agent. People are found
// by the text of the person filter; a filter that matches nobody gives no
// results rather than ignoring the filter.
func (a *app) searchHits(r *http.Request, q, person string, includeHidden bool, limit, offset int) ([]repo.Hit, bool, string, error) {
	ctx := r.Context()
	match := repo.MatchExpr(q)
	if match == "" {
		return nil, false, "", nil
	}
	sq := repo.SearchQuery{Match: match, IncludeHidden: includeHidden, Limit: limit + 1, Offset: offset}
	matched := ""
	if p := strings.TrimSpace(person); p != "" {
		people, err := a.store.FindPeople(ctx, p, 20)
		if err != nil {
			return nil, false, "", err
		}
		if len(people) == 0 {
			return nil, false, "nobody", nil
		}
		for _, pp := range people {
			sq.PersonIDs = append(sq.PersonIDs, pp.ID)
		}
		matched = strconv.Itoa(len(people))
	}
	hits, err := a.store.Search(ctx, sq)
	if err != nil {
		return nil, false, "", err
	}
	more := len(hits) > limit
	if more {
		hits = hits[:limit]
	}
	return hits, more, matched, nil
}

func (a *app) searchPageHandler(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	person := strings.TrimSpace(r.URL.Query().Get("person"))
	if len([]rune(q)) > 200 {
		q = string([]rune(q)[:200])
	}
	offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
	offset = max(offset, 0)
	v := searchView{Query: q, Person: person, Done: repo.MatchExpr(q) != ""}
	hits, more, matched, err := a.searchHits(r, q, person, false, searchPage, offset)
	if err != nil {
		appkit.Error("searching", err)
		v.Error = "The search did not work. Try again."
	}
	if matched == "nobody" {
		v.Matched = "Nobody on People matches that person."
	}
	if more {
		v.More = offset + searchPage
	}
	v.Hits = a.hitViews(r, hits)
	a.render(w, "search.html", http.StatusOK, pageData{EnvText: a.envText, Version: a.version, Search: v})
}

// hitViews makes the views of found rows, with the names the owner reads.
func (a *app) hitViews(r *http.Request, hits []repo.Hit) []hitView {
	now := a.now()
	var out []hitView
	for _, h := range hits {
		hv := hitView{Mark: hitLabels[h.Kind], Parts: snippetParts(h.Snippet), Title: oneLine(h.Title, 200), Subject: oneLine(h.Subject, 200), Stage: stageLabels[h.Stage]}
		hv.When, hv.WhenFull, hv.ISO = a.whenText(h.At, now)
		switch h.Kind {
		case "note":
			hv.Link = "/notes/" + strconv.FormatInt(h.Ref, 10)
			if hv.Title == "" {
				hv.Title = "Note"
			}
			hv.Who = actorLabel(h.Who)
		case "comment":
			hv.Link = "/items/" + strconv.FormatInt(h.ItemID, 10)
			hv.Who = actorLabel(h.Who)
		default:
			hv.Link = "/items/" + strconv.FormatInt(h.ItemID, 10)
			hv.Who = h.Who
		}
		out = append(out, hv)
	}
	a.dressHits(r, out, hits)
	return out
}

// dressHits shows the name the owner calls a sender instead of their address.
func (a *app) dressHits(r *http.Request, views []hitView, hits []repo.Hit) {
	var hs []repo.Handle
	for _, h := range hits {
		if h.Kind == "mail" {
			hs = append(hs, repo.Handle{Network: "mail", Value: strings.ToLower(h.Who)})
		}
	}
	people, err := a.store.PeopleByHandles(r.Context(), hs)
	if err != nil {
		return
	}
	for i, h := range hits {
		if p, ok := people[repo.Handle{Network: "mail", Value: strings.ToLower(h.Who)}]; ok && h.Kind == "mail" {
			views[i].Who = p.Shown
		}
	}
}
