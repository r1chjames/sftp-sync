package sftp

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"strings"
	"testing"
	"time"
)

// step is one thing a fake walker reports.
type step struct {
	path string
	err  error
	info os.FileInfo
}

// fakeWalker replays steps, so the walk aggregation can be tested without a
// server. The real walker is a *sftp.Walker, which this stands in for.
type fakeWalker struct {
	steps []step
	index int
}

func (w *fakeWalker) Step() bool {
	if w.index >= len(w.steps) {
		return false
	}
	w.index++
	return true
}

func (w *fakeWalker) Path() string { return w.steps[w.index-1].path }

func (w *fakeWalker) Stat() os.FileInfo { return w.steps[w.index-1].info }

func (w *fakeWalker) Err() error { return w.steps[w.index-1].err }

// fakeInfo is a minimal os.FileInfo for a regular file.
type fakeInfo struct {
	name string
	size int64
	dir  bool
}

func (i fakeInfo) Name() string       { return i.name }
func (i fakeInfo) Size() int64        { return i.size }
func (i fakeInfo) Mode() os.FileMode  { return 0644 }
func (i fakeInfo) ModTime() time.Time { return time.Unix(0, 0) }
func (i fakeInfo) IsDir() bool        { return i.dir }
func (i fakeInfo) Sys() any           { return nil }

func file(path string, size int64) step {
	return step{path: path, info: fakeInfo{name: path, size: size}}
}

func TestWalkFilesCollectsFilesAndSkipsDirectories(t *testing.T) {
	walker := &fakeWalker{steps: []step{
		{path: "/photos", info: fakeInfo{name: "photos", dir: true}},
		file("/photos/a.jpg", 10),
		file("/photos/b.jpg", 20),
	}}

	result, err := walkFiles(context.Background(), "/photos", walker)

	if err != nil {
		t.Fatalf("walkFiles() error = %v", err)
	}
	if !result.Complete() {
		t.Fatalf("Complete() = false, want true: %+v", result)
	}
	if len(result.Files) != 2 {
		t.Fatalf("files = %+v, want 2", result.Files)
	}
	if result.Files[0].Path != "/photos/a.jpg" || result.Files[0].Size != 10 {
		t.Fatalf("first file = %+v", result.Files[0])
	}
	if result.Summary() != "" {
		t.Fatalf("Summary() = %q, want empty for a complete walk", result.Summary())
	}
}

// TestWalkFilesReportsUnreadableEntries is the core of the issue: an unreadable
// path must not be silently skipped, because the result is then not an inventory
// of the server.
func TestWalkFilesReportsUnreadableEntries(t *testing.T) {
	walker := &fakeWalker{steps: []step{
		file("/photos/a.jpg", 10),
		{path: "/photos/locked", err: errors.New("permission denied")},
		file("/photos/b.jpg", 20),
	}}

	result, err := walkFiles(context.Background(), "/photos", walker)

	if err != nil {
		t.Fatalf("walkFiles() error = %v, want the readable files back", err)
	}
	if result.Complete() {
		t.Fatal("Complete() = true, want false when a subtree could not be read")
	}
	if len(result.Files) != 2 {
		t.Fatalf("files = %d, want both readable files", len(result.Files))
	}
	if result.FailureCount != 1 || len(result.Failures) != 1 {
		t.Fatalf("failures = %d/%d, want 1", len(result.Failures), result.FailureCount)
	}
	if result.Failures[0].Path != "/photos/locked" {
		t.Fatalf("failure path = %q", result.Failures[0].Path)
	}

	summary := result.Summary()
	if !strings.Contains(summary, "incomplete") || !strings.Contains(summary, "/photos/locked") {
		t.Fatalf("summary = %q, want it to name the unreadable path", summary)
	}
}

func TestWalkFilesTreatsAMissingStatAsUnreadable(t *testing.T) {
	walker := &fakeWalker{steps: []step{
		{path: "/photos/vanished", info: nil},
	}}

	result, err := walkFiles(context.Background(), "/photos", walker)

	if err != nil {
		t.Fatalf("walkFiles() error = %v", err)
	}
	if result.Complete() {
		t.Fatal("Complete() = true, want false")
	}
	if len(result.Files) != 0 {
		t.Fatalf("files = %+v, want none", result.Files)
	}
}

// TestWalkFilesFailsWhenTheRootIsUnreadable keeps the existing behaviour for a
// remote path that cannot be read at all: that is a configuration error, not a
// partial scan.
func TestWalkFilesFailsWhenTheRootIsUnreadable(t *testing.T) {
	wantErr := errors.New("no such file")
	walker := &fakeWalker{steps: []step{
		{path: "/photos", err: wantErr},
	}}

	_, err := walkFiles(context.Background(), "/photos", walker)

	if !errors.Is(err, wantErr) {
		t.Fatalf("error = %v, want it to wrap %v", err, wantErr)
	}
	if !strings.Contains(err.Error(), "unreadable") {
		t.Fatalf("error = %q, want it to say the root could not be read", err)
	}
}

// TestWalkFilesLimitsFailureAggregation bounds how much a broken subtree can add
// to a status response.
func TestWalkFilesLimitsFailureAggregation(t *testing.T) {
	steps := make([]step, 0, maxWalkFailures+5)
	for i := 0; i < maxWalkFailures+5; i++ {
		steps = append(steps, step{path: "/photos/locked", err: fs.ErrPermission})
	}

	result, err := walkFiles(context.Background(), "/photos", &fakeWalker{steps: steps})
	if err != nil {
		t.Fatalf("walkFiles() error = %v", err)
	}

	if result.FailureCount != maxWalkFailures+5 {
		t.Fatalf("FailureCount = %d, want %d", result.FailureCount, maxWalkFailures+5)
	}
	if len(result.Failures) != maxWalkFailures {
		t.Fatalf("kept %d failures, want at most %d", len(result.Failures), maxWalkFailures)
	}

	summary := result.Summary()
	if len(summary) > 400 {
		t.Fatalf("summary is %d bytes, want it bounded: %q", len(summary), summary)
	}
	if !strings.Contains(summary, "24 more in the daemon log") {
		t.Fatalf("summary = %q, want the unkept failures counted", summary)
	}
}

// TestWalkFilesStopsOnCancellation checks that a shutdown does not have to wait
// for a walk of a large tree to finish.
func TestWalkFilesStopsOnCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	steps := []step{
		file("/photos/a.jpg", 10),
		file("/photos/b.jpg", 10),
		file("/photos/c.jpg", 10),
	}
	walker := &cancellingWalker{fakeWalker: &fakeWalker{steps: steps}, cancelAfter: 2, cancel: cancel}

	result, err := walkFiles(ctx, "/photos", walker)

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
	// The file read before the stop is returned, and the entry stepped over as the
	// cancel arrived is not processed: cancellation means stop, and the caller
	// discards the result anyway.
	if len(result.Files) != 1 {
		t.Fatalf("files = %d, want the one file read before cancellation", len(result.Files))
	}
}

type cancellingWalker struct {
	*fakeWalker
	cancelAfter int
	cancel      func()
}

func (w *cancellingWalker) Step() bool {
	ok := w.fakeWalker.Step()
	if ok && w.fakeWalker.index == w.cancelAfter {
		w.cancel()
	}
	return ok
}
