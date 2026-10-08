package syncer

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"math/rand/v2"
	"net"
	"os"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/pkg/sftp"
)

// Retry backoff bounds. The first retry waits at least half of retryBaseDelay,
// and no wait exceeds retryMaxDelay, so a long outage cannot hold a worker for
// minutes between attempts.
const (
	retryBaseDelay = 500 * time.Millisecond
	retryMaxDelay  = 8 * time.Second
)

// maxFailureDetail bounds how much of one file's error is repeated into the
// status and API response. The full error is always in the log.
const maxFailureDetail = 200

// transient reports whether an error is worth retrying.
//
// Only errors that are recognised as a transport failure are retried. An
// unrecognised error is treated as deterministic, because retrying a permission
// problem or a path that does not exist can only waste attempts and delay the
// report: the issue is not going to fix itself between two attempts.
func transient(err error) bool {
	if err == nil {
		return false
	}

	// Our own shutdown, a deadline we set, or a deliberate stop: retrying would
	// fight the cancellation rather than survive a network blip.
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}

	// Local filesystem problems are deterministic in both directions: a missing
	// staging directory and a denied write are not going to change.
	if errors.Is(err, fs.ErrPermission) || errors.Is(err, fs.ErrNotExist) {
		return false
	}

	// A transfer whose bytes did not survive it is worth another attempt: that
	// is exactly what the retry may fix.
	if errors.Is(err, errChecksumMismatch) {
		return true
	}

	// Short reads and truncated transfers are the classic flaky-network case
	// and are always worth another attempt.
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.ErrClosedPipe) {
		return true
	}
	if errors.Is(err, os.ErrDeadlineExceeded) {
		return true
	}

	// Connection-level failures.
	if errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.ECONNREFUSED) ||
		errors.Is(err, syscall.ECONNABORTED) || errors.Is(err, syscall.EPIPE) ||
		errors.Is(err, syscall.ETIMEDOUT) || errors.Is(err, syscall.EHOSTUNREACH) ||
		errors.Is(err, syscall.ENETUNREACH) {
		return true
	}
	if errors.Is(err, sftp.ErrSSHFxConnectionLost) || errors.Is(err, sftp.ErrSSHFxNoConnection) {
		return true
	}

	var netErr net.Error
	if errors.As(err, &netErr) {
		return true
	}

	return false
}

// retryDelay returns how long to wait before attempt number attempt, where the
// first retry is attempt 2. The delay doubles up to retryMaxDelay, then half of
// it is replaced by jitter so that several workers failing together do not all
// come back at the same instant.
func retryDelay(attempt int, jitter func() float64) time.Duration {
	if attempt < 2 {
		return 0
	}

	delay := retryBaseDelay
	for i := 2; i < attempt; i++ {
		delay *= 2
		if delay >= retryMaxDelay {
			delay = retryMaxDelay
			break
		}
	}
	if delay > retryMaxDelay {
		delay = retryMaxDelay
	}

	// Half fixed, half jittered: the wait stays bounded and never collapses to
	// zero, so a retry cannot become a tight loop.
	half := delay / 2
	return half + time.Duration(jitter()*float64(half))
}

// sleepFor waits for d, or returns early with the context error when the syncer
// is shutting down or the job is stopped. A retry wait must never delay a
// shutdown.
func (s *Syncer) sleepFor(ctx context.Context, d time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if d <= 0 {
		return nil
	}

	timer := time.NewTimer(d)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// downloadWithRetries runs fetch until it succeeds, the error is not retryable,
// or the attempt budget is spent.
//
// fetch is a parameter rather than a call to the client so the retry behaviour
// can be scripted in tests, which is the only way to cover fail-then-succeed and
// exhausted retries without a live SFTP server.
func (s *Syncer) downloadWithRetries(
	ctx context.Context,
	remotePath string,
	fp *fileProgress,
	fetch func() (string, error),
) (string, error) {
	maxAttempts := s.cfg.Sync.MaxAttempts
	if maxAttempts < 1 {
		maxAttempts = 1
	}

	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		if attempt > 1 {
			delay := retryDelay(attempt, s.jitter)
			if err := s.sleepFor(ctx, delay); err != nil {
				return "", fmt.Errorf("retry of %s interrupted after %d attempt(s): %w", remotePath, attempt-1, err)
			}
			log.Printf("retrying %s (attempt %d of %d) after %v", remotePath, attempt, maxAttempts, lastErr)
		}

		tmpPath, err := fetch()
		fp.flush()
		if err == nil {
			return tmpPath, nil
		}
		lastErr = err

		if !transient(err) {
			// Report immediately: a deterministic failure does not improve with
			// another attempt, and the delay would only postpone the error.
			return "", err
		}

		// The connection is broken, so the next attempt must not reuse it. It is
		// marked rather than closed here: other workers may be mid-transfer on
		// the same connection, and only the next user of it may replace it.
		s.client.MarkStale()

		// The bytes this attempt copied are not progress: the file is not
		// written, and the retry starts again from the beginning.
		s.discardAttempt(fp)
	}

	return "", fmt.Errorf("after %d attempt(s): %w", maxAttempts, lastErr)
}

// discardAttempt removes the bytes an unsuccessful attempt contributed, so a
// retry does not count the same file twice, and resets the progress tracker for
// the next attempt.
func (s *Syncer) discardAttempt(fp *fileProgress) {
	copied := fp.copied
	fp.reset()
	if copied <= 0 {
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.status.BytesCompleted -= copied
	if s.status.BytesCompleted < 0 {
		s.status.BytesCompleted = 0
	}
	s.status.CurrentFileBytesCompleted = 0
}

// truncateText shortens text to at most limit bytes, marking that it was cut.
// It is used to bound what a single file's error can add to status and API
// responses.
//
// The cut is moved back to a character boundary, because these strings carry
// filenames: splitting a multi-byte character would put invalid UTF-8 into the
// status message and corrupt the name it is meant to describe.
func truncateText(text string, limit int) string {
	if limit <= 0 || len(text) <= limit {
		return text
	}
	const marker = "…"
	if limit <= len(marker) {
		return marker
	}

	cut := limit - len(marker)
	for cut > 0 && !utf8.RuneStart(text[cut]) {
		cut--
	}
	return text[:cut] + marker
}

// defaultJitter returns a value in [0, 1) used to spread retries out.
func defaultJitter() float64 {
	return rand.Float64()
}
