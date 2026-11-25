package main

import (
	"bufio"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	testAddr = "owner@example.test"
	testPass = "abcdefghijklmnop"
)

func newTestApp(t *testing.T) (*app, http.Handler) {
	t.Helper()
	dir := t.TempDir()
	st, err := openStore(filepath.Join(dir, "one.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	a := newApp(st, dir, "", "")
	return a, a.routes()
}

func post(h http.Handler, path string, form url.Values) *httptest.ResponseRecorder {
	req := httptest.NewRequest("POST", path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func get(h http.Handler, path string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
	return rec
}

// testCert makes a self-signed certificate for the fake mail server.
func testCert(t *testing.T, host string) (tls.Certificate, *x509.CertPool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: host},
		DNSNames:     []string{host},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, _ := x509.ParseCertificate(der)
	pool := x509.NewCertPool()
	pool.AddCert(cert)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, pool
}

// fakeIMAP is a scripted Gmail: it knows one account and answers the few
// commands the trial sends. It records the commands (never the password).
type fakeIMAP struct {
	addr string
	mu   sync.Mutex
	cmds []string
	box  *fakeBox // when set, the fake serves this mailbox with UIDs, Gmail ids and CONDSTORE
}

func (f *fakeIMAP) record(cmd string) {
	f.mu.Lock()
	f.cmds = append(f.cmds, cmd)
	f.mu.Unlock()
}

// commands returns a copy of everything the client sent (never the password).
func (f *fakeIMAP) commands() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.cmds...)
}

func startFakeIMAP(t *testing.T, cert tls.Certificate, user, pass string) *fakeIMAP {
	t.Helper()
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{cert}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	f := &fakeIMAP{addr: ln.Addr().String()}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go f.serve(c, user, pass)
		}
	}()
	return f
}

func header(from, subj string) string {
	return "From: " + from + "\r\nSubject: " + subj + "\r\nDate: Thu, 19 Nov 2026 10:00:00 +0100\r\n\r\n"
}

func (f *fakeIMAP) serve(c net.Conn, user, pass string) {
	defer c.Close()
	rd := bufio.NewReader(c)
	fmt.Fprint(c, "* OK Gimap ready\r\n")
	for {
		line, err := rd.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimRight(line, "\r\n")
		tag, cmd, _ := strings.Cut(line, " ")
		verb, _, _ := strings.Cut(cmd, " ")
		switch strings.ToUpper(verb) {
		case "LOGIN":
			f.record("LOGIN")
			if strings.Contains(cmd, `"`+user+`" "`+pass+`"`) {
				fmt.Fprintf(c, "%s OK %s authenticated (Success)\r\n", tag, user)
			} else {
				fmt.Fprintf(c, "%s NO [AUTHENTICATIONFAILED] Invalid credentials (Failure)\r\n", tag)
			}
		case "LIST":
			f.record("LIST")
			fmt.Fprintf(c, "* LIST (\\HasNoChildren) \"/\" \"INBOX\"\r\n* LIST (\\HasNoChildren \\All) \"/\" \"[Gmail]/All Mail\"\r\n%s OK Success\r\n", tag)
		case "EXAMINE":
			f.record(cmd)
			if f.box != nil {
				f.box.examine(c, tag)
				continue
			}
			fmt.Fprintf(c, "* 42 EXISTS\r\n%s OK [READ-ONLY] [Gmail]/All Mail selected. (Success)\r\n", tag)
		case "UID":
			f.record(cmd)
			if f.box == nil || !f.box.uid(c, tag, cmd) {
				return
			}
		case "FETCH":
			f.record(cmd)
			for seq := 38; seq <= 42; seq++ {
				h := header(fmt.Sprintf("Sender %d <s%d@example.test>", seq, seq), fmt.Sprintf("Subject %d", seq))
				if seq == 42 {
					h = header("=?UTF-8?Q?Jos=C3=A9_Nov=C3=A1k?= <jose@example.test>", "=?UTF-8?B?xI1lc2vDvSDDunZvZA==?=")
				}
				fmt.Fprintf(c, "* %d FETCH (INTERNALDATE \"%d-Nov-2026 10:00:00 +0100\" X-GM-THRID %d%d X-GM-LABELS (\"\\\\Inbox\" Work \"My Label\") BODY[HEADER.FIELDS (FROM SUBJECT DATE)] {%d}\r\n%s)\r\n", seq, seq-20, 1000, seq, len(h), h)
			}
			fmt.Fprintf(c, "%s OK Success\r\n", tag)
		case "LOGOUT":
			fmt.Fprintf(c, "* BYE\r\n%s OK done\r\n", tag)
			return
		default:
			f.record(cmd)
			fmt.Fprintf(c, "%s BAD unknown\r\n", tag)
		}
	}
}

// useFakeGmail points the app at a fake IMAP server and returns it.
func useFakeGmail(t *testing.T, a *app) *fakeIMAP {
	t.Helper()
	cert, pool := testCert(t, "imap.gmail.com")
	f := startFakeIMAP(t, cert, testAddr, testPass)
	a.gmail.IMAPAddr = f.addr
	a.gmail.TLS = &tls.Config{ServerName: "imap.gmail.com", RootCAs: pool, MinVersion: tls.VersionTLS12}
	a.gmail.Timeout = 5 * time.Second
	return f
}
