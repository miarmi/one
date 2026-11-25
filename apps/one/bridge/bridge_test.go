package main

import (
	"context"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var testCtx = context.Background()

func TestTheBridgeSendsEachMessageOnceAcrossARestartAndAnEditAgain(t *testing.T) {
	r := newRig(t)
	r.chat("c1", "Jana", "single", r.msg("m1", "Ahoj"), r.msg("m2", "Čau"))
	b := r.start()
	cycle(t, b)
	if ids := r.one.messageIDs(); len(ids) != 2 {
		t.Fatalf("first round: %v", ids)
	}
	// Nothing new: nothing is sent, even by a fresh program with the same state file.
	for i := 0; i < 2; i++ {
		b = r.start()
		cycle(t, b)
		cycle(t, b)
	}
	if ids := r.one.messageIDs(); len(ids) != 2 {
		t.Fatalf("after restarts: %v", ids)
	}
	// A new message goes once; the old ones do not go with it.
	r.now = r.now.Add(time.Minute)
	r.chat("c1", "Jana", "single", r.msg("m1", "Ahoj"), r.msg("m2", "Čau"), r.msg("m3", "Nové"))
	b = r.start()
	cycle(t, b)
	cycle(t, r.start())
	if ids := r.one.messageIDs(); strings.Join(ids, ",") != "m1,m2,m3" {
		t.Fatalf("after a new message: %v", ids)
	}
	// An edit is a changed message and goes again, alone.
	r.now = r.now.Add(time.Minute)
	r.chat("c1", "Jana", "single", r.msg("m1", "Ahoj!"), r.msg("m2", "Čau"), r.msg("m3", "Nové"))
	cycle(t, r.start())
	if ids := r.one.messageIDs(); strings.Join(ids, ",") != "m1,m2,m3,m1" {
		t.Fatalf("after an edit: %v", ids)
	}
	if st, _ := os.Stat(r.cfg.StatePath); st.Mode().Perm() != 0o600 {
		t.Errorf("the state file has mode %v", st.Mode().Perm())
	}
}

func TestTheBridgeKeepsItsPlaceOnlyWhenONETookTheBatch(t *testing.T) {
	r := newRig(t)
	r.chat("c1", "Jana", "single", r.msg("m1", "Ahoj"))
	r.cfg.OneToken = "wrong-token-EEEE5555FFFF6666"
	b := r.start()
	if err := b.Cycle(context.Background()); err == nil {
		t.Fatal("a refused token is not an error")
	}
	r.cfg.OneToken = tokOne
	cycle(t, r.start())
	if ids := r.one.messageIDs(); len(ids) != 1 {
		t.Errorf("the message was lost or doubled: %v", ids)
	}
}

func TestTheBridgeRefusesABeeperAddressThatIsNotThisMac(t *testing.T) {
	r := newRig(t)
	for _, bad := range []string{"http://example.com:23373", "http://192.168.1.20:23373", "https://beeper.example.org", "http://localhost.example.com:23373", "http://user:pw@localhost:23373", "ftp://localhost"} {
		cfg := r.cfg
		cfg.BeeperURL = bad
		if err := cfg.Validate(); err == nil {
			t.Errorf("accepted the Beeper address %q", bad)
		}
		if _, err := NewBridge(cfg, log.New(io.Discard, "", 0)); err == nil {
			t.Errorf("started with the Beeper address %q", bad)
		}
	}
	for _, good := range []string{"http://localhost:23373", "http://127.0.0.1:23373", "http://[::1]:23373"} {
		cfg := r.cfg
		cfg.BeeperURL = good
		if err := cfg.Validate(); err != nil {
			t.Errorf("refused %q: %v", good, err)
		}
	}
	// ONE must be https, apart from this Mac for tests, and only an address.
	for _, bad := range []string{"http://one.example.org", "https://one.example.org/path", "https://u:p@one.example.org", "https://one.example.org?x=1", ""} {
		cfg := r.cfg
		cfg.OneURL = bad
		if err := cfg.Validate(); err == nil {
			t.Errorf("accepted the ONE address %q", bad)
		}
	}
	cfg := r.cfg
	cfg.OneURL = "https://one.example.org"
	if err := cfg.Validate(); err != nil {
		t.Error(err)
	}
}

func TestTheBridgeDoesNotFollowARedirectToAnotherAddress(t *testing.T) {
	r := newRig(t)
	elsewhere := 0
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { elsewhere++ }))
	defer other.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		http.Redirect(w, req, other.URL+req.URL.Path, http.StatusTemporaryRedirect)
	}))
	defer redirect.Close()
	r.cfg.OneURL = redirect.URL
	r.chat("c1", "Jana", "single", r.msg("m1", "Ahoj"))
	if err := r.start().Cycle(context.Background()); err == nil {
		t.Error("a redirect counted as an answer")
	}
	if elsewhere != 0 {
		t.Error("the bridge followed a redirect and sent a token elsewhere")
	}
}

