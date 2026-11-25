package main

import (
	"archive/zip"
	"bytes"
	"encoding/base64"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"strings"
	"testing"

	"lab/apps/one/repo"
)

// Tests of get_file and list_items (SPEC 29). Every file is built in the
// test from a few lines; no real document is committed.

// tinyPDF builds a valid PDF whose pages show the given lines (an empty line
// is a page without text, like a scan).
func tinyPDF(pages ...string) []byte {
	var objs []string
	kids := ""
	for i := range pages {
		kids += fmt.Sprintf("%d 0 R ", 4+2*i)
	}
	objs = append(objs, "<< /Type /Catalog /Pages 2 0 R >>",
		fmt.Sprintf("<< /Type /Pages /Kids [%s] /Count %d >>", kids, len(pages)),
		"<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica /Encoding /WinAnsiEncoding >>")
	for i, p := range pages {
		objs = append(objs, fmt.Sprintf("<< /Type /Page /Parent 2 0 R /MediaBox [0 0 300 300] /Contents %d 0 R /Resources << /Font << /F1 3 0 R >> >> >>", 5+2*i))
		stream := "0 0 m 10 10 l S"
		if p != "" {
			stream = fmt.Sprintf("BT /F1 12 Tf 20 200 Td (%s) Tj ET", p)
		}
		objs = append(objs, fmt.Sprintf("<< /Length %d >>\nstream\n%s\nendstream", len(stream), stream))
	}
	return assemblePDF(objs, "")
}

func assemblePDF(objs []string, pagesOverride string) []byte {
	var b bytes.Buffer
	b.WriteString("%PDF-1.4\n")
	var off []int
	for i, o := range objs {
		off = append(off, b.Len())
		fmt.Fprintf(&b, "%d 0 obj\n%s\nendobj\n", i+1, o)
	}
	x := b.Len()
	fmt.Fprintf(&b, "xref\n0 %d\n0000000000 65535 f \n", len(objs)+1)
	for _, o := range off {
		fmt.Fprintf(&b, "%010d 00000 n \n", o)
	}
	fmt.Fprintf(&b, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(objs)+1, x)
	return b.Bytes()
}

func zipOf(files map[string]string) []byte {
	var b bytes.Buffer
	zw := zip.NewWriter(&b)
	for n, c := range files {
		w, _ := zw.Create(n)
		w.Write([]byte(c))
	}
	zw.Close()
	return b.Bytes()
}

func tinyDocx(paras ...string) []byte {
	body := ""
	for _, p := range paras {
		body += "<w:p><w:r><w:t>" + p + "</w:t></w:r></w:p>"
	}
	return zipOf(map[string]string{"word/document.xml": `<?xml version="1.0"?><w:document xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main"><w:body>` + body + `</w:body></w:document>`})
}

func tinyXlsx(rows int) []byte {
	var sh strings.Builder
	sh.WriteString(`<worksheet xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main"><sheetData>`)
	sh.WriteString(`<row r="1"><c r="A1" t="s"><v>0</v></c><c r="B1" t="s"><v>1</v></c></row>`)
	for i := 2; i <= rows; i++ {
		fmt.Fprintf(&sh, `<row r="%d"><c r="A%d"><v>%d</v></c><c r="C%d" t="inlineStr"><is><t>row %d</t></is></c></row>`, i, i, i*10, i, i)
	}
	sh.WriteString(`</sheetData></worksheet>`)
	return zipOf(map[string]string{
		"xl/workbook.xml":            `<workbook xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main" xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships"><sheets><sheet name="Costs" sheetId="1" r:id="rId1"/></sheets></workbook>`,
		"xl/_rels/workbook.xml.rels": `<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships"><Relationship Id="rId1" Target="worksheets/sheet1.xml"/></Relationships>`,
		"xl/sharedStrings.xml":       `<sst xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main"><si><t>Item</t></si><si><t>Price</t></si></sst>`,
		"xl/worksheets/sheet1.xml":   sh.String(),
	})
}

