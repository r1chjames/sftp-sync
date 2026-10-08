package syncer

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/pkg/sftp"
	"github.com/r1chjames/sftp-sync/internal/config"
	sftpclient "github.com/r1chjames/sftp-sync/internal/sftp"
	"github.com/r1chjames/sftp-sync/internal/state"
)

// errTransient is a failure the retry policy must treat as retryable.
var errTransient = errors.New("connection lost")

// testProgress builds a progress tracker wired to a syncer, as the worker does.
func testProgress(s *Syncer, path string, total int64) *fileProgress {
	return &fileProgress{
		total:    total,
		interval: 0,
		now:      time.Now,
		record:   func(delta, copied, fileTotal int64) { s.recordBytes(path, delta, copied, fileTotal) },
	}
}

// scriptedFetch returns a fetch function that yields one result per call, and a
// counter of how many calls were made.
func scriptedFetch(results []error, tmpPath string) (func() (string, error), *int) {
	calls := 0
	return func() (string, error) {
		err := results[calls]
		calls++
		if err != nil {
			return "", err
		}
		return tmpPath, nil
	}, &calls
}

func retryTestSyncer(t *testing.T, maxAttempts int) *Syncer {
	t.Helper()
	s := testSyncer(t, time.Hour)
	s.cfg.Sync.MaxAttempts = maxAttempts
	// Deterministic backoff, and waits that do not actually sleep.
	s.jitter = func() float64 { return 0 }
	return s
}

func TestDownloadWithRetriesSucceedsAfterATransientFailure(t *testing.T) {
	s := retryTestSyncer(t, 3)
	fp := testProgress(s, "/photos/a.jpg", 100)
	fetch, calls := scriptedFetch([]error{
		&net.OpError{Op: "read", Err: syscall.ECONNRESET},
		nil,
	}, "/tmp/a.jpg.tmp")

	got, err := s.downloadWithRetries(context.Background(), "/photos/a.jpg", fp, fetch)

	if err != nil {
		t.Fatalf("downloadWithRetries() error = %v, want success on the second attempt", err)
	}
	if got != "/tmp/a.jpg.tmp" {
		t.Fatalf("temp path = %q, want %q", got, "/tmp/a.jpg.tmp")
	}
	if *calls != 2 {
		t.Fatalf("fetch called %d times, want 2", *calls)
	}
}

func TestDownloadWithRetriesGivesUpAfterTheBudget(t *testing.T) {
	s := retryTestSyncer(t, 3)
	fp := testProgress(s, "/photos/a.jpg", 100)
	fetch, calls := scriptedFetch([]error{
		io.ErrUnexpectedEOF,
		io.ErrUnexpectedEOF,
		io.ErrUnexpectedEOF,
		nil, // must never be reached
	}, "")

	_, err := s.downloadWithRetries(context.Background(), "/photos/a.jpg", fp, fetch)

	if err == nil {
		t.Fatal("downloadWithRetries() = nil, want a failure once the budget is spent")
	}
	if !strings.Contains(err.Error(), "after 3 attempt(s)") {
		t.Fatalf("error = %q, want it to name the attempt budget", err)
	}
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("error %v does not wrap the last failure", err)
	}
	if *calls != 3 {
		t.Fatalf("fetch called %d times, want exactly the budget of 3", *calls)
	}
}

// TestDownloadWithRetriesStopsOnADeterministicFailure is the requirement that a
// permission problem is not retried: no number of attempts can fix it.
func TestDownloadWithRetriesStopsOnADeterministicFailure(t *testing.T) {
	tests := []struct {
		name string
		err  error
	}{
		{name: "permission denied", err: &fs.PathError{Op: "open", Path: "/local/a.jpg", Err: syscall.EACCES}},
		{name: "missing local directory", err: &fs.PathError{Op: "open", Path: "/local/a.jpg", Err: syscall.ENOENT}},
		{name: "missing remote file", err: sftp.ErrSSHFxNoSuchFile},
		{name: "our own cancellation", err: context.Canceled},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := retryTestSyncer(t, 3)
			fp := testProgress(s, "/photos/a.jpg", 100)
			fetch, calls := scriptedFetch([]error{tt.err, nil}, "")

			_, err := s.downloadWithRetries(context.Background(), "/photos/a.jpg", fp, fetch)

			if !errors.Is(err, tt.err) {
				t.Fatalf("error = %v, want the original failure %v", err, tt.err)
			}
			if *calls != 1 {
				t.Fatalf("fetch called %d times, want 1: a deterministic failure is not retried", *calls)
			}
		})
	}
}

