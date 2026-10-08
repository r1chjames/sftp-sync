package syncer

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/r1chjames/sftp-sync/internal/config"
	sftpclient "github.com/r1chjames/sftp-sync/internal/sftp"
	"github.com/r1chjames/sftp-sync/internal/state"
)

func TestLocalPath(t *testing.T) {
	s := &Syncer{
		cfg: &config.Config{
			LocalPath: "/output",
			SFTP:      config.SFTPConfig{RemotePath: "/photos"},
		},
	}

	tests := []struct {
		name       string
		remotePath string
		want       string
	}{
		{"strips prefix", "/photos/vacation/IMG_001.jpg", "/output/vacation/IMG_001.jpg"},
		{"top-level file", "/photos/IMG_001.jpg", "/output/IMG_001.jpg"},
		{"deep nesting", "/photos/2024/06/15/IMG_001.jpg", "/output/2024/06/15/IMG_001.jpg"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := s.localPath(tt.remotePath)
			if got != tt.want {
				t.Fatalf("localPath(%q) = %q, want %q", tt.remotePath, got, tt.want)
			}
		})
	}
}

func TestDatePath(t *testing.T) {
	date := time.Date(2024, 6, 15, 14, 30, 0, 0, time.UTC)
	remotePath := "/photos/vacation/IMG_001.jpg"

	tests := []struct {
		name            string
		folderStructure string
		want            string
	}{
		{"none falls back to remote structure", "none", "/output/vacation/IMG_001.jpg"},
		{"year only", "year", "/output/2024/IMG_001.jpg"},
		{"year/month", "year_month", "/output/2024/06/IMG_001.jpg"},
		{"year/month/day", "year_month_day", "/output/2024/06/15/IMG_001.jpg"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := &Syncer{
				cfg: &config.Config{
					LocalPath: "/output",
					SFTP:      config.SFTPConfig{RemotePath: "/photos"},
					Sync:      config.SyncConfig{FolderStructure: tt.folderStructure},
				},
			}

			got := s.datePath(remotePath, date)
			if got != tt.want {
				t.Fatalf("datePath(%q) = %q, want %q", remotePath, got, tt.want)
			}
		})
	}
}

func TestStatusTransitions(t *testing.T) {
	s := New(&config.Config{})
	if got := s.Status().Phase; got != PhaseIdle {
		t.Fatalf("initial phase = %q, want %q", got, PhaseIdle)
	}

	s.markFailure(errors.New("previous failure"))
	lastAttempt := s.Status().LastSync
	s.beginDownload(3, 0)
	s.setCurrentFile("/photos/a.jpg")
	s.recordFileResult("/photos/a.jpg", nil, 0)
	s.beginScan()

	st := s.Status()
	if st.Phase != PhaseScanning {
		t.Fatalf("scan phase = %q, want %q", st.Phase, PhaseScanning)
	}
	if st.BatchTotal != 0 || st.Completed != 0 || st.Failed != 0 || st.Remaining != 0 || st.Pending != 0 {
		t.Fatalf("scan did not reset batch counters: %+v", st)
	}
	if st.CurrentFile != "" || st.StartedAt.IsZero() {
		t.Fatalf("scan current/start state = %q/%v", st.CurrentFile, st.StartedAt)
	}
	if st.BytesTotal != 0 || st.BytesCompleted != 0 || st.CurrentFileBytesTotal != 0 || st.CurrentFileBytesCompleted != 0 {
		t.Fatalf("scan did not reset byte counters: %+v", st)
	}
	if st.LastError == nil || st.LastError.Error() != "previous failure" {
		t.Fatalf("scan cleared previous error: %v", st.LastError)
	}
	if !st.LastSync.Equal(lastAttempt) {
		t.Fatalf("scan changed last attempt: got %v, want %v", st.LastSync, lastAttempt)
	}

	s.setEligibleFiles(8)
	s.beginDownload(3, 0)
	s.setCurrentFile("/photos/a.jpg")
	s.recordFileResult("/photos/a.jpg", nil, 0)
	s.setCurrentFile("/photos/b.jpg")
	s.recordFileResult("/photos/b.jpg", errors.New("copy failed"), 0)
	st = s.Status()
	if st.Phase != PhaseDownloading || st.FilesTotal != 8 || st.EligibleFiles != 8 {
		t.Fatalf("download phase/totals incorrect: %+v", st)
	}
	if st.BatchTotal != 3 || st.Completed != 1 || st.Failed != 1 || st.Remaining != 1 || st.Pending != 1 {
		t.Fatalf("download counters incorrect: %+v", st)
	}
}

