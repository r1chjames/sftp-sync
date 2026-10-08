package syncer

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/r1chjames/sftp-sync/internal/config"
	sftpclient "github.com/r1chjames/sftp-sync/internal/sftp"
	"github.com/r1chjames/sftp-sync/internal/state"
	"github.com/r1chjames/sftp-sync/internal/verify"
)

// adoptTestSyncer builds a syncer whose local root and remote root are separate
// temp directories, with the chosen verification mode.
func adoptTestSyncer(t *testing.T, verifyMode string) (*Syncer, string) {
	t.Helper()

	root := t.TempDir()
	s := New(&config.Config{
		LocalPath: filepath.Join(root, "local"),
		SFTP:      config.SFTPConfig{RemotePath: "/photos"},
		StatePath: filepath.Join(root, "manifest.json"),
		Sync:      config.SyncConfig{Workers: 1, Interval: time.Hour, Verify: verifyMode},
	})
	return s, s.cfg.LocalPath
}

// writeLocal creates a local file with the given contents and returns the size.
func writeLocal(t *testing.T, path, content string) int64 {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	return int64(len(content))
}

// TestAdoptableRequiresMatchingSize is the core of the issue: a local file may
// only be taken as the synced copy when its size matches the remote file, so a
// wrong, empty, or truncated file is not recorded as a synced photo.
func TestAdoptableRequiresMatchingSize(t *testing.T) {
	tests := []struct {
		name      string
		content   string
		remoteSz  int64
		wantAdopt bool
	}{
		{name: "matching size", content: "0123456789", remoteSz: 10, wantAdopt: true},
		{name: "local file is truncated", content: "01234", remoteSz: 10, wantAdopt: false},
		{name: "local file is longer", content: "0123456789abc", remoteSz: 10, wantAdopt: false},
		{name: "zero-byte local file", content: "", remoteSz: 10, wantAdopt: false},
		{name: "zero-byte on both sides", content: "", remoteSz: 0, wantAdopt: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, root := adoptTestSyncer(t, config.VerifySize)
			local := filepath.Join(root, "photo.jpg")
			writeLocal(t, local, tt.content)

			remote := sftpclient.RemoteFile{Path: "/photos/photo.jpg", Size: tt.remoteSz, MTime: time.Now()}
			adopted, digest, err := s.adoptable(context.Background(), remote, local)
			if err != nil {
				t.Fatalf("adoptable() error = %v", err)
			}
			if adopted != tt.wantAdopt {
				t.Fatalf("adoptable() = %v, want %v", adopted, tt.wantAdopt)
			}
			if digest != "" {
				t.Fatalf("digest = %q, want empty in size mode", digest)
			}
		})
	}
}

func TestAdoptableWithoutALocalFile(t *testing.T) {
	s, root := adoptTestSyncer(t, config.VerifySize)

	remote := sftpclient.RemoteFile{Path: "/photos/photo.jpg", Size: 10, MTime: time.Now()}
	adopted, _, err := s.adoptable(context.Background(), remote, filepath.Join(root, "absent.jpg"))

	if err != nil {
		t.Fatalf("adoptable() error = %v, want no error for a file that is simply absent", err)
	}
	if adopted {
		t.Fatal("adoptable() = true, want false when there is no local file")
	}
}

