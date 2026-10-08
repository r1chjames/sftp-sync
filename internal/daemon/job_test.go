package daemon

import (
	"errors"
	"testing"
	"time"

	"github.com/r1chjames/sftp-sync/internal/syncer"
)

// stubJobSyncer records control actions and lets tests publish an arbitrary
// status, so handlers can be exercised without SFTP.
type stubJobSyncer struct {
	status  syncer.SyncStatus
	actions []string
}

func (s *stubJobSyncer) Status() syncer.SyncStatus { return s.status }
func (s *stubJobSyncer) Stop()                     { s.actions = append(s.actions, "stop") }

func (s *stubJobSyncer) SyncNow() { s.actions = append(s.actions, "sync") }

func (s *stubJobSyncer) Pause() {
	s.actions = append(s.actions, "pause")
	s.status.Phase = syncer.PhasePaused
}

func (s *stubJobSyncer) Resume() {
	s.actions = append(s.actions, "resume")
	s.status.Phase = syncer.PhaseScanning
}

func TestJobToResponseWithoutLastError(t *testing.T) {
	addedAt := time.Date(2024, 6, 15, 12, 0, 0, 0, time.UTC)
	job := &Job{
		ID:         "abc12345",
		ConfigPath: "/tmp/photos.yaml",
		AddedAt:    addedAt,
		syncer: &stubJobSyncer{status: syncer.SyncStatus{
			Phase:         syncer.PhaseIdle,
			FilesTotal:    12,
			EligibleFiles: 12,
		}},
	}

	got := job.toResponse()
	if got.ID != job.ID || got.ConfigPath != job.ConfigPath || !got.AddedAt.Equal(addedAt) {
		t.Fatalf("job identity not mapped: %+v", got)
	}
	if got.Status.Phase != "idle" || got.Status.FilesTotal != 12 || got.Status.EligibleFiles != 12 {
		t.Fatalf("status not mapped: %+v", got.Status)
	}
	if got.Status.LastError != "" {
		t.Fatalf("last error = %q, want empty", got.Status.LastError)
	}
}

func TestJobToResponseConvertsLastErrorAtBoundary(t *testing.T) {
	startedAt := time.Date(2024, 6, 15, 12, 5, 0, 0, time.UTC)
	lastSuccess := startedAt.Add(-time.Hour)
	job := &Job{
		ID: "abc12345",
		syncer: &stubJobSyncer{status: syncer.SyncStatus{
			Phase:              syncer.PhaseError,
			LastSuccessfulSync: lastSuccess,
			BatchTotal:         3,
			Completed:          1,
			Failed:             1,
			Remaining:          1,
			Pending:            1,
			CurrentFile:        "/photos/b.jpg",
			StartedAt:          startedAt,
			LastError:          errors.New("copy failed"),
		}},
	}

	got := job.toResponse().Status
	if got.LastError != "copy failed" {
		t.Fatalf("last error = %q, want %q", got.LastError, "copy failed")
	}
	if got.Phase != "error" || got.BatchTotal != 3 || got.Completed != 1 || got.Failed != 1 || got.Remaining != 1 || got.Pending != 1 {
		t.Fatalf("batch status not mapped: %+v", got)
	}
	if got.CurrentFile != "/photos/b.jpg" || !got.StartedAt.Equal(startedAt) || !got.LastSuccessfulSync.Equal(lastSuccess) {
		t.Fatalf("live status not mapped: %+v", got)
	}
}