func TestMarkFailurePreservesLastSuccessAndCounters(t *testing.T) {
	s := New(&config.Config{})
	s.markSuccess()
	lastSuccess := s.Status().LastSuccessfulSync
	s.beginDownload(2, 0)
	s.recordFileResult("/photos/a.jpg", nil, 0)

	s.markFailure(errors.New("batch failed"))
	st := s.Status()
	if st.Phase != PhaseError || st.LastError == nil || st.LastError.Error() != "batch failed" {
		t.Fatalf("failure state incorrect: %+v", st)
	}
	if !st.LastSuccessfulSync.Equal(lastSuccess) {
		t.Fatalf("last success changed: got %v, want %v", st.LastSuccessfulSync, lastSuccess)
	}
	if st.LastSync.IsZero() || st.Completed != 1 || st.Remaining != 1 || st.Pending != 1 {
		t.Fatalf("failure did not preserve counters: %+v", st)
	}

	s.markSuccess()
	st = s.Status()
	if st.Phase != PhaseIdle || st.LastError != nil || st.LastSuccessfulSync.IsZero() {
		t.Fatalf("success state incorrect: %+v", st)
	}
}

func TestByteProgressAccounting(t *testing.T) {
	s := New(&config.Config{})
	s.beginDownload(2, 300)

	st := s.Status()
	if st.BytesTotal != 300 || st.BytesCompleted != 0 {
		t.Fatalf("batch bytes = %d/%d, want 0/300", st.BytesCompleted, st.BytesTotal)
	}

	s.recordBytes("/photos/a.jpg", 120, 120, 100)
	st = s.Status()
	if st.BytesCompleted != 120 {
		t.Fatalf("completed bytes = %d, want 120", st.BytesCompleted)
	}
	if st.CurrentFile != "/photos/a.jpg" || st.CurrentFileBytesCompleted != 120 || st.CurrentFileBytesTotal != 100 {
		t.Fatalf("current file bytes not tracked: %+v", st)
	}

	// A remote file that grew after the walk must not push published progress
	// above the batch total.
	s.recordBytes("/photos/b.jpg", 260, 260, 200)
	st = s.Status()
	if st.BytesCompleted != st.BytesTotal {
		t.Fatalf("completed bytes = %d, want clamp to %d", st.BytesCompleted, st.BytesTotal)
	}
}

func TestFileProgressThrottlingAndFlush(t *testing.T) {
	clock := time.Date(2024, 6, 15, 12, 0, 0, 0, time.UTC)
	type emit struct{ delta, copied, total int64 }
	var emits []emit

	p := &fileProgress{
		total:    4096,
		interval: byteProgressInterval,
		now:      func() time.Time { return clock },
		record:   func(delta, copied, total int64) { emits = append(emits, emit{delta, copied, total}) },
	}

	// First update emits immediately so a transfer that finishes inside one
	// interval still reports progress.
	if err := p.update(1024); err != nil {
		t.Fatalf("update: %v", err)
	}
	if len(emits) != 1 || emits[0].delta != 1024 || emits[0].total != 4096 {
		t.Fatalf("first emit = %+v, want one delta of 1024", emits)
	}

	// Updates inside the interval are withheld.
	clock = clock.Add(50 * time.Millisecond)
	if err := p.update(2048); err != nil {
		t.Fatalf("update: %v", err)
	}
	if len(emits) != 1 {
		t.Fatalf("throttled update emitted: %+v", emits)
	}

	// Once the interval passes the withheld delta is reported in full.
	clock = clock.Add(byteProgressInterval)
	if err := p.update(3072); err != nil {
		t.Fatalf("update: %v", err)
	}
	if len(emits) != 2 || emits[1].delta != 2048 || emits[1].copied != 3072 {
		t.Fatalf("second emit = %+v, want delta 2048 at 3072", emits)
	}

	// The final flush reports the remainder, so emitted bytes always equal
	// copied bytes and a failure can subtract the exact contribution.
	if err := p.update(4096); err != nil {
		t.Fatalf("update: %v", err)
	}
	p.flush()
	if p.reported != p.copied {
		t.Fatalf("reported %d != copied %d after flush", p.reported, p.copied)
	}
	var totalEmitted int64
	for _, e := range emits {
		totalEmitted += e.delta
	}
	if totalEmitted != p.copied {
		t.Fatalf("emitted %d bytes, copied %d", totalEmitted, p.copied)
	}

	// A second flush is a no-op.
	before := len(emits)
	p.flush()
	if len(emits) != before {
		t.Fatalf("repeated flush emitted again: %+v", emits[before:])
	}
}

