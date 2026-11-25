package main

import (
	"context"
	"fmt"
	"net/http"
	"regexp"
	"regexp/syntax"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"lab/apps/one/repo"
	"lab/platform/appkit"
)

// Matter patterns (SPEC 10). The owner sets, in Settings, patterns for the
// reference of a matter (an order, parcel, invoice or file number). Mail whose
// subject or text carries the same reference for the same pattern joins one
// row, even when it arrives as separate mails. The app ships with no pattern.
// Similar sender or subject alone never joins rows.

const (
	keyMatterPatterns  = "matter_patterns"
	maxMatterPatterns  = 20
	maxMatterName      = 60
	maxMatterRegex     = 200
	matterLinkKind     = "matter"
	matterTextScanSize = 4000
	matterSubjectScan  = 500 // bytes of a subject that are searched
	maxMatterRepeat    = 50  // the largest repeat count a pattern may ask for, as in x{50}
	matterCountRows    = 3000
	matterCountDays    = 90
	matterCountBudget  = 2 * time.Second
)

type matterPattern struct {
	Name string
	Re   string
}

// matterPatterns reads the patterns the owner set: one per line, name and
// expression separated by a tab.
func (a *app) matterPatterns(ctx context.Context, q repo.Q) []matterPattern {
	var out []matterPattern
	for _, line := range strings.Split(q.Setting(ctx, keyMatterPatterns), "\n") {
		name, re, ok := strings.Cut(line, "\t")
		if !ok || strings.TrimSpace(re) == "" {
			continue
		}
		out = append(out, matterPattern{Name: name, Re: re})
	}
	return out
}

// matterPatternsFor are the patterns that apply to what is imported under ctx:
// The owner's, and under a test run also the patterns the owner approved from that
// run's own proposals (they live with the run's marked records and go with it).
func (a *app) matterPatternsFor(ctx context.Context, q repo.Q) []matterPattern {
	out := a.matterPatterns(ctx, q)
	ps, _ := q.ApprovedRunPatterns(ctx)
	for _, p := range ps {
		if !slices.ContainsFunc(out, func(m matterPattern) bool { return m.Re == p.Pattern }) {
			out = append(out, matterPattern{Name: p.Name, Re: p.Pattern})
		}
	}
	return out
}

