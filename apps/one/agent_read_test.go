package main

import (
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	"lab/apps/one/repo"
)

// Tests of what an agent reads: the review queue in pages, an item as text,
// and the summary of a short thread (SPEC 11, 29). All data is invented.

const oakLine = "Hello the owner, the delivery of the invented oak boards is planned for Tuesday morning at the yard."

func (e *agentEnv) messageIDs(ref string) map[string]float64 {
	e.t.Helper()
	out := map[string]float64{}
	for _, m := range e.getJSON("/api/items/" + ref + "?all=1")["messages"].([]any) {
		mm := m.(map[string]any)
		out[mm["subject"].(string)] = mm["message_id"].(float64)
	}
	return out
}

func (e *agentEnv) textOf(ref, subject string) (text string, msg map[string]any) {
	e.t.Helper()
	for _, m := range e.getJSON("/api/items/" + ref + "?all=1")["messages"].([]any) {
		mm := m.(map[string]any)
		if mm["subject"] == subject {
			return mm["text"].(string), mm
		}
	}
	e.t.Fatalf("no message %q", subject)
	return "", nil
}

// TestReviewQueuePagesWithAStableOrderTrueTotalAndShortCleanPreviews
func TestReviewQueuePagesWithAStableOrderTrueTotalAndShortCleanPreviews(t *testing.T) {
	e := newAgentEnv(t)
	html := `<html><head><style>p{color:red}</style></head><body><div style="x">Dear the owner,&nbsp;the   invented parcel</div><p>` +
		strings.Repeat("is on its way and will arrive soon. ", 40) + `</p><script>alert(1)</script></body></html>`
	var items []repo.Item
	for i := 0; i < 7; i++ {
		// The oldest message first: item 0 is first in the queue.
		m := e.mail(fmt.Sprintf("Parcel %d", i), float64(20-i))
		m.text = html
		items = append(items, e.seed(stageNew, m))
		it := items[i]
		it.Hidden, it.NeedsClaude = true, true
		if err := e.a.store.SaveItem(e.ctx, it); err != nil {
			t.Fatal(err)
		}
	}
	ids := func(q map[string]any) []string {
		var out []string
		for _, x := range q["items"].([]any) {
			out = append(out, x.(map[string]any)["id"].(string))
		}
		return out
	}
	p1 := e.getJSON("/api/queue?limit=3")
	if p1["waiting"] != float64(7) || p1["returned"] != float64(3) || p1["more"] != true || p1["next_after"] != ref(items[2]) {
		t.Fatalf("page 1: waiting=%v returned=%v more=%v next=%v", p1["waiting"], p1["returned"], p1["more"], p1["next_after"])
	}
	if got := ids(p1); strings.Join(got, ",") != ref(items[0])+","+ref(items[1])+","+ref(items[2]) {
		t.Fatalf("page 1 order: %v", got)
	}
	// Items of the first page leave the queue, even the one the next page starts after.
	for _, it := range items[:3] {
		e.post("/api/items/"+ref(it)+"/seen", nil)
	}
	p2 := e.getJSON("/api/queue?limit=3&after=" + ref(items[2]))
	if p2["waiting"] != float64(4) || p2["more"] != true || p2["next_after"] != ref(items[5]) {
		t.Fatalf("page 2: %v", p2)
	}
	if got := ids(p2); strings.Join(got, ",") != ref(items[3])+","+ref(items[4])+","+ref(items[5]) {
		t.Fatalf("page 2 order: %v", got)
	}
	p3 := e.getJSON("/api/queue?limit=3&after=" + ref(items[5]))
	if p3["more"] != false || p3["returned"] != float64(1) || p3["next_after"] != nil || ids(p3)[0] != ref(items[6]) {
		t.Fatalf("page 3: %v", p3)
	}
	// The default and the largest page.
	if all := e.getJSON("/api/queue"); all["returned"] != float64(4) || all["waiting"] != float64(4) {
		t.Fatalf("default page: %v", all["returned"])
	}
	if defaultQueuePage != 50 || maxQueuePage != 100 {
		t.Fatal("the page sizes changed")
	}
	if rec := e.api("GET", "/api/queue?after=Q-9999", nil); rec.Code == 200 {
		t.Fatal("an unknown after was accepted")
	}
	// Previews are short, clean text on one line.
	for _, x := range p2["items"].([]any) {
		pv := x.(map[string]any)["preview"].(string)
		if utf8.RuneCountInString(pv) > 121 || len(pv) < 40 || strings.ContainsAny(pv, "<>\n") || strings.Contains(pv, "alert") || strings.Contains(pv, "color:red") || strings.Contains(pv, "  ") {
			t.Fatalf("preview %q", pv)
		}
		if !strings.HasPrefix(pv, "Dear the owner, the invented parcel") {
			t.Fatalf("preview %q", pv)
		}
	}
}

