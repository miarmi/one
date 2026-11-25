package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

// Tests of the bridge against the stricter fake Beeper, which behaves as
// Beeper's documentation says (see fakes_test.go). Every name, text and file is invented.

func (r *rig) networksSaid(i int) map[string]map[string]any {
	r.one.mu.Lock()
	defer r.one.mu.Unlock()
	out := map[string]map[string]any{}
	for _, n := range r.one.alives[i]["networks"].([]any) {
		m := n.(map[string]any)
		out[m["network"].(string)] = m
	}
	return out
}

func TestTheBridgeReadsAccountsAsABareArrayAndReportsEachNetworksStatus(t *testing.T) {
	r := newRig(t)
	r.beeper.accounts = []bAccount{
		{AccountID: "a1", Network: "WhatsApp", Status: "connected"},
		{AccountID: "a2", Network: "Facebook/Messenger", Status: "connection_required", StatusText: "The login ran out"},
		{AccountID: "a3", Network: "Signal", Status: "disabled"},
		{AccountID: "a4", Network: "iMessage", Status: "backfilling"},
	}
	r.chat("c1", "Jana", "single", r.msg("m1", "Ahoj"))
	cycle(t, r.start())
	if r.one.alive != 1 {
		t.Fatalf("alive %d", r.one.alive)
	}
	n := r.networksSaid(0)
	if n["whatsapp"]["state"] != "connected" || n["whatsapp"]["status"] != "connected" {
		t.Errorf("whatsapp: %v", n["whatsapp"])
	}
	m := n["messenger"]
	if m["state"] != "needs_login" || m["status"] != "connection_required" || m["status_text"] != "The login ran out" {
		t.Errorf("messenger: %v", m)
	}
	if n["signal"]["state"] != "unknown" || n["signal"]["status"] != "disabled" {
		t.Errorf("a network switched off in Beeper is not a login: %v", n["signal"])
	}
	if n["imessage"]["state"] != "connected" || n["imessage"]["status"] != "backfilling" {
		t.Errorf("a network catching up is not a login: %v", n["imessage"])
	}
	if r.one.alives[0]["files_pending"].(float64) != 0 {
		t.Errorf("files waiting: %v", r.one.alives[0]["files_pending"])
	}
}

func TestTheBridgeReportsANetworkThatNeedsALoginForEveryStatusThatSaysSo(t *testing.T) {
	for _, status := range []string{"connection_required", "reconnect_required", "attention_required", "disconnected"} {
		r := newRig(t)
		r.beeper.accounts = []bAccount{{AccountID: "a1", Network: "WhatsApp", Status: status}}
		r.chat("c1", "Jana", "single", r.msg("m1", "Ahoj"))
		cycle(t, r.start())
		if got := r.networksSaid(0)["whatsapp"]; got["state"] != "needs_login" || got["status"] != status {
			t.Errorf("%s: %v", status, got)
		}
	}
	// Two logins of one network: the one that needs a login counts.
	r := newRig(t)
	r.beeper.accounts = []bAccount{{AccountID: "a1", Network: "WhatsApp", Status: "connected"}, {AccountID: "a2", Network: "WhatsApp", Status: "reconnect_required"}}
	r.chat("c1", "Jana", "single", r.msg("m1", "Ahoj"))
	cycle(t, r.start())
	if got := r.networksSaid(0); len(got) != 1 || got["whatsapp"]["state"] != "needs_login" {
		t.Errorf("%v", got)
	}
}

