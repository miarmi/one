package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// installBinary checks a new labd and swaps it in, keeping the old one as
// labd.prev. The caller exits afterwards so systemd starts the new binary.
func installBinary(ctx context.Context, cfg *Config, uploaded string) (string, error) {
	target := cfg.paths.binary
	if err := os.Chmod(uploaded, 0o755); err != nil {
		return "", err
	}
	out, err := run(ctx, 15*time.Second, uploaded, "-version")
	if err != nil {
		return "", fmt.Errorf("new labd failed -version: %w", err)
	}
	if !strings.HasPrefix(out, "labd ") {
		return "", fmt.Errorf("new binary does not look like labd: %q", out)
	}
	if cfg.path != "" {
		if _, err := run(ctx, 15*time.Second, uploaded, "-selftest", "-config", cfg.path); err != nil {
			return "", fmt.Errorf("new labd failed -selftest: %w", err)
		}
	}
	prev := target + ".prev"
	if _, err := os.Stat(target); err == nil {
		os.Remove(prev)
		if err := os.Link(target, prev); err != nil {
			return "", fmt.Errorf("keeping the old binary: %w", err)
		}
	}
	if err := os.Rename(uploaded, target); err != nil {
		return "", err
	}
	if err := syncDir(filepath.Dir(target)); err != nil {
		return "", err
	}
	return out, nil
}

// receiveFile streams a request body into a temp file in dir, returning its
// path and SHA-256. It fails if the body exceeds limit.
func receiveFile(r io.Reader, dir string, limit int64) (string, string, error) {
	f, err := os.CreateTemp(dir, ".labd-upload-*")
	if err != nil {
		return "", "", err
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, h), io.LimitReader(r, limit+1))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil && n > limit {
		err = errTooLarge
	}
	if err != nil {
		os.Remove(f.Name())
		return "", "", err
	}
	return f.Name(), hex.EncodeToString(h.Sum(nil)), nil
}

var errTooLarge = errors.New("request body too large")
