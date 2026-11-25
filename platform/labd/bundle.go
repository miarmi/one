package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
)

const (
	maxUpload      = 200 << 20 // compressed request body
	maxAppBinary   = 190 << 20 // uncompressed app
	maxManifest    = 64 << 10
	defaultMemory  = "256M"
	defaultHealth  = "/healthz"
	healthPathMax  = 200
	memoryMinBytes = 16 << 20
	memoryMaxBytes = 2 << 30
)

var (
	memoryRe     = regexp.MustCompile(`^([1-9][0-9]{0,5})([KMG])$`)
	healthPathRe = regexp.MustCompile(`^/[A-Za-z0-9._~/-]*$`)
)

// Manifest is app.json inside a bundle.
type Manifest struct {
	Name   string `json:"name"`
	Title  string `json:"title"`
	Health string `json:"health"`
	Public bool   `json:"public"`
	Memory string `json:"memory"`
	// OpenPaths skip the gates: a prefix with a trailing slash ("/kid/") or
	// one exact path without ("/send"); the app must check such requests
	// itself, e.g. with signed links or a token of its own.
	OpenPaths []string `json:"open_paths"`
}

func (m *Manifest) validate(app string) error {
	if m.Name != app {
		return fmt.Errorf("app.json name is %q, expected %q", m.Name, app)
	}
	if m.Health == "" {
		m.Health = defaultHealth
	}
	if len(m.Health) > healthPathMax || !healthPathRe.MatchString(m.Health) {
		return errors.New("app.json health must be a plain path like /healthz")
	}
	if !validOpenPaths(m.OpenPaths) {
		return errors.New("app.json open_paths must be at most four paths like /kid/ or /send")
	}
	if m.Memory == "" {
		m.Memory = defaultMemory
	}
	sm := memoryRe.FindStringSubmatch(m.Memory)
	if sm == nil {
		return errors.New("app.json memory must look like 256M")
	}
	n, _ := strconv.ParseInt(sm[1], 10, 64)
	n <<= map[string]uint{"K": 10, "M": 20, "G": 30}[sm[2]]
	if n < memoryMinBytes || n > memoryMaxBytes {
		return errors.New("app.json memory must be between 16M and 2G")
	}
	return nil
}

// extractBundle unpacks a tar.gz holding exactly the regular files "app" and
// "app.json" into dir (which must not exist yet) and returns the manifest.
func extractBundle(r io.Reader, dir, app string) (*Manifest, error) {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return nil, errors.New("bundle is not gzip")
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	files := map[string][]byte{}
	var binTmp string
	defer func() {
		if binTmp != "" {
			os.Remove(binTmp)
		}
	}()
	if err := os.Mkdir(dir, 0o755); err != nil {
		return nil, err
	}
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, errors.New("bundle is not a valid tar archive")
		}
		if h.Typeflag != tar.TypeReg {
			return nil, fmt.Errorf("bundle entry %q is not a regular file", h.Name)
		}
		if h.Name != "app" && h.Name != "app.json" {
			return nil, fmt.Errorf("bundle may only contain app and app.json, found %q", h.Name)
		}
		if _, dup := files[h.Name]; dup {
			return nil, fmt.Errorf("bundle has %q twice", h.Name)
		}
		switch h.Name {
		case "app.json":
			if h.Size > maxManifest {
				return nil, errors.New("app.json too large")
			}
			b, err := io.ReadAll(io.LimitReader(tr, maxManifest+1))
			if err != nil || int64(len(b)) > maxManifest {
				return nil, errors.New("app.json unreadable or too large")
			}
			files["app.json"] = b
		case "app":
			if h.Size > maxAppBinary {
				return nil, errors.New("app binary too large")
			}
			f, err := os.OpenFile(filepath.Join(dir, "app"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o755)
			if err != nil {
				return nil, err
			}
			binTmp = f.Name()
			n, err := io.Copy(f, io.LimitReader(tr, maxAppBinary+1))
			if cerr := f.Close(); err == nil {
				err = cerr
			}
			if err != nil {
				return nil, err
			}
			if n > maxAppBinary {
				return nil, errors.New("app binary too large")
			}
			files["app"] = nil
		}
	}
	if _, ok := files["app"]; !ok {
		return nil, errors.New("bundle has no app")
	}
	manifestRaw, ok := files["app.json"]
	if !ok {
		return nil, errors.New("bundle has no app.json")
	}
	var m Manifest
	dec := json.NewDecoder(bytes.NewReader(manifestRaw))
	if err := dec.Decode(&m); err != nil {
		return nil, errors.New("app.json is not valid JSON")
	}
	if err := m.validate(app); err != nil {
		return nil, err
	}
	head := make([]byte, 4)
	f, err := os.Open(filepath.Join(dir, "app"))
	if err != nil {
		return nil, err
	}
	_, err = io.ReadFull(f, head)
	f.Close()
	if err != nil || !bytes.Equal(head, []byte("\x7fELF")) {
		return nil, errors.New("app is not a Linux executable")
	}
	if err := os.WriteFile(filepath.Join(dir, "app.json"), manifestRaw, 0o644); err != nil {
		return nil, err
	}
	if err := os.Chmod(filepath.Join(dir, "app"), 0o755); err != nil {
		return nil, err
	}
	binTmp = "" // keep it
	return &m, nil
}