func encodeMatterPatterns(ps []matterPattern) string {
	var b strings.Builder
	for _, p := range ps {
		b.WriteString(p.Name + "\t" + p.Re + "\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

// matterRegexes keeps every pattern compiled once, so that a pattern is not
// compiled for every message that arrives. A pattern that does not compile is
// kept as nil. The cache is small and starts again when it is full.
// matterCompiles counts real compilations (for the tests).
var matterCompiles atomic.Int64

var matterRegexes = struct {
	sync.Mutex
	m map[string]*regexp.Regexp
}{m: map[string]*regexp.Regexp{}}

func compiledMatter(re string) *regexp.Regexp {
	if len(re) > maxMatterRegex {
		return nil
	}
	matterRegexes.Lock()
	defer matterRegexes.Unlock()
	if c, ok := matterRegexes.m[re]; ok {
		return c
	}
	if len(matterRegexes.m) >= 200 {
		matterRegexes.m = map[string]*regexp.Regexp{}
	}
	matterCompiles.Add(1)
	c, err := regexp.Compile("(?i)" + re)
	if err != nil {
		c = nil
	}
	matterRegexes.m[re] = c
	return c
}

// cutBytes shortens s to at most n bytes without breaking a character.
func cutBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return strings.ToValidUTF8(s[:n], "")
}

// matterKey is the matter a mail is about: the first pattern that finds a
// reference in the subject, or else in the beginning of the text. The
// reference is the pattern's first group, or the whole match. The key is the
// same for the same pattern and the same reference, whatever the case. The
// patterns are compiled once, the pattern length and the searched text are
// bounded; it is called before a write transaction is opened.
func matterKey(ps []matterPattern, subject, text string) string {
	text, subject = cutBytes(text, matterTextScanSize), cutBytes(subject, matterSubjectScan)
	if len(ps) > maxMatterPatterns {
		ps = ps[:maxMatterPatterns]
	}
	for _, p := range ps {
		re := compiledMatter(p.Re)
		if re == nil {
			continue
		}
		for _, hay := range []string{subject, text} {
			m := re.FindStringSubmatch(hay)
			if m == nil {
				continue
			}
			ref := m[0]
			if len(m) > 1 && m[1] != "" {
				ref = m[1]
			}
			ref = strings.ToLower(strings.TrimSpace(ref))
			if ref == "" {
				continue
			}
			return strings.ToLower(strings.TrimSpace(p.Name)) + "|" + ref
		}
	}
	return ""
}

// checkMatterRegex says in a sentence why a pattern cannot be used ("" when it can).
func checkMatterRegex(re string) string {
	if re == "" {
		return "Write the pattern, for example INV-[0-9]+."
	}
	if len(re) > maxMatterRegex || strings.ContainsAny(re, "\t\n\r") {
		return "The pattern is too long or has more than one line."
	}
	c, err := regexp.Compile(re)
	if err != nil {
		return "That is not a valid pattern."
	}
	if c.MatchString("") {
		return "The pattern must not match an empty text."
	}
	tree, err := syntax.Parse(re, syntax.Perl)
	if err != nil {
		return "That is not a valid pattern."
	}
	if bigRepeat(tree) {
		return fmt.Sprintf("The pattern asks for a repeat count above %d. Use + or a smaller count.", maxMatterRepeat)
	}
	if !hasLiteral(tree) {
		return "The pattern must contain a fixed piece of at least 3 characters, like INV- in INV-[0-9]+, or it would join unrelated mail."
	}
	return ""
}

func bigRepeat(r *syntax.Regexp) bool {
	if r.Op == syntax.OpRepeat && (r.Min > maxMatterRepeat || r.Max > maxMatterRepeat) {
		return true
	}
	return slices.ContainsFunc(r.Sub, bigRepeat)
}

// hasLiteral says whether every way the pattern can match holds a fixed piece
// of at least three characters.
func hasLiteral(r *syntax.Regexp) bool {
	switch r.Op {
	case syntax.OpLiteral:
		return len(r.Rune) >= 3
	case syntax.OpConcat:
		return slices.ContainsFunc(r.Sub, hasLiteral)
	case syntax.OpCapture, syntax.OpPlus:
		return hasLiteral(r.Sub[0])
	case syntax.OpRepeat:
		return r.Min >= 1 && hasLiteral(r.Sub[0])
	case syntax.OpAlternate:
		for _, s := range r.Sub {
			if !hasLiteral(s) {
				return false
			}
		}
		return len(r.Sub) > 0
	}
	return false
}

// countMatterJoins says how many of the rows the owner already has a pattern would
// have joined: the mail of the last 90 days (at most the newest 3,000 messages)
// is read, grouped by the reference the pattern finds, and every group that
// holds more than one row counts its rows. -1 means it could not be counted in
// time. It reads only; it is called before any write.
func (a *app) countMatterJoins(ctx context.Context, name, re string) int {
	since := a.now().AddDate(0, 0, -matterCountDays).Unix()
	rows, err := a.store.MailForPatterns(ctx, since, matterCountRows)
	if err != nil {
		return -1
	}
	probe := []matterPattern{{Name: name, Re: re}}
	deadline := time.Now().Add(matterCountBudget)
	groups := map[string]map[int64]bool{}
	for _, m := range rows {
		if time.Now().After(deadline) {
			return -1
		}
		if k := matterKey(probe, m.Subject, m.Text); k != "" {
			if groups[k] == nil {
				groups[k] = map[int64]bool{}
			}
			groups[k][m.ItemID] = true
		}
	}
	n := 0
	for _, items := range groups {
		if len(items) > 1 {
			n += len(items)
		}
	}
	return n
}

// --- Settings: the section Matter patterns ---

type matterView struct {
	N    int
	Name string
	Re   string
}

func (a *app) matterViewsFor(ctx context.Context) []matterView {
	var out []matterView
	for i, p := range a.matterPatterns(ctx, a.store.Q) {
		out = append(out, matterView{N: i, Name: p.Name, Re: p.Re})
	}
	return out
}

func (a *app) addMatterPattern(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 8<<10)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "The form could not be read.", http.StatusBadRequest)
		return
	}
	re := strings.TrimSpace(r.PostForm.Get("pattern"))
	name := oneLine(strings.TrimSpace(r.PostForm.Get("name")), maxMatterName)
	if name == "" {
		name = oneLine(re, maxMatterName)
	}
	fail := func(msg string) { a.renderSettings(w, r, http.StatusBadRequest, "", msg+" Nothing was saved.") }
	if msg := checkMatterRegex(re); msg != "" {
		fail(msg)
		return
	}
	ctx := r.Context()
	now := a.now()
	err := a.store.Update(ctx, func(tx *repo.Tx) error {
		ps := a.matterPatterns(ctx, tx.Q)
		for _, p := range ps {
			if p.Re == re {
				return fmt.Errorf("There is already a pattern that says the same.")
			}
		}
		if len(ps) >= maxMatterPatterns {
			return fmt.Errorf("There are already %d patterns.", maxMatterPatterns)
		}
		ps = append(ps, matterPattern{Name: name, Re: re})
		if err := tx.SetSettingBy(ctx, keyMatterPatterns, encodeMatterPatterns(ps), "owner", now); err != nil {
			return err
		}
		return tx.InsertEvent(ctx, now.Unix(), 0, "owner", "setting_changed", map[string]any{"matter_patterns": len(ps)})
	})
	if err != nil {
		if strings.HasPrefix(err.Error(), "There ") {
			fail(err.Error())
			return
		}
		appkit.Error("saving a matter pattern", err)
		http.Error(w, "That did not work. Try again.", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/settings?n=matteradded#h-matter", http.StatusSeeOther)
}

func (a *app) removeMatterPattern(w http.ResponseWriter, r *http.Request) {
	n, err := strconv.Atoi(r.PathValue("n"))
	if err != nil || n < 0 {
		http.NotFound(w, r)
		return
	}
	ctx := r.Context()
	now := a.now()
	err = a.store.Update(ctx, func(tx *repo.Tx) error {
		ps := a.matterPatterns(ctx, tx.Q)
		if n >= len(ps) {
			return fmt.Errorf("gone")
		}
		ps = append(ps[:n], ps[n+1:]...)
		if err := tx.SetSettingBy(ctx, keyMatterPatterns, encodeMatterPatterns(ps), "owner", now); err != nil {
			return err
		}
		return tx.InsertEvent(ctx, now.Unix(), 0, "owner", "setting_changed", map[string]any{"matter_patterns": len(ps)})
	})
	if err != nil && err.Error() != "gone" {
		appkit.Error("removing a matter pattern", err)
		http.Error(w, "That did not work. Try again.", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/settings?n=matterremoved#h-matter", http.StatusSeeOther)
}