// TestAdoptableRejectsADirectory checks that a directory in the way is left to
// the collision policy rather than being adopted and then written into.
func TestAdoptableRejectsADirectory(t *testing.T) {
	s, root := adoptTestSyncer(t, config.VerifySize)
	local := filepath.Join(root, "photo.jpg")
	if err := os.MkdirAll(local, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	remote := sftpclient.RemoteFile{Path: "/photos/photo.jpg", Size: 10, MTime: time.Now()}
	adopted, _, err := s.adoptable(context.Background(), remote, local)

	if err != nil {
		t.Fatalf("adoptable() error = %v", err)
	}
	if adopted {
		t.Fatal("adoptable() = true, want false for a directory")
	}
}

// TestSizeModeDoesNotReadTheRemote is the requirement that hashing is not a
// prerequisite for the default mode: adoption must decide from one stat.
func TestSizeModeDoesNotReadTheRemote(t *testing.T) {
	s, root := adoptTestSyncer(t, config.VerifySize)
	local := filepath.Join(root, "photo.jpg")
	writeLocal(t, local, "0123456789")

	read := false
	s.hashRemote = func(context.Context, string) (string, error) {
		read = true
		return "", errors.New("must not be called")
	}

	remote := sftpclient.RemoteFile{Path: "/photos/photo.jpg", Size: 10, MTime: time.Now()}
	adopted, _, err := s.adoptable(context.Background(), remote, local)
	if err != nil {
		t.Fatalf("adoptable() error = %v", err)
	}
	if !adopted {
		t.Fatal("adoptable() = false, want a match on size alone")
	}
	if read {
		t.Fatal("size mode read the remote file")
	}
}

func TestAdoptableHashModeComparesContents(t *testing.T) {
	t.Run("matching contents are adopted with the digest", func(t *testing.T) {
		s, root := adoptTestSyncer(t, config.VerifySHA256)
		local := filepath.Join(root, "photo.jpg")
		writeLocal(t, local, "abc")

		remoteDigest := digestOf(t, "abc")
		s.hashRemote = func(context.Context, string) (string, error) { return remoteDigest, nil }

		remote := sftpclient.RemoteFile{Path: "/photos/photo.jpg", Size: 3, MTime: time.Now()}
		adopted, digest, err := s.adoptable(context.Background(), remote, local)

		if err != nil {
			t.Fatalf("adoptable() error = %v", err)
		}
		if !adopted {
			t.Fatal("adoptable() = false, want true for identical contents")
		}
		if digest != remoteDigest {
			t.Fatalf("digest = %q, want %q", digest, remoteDigest)
		}
	})

	t.Run("same size, different contents are not adopted", func(t *testing.T) {
		s, root := adoptTestSyncer(t, config.VerifySHA256)
		local := filepath.Join(root, "photo.jpg")
		writeLocal(t, local, "abc")

		// A different file of the same length is exactly what a size check cannot
		// catch, which is the reason hash mode exists.
		s.hashRemote = func(context.Context, string) (string, error) { return digestOf(t, "abd"), nil }

		remote := sftpclient.RemoteFile{Path: "/photos/photo.jpg", Size: 3, MTime: time.Now()}
		adopted, digest, err := s.adoptable(context.Background(), remote, local)

		if err != nil {
			t.Fatalf("adoptable() error = %v", err)
		}
		if adopted {
			t.Fatal("adoptable() = true, want false when the contents differ")
		}
		if digest != "" {
			t.Fatalf("digest = %q, want empty when nothing was adopted", digest)
		}
	})

	t.Run("an unreadable remote file is never adopted", func(t *testing.T) {
		s, root := adoptTestSyncer(t, config.VerifySHA256)
		local := filepath.Join(root, "photo.jpg")
		writeLocal(t, local, "abc")

		s.hashRemote = func(context.Context, string) (string, error) {
			return "", errors.New("permission denied")
		}

		remote := sftpclient.RemoteFile{Path: "/photos/photo.jpg", Size: 3, MTime: time.Now()}
		adopted, _, err := s.adoptable(context.Background(), remote, local)

		if err == nil {
			t.Fatal("adoptable() error = nil, want the remote read failure reported")
		}
		if adopted {
			t.Fatal("adoptable() = true, want false when verification could not run")
		}
	})

	t.Run("cancellation is not an adoption", func(t *testing.T) {
		s, root := adoptTestSyncer(t, config.VerifySHA256)
		local := filepath.Join(root, "photo.jpg")
		writeLocal(t, local, "abc")

		s.hashRemote = func(ctx context.Context, _ string) (string, error) { return "", ctx.Err() }

		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		remote := sftpclient.RemoteFile{Path: "/photos/photo.jpg", Size: 3, MTime: time.Now()}
		adopted, _, err := s.adoptable(ctx, remote, local)

		if !errors.Is(err, context.Canceled) {
			t.Fatalf("error = %v, want context.Canceled", err)
		}
		if adopted {
			t.Fatal("adoptable() = true, want false when the check was cancelled")
		}
	})
}

func digestOf(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "digest")
	writeLocal(t, path, content)

	// Use the same code path production uses, so the test cannot drift from it.
	digest, err := verify.SHA256File(context.Background(), path)
	if err != nil {
		t.Fatalf("hashing %q: %v", content, err)
	}
	return digest
}

func TestSelectForDownloadRequiresVerificationBeforeAdopting(t *testing.T) {
	s, root := adoptTestSyncer(t, config.VerifySize)
	s.manifest = &state.Manifest{Entries: map[string]state.Entry{}}

	// An empty local file where a 10-byte remote file belongs must be
	// downloaded, not adopted.
	writeLocal(t, filepath.Join(root, "empty.jpg"), "")
	// A complete copy may be adopted.
	writeLocal(t, filepath.Join(root, "complete.jpg"), "0123456789")

	mtime := time.Now()
	files := []sftpclient.RemoteFile{
		{Path: "/photos/empty.jpg", Size: 10, MTime: mtime},
		{Path: "/photos/complete.jpg", Size: 10, MTime: mtime},
		{Path: "/photos/absent.jpg", Size: 10, MTime: mtime},
	}

	eligible, downloads := s.selectForDownload(context.Background(), files)

	if eligible != 3 {
		t.Fatalf("eligible = %d, want 3", eligible)
	}
	paths := make(map[string]bool)
	for _, f := range downloads {
		paths[f.Path] = true
	}
	if len(downloads) != 2 || !paths["/photos/empty.jpg"] || !paths["/photos/absent.jpg"] {
		t.Fatalf("downloads = %+v, want the empty and the absent file", downloads)
	}
	if _, ok := s.manifest.Get("/photos/complete.jpg"); !ok {
		t.Fatal("the complete local file was not adopted")
	}
	if _, ok := s.manifest.Get("/photos/empty.jpg"); ok {
		t.Fatal("the empty local file was recorded as synced")
	}
}

