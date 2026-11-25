package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"lab/apps/one/repo"
)

// A file is dated by its message, not by when ONE saved it.
func TestAFileInTheListSaysWhichMessageItBelongsToAndWhenThatWasSent(t *testing.T) {
	e := newClaudeEnv(t)
	old := e.seedMail("gmail", "erin@shop.test", "Card", "<f1@shop.test>", "An invented old message with a card.", 400, false)
	oldMsg := e.messagesOf(old)[0]
	var newID int64
	var fileID, agentFileID int64
	if err := e.a.store.Update(e.ctx, func(tx *repo.Tx) error {
		at := e.ago(1)
		rawID, _, err := tx.InsertRaw(e.ctx, repo.Raw{Source: "gmail", SourceID: "n-1", VersionKey: "a@0", ThreadKey: "gmail:x", ReceivedAt: at, IngestedAt: at, BlobSHA: "ab", Payload: "{}"})
		if err != nil {
			return err
		}
		newID, _, err = tx.InsertMessage(e.ctx, repo.Message{ItemID: old.ID, RawID: rawID, Source: "gmail", SourceID: "n-1", ThreadKey: "gmail:x", Network: "mail",
			Sender: "erin@shop.test", Subject: "Re: Card", Text: "An invented new message without a file.", ReceivedAt: at, HeaderID: "<f2@shop.test>"})
		if err != nil {
			return err
		}
		fileID, err = tx.InsertFile(e.ctx, repo.File{ItemID: old.ID, Source: "gmail", MessageID: oldMsg.ID, Part: "1", Filename: "card.vcf", ContentType: "text/vcard", Disposition: "attachment", Size: 10, Status: "pending", AddedAt: e.now.Unix()})
		if err != nil {
			return err
		}
		agentFileID, err = tx.InsertOwnFile(e.ctx, repo.File{ItemID: old.ID, Source: "claude", Part: "agent-1", Filename: "made.txt", ContentType: "text/plain", Size: 5, AddedAt: e.now.Unix()})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		path string
		old  bool
	}{{"/api/items/" + ref(old), true}, {"/api/items/" + ref(old) + "?all=1", false}} {
		text, isErr := e.a.callAPI(e.ctx, "GET", c.path, nil)
		if isErr {
			t.Fatal(text)
		}
		var got struct {
			Files []map[string]any `json:"files"`
		}
		json.Unmarshal([]byte(text), &got)
		by := map[string]map[string]any{}
		for _, f := range got.Files {
			by[fmt.Sprint(f["name"])] = f
		}
		card, made := by["card.vcf"], by["made.txt"]
		if card == nil || made == nil {
			t.Fatalf("%s: files %v", c.path, got.Files)
		}
		if card["sent_at"] != isoTime(oldMsg.ReceivedAt) || card["message_id"] != float64(oldMsg.ID) || card["added_at"] != isoTime(e.now.Unix()) {
			t.Errorf("%s: card %v", c.path, card)
		}
		if (card["from_older_message"] == true) != c.old {
			t.Errorf("%s: from_older_message %v, want %v", c.path, card["from_older_message"], c.old)
		}
		if _, has := made["sent_at"]; has || made["message_id"] != nil || made["added_by"] == nil || made["from_older_message"] == true {
			t.Errorf("%s: a file an agent added: %v", c.path, made)
		}
	}
	_ = newID
	// get_file names the message too.
	text, isErr := e.a.callAPI(e.ctx, "GET", fmt.Sprintf("/api/items/%s/file?file_id=%d", ref(old), fileID), nil)
	if isErr || !strings.Contains(text, `"sent_at":"`+isoTime(oldMsg.ReceivedAt)+`"`) {
		t.Errorf("get_file: %v %s", isErr, text)
	}
	_ = agentFileID
}
