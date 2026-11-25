package main

import (
	"archive/zip"
	"bytes"
	"encoding/base64"
	"encoding/xml"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"io"
	"net/http"
	"path"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/ledongthuc/pdf"
	"golang.org/x/text/encoding/charmap"
)

// Reading the files of an item for agents (SPEC 29). A file comes from other
// people, so every reader here assumes it is hostile: it only reads bytes
// already in the pool (no network, nothing written), and bounds the size of
// the file, the time, the number of pages, rows and zip entries, the size a
// zip entry may have once unpacked, and the nesting of what it parses. A
// parser that panics is recovered and the file is answered as unreadable.

// These are variables so that the tests can prove a bound with a small input.
var (
	fileMaxBytes     int64 = 25 << 20 // a larger file is not opened
	zipMaxEntries          = 3000     // entries a zip may list
	zipMaxEntryBytes int64 = 20 << 20 // one entry, once unpacked
	zipMaxTotalBytes int64 = 40 << 20 // all entries read for one file, once unpacked
	xmlMaxDepth            = 200      // nesting of elements
	pdfMaxPages            = 2000     // a PDF that claims more is refused
	pdfTimeLimit           = 15 * time.Second
	textExtractMax         = 1_500_000 // characters taken out of one file
	xlsxMaxSheets          = 20
	xlsxMaxRows            = 500 // per sheet
	xlsxMaxCols            = 50
	xlsxMaxStrings         = 200_000
	imageMaxPixels         = 25_000_000
	imageMaxSide           = 1600
	imageMaxBytes          = 1_000_000 // what goes back to the agent
)

const fileTextCap = 20000 // characters of a file's text in one answer

// fileAnswer is what reading one file gives. When Message is set the file
// cannot be read here and Message says why in a plain sentence.
type fileAnswer struct {
	Kind    string // pdf, word, spreadsheet, text, html, calendar, picture, unreadable
	Message string
	Text    string // the whole text taken out (cut later by cutFileText)
	More    bool   // the file holds more text than was taken out
	Pages   int    // PDF: pages in the file
	// A PDF's text starts at page FirstPage; PageAt[i] is the rune offset where the i-th page read begins.
	FirstPage int
	PageNums  []int
	PageAt    []int
	Note      string // a remark to carry along (time limit, pages skipped)
	// A picture.
	Image     []byte
	ImageMime string
	ImageInfo string
}

func unreadable(format string, a ...any) fileAnswer {
	return fileAnswer{Kind: "unreadable", Message: fmt.Sprintf(format, a...)}
}

var fileSlots = make(chan struct{}, 3) // files being read at once
var pdfSlots = make(chan struct{}, 2)  // a PDF parser that never ends keeps its slot, so at most 2 can hang

// readFileBytes reads a pool file whole, within the size bound.
func readLimited(r io.Reader, size int64) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(r, size+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > size {
		return nil, errTooBig
	}
	return b, nil
}

var errTooBig = errors.New("too big")

// readFileData decides what a file is from its bytes (the declared type and
// the name only help for plain text) and reads it.
func readFileData(name, ctype string, data []byte, firstPage int) (ans fileAnswer) {
	defer func() {
		if x := recover(); x != nil {
			ans = unreadable("This file could not be read here: it is damaged or built in a way that cannot be read.")
		}
	}()
	if len(data) == 0 {
		return unreadable("This file is empty.")
	}
	ext := strings.ToLower(path.Ext(name))
	switch {
	case bytes.HasPrefix(data, []byte("%PDF-")) || (len(data) > 1024 && bytes.Contains(data[:1024], []byte("%PDF-"))):
		return readPDF(data, firstPage)
	case bytes.HasPrefix(data, []byte("PK\x03\x04")) || bytes.HasPrefix(data, []byte("PK\x05\x06")):
		return readOffice(data, ext)
	}
	switch sniff := http.DetectContentType(data); {
	case sniff == "image/jpeg" || sniff == "image/png" || sniff == "image/gif" || sniff == "image/webp":
		return readPicture(data, sniff)
	}
	if bytes.HasPrefix(data, []byte("\xd0\xcf\x11\xe0")) {
		return unreadable("This is an old Word or Excel file (.doc, .xls); it cannot be read here. Ask for it as .docx, .xlsx or PDF.")
	}
	if kind, ok := textKind(ext, strings.ToLower(ctype)); ok {
		txt, ok := decodeText(data)
		if !ok {
			return unreadable("This file looks like %s but holds binary data, so it cannot be read here.", kindWord(kind))
		}
		switch kind {
		case "html":
			txt = htmlToText(txt)
		case "calendar":
			txt = unfoldICS(txt)
		}
		a := fileAnswer{Kind: kind}
		a.Text, a.More = clipRunes(strings.TrimSpace(txt), textExtractMax)
		return a
	}
	return unreadable("This kind of file (%s) cannot be read here. Only PDF, Word (.docx), Excel (.xlsx), text, CSV, HTML, calendar (.ics) files and pictures can.", describeType(name, ctype))
}

