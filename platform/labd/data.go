package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// Limits for copying production's data into a new preview.
const (
	copyMaxFiles  = 200000
	copyMaxDepth  = 16
	copyKeepFree  = 1 << 30 // never let a copy bring free disk space below this
	secretsFolder = "secrets"
)

// The owner decided on 2026-11-22 that a preview keeps getting the whole of
// production's data even when it is real mail and files (ONE), so the limit
// is wide; the free disk (copyKeepFree) still stops a copy. A var for tests.
var copyMaxBytes int64 = 16 << 30

// Limits for promote-data, which moves an app's real data from a preview into
// production: real mail and files outgrow the test-data limits above, so only
// the disk decides (copyKeepFree) and a file count far above any real app.
const promoteMaxFiles = 500000

var promoteMaxBytes int64 = 1 << 40 // in effect the free disk; a var for tests

// copyData gives a new preview a copy of production's data directory.
//
// Production's app owns that directory and could plant links or swap files
// while labd copies, so nothing in it is ever followed: every entry is opened
// relative to its already open parent with O_NOFOLLOW, and only directories
// and regular files are copied, from the descriptor that was checked.
// SQLite databases are copied with the sqlite3 CLI's online backup (which
// refuses links too), so the copy is consistent while production keeps
// writing; WAL, shared-memory and journal files are skipped. The top-level
// secrets/ folder stays with production (CONTRACT.md). On the server this
// runs as "labd -copy-data" in a unit that sees only the two directories
// (systemdRunner.CopyData).
func copyData(ctx context.Context, src, dst string) error {
	return copyTree(ctx, src, dst, false)
}

// copyAllData copies a stopped preview's whole data directory into
// production's new one (Manager.PromoteData): the same walk as copyData,
// following nothing, but secrets/ included and every file byte for byte,
// SQLite's -wal and -shm with their database. The preview is stopped, so
// the files are at rest and the copy is consistent without sqlite3, which
// would leave root-owned side files in the preview's folder. Files keep
// their owner's permission bits only (0600, 0700), as production's own
// files get them under UMask=0077; folders are 0700. On the server this
// runs as "labd -copy-data -all" (systemdRunner.CopyAllData).
func copyAllData(ctx context.Context, src, dst string) error {
	return copyTree(ctx, src, dst, true)
}

func copyTree(ctx context.Context, src, dst string, all bool) error {
	d, err := os.OpenFile(src, os.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW, 0)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer d.Close()
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return err
	}
	if err := os.Mkdir(dst, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		return err
	}
	var st syscall.Statfs_t
	if err := syscall.Statfs(dst, &st); err != nil {
		return err
	}
	c := &copier{ctx: ctx, budget: copyMaxBytes, maxFiles: copyMaxFiles, all: all}
	if all {
		c.budget, c.maxFiles = promoteMaxBytes, promoteMaxFiles
	}
	if free := int64(st.Bavail) * int64(st.Bsize); free-copyKeepFree < c.budget {
		c.budget = free - copyKeepFree
	}
	return c.dir(d, src, dst, 0)
}

type copier struct {
	ctx      context.Context
	budget   int64 // bytes still allowed
	files    int
	maxFiles int
	all      bool // promote: secrets and SQLite side files too, all as plain files
}

func (c *copier) dir(d *os.File, srcPath, dstPath string, depth int) error {
	if depth > copyMaxDepth {
		return fmt.Errorf("%s: folders nested too deep to copy", srcPath)
	}
	entries, err := d.ReadDir(-1)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if err := c.ctx.Err(); err != nil {
			return err
		}
		name := e.Name()
		if depth == 0 && name == secretsFolder && !c.all {
			continue
		}
		fd, err := syscall.Openat(int(d.Fd()), name, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
		switch {
		case errors.Is(err, syscall.ELOOP), errors.Is(err, syscall.ENOENT), errors.Is(err, syscall.ENXIO):
			continue // a link, gone meanwhile, or a socket: never copied
		case err != nil:
			return fmt.Errorf("%s: %w", filepath.Join(srcPath, name), err)
		}
		f := os.NewFile(uintptr(fd), filepath.Join(srcPath, name))
		err = c.entry(f, filepath.Join(srcPath, name), filepath.Join(dstPath, name), depth)
		f.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

func (c *copier) entry(f *os.File, src, dst string, depth int) error {
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if info.IsDir() {
		if err := os.Mkdir(dst, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
			return err
		}
		return c.dir(f, src, dst, depth+1)
	}
	if !info.Mode().IsRegular() {
		return nil // pipes and the like: never read
	}
	name := filepath.Base(src)
	if !c.all && (strings.HasSuffix(name, "-wal") || strings.HasSuffix(name, "-shm") || strings.HasSuffix(name, "-journal")) {
		return nil
	}
	c.files++
	c.budget -= info.Size()
	if c.files > c.maxFiles || c.budget < 0 {
		if c.all {
			return errors.New("the preview's data is too large to copy (or the disk is nearly full)")
		}
		return errors.New("production data is too large to copy into a preview (or the disk is nearly full)")
	}
	if c.all {
		return c.plain(f, info, dst, info.Mode().Perm()&0o700)
	}
	// Databases at the top (where apps keep them) are copied through
	// sqlite3, which opens them by name; that is safe only where the name's
	// folder can't be swapped, so deeper ones are copied as plain files.
	if strings.HasSuffix(name, ".db") && depth == 0 {
		if strings.ContainsAny(dst, "'\n") || strings.ContainsAny(src, "\n") {
			return fmt.Errorf("unsupported file name %q", name)
		}
		_, err := run(c.ctx, 2*time.Minute, "sqlite3", "-nofollow", src, ".backup '"+dst+"'")
		return err
	}
	return c.plain(f, info, dst, info.Mode().Perm())
}

// plain copies a regular file from its checked descriptor, only as far as
// its size when checked.
func (c *copier) plain(f *os.File, info os.FileInfo, dst string, perm os.FileMode) error {
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, perm)
	if err != nil {
		return err
	}
	// Only as much as was counted: a file that grows meanwhile is cut.
	if _, err := io.CopyN(out, f, info.Size()); err != nil && !errors.Is(err, io.EOF) {
		out.Close()
		return err
	}
	if c.all { // exactly the owner's bits, whatever the umask took off
		if err := out.Chmod(perm); err != nil {
			out.Close()
			return err
		}
	}
	return out.Close()
}
