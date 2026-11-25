package main

import (
	"context"
	"strconv"
	"strings"
	"time"
)

// importView is the state of the running or last import, in words for the page.
type importView struct {
	Has           bool
	Running       bool
	Title         string
	Status        string
	Started       string
	Finished      string
	Read          int
	Stored        int
	Skipped       int
	ItemsMade     int
	FilesSaved    int
	FilesPending  int
	Bytes         string
	Error         string
	LastSync      string
	TotalMessages int // all messages stored, whatever import brought them
	TotalItems    int // all items stored, history included
	// Totals and Line are the state in words (the owner, 2026-11-19: "why all
	// these 0s"): the totals first, then the last run in one sentence that
	// names only what is not zero. A run that found nothing new says so.
	Totals string
	Line   string
}

// importLine says what a run did in one sentence. runName is "Import of one
// week"; when is its start; status is running, done, failed or anything else.
func importLine(runName, when, status string, v importView) string {
	head := runName
	if when != "" {
		head += " (" + when + ")"
	}
	read := plural(int64(v.Read), "message")
	switch status {
	case "running":
		return head + ": running, " + read + " read so far."
	case "failed":
		return head + ": failed after " + read + " read."
	case "done":
	default:
		return head + ": stopped after " + read + " read; it continues where it left off."
	}
	var s string
	switch {
	case v.Read == 0:
		s = head + ": finished. It found no messages in that time."
	case v.Stored == 0 && v.ItemsMade == 0 && v.FilesSaved == 0:
		s = head + ": finished. It read " + read + " and found nothing new; they were all stored already"
		if v.Skipped > 0 {
			s = head + ": finished. It read " + read + " and found nothing new; " + strconv.Itoa(v.Skipped) + " " + pluralWas(v.Skipped) + " skipped by a rule and the rest stored already"
		}
		s += "."
	default:
		var also []string
		if v.Skipped > 0 {
			also = append(also, strconv.Itoa(v.Skipped)+" skipped by a rule")
		}
		if v.ItemsMade > 0 {
			also = append(also, plural(int64(v.ItemsMade), "item")+" made")
		}
		if v.FilesSaved > 0 {
			also = append(also, plural(int64(v.FilesSaved), "file")+" saved")
		}
		s = head + ": finished. It read " + read + " and stored " + strconv.Itoa(v.Stored) + " as new"
		if len(also) > 0 {
			s += " (" + strings.Join(also, ", ") + ")"
		}
		s += "."
	}
	if v.FilesPending > 0 {
		s += " " + plural(int64(v.FilesPending), "file") + " still to be fetched."
	}
	return s
}

func pluralWas(n int) string {
	if n == 1 {
		return "was"
	}
	return "were"
}

var runTitles = map[string]string{
	"first":    "First import",
	"week":     "Import of one week",
	"month":    "Import of one month",
	"reimport": "Import of one skipped message",
}

var stageLabels = map[string]string{
	stageNew:        "New",
	stageInProgress: "In progress",
	stagePending:    "Pending",
	stageResolved:   "Resolved",
	stageClosed:     "Closed",
	stageHistory:    "History",
}

func (a *app) clock(unix int64) string {
	if unix == 0 {
		return ""
	}
	return time.Unix(unix, 0).In(a.loc).Format("2 Jan 2006 15:04")
}

func (a *app) importViewFor(ctx context.Context, src mailSource) importView {
	var v importView
	v.TotalMessages = a.store.CountMessages(ctx, src.Name)
	v.TotalItems = a.store.CountItemsOf(ctx, src.Name)
	v.Totals = plural(int64(v.TotalMessages), "message") + " in " + plural(int64(v.TotalItems), "item") + " stored."
	if r, ok := a.store.LastFinishedRun(ctx, src.Name, "sync"); ok {
		v.LastSync = a.clock(r.FinishedAt)
	}
	r, ok := a.store.LatestRun(ctx, src.Name, "first", "week", "month", "reimport")
	if !ok {
		return v
	}
	v.Has = true
	v.Running = r.Status == "running"
	v.Title = runTitles[r.Mode]
	switch r.Status {
	case "running":
		v.Status = "Running"
	case "done":
		v.Status = "Finished"
	case "failed":
		v.Status = "Failed"
	default:
		v.Status = "Stopped"
	}
	v.Started, v.Finished = a.clock(r.StartedAt), a.clock(r.FinishedAt)
	v.Read, v.Stored, v.Skipped, v.ItemsMade, v.FilesSaved = r.Fetched, r.Stored, r.Skipped, r.ItemsMade, r.FilesSaved
	v.FilesPending = a.store.PendingFileCountOf(ctx, src.Name)
	v.Bytes = humanBytes(uint64(max(r.Bytes, 0)))
	v.Error = r.Error
	v.Line = importLine(v.Title, v.Started, r.Status, v)
	return v
}

// historyView is the state of the history of one whole mailbox, in words.
type historyView struct {
	Started  bool
	Running  bool
	Status   string
	Recorded int    // messages in the history
	Since    string // the date of the oldest
	Error    string
	Line     string // the state in one sentence
}

func (a *app) historyViewFor(ctx context.Context, src mailSource, key string) historyView {
	var v historyView
	hs := a.loadHistoryState(ctx, key)
	n, oldest := a.store.HistoryTotals(ctx, src.Name)
	v.Recorded = n
	if oldest > 0 {
		v.Since = time.Unix(oldest, 0).In(a.loc).Format("2 Jan 2006")
	}
	v.Started = hs.Started > 0
	r, ok := a.store.LatestRun(ctx, src.Name, historyMode)
	switch {
	case !v.Started:
		v.Status = "Not read yet"
	case hs.Done:
		v.Status = "Finished"
	case ok && r.Status == "running":
		v.Status, v.Running = "Reading", true
	case ok && r.Status == "failed":
		v.Status, v.Error = "Failed", r.Error
	case ok && r.Status == "interrupted":
		v.Status = "Stopped; it continues where it left off"
	default:
		v.Status = "Waiting for its turn"
	}
	v.Line = "History: " + strings.ToLower(v.Status[:1]) + v.Status[1:] + "."
	if v.Recorded > 0 {
		v.Line += " " + plural(int64(v.Recorded), "message") + " recorded"
		if v.Since != "" {
			v.Line += ", back to " + v.Since
		}
		v.Line += "."
	}
	return v
}