func kindWord(kind string) string {
	switch kind {
	case "html":
		return "a web page"
	case "calendar":
		return "a calendar file"
	}
	return "text"
}

func describeType(name, ctype string) string {
	if ctype = strings.TrimSpace(ctype); ctype != "" {
		return oneLine(ctype, 60)
	}
	if e := path.Ext(name); e != "" {
		return oneLine(e, 20)
	}
	return "unknown type"
}

var textExts = map[string]string{".txt": "text", ".csv": "text", ".tsv": "text", ".md": "text", ".log": "text", ".json": "text", ".xml": "text",
	".html": "html", ".htm": "html", ".ics": "calendar", ".ical": "calendar", ".vcf": "text"}

func textKind(ext, ctype string) (string, bool) {
	if k, ok := textExts[ext]; ok {
		return k, true
	}
	switch {
	case strings.HasPrefix(ctype, "text/html"):
		return "html", true
	case strings.HasPrefix(ctype, "text/calendar"):
		return "calendar", true
	case strings.HasPrefix(ctype, "text/"), ctype == "application/json", ctype == "application/xml":
		return "text", true
	}
	return "", false
}

// decodeText reads bytes as text: UTF-8, UTF-16 with a byte order mark, or
// (when the bytes are not valid UTF-8) Windows-1250. ok is false for binary.
func decodeText(b []byte) (string, bool) {
	switch {
	case bytes.HasPrefix(b, []byte{0xff, 0xfe}), bytes.HasPrefix(b, []byte{0xfe, 0xff}):
		big := b[0] == 0xfe
		b = b[2:]
		u := make([]uint16, 0, len(b)/2)
		for i := 0; i+1 < len(b); i += 2 {
			if big {
				u = append(u, uint16(b[i])<<8|uint16(b[i+1]))
			} else {
				u = append(u, uint16(b[i+1])<<8|uint16(b[i]))
			}
		}
		return string(utf16.Decode(u)), true
	case bytes.HasPrefix(b, []byte{0xef, 0xbb, 0xbf}):
		b = b[3:]
	}
	probe := b
	if len(probe) > 8192 {
		probe = probe[:8192]
	}
	if bytes.IndexByte(probe, 0) >= 0 {
		return "", false
	}
	if utf8.Valid(b) {
		return string(b), true
	}
	if s, err := charmap.Windows1250.NewDecoder().Bytes(b); err == nil {
		return string(s), true
	}
	return "", false
}

func unfoldICS(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\n ", "")
	return strings.ReplaceAll(s, "\n\t", "")
}

// clipRunes keeps the first max characters; more says some were dropped.
func clipRunes(s string, max int) (string, bool) {
	if len(s) <= max {
		return s, false
	}
	n := 0
	for i := range s {
		if n == max {
			return s[:i], true
		}
		n++
	}
	return s, false
}

// --- PDF ---

var pdfColumnsRE = regexp.MustCompile(`/Columns\s+(\d{6,})`)