func TestTokensComeFromOwnerOnlyFilesOrTheEnvironmentNeverTheCommandLine(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "tok")
	os.WriteFile(f, []byte("file-token-123\n"), 0o644)
	t.Setenv("BEEPER_TOKEN", "")
	t.Setenv("BEEPER_TOKEN_FILE", f)
	if _, err := tokenFrom("BEEPER_TOKEN", "BEEPER_TOKEN_FILE", ""); err == nil {
		t.Error("a token file that others can read was accepted")
	}
	os.Chmod(f, 0o600)
	if v, err := tokenFrom("BEEPER_TOKEN", "BEEPER_TOKEN_FILE", ""); err != nil || v != "file-token-123" {
		t.Errorf("%q %v", v, err)
	}
	t.Setenv("BEEPER_TOKEN", "env-token-9")
	if v, _ := tokenFrom("BEEPER_TOKEN", "BEEPER_TOKEN_FILE", ""); v != "env-token-9" {
		t.Errorf("the environment did not win: %q", v)
	}
	// The program has one flag, and it is not a token.
	src, _ := os.ReadFile("main.go")
	if strings.Contains(string(src), "flag.String") || strings.Contains(string(src), "os.Args") || strings.Count(string(src), "flag.Bool") != 1 {
		t.Error("the program takes something else on the command line")
	}
}

func TestAReplyIsSentOnceReportedOnceAndNeverSentAgainAfterARestart(t *testing.T) {
	r := newRig(t)
	r.chat("c1", "Jana", "single", r.msg("m1", "Ahoj"))
	cycle(t, r.start())
	r.one.replies = []wReply{{ID: 7, Network: "whatsapp", ChatID: "c1", Text: secretTxt}}
	r.one.reportErr = true // ONE cannot be told yet
	b := r.start()
	if err := b.Cycle(context.Background()); err == nil {
		t.Fatal("a report ONE did not take is not an error")
	}
	if len(r.beeper.sent) != 1 || r.beeper.sent[0].Text != secretTxt || r.beeper.sent[0].Chat != "c1" {
		t.Fatalf("sent: %v", r.beeper.sent)
	}
	// Restart before ONE heard: ONE will not hand it out again; the bridge reports what it has and sends nothing.
	r.one.reportErr = false
	b = r.start()
	cycle(t, b)
	cycle(t, r.start())
	if len(r.beeper.sent) != 1 {
		t.Errorf("the reply was sent %d times", len(r.beeper.sent))
	}
	res := r.one.results["7"]
	if res == nil || res["state"] != "sent" {
		t.Fatalf("reported: %v", r.one.results)
	}
	// Even if ONE handed it out again, it is not sent again.
	r.one.mu.Lock()
	r.one.handed = map[int64]bool{}
	r.one.mu.Unlock()
	cycle(t, r.start())
	if len(r.beeper.sent) != 1 {
		t.Errorf("a reply handed out twice was sent %d times", len(r.beeper.sent))
	}
}