// TestDownloadWithRetriesCancelsDuringBackoff is the acceptance criterion that a
// shutdown interrupts a retry wait promptly.
func TestDownloadWithRetriesCancelsDuringBackoff(t *testing.T) {
	s := retryTestSyncer(t, 5)
	fp := testProgress(s, "/photos/a.jpg", 100)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	fetch, calls := scriptedFetch([]error{io.EOF, nil}, "")
	fetchWithCancel := func() (string, error) {
		path, err := fetch()
		// Cancel while the first failure is being handled, so the backoff for
		// the second attempt is entered with a cancelled context.
		cancel()
		return path, err
	}

	done := make(chan error, 1)
	go func() {
		_, err := s.downloadWithRetries(ctx, "/photos/a.jpg", fp, fetchWithCancel)
		done <- err
	}()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("error = %v, want context.Canceled", err)
		}
		if !strings.Contains(err.Error(), "interrupted after 1 attempt") {
			t.Fatalf("error = %q, want it to report the interrupted retry", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("retry wait did not return promptly after cancellation")
	}

	if *calls != 1 {
		t.Fatalf("fetch called %d times, want 1: the retry must not start", *calls)
	}
}

// TestDownloadWithRetriesDiscardsFailedAttemptBytes checks that a retry does not
// count the same file twice in the batch byte totals.
func TestDownloadWithRetriesDiscardsFailedAttemptBytes(t *testing.T) {
	s := retryTestSyncer(t, 3)
	s.beginDownload(1, 1000)
	fp := testProgress(s, "/photos/a.jpg", 1000)

	fetch, _ := scriptedFetch([]error{io.EOF, nil}, "/tmp/a.jpg.tmp")
	withBytes := func() (string, error) {
		path, err := fetch()
		if err == nil {
			fp.update(1000)
			fp.flush()
		} else {
			// The attempt copied 400 bytes before it failed.
			fp.update(400)
			fp.flush()
		}
		return path, err
	}

	if _, err := s.downloadWithRetries(context.Background(), "/photos/a.jpg", fp, withBytes); err != nil {
		t.Fatalf("downloadWithRetries() error = %v", err)
	}

	if got := s.Status().BytesCompleted; got != 1000 {
		t.Fatalf("bytes completed = %d, want 1000: the failed attempt's bytes are not progress", got)
	}
}

