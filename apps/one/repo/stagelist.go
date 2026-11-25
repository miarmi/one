package repo

import "context"

// StagePage lists one page of the shown items of a stage for an agent
// (SPEC 29), the one whose newest message came last first. Items of a test
// run are in it only when run names that run; with no run, only items made
// without one. hasAfter starts the page behind the place (afterAt, afterID)
// of the last item of the page before, so a page does not move while items
// change stage.
func (q Q) StagePage(ctx context.Context, stage, run string, hasAfter bool, afterAt, afterID int64, limit int) ([]ListRow, error) {
	where, args := "i.stage = ? AND i.hidden = 0 AND COALESCE(i.test_run, '') = ?", []any{stage, run}
	if hasAfter {
		where += " AND (i.last_message_at < ? OR (i.last_message_at = ? AND i.id < ?))"
		args = append(args, afterAt, afterAt, afterID)
	}
	return q.listRows(ctx, where, args, "ORDER BY i.last_message_at DESC, i.id DESC LIMIT ? OFFSET ?", limit, 0)
}

// StageCount is how many shown items the stage holds for that run (the true
// total behind StagePage).
func (q Q) StageCount(ctx context.Context, stage, run string) int {
	var n int
	q.x.QueryRowContext(ctx, `SELECT COUNT(*) FROM items i WHERE i.stage = ? AND i.hidden = 0 AND COALESCE(i.test_run, '') = ?`, stage, run).Scan(&n)
	return n
}

// HasOwnerNote says whether the owner wrote a note on the item at any time.
func (q Q) HasOwnerNote(ctx context.Context, id int64) bool {
	var n int
	q.x.QueryRowContext(ctx, `SELECT COUNT(*) FROM events WHERE item_id=? AND type='noted' AND actor='owner'`, id).Scan(&n)
	return n > 0
}
