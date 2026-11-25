package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"mime/quotedprintable"
	"net"
	"net/smtp"
	"net/textproto"
	"net/url"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// Sending a reply (SPEC 13, DESIGN 4.1 principle 5). This file only knows how
// to put a mail together and hand it to Gmail or to Microsoft; reply.go decides
// whether, to whom and in which thread, and is its only caller. Nothing here
// logs a recipient, a subject or any text.

// sendFailure is a send that did not end well, in words for the thread.
// Doubt says the mail may have gone anyway: the connection dropped after the
// mail was handed over and before the server said so. A send in doubt is
// never repeated.
type sendFailure struct {
	Text  string
	Doubt bool
	Code  int // the server's number, for the log; 0 when there was none
}

func (e *sendFailure) Error() string { return e.Text }

// --- putting a mail together (Gmail) ---

// replyMail is everything a Gmail reply holds besides the connection.
type replyMail struct {
	From       string
	To, Cc     []string
	Subject    string // the subject of the thread, as it stands
	InReplyTo  string
	References []string
	Body       string // plain text, as the owner approved it
	Now        time.Time
	MessageID  string
}

// newMessageID makes the Message-ID of a mail the app sends, on the domain of
// the connected address.
func newMessageID(from string) string {
	b := make([]byte, 12)
	rand.Read(b)
	domain := "localhost"
	if i := strings.LastIndex(from, "@"); i >= 0 && i+1 < len(from) {
		domain = from[i+1:]
	}
	return "<" + hex.EncodeToString(b) + "@" + domain + ">"
}

var replyPrefixRE = regexp.MustCompile(`(?i)^(re|odp|aw|sv)(\[\d+\])?\s*:`)

// cleanHeaderText makes text from a mail safe to be a header: no line breaks
// or other control characters, one space between words, valid UTF-8.
func cleanHeaderText(s string) string {
	s = strings.ToValidUTF8(s, "")
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || r == ' ' || r == ' ' {
			return ' '
		}
		return r
	}, s)
	return strings.Join(strings.Fields(s), " ")
}

// replySubject is the subject of a reply: the thread's subject with Re: in
// front of it once.
func replySubject(s string) string {
	s = cleanHeaderText(s)
	if s == "" {
		return "Re:"
	}
	if replyPrefixRE.MatchString(s) {
		return s
	}
	return "Re: " + s
}

func isASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x80 {
			return false
		}
	}
	return true
}

const headerWidth = 76

// foldWords breaks a header at its spaces so that no line is longer than
// headerWidth where a space allows it. first is the length of what stands in
// front of the value on the first line ("Subject: ").
func foldWords(prefix, value, sep string) string {
	var b strings.Builder
	line := len(prefix)
	b.WriteString(prefix)
	for i, w := range strings.Split(value, sep) {
		if i > 0 {
			if line+len(sep)+len(w) > headerWidth {
				b.WriteString(strings.TrimRight(sep, " ") + "\r\n ")
				line = 1
			} else {
				b.WriteString(sep)
				line += len(sep)
			}
		}
		b.WriteString(w)
		line += len(w)
	}
	return b.String()
}

// encodedWords writes a text that is not plain ASCII as RFC 2047 words, each
// so short that the line stays within headerWidth, one word per line.
func encodedWords(prefix, value string) string {
	const maxBytes = 36 // 48 base64 characters, a word of 60 with its frame
	var words []string
	for len(value) > 0 {
		n := min(maxBytes, len(value))
		for n < len(value) && !utf8.RuneStart(value[n]) {
			n--
		}
		words = append(words, "=?utf-8?b?"+base64.StdEncoding.EncodeToString([]byte(value[:n]))+"?=")
		value = value[n:]
	}
	return prefix + strings.Join(words, "\r\n ")
}

func headerLine(name, value string) string {
	value = cleanHeaderText(value)
	if isASCII(value) {
		return foldWords(name+": ", value, " ")
	}
	return encodedWords(name+": ", value)
}

