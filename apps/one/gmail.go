package main

import (
	"crypto/tls"
	"errors"
	"fmt"
	"mime"
	"net"
	"net/mail"
	"net/smtp"
	"strings"
	"time"
	"unicode"
)

// Trial 1: can the server read and send the owner's Gmail with its own sign-in?
// Decided for this trial: IMAP and SMTP with a Google app password, no OAuth.

const (
	gmailIMAPAddr = "imap.gmail.com:993"
	gmailSMTPHost = "smtp.gmail.com"
	gmailSMTPAddr = "smtp.gmail.com:587"
	gmailConn     = "gmail"
	testMailBody  = "This is a test mail from ONE, sent to yourself to check that the server can send.\r\n"
)

type gmailSecret struct {
	Address  string `json:"address"`
	Password string `json:"password"`
}

// gmailConfig holds the addresses so tests can point at fakes.
type gmailConfig struct {
	IMAPAddr string
	SMTPAddr string
	SMTPHost string
	TLS      *tls.Config
	Timeout  time.Duration
}

func defaultGmailConfig() gmailConfig {
	return gmailConfig{
		IMAPAddr: gmailIMAPAddr,
		SMTPAddr: gmailSMTPAddr,
		SMTPHost: gmailSMTPHost,
		TLS:      &tls.Config{ServerName: "imap.gmail.com", MinVersion: tls.VersionTLS12},
		Timeout:  25 * time.Second,
	}
}

// cleanAddress accepts one plain mail address and nothing else: no name, no
// list, no control characters, so it can go into a header safely.
func cleanAddress(s string) (string, bool) {
	s = strings.TrimSpace(s)
	if s == "" || len(s) > 200 {
		return "", false
	}
	for _, r := range s {
		if unicode.IsControl(r) || unicode.IsSpace(r) || r == '<' || r == '>' || r == ',' || r == ';' {
			return "", false
		}
	}
	a, err := mail.ParseAddress(s)
	if err != nil || a.Address != s {
		return "", false
	}
	return s, true
}

// cleanAppPassword removes the spaces Google shows in an app password.
func cleanAppPassword(s string) string {
	return strings.Join(strings.Fields(s), "")
}

func decodeWords(s string) string {
	d := mime.WordDecoder{}
	if out, err := d.DecodeHeader(s); err == nil {
		return out
	}
	return s
}

// scrub removes every secret from a text before it is shown or logged.
func scrub(text string, secrets ...string) string {
	for _, s := range secrets {
		if len(s) >= 4 {
			text = strings.ReplaceAll(text, s, "***")
		}
	}
	return text
}

// oneLine makes a piece of mail content safe to show in one line.
func oneLine(s string, max int) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, s)
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > max {
		s = string(r[:max]) + "…"
	}
	return s
}

// plainError tells a network or server failure in words a person can act on.
func plainError(what string, err error, secrets ...string) string {
	var ref *errIMAPRefused
	var ne net.Error
	msg := ""
	switch {
	case errors.As(err, &ref):
		msg = what + " refused: " + ref.Text
	case errors.As(err, &ne) && ne.Timeout():
		msg = what + " did not answer in time."
	default:
		msg = what + " could not be reached or answered something unexpected: " + err.Error()
	}
	return oneLine(scrub(msg, secrets...), 300)
}

// gmailTest signs in and reads; it returns the lines to show and whether it
// worked.
func gmailTest(cfg gmailConfig, sec gmailSecret, loc *time.Location) (string, bool) {
	s, err := readGmail(cfg, sec.Address, sec.Password, 5)
	if err != nil {
		return plainError("Gmail", err, sec.Password), false
	}
	var b strings.Builder
	b.WriteString("Sign-in ok.\n")
	fmt.Fprintf(&b, "Messages in %s: %d\n", oneLine(s.Folder, 80), s.Total)
	if len(s.Messages) == 0 {
		b.WriteString("No messages to show.\n")
	}
	for _, m := range s.Messages {
		date := "date unknown"
		if !m.Date.IsZero() {
			date = m.Date.In(loc).Format("2 Jan 2006 15:04")
		}
		labels := "none"
		if len(m.Labels) > 0 {
			ls := make([]string, len(m.Labels))
			for i, l := range m.Labels {
				ls[i] = oneLine(l, 40)
			}
			labels = strings.Join(ls, ", ")
		}
		fmt.Fprintf(&b, "%s | %s | %s | thread %s | labels: %s\n", date, oneLine(m.From, 80), oneLine(m.Subject, 100), oneLine(m.ThreadID, 30), labels)
	}
	return strings.TrimRight(b.String(), "\n"), true
}

// buildTestMail makes the one test mail. The only address it takes is the
// connected one, used as sender and as the only recipient.
func buildTestMail(self string, now time.Time) (recipients []string, msg []byte) {
	hdr := []string{
		"From: " + self,
		"To: " + self,
		"Subject: ONE test mail",
		"Date: " + now.Format(time.RFC1123Z),
		"MIME-Version: 1.0",
		"Content-Type: text/plain; charset=utf-8",
		"Content-Transfer-Encoding: 7bit",
	}
	return []string{self}, []byte(strings.Join(hdr, "\r\n") + "\r\n\r\n" + testMailBody)
}

// gmailSend sends the test mail to the connected address only. There is no
// parameter for another recipient.
func gmailSend(cfg gmailConfig, sec gmailSecret, now time.Time) error {
	to, msg := buildTestMail(sec.Address, now)
	d := net.Dialer{Timeout: 15 * time.Second}
	raw, err := d.Dial("tcp", cfg.SMTPAddr)
	if err != nil {
		return err
	}
	// Port 587 with STARTTLS: the lab's host blocks outgoing port 465.
	raw.SetDeadline(time.Now().Add(cfg.Timeout))
	c, err := smtp.NewClient(raw, cfg.SMTPHost)
	if err != nil {
		raw.Close()
		return err
	}
	defer c.Close()
	if ok, _ := c.Extension("STARTTLS"); !ok {
		return errors.New("the mail server offers no encrypted connection")
	}
	if err := c.StartTLS(&tls.Config{ServerName: cfg.SMTPHost, MinVersion: tls.VersionTLS12, RootCAs: cfg.TLS.RootCAs}); err != nil {
		return err
	}
	if err := c.Auth(smtp.PlainAuth("", sec.Address, sec.Password, cfg.SMTPHost)); err != nil {
		return err
	}
	if err := c.Mail(sec.Address); err != nil {
		return err
	}
	for _, r := range to {
		if err := c.Rcpt(r); err != nil {
			return err
		}
	}
	w, err := c.Data()
	if err != nil {
		return err
	}
	if _, err := w.Write(msg); err != nil {
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}
	return c.Quit()
}