func bigPNG(w, h int) []byte {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{uint8(x), uint8(y), uint8(x ^ y), 255})
		}
	}
	var b bytes.Buffer
	png.Encode(&b, img)
	return b.Bytes()
}

// itemWithFiles makes an item and saves the files on its first message.
func (e *agentEnv) itemWithFiles(stage string, files ...[3]any) (repo.Item, []int64) {
	e.t.Helper()
	it := e.seed(stage, e.mail("With files", 1))
	src := fmt.Sprint("m", seedN.Load(), "-0")
	var ids []int64
	for _, f := range files {
		ids = append(ids, e.saveFile(it, src, f[0].(string), f[1].(string), "attachment", f[2].([]byte)))
	}
	return it, ids
}

func (e *agentEnv) fileJSON(it repo.Item, query string) map[string]any {
	e.t.Helper()
	return e.getJSON("/api/items/" + ref(it) + "/file?" + query)
}

// TestGetFileReadsATextPDFPageByPage: the words come back by page with the page count.
func TestGetFileReadsATextPDFPageByPage(t *testing.T) {
	e := newAgentEnv(t)
	it, ids := e.itemWithFiles(stageNew, [3]any{"summons.pdf", "application/pdf", tinyPDF("Hearing on 14 December 2026", "Second page about costs")})
	m := e.fileJSON(it, fmt.Sprintf("file_id=%d", ids[0]))
	text, _ := m["text"].(string)
	if m["readable"] != true || m["pages"] != float64(2) || !strings.Contains(text, "[page 1]\nHearing on 14 December 2026") || !strings.Contains(text, "[page 2]\nSecond page about costs") {
		t.Fatalf("pdf: %v", m)
	}
	if m["note"] != fileDataNote || m["pages_shown"] != "1-2" {
		t.Errorf("notice or pages: %v %v", m["note"], m["pages_shown"])
	}
	m = e.fileJSON(it, fmt.Sprintf("file_id=%d&page=2", ids[0]))
	if text, _ = m["text"].(string); strings.Contains(text, "Hearing") || !strings.Contains(text, "[page 2]") {
		t.Errorf("from page 2: %q", text)
	}
	// By name, through the tool.
	out, bad := e.tool("get_file", map[string]any{"id": ref(it), "file": "summons.pdf"})
	if bad || !strings.Contains(out, "Hearing on 14 December 2026") {
		t.Errorf("tool: %v %s", bad, out)
	}
	// get_item lists the file with its file_id.
	files := e.getJSON("/api/items/" + ref(it))["files"].([]any)
	if files[0].(map[string]any)["file_id"] != float64(ids[0]) {
		t.Errorf("files: %v", files)
	}
}

// TestGetFileSaysAScannedPDFHasNoReadableText
func TestGetFileSaysAScannedPDFHasNoReadableText(t *testing.T) {
	e := newAgentEnv(t)
	it, ids := e.itemWithFiles(stageNew, [3]any{"scan.pdf", "application/pdf", tinyPDF("", "")})
	m := e.fileJSON(it, fmt.Sprintf("file_id=%d", ids[0]))
	msg, _ := m["message"].(string)
	if m["readable"] != false || !strings.Contains(msg, "no readable text") || !strings.Contains(msg, "scanned") || m["text"] != nil {
		t.Fatalf("scan: %v", m)
	}
}