func TestFileProgressFailureSubtractsExactlyTheContribution(t *testing.T) {
	s := New(&config.Config{})
	s.beginDownload(1, 2048)

	clock := time.Date(2024, 6, 15, 12, 0, 0, 0, time.UTC)
	p := &fileProgress{
		total:    2048,
		interval: byteProgressInterval,
		now:      func() time.Time { return clock },
		record:   func(delta, copied, total int64) { s.recordBytes("/photos/a.raw", delta, copied, total) },
	}

	// Copy succeeds but the bytes are withheld by throttling, then placement
	// fails. Subtracting the full file size must not wipe unrelated progress.
	if err := p.update(2048); err != nil {
		t.Fatalf("update: %v", err)
	}
	p.flush()
	if got := s.Status().BytesCompleted; got != 2048 {
		t.Fatalf("completed bytes = %d, want 2048", got)
	}

	s.recordFileResult("/photos/a.raw", errors.New("rename: permission denied"), p.copied)
	if got := s.Status().BytesCompleted; got != 0 {
		t.Fatalf("completed bytes after failure = %d, want 0", got)
	}
}

func TestRecordFileResultRemovesFailedBytes(t *testing.T) {
	s := New(&config.Config{})
	s.beginDownload(2, 300)
	s.recordBytes("/photos/a.jpg", 100, 100, 100)
	s.recordFileResult("/photos/a.jpg", nil, 100)
	s.recordBytes("/photos/b.jpg", 200, 200, 200)

	st := s.Status()
	if st.BytesCompleted != 300 {
		t.Fatalf("completed bytes = %d, want 300", st.BytesCompleted)
	}

	// b.jpg copied fully but failed to be placed: its bytes were not committed.
	s.recordFileResult("/photos/b.jpg", errors.New("rename: permission denied"), 200)
	st = s.Status()
	if st.BytesCompleted != 100 {
		t.Fatalf("completed bytes after failure = %d, want 100", st.BytesCompleted)
	}
	if st.Completed != 1 || st.Failed != 1 || st.Remaining != 0 || st.Pending != 0 {
		t.Fatalf("file counters incorrect after failure: %+v", st)
	}

	// A failure correction must never drive the total negative.
	s.recordFileResult("/photos/c.jpg", errors.New("copy failed"), 999)
	if got := s.Status().BytesCompleted; got != 0 {
		t.Fatalf("completed bytes = %d, want 0", got)
	}
}

func TestRecordCycle(t *testing.T) {
	live := context.Background()
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()

	s := New(&config.Config{})
	s.recordCycle(cancelled, errors.New("connect: context canceled"))
	st := s.Status()
	if st.Phase != PhaseIdle || st.LastError != nil || !st.LastSuccessfulSync.IsZero() {
		t.Fatalf("cancelled cycle recorded a failure: %+v", st)
	}

	s.recordCycle(live, errors.New("walk /photos: boom"))
	st = s.Status()
	if st.Phase != PhaseError || st.LastError == nil || st.LastError.Error() != "walk /photos: boom" {
		t.Fatalf("failed cycle not recorded: %+v", st)
	}

	s.recordCycle(live, nil)
	st = s.Status()
	if st.Phase != PhaseIdle || st.LastError != nil || st.LastSuccessfulSync.IsZero() {
		t.Fatalf("successful cycle not recorded: %+v", st)
	}
}