func TestTheBridgePagesMessagesByCursorWithoutLossOrRepeatAcrossARestart(t *testing.T) {
	r := newRig(t)
	r.chat("c1", "Jana", "single", r.msg("m1", "1"), r.msg("m2", "2"), r.msg("m3", "3"), r.msg("m4", "4"), r.msg("m5", "5"))
	cycle(t, r.start())
	if got := strings.Join(r.one.messageIDs(), ","); got != "m1,m2,m3,m4,m5" {
		t.Fatalf("the first read: %s", got)
	}
	for _, q := range r.beeper.reqs {
		if strings.Contains(q, "limit") && strings.Contains(q, "/messages") {
			t.Errorf("a limit was asked of messages: %s", q)
		}
	}
	st, _ := loadState(r.cfg.StatePath)
	cs := st.Chats["whatsapp/c1"]
	if cs == nil || cs.Cursor != "c-"+r.beeper.msgs["c1"][4].SortKey {
		t.Fatalf("the newest cursor is not kept: %+v", cs)
	}
	// A restart: three new messages come, and only they are asked for and sent, from the cursor on.
	r.now = r.now.Add(time.Minute)
	r.chat("c1", "Jana", "single", r.msg("m1", "1"), r.msg("m2", "2"), r.msg("m3", "3"), r.msg("m4", "4"), r.msg("m5", "5"), r.msg("m6", "6"), r.msg("m7", "7"), r.msg("m8", "8"))
	r.beeper.reqs = nil
	cycle(t, r.start())
	if got := strings.Join(r.one.messageIDs(), ","); got != "m1,m2,m3,m4,m5,m6,m7,m8" {
		t.Fatalf("after the restart: %s", got)
	}
	asked := false
	for _, q := range r.beeper.reqs {
		if strings.Contains(q, "direction=after") && strings.Contains(q, "cursor=c-"+r.beeper.msgs["c1"][4].SortKey) {
			asked = true
		}
	}
	if !asked {
		t.Errorf("the read did not go on from the cursor: %v", r.beeper.reqs)
	}
	// ONE refuses a batch: the cursor stays, and the message is neither lost nor sent twice.
	r.now = r.now.Add(time.Minute)
	r.chat("c1", "Jana", "single", r.msg("m1", "1"), r.msg("m2", "2"), r.msg("m3", "3"), r.msg("m4", "4"), r.msg("m5", "5"), r.msg("m6", "6"), r.msg("m7", "7"), r.msg("m8", "8"), r.msg("m9", "9"))
	r.one.mu.Lock()
	r.one.fileFail = 0
	r.one.mu.Unlock()
	good := r.cfg.OneToken
	r.cfg.OneToken = "wrong-token-EEEE5555FFFF6666"
	if err := r.start().Cycle(testCtx); err == nil {
		t.Fatal("a refused token is not an error")
	}
	r.cfg.OneToken = good
	cycle(t, r.start())
	cycle(t, r.start())
	if got := strings.Join(r.one.messageIDs(), ","); got != "m1,m2,m3,m4,m5,m6,m7,m8,m9" {
		t.Errorf("after a refused batch: %s", got)
	}
}

func TestTheFirstReadGoesBackOnlyAsFarAsONETellsTheBridgeAndAMonthByDefault(t *testing.T) {
	old := func(id string, days int) bMessage {
		m := (&rig{}).msg(id, id)
		m.Timestamp = time.Date(2026, 11, 19, 11, 0, 0, 0, time.UTC).AddDate(0, 0, -days).Format(time.RFC3339)
		return m
	}
	r := newRig(t)
	r.beeper.pageSize = 1
	r.chat("c1", "Jana", "single", old("m1", 40), old("m2", 20), old("m3", 6), old("m4", 1))
	r.one.backfill = 0 // ONE says nothing: the default of a month
	cycle(t, r.start())
	if got := strings.Join(r.one.messageIDs(), ","); got != "m2,m3,m4" {
		t.Errorf("by default a month: %s", got)
	}
	r2 := newRig(t)
	r2.beeper.pageSize = 1
	r2.chat("c1", "Jana", "single", old("m1", 40), old("m2", 20), old("m3", 6), old("m4", 1))
	r2.one.backfill = 7
	cycle(t, r2.start())
	if got := strings.Join(r2.one.messageIDs(), ","); got != "m3,m4" {
		t.Errorf("as ONE says, 7 days: %s", got)
	}
}

func TestTheBridgeDecodesAnAttachmentWithSizeAsAnObjectAndTakesTheBytesFromFileSize(t *testing.T) {
	r := newRig(t)
	m := r.msg("m1", "Foto")
	m.Attachments = []bAttachment{{ID: "mxc://example.org/a1", Type: "img", FileName: "cabin.png", MimeType: "image/png", FileSize: 2048, SrcURL: "mxc://example.org/a1",
		Size: json.RawMessage(`{"width":640,"height":480}`)}}
	r.chat("c1", "Jana", "single", m)
	cycle(t, r.start())
	got := r.one.message("m1")
	if got == nil {
		t.Fatal("the message with an attachment never arrived")
	}
	a := got["attachments"].([]any)[0].(map[string]any)
	if a["size"].(float64) != 2048 || a["name"] != "cabin.png" || a["type"] != "image/png" {
		t.Errorf("the attachment: %v", a)
	}
}

