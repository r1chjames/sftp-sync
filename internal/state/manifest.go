package state

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Entry records the last-seen metadata for a remote file.
type Entry struct {
	MTime time.Time `json:"mtime"`
	Size  int64     `json:"size"`
}

// Manifest persists the set of remote files that have been synced locally.
type Manifest struct {
	path    string
	Entries map[string]Entry `json:"entries"`
}

// Load reads the manifest from disk. Returns an empty manifest if the file
// does not yet exist.
func Load(path string) (*Manifest, error) {
	m := &Manifest{
		path:    path,
		Entries: make(map[string]Entry),
	}

	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return m, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading manifest: %w", err)
	}

	if err := json.Unmarshal(data, m); err != nil {
		return nil, fmt.Errorf("parsing manifest: %w", err)
	}
	m.path = path
	return m, nil
}

// Save writes the manifest to disk atomically via a temp-file rename.
func (m *Manifest) Save() error {
	if err := os.MkdirAll(filepath.Dir(m.path), 0755); err != nil {
		return fmt.Errorf("mkdir: %w", err)
	}

	data, err := json.Marshal(m)
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}

	tmp := m.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0644); err != nil {
		return fmt.Errorf("write: %w", err)
	}
	return os.Rename(tmp, m.path)
}

func (m *Manifest) Get(path string) (Entry, bool) {
	e, ok := m.Entries[path]
	return e, ok
}

func (m *Manifest) Set(path string, e Entry) {
	m.Entries[path] = e
}
