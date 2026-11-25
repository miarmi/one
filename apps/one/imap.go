package main

import (
	"bufio"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"time"
)

// A very small IMAP client: just what the Gmail trial needs (log in, list
// folders, read one folder's size and the newest messages with Gmail's
// thread id and labels). It only reads: it uses EXAMINE and BODY.PEEK, so
// nothing on the mailbox is changed, not even a read flag.

// errIMAPRefused: the server answered NO or BAD; the text is the server's.
type errIMAPRefused struct{ Text string }

func (e *errIMAPRefused) Error() string { return e.Text }

// node is one parsed IMAP value: an atom, a string or a list.
type node struct {
	atom string
	str  string
	list []node
	kind byte // 'a' atom, 's' string, 'l' list
}

func (n node) text() string {
	if n.kind == 's' {
		return n.str
	}
	return n.atom
}

type imapConn struct {
	c   net.Conn
	r   *bufio.Reader
	tag int
}

// dialIMAP opens an implicit-TLS connection and reads the greeting.
func dialIMAP(addr string, cfg *tls.Config, timeout time.Duration) (*imapConn, error) {
	d := net.Dialer{Timeout: 15 * time.Second}
	raw, err := d.Dial("tcp", addr)
	if err != nil {
		return nil, err
	}
	tc := tls.Client(raw, cfg)
	raw.SetDeadline(time.Now().Add(timeout))
	if err := tc.Handshake(); err != nil {
		raw.Close()
		return nil, err
	}
	ic := &imapConn{c: tc, r: bufio.NewReaderSize(tc, 64<<10)}
	if timeout > 0 {
		// The timeout counts from the last bytes moved, so a long message
		// that keeps arriving is not cut off, and a silent server is.
		ic = &imapConn{c: idleConn{Conn: tc, idle: timeout}}
		ic.r = bufio.NewReaderSize(ic.c, 64<<10)
	}
	line, err := ic.readLogical()
	if err != nil {
		tc.Close()
		return nil, err
	}
	if !strings.HasPrefix(line, "* OK") && !strings.HasPrefix(line, "* PREAUTH") {
		tc.Close()
		return nil, &errIMAPRefused{Text: "the server did not greet us"}
	}
	return ic, nil
}

func (ic *imapConn) Close() error { return ic.c.Close() }

// idleConn gives every read and write its own deadline.
type idleConn struct {
	net.Conn
	idle time.Duration
}

func (c idleConn) Read(p []byte) (int, error) {
	c.Conn.SetReadDeadline(time.Now().Add(c.idle))
	return c.Conn.Read(p)
}

func (c idleConn) Write(p []byte) (int, error) {
	c.Conn.SetWriteDeadline(time.Now().Add(c.idle))
	return c.Conn.Write(p)
}

// readLogical reads one response line. A literal ({n}) is read and put
// into the line as a quoted string, so the parser sees one flat line.
func (ic *imapConn) readLogical() (string, error) {
	var out strings.Builder
	for {
		line, err := ic.r.ReadString('\n')
		if err != nil {
			if errors.Is(err, io.EOF) && line == "" {
				return "", io.ErrUnexpectedEOF
			}
			if line == "" {
				return "", err
			}
		}
		line = strings.TrimRight(line, "\r\n")
		if i := strings.LastIndex(line, "{"); i >= 0 && strings.HasSuffix(line, "}") {
			if n, err := strconv.Atoi(line[i+1 : len(line)-1]); err == nil && n >= 0 && n <= 1<<20 {
				buf := make([]byte, n)
				if _, err := io.ReadFull(ic.r, buf); err != nil {
					return "", err
				}
				out.WriteString(line[:i])
				out.WriteString(quote(string(buf)))
				continue
			}
		}
		out.WriteString(line)
		return out.String(), nil
	}
}

func quote(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	return `"` + s + `"`
}

// response is what one command returned.
type response struct {
	untagged []string // the lines that began with "* "
	status   string   // OK, NO or BAD
	text     string   // the server's words after the status
}

