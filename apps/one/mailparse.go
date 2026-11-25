package main

import (
	"bufio"
	"encoding/base64"
	"io"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net/mail"
	"net/textproto"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/encoding/htmlindex"
)

// Reading a stored message. Everything here works on a stream: the parts of
// a message are read one after another, text is kept up to a cap, and every
// file part is handed to a sink that writes it to disk. Nothing in a mail is
// ever fetched from an address it names.

const (
	maxTextBytes = 1 << 20 // per text part, before it is turned into text
	maxParts     = 300     // parts recorded per message
	maxDepth     = 12      // how deep multipart messages may nest
)

// partInfo describes one file inside a message.
type partInfo struct {
	Part        string // 1, 1.2, 1.2.1 ...
	Filename    string
	ContentType string
	Disposition string // attachment or inline
	ContentID   string
	Size        int64
	SHA         string
	Path        string
}

// fileSink receives the decoded bytes of one file part.
type fileSink interface {
	save(p *partInfo, r io.Reader) error
}

// parsedMail is what the import keeps of a message besides its bytes.
type parsedMail struct {
	Subject   string
	FromName  string
	FromAddr  string
	To, Cc    []string // addresses, lower case
	MessageID string
	Date      time.Time
	Text      string
	Parts     []partInfo
}

func (m parsedMail) hasFiles() bool { return len(m.Parts) > 0 }

func charsetReader(label string, input io.Reader) (io.Reader, error) {
	enc, err := htmlindex.Get(label)
	if err != nil {
		return nil, err
	}
	return enc.NewDecoder().Reader(input), nil
}

var wordDecoder = &mime.WordDecoder{CharsetReader: charsetReader}

// decodeHeaderText decodes encoded words in a header value; a value it
// cannot decode is kept as it is.
func decodeHeaderText(s string) string {
	if out, err := wordDecoder.DecodeHeader(s); err == nil {
		s = out
	}
	return cleanText(s)
}

// cleanText makes sure a text is valid UTF-8 and has no control characters
// other than line breaks and tabs.
func cleanText(s string) string {
	s = strings.ToValidUTF8(s, "�")
	return strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' {
			return r
		}
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, s)
}

var addrParser = mail.AddressParser{WordDecoder: wordDecoder}

func parseAddress(s string) (name, addr string) {
	a, err := addrParser.Parse(unfold(s))
	if err != nil {
		return "", ""
	}
	return decodeName(a.Name), strings.ToLower(a.Address)
}

// unfold joins a header value that was broken over several lines.
func unfold(s string) string {
	return strings.NewReplacer("\r\n", "", "\n", "", "\r", "").Replace(s)
}

// decodeName decodes a display name that is still an encoded word. Many
// senders put the encoded word in quotes, which the address parser leaves
// as it is.
func decodeName(s string) string {
	if strings.Contains(s, "=?") {
		return decodeHeaderText(s)
	}
	return cleanText(s)
}

func parseAddressList(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	l, err := addrParser.ParseList(unfold(s))
	if err != nil {
		return nil
	}
	out := make([]string, 0, len(l))
	for _, a := range l {
		out = append(out, strings.ToLower(a.Address))
	}
	return out
}

// lenient turns a decoding error into the end of the data, so one damaged
// part cannot stop an import.
type lenient struct{ r io.Reader }

func (l lenient) Read(p []byte) (int, error) {
	n, err := l.r.Read(p)
	if err != nil && err != io.EOF {
		err = io.EOF
	}
	return n, err
}

func decodeTransfer(cte string, r io.Reader) io.Reader {
	switch strings.ToLower(strings.TrimSpace(cte)) {
	case "base64":
		return lenient{base64.NewDecoder(base64.StdEncoding, lenient{newSpaceStripper(r)})}
	case "quoted-printable":
		return lenient{quotedprintable.NewReader(r)}
	}
	return r
}

// spaceStripper drops blanks inside base64 text, which some programs write.
type spaceStripper struct{ r io.Reader }

func newSpaceStripper(r io.Reader) io.Reader { return spaceStripper{r} }

func (s spaceStripper) Read(p []byte) (int, error) {
	n, err := s.r.Read(p)
	j := 0
	for i := 0; i < n; i++ {
		if p[i] != ' ' && p[i] != '\t' {
			p[j] = p[i]
			j++
		}
	}
	return j, err
}

func readTextPart(r io.Reader, charset string) string {
	r = io.LimitReader(r, maxTextBytes)
	cs := strings.ToLower(strings.TrimSpace(charset))
	if cs != "" && cs != "utf-8" && cs != "utf8" && cs != "us-ascii" {
		if dr, err := charsetReader(cs, r); err == nil {
			r = lenient{dr}
		}
	}
	b, _ := io.ReadAll(r)
	return strings.ReplaceAll(cleanText(string(b)), "\r\n", "\n")
}

