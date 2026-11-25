package appkit

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNotifyWritesOutboxFile(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("DATA_DIR", dir)

	if err := Notify("Milk is low", "2 litres left", "https://shop.example/"); err != nil {
		t.Fatal(err)
	}
	if err := NotifyTopic("lab", "Second", "", ""); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(filepath.Join(dir, "notify", "outbox"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("%d files in the outbox, want 2", len(entries))
	}
	var seen []string
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".json") {
			t.Fatalf("unexpected file %s", e.Name())
		}
		raw, err := os.ReadFile(filepath.Join(dir, "notify", "outbox", e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		var m map[string]string
		if err := json.Unmarshal(raw, &m); err != nil {
			t.Fatalf("%s: %v", e.Name(), err)
		}
		seen = append(seen, m["topic"]+"/"+m["title"]+"/"+m["body"]+"/"+m["url"])
	}
	want := map[string]bool{"bots/Milk is low/2 litres left/https://shop.example/": true, "lab/Second//": true}
	for _, s := range seen {
		if !want[s] {
			t.Fatalf("unexpected message %q", s)
		}
	}
}

func TestNotifyRefusesBadInput(t *testing.T) {
	t.Setenv("DATA_DIR", t.TempDir())
	if Notify("", "x", "") == nil || Notify(strings.Repeat("t", 101), "", "") == nil || Notify("t", strings.Repeat("b", 1001), "") == nil {
		t.Fatal("bad input accepted")
	}
}

func TestNotifyWithoutDataDirOnlyLogs(t *testing.T) {
	t.Setenv("DATA_DIR", "")
	if err := Notify("local run", "", ""); err != nil {
		t.Fatal(err)
	}
}