func readPDF(data []byte, firstPage int) fileAnswer {
	// The library builds a buffer as wide as a predictor's /Columns says.
	if pdfColumnsRE.Match(data) {
		return unreadable("This PDF is built in a way that cannot be read safely here.")
	}
	select {
	case pdfSlots <- struct{}{}:
	default:
		return unreadable("The PDF reader is busy; try again in a minute.")
	}
	type state struct {
		sync.Mutex
		done     bool
		err      error
		encrypt  bool
		pages    int
		text     strings.Builder
		runes    int
		more     bool
		pageNums []int
		pageAt   []int
		stopped  string
	}
	st := &state{}
	abort := make(chan struct{})
	finished := make(chan struct{})
	go func() {
		defer func() { <-pdfSlots; close(finished) }()
		defer func() {
			if x := recover(); x != nil {
				st.Lock()
				if st.err == nil {
					st.err = fmt.Errorf("damaged")
				}
				st.Unlock()
			}
		}()
		r, err := pdf.NewReader(bytes.NewReader(data), int64(len(data)))
		if err != nil {
			st.Lock()
			st.err = err
			st.encrypt = errors.Is(err, pdf.ErrInvalidPassword) || strings.Contains(strings.ToLower(err.Error()), "password") || strings.Contains(strings.ToLower(err.Error()), "encrypt")
			st.Unlock()
			return
		}
		n := r.NumPage()
		st.Lock()
		st.pages = n
		st.Unlock()
		if n <= 0 || n > pdfMaxPages {
			return
		}
		for i := max(firstPage, 1); i <= n; i++ {
			select {
			case <-abort:
				return
			default:
			}
			txt := pageText(r, i)
			st.Lock()
			st.pageNums = append(st.pageNums, i)
			st.pageAt = append(st.pageAt, st.runes)
			if st.runes+utf8.RuneCountInString(txt) > textExtractMax {
				clipped, _ := clipRunes(txt, textExtractMax-st.runes)
				st.text.WriteString(clipped)
				st.runes = textExtractMax
				st.more = true
				st.Unlock()
				return
			}
			st.text.WriteString(txt)
			st.runes += utf8.RuneCountInString(txt)
			st.Unlock()
		}
		st.Lock()
		st.done = true
		st.Unlock()
	}()
	timer := time.NewTimer(pdfTimeLimit)
	defer timer.Stop()
	timedOut := false
	select {
	case <-finished:
	case <-timer.C:
		timedOut = true
		close(abort)
		select { // give a page that is nearly done a moment
		case <-finished:
		case <-time.After(300 * time.Millisecond):
		}
	}
	st.Lock()
	defer st.Unlock()
	switch {
	case st.encrypt:
		return unreadable("This PDF is encrypted (it asks for a password), so it cannot be read here.")
	case st.err != nil && st.pages == 0:
		return unreadable("This PDF could not be read: it is damaged or cut short.")
	case st.pages <= 0:
		return unreadable("This PDF holds no pages that can be read.")
	case st.pages > pdfMaxPages:
		return unreadable("This PDF says it has %d pages, more than the %d that can be read here, so it is refused.", st.pages, pdfMaxPages)
	}
	a := fileAnswer{Kind: "pdf", Pages: st.pages, FirstPage: max(firstPage, 1), PageNums: st.pageNums, PageAt: st.pageAt, More: st.more}
	if firstPage > st.pages {
		return unreadable("This PDF has %d pages; there is no page %d.", st.pages, firstPage)
	}
	a.Text = st.text.String()
	switch {
	case timedOut && !st.done:
		a.Note = fmt.Sprintf("Reading stopped after the time limit (%d seconds), at page %d of %d.", int(pdfTimeLimit/time.Second), lastOr(st.pageNums, firstPage), st.pages)
	case st.err != nil && !st.done:
		a.Note = fmt.Sprintf("Reading stopped at a damaged page after page %d of %d.", lastOr(st.pageNums, firstPage), st.pages)
	}
	if strings.TrimSpace(a.Text) == "" && st.done {
		// A scanned PDF: the page is a picture. Many scans hold one JPEG per page; that picture goes back, scaled like any other.
		pg := max(firstPage, 1)
		if jpg := scannedPageJPEG(data, pg); jpg != nil {
			if pic := readPicture(jpg, "image/jpeg"); pic.Kind == "picture" {
				pic.ImageInfo = fmt.Sprintf("page %d of %d of a scanned PDF (it has no text layer; the page is this picture): %s. Ask for another page with page=N.", pg, st.pages, pic.ImageInfo)
				return pic
			}
		}
		if firstPage <= 1 {
			return unreadable("This PDF has %d page(s) and no readable text: it looks scanned (its pages are pictures), and only scans made of one JPEG picture per page can be shown here; this one cannot.", st.pages)
		}
	}
	return a
}

