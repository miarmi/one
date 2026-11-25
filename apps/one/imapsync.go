package main

import (
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf16"
)

// The calls the import needs, on top of the small client in imap.go. All of
// them only read: the folder is opened with EXAMINE and bodies are fetched
// with BODY.PEEK, so no flag, label or folder is ever changed.

// mailboxInfo is what EXAMINE says about the folder.
type mailboxInfo struct {
	UIDValidity   uint32
	UIDNext       uint32
	HighestModSeq uint64 // 0 when the server does not keep one
	Exists        int
}

// examine opens a folder read-only with CONDSTORE switched on.
func (ic *imapConn) examine(folder string) (mailboxInfo, error) {
	res, err := ic.do("EXAMINE " + quote(folder) + " (CONDSTORE)")
	if err != nil {
		return mailboxInfo{}, err
	}
	var mi mailboxInfo
	for _, u := range res.untagged {
		if n, ok := existsCount([]string{u}); ok {
			mi.Exists = n
			continue
		}
		i := strings.Index(u, "[")
		j := strings.Index(u, "]")
		if !strings.HasPrefix(u, "OK ") || i < 0 || j < i {
			continue
		}
		f := strings.Fields(u[i+1 : j])
		if len(f) != 2 {
			continue
		}
		n, err := strconv.ParseUint(f[1], 10, 64)
		if err != nil {
			continue
		}
		switch strings.ToUpper(f[0]) {
		case "UIDVALIDITY":
			mi.UIDValidity = uint32(n)
		case "UIDNEXT":
			mi.UIDNext = uint32(n)
		case "HIGHESTMODSEQ":
			mi.HighestModSeq = n
		}
	}
	if mi.UIDValidity == 0 {
		return mi, errors.New("the server did not say which UID validity the folder has")
	}
	return mi, nil
}

// uidSearch runs UID SEARCH and returns the numbers it found, ascending.
func (ic *imapConn) uidSearch(criteria string) ([]uint32, error) {
	res, err := ic.do("UID SEARCH " + criteria)
	if err != nil {
		return nil, err
	}
	var out []uint32
	for _, u := range res.untagged {
		f := strings.Fields(u)
		if len(f) == 0 || !strings.EqualFold(f[0], "SEARCH") {
			continue
		}
		for _, w := range f[1:] {
			n, err := strconv.ParseUint(w, 10, 32)
			if err != nil {
				continue
			}
			out = append(out, uint32(n))
		}
	}
	slices.Sort(out)
	return out, nil
}

func uidList(uids []uint32) string {
	parts := make([]string, len(uids))
	for i, u := range uids {
		parts[i] = strconv.FormatUint(uint64(u), 10)
	}
	return strings.Join(parts, ",")
}

// msgMeta is what one message says about itself without its body.
type msgMeta struct {
	UID      uint32
	MsgID    string // X-GM-MSGID
	ThreadID string // X-GM-THRID
	Labels   []string
	Flags    []string
	Date     time.Time // INTERNALDATE
	Size     int64
	ModSeq   uint64
	Header   map[string]string // raw header values, lower-case keys
}

func (m msgMeta) hasFlag(f string) bool {
	for _, x := range m.Flags {
		if strings.EqualFold(x, f) {
			return true
		}
	}
	return false
}

func (m msgMeta) hasLabel(l string) bool {
	for _, x := range m.Labels {
		if strings.EqualFold(x, l) {
			return true
		}
	}
	return false
}

// metaFields is what the import asks Gmail for about every message.
const metaItems = "UID X-GM-MSGID X-GM-THRID X-GM-LABELS FLAGS INTERNALDATE RFC822.SIZE MODSEQ BODY.PEEK[HEADER.FIELDS (FROM TO CC SUBJECT DATE MESSAGE-ID X-GITHUB-REASON)]"

// fetchMeta reads the metadata of up to a page of messages.
func (ic *imapConn) fetchMeta(uids []uint32) ([]msgMeta, error) {
	if len(uids) == 0 {
		return nil, nil
	}
	res, err := ic.do("UID FETCH " + uidList(uids) + " (" + metaItems + ")")
	if err != nil {
		return nil, err
	}
	var out []msgMeta
	for _, u := range res.untagged {
		if m, ok := parseMeta(u); ok {
			out = append(out, m)
		}
	}
	return out, nil
}

// parseMeta reads "5 FETCH (UID 9 X-GM-MSGID 1 ... )".
func parseMeta(u string) (msgMeta, bool) {
	_, rest, ok := strings.Cut(u, " ")
	if !ok {
		return msgMeta{}, false
	}
	word, rest, _ := strings.Cut(rest, " ")
	if !strings.EqualFold(word, "FETCH") {
		return msgMeta{}, false
	}
	vals, err := parseValues(rest)
	if err != nil || len(vals) != 1 || vals[0].kind != 'l' {
		return msgMeta{}, false
	}
	var m msgMeta
	l := vals[0].list
	for i := 0; i+1 < len(l); i += 2 {
		key := strings.ToUpper(l[i].atom)
		v := l[i+1]
		switch {
		case key == "UID":
			n, _ := strconv.ParseUint(v.text(), 10, 32)
			m.UID = uint32(n)
		case key == "X-GM-MSGID":
			m.MsgID = v.text()
		case key == "X-GM-THRID":
			m.ThreadID = v.text()
		case key == "X-GM-LABELS":
			for _, x := range v.list {
				m.Labels = append(m.Labels, decodeMUTF7(x.text()))
			}
		case key == "FLAGS":
			for _, x := range v.list {
				m.Flags = append(m.Flags, x.text())
			}
		case key == "INTERNALDATE":
			m.Date, _ = time.Parse("_2-Jan-2006 15:04:05 -0700", v.text())
		case key == "RFC822.SIZE":
			m.Size, _ = strconv.ParseInt(v.text(), 10, 64)
		case key == "MODSEQ":
			if len(v.list) == 1 {
				m.ModSeq, _ = strconv.ParseUint(v.list[0].text(), 10, 64)
			}
		case strings.HasPrefix(key, "BODY["):
			m.Header = parseHeaderRaw(v.text())
		}
	}
	if m.UID == 0 || m.MsgID == "" {
		return msgMeta{}, false
	}
	return m, true
}