func addrHeader(name string, addrs []string) string {
	return foldWords(name+": ", strings.Join(addrs, ", "), ", ")
}

var msgIDRE = regexp.MustCompile(`^<[^<>\s]{1,300}>$`)

// msgIDsOf reads the message ids out of a References or In-Reply-To value.
func msgIDsOf(s string) []string {
	var out []string
	for _, f := range strings.Fields(s) {
		if msgIDRE.MatchString(f) {
			out = append(out, f)
		}
	}
	return out
}

// buildReplyMail is the Gmail reply as bytes, with CRLF line ends: headers in
// ASCII (what is not ASCII is encoded), the text as quoted-printable UTF-8 so
// no line is longer than the standard allows, and no quoted original.
func buildReplyMail(m replyMail) []byte {
	var h []string
	h = append(h, "From: "+m.From)
	h = append(h, addrHeader("To", m.To))
	if len(m.Cc) > 0 {
		h = append(h, addrHeader("Cc", m.Cc))
	}
	h = append(h, headerLine("Subject", replySubject(m.Subject)))
	h = append(h, "Date: "+m.Now.Format(time.RFC1123Z))
	h = append(h, "Message-ID: "+m.MessageID)
	if m.InReplyTo != "" {
		h = append(h, "In-Reply-To: "+m.InReplyTo)
	}
	if len(m.References) > 0 {
		h = append(h, foldWords("References: ", strings.Join(m.References, " "), " "))
	}
	h = append(h, "MIME-Version: 1.0", "Content-Type: text/plain; charset=utf-8", "Content-Transfer-Encoding: quoted-printable")
	var body bytes.Buffer
	w := quotedprintable.NewWriter(&body)
	w.Write([]byte(strings.ReplaceAll(m.Body, "\r\n", "\n")))
	w.Close()
	end := "\r\n"
	if strings.HasSuffix(body.String(), "\r\n") {
		end = ""
	}
	return []byte(strings.Join(h, "\r\n") + "\r\n\r\n" + body.String() + end)
}

// --- handing it to Gmail ---

// smtpDeliver sends one mail over port 587 with STARTTLS (the lab's host
// blocks 465). Everything up to the end of the data is a plain failure: the
// mail did not go. When the data was handed over and the server's answer never
// came, the failure is in doubt, and nothing may send the mail again.
func smtpDeliver(cfg gmailConfig, sec gmailSecret, rcpts []string, msg []byte) *sendFailure {
	fail := func(err error) *sendFailure {
		f := &sendFailure{Text: plainError("Gmail", err, sec.Password)}
		var te *textproto.Error
		if errors.As(err, &te) {
			f.Code = te.Code
			f.Text = oneLine(scrub(fmt.Sprintf("Gmail refused it: %d %s", te.Code, te.Msg), sec.Password), 300)
		}
		return f
	}
	d := net.Dialer{Timeout: 15 * time.Second}
	raw, err := d.Dial("tcp", cfg.SMTPAddr)
	if err != nil {
		return fail(err)
	}
	raw.SetDeadline(time.Now().Add(cfg.Timeout))
	c, err := smtp.NewClient(raw, cfg.SMTPHost)
	if err != nil {
		raw.Close()
		return fail(err)
	}
	defer c.Close()
	if ok, _ := c.Extension("STARTTLS"); !ok {
		return &sendFailure{Text: "The mail server offers no encrypted connection, so nothing was sent."}
	}
	var roots *tls.Config
	if cfg.TLS != nil {
		roots = cfg.TLS
	}
	conf := &tls.Config{ServerName: cfg.SMTPHost, MinVersion: tls.VersionTLS12}
	if roots != nil {
		conf.RootCAs = roots.RootCAs
	}
	if err := c.StartTLS(conf); err != nil {
		return fail(err)
	}
	if err := c.Auth(smtp.PlainAuth("", sec.Address, sec.Password, cfg.SMTPHost)); err != nil {
		f := fail(err)
		f.Text = "Gmail did not accept the sign-in, so nothing was sent. Connect Gmail again on the Connectors page."
		return f
	}
	if err := c.Mail(sec.Address); err != nil {
		return fail(err)
	}
	for _, r := range rcpts {
		if err := c.Rcpt(r); err != nil {
			return fail(err)
		}
	}
	w, err := c.Data()
	if err != nil {
		return fail(err)
	}
	if _, err := w.Write(msg); err != nil {
		return fail(err)
	}
	// From here the server may have the whole mail.
	if err := w.Close(); err != nil {
		f := fail(err)
		var te *textproto.Error
		if !errors.As(err, &te) {
			f.Doubt = true
			f.Text = "The connection to Gmail ended before it said whether it took the mail. It may have gone: check the Sent folder before you try again."
		}
		return f
	}
	c.Quit() // the mail is taken; whatever Quit says changes nothing
	return nil
}