func TestTheBridgeFetchesAFileByTheDownloadCallAndReadsTheLocalFile(t *testing.T) {
	r := newRig(t)
	local := r.writeLocal("cabin.png", []byte("PNGxx"))
	m := r.msg("m1", "Foto")
	m.Attachments = []bAttachment{{ID: "mxc://example.org/a1", FileName: "cabin.png", MimeType: "image/png", FileSize: 5, SrcURL: "mxc://example.org/a1"}}
	r.beeper.media["mxc://example.org/a1"] = local
	// A second file is already a local file: it is read as it is.
	m.Attachments = append(m.Attachments, bAttachment{ID: "loc-2", FileName: "b.txt", MimeType: "text/plain", FileSize: 3, SrcURL: "file://" + r.writeLocal("b.txt", []byte("abc"))})
	r.chat("c1", "Jana", "single", m)
	r.one.missing = []wMissing{{Network: "whatsapp", ChatID: "c1", MessageID: "m1", Attachment: "mxc://example.org/a1"}, {Network: "whatsapp", ChatID: "c1", MessageID: "m1", Attachment: "loc-2"}}
	cycle(t, r.start())
	if string(r.one.files["mxc://example.org/a1"]) != "PNGxx" || string(r.one.files["loc-2"]) != "abc" {
		t.Errorf("uploaded: %q %q", r.one.files["mxc://example.org/a1"], r.one.files["loc-2"])
	}
	downloaded := 0
	for _, q := range r.beeper.reqs {
		if strings.HasPrefix(q, "POST /v1/assets/download") {
			downloaded++
		}
		if strings.Contains(q, "/assets/serve") {
			t.Errorf("an address Beeper does not document: %s", q)
		}
	}
	if downloaded != 1 {
		t.Errorf("the download call was made %d times", downloaded)
	}
}

func TestTheBridgeReportsAnEditAndADeletionAsVersions(t *testing.T) {
	r := newRig(t)
	r.chat("c1", "Jana", "single", r.msg("m1", "Ahoj"), r.msg("m2", "Čau"))
	b := r.start()
	cycle(t, b)
	if m := r.one.message("m1"); m["text"] != "Ahoj" || m["edited_at"] != nil || m["deleted"] != nil {
		t.Fatalf("first: %v", m)
	}
	// The text is edited: Beeper says so with editedTimestamp. The bridge looks at the newest pages again now and then.
	edited := r.msg("m1", "Ahoj!")
	edited.EditedTimestamp = "2026-11-19T11:30:00Z"
	r.chat("c1", "Jana", "single", edited, r.msg("m2", "Čau"))
	for i := 0; i < 16; i++ {
		cycle(t, b)
	}
	m := r.one.message("m1")
	if m["text"] != "Ahoj!" || m["edited_at"] != "2026-11-19T11:30:00Z" {
		t.Fatalf("the edit is not reported as a version: %v", m)
	}
	versions := 0
	for _, id := range r.one.messageIDs() {
		if id == "m1" {
			versions++
		}
	}
	if versions != 2 {
		t.Errorf("m1 went %d times, once as it was and once edited", versions)
	}
	// The message is deleted: isDeleted. It goes as a version marked deleted, with what Beeper still says.
	gone := r.msg("m2", "")
	gone.IsDeleted = true
	r.chat("c1", "Jana", "single", edited, gone)
	for i := 0; i < 15; i++ {
		cycle(t, b)
	}
	if m := r.one.message("m2"); m["deleted"] != true {
		t.Errorf("the deletion is not reported: %v", m)
	}
}

