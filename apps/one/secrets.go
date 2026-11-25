package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
)

// Secrets live in $DATA_DIR/secrets, one JSON file per connector, readable
// only by the app. A preview never receives this folder. Nothing in here is
// ever shown on a page, written to a log or put in an error text.

const (
	gmailSecretFile = "gmail.json"
	graphSecretFile = "graph.json"
)

// writeSecret stores v as JSON in dir/name with mode 0600: written to a
// temporary file first, then renamed over the old one.
func writeSecret(dir, name string, v any) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	path := filepath.Join(dir, name)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	if err := os.Chmod(tmp, 0o600); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// readSecret loads dir/name into v; ok is false when there is none.
func readSecret(dir, name string, v any) bool {
	b, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		return false
	}
	return json.Unmarshal(b, v) == nil
}

func removeSecret(dir, name string) error {
	err := os.Remove(filepath.Join(dir, name))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}
