package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Certs gives hosts their TLS certificates in domain mode (CONTRACT.md,
// "nginx"): one certificate per host name, so no key for the whole domain
// ever sits on the server and nothing but HTTP is needed to get one.
type Certs interface {
	// Paths returns host's certificate chain and key, if it has them.
	Paths(host string) (cert, key string, ok bool)
	// Ensure gets a certificate for host if it has none. It can take a while.
	Ensure(ctx context.Context, host string) error
	// Remove deletes host's certificate.
	Remove(ctx context.Context, host string) error
}

// certbotCerts gets certificates from Let's Encrypt with certbot's webroot
// method (nginx serves /.well-known/acme-challenge/ from webroot on every
// host). certbot.timer renews them; the deploy hook reloads nginx.
type certbotCerts struct {
	webroot string
	live    string // /etc/letsencrypt/live
	run     func(ctx context.Context, timeout time.Duration, name string, args ...string) (string, error)
	now     func() time.Time
	pause   time.Duration // between tries while certbot.timer holds certbot's lock

	busy   sync.Mutex // one certbot at a time: it refuses to run twice
	mu     sync.Mutex
	failed map[string]time.Time // host -> last failure, so Let's Encrypt isn't asked again too soon
}

// certRetry is how long a host whose certificate failed waits before the
// next try: Let's Encrypt allows only a few failed validations an hour.
const certRetry = 30 * time.Minute

func newCertbotCerts(webroot, live string) *certbotCerts {
	return &certbotCerts{webroot: webroot, live: live, run: run, now: time.Now, pause: 5 * time.Second, failed: map[string]time.Time{}}
}

// certbot runs certbot, waiting while another certbot (the renewal timer)
// holds its lock.
func (c *certbotCerts) certbot(ctx context.Context, timeout time.Duration, args ...string) error {
	c.busy.Lock()
	defer c.busy.Unlock()
	for try := 0; ; try++ {
		_, err := c.run(ctx, timeout, "certbot", args...)
		if err == nil || try == 11 || !strings.Contains(err.Error(), "Another instance of Certbot is already running") {
			return err
		}
		select {
		case <-ctx.Done():
			return err
		case <-time.After(c.pause):
		}
	}
}

func (c *certbotCerts) Paths(host string) (string, string, bool) {
	cert, key := filepath.Join(c.live, host, "fullchain.pem"), filepath.Join(c.live, host, "privkey.pem")
	return cert, key, fileExists(cert) && fileExists(key)
}

func (c *certbotCerts) Ensure(ctx context.Context, host string) error {
	if !safeHost(host) {
		return fmt.Errorf("refusing a certificate for %q", host)
	}
	if _, _, ok := c.Paths(host); ok {
		return nil
	}
	c.mu.Lock()
	last, failed := c.failed[host]
	c.mu.Unlock()
	if failed && c.now().Sub(last) < certRetry {
		return fmt.Errorf("the certificate for %s failed recently; trying again after %s", host, last.Add(certRetry).Format("15:04"))
	}
	err := c.certbot(ctx, 3*time.Minute, "certonly", "--webroot", "-w", c.webroot, "-d", host, "--cert-name", host,
		"--non-interactive", "--agree-tos", "--register-unsafely-without-email", "--keep-until-expiring",
		"--deploy-hook", "systemctl reload nginx")
	c.mu.Lock()
	defer c.mu.Unlock()
	if err != nil {
		c.failed[host] = c.now()
		return fmt.Errorf("getting a certificate for %s: %w", host, err)
	}
	delete(c.failed, host)
	return nil
}

func (c *certbotCerts) Remove(ctx context.Context, host string) error {
	if !safeHost(host) {
		return nil
	}
	if _, _, ok := c.Paths(host); !ok {
		return nil
	}
	return c.certbot(ctx, time.Minute, "delete", "--cert-name", host, "--non-interactive")
}

// selfsignedCerts makes its own certificates, for tests and machines that
// Let's Encrypt can't reach (config "certs": "selfsigned").
type selfsignedCerts struct {
	dir string
}

func (c *selfsignedCerts) Paths(host string) (string, string, bool) {
	cert, key := filepath.Join(c.dir, host, "fullchain.pem"), filepath.Join(c.dir, host, "privkey.pem")
	return cert, key, fileExists(cert) && fileExists(key)
}

func (c *selfsignedCerts) Ensure(_ context.Context, host string) error {
	if !safeHost(host) {
		return fmt.Errorf("refusing a certificate for %q", host)
	}
	if _, _, ok := c.Paths(host); ok {
		return nil
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 120))
	if err != nil {
		return err
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: host},
		DNSNames:     []string{host},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(90 * 24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return err
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return err
	}
	dir := filepath.Join(c.dir, host)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if err := writeFileAtomic(filepath.Join(dir, "privkey.pem"), pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		return err
	}
	return writeFileAtomic(filepath.Join(dir, "fullchain.pem"), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644)
}

func (c *selfsignedCerts) Remove(_ context.Context, host string) error {
	if !safeHost(host) {
		return nil
	}
	return os.RemoveAll(filepath.Join(c.dir, host))
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular()
}