func TestAFileThatCannotBeFetchedIsReportedAsWaitingAndTriedAgainNotDropped(t *testing.T) {
	r := newRig(t)
	m := r.msg("m1", "Foto")
	m.Attachments = []bAttachment{{ID: "mxc://example.org/a1", FileName: "cabin.png", FileSize: 5, SrcURL: "mxc://example.org/a1"}}
	r.chat("c1", "Jana", "single", m)
	r.one.missing = []wMissing{{Network: "whatsapp", ChatID: "c1", MessageID: "m1", Attachment: "mxc://example.org/a1"}}
	b := r.start()
	cycle(t, b) // Beeper has no such file (yet)
	if len(r.one.files) != 0 {
		t.Fatal("a file that was not there was uploaded")
	}
	st, _ := loadState(r.cfg.StatePath)
	if len(st.Files) != 1 {
		t.Fatalf("the file is not kept as waiting: %+v", st.Files)
	}
	// ONE is told that a file waits, and why, with the next alive call.
	r.now = r.now.Add(2 * time.Minute)
	cycle(t, b)
	last := r.one.alives[len(r.one.alives)-1]
	if last["files_pending"].(float64) != 1 || last["files_note"] == "" {
		t.Errorf("alive: %v", last)
	}
	// The file appears; the bridge brings it by itself, with no new ask from ONE.
	r.beeper.media["mxc://example.org/a1"] = r.writeLocal("cabin.png", []byte("PNGxx"))
	r.now = r.now.Add(2 * time.Minute)
	cycle(t, b)
	if string(r.one.files["mxc://example.org/a1"]) != "PNGxx" {
		t.Errorf("a waiting file was not brought: %q", r.one.files)
	}
	r.now = r.now.Add(2 * time.Minute)
	cycle(t, b)
	if last := r.one.alives[len(r.one.alives)-1]; last["files_pending"].(float64) != 0 {
		t.Errorf("alive after: %v", last)
	}
	// A file bigger than ONE says it takes is not read.
	r2 := newRig(t)
	r2.one.maxFile = 3
	m2 := r2.msg("m1", "Foto")
	m2.Attachments = []bAttachment{{ID: "mxc://example.org/a1", FileName: "velke.png", FileSize: 5, SrcURL: "mxc://example.org/a1"}}
	r2.beeper.media["mxc://example.org/a1"] = r2.writeLocal("velke.png", []byte("PNGxx"))
	r2.chat("c1", "Jana", "single", m2)
	r2.one.missing = []wMissing{{Network: "whatsapp", ChatID: "c1", MessageID: "m1", Attachment: "mxc://example.org/a1"}}
	b2 := r2.start()
	cycle(t, b2)
	cycle(t, b2) // the cap is learned from ONE's first answer; the file waits, with the reason
	if len(r2.one.files) != 0 {
		t.Error("a file over the cap was uploaded")
	}
	if st, _ := loadState(r2.cfg.StatePath); len(st.Files) != 1 || !strings.Contains(st.Files["whatsapp|c1|m1|mxc://example.org/a1"].Reason, "larger") {
		t.Errorf("waiting: %+v", st.Files)
	}
}

func TestAFileFromAHiddenFolderIsNotRead(t *testing.T) {
	r := newRig(t)
	secret := r.writeLocal(".ssh/id", []byte("not for ONE"))
	m := r.msg("m1", "Foto")
	m.Attachments = []bAttachment{{ID: "loc", FileName: "x", FileSize: 1, SrcURL: "file://" + secret}}
	r.chat("c1", "Jana", "single", m)
	r.one.missing = []wMissing{{Network: "whatsapp", ChatID: "c1", MessageID: "m1", Attachment: "loc"}}
	cycle(t, r.start())
	if len(r.one.files) != 0 {
		t.Error("a file in a hidden folder was uploaded")
	}
}