// scannedPageJPEG returns the JPEG (DCTDecode) picture that fills a page of a
// scanned PDF: the page's largest such image, taken as it is stored. The
// parser library cannot hand out a DCT stream, but it names where the page's
// own image stream starts in the file (the number after "@" in the printed
// stream), and the bytes are cut there by the stream's own /Length. nil when
// the page holds no such picture or the stream cannot be identified for sure.
func scannedPageJPEG(data []byte, page int) (jpg []byte) {
	defer func() {
		if recover() != nil {
			jpg = nil
		}
	}()
	r, err := pdf.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil || page < 1 || page > r.NumPage() {
		return nil
	}
	xo := r.Page(page).Resources().Key("XObject")
	var best int64
	var off, length int64 = -1, 0
	for _, k := range xo.Keys() {
		x := xo.Key(k)
		if x.Kind() != pdf.Stream || x.Key("Subtype").Name() != "Image" {
			continue
		}
		f := x.Key("Filter")
		if f.Kind() == pdf.Array && f.Len() == 1 {
			f = f.Index(0)
		}
		if f.Kind() != pdf.Name || f.Name() != "DCTDecode" {
			continue
		}
		w, h, n := x.Key("Width").Int64(), x.Key("Height").Int64(), x.Key("Length").Int64()
		str := x.String()
		at := strings.LastIndex(str, "@")
		if at < 0 || w <= 0 || h <= 0 || n <= 0 || w*h <= best {
			continue
		}
		o, err := strconv.ParseInt(str[at+1:], 10, 64)
		if err != nil {
			continue
		}
		best, off, length = w*h, o, n
	}
	if off < 0 || off+length > int64(len(data)) {
		return nil
	}
	c := data[off : off+length]
	if len(c) < 4 || c[0] != 0xFF || c[1] != 0xD8 || !bytes.HasPrefix(bytes.TrimLeft(data[off+length:], "\r\n "), []byte("endstream")) {
		return nil
	}
	return c
}

func lastOr(l []int, d int) int {
	if len(l) == 0 {
		return d
	}
	return l[len(l)-1]
}

func pageText(r *pdf.Reader, i int) (txt string) {
	defer func() {
		if recover() != nil {
			txt = ""
		}
	}()
	t, err := r.Page(i).GetPlainText(nil)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(strings.ToValidUTF8(t, ""))
}

// --- Word and Excel (zip of XML) ---

func readOffice(data []byte, ext string) fileAnswer {
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return unreadable("This file looks like a zip archive but is damaged or cut short, so it cannot be read here.")
	}
	if len(zr.File) > zipMaxEntries {
		return unreadable("This archive lists %d entries, more than the %d that can be read here, so it is refused.", len(zr.File), zipMaxEntries)
	}
	z := &zipReader{files: map[string]*zip.File{}}
	var declared uint64
	for _, f := range zr.File {
		z.files[f.Name] = f
		declared += f.UncompressedSize64
	}
	if declared > uint64(zipMaxTotalBytes)*4 {
		return unreadable("This archive unpacks to far more than can be read here, so it is refused.")
	}
	switch {
	case z.files["word/document.xml"] != nil:
		return readDocx(z)
	case z.files["xl/workbook.xml"] != nil:
		return readXlsx(z)
	}
	return unreadable("This is a zip archive that is not a Word (.docx) or Excel (.xlsx) file, so it cannot be read here.")
}

type zipReader struct {
	files map[string]*zip.File
	total int64
}