// TestGetFileReadsWordAndExcelFiles
func TestGetFileReadsWordAndExcelFiles(t *testing.T) {
	e := newAgentEnv(t)
	it, ids := e.itemWithFiles(stageNew,
		[3]any{"letter.docx", "application/octet-stream", tinyDocx("Dear the owner,", "The rent is due on 1 January.")},
		[3]any{"costs.xlsx", "application/octet-stream", tinyXlsx(600)},
		[3]any{"notes.csv", "text/csv", []byte("a,b\n1,2\n")},
		[3]any{"page.html", "text/html", []byte("<html><body><script>x()</script><p>Hello <b>there</b></p></body></html>")},
		[3]any{"meet.ics", "text/calendar", []byte("BEGIN:VEVENT\r\nSUMMARY:Visit to the\r\n  site\r\nEND:VEVENT\r\n")})
	text := func(i int) string { return e.fileJSON(it, fmt.Sprintf("file_id=%d", ids[i]))["text"].(string) }
	if d := text(0); d != "Dear the owner,\nThe rent is due on 1 January." {
		t.Errorf("docx: %q", d)
	}
	x := text(1)
	if !strings.Contains(x, `Sheet "Costs"`) || !strings.Contains(x, "Item | Price") || !strings.Contains(x, "20 |  | row 2") || strings.Contains(x, "row 600") ||
		!strings.Contains(x, "more rows, not shown") {
		t.Errorf("xlsx: %.300q", x)
	}
	if c := text(2); c != "a,b\n1,2" {
		t.Errorf("csv: %q", c)
	}
	if h := text(3); h != "Hello there" || strings.Contains(h, "x()") {
		t.Errorf("html: %q", h)
	}
	if c := text(4); !strings.Contains(c, "SUMMARY:Visit to the site") {
		t.Errorf("ics: %q", c)
	}
}

// TestGetFilePictureComesBackAsImageContentAndIsScaled
func TestGetFilePictureComesBackAsImageContentAndIsScaled(t *testing.T) {
	e := newAgentEnv(t)
	small := bigPNG(40, 30)
	it, ids := e.itemWithFiles(stageNew, [3]any{"huge.png", "image/png", bigPNG(2400, 1000)}, [3]any{"icon.png", "image/png", small})
	res := e.rpc(e.a.agentTokenValue(), "tools/call", map[string]any{"name": "get_file", "arguments": map[string]any{"id": ref(it), "file_id": ids[0]}})["result"].(map[string]any)
	content := res["content"].([]any)
	if len(content) != 2 || content[1].(map[string]any)["type"] != "image" || content[1].(map[string]any)["mimeType"] != "image/jpeg" {
		t.Fatalf("content: %v", res)
	}
	raw, err := base64.StdEncoding.DecodeString(content[1].(map[string]any)["data"].(string))
	if err != nil || len(raw) > 1_000_000 {
		t.Fatalf("image: %v %d bytes", err, len(raw))
	}
	img, err := jpeg.Decode(bytes.NewReader(raw))
	if err != nil || img.Bounds().Dx() != 1600 || img.Bounds().Dy() != 666 {
		t.Fatalf("scaled picture: %v %v", err, img.Bounds())
	}
	head := content[0].(map[string]any)["text"].(string)
	if !strings.Contains(head, "huge.png") || !strings.Contains(head, "2400x1000") || !strings.Contains(head, "scaled down to 1600x666") || strings.Contains(head, "image_base64") {
		t.Errorf("head: %s", head)
	}
	// A small picture goes as it is.
	res = e.rpc(e.a.agentTokenValue(), "tools/call", map[string]any{"name": "get_file", "arguments": map[string]any{"id": ref(it), "file_id": ids[1]}})["result"].(map[string]any)
	if got, _ := base64.StdEncoding.DecodeString(res["content"].([]any)[1].(map[string]any)["data"].(string)); !bytes.Equal(got, small) {
		t.Error("a small picture was changed")
	}
}