func TestAReplyThatBeeperRefusedIsReportedFailedAndOneThatDiedMidSendIsInDoubt(t *testing.T) {
	r := newRig(t)
	r.chat("c1", "Jana", "single", r.msg("m1", "Ahoj"))
	cycle(t, r.start())
	r.beeper.sendFail = 404
	r.one.replies = []wReply{{ID: 1, Network: "whatsapp", ChatID: "c1", Text: secretTxt}, {ID: 2, Network: "whatsapp", ChatID: "zz", Text: "x"}}
	cycle(t, r.start())
	if r.one.results["1"]["state"] != "failed" || !strings.Contains(r.one.results["1"]["error"].(string), "Beeper refused") {
		t.Errorf("%v", r.one.results["1"])
	}
	if r.one.results["2"]["state"] != "failed" || len(r.beeper.sent) != 0 {
		t.Errorf("a chat the Mac never saw: %v", r.one.results["2"])
	}
	// A crash between the note and the send leaves "sending" on disk: reported as in doubt, never sent.
	b := r.start()
	b.st.Replies["3"] = &replyState{State: "sending"}
	b.st.save()
	r.beeper.sendFail = 0
	cycle(t, r.start())
	if r.one.results["3"]["state"] != "unknown" || len(r.beeper.sent) != 0 {
		t.Errorf("%v sent=%v", r.one.results["3"], r.beeper.sent)
	}
}

func TestImportAgainBringsBackJustTheMessageAskedFor(t *testing.T) {
	r := newRig(t)
	r.chat("c1", "Rodina", "group", r.msg("m1", "Jedna"), r.msg("m2", "Dvě"))
	cycle(t, r.start())
	r.one.resend = []wMissing{{Network: "whatsapp", ChatID: "c1", MessageID: "m2"}}
	cycle(t, r.start())
	ids := r.one.messageIDs()
	if strings.Join(ids, ",") != "m1,m2,m2" {
		t.Fatalf("%v", ids)
	}
	last := r.one.batches[len(r.one.batches)-1]
	if last["again"] != true {
		t.Error("the batch is not marked as asked for")
	}
}

func TestTheHeartbeatSaysWhichNetworksAreThereAndTheChatShapeIsRight(t *testing.T) {
	r := newRig(t)
	r.chat("c1", "Rodina", "group", r.msg("m1", "Jedna"))
	cycle(t, r.start())
	if r.one.alive != 1 {
		t.Errorf("alive %d", r.one.alive)
	}
	c := r.one.batches[0]["chats"].([]any)[0].(map[string]any)
	if c["type"] != "group" || c["network"] != "whatsapp" || c["name"] != "Rodina" {
		t.Errorf("%v", c)
	}
	if len(c["participants"].([]any)) != 2 {
		t.Errorf("%v", c["participants"])
	}
	if n := networkName(bAccount{AccountID: "facebookgo", Network: "Facebook/Messenger"}); n != "messenger" {
		t.Errorf("%q", n)
	}
	// Beeper down: the bridge still says it is alive, and says what is wrong.
	r.beeper.srv.Close()
	r.now = r.now.Add(2 * time.Minute)
	if err := r.start().Cycle(context.Background()); err == nil {
		t.Error("Beeper being down is not an error")
	}
	if r.one.alive != 2 {
		t.Errorf("the bridge did not say it was alive while Beeper was down: %d", r.one.alive)
	}
}