// --- handing it to Microsoft ---

type graphRcpt struct {
	EmailAddress struct {
		Address string `json:"address"`
	} `json:"emailAddress"`
}

func graphRcpts(list []string) []graphRcpt {
	out := make([]graphRcpt, 0, len(list))
	for _, a := range list {
		var g graphRcpt
		g.EmailAddress.Address = a
		out = append(out, g)
	}
	return out
}

// graphReplyBody is the body of Graph's reply call: the text as plain text and
// the people it goes to, which the server worked out. There is no quoted
// original and nothing else in it.
func graphReplyBody(text string, to, cc []string) map[string]any {
	return map[string]any{"message": map[string]any{
		"body":         map[string]any{"contentType": "Text", "content": text},
		"toRecipients": graphRcpts(to),
		"ccRecipients": graphRcpts(cc),
	}}
}

// msFailure words a refusal from Microsoft for the thread and says whether
// the reply may have gone.
func msFailure(err error) *sendFailure {
	var me *msError
	if errors.As(err, &me) && me.Status >= 400 && me.Status < 500 {
		f := &sendFailure{Code: me.Status}
		switch {
		case me.Status == 403:
			f.Text = "Office mail did not let ONE send: the sign-in has no permission to send mail (Mail.Send). Press Sign in on the Connectors page and accept it, then ask for the reply again."
		case me.Status == 404:
			f.Text = "Office mail no longer has the message to answer, so nothing was sent."
		case me.Status == 401:
			f.Text = "Office mail did not accept the sign-in, so nothing was sent. Press Sign in on the Connectors page."
		case me.Status == 429:
			f.Text = "Office mail asked ONE to wait, so nothing was sent."
		default:
			f.Text = plainMS("Office mail refused it", err)
		}
		return f
	}
	f := &sendFailure{Doubt: true, Text: "The connection to Office mail ended before it said whether it took the reply. It may have gone: check the Sent folder before you try again."}
	if errors.As(err, &me) {
		f.Code = me.Status
	}
	return f
}

// graphReply answers one message through Graph's reply call: the one write
// the app makes to the Office mailbox. A refused token is renewed once; that
// is safe because a refused token sent nothing.
func (a *app) graphReply(ctx context.Context, graphID, text string, to, cc []string) *sendFailure {
	token, _, err := a.graphAccess(ctx)
	if err != nil {
		if errors.Is(err, errNeedsLogin) {
			return &sendFailure{Text: "Office mail needs a new sign-in, so nothing was sent. Press Sign in on the Connectors page."}
		}
		return &sendFailure{Text: plainMS("Office mail could not be reached, so nothing was sent", err)}
	}
	path := "/me/messages/" + url.PathEscape(graphID) + "/reply"
	body := graphReplyBody(text, to, cc)
	err = a.graph.apiPrefer(ctx, token, "POST", path, graphImmutable, body, nil)
	var me *msError
	if errors.As(err, &me) && me.Status == 401 {
		if token, _, terr := a.graphToken(ctx, true); terr == nil {
			err = a.graph.apiPrefer(ctx, token, "POST", path, graphImmutable, body, nil)
		}
	}
	if err != nil {
		return msFailure(err)
	}
	return nil
}
