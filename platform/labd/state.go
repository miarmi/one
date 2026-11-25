package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	portMin = 20000
	portMax = 29999
)

// Instance is one running copy of an app: production or a preview.
type Instance struct {
	App        string    `json:"app"`
	Env        string    `json:"env"`
	Title      string    `json:"title,omitempty"` // from app.json, for the lab's list of apps
	Port       int       `json:"port"`
	Release    string    `json:"release"`
	Previous   string    `json:"previous,omitempty"`
	Commit     string    `json:"commit,omitempty"`
	PrevCommit string    `json:"previous_commit,omitempty"`
	Status     string    `json:"status"` // running | failed
	Public     bool      `json:"public,omitempty"`
	OpenPaths  []string  `json:"open_paths,omitempty"`
	Memory     string    `json:"memory,omitempty"`
	Health     string    `json:"health,omitempty"`
	Sandbox    string    `json:"sandbox,omitempty"`     // "basic" after failing in the hardened one
	RolledBack bool      `json:"rolled_back,omitempty"` // rolled back since its last deploy
	Note       string    `json:"note,omitempty"`        // why it isn't running, when labd stopped it
	DeployedAt time.Time `json:"deployed_at"`
}

func (i *Instance) ID() string { return instanceID(i.App, i.Env) }

// Settings come from lab.json through POST /api/settings.
type Settings struct {
	Domain   string   `json:"domain"`
	PublicIP string   `json:"public_ip"`
	AllowIPs []string `json:"allow_ips,omitempty"` // empty: apps open to everyone
}

// Store holds state.json and settings.json. All access goes through its mutex;
// callers get copies, never pointers into the map.
type Store struct {
	dir       string
	mu        sync.Mutex
	instances map[string]Instance
	settings  Settings
}

// openStore reads the state. The state directory can be passed through (apps
// in the basic sandbox run their binary from it) but not listed, and the
// files in it are root's alone: they hold the allowed IP addresses and the
// gate's signing key.
func openStore(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o711); err != nil {
		return nil, err
	}
	if err := os.Chmod(dir, 0o711); err != nil {
		return nil, err
	}
	for _, name := range []string{"state.json", "settings.json", "sandbox"} {
		if err := os.Chmod(filepath.Join(dir, name), 0o600); err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
	}
	// Instance folders made by earlier versions could be listed by anyone.
	if entries, err := os.ReadDir(filepath.Join(dir, "instances")); err == nil {
		os.Chmod(filepath.Join(dir, "instances"), 0o711)
		for _, e := range entries {
			if e.IsDir() && !strings.HasPrefix(e.Name(), ".") {
				os.Chmod(filepath.Join(dir, "instances", e.Name()), 0o711)
				os.Chmod(filepath.Join(dir, "instances", e.Name(), "releases"), 0o711)
			}
		}
	}
	s := &Store{dir: dir, instances: map[string]Instance{}}
	var st struct {
		Instances map[string]Instance `json:"instances"`
	}
	if err := readJSON(filepath.Join(dir, "state.json"), &st); err != nil {
		return nil, err
	}
	for id, inst := range st.Instances {
		if id != inst.ID() || !validAppName(inst.App) || !validEnv(inst.Env) {
			return nil, fmt.Errorf("state.json: bad instance %q", id)
		}
		s.instances[id] = inst
	}
	if err := readJSON(filepath.Join(dir, "settings.json"), &s.settings); err != nil {
		return nil, err
	}
	return s, nil
}

func readJSON(path string, v any) error {
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := json.Unmarshal(raw, v); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	return nil
}

// writeFileAtomic writes via a temp file, fsync and rename, then fsyncs the dir.
func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp) // no-op after a successful rename
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Chmod(mode); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	return syncDir(dir)
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

func (s *Store) saveLocked() error {
	raw, err := json.MarshalIndent(struct {
		Instances map[string]Instance `json:"instances"`
	}{s.instances}, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(filepath.Join(s.dir, "state.json"), append(raw, '\n'), 0o600)
}

func (s *Store) Settings() Settings {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.settings
}

func (s *Store) SetSettings(set Settings) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	raw, err := json.MarshalIndent(set, "", "  ")
	if err != nil {
		return err
	}
	if err := writeFileAtomic(filepath.Join(s.dir, "settings.json"), append(raw, '\n'), 0o600); err != nil {
		return err
	}
	s.settings = set
	return nil
}

func (s *Store) Get(id string) (Instance, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	inst, ok := s.instances[id]
	return inst, ok
}

func (s *Store) Put(inst Instance) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	old, had := s.instances[inst.ID()]
	s.instances[inst.ID()] = inst
	if err := s.saveLocked(); err != nil {
		if had {
			s.instances[inst.ID()] = old
		} else {
			delete(s.instances, inst.ID())
		}
		return err
	}
	return nil
}

func (s *Store) Delete(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	old, had := s.instances[id]
	if !had {
		return nil
	}
	delete(s.instances, id)
	if err := s.saveLocked(); err != nil {
		s.instances[id] = old
		return err
	}
	return nil
}

// List returns all instances sorted by app, then prod first, then env.
func (s *Store) List() []Instance {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Instance, 0, len(s.instances))
	for _, inst := range s.instances {
		out = append(out, inst)
	}
	sort.Slice(out, func(a, b int) bool {
		if out[a].App != out[b].App {
			return out[a].App < out[b].App
		}
		if (out[a].Env == prodEnv) != (out[b].Env == prodEnv) {
			return out[a].Env == prodEnv
		}
		return out[a].Env < out[b].Env
	})
	return out
}

// FreePort returns the lowest port in range that no instance uses and that
// can be bound on 127.0.0.1 right now.
func (s *Store) FreePort() (int, error) {
	s.mu.Lock()
	used := map[int]bool{}
	for _, inst := range s.instances {
		used[inst.Port] = true
	}
	s.mu.Unlock()
	for p := portMin; p <= portMax; p++ {
		if used[p] {
			continue
		}
		l, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(p))
		if err != nil {
			continue
		}
		l.Close()
		return p, nil
	}
	return 0, errors.New("no free port left")
}
