package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"

	"lab/apps/one/repo"
)

// Reimport brings back one message of the Office mailbox that a rule
// dropped, by reading it again with the rules left out for this message only.
// Like every other call in this importer it only reads.
func (gi *graphImporter) Reimport(ctx context.Context, skippedID int64) (err error) {
	gi.mu.Lock()
	defer gi.mu.Unlock()
	gi.busy.Store(true)
	defer gi.busy.Store(false)
	if !gi.a.graphSigned() {
		return errNotConnected
	}
	sk, ok := gi.a.store.SkippedByID(ctx, skippedID)
	if !ok || sk.Source != srcGraph {
		return errors.New("there is no such skipped message")
	}
	run, err := gi.startRun(ctx, "reimport")
	if err != nil {
		return err
	}
	defer func() { gi.finish(ctx, run, err) }()
	_, sec, err := gi.a.graphAccess(ctx)
	if err != nil {
		return err
	}
	folders, err := gi.folders(ctx)
	if err != nil {
		return err
	}
	gs, err := gi.scope(ctx, run, sec.Address, folders)
	if err != nil {
		return err
	}
	gs.ignoreRules = true
	gs.strict = true // the owner asked for this one: a failure is told to him, not left out
	var raw json.RawMessage
	var f graphFolderInfo
	if raw, f, err = gi.readOne(ctx, sk.SourceID); err != nil {
		return err
	}
	if err = gs.entry(ctx, f, raw); err != nil {
		return err
	}
	return gi.a.store.Update(ctx, func(tx *repo.Tx) error { return tx.MarkReimported(ctx, sk.ID, gi.a.now().Unix()) })
}

var errGraphGone = errors.New("Microsoft no longer has that message")

// readOne reads one message by its own id, with the folder it sits in.
func (gi *graphImporter) readOne(ctx context.Context, id string) (json.RawMessage, graphFolderInfo, error) {
	var raw json.RawMessage
	if err := gi.get(ctx, "/me/messages/"+url.PathEscape(id)+"?%24select="+url.QueryEscape(graphSelect), &raw); err != nil {
		return nil, graphFolderInfo{}, err
	}
	var m graphMessage
	if err := json.Unmarshal(raw, &m); err != nil || m.ID == "" {
		return nil, graphFolderInfo{}, errGraphGone
	}
	var parent struct {
		Parent string `json:"parentFolderId"`
	}
	json.Unmarshal(raw, &parent)
	f := graphFolderInfo{ID: parent.Parent}
	var info graphFolderInfo
	if gi.get(ctx, "/me/mailFolders/"+url.PathEscape(parent.Parent)+"?%24select=id,displayName,childFolderCount", &info) == nil && info.ID != "" {
		f = info
	}
	return raw, f, nil
}

// retryFailed tries again the messages that failed on their own in an
// earlier run (the ledger of softfail.go). One that Microsoft no longer has
// leaves the ledger; a failure of the connection ends the run as always.
func (gs *graphScope) retryFailed(ctx context.Context) error {
	a := gs.gi.a
	for _, id := range a.failDue(ctx, srcGraph, graphPageSize) {
		raw, f, err := gs.gi.readOne(ctx, id)
		var me *msError
		if errors.Is(err, errGraphGone) || (errors.As(err, &me) && me.Status == 404) {
			a.failClear(ctx, srcGraph, id)
			continue
		}
		if err != nil {
			return err
		}
		if err := gs.entry(ctx, f, raw); err != nil {
			return err
		}
	}
	return nil
}
