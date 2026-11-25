package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type entry struct {
	name     string
	typ      byte
	body     []byte
	linkname string
}

func makeBundle(t *testing.T, entries ...entry) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, e := range entries {
		typ := e.typ
		if typ == 0 {
			typ = tar.TypeReg
		}
		h := &tar.Header{Name: e.name, Typeflag: typ, Mode: 0o755, Size: int64(len(e.body)), Linkname: e.linkname}
		if typ != tar.TypeReg {
			h.Size = 0
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if typ == tar.TypeReg {
			tw.Write(e.body)
		}
	}
	tw.Close()
	gz.Close()
	return buf.Bytes()
}

var fakeELF = append([]byte("\x7fELF"), bytes.Repeat([]byte{0}, 60)...)

func manifest(name string) []byte {
	return []byte(`{"name":"` + name + `","title":"T","health":"/healthz","public":false}`)
}

func TestExtractBundleOK(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "rel")
	m, err := extractBundle(bytes.NewReader(makeBundle(t,
		entry{name: "app", body: fakeELF}, entry{name: "app.json", body: manifest("hello")})), dir, "hello")
	if err != nil {
		t.Fatal(err)
	}
	if m.Health != "/healthz" || m.Memory != "256M" {
		t.Fatalf("defaults: %+v", m)
	}
	info, err := os.Stat(filepath.Join(dir, "app"))
	if err != nil || info.Mode().Perm() != 0o755 {
		t.Fatalf("app mode %v %v", info.Mode(), err)
	}
}

func TestExtractBundleRejects(t *testing.T) {
	app := entry{name: "app", body: fakeELF}
	man := entry{name: "app.json", body: manifest("hello")}
	cases := map[string][]byte{
		"not gzip":       []byte("plain"),
		"traversal":      makeBundle(t, entry{name: "../app", body: fakeELF}, man),
		"absolute":       makeBundle(t, entry{name: "/app", body: fakeELF}, man),
		"subdir":         makeBundle(t, entry{name: "x/app", body: fakeELF}, man),
		"dot slash":      makeBundle(t, entry{name: "./app", body: fakeELF}, man),
		"symlink":        makeBundle(t, entry{name: "app", typ: tar.TypeSymlink, linkname: "/bin/sh"}, man),
		"hardlink":       makeBundle(t, entry{name: "app", typ: tar.TypeLink, linkname: "app.json"}, man),
		"device":         makeBundle(t, entry{name: "app", typ: tar.TypeChar}, man),
		"directory":      makeBundle(t, entry{name: "app", typ: tar.TypeDir}, man),
		"extra file":     makeBundle(t, app, man, entry{name: "evil.sh", body: []byte("x")}),
		"duplicate":      makeBundle(t, app, app, man),
		"no app":         makeBundle(t, man),
		"no manifest":    makeBundle(t, app),
		"wrong name":     makeBundle(t, app, entry{name: "app.json", body: manifest("other")}),
		"bad json":       makeBundle(t, app, entry{name: "app.json", body: []byte("{")}),
		"not elf":        makeBundle(t, entry{name: "app", body: []byte("#!/bin/sh\necho hi\n")}, man),
		"bad health":     makeBundle(t, app, entry{name: "app.json", body: []byte(`{"name":"hello","health":"/x;rm -rf /"}`)}),
		"health no lead": makeBundle(t, app, entry{name: "app.json", body: []byte(`{"name":"hello","health":"healthz"}`)}),
		"memory huge":    makeBundle(t, app, entry{name: "app.json", body: []byte(`{"name":"hello","memory":"64G"}`)}),
		"memory odd":     makeBundle(t, app, entry{name: "app.json", body: []byte(`{"name":"hello","memory":"lots"}`)}),
		"big manifest":   makeBundle(t, app, entry{name: "app.json", body: []byte(`{"name":"hello","title":"` + strings.Repeat("x", 70<<10) + `"}`)}),
	}
	for name, raw := range cases {
		dir := filepath.Join(t.TempDir(), "rel")
		if _, err := extractBundle(bytes.NewReader(raw), dir, "hello"); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestManifestOpenPaths(t *testing.T) {
	// An exact path ("/send") or a prefix ("/kid/"), at most four.
	ok := Manifest{Name: "notify", OpenPaths: []string{"/send", "/hooks/"}}
	if err := ok.validate("notify"); err != nil {
		t.Fatal(err)
	}
	for _, bad := range [][]string{{"/"}, {"/_lab/auth"}, {"/_lab"}, {"/_lab/"}, {"send"}, {"/hooks/github"}, {"/a//b"}, {"/send", "/send"}, {"/x y"}, {"/Send"}} {
		m := Manifest{Name: "notify", OpenPaths: bad}
		if err := m.validate("notify"); err == nil {
			t.Errorf("open_paths %v accepted", bad)
		}
	}
	many := []string{"/p0", "/p1", "/p2", "/p3", "/p4"}
	if err := (&Manifest{Name: "notify", OpenPaths: many}).validate("notify"); err == nil {
		t.Error("too many open paths accepted")
	}
}