// TestGetFileSaysAnUnknownTypeCannotBeRead
func TestGetFileSaysAnUnknownTypeCannotBeRead(t *testing.T) {
	e := newAgentEnv(t)
	it, ids := e.itemWithFiles(stageNew,
		[3]any{"tool.exe", "application/x-msdownload", []byte("MZ\x90\x00\x03\x00\x00\x00 binary")},
		[3]any{"old.doc", "application/msword", append([]byte("\xd0\xcf\x11\xe0"), make([]byte, 50)...)},
		[3]any{"fake.txt", "text/plain", append([]byte("abc"), make([]byte, 50)...)},
		[3]any{"empty.txt", "text/plain", []byte{}},
		[3]any{"pack.zip", "application/zip", zipOf(map[string]string{"a.txt": "x"})})
	for i, want := range []string{"cannot be read here", "old Word or Excel", "binary data", "", "not a Word"} {
		m := e.fileJSON(it, fmt.Sprintf("file_id=%d", ids[i]))
		msg, _ := m["message"].(string)
		if i == 3 {
			continue // an empty file may be stored as nothing at all
		}
		if m["readable"] != false || !strings.Contains(msg, want) {
			t.Errorf("%d: %v", i, m)
		}
	}
}

// TestGetFileRefusesAZipBombAHugePageCountAndATruncatedFileWithinBounds
func TestGetFileRefusesAZipBombAHugePageCountAndATruncatedFileWithinBounds(t *testing.T) {
	e := newAgentEnv(t)
	bomb := zipOf(map[string]string{"word/document.xml": "<w:document>" + strings.Repeat("<w:p>", 1) + strings.Repeat(" ", int(zipMaxEntryBytes)+10) + "</w:document>"})
	deep := tinyDocxRaw(strings.Repeat("<a>", 500) + strings.Repeat("</a>", 500))
	huge := assemblePDF([]string{"<< /Type /Catalog /Pages 2 0 R >>", "<< /Type /Pages /Kids [] /Count 999999 >>"}, "")
	trunc := tinyPDF("Some words")
	trunc = trunc[:len(trunc)/2]
	cut := tinyDocx("Some words")
	cut = cut[:len(cut)/2]
	hostile := assemblePDF([]string{"<< /Type /Catalog /Pages 2 0 R >>", "<< /Type /Pages /Kids [3 0 R] /Count 1 >>",
		"<< /Type /Page /Parent 2 0 R /Contents 4 0 R /Resources << >> >>",
		"<< /Length 5 /Filter /FlateDecode /DecodeParms << /Predictor 12 /Columns 99999999999 >> >>\nstream\nxxxxx\nendstream"}, "")
	it, ids := e.itemWithFiles(stageNew, [3]any{"bomb.docx", "", bomb}, [3]any{"deep.docx", "", deep}, [3]any{"huge.pdf", "", huge},
		[3]any{"cut.pdf", "", trunc}, [3]any{"cut.docx", "", cut}, [3]any{"junk.pdf", "", []byte("%PDF-1.4\nnot really")}, [3]any{"hostile.pdf", "", hostile})
	for i, want := range []string{"zip bomb", "damaged or built", "999999 pages", "damaged or cut short", "damaged or cut short", "damaged or cut short", "cannot be read safely"} {
		m := e.fileJSON(it, fmt.Sprintf("file_id=%d", ids[i]))
		msg, _ := m["message"].(string)
		if m["_status"] != float64(200) || m["readable"] != false || !strings.Contains(msg, want) {
			t.Errorf("%d (%s): %v", i, want, m)
		}
	}
	// Bounds are small: a file over the size bound is not opened.
	old := fileMaxBytes
	fileMaxBytes = 100
	defer func() { fileMaxBytes = old }()
	it2, ids2 := e.itemWithFiles(stageNew, [3]any{"big.txt", "text/plain", bytes.Repeat([]byte("a"), 500)})
	if m := e.fileJSON(it2, fmt.Sprintf("file_id=%d", ids2[0])); m["readable"] != false || !strings.Contains(m["message"].(string), "larger than") {
		t.Errorf("too large: %v", m)
	}
}

func tinyDocxRaw(body string) []byte {
	return zipOf(map[string]string{"word/document.xml": `<w:document xmlns:w="x"><w:body>` + body + `</w:body></w:document>`})
}

