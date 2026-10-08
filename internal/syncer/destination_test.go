package syncer

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/r1chjames/sftp-sync/internal/config"
	"github.com/r1chjames/sftp-sync/internal/state"
)

// writeFile creates a file with content, failing the test if it cannot.
func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func manifestEntry(localPath string) state.Entry {
	return state.Entry{
		MTime:     time.Date(2024, 6, 15, 12, 0, 0, 0, time.UTC),
		Size:      100,
		LocalPath: localPath,
	}
}

func TestResolveWithoutCollisionUsesTheDesiredPath(t *testing.T) {
	dir := t.TempDir()
	desired := filepath.Join(dir, "IMG_0001.JPG")

	for _, policy := range []string{config.CollisionError, config.CollisionSkip, config.CollisionRename} {
		t.Run(policy, func(t *testing.T) {
			d := newBatchDestinations(nil)

			got, skipped, err := d.resolve(policy, "/photos/IMG_0001.JPG", desired)

			if err != nil || skipped {
				t.Fatalf("resolve() = (%q, %v, %v), want the desired path", got, skipped, err)
			}
			if got != desired {
				t.Fatalf("path = %q, want %q", got, desired)
			}
		})
	}
}

func TestResolveErrorPolicyFailsOnAnExistingFile(t *testing.T) {
	dir := t.TempDir()
	desired := filepath.Join(dir, "IMG_0001.JPG")
	writeFile(t, desired, "someone else's photo")

	d := newBatchDestinations(nil)

	got, skipped, err := d.resolve(config.CollisionError, "/photos/IMG_0001.JPG", desired)

	if err == nil {
		t.Fatal("resolve() error = nil, want a failure rather than an overwrite")
	}
	if skipped {
		t.Fatal("resolve() skipped = true, want a failure")
	}
	if got != "" {
		t.Fatalf("path = %q, want empty on failure", got)
	}
	if !strings.Contains(err.Error(), desired) {
		t.Fatalf("error = %q, want it to name the destination", err)
	}
	// The existing file is untouched.
	data, readErr := os.ReadFile(desired)
	if readErr != nil || string(data) != "someone else's photo" {
		t.Fatalf("existing file changed: %q, %v", data, readErr)
	}
}

func TestResolveSkipPolicyLeavesTheExistingFile(t *testing.T) {
	dir := t.TempDir()
	desired := filepath.Join(dir, "IMG_0001.JPG")
	writeFile(t, desired, "someone else's photo")

	d := newBatchDestinations(nil)

	got, skipped, err := d.resolve(config.CollisionSkip, "/photos/IMG_0001.JPG", desired)

	if err != nil {
		t.Fatalf("resolve() error = %v, want a skip", err)
	}
	if !skipped {
		t.Fatal("resolve() skipped = false, want a skip")
	}
	if got != "" {
		t.Fatalf("path = %q, want empty on skip", got)
	}
	data, _ := os.ReadFile(desired)
	if string(data) != "someone else's photo" {
		t.Fatalf("existing file = %q, want it left alone", data)
	}
}

func TestResolveRenamePolicySuffixesTheName(t *testing.T) {
	dir := t.TempDir()
	desired := filepath.Join(dir, "IMG_0001.JPG")
	writeFile(t, desired, "someone else's photo")

	d := newBatchDestinations(nil)

	got, skipped, err := d.resolve(config.CollisionRename, "/photos/IMG_0001.JPG", desired)

	if err != nil || skipped {
		t.Fatalf("resolve() = (%q, %v, %v), want a renamed path", got, skipped, err)
	}
	if want := filepath.Join(dir, "IMG_0001-2.JPG"); got != want {
		t.Fatalf("path = %q, want %q", got, want)
	}
}

func TestResolveRenamePolicyUsesTheLowestFreeSuffix(t *testing.T) {
	dir := t.TempDir()
	desired := filepath.Join(dir, "IMG_0001.JPG")
	writeFile(t, desired, "one")
	writeFile(t, filepath.Join(dir, "IMG_0001-2.JPG"), "two")
	writeFile(t, filepath.Join(dir, "IMG_0001-3.JPG"), "three")

	d := newBatchDestinations(nil)

	got, _, err := d.resolve(config.CollisionRename, "/photos/IMG_0001.JPG", desired)
	if err != nil {
		t.Fatalf("resolve() error = %v", err)
	}
	if want := filepath.Join(dir, "IMG_0001-4.JPG"); got != want {
		t.Fatalf("path = %q, want the lowest free suffix %q", got, want)
	}
}

