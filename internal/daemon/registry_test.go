package daemon

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/r1chjames/sftp-sync/internal/syncer"
)

func TestParseRegistryTreatsAbsentPausedAsActive(t *testing.T) {
	// Exactly what a daemon released before pause support would have written.
	old := []byte(`{
	  "jobs": [
	    {"id": "aaaaaaaa", "config_path": "/tmp/a.yaml", "added_at": "2024-06-15T12:00:00Z"},
	    {"id": "bbbbbbbb", "config_path": "/tmp/b.yaml", "added_at": "2024-06-15T13:00:00Z", "paused": true}
	  ]
	}`)

	reg, err := parseRegistry(old)
	if err != nil {
		t.Fatalf("parseRegistry: %v", err)
	}
	if len(reg.Jobs) != 2 {
		t.Fatalf("jobs = %d, want 2", len(reg.Jobs))
	}
	if reg.Jobs[0].Paused {
		t.Fatal("a registry without a paused field must load as active")
	}
	if !reg.Jobs[1].Paused {
		t.Fatal("paused: true was not read")
	}
	if !reg.Jobs[0].AddedAt.Equal(time.Date(2024, 6, 15, 12, 0, 0, 0, time.UTC)) {
		t.Fatalf("added_at = %v", reg.Jobs[0].AddedAt)
	}
}

func TestParseRegistryRejectsMalformedJSON(t *testing.T) {
	if _, err := parseRegistry([]byte("{")); err == nil {
		t.Fatal("malformed registry parsed without error")
	}
}

func TestRegistryRoundTrip(t *testing.T) {
	base := time.Date(2024, 6, 15, 12, 0, 0, 0, time.UTC)
	active := testJob("aaaaaaaa", base, syncer.SyncStatus{})
	paused := testJob("bbbbbbbb", base.Add(time.Minute), syncer.SyncStatus{})
	paused.syncer.(*stubJobSyncer).Pause()

	data, err := json.Marshal(registryFile{Jobs: newRegistryEntries([]*Job{paused, active})})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	reg, err := parseRegistry(data)
	if err != nil {
		t.Fatalf("parseRegistry: %v", err)
	}
	if len(reg.Jobs) != 2 {
		t.Fatalf("jobs = %d, want 2", len(reg.Jobs))
	}
	if reg.Jobs[0].ID != "aaaaaaaa" || reg.Jobs[0].Paused {
		t.Fatalf("first entry = %+v, want active aaaaaaaa", reg.Jobs[0])
	}
	if reg.Jobs[1].ID != "bbbbbbbb" || !reg.Jobs[1].Paused {
		t.Fatalf("second entry = %+v, want paused bbbbbbbb", reg.Jobs[1])
	}
}

func TestNewRegistryEntriesOmitsPausedWhenFalse(t *testing.T) {
	job := testJob("aaaaaaaa", time.Now(), syncer.SyncStatus{})

	data, err := json.Marshal(registryFile{Jobs: newRegistryEntries([]*Job{job})})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(data), "paused") {
		t.Fatalf("active job serialised a paused key: %s", data)
	}
}

func TestSaveRegistryPersistsPausedState(t *testing.T) {
	d := newTestDaemon(t, testJob("aaaaaaaa", time.Now(), syncer.SyncStatus{}))
	paused := testJob("bbbbbbbb", time.Now().Add(time.Minute), syncer.SyncStatus{})
	paused.syncer.(*stubJobSyncer).Pause()
	d.jobs["bbbbbbbb"] = paused

	if err := d.saveRegistry(); err != nil {
		t.Fatalf("saveRegistry: %v", err)
	}

	data, err := os.ReadFile(d.registryPath)
	if err != nil {
		t.Fatalf("read registry: %v", err)
	}
	reg, err := parseRegistry(data)
	if err != nil {
		t.Fatalf("parseRegistry: %v", err)
	}
	byID := map[string]registryEntry{}
	for _, entry := range reg.Jobs {
		byID[entry.ID] = entry
	}
	if byID["aaaaaaaa"].Paused {
		t.Fatal("active job was persisted as paused")
	}
	if !byID["bbbbbbbb"].Paused {
		t.Fatal("paused job was not persisted as paused")
	}
}