type walker struct {
	sink   fileSink
	plain  []string
	html   []string
	parts  []partInfo
	nParts int
	err    error
}

// parseMail reads one message from r. A failure of the sink stops the read
// and is returned; damage in the message itself only loses that part.
func parseMail(r io.Reader, sink fileSink) (parsedMail, error) {
	var pm parsedMail
	msg, err := mail.ReadMessage(bufio.NewReaderSize(r, 32<<10))
	if err != nil {
		return pm, err
	}
	h := msg.Header
	pm.Subject = decodeHeaderText(h.Get("Subject"))
	pm.FromName, pm.FromAddr = parseAddress(h.Get("From"))
	pm.To = parseAddressList(h.Get("To"))
	pm.Cc = parseAddressList(h.Get("Cc"))
	pm.MessageID = strings.TrimSpace(h.Get("Message-Id"))
	if d, err := mail.ParseDate(h.Get("Date")); err == nil {
		pm.Date = d
	}
	w := &walker{sink: sink}
	w.walk(textproto.MIMEHeader(h), msg.Body, "1", 0)
	pm.Parts = w.parts
	if w.err != nil {
		return pm, w.err
	}
	text := strings.TrimSpace(strings.Join(w.plain, "\n\n"))
	if text == "" && len(w.html) > 0 {
		text = htmlToText(strings.Join(w.html, "\n"))
	}
	// A plain part that is HTML (no content type, or a program that wrote the
	// page into the text part) is turned into text too, twice when the markup
	// was escaped once more on the way.
	for k := 0; k < 2 && startsWithMarkup(text); k++ {
		text = htmlToText(text)
	}
	pm.Text = text
	return pm, nil
}

func (w *walker) walk(h textproto.MIMEHeader, body io.Reader, path string, depth int) {
	if w.err != nil {
		return
	}
	ct, params, err := mime.ParseMediaType(h.Get("Content-Type"))
	if err != nil || ct == "" {
		ct, params = "text/plain", map[string]string{}
	}
	disp, dparams, _ := mime.ParseMediaType(h.Get("Content-Disposition"))
	disp = strings.ToLower(disp)
	if strings.HasPrefix(ct, "multipart/") && params["boundary"] != "" && depth < maxDepth {
		mr := multipart.NewReader(body, params["boundary"])
		for i := 1; w.err == nil; i++ {
			p, err := mr.NextRawPart()
			if err != nil {
				return
			}
			w.walk(p.Header, p, path+"."+strconv.Itoa(i), depth+1)
		}
		return
	}
	name := dparams["filename"]
	if name == "" {
		name = params["name"]
	}
	name = cleanFilename(name)
	decoded := decodeTransfer(h.Get("Content-Transfer-Encoding"), body)
	isText := ct == "text/plain" || ct == "text/html"
	if isText && disp != "attachment" && name == "" {
		t := readTextPart(decoded, params["charset"])
		if ct == "text/html" {
			w.html = append(w.html, t)
		} else {
			w.plain = append(w.plain, t)
		}
		return
	}
	if w.nParts >= maxParts {
		return
	}
	w.nParts++
	kind := "attachment"
	cid := strings.Trim(strings.TrimSpace(h.Get("Content-Id")), "<>")
	if disp == "inline" || (disp == "" && cid != "") {
		kind = "inline"
	}
	if name == "" {
		name = "part-" + path
	}
	info := partInfo{Part: path, Filename: name, ContentType: ct, Disposition: kind, ContentID: cleanText(cid)}
	if err := w.sink.save(&info, decoded); err != nil {
		w.err = err
		return
	}
	w.parts = append(w.parts, info)
}

// cleanFilename keeps the name a person would see: decoded, no folders, no
// control characters, at most 200 characters.
func cleanFilename(s string) string {
	s = decodeHeaderText(s)
	s = strings.ReplaceAll(s, "\\", "/")
	if i := strings.LastIndex(s, "/"); i >= 0 {
		s = s[i+1:]
	}
	s = strings.TrimSpace(s)
	if utf8.RuneCountInString(s) > 200 {
		s = string([]rune(s)[:200])
	}
	return s
}

// countingSink reads a file part to its end without keeping it: the first
// pass over a message, which only learns what files it holds.
type countingSink struct{}

func (countingSink) save(p *partInfo, r io.Reader) error {
	n, _ := io.Copy(io.Discard, r)
	p.Size = n
	return nil
}

// poolSink is the second pass: it writes every file part into the pool.
type poolSink struct {
	p       pool
	bytes   int64 // bytes newly written, files that were already there not counted
	saved   int
	failure error
}

func (s *poolSink) save(p *partInfo, r io.Reader) error {
	sha, rel, size, created, err := s.p.write(r)
	if err != nil {
		s.failure = err
		return err
	}
	p.SHA, p.Path, p.Size = sha, rel, size
	if created {
		s.bytes += size
	}
	s.saved++
	return nil
}