// TestResolveRenameKeepsNamesStableAcrossSyncs is the acceptance criterion that
// repeated syncs must reuse the same destination rather than renaming again.
func TestResolveRenameKeepsNamesStableAcrossSyncs(t *testing.T) {
	dir := t.TempDir()
	desired := filepath.Join(dir, "IMG_0001.JPG")
	writeFile(t, desired, "someone else's photo")

	const remotePath = "/photos/IMG_0001.JPG"

	// First sync: a name conflict, so the file is renamed and recorded.
	first := newBatchDestinations(nil)
	chosen, _, err := first.resolve(config.CollisionRename, remotePath, desired)
	if err != nil {
		t.Fatalf("first resolve: %v", err)
	}
	writeFile(t, chosen, "our photo")

	// Second sync, now with the manifest record the batch machinery writes.
	entries := map[string]state.Entry{remotePath: manifestEntry(chosen)}
	second := newBatchDestinations(entries)

	again, skipped, err := second.resolve(config.CollisionRename, remotePath, desired)
	if err != nil || skipped {
		t.Fatalf("second resolve = (%q, %v, %v), want the recorded path", again, skipped, err)
	}
	if again != chosen {
		t.Fatalf("second resolve = %q, want %q: an update must replace the same file", again, chosen)
	}
}

// TestResolveRecordedPathWinsEvenThoughTheFileExists covers an update to a file
// that was previously renamed: the recorded destination must be reused even
// though a file is already sitting there, because that file is ours.
func TestResolveRecordedPathWinsEvenThoughTheFileExists(t *testing.T) {
	dir := t.TempDir()
	recorded := filepath.Join(dir, "IMG_0001-2.JPG")
	writeFile(t, recorded, "an older copy of our photo")

	desired := filepath.Join(dir, "IMG_0001.JPG")
	writeFile(t, desired, "someone else's photo")

	entries := map[string]state.Entry{"/photos/IMG_0001.JPG": manifestEntry(recorded)}

	for _, policy := range []string{config.CollisionError, config.CollisionSkip, config.CollisionRename} {
		t.Run(policy, func(t *testing.T) {
			d := newBatchDestinations(entries)

			got, skipped, err := d.resolve(policy, "/photos/IMG_0001.JPG", desired)

			if err != nil || skipped {
				t.Fatalf("resolve() = (%q, %v, %v), want the recorded path", got, skipped, err)
			}
			if got != recorded {
				t.Fatalf("path = %q, want the recorded %q", got, recorded)
			}
		})
	}
}

// TestResolveRenameIsStableWhenTheRecordedPathIsContested covers a manifest that
// maps two remote files to one local path, for example after hand editing.
func TestResolveRenameIsStableWhenTheRecordedPathIsContested(t *testing.T) {
	dir := t.TempDir()
	contested := filepath.Join(dir, "IMG_0001.JPG")
	writeFile(t, contested, "contested")

	entries := map[string]state.Entry{
		"/photos/a/IMG_0001.JPG": manifestEntry(contested),
		"/photos/b/IMG_0001.JPG": manifestEntry(contested),
	}
	d := newBatchDestinations(entries)

	first, _, err := d.resolve(config.CollisionRename, "/photos/a/IMG_0001.JPG", contested)
	if err != nil {
		t.Fatalf("first resolve: %v", err)
	}
	if first != contested {
		t.Fatalf("first path = %q, want the uncontested %q", first, contested)
	}

	second, _, err := d.resolve(config.CollisionRename, "/photos/b/IMG_0001.JPG", contested)
	if err != nil {
		t.Fatalf("second resolve: %v", err)
	}
	if second == contested {
		t.Fatalf("second path = %q, want a different destination", second)
	}
	if want := filepath.Join(dir, "IMG_0001-2.JPG"); second != want {
		t.Fatalf("second path = %q, want %q", second, want)
	}
}

// TestResolveIsConcurrencySafe is the requirement that two workers must never be
// told the same destination. Every concurrent caller asks for the same desired
// path; each must come away with a distinct name.
func TestResolveIsConcurrencySafe(t *testing.T) {
	const workers = 16

	dir := t.TempDir()
	desired := filepath.Join(dir, "IMG_0001.JPG")
	writeFile(t, desired, "someone else's photo")

	d := newBatchDestinations(nil)

	var (
		mu    sync.Mutex
		paths = make(map[string]string)
		wg    sync.WaitGroup
	)

	for i := 0; i < workers; i++ {
		remotePath := fmt.Sprintf("/photos/%d/IMG_0001.JPG", i)
		wg.Add(1)
		go func(remotePath string) {
			defer wg.Done()
			got, skipped, err := d.resolve(config.CollisionRename, remotePath, desired)
			if err != nil || skipped {
				t.Errorf("resolve(%s) = (%q, %v, %v)", remotePath, got, skipped, err)
				return
			}
			mu.Lock()
			defer mu.Unlock()
			if owner, taken := paths[got]; taken {
				t.Errorf("destination %q given to both %s and %s", got, owner, remotePath)
			}
			paths[got] = remotePath
		}(remotePath)
	}
	wg.Wait()

	if len(paths) != workers {
		t.Fatalf("got %d distinct destinations for %d workers", len(paths), workers)
	}
}