// TestGetFileCutsLongTextAndContinues
func TestGetFileCutsLongTextAndContinues(t *testing.T) {
	e := newAgentEnv(t)
	text := strings.Repeat("0123456789", 5000) // 50,000 characters
	it, ids := e.itemWithFiles(stageNew, [3]any{"long.txt", "text/plain", []byte(text)})
	q := fmt.Sprintf("file_id=%d", ids[0])
	m := e.fileJSON(it, q)
	got := m["text"].(string)
	if m["characters_total"] != float64(50000) || m["characters_left_out"] != float64(30000) || m["next_offset"] != float64(20000) ||
		!strings.HasSuffix(got, "[30000 more characters left out; continue with offset=20000]") || !strings.HasPrefix(got, text[:20000]) {
		t.Fatalf("first: %v %.80q", m["characters_left_out"], got[len(got)-80:])
	}
	m = e.fileJSON(it, q+"&offset=40000")
	if m["characters_left_out"] != float64(0) || m["text"] != text[40000:] {
		t.Errorf("last part: %v", m["characters_left_out"])
	}
	if m = e.fileJSON(it, q+"&offset=90000"); m["_status"] != float64(400) {
		t.Errorf("offset past the end: %v", m)
	}
}

// TestGetFileOfAnotherItemCannotBeReadThroughAWrongItem
func TestGetFileOfAnotherItemCannotBeReadThroughAWrongItem(t *testing.T) {
	e := newAgentEnv(t)
	a, ida := e.itemWithFiles(stageNew, [3]any{"same.txt", "text/plain", []byte("secret of A")}, [3]any{"same.txt", "text/plain", []byte("second same name")})
	b, idb := e.itemWithFiles(stageNew, [3]any{"b.txt", "text/plain", []byte("text of B")})
	if m := e.fileJSON(b, fmt.Sprintf("file_id=%d", ida[0])); m["_status"] != float64(404) || strings.Contains(fmt.Sprint(m), "secret") {
		t.Errorf("wrong item: %v", m)
	}
	if m := e.fileJSON(b, "name=same.txt"); m["_status"] != float64(404) {
		t.Errorf("name of another item: %v", m)
	}
	if m := e.fileJSON(b, fmt.Sprintf("file_id=%d", idb[0])); m["text"] != "text of B" {
		t.Errorf("own file: %v", m)
	}
	// Two files with one name need the file_id.
	m := e.fileJSON(a, "name=same.txt")
	if m["_status"] != float64(409) || !strings.Contains(m["error"].(string), fmt.Sprint(ida[0])) || !strings.Contains(m["error"].(string), fmt.Sprint(ida[1])) {
		t.Errorf("ambiguous name: %v", m)
	}
	if m := e.fileJSON(a, fmt.Sprintf("file_id=%d", ida[1])); m["text"] != "second same name" {
		t.Errorf("by id: %v", m)
	}
	if m := e.fileJSON(a, ""); m["_status"] != float64(400) {
		t.Errorf("no file named: %v", m)
	}
}

// seedRun makes an item that belongs to a test run.
func (e *agentEnv) seedRun(run, stage string, m seedMsg) repo.Item {
	e.t.Helper()
	old := e.ctx
	e.ctx = repo.WithRun(old, run)
	defer func() { e.ctx = old }()
	return e.seed(stage, m)
}

