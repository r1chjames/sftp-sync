package state

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestOldManifestWithoutDigestsStillLoads covers manifests written before
// content verification existed: they have no sha256 field at all.
func TestOldManifestWithoutDigestsStillLoads(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "manifest.json")
	old := `{"entries":{"/photos/a.jpg":{"mtime":"2024-06-15T12:00:00Z","size":100}}}`
	if err := os.WriteFile(path, []byte(old), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}

	manifest, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	entry, ok := manifest.Get("/photos/a.jpg")
	if !ok {
		t.Fatal("entry missing from the old manifest")
	}
	if entry.SHA256 != "" {
		t.Fatalf("digest = %q, want empty for a manifest that never recorded one", entry.SHA256)
	}
}

// TestEmptyDigestIsNotPersisted keeps the manifest unchanged for users who do not
// enable verification: no digest field appears at all.
func TestEmptyDigestIsNotPersisted(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "manifest.json")

	manifest, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	manifest.Set("/photos/a.jpg", Entry{MTime: time.Now(), Size: 10, LocalPath: "/local/a.jpg"})
	if err := manifest.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if strings.Contains(string(data), "sha256") {
		t.Fatalf("manifest = %s, want no digest field when none was computed", data)
	}

	// A digest that was computed is persisted.
	manifest.Set("/photos/b.jpg", Entry{MTime: time.Now(), Size: 10, SHA256: "abc123"})
	if err := manifest.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
	reloaded, err := Load(path)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	entry, ok := reloaded.Get("/photos/b.jpg")
	if !ok || entry.SHA256 != "abc123" {
		t.Fatalf("digest = %q (found %v), want it persisted", entry.SHA256, ok)
	}
}