// TestResolveSkipDoesNotReserveTheExistingFile checks that a skipped file leaves
// the name free: a skip decides not to write, so it must not claim the path and
// block another remote file from renaming to it.
func TestResolveSkipDoesNotReserveTheExistingFile(t *testing.T) {
	dir := t.TempDir()
	desired := filepath.Join(dir, "IMG_0001.JPG")
	writeFile(t, desired, "someone else's photo")

	d := newBatchDestinations(nil)

	if _, skipped, err := d.resolve(config.CollisionSkip, "/photos/a/IMG_0001.JPG", desired); !skipped || err != nil {
		t.Fatalf("skip resolve = (%v, %v), want a skip", skipped, err)
	}
	renamed, skipped, err := d.resolve(config.CollisionRename, "/photos/b/IMG_0001.JPG", desired)
	if err != nil || skipped {
		t.Fatalf("rename resolve = (%q, %v, %v)", renamed, skipped, err)
	}
	if renamed == desired {
		t.Fatalf("renamed path = %q, want a suffixed name", renamed)
	}
}

// TestResolveFailsWhenTheSuffixSearchIsExhausted bounds the search rather than
// looping forever on a directory full of conflicting names. The budget is shrunk
// for the test, because 1000 conflicting files would be slow to create.
func TestResolveFailsWhenTheSuffixSearchIsExhausted(t *testing.T) {
	dir := t.TempDir()
	desired := filepath.Join(dir, "IMG_0001.JPG")

	entries := map[string]state.Entry{
		"/photos/a/IMG_0001.JPG": manifestEntry(suffixed(desired, 1)),
		"/photos/b/IMG_0001.JPG": manifestEntry(suffixed(desired, 2)),
	}

	d := newBatchDestinations(entries)
	d.maxAttempts = 2

	got, skipped, err := d.resolve(config.CollisionRename, "/photos/new/IMG_0001.JPG", desired)

	if err == nil {
		t.Fatalf("resolve() = (%q, %v, nil), want an error once the budget is spent", got, skipped)
	}
	if !strings.Contains(err.Error(), "2 attempts") {
		t.Fatalf("error = %q, want it to name the attempt budget", err)
	}
}

// TestResolveContinuesPastClaimedSuffixes checks the search does not stop at the
// first taken name.
func TestResolveContinuesPastClaimedSuffixes(t *testing.T) {
	dir := t.TempDir()
	desired := filepath.Join(dir, "IMG_0001.JPG")

	entries := map[string]state.Entry{
		"/photos/a/IMG_0001.JPG": manifestEntry(suffixed(desired, 1)),
		"/photos/b/IMG_0001.JPG": manifestEntry(suffixed(desired, 2)),
	}

	d := newBatchDestinations(entries)

	got, _, err := d.resolve(config.CollisionRename, "/photos/new/IMG_0001.JPG", desired)
	if err != nil {
		t.Fatalf("resolve() error = %v", err)
	}
	if want := suffixed(desired, 3); got != want {
		t.Fatalf("path = %q, want %q", got, want)
	}
}

func TestSuffixed(t *testing.T) {
	tests := []struct {
		attempt int
		want    string
	}{
		{attempt: 1, want: "/photos/IMG_0001.JPG"},
		{attempt: 2, want: "/photos/IMG_0001-2.JPG"},
		{attempt: 10, want: "/photos/IMG_0001-10.JPG"},
	}

	for _, tt := range tests {
		if got := suffixed("/photos/IMG_0001.JPG", tt.attempt); got != tt.want {
			t.Fatalf("suffixed(attempt %d) = %q, want %q", tt.attempt, got, tt.want)
		}
	}

	// A name with no extension is still suffixed predictably.
	if got := suffixed("/photos/IMG_0001", 2); got != "/photos/IMG_0001-2" {
		t.Fatalf("suffixed() without an extension = %q", got)
	}
	// Dots in directory names are not treated as extensions.
	if got := suffixed("/photos/2024.06/IMG_0001", 2); got != "/photos/2024.06/IMG_0001-2" {
		t.Fatalf("suffixed() with a dotted directory = %q", got)
	}
}

