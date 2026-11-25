package main

import (
	"bufio"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// fakeBox is the mailbox behind the fake Gmail: messages with UIDs, Gmail
// message and thread ids, labels, flags and a change counter, as Gmail's
// All Mail has them. It answers only reading commands.

type fakeMsg struct {
	uid    uint32
	msgid  string
	thrid  string
	labels []string
	flags  []string
	date   time.Time
	header string // the whole header block, ending with a blank line
	body   []byte
	gen    func(w io.Writer) // a body too large to keep in memory; written on demand
	size   int64
	modseq uint64
}

type fakeBox struct {
	mu       sync.Mutex
	validity uint32
	next     uint32
	modseq   uint64
	nextID   uint64
	msgs     []*fakeMsg

	failAfter  int // drop the connection at the next body fetch once this many were served; 0 = never
	bodyServed int
	bodyUIDs   []uint32 // every UID whose body was fetched
	peekUIDs   []uint32 // every UID whose first bytes were read (a dropped message keeps its beginning)
	onBody     func()   // called, without the lock, after each body is served
	onMeta     func()   // called, without the lock, after each read of headers and flags
	metaSets   []string // the UID set of every read of headers and flags
}

func newFakeBox() *fakeBox { return &fakeBox{validity: 7, next: 1, modseq: 100, nextID: 1000} }

func (b *fakeBox) total() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.msgs)
}

func (b *fakeBox) metaReads() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.metaSets...)
}

func (b *fakeBox) peeked() []uint32 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]uint32(nil), b.peekUIDs...)
}

func (b *fakeBox) fetchedBodies() []uint32 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]uint32(nil), b.bodyUIDs...)
}

// testMail describes one test message.
type testMail struct {
	thrid   string
	from    string
	to      string
	cc      string
	subject string
	date    time.Time
	labels  []string
	flags   []string
	text    string   // text/plain body
	html    string   // text/html body
	extra   []string // more header lines
	attach  []attachment
	raw     string // a complete message, used as it is
}

type attachment struct {
	name    string
	ctype   string
	content []byte
	inline  bool
	cid     string
}

func (m testMail) bytes() (header string, body []byte) {
	if m.raw != "" {
		h, b, _ := strings.Cut(m.raw, "\r\n\r\n")
		return h + "\r\n\r\n", []byte(b)
	}
	var h strings.Builder
	fmt.Fprintf(&h, "From: %s\r\nTo: %s\r\n", m.from, m.to)
	if m.cc != "" {
		fmt.Fprintf(&h, "Cc: %s\r\n", m.cc)
	}
	fmt.Fprintf(&h, "Subject: %s\r\nDate: %s\r\nMessage-ID: <%s@example.test>\r\n", m.subject, m.date.Format(time.RFC1123Z), strconv.FormatInt(m.date.UnixNano(), 36))
	for _, x := range m.extra {
		h.WriteString(x + "\r\n")
	}
	h.WriteString("MIME-Version: 1.0\r\n")
	switch {
	case len(m.attach) > 0:
		h.WriteString("Content-Type: multipart/mixed; boundary=\"BND\"\r\n\r\n")
		var b strings.Builder
		b.WriteString("--BND\r\n")
		if m.html != "" && m.text == "" {
			b.WriteString("Content-Type: text/html; charset=utf-8\r\n\r\n" + m.html + "\r\n")
		} else {
			b.WriteString("Content-Type: text/plain; charset=utf-8\r\n\r\n" + m.text + "\r\n")
		}
		for _, a := range m.attach {
			disp := "attachment"
			if a.inline {
				disp = "inline"
			}
			b.WriteString("--BND\r\nContent-Type: " + a.ctype + "; name=\"" + a.name + "\"\r\n")
			b.WriteString("Content-Disposition: " + disp + "; filename=\"" + a.name + "\"\r\n")
			if a.cid != "" {
				b.WriteString("Content-ID: <" + a.cid + ">\r\n")
			}
			b.WriteString("Content-Transfer-Encoding: base64\r\n\r\n")
			enc := base64.StdEncoding.EncodeToString(a.content)
			for len(enc) > 76 {
				b.WriteString(enc[:76] + "\r\n")
				enc = enc[76:]
			}
			b.WriteString(enc + "\r\n")
		}
		b.WriteString("--BND--\r\n")
		return h.String(), []byte(b.String())
	case m.html != "" && m.text == "":
		h.WriteString("Content-Type: text/html; charset=utf-8\r\n\r\n")
		return h.String(), []byte(m.html + "\r\n")
	}
	h.WriteString("Content-Type: text/plain; charset=utf-8\r\n\r\n")
	return h.String(), []byte(m.text + "\r\n")
}