func TestTransientClassification(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{name: "nil", err: nil, want: false},
		{name: "eof", err: io.EOF, want: true},
		{name: "unexpected eof", err: io.ErrUnexpectedEOF, want: true},
		{name: "closed pipe", err: io.ErrClosedPipe, want: true},
		{name: "connection reset", err: syscall.ECONNRESET, want: true},
		{name: "connection refused", err: syscall.ECONNREFUSED, want: true},
		{name: "broken pipe", err: syscall.EPIPE, want: true},
		{name: "timeout", err: syscall.ETIMEDOUT, want: true},
		{name: "deadline exceeded", err: os.ErrDeadlineExceeded, want: true},
		{name: "net op error", err: &net.OpError{Op: "read", Err: errors.New("reset by peer")}, want: true},
		{name: "sftp connection lost", err: sftp.ErrSSHFxConnectionLost, want: true},
		{name: "wrapped transient", err: wrap("open remote", io.ErrUnexpectedEOF), want: true},
		{name: "checksum mismatch", err: errChecksumMismatch, want: true},

		{name: "permission denied", err: syscall.EACCES, want: false},
		{name: "no such file", err: syscall.ENOENT, want: false},
		{name: "sftp protocol failure", err: sftp.ErrSSHFxFailure, want: false},
		{name: "cancelled", err: context.Canceled, want: false},
		{name: "deadline from caller", err: context.DeadlineExceeded, want: false},
		{name: "unknown error", err: errors.New("something went wrong"), want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := transient(tt.err); got != tt.want {
				t.Fatalf("transient(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

func wrap(prefix string, err error) error {
	return errors.Join(errors.New(prefix), err)
}

func TestRetryDelayIsBoundedAndJittered(t *testing.T) {
	noJitter := func() float64 { return 0 }
	fullJitter := func() float64 { return 0.999 }

	tests := []struct {
		attempt int
		wantMin time.Duration
		wantMax time.Duration
	}{
		{attempt: 1, wantMin: 0, wantMax: 0},
		{attempt: 2, wantMin: retryBaseDelay / 2, wantMax: retryBaseDelay},
		{attempt: 3, wantMin: retryBaseDelay, wantMax: 2 * retryBaseDelay},
		{attempt: 4, wantMin: 2 * retryBaseDelay, wantMax: 4 * retryBaseDelay},
		{attempt: 5, wantMin: 4 * retryBaseDelay, wantMax: retryMaxDelay},
		{attempt: 9, wantMin: retryMaxDelay / 2, wantMax: retryMaxDelay},
		{attempt: 20, wantMin: retryMaxDelay / 2, wantMax: retryMaxDelay},
	}

	for _, tt := range tests {
		low := retryDelay(tt.attempt, noJitter)
		high := retryDelay(tt.attempt, fullJitter)

		if low < tt.wantMin || low > tt.wantMax {
			t.Fatalf("retryDelay(attempt %d, no jitter) = %v, want within [%v, %v]",
				tt.attempt, low, tt.wantMin, tt.wantMax)
		}
		if high < tt.wantMin || high > tt.wantMax {
			t.Fatalf("retryDelay(attempt %d, full jitter) = %v, want within [%v, %v]",
				tt.attempt, high, tt.wantMin, tt.wantMax)
		}
		if high > retryMaxDelay {
			t.Fatalf("retryDelay(attempt %d) = %v, want no more than %v", tt.attempt, high, retryMaxDelay)
		}
	}

	// The wait must not collapse to nothing, or a retry would become a spin.
	if got := retryDelay(2, noJitter); got <= 0 {
		t.Fatalf("retryDelay(2) = %v, want a positive minimum wait", got)
	}
}

func TestTruncateText(t *testing.T) {
	tests := []struct {
		name  string
		text  string
		limit int
		want  string
	}{
		{name: "shorter than the limit", text: "short", limit: 10, want: "short"},
		{name: "exactly the limit", text: "0123456789", limit: 10, want: "0123456789"},
		{name: "cut", text: "0123456789", limit: 5, want: "01…"},
		{name: "multibyte is cut on a boundary", text: "héllo wörld", limit: 6, want: "hé…"},
		{name: "zero limit is unlimited", text: "0123456789", limit: 0, want: "0123456789"},
		{name: "limit below the marker", text: "0123456789", limit: 1, want: "…"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := truncateText(tt.text, tt.limit)
			if got != tt.want {
				t.Fatalf("truncateText(%q, %d) = %q, want %q", tt.text, tt.limit, got, tt.want)
			}
			if !utf8.ValidString(got) {
				t.Fatalf("truncateText(%q, %d) = %q, want valid UTF-8", tt.text, tt.limit, got)
			}
		})
	}
}

// TestApplyDownloadOutcomesKeepsFailuresOutOfTheManifest covers the acceptance
// criteria that successful files are not retried later and failed files are: only
// successes enter the manifest, so a failure is still new on the next cycle.
func TestApplyDownloadOutcomesKeepsFailuresOutOfTheManifest(t *testing.T) {
	dir := t.TempDir()
	s := testSyncer(t, time.Hour)
	manifest, err := state.Load(filepath.Join(dir, "manifest.json"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	s.manifest = manifest

	mtime := time.Date(2024, 6, 15, 12, 0, 0, 0, time.UTC)
	outcomes := []downloadOutcome{
		{file: sftpclient.RemoteFile{Path: "/photos/good.jpg", MTime: mtime, Size: 10}, localPath: filepath.Join(dir, "good.jpg")},
		{file: sftpclient.RemoteFile{Path: "/photos/bad.jpg", MTime: mtime, Size: 20}, err: errTransient},
		{file: sftpclient.RemoteFile{Path: "/photos/conflict.jpg", MTime: mtime, Size: 30}, skipped: true},
	}

	result := s.applyDownloadOutcomes(outcomes, len(outcomes), len(outcomes))

	if result.Completed != 1 || result.Failed != 1 || result.Skipped != 1 {
		t.Fatalf("result = %+v, want 1 completed, 1 failed, 1 skipped", result)
	}
	if _, ok := manifest.Get("/photos/good.jpg"); !ok {
		t.Fatal("successful file missing from the manifest")
	}
	if _, ok := manifest.Get("/photos/bad.jpg"); ok {
		t.Fatal("failed file was recorded as synced")
	}
	if _, ok := manifest.Get("/photos/conflict.jpg"); ok {
		t.Fatal("skipped file was recorded as synced")
	}

	// The failure is reported with context and a bounded message.
	summary := result.failureError()
	if summary == nil {
		t.Fatal("failureError() = nil, want a summary")
	}
	if !strings.Contains(summary.Error(), "/photos/bad.jpg") {
		t.Fatalf("summary = %q, want it to name the failed file", summary)
	}
	if !strings.Contains(summary.Error(), "1 of 3 file(s) failed") {
		t.Fatalf("summary = %q, want it to count the failure", summary)
	}
}

func TestApplyDownloadOutcomesSucceedsWhenNothingFails(t *testing.T) {
	dir := t.TempDir()
	s := testSyncer(t, time.Hour)
	manifest, err := state.Load(filepath.Join(dir, "manifest.json"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	s.manifest = manifest

	result := s.applyDownloadOutcomes(nil, 0, 0)

	if err := result.failureError(); err != nil {
		t.Fatalf("failureError() = %v, want nil for a batch with no failures", err)
	}
	if result.Attempted != 0 || result.Failed != 0 {
		t.Fatalf("result = %+v, want an empty batch", result)
	}
}

// TestMaxAttemptsComesFromConfig checks the wiring the retry loop depends on.
func TestMaxAttemptsComesFromConfig(t *testing.T) {
	cfg := &config.Config{
		StatePath: filepath.Join(t.TempDir(), "manifest.json"),
		Sync:      config.SyncConfig{Interval: time.Hour, Workers: 1, MaxAttempts: 1},
	}
	s := New(cfg)

	fetch, calls := scriptedFetch([]error{io.EOF, nil}, "")

	_, err := s.downloadWithRetries(context.Background(), "/photos/a.jpg", testProgress(s, "/photos/a.jpg", 1), fetch)

	if err == nil {
		t.Fatal("error = nil, want a failure with max_attempts of 1")
	}
	if *calls != 1 {
		t.Fatalf("fetch called %d times, want 1 with max_attempts of 1", *calls)
	}
}