func TestASendUsesTheChatIDItReturnsAndTreatsSendStatusAsDocumented(t *testing.T) {
	r := newRig(t)
	r.chat("room-merged", "Rodina", "group", r.msg("m1", "Ahoj"))
	cycle(t, r.start())
	r.one.replies = []wReply{{ID: 1, Network: "whatsapp", ChatID: "room-merged", Text: secretTxt}}
	cycle(t, r.start())
	got := false
	for _, q := range r.beeper.reqs {
		if q == "GET /v1/chats/room/messages/pending-1" {
			got = true
		}
		if strings.Contains(q, "room-merged/messages/pending") {
			t.Errorf("the follow-up went to the chat that was asked, not the one it went to: %s", q)
		}
	}
	if !got {
		t.Errorf("the pending message was not read in the chat it went to: %v", r.beeper.reqs)
	}
	if r.one.results["1"]["state"] != "sent" {
		t.Errorf("%v", r.one.results["1"])
	}

	// FAIL_PERMANENT: failed, with Beeper's reason, never sent again.
	r.beeper.status["pending-2"] = &bSendStatus{Status: "FAIL_PERMANENT", Reason: "the person blocked you"}
	r.one.replies = append(r.one.replies, wReply{ID: 2, Network: "whatsapp", ChatID: "room-merged", Text: "dva"})
	cycle(t, r.start())
	res := r.one.results["2"]
	if res["state"] != "failed" || !strings.Contains(res["error"].(string), "the person blocked you") {
		t.Errorf("a permanent failure: %v", res)
	}
	for i := 0; i < 2; i++ {
		cycle(t, r.start())
	}
	if len(r.beeper.sent) != 2 {
		t.Errorf("sent %d times", len(r.beeper.sent))
	}

	// PENDING and FAIL_RETRIABLE: still going out. Nothing is reported, nothing is sent again, until Beeper says.
	r.beeper.status["pending-3"] = &bSendStatus{Status: "PENDING"}
	r.one.replies = append(r.one.replies, wReply{ID: 3, Network: "whatsapp", ChatID: "room-merged", Text: "tri"})
	b := r.start()
	cycle(t, b)
	if _, said := r.one.results["3"]; said {
		t.Fatalf("a message still going out was reported: %v", r.one.results["3"])
	}
	r.beeper.status["pending-3"] = &bSendStatus{Status: "FAIL_RETRIABLE", Message: "no network"}
	cycle(t, b)
	cycle(t, r.start()) // a restart in between
	if _, said := r.one.results["3"]; said || len(r.beeper.sent) != 3 {
		t.Fatalf("retriable: reported %v, sent %d", r.one.results["3"], len(r.beeper.sent))
	}
	r.beeper.status["pending-3"] = &bSendStatus{Status: "SUCCESS"}
	cycle(t, r.start())
	if r.one.results["3"]["state"] != "sent" || len(r.beeper.sent) != 3 {
		t.Errorf("success: %v, sent %d", r.one.results["3"], len(r.beeper.sent))
	}

	// Still pending after half an hour: reported as in doubt, never sent again.
	r.beeper.status["pending-4"] = &bSendStatus{Status: "PENDING"}
	r.one.replies = append(r.one.replies, wReply{ID: 4, Network: "whatsapp", ChatID: "room-merged", Text: "ctyri"})
	cycle(t, r.start())
	r.now = r.now.Add(31 * time.Minute)
	cycle(t, r.start())
	if r.one.results["4"]["state"] != "unknown" || len(r.beeper.sent) != 4 {
		t.Errorf("long pending: %v, sent %d", r.one.results["4"], len(r.beeper.sent))
	}
}

func TestAMessageWhosePendingIdIsGoneAfterwardsCountsAsSentNotAsALostOne(t *testing.T) {
	r := newRig(t)
	r.chat("c1", "Jana", "single", r.msg("m1", "Ahoj"))
	cycle(t, r.start())
	r.beeper.gone["pending-1"] = true
	r.one.replies = []wReply{{ID: 1, Network: "whatsapp", ChatID: "c1", Text: "x"}}
	cycle(t, r.start())
	cycle(t, r.start())
	if r.one.results["1"]["state"] != "sent" || len(r.beeper.sent) != 1 {
		t.Errorf("%v sent %d", r.one.results["1"], len(r.beeper.sent))
	}
}

func (r *rig) post(id int64, group string, seq int, kind, text, file string, data []byte) wPost {
	p := wPost{ID: id, Group: group, Seq: seq, Network: "whatsapp", ChatID: "c1", Kind: kind, Text: text}
	if kind == "file" {
		p.File = &struct {
			Name string `json:"name"`
			Type string `json:"type"`
			Size int64  `json:"size"`
		}{Name: file, Type: map[string]string{"png": "image/png", "pdf": "application/pdf"}[file[len(file)-3:]], Size: int64(len(data))}
		r.one.postFiles[id] = data
	}
	return p
}

