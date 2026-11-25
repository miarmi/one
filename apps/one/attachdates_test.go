package main

import (
	"strings"
	"testing"
	"time"

	"lab/apps/one/repo"
)

// SPEC 7: the Attachments list goes by when a file was sent or attached, not
// by when ONE saved it, and shows that date as the thread does.
func TestAttachmentsAreOrderedBySendTimeAndShowTheDateAsTheThreadDoes(t *testing.T) {
	e := newListEnv(t)
	oldAt := time.Date(2024, 1, 26, 10, 0, 0, 0, time.UTC).Unix()
	it := e.seed(stageNew, seedMsg{subject: "Card", name: "Alice Example", sender: "alice@example.test", text: "Old card.", at: oldAt},
		seedMsg{subject: "Re: Card", name: "Alice Example", sender: "alice@example.test", text: "Recent photo.", at: e.ago(1)})
	msgs, _, err := e.a.store.MessagesSince(e.ctx, it.ID, 0)
	if err != nil || len(msgs) != 2 {
		t.Fatal(err, len(msgs))
	}
	var oldMsg, newMsg repo.Message
	for _, m := range msgs {
		if m.ReceivedAt == oldAt {
			oldMsg = m
		} else {
			newMsg = m
		}
	}
	saved := e.now.Unix() // ONE saved the old card today
	if err := e.a.store.Update(e.ctx, func(tx *repo.Tx) error {
		if _, err := tx.InsertFile(e.ctx, repo.File{ItemID: it.ID, Source: srcGmail, MessageID: oldMsg.ID, Part: "1", Filename: "card-old.vcf", ContentType: "text/vcard", Disposition: "attachment", Size: 10, Status: "saved", Path: "x", AddedAt: saved}); err != nil {
			return err
		}
		if _, err := tx.InsertFile(e.ctx, repo.File{ItemID: it.ID, Source: srcGmail, MessageID: newMsg.ID, Part: "2", Filename: "photo-new.jpg", ContentType: "image/jpeg", Disposition: "attachment", Size: 10, Status: "saved", Path: "y", AddedAt: oldAt}); err != nil {
			return err
		}
		_, err := tx.InsertOwnFile(e.ctx, repo.File{ItemID: it.ID, Source: "claude", Part: "agent-1", Filename: "made-by-agent.txt", ContentType: "text/plain", Size: 5, AddedAt: e.ago(5)})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	page := e.openRow(it.ID)
	i := strings.Index(page, `class="attachments"`)
	if i < 0 {
		t.Fatalf("no Attachments list:\n%s", page)
	}
	list := page[i:]
	list = list[:strings.Index(list, "</section>")]
	pos := func(s string) int { return strings.Index(list, s) }
	a, b, c := pos("card-old.vcf"), pos("made-by-agent.txt"), pos("photo-new.jpg")
	if a < 0 || b < 0 || c < 0 || !(a < b && b < c) {
		t.Errorf("order by send time: card %d, agent %d, photo %d\n%s", a, b, c, list)
	}
	for _, want := range []string{"sent <time", "attached <time", ">26 Jan 2024<", ">14 Nov<", ">18 Nov<"} {
		if !strings.Contains(list, want) {
			t.Errorf("no date %q in\n%s", want, list)
		}
	}
	// each caption says which date it is: sent for a file of a message, attached for one attached in the thread
	for _, want := range []string{`added by Alice Example · sent <time datetime="2024-01-26T10:00:00Z"`, `added by Claude · attached <time datetime="2026-11-14T12:00:00Z"`} {
		if !strings.Contains(list, want) {
			t.Errorf("no caption %q in\n%s", want, list)
		}
	}
	if strings.Contains(list, ">19 Nov<") {
		t.Errorf("the saving time is shown:\n%s", list)
	}
}