// TestAgentReadsAnItemAsTextWithoutQuotes: no markup, quotes the thread holds
// cut with a marker line, a quote the thread does not hold kept.
func TestAgentReadsAnItemAsTextWithoutQuotes(t *testing.T) {
	e := newAgentEnv(t)
	orig := e.mail("original", 9)
	orig.text = oakLine
	gmailReply := e.mail("gmail reply", 8)
	gmailReply.text = `<html><body><div>Thanks, Tuesday is fine.</div><div>On Mon, 2 Nov 2026 at 10:00, Alice Example &lt;alice@example.test&gt; wrote:</div>` +
		`<blockquote><div>` + oakLine + `</div></blockquote></body></html>`
	outlook := e.mail("outlook reply", 7)
	outlook.text = "Please send the invoice too.\n\n________________________________\nFrom: Alice Example <alice@example.test>\nSent: Monday, 2 November 2026 10:00\nTo: the owner\nSubject: original\n\n" + oakLine + "\n\nBest regards"
	marked := e.mail("marked reply", 6)
	marked.text = "Inline answer first.\n> " + oakLine + "\nSecond answer."
	czech := e.mail("czech reply", 5)
	czech.text = "Dobře, díky.\n\nDne po 2. 11. 2026 v 10:00 Alice Example <alice@example.test> napsal(a):\n> " + oakLine
	unknown := e.mail("unknown quote", 4)
	unknown.text = "I agree.\n> A line from some other thread that nobody here has ever seen before today."
	it := e.seed(stageNew, orig, gmailReply, outlook, marked, czech, unknown)
	r := ref(it)

	for _, subj := range []string{"gmail reply", "outlook reply", "marked reply", "czech reply"} {
		text, msg := e.textOf(r, subj)
		if strings.Contains(text, "oak boards") || strings.Contains(text, "<div") || strings.Contains(text, "<alice") || strings.Contains(text, "From:") || strings.Contains(text, "wrote:") || strings.Contains(text, "napsal") {
			t.Errorf("%s: %q", subj, text)
		}
		if !strings.Contains(text, quoteMarker) || strings.Count(text, quoteMarker) != 1 || msg["quote_left_out"] != true {
			t.Errorf("%s: no single marker line: %q", subj, text)
		}
	}
	if text, _ := e.textOf(r, "gmail reply"); text != "Thanks, Tuesday is fine.\n"+quoteMarker {
		t.Errorf("gmail reply: %q", text)
	}
	if text, _ := e.textOf(r, "outlook reply"); text != "Please send the invoice too.\n"+quoteMarker {
		t.Errorf("outlook reply: %q", text)
	}
	if text, _ := e.textOf(r, "marked reply"); text != "Inline answer first.\n"+quoteMarker+"\nSecond answer." {
		t.Errorf("marked reply: %q", text)
	}
	if text, msg := e.textOf(r, "original"); text != oakLine || msg["quote_left_out"] != false {
		t.Errorf("the original changed: %q", text)
	}
	if text, msg := e.textOf(r, "unknown quote"); !strings.Contains(text, "nobody here has ever seen") || strings.Contains(text, quoteMarker) || msg["quote_left_out"] != false {
		t.Errorf("a quote that is not in the thread was cut: %q", text)
	}
	// One message in full: quotes and all, as text.
	id := e.messageIDs(r)["gmail reply"]
	full := e.getJSON(fmt.Sprintf("/api/items/%s?message=%d", r, int64(id)))
	msgs := full["messages"].([]any)
	if len(msgs) != 1 {
		t.Fatalf("one message wanted: %v", full)
	}
	ft := msgs[0].(map[string]any)["text"].(string)
	if !strings.Contains(ft, "oak boards") || strings.Contains(ft, "<div") || strings.Contains(ft, "<html") || strings.Contains(ft, quoteMarker) {
		t.Errorf("message in full: %q", ft)
	}
	if rec := e.api("GET", fmt.Sprintf("/api/items/%s?message=%d", r, int64(id)+99999), nil); rec.Code != 404 {
		t.Errorf("a message of no item: %d", rec.Code)
	}
}

// TestAgentReadCapsLongTexts: a long text is cut with a line that says how
// much was left out, and the whole of it can be fetched.
func TestAgentReadCapsLongTexts(t *testing.T) {
	e := newAgentEnv(t)
	long := e.mail("long", 3)
	long.text = strings.Repeat("abcdefghi ", 900) // 9,000 characters
	it := e.seed(stageNew, long)
	r := ref(it)
	text, msg := e.textOf(r, "long")
	id := int64(msg["message_id"].(float64))
	// The cut falls on a space that is trimmed, so allow for it.
	idx := strings.LastIndex(text, "\n[")
	if idx < 0 || msg["text_cut"] != true {
		t.Fatalf("no marker line: ...%q", text[max(0, len(text)-120):])
	}
	body, line := text[:idx], text[idx+1:]
	if n := utf8.RuneCountInString(body); n > agentTextCap || n < agentTextCap-2 {
		t.Fatalf("kept %d characters", n)
	}
	var left int
	if _, err := fmt.Sscanf(line, "[%d more characters left out; read this message in full with message %d]", &left, new(int64)); err != nil || left < 4990 || left > 5001 || !strings.HasSuffix(line, fmt.Sprintf("with message %d]", id)) {
		t.Fatalf("marker line %q", line)
	}
	full := e.getJSON(fmt.Sprintf("/api/items/%s?message=%d", r, id))["messages"].([]any)[0].(map[string]any)
	if got := full["text"].(string); utf8.RuneCountInString(got) < 8999 || strings.Contains(got, "left out") || full["text_cut"] != false {
		t.Fatalf("not in full: %d characters", len(got))
	}
}