// TestGetFileAndListItemsKeepTestRunsApart
func TestGetFileAndListItemsKeepTestRunsApart(t *testing.T) {
	e := newAgentEnv(t)
	const run, other = "e2e-aaaaaaaa1", "e2e-bbbbbbbb2"
	real := e.seed(stageNew, e.mail("Real mail", 1))
	mine := e.seedRun(run, stageNew, e.mail("Marked mail", 2))
	src := fmt.Sprint("m", seedN.Load(), "-0")
	fid := e.saveFile(mine, src, "run.txt", "text/plain", "attachment", []byte("run file"))
	titles := func(m map[string]any) string {
		var s []string
		for _, x := range m["items"].([]any) {
			s = append(s, x.(map[string]any)["subject"].(string))
		}
		return strings.Join(s, ",")
	}
	if m := e.getJSON("/api/stage-items?stage=new"); titles(m) != "Real mail" || m["total"] != float64(1) {
		t.Errorf("without a run: %v", titles(m))
	}
	if m := e.getJSON("/api/stage-items?stage=new&run=" + run); titles(m) != "Marked mail" || m["total"] != float64(1) {
		t.Errorf("with the run: %v", titles(m))
	}
	if m := e.getJSON("/api/stage-items?stage=new&run=" + other); titles(m) != "" || m["total"] != float64(0) {
		t.Errorf("other run: %v", titles(m))
	}
	out, bad := e.tool("list_items", map[string]any{"stage": "new", "run": run})
	if bad || !strings.Contains(out, "Marked mail") || strings.Contains(out, "Real mail") {
		t.Errorf("tool with run: %v %s", bad, out)
	}
	// A call that carries a run reads only that run's files.
	if m := e.fileJSON(mine, fmt.Sprintf("file_id=%d&run=%s", fid, run)); m["text"] != "run file" {
		t.Errorf("own run: %v", m)
	}
	for _, bd := range []string{"&run=" + other} {
		if m := e.fileJSON(mine, fmt.Sprintf("file_id=%d%s", fid, bd)); m["_status"] != float64(403) || m["text"] != nil {
			t.Errorf("other run: %v", m)
		}
	}
	rf := e.saveFile(real, fmt.Sprint("m", seedN.Load()-1, "-0"), "real.txt", "text/plain", "attachment", []byte("real file"))
	if m := e.fileJSON(real, fmt.Sprintf("file_id=%d&run=%s", rf, run)); m["_status"] != float64(403) || m["text"] != nil {
		t.Errorf("a run reads a real item's file: %v", m)
	}
	if m := e.fileJSON(real, fmt.Sprintf("file_id=%d", rf)); m["text"] != "real file" {
		t.Errorf("no run: %v", m)
	}
	if m := e.getJSON("/api/stage-items?stage=new&run=bad"); m["_status"] != float64(400) {
		t.Errorf("bad run: %v", m)
	}
}

