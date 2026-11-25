package main

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"lab/apps/one/repo"
	"lab/platform/appkit"
)

// Search while typing (SPEC 26). The field in the top line asks this address
// after each pause in typing and shows the answer under the field. It is a
// page of the signed-in app (GET /search/live), not part of /api/: agents
// have their own search there. The answer is small and bounded: at most
// liveGroupMax rows in each group, and nothing for fewer than liveMinChars
// letters. Text from mail travels as JSON strings split into plain and
// marked parts; the script puts them in with textContent, so no markup is
// ever made from it.

const (
	liveGroupMax = 4
	liveMinChars = 2
)

// liveKinds are the groups after People, in the order they are shown.
var liveKinds = []string{"mail", "chat", "event", "comment", "note"}

type livePart struct {
	T string `json:"t"`
	H bool   `json:"h,omitempty"`
}

type liveRow struct {
	Link    string     `json:"link"`
	Title   []livePart `json:"title"`
	Snippet []livePart `json:"snippet,omitempty"`
	Who     string     `json:"who,omitempty"`
	When    string     `json:"when,omitempty"`
	ISO     string     `json:"iso,omitempty"`
}

type liveGroup struct {
	Kind  string    `json:"kind"`
	Label string    `json:"label"`
	Hits  []liveRow `json:"hits"`
}

type liveAnswer struct {
	Query  string      `json:"q"`
	Groups []liveGroup `json:"groups"`
}

func parts(in []snippetPart) []livePart {
	out := make([]livePart, 0, len(in))
	for _, p := range in {
		out = append(out, livePart{T: p.Text, H: p.Hit})
	}
	return out
}

// markWords splits a text at the words the person typed: a word of the text
// that starts with one of them, without regard to case or accents, is marked
// as a whole (the index marks whole words in a snippet too).
func markWords(text string, words []string) []livePart {
	rs := []rune(text)
	folded := []rune(repo.Fold(text))
	if len(rs) != len(folded) {
		return []livePart{{T: text}}
	}
	var fw [][]rune
	for _, w := range words {
		if w != "" {
			fw = append(fw, []rune(repo.Fold(w)))
		}
	}
	isWord := func(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) }
	var out []livePart
	start := 0 // start of the plain run
	for i := 0; i < len(rs); {
		if !isWord(rs[i]) {
			i++
			continue
		}
		j := i
		for j < len(rs) && isWord(rs[j]) {
			j++
		}
		hit := false
		for _, w := range fw {
			if j-i >= len(w) && string(folded[i:i+len(w)]) == string(w) {
				hit = true
				break
			}
		}
		if hit {
			if i > start {
				out = append(out, livePart{T: string(rs[start:i])})
			}
			out = append(out, livePart{T: string(rs[i:j]), H: true})
			start = j
		}
		i = j
	}
	if start < len(rs) {
		out = append(out, livePart{T: string(rs[start:])})
	}
	if len(out) == 0 {
		out = []livePart{{T: ""}}
	}
	return out
}

func queryWords(q string) []string {
	return strings.FieldsFunc(q, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) })
}

func (a *app) searchLive(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if utf8.RuneCountInString(q) > 100 {
		q = string([]rune(q)[:100])
	}
	ans := liveAnswer{Query: q, Groups: []liveGroup{}}
	if utf8.RuneCountInString(q) >= liveMinChars {
		ctx := r.Context()
		words := queryWords(q)
		if people, err := a.store.SearchPeople(ctx, q, liveGroupMax); err != nil {
			if ctx.Err() == nil {
				appkit.Error("searching people while typing", err)
			}
		} else if len(people) > 0 {
			g := liveGroup{Kind: "people", Label: "People"}
			for _, p := range people {
				row := liveRow{Link: "/people/" + strconv.FormatInt(p.ID, 10), Title: markWords(p.Shown(), words)}
				if p.Relation != "" {
					row.Who = oneLine(p.Relation, 80)
				}
				g.Hits = append(g.Hits, row)
			}
			ans.Groups = append(ans.Groups, g)
		}
		if match := repo.MatchExpr(q); match != "" {
			for _, kind := range liveKinds {
				hits, err := a.store.Search(ctx, repo.SearchQuery{Match: match, Kind: kind, Limit: liveGroupMax})
				if err != nil {
					if ctx.Err() != nil {
						return // a newer keystroke cancelled this one
					}
					appkit.Error("searching while typing", err)
					continue
				}
				if len(hits) == 0 {
					continue
				}
				g := liveGroup{Kind: kind, Label: hitLabels[kind]}
				for _, hv := range a.hitViews(r, hits) {
					title := hv.Title
					if title == "" {
						title = hv.Subject
					}
					g.Hits = append(g.Hits, liveRow{Link: hv.Link, Title: markWords(title, words), Snippet: parts(hv.Parts), Who: hv.Who, When: hv.When, ISO: hv.ISO})
				}
				ans.Groups = append(ans.Groups, g)
			}
		}
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(ans)
}
