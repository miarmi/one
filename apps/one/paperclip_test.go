package main

import (
	"fmt"
	"strings"
	"testing"

	"lab/apps/one/repo"
)

// SPEC 7: a closed row whose thread holds a file shows a paperclip named
// `Has attachments`; a picture inside a message's text (an inline part) and a
// signature logo do not count, and a row without a file has no paperclip.
func TestClosedRowShowsAPaperclipOnlyWhenTheThreadHoldsARealFile(t *testing.T) {
	e := newListEnv(t)
	add := func(it repo.Item, disp, ctype, name string) {
		t.Helper()
		if err := e.a.store.Update(e.ctx, func(tx *repo.Tx) error {
			msgs, _, err := tx.MessagesSince(e.ctx, it.ID, 0)
			if err != nil || len(msgs) == 0 {
				return fmt.Errorf("no message: %v", err)
			}
			_, err = tx.InsertFile(e.ctx, repo.File{ItemID: it.ID, Source: srcGmail, MessageID: msgs[0].ID, Part: name, Filename: name, ContentType: ctype, Disposition: disp, Status: "pending", AddedAt: 1})
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	withFile := e.seed(stageNew, e.mail("Invoice with a file", 1))
	add(withFile, "attachment", "application/pdf", "invoice.pdf")
	logoOnly := e.seed(stageNew, e.mail("Newsletter with a logo", 2))
	add(logoOnly, "inline", "image/png", "logo.png")
	plain := e.seed(stageNew, e.mail("Just words", 3))
	both := e.seed(stageNew, e.mail("Logo and a photo", 4))
	add(both, "inline", "image/png", "signature.png")
	add(both, "attachment", "image/jpeg", "photo.jpg")

	page := get(e.h, "/list/new").Body.String()
	row := func(it repo.Item) string {
		marker := fmt.Sprintf(`id="item-%d"`, it.ID)
		i := strings.Index(page, marker)
		if i < 0 {
			t.Fatalf("no row %d", it.ID)
		}
		j := strings.Index(page[i:], "</li>")
		return page[i : i+j]
	}
	const clip = `aria-label="Has attachments"`
	for name, want := range map[string]bool{"with a file": true, "logo only": false, "plain": false, "logo and photo": true} {
		it := map[string]repo.Item{"with a file": withFile, "logo only": logoOnly, "plain": plain, "logo and photo": both}[name]
		if got := strings.Contains(row(it), clip); got != want {
			t.Errorf("%s: paperclip %v, want %v", name, got, want)
		}
	}
	if n := strings.Count(page, clip); n != 2 {
		t.Errorf("paperclips on the page: %d", n)
	}
}