var errBomb = errors.New("unpacks too large")

// read unpacks one entry within the bounds (nil, nil when there is none).
func (z *zipReader) read(name string) ([]byte, error) {
	f := z.files[name]
	if f == nil {
		return nil, nil
	}
	if f.UncompressedSize64 > uint64(zipMaxEntryBytes) {
		return nil, errBomb
	}
	rc, err := f.Open()
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	b, err := readLimited(rc, min(zipMaxEntryBytes, zipMaxTotalBytes-z.total))
	if err != nil {
		if errors.Is(err, errTooBig) {
			return nil, errBomb
		}
		return nil, err
	}
	z.total += int64(len(b))
	return b, nil
}

func officeFail(err error) fileAnswer {
	if errors.Is(err, errBomb) {
		return unreadable("This file unpacks to far more than can be read here (it may be a zip bomb), so it is refused.")
	}
	return unreadable("This file could not be read here: it is damaged or built in a way that cannot be read.")
}

var errDeep = errors.New("nested too deep")

// walkXML calls fn for every token and refuses a document nested deeper than
// xmlMaxDepth. It never loads a DTD or an external entity.
func walkXML(b []byte, fn func(t xml.Token, depth int) bool) error {
	d := xml.NewDecoder(bytes.NewReader(b))
	d.Strict = false
	d.CharsetReader = func(label string, in io.Reader) (io.Reader, error) { return in, nil }
	depth := 0
	for {
		t, err := d.Token()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		switch t.(type) {
		case xml.StartElement:
			depth++
			if depth > xmlMaxDepth {
				return errDeep
			}
		}
		if !fn(t, depth) {
			return nil
		}
		if _, ok := t.(xml.EndElement); ok {
			depth--
		}
	}
}

func readDocx(z *zipReader) fileAnswer {
	b, err := z.read("word/document.xml")
	if err != nil {
		return officeFail(err)
	}
	var sb strings.Builder
	n, more := 0, false
	add := func(s string) bool {
		if n+len(s) > textExtractMax*4 { // bytes, a loose bound; clipped below
			more = true
			return false
		}
		sb.WriteString(s)
		n += len(s)
		return true
	}
	inT := false
	err = walkXML(b, func(t xml.Token, _ int) bool {
		switch t := t.(type) {
		case xml.StartElement:
			switch t.Name.Local {
			case "t":
				inT = true
			case "tab":
				return add("\t")
			case "br", "cr":
				return add("\n")
			}
		case xml.EndElement:
			switch t.Name.Local {
			case "t":
				inT = false
			case "p":
				return add("\n")
			case "tc":
				return add("\t")
			}
		case xml.CharData:
			if inT {
				return add(string(t))
			}
		}
		return true
	})
	if err != nil && !errors.Is(err, errDeep) && sb.Len() > 0 {
		err = nil // keep what was read of a document cut short
	}
	if err != nil {
		return officeFail(err)
	}
	txt := strings.ToValidUTF8(tidyText(sb.String()), "")
	a := fileAnswer{Kind: "word", More: more}
	var clipped bool
	a.Text, clipped = clipRunes(txt, textExtractMax)
	a.More = a.More || clipped
	if strings.TrimSpace(a.Text) == "" {
		return unreadable("This Word file holds no text (it may hold only pictures).")
	}
	return a
}