// add puts a message in the box as the newest.
func (b *fakeBox) add(m testMail) *fakeMsg {
	h, body := m.bytes()
	b.mu.Lock()
	defer b.mu.Unlock()
	b.nextID++
	b.modseq++
	if m.date.IsZero() {
		m.date = time.Now()
	}
	fm := &fakeMsg{uid: b.next, msgid: strconv.FormatUint(b.nextID, 10), thrid: m.thrid, labels: m.labels, flags: m.flags,
		date: m.date, header: h, body: body, size: int64(len(h) + len(body)), modseq: b.modseq}
	if fm.thrid == "" {
		fm.thrid = "t" + fm.msgid
	}
	b.next++
	b.msgs = append(b.msgs, fm)
	return fm
}

// addGenerated adds a message whose body is produced while it is served.
func (b *fakeBox) addGenerated(m testMail, gen func(w io.Writer)) *fakeMsg {
	fm := b.add(m)
	b.mu.Lock()
	defer b.mu.Unlock()
	cw := &countWriter{}
	gen(cw)
	fm.body, fm.gen = nil, gen
	fm.size = int64(len(fm.header)) + cw.n
	return fm
}

type countWriter struct{ n int64 }

func (c *countWriter) Write(p []byte) (int, error) { c.n += int64(len(p)); return len(p), nil }

// change sets new flags and labels on a message, as Gmail would after a click.
func (b *fakeBox) change(msgid string, flags, labels []string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, m := range b.msgs {
		if m.msgid == msgid {
			b.modseq++
			m.flags, m.labels, m.modseq = flags, labels, b.modseq
		}
	}
}

func (b *fakeBox) examine(c net.Conn, tag string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	fmt.Fprintf(c, "* %d EXISTS\r\n* OK [UIDVALIDITY %d] UIDs valid\r\n* OK [UIDNEXT %d] Predicted next UID\r\n* OK [HIGHESTMODSEQ %d] Highest\r\n%s OK [READ-ONLY] [Gmail]/All Mail selected. (Success)\r\n",
		len(b.msgs), b.validity, b.next, b.modseq, tag)
}

func base64Encode(dst, src []byte) { base64.StdEncoding.Encode(dst, src) }

