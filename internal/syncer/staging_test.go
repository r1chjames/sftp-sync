package syncer

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	sftpclient "github.com/r1chjames/sftp-sync/internal/sftp"
)

func TestPruneStagingRemovesOnlyStaleStagingFiles(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2024, 6, 15, 12, 0, 0, 0, time.UTC)

	write := func(name string, age time.Duration) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte("data"), 0644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
		mod := now.Add(-age)
		if err := os.Chtimes(path, mod, mod); err != nil {
			t.Fatalf("chtimes %s: %v", name, err)
		}
		return path
	}

	staleStaging := write(".sftpsync-1234", 48*time.Hour)
	freshStaging := write(".sftpsync-5678", time.Minute)
	// A normal photo is never a staging file, however old it is.
	oldPhoto := write("IMG_0001.JPG", 365*24*time.Hour)
	// And a file that merely contains the pattern is not one either.
	similarlyNamed := write("my.sftpsync-backup.jpg", 365*24*time.Hour)
	// A directory that looks like a staging file is not a file to remove.
	stagingDir := filepath.Join(dir, ".sftpsync-dir")
	if err := os.MkdirAll(stagingDir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	s := testSyncer(t, time.Hour)
	removed := s.pruneStaging(dir, now)

	if removed != 1 {
		t.Fatalf("removed = %d, want 1", removed)
	}
	if _, err := os.Stat(staleStaging); !os.IsNotExist(err) {
		t.Fatalf("stale staging file still present: %v", err)
	}
	for name, path := range map[string]string{
		"fresh staging file": freshStaging,
		"old photo":          oldPhoto,
		"similarly named":    similarlyNamed,
		"staging directory":  stagingDir,
	} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("%s was removed: %v", name, err)
		}
	}
}

// TestPruneStagingAgeBoundary pins the threshold, because an off-by-one here
// deletes a live transfer's temp file.
func TestPruneStagingAgeBoundary(t *testing.T) {
	tests := []struct {
		name    string
		age     time.Duration
		removed bool
	}{
		{name: "older than the threshold", age: stagingMaxAge + time.Minute, removed: true},
		{name: "exactly at the threshold", age: stagingMaxAge, removed: true},
		{name: "just under the threshold", age: stagingMaxAge - time.Minute, removed: false},
		{name: "brand new", age: 0, removed: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			now := time.Date(2024, 6, 15, 12, 0, 0, 0, time.UTC)
			path := filepath.Join(dir, ".sftpsync-abc")
			if err := os.WriteFile(path, []byte("partial"), 0644); err != nil {
				t.Fatalf("write: %v", err)
			}
			mod := now.Add(-tt.age)
			if err := os.Chtimes(path, mod, mod); err != nil {
				t.Fatalf("chtimes: %v", err)
			}

			s := testSyncer(t, time.Hour)
			got := s.pruneStaging(dir, now)

			wantRemoved := 0
			if tt.removed {
				wantRemoved = 1
			}
			if got != wantRemoved {
				t.Fatalf("removed = %d, want %d for age %s", got, wantRemoved, tt.age)
			}

			_, err := os.Stat(path)
			if gone := os.IsNotExist(err); gone != tt.removed {
				t.Fatalf("file removed = %v, want %v for age %s", gone, tt.removed, tt.age)
			}
		})
	}
}

// TestPruneStagingDoesNotFollowSymlinks checks that a link named like a staging
// file cannot be used to delete something else.
func TestPruneStagingDoesNotFollowSymlinks(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2024, 6, 15, 12, 0, 0, 0, time.UTC)

	target := filepath.Join(dir, "IMG_0001.JPG")
	if err := os.WriteFile(target, []byte("photo"), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}

	link := filepath.Join(dir, ".sftpsync-link")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	old := now.Add(-72 * time.Hour)
	if err := os.Chtimes(target, old, old); err != nil {
		t.Fatalf("chtimes: %v", err)
	}

	s := testSyncer(t, time.Hour)
	if removed := s.pruneStaging(dir, now); removed != 0 {
		t.Fatalf("removed = %d, want 0: a symlink is not a staging file", removed)
	}
	if _, err := os.Stat(target); err != nil {
		t.Fatalf("the symlink target was affected: %v", err)
	}
}

func TestPruneStagingHandlesAMissingDirectory(t *testing.T) {
	s := testSyncer(t, time.Hour)

	if removed := s.pruneStaging(filepath.Join(t.TempDir(), "does-not-exist"), time.Now()); removed != 0 {
		t.Fatalf("removed = %d, want 0 for a directory that is not there yet", removed)
	}
}