// do sends one command and reads until its tagged answer. The command line
// is never logged or returned, because it can hold the password.
func (ic *imapConn) do(cmd string) (response, error) {
	ic.tag++
	tag := fmt.Sprintf("A%03d", ic.tag)
	if _, err := io.WriteString(ic.c, tag+" "+cmd+"\r\n"); err != nil {
		return response{}, err
	}
	var res response
	for {
		line, err := ic.readLogical()
		if err != nil {
			return res, err
		}
		if strings.HasPrefix(line, "* ") {
			res.untagged = append(res.untagged, line[2:])
			continue
		}
		if strings.HasPrefix(line, tag+" ") {
			rest := line[len(tag)+1:]
			st, txt, _ := strings.Cut(rest, " ")
			res.status, res.text = strings.ToUpper(st), txt
			if res.status != "OK" {
				return res, &errIMAPRefused{Text: txt}
			}
			return res, nil
		}
		// A continuation request or anything else is not expected here.
		if strings.HasPrefix(line, "+") {
			return res, errors.New("the server asked for more than we sent")
		}
	}
}

// parseValues turns "a (b "c d") e" into nodes.
func parseValues(s string) ([]node, error) {
	p := &parser{s: s}
	nodes, err := p.parseUntil(0)
	if err != nil {
		return nil, err
	}
	return nodes, nil
}

type parser struct {
	s string
	i int
}

func (p *parser) parseUntil(end byte) ([]node, error) {
	var out []node
	for p.i < len(p.s) {
		c := p.s[p.i]
		switch {
		case c == ' ' || c == '\t':
			p.i++
		case c == end && end != 0:
			p.i++
			return out, nil
		case c == '(':
			p.i++
			l, err := p.parseUntil(')')
			if err != nil {
				return nil, err
			}
			out = append(out, node{kind: 'l', list: l})
		case c == ')':
			return nil, errors.New("unbalanced answer")
		case c == '"':
			p.i++
			var b strings.Builder
			closed := false
			for p.i < len(p.s) {
				ch := p.s[p.i]
				if ch == '\\' && p.i+1 < len(p.s) {
					b.WriteByte(p.s[p.i+1])
					p.i += 2
					continue
				}
				if ch == '"' {
					p.i++
					closed = true
					break
				}
				b.WriteByte(ch)
				p.i++
			}
			if !closed {
				return nil, errors.New("unterminated string")
			}
			out = append(out, node{kind: 's', str: b.String()})
		default:
			start := p.i
			for p.i < len(p.s) {
				ch := p.s[p.i]
				if ch == '[' { // BODY[HEADER.FIELDS (...)] is one atom
					j := strings.IndexByte(p.s[p.i:], ']')
					if j < 0 {
						return nil, errors.New("unterminated bracket")
					}
					p.i += j + 1
					continue
				}
				if ch == ' ' || ch == '(' || ch == ')' || ch == '"' {
					break
				}
				p.i++
			}
			out = append(out, node{kind: 'a', atom: p.s[start:p.i]})
		}
	}
	if end != 0 {
		return nil, errors.New("unterminated list")
	}
	return out, nil
}

// folderWithAttr finds the folder carrying a special-use attribute such as
// \All, from LIST answers like: (\HasNoChildren \All) "/" "[Gmail]/All Mail".
func folderWithAttr(untagged []string, attr string) string {
	for _, u := range untagged {
		if !strings.HasPrefix(strings.ToUpper(u), "LIST ") {
			continue
		}
		vals, err := parseValues(u[5:])
		if err != nil || len(vals) < 3 || vals[0].kind != 'l' {
			continue
		}
		for _, a := range vals[0].list {
			if strings.EqualFold(a.atom, attr) {
				return vals[len(vals)-1].text()
			}
		}
	}
	return ""
}

// existsCount reads "* 1234 EXISTS" from a SELECT or EXAMINE answer.
func existsCount(untagged []string) (int, bool) {
	for _, u := range untagged {
		f := strings.Fields(u)
		if len(f) == 2 && strings.EqualFold(f[1], "EXISTS") {
			if n, err := strconv.Atoi(f[0]); err == nil {
				return n, true
			}
		}
	}
	return 0, false
}

// fetched is one message as the trial shows it.
type fetched struct {
	Seq      int
	Date     time.Time // arrival time at the server
	From     string
	Subject  string
	ThreadID string
	Labels   []string
}