// unwritableRegistryPath returns a registry path whose parent is a regular
// file, so MkdirAll fails deterministically without touching permissions.
func unwritableRegistryPath(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	blocker := filepath.Join(dir, "blocker")
	if err := os.WriteFile(blocker, []byte("not a directory"), 0644); err != nil {
		t.Fatal(err)
	}
	return filepath.Join(blocker, "registry.json")
}

func TestSaveRegistryReportsWriteFailure(t *testing.T) {
	d := newTestDaemon(t, testJob("aaaaaaaa", time.Now(), syncer.SyncStatus{}))
	d.registryPath = unwritableRegistryPath(t)

	if err := d.saveRegistry(); err == nil {
		t.Fatal("saveRegistry succeeded with an unwritable path")
	}
}

func TestJobControlReportsPersistFailureAndRestoresState(t *testing.T) {
	stub := &stubJobSyncer{status: syncer.SyncStatus{Phase: syncer.PhaseIdle}}
	job := testJob("abc12345", time.Now(), syncer.SyncStatus{})
	job.syncer = stub
	d := newTestDaemon(t, job)
	d.registryPath = unwritableRegistryPath(t)

	rec := serve(t, d, http.MethodPost, "/jobs/abc12345/pause", "")
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d (body %q)", rec.Code, http.StatusInternalServerError, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "persist pause") || !strings.Contains(rec.Body.String(), "abc12345") {
		t.Fatalf("body = %q, want the action and job ID", rec.Body.String())
	}

	// The runtime state must not disagree with the registry.
	if stub.IsPaused() {
		t.Fatal("job stayed paused after the paused state failed to persist")
	}
}

func TestJobControlRestoresPausedStateWhenResumeFailsToPersist(t *testing.T) {
	stub := &stubJobSyncer{status: syncer.SyncStatus{Phase: syncer.PhasePaused, Paused: true}}
	job := testJob("abc12345", time.Now(), syncer.SyncStatus{})
	job.syncer = stub
	d := newTestDaemon(t, job)
	d.registryPath = unwritableRegistryPath(t)

	rec := serve(t, d, http.MethodPost, "/jobs/abc12345/resume", "")
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusInternalServerError)
	}
	if !stub.IsPaused() {
		t.Fatal("job stayed resumed after the resumed state failed to persist")
	}
}

func TestIdempotentPauseNeedsNoRegistryWrite(t *testing.T) {
	stub := &stubJobSyncer{status: syncer.SyncStatus{Phase: syncer.PhasePaused, Paused: true}}
	job := testJob("abc12345", time.Now(), syncer.SyncStatus{})
	job.syncer = stub
	d := newTestDaemon(t, job)
	d.registryPath = unwritableRegistryPath(t)

	rec := serve(t, d, http.MethodPost, "/jobs/abc12345/pause", "")
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d (body %q)", rec.Code, http.StatusAccepted, rec.Body.String())
	}
	if !stub.IsPaused() {
		t.Fatal("job is no longer paused")
	}
}

func TestSyncActionDoesNotWriteTheRegistry(t *testing.T) {
	stub := &stubJobSyncer{status: syncer.SyncStatus{Phase: syncer.PhaseIdle}}
	job := testJob("abc12345", time.Now(), syncer.SyncStatus{})
	job.syncer = stub
	d := newTestDaemon(t, job)
	d.registryPath = unwritableRegistryPath(t)

	rec := serve(t, d, http.MethodPost, "/jobs/abc12345/sync", "")
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d (body %q)", rec.Code, http.StatusAccepted, rec.Body.String())
	}
}
