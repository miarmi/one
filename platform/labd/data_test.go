package main

import (
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// Copying production data into a preview follows no link, copies only
// directories and regular files, never blocks on a pipe and leaves the
// secrets folder behind (security review M1 and NEW-5).
func TestCopyDataFollowsNothing(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not available")
	}
	src, outside := t.TempDir(), t.TempDir()
	dst := filepath.Join(t.TempDir(), "preview")
	os.WriteFile(filepath.Join(outside, "secret"), []byte("TOPSECRET"), 0o600)
	os.WriteFile(filepath.Join(outside, "other.db"), nil, 0o600)
	mkdb := exec.Command("python3", "-c", `import sqlite3,sys
for p in sys.argv[1:]:
    c=sqlite3.connect(p); c.execute("create table t(v text)"); c.execute("insert into t values(?)", ("TOPSECRET" if "other" in p else "prod row",)); c.commit()`,
		filepath.Join(src, "app.db"), filepath.Join(outside, "other.db"))
	if out, err := mkdb.CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	os.WriteFile(filepath.Join(src, "notes.txt"), []byte("hello"), 0o640)
	os.MkdirAll(filepath.Join(src, "sub", "deeper"), 0o700)
	os.WriteFile(filepath.Join(src, "sub", "deeper", "a.txt"), []byte("a"), 0o600)
	os.WriteFile(filepath.Join(src, "app.db-wal"), []byte("wal"), 0o600)
	os.MkdirAll(filepath.Join(src, "secrets"), 0o700)
	os.WriteFile(filepath.Join(src, "secrets", "api_key"), []byte("TOPSECRET"), 0o600)
	os.MkdirAll(filepath.Join(src, "sub", "secrets"), 0o700) // only the top-level folder is special
	os.WriteFile(filepath.Join(src, "sub", "secrets", "kept.txt"), []byte("kept"), 0o600)
	for link, target := range map[string]string{
		"leak.txt":     filepath.Join(outside, "secret"),
		"leakdir":      outside,
		"evil.db":      filepath.Join(outside, "other.db"),
		"sub/leak2":    filepath.Join(outside, "secret"),
		"dangling.txt": filepath.Join(outside, "nothing"),
	} {
		if err := os.Symlink(target, filepath.Join(src, link)); err != nil {
			t.Fatal(err)
		}
	}
	if err := syscall.Mkfifo(filepath.Join(src, "pipe"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := copyData(t.Context(), src, dst); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{"notes.txt": "hello", "sub/deeper/a.txt": "a", "sub/secrets/kept.txt": "kept"} {
		if b, err := os.ReadFile(filepath.Join(dst, name)); err != nil || string(b) != want {
			t.Errorf("%s: %q %v", name, b, err)
		}
	}
	if info, _ := os.Stat(filepath.Join(dst, "notes.txt")); info == nil || info.Mode().Perm() != 0o640 {
		t.Errorf("mode not kept: %v", info)
	}
	out, err := exec.Command("python3", "-c", `import sqlite3,sys
print(sqlite3.connect(sys.argv[1]).execute("select v from t").fetchone()[0])`, filepath.Join(dst, "app.db")).CombinedOutput()
	if err != nil || strings.TrimSpace(string(out)) != "prod row" {
		t.Errorf("database: %v %s", err, out)
	}
	for _, gone := range []string{"leak.txt", "leakdir", "evil.db", "sub/leak2", "dangling.txt", "pipe", "app.db-wal", "secrets"} {
		if _, err := os.Lstat(filepath.Join(dst, gone)); !os.IsNotExist(err) {
			t.Errorf("%s was copied", gone)
		}
	}
	filepath.WalkDir(dst, func(p string, d fs.DirEntry, err error) error {
		if err == nil && d.Type().IsRegular() {
			if b, _ := os.ReadFile(p); strings.Contains(string(b), "TOPSECRET") {
				t.Errorf("%s holds data from outside production's folder", p)
			}
		}
		return nil
	})
}

func TestCopyDataLimits(t *testing.T) {
	src := t.TempDir()
	os.WriteFile(filepath.Join(src, "big.bin"), make([]byte, 4096), 0o600)
	old := copyMaxBytes
	copyMaxBytes = 1024
	defer func() { copyMaxBytes = old }()
	err := copyData(t.Context(), src, filepath.Join(t.TempDir(), "preview"))
	if err == nil || !strings.Contains(err.Error(), "too large") {
		t.Fatalf("copied more than the limit: %v", err)
	}
	if err := copyData(t.Context(), filepath.Join(t.TempDir(), "missing"), filepath.Join(t.TempDir(), "preview")); err != nil {
		t.Fatalf("no production data is not an error: %v", err)
	}
}

// promote-data moves real data: the test-data size limit must not stop it,
// its own limit and the free disk still do.
func TestCopyAllDataIsNotHeldToThePreviewLimit(t *testing.T) {
	src := t.TempDir()
	os.WriteFile(filepath.Join(src, "big.bin"), make([]byte, 4096), 0o600)
	old := copyMaxBytes
	copyMaxBytes = 1024
	defer func() { copyMaxBytes = old }()
	dst := filepath.Join(t.TempDir(), "prod")
	if err := copyAllData(t.Context(), src, dst); err != nil {
		t.Fatalf("promote was held to the preview copy's limit: %v", err)
	}
	if b, err := os.ReadFile(filepath.Join(dst, "big.bin")); err != nil || len(b) != 4096 {
		t.Fatalf("the file was not copied whole: %v", err)
	}
	oldP := promoteMaxBytes
	promoteMaxBytes = 1024
	defer func() { promoteMaxBytes = oldP }()
	err := copyAllData(t.Context(), src, filepath.Join(t.TempDir(), "prod2"))
	if err == nil || !strings.Contains(err.Error(), "too large") {
		t.Fatalf("promote copied more than its own limit: %v", err)
	}
}
