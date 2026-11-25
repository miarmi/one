package main

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"lab/apps/one/repo"
	"lab/platform/appkit"
)

// Test runs (SPEC 16, 30; API.md "Test runs"). A browser test names its run
// when it makes something, and one address removes everything of that run and
// nothing else. This file holds the way a run name comes in, the two addresses
// and the section of the Settings page; the marking itself is in repo/.

const (
	runHeader     = "X-Test-Run"
	testRunsPath  = "/api/test-runs"
	badRunMessage = "The test run name must be e2e- followed by 8 to 40 lower-case letters and digits."
)

// runFrom reads the run an agent's request names: the header, and for a body
// field (the bridge's batch) the field too. Both may be given if they agree.
// It says whether any was given; an invalid name is an error in a sentence.
func runFrom(headers http.Header, field string) (run string, err error) {
	h := strings.TrimSpace(headers.Get(runHeader))
	f := strings.TrimSpace(field)
	if h != "" && f != "" && h != f {
		return "", bad("The header and the field name different test runs.")
	}
	if h == "" {
		h = f
	}
	if h != "" && !repo.ValidRun(h) {
		return "", bad(badRunMessage)
	}
	return h, nil
}

// withHeaderRun puts the run of a write request into its context. A request
// that only reads is not changed by a run name (it may still be checked).
func withHeaderRun(w http.ResponseWriter, r *http.Request) (*http.Request, bool) {
	run, err := runFrom(r.Header, "")
	if err != nil {
		answerErr(w, err)
		return r, false
	}
	if run == "" {
		return r, true
	}
	switch r.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return r, true
	}
	return r.WithContext(repo.WithRun(r.Context(), run)), true
}

// findItemW finds the item a write is about. A call under a test run may write
// only to an item of that run: a real item, or another run's, is refused with a
// plain message and nothing is written.
func (a *app) findItemW(ctx context.Context, q repo.Q, ref string) (repo.Item, error) {
	it, err := a.findItem(ctx, q, ref)
	if err != nil {
		return it, err
	}
	if run := repo.RunOf(ctx); run != "" && it.TestRun != run {
		return repo.Item{}, runRefused()
	}
	return it, nil
}

func runRefused() error {
	return &userErr{http.StatusForbidden, (&repo.RunError{}).Error()}
}

// asUserErr turns the repo's refusal of a record outside the run into the
// answer an agent gets.
func asUserErr(err error) error {
	if repo.IsRunError(err) {
		return runRefused()
	}
	return err
}

// checkRunRow refuses (in a sentence) a call under a run that names a person,
// note, rule or entry which is not part of that run.
func checkRunRow(ctx context.Context, q repo.Q, table string, id int64) error {
	return asUserErr(q.CheckRow(ctx, table, id))
}

// --- the two addresses ---

// isTestRunsPath says whether a request is for the run addresses, which both
// the agents' token and the bridge's token open: neither can have made
// anything but marked data.
func isTestRunsPath(p string) bool {
	return p == testRunsPath || strings.HasPrefix(p, testRunsPath+"/")
}

func countsJSON(c repo.RunCounts) map[string]any {
	out := map[string]any{}
	for k, v := range c {
		out[k] = v
	}
	return out
}

func (a *app) testRunsRoutes(m interface {
	HandleFunc(string, func(http.ResponseWriter, *http.Request))
}) {
	m.HandleFunc("GET "+testRunsPath, a.apiTestRuns)
	m.HandleFunc("DELETE "+testRunsPath+"/{run}", a.apiRemoveTestRun)
	m.HandleFunc("POST "+testRunsPath+"/{run}/closed-summary", a.apiTestRunSummary)
}

// apiTestRuns lists the runs that have records, so leftovers of a broken run
// can be found.
func (a *app) apiTestRuns(w http.ResponseWriter, r *http.Request) {
	runs, err := a.store.ListRuns(r.Context())
	if err != nil {
		answerErr(w, err)
		return
	}
	out := []map[string]any{}
	for _, ri := range runs {
		m := map[string]any{"run": ri.Run, "records": ri.Total, "counts": countsJSON(ri.Counts)}
		if ri.FirstAt > 0 {
			m["first_at"] = isoTime(ri.FirstAt)
		}
		out = append(out, m)
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "runs": out})
}

// apiRemoveTestRun removes one run completely. A run that does not exist
// answers zeros.
func (a *app) apiRemoveTestRun(w http.ResponseWriter, r *http.Request) {
	run := r.PathValue("run")
	if !repo.ValidRun(run) {
		apiError(w, http.StatusBadRequest, badRunMessage)
		return
	}
	counts, err := a.removeRun(r.Context(), run)
	if err != nil {
		answerErr(w, err)
		return
	}
	// SPEC 16: "total" is the sum of the counts the answer shows, every kind in it.
	total := 0
	for _, n := range counts {
		total += n
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "run": run, "removed": countsJSON(counts), "total": total})
}