// TestListItemsPagesInNewestFirstOrderWithCountsAndFlags
func TestListItemsPagesInNewestFirstOrderWithCountsAndFlags(t *testing.T) {
	e := newAgentEnv(t)
	var items []repo.Item
	for i := 0; i < 5; i++ { // item 0 is the oldest
		items = append(items, e.seed(stagePending, e.mail(fmt.Sprintf("Pending %d", i), float64(10-i))))
	}
	two := e.seed(stagePending, e.mail("Two messages", 0.5), e.mail("Two messages", 0.4))
	e.seed(stageResolved, e.mail("Resolved one", 1))
	e.seed(stageNew, e.mail("New one", 1))
	// The owner's note, a waiting proposal and an open to-do on the oldest.
	if err := e.a.store.Update(e.ctx, func(tx *repo.Tx) error {
		return tx.InsertEvent(e.ctx, e.now.Unix(), items[0].ID, "owner", "noted", `{"text":"remember"}`)
	}); err != nil {
		t.Fatal(err)
	}
	e.propose(items[0], "close", "Done.")
	ids := func(m map[string]any) []string {
		var s []string
		for _, x := range m["items"].([]any) {
			s = append(s, x.(map[string]any)["id"].(string))
		}
		return s
	}
	p1 := e.getJSON("/api/stage-items?stage=pending&limit=3")
	want := []string{ref(two), ref(items[4]), ref(items[3])}
	if fmt.Sprint(ids(p1)) != fmt.Sprint(want) || p1["total"] != float64(6) || p1["more"] != true || p1["next_after"] != ref(items[3]) || p1["note"] != dataNote {
		t.Fatalf("page 1: %v total %v", ids(p1), p1["total"])
	}
	first := p1["items"].([]any)[0].(map[string]any)
	if first["messages"] != float64(2) || first["from"] != "alice@example.test" || first["from_name"] != "Alice Example" || first["source"] == "" || first["last_message_at"] == "" {
		t.Errorf("entry: %v", first)
	}
	p2 := e.getJSON("/api/stage-items?stage=pending&limit=3&after=" + ref(items[3]))
	if fmt.Sprint(ids(p2)) != fmt.Sprint([]string{ref(items[2]), ref(items[1]), ref(items[0])}) || p2["more"] != false || p2["next_after"] != nil {
		t.Fatalf("page 2: %v", ids(p2))
	}
	oldest := p2["items"].([]any)[2].(map[string]any)
	if oldest["owner_left_note"] != true || oldest["proposals_waiting"] != float64(1) || oldest["waiting_for_claude"] != false {
		t.Errorf("flags: %v", oldest)
	}
	if p2["items"].([]any)[0].(map[string]any)["owner_left_note"] != false {
		t.Error("a note on every row")
	}
	if m := e.getJSON("/api/stage-items?stage=resolved"); m["total"] != float64(1) || len(ids(m)) != 1 {
		t.Errorf("resolved: %v", m)
	}
	for _, bad := range []string{"stage=closed", "stage=", "stage=history"} {
		if m := e.getJSON("/api/stage-items?" + bad); m["_status"] != float64(400) {
			t.Errorf("%s: %v", bad, m["_status"])
		}
	}
	if m := e.getJSON("/api/stage-items?stage=pending&after=Q-9999"); m["_status"] != float64(404) {
		t.Errorf("after a missing item: %v", m["_status"])
	}
	if m := e.getJSON("/api/stage-items?stage=pending&limit=1000"); len(m["items"].([]any)) != 6 {
		t.Errorf("limit: %v", m)
	}
	if out, bad := e.tool("list_items", map[string]any{"stage": "pending", "limit": 2, "after": ref(two)}); bad || !strings.Contains(out, "Pending 4") {
		t.Errorf("tool: %v %s", bad, out)
	}
}

// TestTheAppsOwnReviewDoesNotGetGetFileOrListItems
func TestTheAppsOwnReviewDoesNotGetGetFileOrListItems(t *testing.T) {
	newAgentEnv(t)
	for _, tl := range modelTools() {
		if tl.Name == "get_file" || tl.Name == "list_items" {
			t.Errorf("the model has %s", tl.Name)
		}
	}
	if len(modelTools()) != len(modelToolNames) {
		t.Errorf("model tools %d, list %d", len(modelTools()), len(modelToolNames))
	}
}

// scannedJPEG is the picture of page i of scannedPDF; a page's picture differs
// by its colour only, so equal sizes give streams of equal length.
func scannedJPEG(i, w, h int) []byte {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{uint8(40 * (i + 1)), uint8(x % 256), uint8(y % 256), 255})
		}
	}
	var jb bytes.Buffer
	jpeg.Encode(&jb, img, &jpeg.Options{Quality: 90})
	return jb.Bytes()
}

// scannedPDF builds a PDF whose pages are one JPEG picture each (DCTDecode),
// with no text layer, like a scan.
func scannedPDF(sizes ...[2]int) []byte {
	var objs []string
	kids := ""
	for i := range sizes {
		kids += fmt.Sprintf("%d 0 R ", 3+3*i)
	}
	objs = append(objs, "<< /Type /Catalog /Pages 2 0 R >>", fmt.Sprintf("<< /Type /Pages /Kids [%s] /Count %d >>", kids, len(sizes)))
	for i, sz := range sizes {
		jb := bytes.NewBuffer(scannedJPEG(i, sz[0], sz[1]))
		content := fmt.Sprintf("q %d 0 0 %d 0 0 cm /Im0 Do Q", sz[0], sz[1])
		objs = append(objs,
			fmt.Sprintf("<< /Type /Page /Parent 2 0 R /MediaBox [0 0 %d %d] /Contents %d 0 R /Resources << /XObject << /Im0 %d 0 R >> >> >>", sz[0], sz[1], 4+3*i, 5+3*i),
			fmt.Sprintf("<< /Length %d >>\nstream\n%s\nendstream", len(content), content),
			fmt.Sprintf("<< /Type /XObject /Subtype /Image /Width %d /Height %d /ColorSpace /DeviceRGB /BitsPerComponent 8 /Filter /DCTDecode /Length %d >>\nstream\n%s\nendstream", sz[0], sz[1], jb.Len(), jb.String()))
	}
	return assemblePDF(objs, "")
}

