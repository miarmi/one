package main

import (
	"crypto/sha256"
	"encoding/hex"
	"hash"
	"io"
	"os"
	"path/filepath"
	"sync"
)

// The file pool: every original message and every attachment lives once, in
// a file named by the SHA-256 of its bytes. Writing is streamed: bytes go to
// a temporary file through a hash and are never held in memory as a whole.

type pool struct {
	root  string
	stats *poolStats
}

// poolStats lets tests prove the pool is fed in small chunks.
type poolStats struct {
	mu       sync.Mutex
	maxWrite int
	written  int64
}

func newPool(root string) pool { return pool{root: root, stats: &poolStats{}} }

func relPath(sha string) string { return sha[:2] + "/" + sha }

func (p pool) open(rel string) (*os.File, error) {
	return os.Open(filepath.Join(p.root, filepath.FromSlash(rel)))
}

// poolWriter is one file being written.
type poolWriter struct {
	p pool
	f *os.File
	h hash.Hash
	n int64
}

func (p pool) newWriter() (*poolWriter, error) {
	tmp := filepath.Join(p.root, "tmp")
	if err := os.MkdirAll(tmp, 0o700); err != nil {
		return nil, err
	}
	f, err := os.CreateTemp(tmp, "in-*")
	if err != nil {
		return nil, err
	}
	return &poolWriter{p: p, f: f, h: sha256.New()}, nil
}

func (w *poolWriter) Write(b []byte) (int, error) {
	n, err := w.f.Write(b)
	w.h.Write(b[:n])
	w.n += int64(n)
	st := w.p.stats
	st.mu.Lock()
	if len(b) > st.maxWrite {
		st.maxWrite = len(b)
	}
	st.written += int64(n)
	st.mu.Unlock()
	return n, err
}

// abort throws the half-written file away.
func (w *poolWriter) abort() {
	w.f.Close()
	os.Remove(w.f.Name())
}

// finish puts the file in its place. created is false when the same bytes
// were already in the pool; the new copy is then dropped.
func (w *poolWriter) finish() (sha, rel string, size int64, created bool, err error) {
	if err = w.f.Sync(); err != nil {
		w.abort()
		return
	}
	if err = w.f.Close(); err != nil {
		os.Remove(w.f.Name())
		return
	}
	sha = hex.EncodeToString(w.h.Sum(nil))
	rel = relPath(sha)
	size = w.n
	dst := filepath.Join(w.p.root, filepath.FromSlash(rel))
	if st, serr := os.Stat(dst); serr == nil && st.Size() == size {
		os.Remove(w.f.Name())
		return sha, rel, size, false, nil
	}
	if err = os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		os.Remove(w.f.Name())
		return
	}
	if err = os.Chmod(w.f.Name(), 0o600); err != nil {
		os.Remove(w.f.Name())
		return
	}
	if err = os.Rename(w.f.Name(), dst); err != nil {
		os.Remove(w.f.Name())
		return
	}
	return sha, rel, size, true, nil
}

// write streams a reader into the pool.
func (p pool) write(r io.Reader) (sha, rel string, size int64, created bool, err error) {
	w, err := p.newWriter()
	if err != nil {
		return
	}
	if _, err = io.Copy(w, r); err != nil {
		w.abort()
		return
	}
	return w.finish()
}