func readXlsx(z *zipReader) fileAnswer {
	wb, err := z.read("xl/workbook.xml")
	if err != nil {
		return officeFail(err)
	}
	type sheetRef struct{ name, rid string }
	var sheets []sheetRef
	err = walkXML(wb, func(t xml.Token, _ int) bool {
		if se, ok := t.(xml.StartElement); ok && se.Name.Local == "sheet" && len(sheets) < xlsxMaxSheets+1 {
			s := sheetRef{}
			for _, at := range se.Attr {
				switch {
				case at.Name.Local == "name":
					s.name = at.Value
				case at.Name.Local == "id" && at.Name.Space != "":
					s.rid = at.Value
				}
			}
			sheets = append(sheets, s)
		}
		return true
	})
	if err != nil {
		return officeFail(err)
	}
	targets := map[string]string{}
	if rels, err := z.read("xl/_rels/workbook.xml.rels"); err != nil {
		return officeFail(err)
	} else if rels != nil {
		walkXML(rels, func(t xml.Token, _ int) bool {
			if se, ok := t.(xml.StartElement); ok && se.Name.Local == "Relationship" {
				var id, target string
				for _, at := range se.Attr {
					switch at.Name.Local {
					case "Id":
						id = at.Value
					case "Target":
						target = at.Value
					}
				}
				targets[id] = target
			}
			return true
		})
	}
	var shared []string
	if ss, err := z.read("xl/sharedStrings.xml"); err != nil {
		return officeFail(err)
	} else if ss != nil {
		shared = sharedStrings(ss)
	}
	var sb strings.Builder
	extra := ""
	for i, sh := range sheets {
		if i >= xlsxMaxSheets {
			extra = fmt.Sprintf("More than %d sheets: the rest are not shown.", xlsxMaxSheets)
			break
		}
		target := targets[sh.rid]
		file := "xl/" + strings.TrimPrefix(target, "xl/")
		if strings.HasPrefix(target, "/") {
			file = strings.TrimPrefix(target, "/")
		}
		file = path.Clean(file)
		data, err := z.read(file)
		if err != nil {
			return officeFail(err)
		}
		if data == nil {
			continue
		}
		rows, moreRows, err := sheetRows(data, shared)
		if err != nil && !errors.Is(err, errDeep) && len(rows) > 0 {
			err = nil
		}
		if err != nil {
			return officeFail(err)
		}
		fmt.Fprintf(&sb, "=== Sheet %q: %d row(s) shown", sh.name, len(rows))
		if moreRows {
			fmt.Fprintf(&sb, "; the sheet holds more rows, not shown (at most %d are read)", xlsxMaxRows)
		}
		sb.WriteString(" ===\n")
		for _, r := range rows {
			sb.WriteString(strings.Join(r, " | "))
			sb.WriteString("\n")
		}
		if sb.Len() > textExtractMax*4 {
			break
		}
	}
	a := fileAnswer{Kind: "spreadsheet", Note: extra}
	a.Text, a.More = clipRunes(strings.ToValidUTF8(strings.TrimSpace(sb.String()), ""), textExtractMax)
	if strings.TrimSpace(a.Text) == "" {
		return unreadable("This Excel file holds no sheets that can be read.")
	}
	return a
}

// sharedStrings reads the shared string table of a workbook (the text of the
// cells that hold words), without phonetic runs.
func sharedStrings(b []byte) []string {
	var out []string
	var cur strings.Builder
	inSI, inT, inPh := false, false, false
	total := 0
	walkXML(b, func(t xml.Token, _ int) bool {
		switch t := t.(type) {
		case xml.StartElement:
			switch t.Name.Local {
			case "si":
				inSI = true
				cur.Reset()
			case "t":
				inT = true
			case "rPh":
				inPh = true
			}
		case xml.EndElement:
			switch t.Name.Local {
			case "si":
				s, _ := clipRunes(cur.String(), 300)
				out = append(out, s)
				total += len(s)
				inSI = false
				if len(out) >= xlsxMaxStrings || total > textExtractMax*4 {
					return false
				}
			case "t":
				inT = false
			case "rPh":
				inPh = false
			}
		case xml.CharData:
			if inSI && inT && !inPh {
				cur.Write(t)
			}
		}
		return true
	})
	return out
}

