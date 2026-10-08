package syncer

import (
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
	s.beginDownload(3)
	s.setCurrentFile("/photos/a.jpg")
	s.recordFileResult("/photos/a.jpg", nil)
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
	if st.LastError == nil || st.LastError.Error() != "previous failure" {
		t.Fatalf("scan cleared previous error: %v", st.LastError)
	}
	if !st.LastSync.Equal(lastAttempt) {
		t.Fatalf("scan changed last attempt: got %v, want %v", st.LastSync, lastAttempt)
	}

	s.setEligibleFiles(8)
	s.beginDownload(3)
	s.setCurrentFile("/photos/a.jpg")
	s.recordFileResult("/photos/a.jpg", nil)
	s.setCurrentFile("/photos/b.jpg")
	s.recordFileResult("/photos/b.jpg", errors.New("copy failed"))
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
	s.beginDownload(2)
	s.recordFileResult("/photos/a.jpg", nil)

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