// TestOldManifestWithoutLocalPathsStillLoads covers the backward-compatibility
// requirement: a manifest written before this change has no local_path fields.
func TestOldManifestWithoutLocalPathsStillLoads(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "manifest.json")
	old := `{"entries":{"/photos/IMG_0001.JPG":{"mtime":"2024-06-15T12:00:00Z","size":100}}}`
	writeFile(t, path, old)

	manifest, err := state.Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v, want the old manifest to load", err)
	}

	entry, ok := manifest.Get("/photos/IMG_0001.JPG")
	if !ok {
		t.Fatal("entry missing from the old manifest")
	}
	if entry.LocalPath != "" {
		t.Fatalf("local path = %q, want empty for an old manifest", entry.LocalPath)
	}

	// An old manifest must still resolve: with no record, the desired path is
	// used when it is free.
	destinations := newBatchDestinations(manifest.Entries)
	desired := filepath.Join(dir, "IMG_0001.JPG")
	got, skipped, err := destinations.resolve(config.CollisionRename, "/photos/IMG_0001.JPG", desired)
	if err != nil || skipped {
		t.Fatalf("resolve() = (%q, %v, %v), want the desired path", got, skipped, err)
	}
	if got != desired {
		t.Fatalf("path = %q, want %q", got, desired)
	}
}

func TestRecordSkippedRetiresTheFileFromTheBatch(t *testing.T) {
	s := testSyncer(t, time.Hour)
	s.beginDownload(3, 300)

	s.recordSkipped("/photos/a/IMG_0001.JPG")
	s.recordSkipped("/photos/b/IMG_0001.JPG")

	status := s.Status()
	if status.Skipped != 2 {
		t.Fatalf("skipped = %d, want 2", status.Skipped)
	}
	if status.Completed != 0 || status.Failed != 0 {
		t.Fatalf("completed = %d, failed = %d, want a skip to count as neither",
			status.Completed, status.Failed)
	}
	if status.Remaining != 1 {
		t.Fatalf("remaining = %d, want 1: a skipped file is not still in flight", status.Remaining)
	}
	if status.Pending != status.Remaining {
		t.Fatalf("pending = %d, want it to track remaining (%d)", status.Pending, status.Remaining)
	}
}

func TestRecordSkipOrFailureRecordsAFailure(t *testing.T) {
	s := testSyncer(t, time.Hour)
	s.beginDownload(2, 200)

	s.recordSkipOrFailure("/photos/a/IMG_0001.JPG", errCollision)
	s.recordSkipOrFailure("/photos/b/IMG_0001.JPG", nil)

	status := s.Status()
	if status.Failed != 1 {
		t.Fatalf("failed = %d, want 1", status.Failed)
	}
	if status.Skipped != 1 {
		t.Fatalf("skipped = %d, want 1", status.Skipped)
	}
	if status.Remaining != 0 {
		t.Fatalf("remaining = %d, want the batch finished", status.Remaining)
	}
}

func TestBeginDownloadResetsSkipCounters(t *testing.T) {
	s := testSyncer(t, time.Hour)
	s.beginDownload(2, 200)
	s.recordSkipped("/photos/a/IMG_0001.JPG")
	s.recordFileResult("/photos/b/IMG_0001.JPG", nil, 10)

	s.beginDownload(1, 100)

	status := s.Status()
	if status.Skipped != 0 || status.Completed != 0 || status.Failed != 0 {
		t.Fatalf("counters leaked between batches: %+v", status)
	}
	if status.BatchTotal != 1 || status.Remaining != 1 {
		t.Fatalf("batch counters = %d/%d, want 1/1", status.Remaining, status.BatchTotal)
	}
}

func TestManifestRecordsTheChosenDestination(t *testing.T) {
	// A round trip through the manifest is what makes a renamed destination
	// stable, so the field must survive a save and a reload.
	dir := t.TempDir()
	path := filepath.Join(dir, "manifest.json")

	manifest, err := state.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	manifest.Set("/photos/IMG_0001.JPG", manifestEntry(filepath.Join(dir, "IMG_0001-2.JPG")))
	if err := manifest.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}

	reloaded, err := state.Load(path)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	entry, ok := reloaded.Get("/photos/IMG_0001.JPG")
	if !ok {
		t.Fatal("entry missing after reload")
	}
	if want := filepath.Join(dir, "IMG_0001-2.JPG"); entry.LocalPath != want {
		t.Fatalf("local path = %q, want %q", entry.LocalPath, want)
	}
}

// errCollision stands in for the error a collision policy produces.
var errCollision = errors.New("destination is already occupied by a local file")