// sheetRows reads the rows of one worksheet, at most xlsxMaxRows rows and
// xlsxMaxCols columns, cells in their columns.
func sheetRows(b []byte, shared []string) (rows [][]string, more bool, err error) {
	var row []string
	var col int
	var typ string
	var val strings.Builder
	var inV, inIS, inT bool
	err = walkXML(b, func(t xml.Token, _ int) bool {
		switch t := t.(type) {
		case xml.StartElement:
			switch t.Name.Local {
			case "row":
				row = nil
			case "c":
				typ = ""
				val.Reset()
				col = len(row)
				for _, at := range t.Attr {
					switch at.Name.Local {
					case "t":
						typ = at.Value
					case "r":
						if c := colIndex(at.Value); c >= 0 {
							col = c
						}
					}
				}
			case "v":
				inV = true
			case "is":
				inIS = true
			case "t":
				inT = true
			}
		case xml.EndElement:
			switch t.Name.Local {
			case "v":
				inV = false
			case "is":
				inIS = false
			case "t":
				inT = false
			case "c":
				if col >= xlsxMaxCols {
					return true
				}
				for len(row) < col {
					row = append(row, "")
				}
				s := val.String()
				switch typ {
				case "s":
					if i, e := strconv.Atoi(strings.TrimSpace(s)); e == nil && i >= 0 && i < len(shared) {
						s = shared[i]
					} else {
						s = ""
					}
				case "b":
					if s == "1" {
						s = "TRUE"
					} else {
						s = "FALSE"
					}
				}
				s = strings.ReplaceAll(strings.ReplaceAll(strings.TrimSpace(s), "\n", " "), "|", "/")
				s, _ = clipRunes(s, 300)
				if col == len(row) {
					row = append(row, s)
				} else {
					row[col] = s
				}
			case "row":
				if len(rows) >= xlsxMaxRows {
					more = true
					return false
				}
				if len(strings.Join(row, "")) > 0 {
					rows = append(rows, row)
				}
			}
		case xml.CharData:
			if inV || (inIS && inT) {
				val.Write(t)
			}
		}
		return true
	})
	return
}

// colIndex turns a cell reference like "C7" into the column number 2.
func colIndex(ref string) int {
	n := 0
	i := 0
	for ; i < len(ref); i++ {
		c := ref[i]
		if c >= 'a' && c <= 'z' {
			c -= 32
		}
		if c < 'A' || c > 'Z' {
			break
		}
		n = n*26 + int(c-'A'+1)
		if n > 100000 {
			return -1
		}
	}
	if i == 0 {
		return -1
	}
	return n - 1
}

// --- pictures ---

func readPicture(data []byte, mime string) fileAnswer {
	if mime == "image/webp" {
		// The standard library cannot decode WebP, so it cannot be scaled.
		if len(data) > imageMaxBytes {
			return unreadable("This WebP picture is %d KB and cannot be scaled down here, so it is not sent.", len(data)/1024)
		}
		return fileAnswer{Kind: "picture", Image: data, ImageMime: mime, ImageInfo: "WebP picture, sent as it is"}
	}
	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return unreadable("This picture is damaged or cut short, so it cannot be read here.")
	}
	if cfg.Width <= 0 || cfg.Height <= 0 || cfg.Width*cfg.Height > imageMaxPixels || cfg.Width > 60000 || cfg.Height > 60000 {
		return unreadable("This picture is %dx%d pixels, too large to scale down here, so it is not sent.", cfg.Width, cfg.Height)
	}
	if len(data) <= imageMaxBytes && max(cfg.Width, cfg.Height) <= imageMaxSide {
		return fileAnswer{Kind: "picture", Image: data, ImageMime: mime, ImageInfo: fmt.Sprintf("%dx%d pixels, sent as it is", cfg.Width, cfg.Height)}
	}
	var img image.Image
	switch mime {
	case "image/jpeg":
		img, err = jpeg.Decode(bytes.NewReader(data))
	case "image/png":
		img, err = png.Decode(bytes.NewReader(data))
	default:
		img, err = gif.Decode(bytes.NewReader(data))
	}
	if err != nil {
		return unreadable("This picture is damaged or cut short, so it cannot be read here.")
	}
	side := imageMaxSide
	for try := 0; try < 5; try++ {
		small := scaleDown(img, side)
		var buf bytes.Buffer
		if err := jpeg.Encode(&buf, small, &jpeg.Options{Quality: 80 - 5*try}); err != nil {
			break
		}
		if buf.Len() <= imageMaxBytes {
			b := small.Bounds()
			return fileAnswer{Kind: "picture", Image: buf.Bytes(), ImageMime: "image/jpeg",
				ImageInfo: fmt.Sprintf("%dx%d pixels, scaled down to %dx%d (JPEG, %d KB)", cfg.Width, cfg.Height, b.Dx(), b.Dy(), buf.Len()/1024)}
		}
		side = side * 2 / 3
	}
	return unreadable("This picture could not be scaled small enough to be sent, so it cannot be read here.")
}