func TestSelectForDownloadRecordsTheVerifiedDigest(t *testing.T) {
	s, root := adoptTestSyncer(t, config.VerifySHA256)
	s.manifest = &state.Manifest{Entries: map[string]state.Entry{}}

	writeLocal(t, filepath.Join(root, "photo.jpg"), "abc")
	digest := digestOf(t, "abc")
	s.hashRemote = func(context.Context, string) (string, error) { return digest, nil }

	files := []sftpclient.RemoteFile{
		{Path: "/photos/photo.jpg", Size: 3, MTime: time.Now()},
	}

	_, downloads := s.selectForDownload(context.Background(), files)

	if len(downloads) != 0 {
		t.Fatalf("downloads = %+v, want the verified file adopted", downloads)
	}
	entry, ok := s.manifest.Get("/photos/photo.jpg")
	if !ok {
		t.Fatal("adopted file missing from the manifest")
	}
	if entry.SHA256 != digest {
		t.Fatalf("recorded digest = %q, want %q", entry.SHA256, digest)
	}
}

func TestSelectForDownloadStopsOnCancellation(t *testing.T) {
	s, root := adoptTestSyncer(t, config.VerifySize)
	s.manifest = &state.Manifest{Entries: map[string]state.Entry{}}

	writeLocal(t, filepath.Join(root, "a.jpg"), "0123456789")
	writeLocal(t, filepath.Join(root, "b.jpg"), "0123456789")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, downloads := s.selectForDownload(ctx, []sftpclient.RemoteFile{
		{Path: "/photos/a.jpg", Size: 10, MTime: time.Now()},
		{Path: "/photos/b.jpg", Size: 10, MTime: time.Now()},
	})

	if len(downloads) != 0 {
		t.Fatalf("downloads = %+v, want none after cancellation", downloads)
	}
}

func TestVerifyDownload(t *testing.T) {
	t.Run("matching digest", func(t *testing.T) {
		s, root := adoptTestSyncer(t, config.VerifySHA256)
		staged := filepath.Join(root, "staged")
		writeLocal(t, staged, "abc")

		digest := digestOf(t, "abc")
		s.hashRemote = func(context.Context, string) (string, error) { return digest, nil }

		got, err := s.verifyDownload(context.Background(), "/photos/photo.jpg", staged)
		if err != nil {
			t.Fatalf("verifyDownload() error = %v", err)
		}
		if got != digest {
			t.Fatalf("digest = %q, want %q", got, digest)
		}
	})

	t.Run("mismatch is reported as a retryable checksum failure", func(t *testing.T) {
		s, root := adoptTestSyncer(t, config.VerifySHA256)
		staged := filepath.Join(root, "staged")
		writeLocal(t, staged, "abc")

		s.hashRemote = func(context.Context, string) (string, error) { return digestOf(t, "abd"), nil }

		_, err := s.verifyDownload(context.Background(), "/photos/photo.jpg", staged)
		if err == nil {
			t.Fatal("verifyDownload() = nil, want a mismatch")
		}
		if !errors.Is(err, errChecksumMismatch) {
			t.Fatalf("error = %v, want it to be a checksum mismatch", err)
		}
		// The message must show both digests, or the mismatch cannot be
		// diagnosed from the log.
		if !strings.Contains(err.Error(), "hashed to") {
			t.Fatalf("error = %q, want both digests", err)
		}
		if !transient(err) {
			t.Fatal("a checksum mismatch must be retryable: corruption is what a retry fixes")
		}
	})

	t.Run("an unreadable remote file keeps the download unverified", func(t *testing.T) {
		s, root := adoptTestSyncer(t, config.VerifySHA256)
		staged := filepath.Join(root, "staged")
		writeLocal(t, staged, "abc")

		s.hashRemote = func(context.Context, string) (string, error) {
			return "", errors.New("sftp: permission denied")
		}

		got, err := s.verifyDownload(context.Background(), "/photos/photo.jpg", staged)
		if err != nil {
			t.Fatalf("verifyDownload() error = %v, want the file kept when the server cannot be re-read", err)
		}
		if got != "" {
			t.Fatalf("digest = %q, want none: nothing was verified", got)
		}
	})
}

func TestEntryForOmitsTheDigestWhenThereIsNone(t *testing.T) {
	remote := sftpclient.RemoteFile{Path: "/photos/a.jpg", Size: 5, MTime: time.Now()}

	entry := entryFor(remote, "/local/a.jpg", "")

	if entry.SHA256 != "" {
		t.Fatalf("digest = %q, want empty", entry.SHA256)
	}
	if entry.LocalPath != "/local/a.jpg" || entry.Size != 5 {
		t.Fatalf("entry = %+v, want the destination and size recorded", entry)
	}
}