// TestGetFileReturnsThePageOfAScannedPDFAsAPicture: a scan made of one JPEG
// per page comes back as the picture of the asked page, scaled down when large.
func TestGetFileReturnsThePageOfAScannedPDFAsAPicture(t *testing.T) {
	e := newAgentEnv(t)
	it, ids := e.itemWithFiles(stageNew, [3]any{"scan.pdf", "application/pdf", scannedPDF([2]int{120, 80}, [2]int{2400, 1000})})
	call := func(page int) map[string]any {
		return e.rpc(e.a.agentTokenValue(), "tools/call", map[string]any{"name": "get_file", "arguments": map[string]any{"id": ref(it), "file_id": ids[0], "page": page}})["result"].(map[string]any)
	}
	res := call(1)
	content := res["content"].([]any)
	if len(content) != 2 || content[1].(map[string]any)["type"] != "image" {
		t.Fatalf("page 1: %v", res)
	}
	raw, _ := base64.StdEncoding.DecodeString(content[1].(map[string]any)["data"].(string))
	img, err := jpeg.Decode(bytes.NewReader(raw))
	if err != nil || img.Bounds().Dx() != 120 || img.Bounds().Dy() != 80 {
		t.Fatalf("page 1 picture: %v", err)
	}
	if head := content[0].(map[string]any)["text"].(string); !strings.Contains(head, "page 1 of 2") || !strings.Contains(head, "scanned PDF") {
		t.Errorf("head: %s", head)
	}
	// Page 2 is large and is scaled down like any picture.
	content = call(2)["content"].([]any)
	raw, _ = base64.StdEncoding.DecodeString(content[1].(map[string]any)["data"].(string))
	img, err = jpeg.Decode(bytes.NewReader(raw))
	if err != nil || img.Bounds().Dx() != 1600 || img.Bounds().Dy() != 666 {
		t.Fatalf("page 2 picture: %v", err)
	}
	// Pages of the same pixel size and different content: each page gets its own picture.
	pdf2 := scannedPDF([2]int{200, 100}, [2]int{200, 100}, [2]int{200, 100})
	it2, ids2 := e.itemWithFiles(stageNew, [3]any{"same.pdf", "application/pdf", pdf2})
	var seen [][]byte
	for pg := 1; pg <= 3; pg++ {
		res := e.rpc(e.a.agentTokenValue(), "tools/call", map[string]any{"name": "get_file", "arguments": map[string]any{"id": ref(it2), "file_id": ids2[0], "page": pg}})["result"].(map[string]any)
		raw, _ := base64.StdEncoding.DecodeString(res["content"].([]any)[1].(map[string]any)["data"].(string))
		want := scannedJPEG(pg-1, 200, 100)
		if !bytes.Equal(raw, want) {
			t.Errorf("page %d did not return its own picture", pg)
		}
		for _, o := range seen {
			if bytes.Equal(o, raw) {
				t.Errorf("page %d returned another page's picture", pg)
			}
		}
		seen = append(seen, raw)
	}
	// A page that does not exist is still said so in a sentence.
	m := e.fileJSON(it, fmt.Sprintf("file_id=%d&page=3", ids[0]))
	if m["readable"] != false || !strings.Contains(m["message"].(string), "no page 3") {
		t.Errorf("page 3: %v", m)
	}
}