// TestSummaryOnAShortThreadIsRefusedAndOnALongOneAccepted: markup, quotes and
// white space do not make a short thread long.
func TestSummaryOnAShortThreadIsRefusedAndOnALongOneAccepted(t *testing.T) {
	e := newAgentEnv(t)
	summary := func(it repo.Item) map[string]any {
		return e.post("/api/items/"+ref(it)+"/comments", map[string]any{"text": "A summary.", "kind": "summary"})
	}
	// Little text under a lot of markup.
	m := e.mail("Newsletter", 2)
	m.text = `<html><head><style>` + strings.Repeat(".a{color:red} ", 60) + `</style></head><body>` +
		strings.Repeat(`<div style="padding:0;margin:0"><span class="x">&nbsp;</span></div>`, 20) + `<p>Hello, a short note.</p></body></html>`
	if len(m.text) < 1000 {
		t.Fatal("the invented mail is not long enough")
	}
	html := e.seed(stageNew, m)
	if got := summary(html); got["_status"] != float64(409) || len(e.events(html.ID, "commented")) != 0 {
		t.Fatalf("summary on markup: %v", got)
	}
	// A short reply under a long quote of a short message.
	o := e.mail("first", 5)
	o.text = oakLine[:60]
	r := e.mail("second", 4)
	r.text = "OK.\n> " + oakLine[:60]
	quoted := e.seed(stageNew, o, r)
	if got := summary(quoted); got["_status"] != float64(409) {
		t.Fatalf("summary on a quote: %v", got)
	}
	// Plenty of white space is not text either.
	sp := e.mail("spaces", 2)
	sp.text = "Hi.\n" + strings.Repeat("   \n\t  \n", 100)
	if got := summary(e.seed(stageNew, sp)); got["_status"] != float64(409) {
		t.Fatalf("summary on white space: %v", got)
	}
	// 300 characters is still short; 301 is not.
	edge := e.mail("edge", 2)
	edge.text = strings.Repeat("x", 300)
	if got := summary(e.seed(stageNew, edge)); got["_status"] != float64(409) {
		t.Fatalf("summary on 300 characters: %v", got)
	}
	edge2 := e.mail("edge2", 2)
	edge2.text = strings.Repeat("x", 301)
	if got := summary(e.seed(stageNew, edge2)); got["ok"] != true {
		t.Fatalf("summary on 301 characters: %v", got)
	}
	// Two short messages that add up to more than 300 characters count together.
	a, b := e.mail("a", 3), e.mail("b", 2)
	a.text, b.text = strings.Repeat("y", 200), strings.Repeat("z", 200)
	if got := summary(e.seed(stageNew, a, b)); got["ok"] != true {
		t.Fatalf("summary on a long thread: %v", got)
	}
}

// TestMCPQueueAndItemTakeTheNewArguments: the tools pass limit, after and
// message on to the same addresses.
func TestMCPQueueAndItemTakeTheNewArguments(t *testing.T) {
	e := newAgentEnv(t)
	for i := 0; i < 3; i++ {
		e.newItem(stageNew, true, fmt.Sprintf("Waiting %d", i), "")
	}
	text, isErr, known := e.a.callTool(e.ctx, "list_review_queue", []byte(`{"limit": 2}`))
	if !known || isErr || !strings.Contains(text, `"waiting":3`) || !strings.Contains(text, `"more":true`) || !strings.Contains(text, `"next_after"`) {
		t.Fatalf("queue tool: %v %s", isErr, text)
	}
	if _, isErr, _ := e.a.callTool(e.ctx, "list_review_queue", []byte(`{"after": "nonsense"}`)); !isErr {
		t.Fatal("a bad after was accepted")
	}
	it := e.newItem(stageNew, false, "One", "")
	id := e.messageIDs(ref(it))["One"]
	text, isErr, _ = e.a.callTool(e.ctx, "get_item", []byte(fmt.Sprintf(`{"id": %q, "message": %d}`, ref(it), int64(id))))
	if isErr || !strings.Contains(text, "A long invented paragraph") || strings.Contains(text, `"thread"`) {
		t.Fatalf("get_item message: %v %.200s", isErr, text)
	}
}