func imapQuote(s string) string { return `"` + strings.ReplaceAll(s, `\`, `\\`) + `"` }

func (m *fakeMsg) metaLine(seq int, withHeader bool, names string) string {
	var lb, fl []string
	for _, l := range m.labels {
		lb = append(lb, imapQuote(l))
	}
	for _, f := range m.flags {
		fl = append(fl, f)
	}
	line := fmt.Sprintf("* %d FETCH (UID %d X-GM-MSGID %s X-GM-THRID %s X-GM-LABELS (%s) FLAGS (%s) MODSEQ (%d)",
		seq, m.uid, m.msgid, m.thrid, strings.Join(lb, " "), strings.Join(fl, " "), m.modseq)
	if withHeader {
		h := pickHeaders(m.header, strings.Fields(strings.ToUpper(names)))
		line += fmt.Sprintf(" INTERNALDATE \"%s\" RFC822.SIZE %d BODY[HEADER.FIELDS (%s)] {%d}\r\n%s", m.date.Format("2-Jan-2006 15:04:05 -0700"), m.size, names, len(h), h)
	}
	return line + ")\r\n"
}

func pickHeaders(header string, names []string) string {
	var out strings.Builder
	keep := false
	for _, l := range strings.Split(strings.ReplaceAll(header, "\r\n", "\n"), "\n") {
		if l == "" {
			continue
		}
		if l[0] == ' ' || l[0] == '\t' {
			if keep {
				out.WriteString(l + "\r\n")
			}
			continue
		}
		k, _, _ := strings.Cut(l, ":")
		keep = false
		for _, n := range names {
			if strings.EqualFold(k, n) {
				keep = true
			}
		}
		if keep {
			out.WriteString(l + "\r\n")
		}
	}
	return out.String() + "\r\n"
}

// parseUIDSet reads "5", "5,7,9" or "1:*".
func (b *fakeBox) matchSet(set string) []*fakeMsg {
	var out []*fakeMsg
	for _, m := range b.msgs {
		for _, part := range strings.Split(set, ",") {
			lo, hi, isRange := strings.Cut(part, ":")
			l, _ := strconv.ParseUint(lo, 10, 32)
			h := l
			if isRange {
				if hi == "*" {
					h = 1 << 32
				} else {
					h, _ = strconv.ParseUint(hi, 10, 32)
				}
			}
			if uint64(m.uid) >= l && uint64(m.uid) <= h {
				out = append(out, m)
				break
			}
		}
	}
	return out
}

// uid answers one UID command. It returns false when it dropped the connection.
func (b *fakeBox) uid(c net.Conn, tag, cmd string) bool {
	rest := strings.TrimSpace(cmd[len("UID "):])
	verb, args, _ := strings.Cut(rest, " ")
	b.mu.Lock()
	defer b.mu.Unlock()
	switch strings.ToUpper(verb) {
	case "SEARCH":
		var found []uint32
		switch {
		case strings.HasPrefix(strings.ToUpper(args), "SINCE "):
			d, err := time.Parse("2-Jan-2006", strings.TrimSpace(args[6:]))
			if err != nil {
				fmt.Fprintf(c, "%s BAD bad date\r\n", tag)
				return true
			}
			for _, m := range b.msgs {
				if !m.date.UTC().Before(d) {
					found = append(found, m.uid)
				}
			}
		case strings.HasPrefix(strings.ToUpper(args), "UID "):
			set := strings.TrimSpace(args[4:])
			ms := b.matchSet(set)
			if len(ms) == 0 && strings.HasSuffix(set, ":*") && len(b.msgs) > 0 {
				ms = []*fakeMsg{b.msgs[len(b.msgs)-1]} // n:* always includes the last message
			}
			for _, m := range ms {
				found = append(found, m.uid)
			}
		case strings.HasPrefix(strings.ToUpper(args), "X-GM-MSGID "):
			id := strings.TrimSpace(args[len("X-GM-MSGID "):])
			for _, m := range b.msgs {
				if m.msgid == id {
					found = append(found, m.uid)
				}
			}
		default:
			fmt.Fprintf(c, "%s BAD unsupported search\r\n", tag)
			return true
		}
		sort.Slice(found, func(i, j int) bool { return found[i] < found[j] })
		var sb strings.Builder
		sb.WriteString("* SEARCH")
		for _, u := range found {
			fmt.Fprintf(&sb, " %d", u)
		}
		fmt.Fprintf(c, "%s\r\n%s OK Search done\r\n", sb.String(), tag)
		return true
	case "FETCH":
		set, items, _ := strings.Cut(args, " ")
		up := strings.ToUpper(items)
		switch {
		case strings.Contains(up, "BODY.PEEK[]<0."):
			// The beginning of a message, for the line a dropped message keeps.
			ms := b.matchSet(set)
			if len(ms) != 1 {
				fmt.Fprintf(c, "%s NO no such message\r\n", tag)
				return true
			}
			n := 0
			fmt.Sscanf(up[strings.Index(up, "<0.")+3:], "%d", &n)
			b.peekUIDs = append(b.peekUIDs, ms[0].uid)
			data := append([]byte(ms[0].header), ms[0].body...)
			if n > 0 && len(data) > n {
				data = data[:n]
			}
			fmt.Fprintf(c, "* %d FETCH (UID %d BODY[]<0> {%d}\r\n", ms[0].uid, ms[0].uid, len(data))
			c.Write(data)
			fmt.Fprintf(c, ")\r\n%s OK Success\r\n", tag)
			return true
		case strings.Contains(up, "BODY.PEEK[]"):
			ms := b.matchSet(set)
			if len(ms) != 1 {
				fmt.Fprintf(c, "%s NO no such message\r\n", tag)
				return true
			}
			if b.failAfter > 0 && b.bodyServed >= b.failAfter {
				b.failAfter = 0
				c.Close()
				return false
			}
			b.bodyServed++
			m := ms[0]
			b.bodyUIDs = append(b.bodyUIDs, m.uid)
			w := bufio.NewWriterSize(c, 32<<10)
			fmt.Fprintf(w, "* %d FETCH (UID %d BODY[] {%d}\r\n", m.uid, m.uid, m.size)
			io.WriteString(w, m.header)
			if m.gen != nil {
				m.gen(w)
			} else {
				w.Write(m.body)
			}
			fmt.Fprintf(w, ")\r\n%s OK Success\r\n", tag)
			w.Flush()
			if b.onBody != nil {
				b.mu.Unlock()
				b.onBody()
				b.mu.Lock()
			}
			return true
		case strings.Contains(up, "CHANGEDSINCE"):
			i := strings.Index(up, "CHANGEDSINCE ")
			n, _ := strconv.ParseUint(strings.TrimRight(strings.TrimSpace(up[i+len("CHANGEDSINCE "):]), ")"), 10, 64)
			for k, m := range b.msgs {
				if m.modseq > n {
					io.WriteString(c, m.metaLine(k+1, false, ""))
				}
			}
			fmt.Fprintf(c, "%s OK Fetch done\r\n", tag)
			return true
		default:
			names := "FROM TO CC SUBJECT DATE MESSAGE-ID X-GITHUB-REASON"
			if i := strings.Index(up, "HEADER.FIELDS ("); i >= 0 {
				if j := strings.Index(up[i:], ")"); j > 0 {
					names = up[i+len("HEADER.FIELDS (") : i+j]
				}
			}
			for k, m := range b.msgs {
				for _, x := range b.matchSet(set) {
					if x == m {
						io.WriteString(c, m.metaLine(k+1, true, names))
					}
				}
			}
			fmt.Fprintf(c, "%s OK Success\r\n", tag)
			b.metaSets = append(b.metaSets, set)
			if b.onMeta != nil {
				b.mu.Unlock()
				b.onMeta()
				b.mu.Lock()
			}
			return true
		}
	}
	fmt.Fprintf(c, "%s BAD unsupported command\r\n", tag)
	return true
}