// removeRun is the one function behind the address and the Settings button.
// The records go in one transaction; the files of the pool that nothing holds
// any more and that only this run made go from the disk once it is committed.
func (a *app) removeRun(ctx context.Context, run string) (repo.RunCounts, error) {
	if !repo.ValidRun(run) {
		return nil, bad(badRunMessage)
	}
	var counts repo.RunCounts
	var paths []string
	err := a.store.Update(ctx, func(tx *repo.Tx) error {
		var err error
		counts, paths, err = tx.RemoveRun(ctx, run)
		return err
	})
	if err != nil {
		if strings.Contains(err.Error(), "FOREIGN KEY") {
			// Something real points at a record of the run: it is not taken away.
			return nil, conflict("Something that is not part of this test run still refers to its records, so nothing was removed.")
		}
		return nil, err
	}
	removed := 0
	for _, p := range paths {
		if !poolPathRE.MatchString(p) {
			continue
		}
		if err := os.Remove(filepath.Join(a.imp.pool.root, filepath.FromSlash(p))); err == nil || errors.Is(err, os.ErrNotExist) {
			removed++
		}
	}
	counts["pool_files"] = removed
	return counts, nil
}

// --- the owner's page: Settings, Test data ---

type testRunView struct {
	Run   string
	Since string
	Lines []string
	Total int
}

// kindWords says each kind of record in words for the page.
var kindWords = []struct{ kind, one, many string }{
	{"items", "item", "items"}, {"messages", "message", "messages"}, {"events", "entry", "entries"}, {"files", "file", "files"},
	{"people", "person", "people"}, {"notes", "note", "notes"}, {"proposals", "proposal", "proposals"}, {"agenda", "to-do", "to-dos"},
	{"rules", "rule", "rules"}, {"skip_rules", "skip rule", "skip rules"}, {"outbox", "outgoing message", "outgoing messages"},
}

func (a *app) testRunViews(ctx context.Context) []testRunView {
	runs, err := a.store.ListRuns(ctx)
	if err != nil {
		appkit.Error("listing test runs", err)
		return nil
	}
	var out []testRunView
	for _, ri := range runs {
		v := testRunView{Run: ri.Run, Total: ri.Total}
		if ri.FirstAt > 0 {
			v.Since, _, _ = a.whenText(ri.FirstAt, a.now())
		}
		for _, k := range kindWords {
			if n := ri.Counts[k.kind]; n > 0 {
				w := k.many
				if n == 1 {
					w = k.one
				}
				v.Lines = append(v.Lines, strconv.Itoa(n)+" "+w)
			}
		}
		out = append(out, v)
	}
	return out
}

// removeTestData is the Remove button of the Settings section. It is a
// signed-in page action behind the cross-site check like every other post.
func (a *app) removeTestData(w http.ResponseWriter, r *http.Request) {
	run := r.PathValue("run")
	if !repo.ValidRun(run) {
		http.NotFound(w, r)
		return
	}
	ctx := r.Context()
	if _, err := a.removeRun(ctx, run); err != nil {
		var ue *userErr
		if errors.As(err, &ue) {
			a.renderSettings(w, r, ue.Status, "", ue.Msg)
			return
		}
		appkit.Error("removing a test run", err)
		http.Error(w, "That did not work. Try again.", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/settings?n=testremoved#h-testdata", http.StatusSeeOther)
}

// apiTestRunSummary is the test-only way to run the summary of what Claude
// closed (SPEC 27) for the items of one test run, as the evening timer would.
// It looks only at that run's items and writes only marked records; a real
// item never enters it, and the phone is never told. With ?now=1 the model's
// text is written at once for those items (a test run reaches the model with
// the app's real key and counts in the day's spend).
func (a *app) apiTestRunSummary(w http.ResponseWriter, r *http.Request) {
	run := r.PathValue("run")
	if !repo.ValidRun(run) {
		apiError(w, http.StatusBadRequest, badRunMessage)
		return
	}
	ctx := r.Context()
	if _, err := a.closedSummarySyncOpt(ctx, run, true, r.URL.Query().Get("now") == "1"); err != nil {
		answerErr(w, err)
		return
	}
	out := map[string]any{"ok": true, "run": run}
	if row, ok := a.store.ClosedSummaryRow(repo.WithRun(ctx, run), run); ok {
		out["item"] = repo.Number(row.Number)
		out["subject"] = row.Subject
		if t, ok := a.summaryCurrent(ctx, a.store.Q, row.ID); ok {
			out["model"] = t.Model
			if t.Intro != "" {
				out["intro"] = t.Intro
			}
		}
	}
	writeJSON(w, http.StatusOK, out)
}
