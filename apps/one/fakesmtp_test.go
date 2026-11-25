package main

import (
	"bufio"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeSMTP is a scripted Gmail submission server on "port 587": it greets in
// the clear, offers STARTTLS, takes AUTH PLAIN after it, and keeps every mail
// it was handed. Its mode makes it misbehave in one way.
type fakeSMTP struct {
	addr string
	user string
	pass string

	mu    sync.Mutex
	mode  string // "", "rejectRcpt", "dropAfterData", "rejectData", "noStarttls", "badAuth"
	mails []smtpMail
	conns int
	cmds  []string // the commands received, with no data and no credentials
}

// smtpMail is one mail the fake accepted (or was handed, in a mode that did not accept it).
type smtpMail struct {
	From  string
	Rcpts []string
	Data  string // as it arrived, with CRLF line ends and dot-stuffing removed
}

func (f *fakeSMTP) setMode(m string) {
	f.mu.Lock()
	f.mode = m
	f.mu.Unlock()
}

func (f *fakeSMTP) handed() []smtpMail {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]smtpMail(nil), f.mails...)
}

func (f *fakeSMTP) commands() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.cmds...)
}

func startFakeSMTP(t *testing.T, host, user, pass string) (*fakeSMTP, *x509.Certificate) {
	t.Helper()
	cert, _ := testCert(t, host)
	crt, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	f := &fakeSMTP{addr: ln.Addr().String(), user: user, pass: pass}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go f.serve(c, &tls.Config{Certificates: []tls.Certificate{cert}})
		}
	}()
	return f, crt
}

func (f *fakeSMTP) serve(raw net.Conn, conf *tls.Config) {
	defer raw.Close()
	raw.SetDeadline(time.Now().Add(10 * time.Second))
	f.mu.Lock()
	f.conns++
	mode := f.mode
	f.mu.Unlock()
	var c net.Conn = raw
	rd := bufio.NewReader(c)
	say := func(s string) { fmt.Fprintf(c, "%s\r\n", s) }
	say("220 smtp.fake ESMTP ready")
	secure, authed := false, false
	var cur smtpMail
	for {
		line, err := rd.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimRight(line, "\r\n")
		verb := strings.ToUpper(line)
		if i := strings.IndexByte(verb, ' '); i > 0 {
			verb = verb[:i]
		}
		if verb != "AUTH" {
			f.mu.Lock()
			f.cmds = append(f.cmds, verb)
			f.mu.Unlock()
		}
		switch verb {
		case "EHLO", "HELO":
			if secure {
				say("250-smtp.fake")
				say("250-AUTH PLAIN")
				say("250 8BITMIME")
			} else if mode == "noStarttls" {
				say("250 smtp.fake")
			} else {
				say("250-smtp.fake")
				say("250 STARTTLS")
			}
		case "STARTTLS":
			say("220 go ahead")
			tc := tls.Server(raw, conf)
			if err := tc.Handshake(); err != nil {
				return
			}
			c, rd, secure = tc, bufio.NewReader(tc), true
		case "AUTH":
			parts := strings.Fields(line)
			if !secure || len(parts) < 3 {
				say("530 5.7.0 Must issue a STARTTLS command first")
				continue
			}
			b, _ := base64.StdEncoding.DecodeString(parts[2])
			f2 := strings.Split(string(b), "\x00")
			if mode == "badAuth" || len(f2) != 3 || f2[1] != f.user || f2[2] != f.pass {
				say("535 5.7.8 Username and Password not accepted")
				continue
			}
			authed = true
			say("235 2.7.0 Accepted")
		case "MAIL":
			if !authed {
				say("530 5.7.0 Authentication Required")
				continue
			}
			cur = smtpMail{From: between(line, "<", ">")}
			say("250 2.1.0 OK")
		case "RCPT":
			if mode == "rejectRcpt" {
				say("550 5.1.1 The email account that you tried to reach does not exist")
				continue
			}
			cur.Rcpts = append(cur.Rcpts, between(line, "<", ">"))
			say("250 2.1.5 OK")
		case "DATA":
			say("354 Go ahead")
			var b strings.Builder
			for {
				l, err := rd.ReadString('\n')
				if err != nil {
					return
				}
				if l == ".\r\n" {
					break
				}
				b.WriteString(strings.TrimPrefix(l, ".")) // dot-stuffing off
			}
			cur.Data = b.String()
			switch mode {
			case "dropAfterData":
				f.mu.Lock()
				f.mails = append(f.mails, cur)
				f.mu.Unlock()
				return // the mail is in, and the client never hears
			case "rejectData":
				say("554 5.7.1 Message rejected as spam")
				continue
			}
			f.mu.Lock()
			f.mails = append(f.mails, cur)
			f.mu.Unlock()
			say("250 2.0.0 OK queued")
		case "QUIT":
			say("221 2.0.0 bye")
			return
		default:
			say("502 5.5.1 not implemented")
		}
	}
}

func between(s, a, b string) string {
	i := strings.Index(s, a)
	j := strings.LastIndex(s, b)
	if i < 0 || j <= i {
		return ""
	}
	return s[i+1 : j]
}