func TestSpaceError(t *testing.T) {
	const needed = 1 << 30 // 1 GiB

	tests := []struct {
		name      string
		available uint64
		needed    int64
		wantErr   bool
	}{
		{name: "plenty of space", available: 10 << 30, needed: needed},
		{name: "exactly the margin", available: needed + diskSpaceMargin, needed: needed},
		{name: "one byte short of the margin", available: needed + diskSpaceMargin - 1, needed: needed, wantErr: true},
		{name: "empty filesystem", available: 0, needed: needed, wantErr: true},
		{name: "nothing to download is never an error", available: 0, needed: 0},
		{name: "negative requirement is ignored", available: 0, needed: -1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := spaceError("/photos", tt.available, tt.needed)

			if tt.wantErr {
				if err == nil {
					t.Fatal("spaceError() = nil, want an error")
				}
				if !strings.Contains(err.Error(), "not enough free space") {
					t.Fatalf("error = %q, want it to report the space problem", err)
				}
				if !strings.Contains(err.Error(), "/photos") {
					t.Fatalf("error = %q, want it to name the destination", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("spaceError() = %v, want nil", err)
			}
		})
	}
}

func TestAvailableBytes(t *testing.T) {
	available, ok := availableBytes(t.TempDir())
	if !ok {
		// The check is optional by design, so an unsupported filesystem is not a
		// failure; it is only reported as unknown.
		t.Skip("free space is not reported on this platform")
	}
	if available == 0 {
		t.Fatalf("available = %d, want a plausible figure", available)
	}

	if _, ok := availableBytes(filepath.Join(t.TempDir(), "absent")); ok {
		t.Fatal("availableBytes() = ok for a path that does not exist, want unknown")
	}
}

// TestCheckSpaceIsNonFatalWhenUnknown covers the requirement that a failure to
// query space must not stop a sync.
func TestCheckSpaceIsNonFatalWhenUnknown(t *testing.T) {
	s := testSyncer(t, time.Hour)

	if err := s.checkSpace(filepath.Join(t.TempDir(), "absent"), 1<<40); err != nil {
		t.Fatalf("checkSpace() = %v, want the sync to proceed when space is unknown", err)
	}
}

func TestCheckSpaceAcceptsASmallBatch(t *testing.T) {
	s := testSyncer(t, time.Hour)

	if err := s.checkSpace(t.TempDir(), 1024); err != nil {
		t.Fatalf("checkSpace() = %v, want a small batch to fit", err)
	}
}

func TestCheckSpaceRejectsAnImpossibleBatch(t *testing.T) {
	if _, ok := availableBytes(t.TempDir()); !ok {
		t.Skip("free space is not reported on this platform")
	}

	s := testSyncer(t, time.Hour)

	// Larger than any filesystem this test will run on, plus the margin.
	if err := s.checkSpace(t.TempDir(), 1<<62); err == nil {
		t.Fatal("checkSpace() = nil, want an error for a batch nothing can hold")
	}
}

// TestPartialScanErrorMarksTheCycle keeps the mapping the cycle depends on: a
// partial walk is an error, a complete one is not.
func TestPartialScanErrorMarksTheCycle(t *testing.T) {
	if err := partialScanError(sftpWalkResult(t, 0)); err != nil {
		t.Fatalf("partialScanError(complete) = %v, want nil", err)
	}

	err := partialScanError(sftpWalkResult(t, 30))
	if err == nil {
		t.Fatal("partialScanError(partial) = nil, want the cycle marked failed")
	}
	if !strings.Contains(err.Error(), "incomplete") {
		t.Fatalf("error = %q, want it to say the scan was incomplete", err)
	}
	if len(err.Error()) > 400 {
		t.Fatalf("error is %d bytes, want it bounded", len(err.Error()))
	}
}

// sftpWalkResult builds a synthetic walk result with the given number of
// unreadable paths.
func sftpWalkResult(t *testing.T, failures int) sftpclient.WalkResult {
	t.Helper()

	result := sftpclient.WalkResult{FailureCount: failures}
	for i := 0; i < failures && i < 20; i++ {
		result.Failures = append(result.Failures, sftpclient.WalkFailure{
			Path: "/photos/locked",
			Err:  os.ErrPermission,
		})
	}
	return result
}