func TestIncompleteBatchError(t *testing.T) {
	if err := incompleteBatchError(3, 3, context.Canceled); err != nil {
		t.Fatalf("fully attempted batch error = %v, want nil", err)
	}
	if err := incompleteBatchError(1, 3, nil); !errors.Is(err, errBatchPaused) {
		t.Fatalf("paused batch error = %v, want errBatchPaused", err)
	}

	err := incompleteBatchError(1, 3, context.Canceled)
	if err == nil {
		t.Fatal("interrupted batch returned nil")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("interrupted batch error %v does not wrap context.Canceled", err)
	}
	want := "batch interrupted: 2 of 3 file(s) not attempted: context canceled"
	if err.Error() != want {
		t.Fatalf("interrupted batch error = %q, want %q", err, want)
	}
}

func TestDownloadBatchError(t *testing.T) {
	if err := downloadBatchError(0, 3, "", nil); err != nil {
		t.Fatalf("successful batch error = %v, want nil", err)
	}

	firstErr := errors.New("copy failed")
	err := downloadBatchError(2, 3, "/photos/a.jpg", firstErr)
	if err == nil {
		t.Fatal("failed batch returned nil")
	}
	if !errors.Is(err, firstErr) {
		t.Fatalf("batch error %v does not wrap first failure", err)
	}
	want := "2 of 3 file(s) failed; first failure /photos/a.jpg: copy failed"
	if err.Error() != want {
		t.Fatalf("batch error = %q, want %q", err, want)
	}
}

func TestSelectForDownloadCountsFilteredAndAdoptedFiles(t *testing.T) {
	root := t.TempDir()
	mtime := time.Date(2024, 6, 15, 12, 0, 0, 0, time.UTC)
	adoptPath := filepath.Join(root, "existing.jpg")
	if err := os.WriteFile(adoptPath, []byte("existing"), 0644); err != nil {
		t.Fatal(err)
	}

	s := New(&config.Config{
		LocalPath: root,
		SFTP:      config.SFTPConfig{RemotePath: "/photos"},
		Sync:      config.SyncConfig{Extensions: []string{".jpg"}},
	})
	s.manifest = &state.Manifest{Entries: map[string]state.Entry{
		"/photos/synced.jpg": {MTime: mtime, Size: 10},
	}}
	files := []sftpclient.RemoteFile{
		{Path: "/photos/synced.jpg", MTime: mtime, Size: 10},
		{Path: "/photos/existing.jpg", MTime: mtime, Size: 8},
		{Path: "/photos/new.jpg", MTime: mtime, Size: 20},
		{Path: "/photos/ignored.raw", MTime: mtime, Size: 30},
	}

	eligible, downloads := s.selectForDownload(files)
	if eligible != 3 {
		t.Fatalf("eligible = %d, want 3", eligible)
	}
	if len(downloads) != 1 || downloads[0].Path != "/photos/new.jpg" {
		t.Fatalf("downloads = %+v, want only new.jpg", downloads)
	}
	if _, ok := s.manifest.Get("/photos/existing.jpg"); !ok {
		t.Fatal("existing eligible file was not adopted")
	}
}

func TestMatchesFilter(t *testing.T) {
	tests := []struct {
		name       string
		extensions []string
		path       string
		want       bool
	}{
		{"no filter accepts all", nil, "/photos/anything.raw", true},
		{"empty filter accepts all", []string{}, "/photos/anything.raw", true},
		{"matching extension", []string{".jpg", ".png"}, "/photos/photo.jpg", true},
		{"case insensitive match", []string{".jpg"}, "/photos/PHOTO.JPG", true},
		{"non-matching extension", []string{".jpg", ".png"}, "/photos/photo.raw", false},
		{"no extension matches nothing", []string{".jpg"}, "/photos/photo", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := &Syncer{
				cfg: &config.Config{
					Sync: config.SyncConfig{Extensions: tt.extensions},
				},
			}

			got := s.matchesFilter(tt.path)
			if got != tt.want {
				t.Fatalf("matchesFilter(%q) = %v, want %v", tt.path, got, tt.want)
			}
		})
	}
}