// fetchChanged calls fn for every message whose flags or labels changed
// after modseq (CONDSTORE). Answers are handled one line at a time.
func (ic *imapConn) fetchChanged(modseq uint64, fn func(msgMeta) error) error {
	cmd := fmt.Sprintf("UID FETCH 1:* (UID X-GM-MSGID X-GM-THRID X-GM-LABELS FLAGS MODSEQ) (CHANGEDSINCE %d)", modseq)
	return ic.doEach(cmd, func(line string) error {
		if m, ok := parseMeta(line); ok {
			return fn(m)
		}
		return nil
	})
}

// doEach sends a command and hands every untagged line to fn as it arrives.
func (ic *imapConn) doEach(cmd string, fn func(string) error) error {
	ic.tag++
	tag := fmt.Sprintf("A%03d", ic.tag)
	if _, err := io.WriteString(ic.c, tag+" "+cmd+"\r\n"); err != nil {
		return err
	}
	var fnErr error
	for {
		line, err := ic.readLogical()
		if err != nil {
			return err
		}
		if strings.HasPrefix(line, "* ") {
			if fnErr == nil {
				fnErr = fn(line[2:])
			}
			continue
		}
		if strings.HasPrefix(line, tag+" ") {
			st, txt, _ := strings.Cut(line[len(tag)+1:], " ")
			if !strings.EqualFold(st, "OK") {
				return &errIMAPRefused{Text: txt}
			}
			return fnErr
		}
		if strings.HasPrefix(line, "+") {
			return errors.New("the server asked for more than we sent")
		}
	}
}

// fetchBody streams one message's full original bytes into w without ever
// holding more than a buffer of them. It returns how many bytes arrived.
func (ic *imapConn) fetchBody(uid uint32, w io.Writer) (int64, error) {
	return ic.fetchSection(uid, "BODY.PEEK[]", w)
}

// fetchPrefix reads the first n bytes of a message without marking it read.
func (ic *imapConn) fetchPrefix(uid uint32, n int, w io.Writer) (int64, error) {
	return ic.fetchSection(uid, fmt.Sprintf("BODY.PEEK[]<0.%d>", n), w)
}

func (ic *imapConn) fetchSection(uid uint32, what string, w io.Writer) (int64, error) {
	ic.tag++
	tag := fmt.Sprintf("A%03d", ic.tag)
	if _, err := io.WriteString(ic.c, fmt.Sprintf("%s UID FETCH %d (%s)\r\n", tag, uid, what)); err != nil {
		return 0, err
	}
	var got int64
	found := false
	for {
		line, err := ic.r.ReadString('\n')
		if err != nil {
			return got, err
		}
		line = strings.TrimRight(line, "\r\n")
		if strings.HasPrefix(line, "* ") && strings.HasSuffix(line, "}") && !found {
			i := strings.LastIndex(line, "{")
			n, err := strconv.ParseInt(line[i+1:len(line)-1], 10, 64)
			if i < 0 || err != nil || n < 0 {
				return got, errors.New("the server sent a message in a form we cannot read")
			}
			got, err = io.CopyN(w, ic.r, n)
			if err != nil {
				return got, err
			}
			found = true
			// The rest of the line (the closing bracket).
			if _, err := ic.r.ReadString('\n'); err != nil {
				return got, err
			}
			continue
		}
		if strings.HasPrefix(line, tag+" ") {
			st, txt, _ := strings.Cut(line[len(tag)+1:], " ")
			if !strings.EqualFold(st, "OK") {
				return got, &errIMAPRefused{Text: txt}
			}
			if !found {
				return got, errors.New("the server did not send the message")
			}
			return got, nil
		}
	}
}

// decodeMUTF7 turns an IMAP modified UTF-7 name (how Gmail writes labels
// with accents) into text. Anything it cannot read is left as it was.
func decodeMUTF7(s string) string {
	if !strings.Contains(s, "&") {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); {
		if s[i] != '&' {
			b.WriteByte(s[i])
			i++
			continue
		}
		j := strings.IndexByte(s[i:], '-')
		if j < 0 {
			b.WriteString(s[i:])
			break
		}
		enc := s[i+1 : i+j]
		i += j + 1
		if enc == "" {
			b.WriteByte('&')
			continue
		}
		raw, err := base64.RawStdEncoding.DecodeString(strings.ReplaceAll(enc, ",", "/"))
		if err != nil || len(raw)%2 != 0 {
			b.WriteString("&" + enc + "-")
			continue
		}
		u := make([]uint16, len(raw)/2)
		for k := range u {
			u[k] = uint16(raw[2*k])<<8 | uint16(raw[2*k+1])
		}
		b.WriteString(string(utf16.Decode(u)))
	}
	return b.String()
}