// parseFetch reads "5 FETCH (X-GM-THRID 1 X-GM-LABELS (...) INTERNALDATE "..." BODY[...] "...")".
func parseFetch(u string) (fetched, bool) {
	numStr, rest, ok := strings.Cut(u, " ")
	if !ok {
		return fetched{}, false
	}
	seq, err := strconv.Atoi(numStr)
	if err != nil {
		return fetched{}, false
	}
	word, rest, _ := strings.Cut(rest, " ")
	if !strings.EqualFold(word, "FETCH") {
		return fetched{}, false
	}
	vals, err := parseValues(rest)
	if err != nil || len(vals) != 1 || vals[0].kind != 'l' {
		return fetched{}, false
	}
	f := fetched{Seq: seq}
	l := vals[0].list
	for i := 0; i+1 < len(l); i += 2 {
		key := strings.ToUpper(l[i].atom)
		v := l[i+1]
		switch {
		case key == "X-GM-THRID":
			f.ThreadID = v.text()
		case key == "X-GM-LABELS":
			for _, x := range v.list {
				f.Labels = append(f.Labels, x.text())
			}
		case key == "INTERNALDATE":
			f.Date, _ = time.Parse("2-Jan-2006 15:04:05 -0700", v.text())
		case strings.HasPrefix(key, "BODY["):
			hdr := parseHeader(v.text())
			f.From, f.Subject = hdr["from"], hdr["subject"]
			if f.Date.IsZero() {
				if d, err := time.Parse(time.RFC1123Z, hdr["date"]); err == nil {
					f.Date = d
				}
			}
		}
	}
	return f, true
}

// parseHeader reads the few header lines asked for, unfolds them and
// decodes encoded words. Keys are lower case.
func parseHeader(h string) map[string]string {
	out := parseHeaderRaw(h)
	for k, v := range out {
		out[k] = decodeWords(v)
	}
	return out
}

// parseHeaderRaw unfolds the header lines and leaves encoded words as they are.
func parseHeaderRaw(h string) map[string]string {
	out := map[string]string{}
	var key string
	for _, line := range strings.Split(strings.ReplaceAll(h, "\r\n", "\n"), "\n") {
		if line == "" {
			continue
		}
		if (line[0] == ' ' || line[0] == '\t') && key != "" {
			out[key] += " " + strings.TrimSpace(line)
			continue
		}
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		key = strings.ToLower(strings.TrimSpace(k))
		out[key] = strings.TrimSpace(v)
	}
	return out
}

// gmailSample is what Test now learns from Gmail.
type gmailSample struct {
	Folder   string
	Total    int
	Messages []fetched // newest first
}

// readGmail signs in, finds All Mail, counts it and reads the newest n.
func readGmail(cfg gmailConfig, user, pass string, n int) (gmailSample, error) {
	ic, err := dialIMAP(cfg.IMAPAddr, cfg.TLS, cfg.Timeout)
	if err != nil {
		return gmailSample{}, err
	}
	defer ic.Close()
	ic.c.SetDeadline(time.Now().Add(cfg.Timeout))
	if _, err := ic.do("LOGIN " + quote(user) + " " + quote(pass)); err != nil {
		return gmailSample{}, loginError(err)
	}
	var s gmailSample
	lst, err := ic.do(`LIST "" "*"`)
	if err != nil {
		return s, err
	}
	s.Folder = folderWithAttr(lst.untagged, `\All`)
	if s.Folder == "" {
		s.Folder = "[Gmail]/All Mail"
	}
	sel, err := ic.do("EXAMINE " + quote(s.Folder))
	if err != nil {
		return s, err
	}
	total, ok := existsCount(sel.untagged)
	if !ok {
		return s, errors.New("the server did not say how many messages the folder holds")
	}
	s.Total = total
	if total > 0 {
		from := total - n + 1
		if from < 1 {
			from = 1
		}
		res, err := ic.do(fmt.Sprintf("FETCH %d:%d (INTERNALDATE X-GM-THRID X-GM-LABELS BODY.PEEK[HEADER.FIELDS (FROM SUBJECT DATE)])", from, total))
		if err != nil {
			return s, err
		}
		for _, u := range res.untagged {
			if f, ok := parseFetch(u); ok {
				s.Messages = append(s.Messages, f)
			}
		}
		// Highest sequence number first = the newest to arrive.
		for i, j := 0, len(s.Messages)-1; i < j; i, j = i+1, j-1 {
			s.Messages[i], s.Messages[j] = s.Messages[j], s.Messages[i]
		}
	}
	ic.do("LOGOUT")
	return s, nil
}

// loginError keeps a refusal as such, so it can be told in plain words.
func loginError(err error) error {
	var ref *errIMAPRefused
	if errors.As(err, &ref) {
		return &errIMAPRefused{Text: ref.Text}
	}
	return err
}