func TestWhatOwnerWritesGoesOutAsTheExactTextThenEachFileAsItsOwnMessageInOrder(t *testing.T) {
	r := newRig(t)
	r.chat("c1", "Jana", "single", r.msg("m1", "Ahoj"))
	cycle(t, r.start())
	r.one.posts = []wPost{
		r.post(1, "g1", 0, "text", "Dobrý den,\n  zítra v osm "+secretTxt, "", nil),
		r.post(2, "g1", 1, "file", "", "cabin.png", []byte("PNGdata")),
		r.post(3, "g1", 2, "file", "", "smlouva.pdf", []byte("%PDF-1.4 invented")),
	}
	b := r.start()
	cycle(t, b)
	if len(r.beeper.sent) != 3 {
		t.Fatalf("sent %d: %+v", len(r.beeper.sent), r.beeper.sent)
	}
	if s := r.beeper.sent[0]; s.Text != "Dobrý den,\n  zítra v osm "+secretTxt || s.UploadID != "" || s.Chat != "c1" {
		t.Errorf("the text: %+v", s)
	}
	if s := r.beeper.sent[1]; s.Text != "" || s.FileName != "cabin.png" || string(r.beeper.uploads[s.UploadID].Bytes) != "PNGdata" {
		t.Errorf("the first file: %+v", s)
	}
	if s := r.beeper.sent[2]; s.FileName != "smlouva.pdf" || string(r.beeper.uploads[s.UploadID].Bytes) != "%PDF-1.4 invented" {
		t.Errorf("the second file: %+v", s)
	}
	for _, id := range []string{"1", "2", "3"} {
		if r.one.pResults[id]["state"] != "sent" {
			t.Errorf("result %s: %v", id, r.one.pResults[id])
		}
	}
	// Once: not again, not after a restart, not even if ONE handed it out again.
	r.one.mu.Lock()
	r.one.handedP = map[int64]bool{}
	r.one.mu.Unlock()
	for i := 0; i < 2; i++ {
		cycle(t, b)
		cycle(t, r.start())
	}
	if len(r.beeper.sent) != 3 {
		t.Errorf("the messages were sent %d times in all", len(r.beeper.sent))
	}
}

func TestAFailureOnOneFileSaysWhichStepFailedAndTheMessagesAfterItAreNotSent(t *testing.T) {
	r := newRig(t)
	r.chat("c1", "Jana", "single", r.msg("m1", "Ahoj"))
	cycle(t, r.start())
	r.one.posts = []wPost{
		r.post(1, "g1", 0, "text", "text", "", nil),
		r.post(2, "g1", 1, "file", "", "cabin.png", []byte("PNGdata")),
		r.post(3, "g1", 2, "file", "", "smlouva.pdf", []byte("%PDF")),
	}
	delete(r.one.postFiles, 2) // ONE no longer has the first file
	cycle(t, r.start())
	if len(r.beeper.sent) != 1 || r.beeper.sent[0].Text != "text" {
		t.Fatalf("sent: %+v", r.beeper.sent)
	}
	if r.one.pResults["1"]["state"] != "sent" || r.one.pResults["2"]["state"] != "failed" || r.one.pResults["3"]["state"] != "failed" {
		t.Errorf("results: %v", r.one.pResults)
	}
	if e := r.one.pResults["3"]["error"].(string); !strings.Contains(e, "Not sent") {
		t.Errorf("the one after it says: %q", e)
	}
	// Another write in the same chat is not held back by it.
	r.one.posts = append(r.one.posts, r.post(4, "g2", 0, "text", "znovu", "", nil))
	cycle(t, r.start())
	if len(r.beeper.sent) != 2 || r.beeper.sent[1].Text != "znovu" {
		t.Errorf("the next write: %+v", r.beeper.sent)
	}
}

func TestAPostToAChatTheMacNeverSawIsNotSent(t *testing.T) {
	r := newRig(t)
	cycle(t, r.start())
	p := r.post(1, "g1", 0, "text", "x", "", nil)
	p.ChatID = "unknown"
	r.one.posts = []wPost{p}
	cycle(t, r.start())
	if len(r.beeper.sent) != 0 || r.one.pResults["1"]["state"] != "failed" {
		t.Errorf("%v %v", r.beeper.sent, r.one.pResults)
	}
}

func TestTheBridgeLogsNothingOfWhatOwnerWrote(t *testing.T) {
	r := newRig(t)
	r.chat("chat-secret-id", "Skupina ZXQ", "group", r.msg("m1", "Ahoj"))
	cycle(t, r.start())
	p := r.post(1, "g1", 0, "text", secretTxt, "", nil)
	p.ChatID = "chat-secret-id"
	f := r.post(2, "g1", 1, "file", "", "smlouva-ZXQ.pdf", []byte("pdf"))
	f.ChatID = "chat-secret-id"
	r.one.posts = []wPost{p, f}
	cycle(t, r.start())
	if len(r.beeper.sent) != 2 {
		t.Fatalf("sent %d", len(r.beeper.sent))
	}
	for _, bad := range []string{secretTxt, "ZXQ", "secret-id", tokBeeper, tokOne} {
		if strings.Contains(r.logs.String(), bad) {
			t.Errorf("the log holds %q:\n%s", bad, r.logs.String())
		}
	}
	if st, err := os.ReadFile(r.cfg.StatePath); err == nil && strings.Contains(string(st), secretTxt) {
		t.Error("the state file holds what the owner wrote")
	}
	_ = fmt.Sprint
}