// scaleDown averages the pixels of img into a picture whose longer side is at
// most side, laying transparent parts on white.
func scaleDown(img image.Image, side int) *image.RGBA {
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	nw, nh := w, h
	if max(w, h) > side {
		if w >= h {
			nw, nh = side, max(1, h*side/w)
		} else {
			nw, nh = max(1, w*side/h), side
		}
	}
	out := image.NewRGBA(image.Rect(0, 0, nw, nh))
	for y := 0; y < nh; y++ {
		y0, y1 := b.Min.Y+y*h/nh, b.Min.Y+(y+1)*h/nh
		if y1 <= y0 {
			y1 = y0 + 1
		}
		for x := 0; x < nw; x++ {
			x0, x1 := b.Min.X+x*w/nw, b.Min.X+(x+1)*w/nw
			if x1 <= x0 {
				x1 = x0 + 1
			}
			var r, g, bl, n uint64
			for yy := y0; yy < y1; yy++ {
				for xx := x0; xx < x1; xx++ {
					cr, cg, cb, ca := img.At(xx, yy).RGBA()
					r += uint64(cr) + uint64(0xffff-ca)
					g += uint64(cg) + uint64(0xffff-ca)
					bl += uint64(cb) + uint64(0xffff-ca)
					n++
				}
			}
			out.SetRGBA(x, y, color.RGBA{uint8(r / n >> 8), uint8(g / n >> 8), uint8(bl / n >> 8), 255})
		}
	}
	return out
}

// --- what an agent gets back ---

// cutFileText returns the part of text an agent gets: at most fileTextCap
// characters from offset, and how many characters lie behind the cut.
func cutFileText(text string, offset int) (shown string, left, total int, err error) {
	r := []rune(text)
	total = len(r)
	if offset < 0 || offset > total || (offset == total && total > 0) {
		return "", 0, total, fmt.Errorf("offset %d is beyond the end: the text has %d characters", offset, total)
	}
	end := min(offset+fileTextCap, total)
	return strings.TrimRight(string(r[offset:end]), " \t\r\n"), total - end, total, nil
}

func pageRange(a fileAnswer, from, to int) string {
	// The pages whose text lies inside the rune range [from, to).
	first, last := 0, 0
	for i, at := range a.PageAt {
		end := to
		if i+1 < len(a.PageAt) {
			end = a.PageAt[i+1]
		}
		if end > from && at < to {
			if first == 0 {
				first = a.PageNums[i]
			}
			last = a.PageNums[i]
		}
	}
	switch {
	case first == 0:
		return ""
	case first == last:
		return strconv.Itoa(first)
	}
	return fmt.Sprintf("%d-%d", first, last)
}

// pdfWithPageMarks puts "[page N]" before each page's text and moves the page
// offsets to match.
func pdfWithPageMarks(a fileAnswer) fileAnswer {
	var sb strings.Builder
	rs := []rune(a.Text)
	pos := 0
	at := make([]int, len(a.PageAt))
	for i, start := range a.PageAt {
		end := len(rs)
		if i+1 < len(a.PageAt) {
			end = a.PageAt[i+1]
		}
		at[i] = pos
		mark := fmt.Sprintf("[page %d]\n", a.PageNums[i])
		body := strings.TrimSpace(string(rs[start:end]))
		if body == "" {
			body = "(no text on this page)"
		}
		sb.WriteString(mark)
		sb.WriteString(body)
		sb.WriteString("\n\n")
		pos = utf8.RuneCountInString(sb.String())
	}
	a.Text = strings.TrimRight(sb.String(), "\n")
	a.PageAt = at
	return a
}

func b64(b []byte) string { return base64.StdEncoding.EncodeToString(b) }