func TestTheBridgeLogsNoTextNoNameNoChatIdAndNoToken(t *testing.T) {
	r := newRig(t)
	m := r.msg("m-secret-id", secretTxt)
	m.SenderName = "Jana Tajná"
	local := r.writeLocal("doc.pdf", []byte("pdf"))
	m.Attachments = []bAttachment{{ID: "mxc://example.org/a1", FileName: "smlouva-ZXQ.pdf", FileSize: 3, SrcURL: "mxc://example.org/a1"}}
	r.chat("chat-secret-id", "Skupina ZXQ", "group", m)
	r.beeper.media["mxc://example.org/a1"] = local
	r.one.missing = []wMissing{{Network: "whatsapp", ChatID: "chat-secret-id", MessageID: "m-secret-id", Attachment: "mxc://example.org/a1"}}
	cycle(t, r.start())
	r.one.replies = []wReply{{ID: 9, Network: "whatsapp", ChatID: "chat-secret-id", Text: secretTxt}}
	cycle(t, r.start())
	r.beeper.sendFail = 500
	r.one.replies = []wReply{{ID: 10, Network: "whatsapp", ChatID: "chat-secret-id", Text: secretTxt}}
	cycle(t, r.start())
	r.cfg.OneToken = "wrong-token-EEEE5555FFFF6666"
	r.start().Cycle(context.Background())
	r.cfg.BeeperToken = "wrong-beeper-GGGG7777"
	r.cfg.OneToken = tokOne
	r.start().Cycle(context.Background())
	if r.logs.Len() == 0 {
		t.Fatal("the bridge logs nothing at all")
	}
	for _, bad := range []string{secretTxt, "ZXQ", "Tajná", "secret-id", tokBeeper, tokOne, "wrong-token", "wrong-beeper", r.one.srv.URL, r.beeper.srv.URL, "127.0.0.1"} {
		if strings.Contains(r.logs.String(), bad) {
			t.Errorf("the log holds %q:\n%s", bad, r.logs.String())
		}
	}
}

func TestNothingIsAskedOfBeeperThatItsDocumentationDoesNotHave(t *testing.T) {
	// The fake Beeper fails the test on any address, parameter or body its
	// documentation does not have, so a full round with every feature proves it.
	r := newRig(t)
	m := r.msg("m1", "Foto")
	local := r.writeLocal("x.png", []byte("x"))
	m.Attachments = []bAttachment{{ID: "mxc://example.org/x", FileSize: 1, SrcURL: "mxc://example.org/x", MimeType: "image/png", FileName: "x.png"}}
	r.beeper.media["mxc://example.org/x"] = local
	r.chat("c1", "Jana", "single", m)
	r.one.missing = []wMissing{{Network: "whatsapp", ChatID: "c1", MessageID: "m1", Attachment: "mxc://example.org/x"}}
	r.one.replies = []wReply{{ID: 1, Network: "whatsapp", ChatID: "c1", Text: "ok"}}
	r.one.resend = []wMissing{{Network: "whatsapp", ChatID: "c1", MessageID: "m1"}}
	cycle(t, r.start())
	src, _ := os.ReadFile("beeper.go")
	for _, bad := range []string{"/v1/oauth", "/v1/focus", "/v1/info", "/v1/assets/serve", "limit="} {
		if strings.Contains(string(src), bad) {
			t.Errorf("the adapter mentions %q", bad)
		}
	}
}

// Beeper answering 401 or 403 to the accounts call is told as a refused
// token, not as Beeper being down; any other failure keeps its old words.
func TestBeeperRefusingTheTokenIsToldAsThat(t *testing.T) {
	r := newRig(t)
	r.cfg.BeeperToken = "wrong-token" // the fake Beeper answers 401 to it
	r.now = r.now.Add(2 * time.Minute)
	r.start().Cycle(context.Background())
	if len(r.one.alives) != 1 {
		t.Fatalf("%d alive calls", len(r.one.alives))
	}
	if got := r.one.alives[0]["last_error"]; got != "Beeper does not accept the token the bridge uses." {
		t.Errorf("last_error %q", got)
	}
	if !strings.Contains(r.logs.String(), "beeper refuses the token") || strings.Contains(r.logs.String(), "not answering") {
		t.Errorf("log %q", r.logs.String())
	}
	// Beeper down keeps today's words.
	r2 := newRig(t)
	r2.beeper.srv.Close()
	r2.start().Cycle(context.Background())
	if got := r2.one.alives[0]["last_error"]; got != "Beeper is not answering on the Mac." {
		t.Errorf("last_error %q", got)
	}
	if !strings.Contains(r2.logs.String(), "beeper not answering") {
		t.Errorf("log %q", r2.logs.String())
	}
}
