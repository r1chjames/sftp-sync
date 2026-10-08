package syncer

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/r1chjames/sftp-sync/internal/config"
	"github.com/r1chjames/sftp-sync/internal/exif"
	sftpclient "github.com/r1chjames/sftp-sync/internal/sftp"
	"github.com/r1chjames/sftp-sync/internal/state"
)

// SyncPhase describes the current stage of a sync cycle.
type SyncPhase string

const (
	PhaseIdle        SyncPhase = "idle"
	PhaseScanning    SyncPhase = "scanning"
	PhaseDownloading SyncPhase = "downloading"
	PhasePaused      SyncPhase = "paused"
	PhaseError       SyncPhase = "error"
)

// errBatchPaused reports that a download batch stopped early because the job
// was paused. It is neither a success nor a failure: files that were already
// copying have been committed, and the rest stay pending for the next resume.
var errBatchPaused = errors.New("download batch paused")

// byteProgressInterval throttles byte-progress status updates so the status
// mutex is not taken for every 32 KiB write of every worker.
const byteProgressInterval = 200 * time.Millisecond

// SyncStatus is a snapshot of the syncer's current state.
type SyncStatus struct {
	Phase                     SyncPhase
	Paused                    bool
	LastSync                  time.Time
	LastSuccessfulSync        time.Time
	FilesTotal                int
	Pending                   int
	EligibleFiles             int
	BatchTotal                int
	Completed                 int
	Failed                    int
	Skipped                   int
	Remaining                 int
	BytesTotal                int64
	BytesCompleted            int64
	CurrentFile               string
	CurrentFileBytesTotal     int64
	CurrentFileBytesCompleted int64
	StartedAt                 time.Time
	LastError                 error
}

// Syncer polls an SFTP server and downloads new or changed files.
type Syncer struct {
	cfg      *config.Config
	client   *sftpclient.Client
	manifest *state.Manifest

	mu     sync.RWMutex
	status SyncStatus
	paused bool
	cancel context.CancelFunc
	done   chan struct{}

	// pauseCh wakes the run loop so it can reflect the paused state without
	// waiting out the current interval.
	pauseCh chan struct{}

	// syncNow carries coalesced immediate-sync requests. A buffered channel of
	// capacity one means any number of requests made while a cycle is running
	// collapse into a single follow-up cycle.
	syncNow chan struct{}

	// jitter supplies the random half of a retry backoff. It is a field so tests
	// can make backoff deterministic.
	jitter func() float64

	// hashRemote computes a remote file's content digest. It is a field so tests
	// can script digests without a server, in the same way syncFn is.
	hashRemote func(ctx context.Context, remotePath string) (string, error)

	// syncFn runs one cycle. It is a field so tests can drive the loop without
	// an SFTP server; production code always uses (*Syncer).sync.
	syncFn func(context.Context) error
}

func New(cfg *config.Config) *Syncer {
	s := &Syncer{
		cfg:     cfg,
		client:  sftpclient.New(cfg),
		status:  SyncStatus{Phase: PhaseIdle},
		jitter:  defaultJitter,
		done:    make(chan struct{}),
		syncNow: make(chan struct{}, 1),
		pauseCh: make(chan struct{}, 1),
	}
	s.syncFn = s.sync
	s.hashRemote = s.client.HashRemote
	return s
}

// Pause stops the job from starting new scans or downloads. A cycle that is
// already running is allowed to finish: files being copied are committed
// atomically, and files not yet started are left for the next resume. The
// status becomes paused once no cycle is running. Pause is idempotent.
func (s *Syncer) Pause() {
	s.mu.Lock()
	if s.paused {
		s.mu.Unlock()
		return
	}
	s.paused = true
	s.status.Paused = true
	s.mu.Unlock()

	// Wake the loop so a job waiting out its interval becomes paused now
	// rather than at the next tick.
	select {
	case s.pauseCh <- struct{}{}:
	default:
	}
}

// Resume clears the paused state and requests an immediate scan. Resume is
// idempotent.
func (s *Syncer) Resume() {
	s.mu.Lock()
	if !s.paused {
		s.mu.Unlock()
		return
	}
	s.paused = false
	s.status.Paused = false
	s.mu.Unlock()

	s.SyncNow()
}

// IsPaused reports whether the job is paused.
func (s *Syncer) IsPaused() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.paused
}

// markPaused publishes the paused phase and clears the in-flight file fields,
// because nothing is being transferred while paused.
func (s *Syncer) markPaused() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.status.Phase = PhasePaused
	s.status.CurrentFile = ""
	s.status.CurrentFileBytesTotal = 0
	s.status.CurrentFileBytesCompleted = 0
}

// shouldStopSubmitting reports whether a running download batch must stop
// taking on new files. Files already copying always finish.
func (s *Syncer) shouldStopSubmitting(ctx context.Context) bool {
	return ctx.Err() != nil || s.IsPaused()
}

// SyncNow requests an immediate sync cycle instead of waiting for the next
// interval tick. It never blocks: a request made while a cycle is already
// running is coalesced into a single follow-up cycle, so repeated calls cannot
// run two cycles concurrently.
func (s *Syncer) SyncNow() {
	select {
	case s.syncNow <- struct{}{}:
	default:
	}
}

// Start loads the manifest and begins the background polling loop.
func (s *Syncer) Start(ctx context.Context) error {
	m, err := state.Load(s.cfg.StatePath)
	if err != nil {
		return fmt.Errorf("load manifest: %w", err)
	}
	s.manifest = m

	ctx, cancel := context.WithCancel(ctx)
	s.cancel = cancel
	go s.run(ctx)
	return nil
}

// Stop signals the polling loop to exit and waits for it to finish.
func (s *Syncer) Stop() {
	if s.cancel != nil {
		s.cancel()
	}
	<-s.done
}

// Status returns a snapshot of the current sync state. Safe for concurrent use.
func (s *Syncer) Status() SyncStatus {
	s.mu.RLock()
	st := s.status
	s.mu.RUnlock()

	// A remote file can grow between the walk and the transfer, which would
	// otherwise publish more completed bytes than the batch total. Clamp the
	// published snapshot rather than the live counters so failure corrections
	// stay exact.
	if st.BytesTotal > 0 && st.BytesCompleted > st.BytesTotal {
		st.BytesCompleted = st.BytesTotal
	}
	return st
}

func (s *Syncer) beginScan() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.status.Phase = PhaseScanning
	s.status.BatchTotal = 0
	s.status.Completed = 0
	s.status.Failed = 0
	s.status.Remaining = 0
	s.status.Pending = 0
	s.status.BytesTotal = 0
	s.status.BytesCompleted = 0
	s.status.CurrentFile = ""
	s.status.CurrentFileBytesTotal = 0
	s.status.CurrentFileBytesCompleted = 0
	s.status.StartedAt = time.Now()
}

func (s *Syncer) setEligibleFiles(total int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.status.EligibleFiles = total
	s.status.FilesTotal = total
}

func (s *Syncer) beginDownload(total int, totalBytes int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.status.Phase = PhaseDownloading
	s.status.BatchTotal = total
	s.status.Remaining = total
	s.status.Pending = total
	s.status.BytesTotal = totalBytes
	s.status.BytesCompleted = 0
	s.status.Completed = 0
	s.status.Failed = 0
	s.status.Skipped = 0
}

// fileProgress tracks the bytes copied for one file and forwards deltas to the
// syncer. Updates are throttled so the status mutex is not taken on every write
// of every worker. Emitted bytes are always less than or equal to copied bytes,
// and flush reports any remainder.
type fileProgress struct {
	total    int64
	interval time.Duration
	now      func() time.Time
	record   func(delta, copied, total int64)

	copied   int64
	reported int64
	lastEmit time.Time
}

// update records the cumulative byte count reported by a single file transfer.
// It matches the signature expected by sftp.DownloadTempProgress.
func (p *fileProgress) update(copied int64) error {
	p.copied = copied
	if copied == p.reported {
		return nil
	}
	if !p.lastEmit.IsZero() && p.now().Sub(p.lastEmit) < p.interval {
		return nil
	}
	p.emit()
	return nil
}

// flush reports bytes withheld by throttling. Call it before recording the
// file's outcome so a failure subtracts exactly what the file contributed.
func (p *fileProgress) flush() {
	p.emit()
}

// reset forgets what this attempt reported, so a retry starts a fresh file
// transfer rather than continuing the byte count of the attempt that failed.
func (p *fileProgress) reset() {
	p.copied = 0
	p.reported = 0
	p.lastEmit = time.Time{}
}

func (p *fileProgress) emit() {
	delta := p.copied - p.reported
	if delta <= 0 {
		return
	}
	p.reported = p.copied
	p.lastEmit = p.now()
	p.record(delta, p.copied, p.total)
}

// recordBytes adds newly copied bytes for an in-flight file to the batch
// totals. With several workers running, the per-file fields describe whichever
// file last reported and are best-effort by design.
func (s *Syncer) recordBytes(path string, delta, fileCopied, fileTotal int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if delta > 0 {
		s.status.BytesCompleted += delta
	}
	s.status.CurrentFile = path
	s.status.CurrentFileBytesCompleted = fileCopied
	s.status.CurrentFileBytesTotal = fileTotal
}

func (s *Syncer) setCurrentFile(path string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.status.CurrentFile = path
}

// recordFileResult updates the batch counters after a single file completes.
// fileBytes is the number of bytes copied for that file: on failure those
// bytes were written to a temp file that is removed, so they must not count
// towards completed bytes.
func (s *Syncer) recordFileResult(path string, err error, fileBytes int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err != nil {
		s.status.Failed++
		s.status.BytesCompleted -= fileBytes
		if s.status.BytesCompleted < 0 {
			s.status.BytesCompleted = 0
		}
	} else {
		s.status.Completed++
	}
	s.finishFileLocked(path)
}

// recordSkipOrFailure records the outcome of a file that was settled before it
// could be written: a skip for the collision policy, or a failure.
func (s *Syncer) recordSkipOrFailure(path string, err error) {
	if err != nil {
		s.recordFileResult(path, err, 0)
		return
	}
	s.recordSkipped(path)
}

// recordSkipped records a file the collision policy left alone. A skipped file
// counts as handled so it does not leave the batch looking unfinished, but it is
// not a success and never enters the manifest.
func (s *Syncer) recordSkipped(path string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.status.Skipped++
	s.finishFileLocked(path)
}

// finishFileLocked retires one file from the batch counters.
func (s *Syncer) finishFileLocked(path string) {
	if s.status.Remaining > 0 {
		s.status.Remaining--
	}
	s.status.Pending = s.status.Remaining
	if s.status.CurrentFile == path {
		s.status.CurrentFile = ""
	}
}

func (s *Syncer) markSuccess() {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	s.status.Phase = PhaseIdle
	s.status.LastSync = now
	s.status.LastSuccessfulSync = now
	s.status.LastError = nil
	s.status.CurrentFile = ""
}

func (s *Syncer) markFailure(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.status.Phase = PhaseError
	s.status.LastSync = time.Now()
	s.status.LastError = err
}

// recordCycle applies the outcome of a single sync cycle to the status
// snapshot. A cycle cancelled by shutdown is not recorded as a failure:
// recording one would report an error for a job that was working correctly
// when the daemon was asked to stop.
func (s *Syncer) recordCycle(ctx context.Context, err error) {
	switch {
	case err == nil:
		s.markSuccess()
	case errors.Is(err, errBatchPaused):
		// Neither a success nor a failure: the remaining files stay pending
		// and are picked up after resume. The run loop sets the paused phase.
		log.Printf("sync paused: %v", err)
	case ctx.Err() != nil:
		log.Printf("sync interrupted: %v", err)
	default:
		log.Printf("sync error: %v", err)
		s.markFailure(err)
	}
}

func (s *Syncer) run(ctx context.Context) {
	defer close(s.done)

	// A reusable timer fires immediately so the first cycle runs at startup,
	// then paces later cycles. Reusing one timer instead of calling time.After
	// on every pass avoids leaving an abandoned timer behind on every tick.
	timer := time.NewTimer(0)
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			s.client.Close()
			return
		case <-timer.C:
		case <-s.syncNow:
		case <-s.pauseCh:
		}

		// The timer is not needed while the cycle runs. Its pending value was
		// either consumed above or is drained here.
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}

		// Never start new work while paused. Resume sends a SyncNow request,
		// and shutdown cancels the context, so waiting here is safe.
		if s.IsPaused() {
			s.markPaused()
			continue
		}

		s.recordCycle(ctx, s.syncFn(ctx))

		// A pause that arrived while this cycle ran takes effect now, without
		// scheduling another cycle.
		if s.IsPaused() {
			s.markPaused()
			continue
		}

		// A request that arrived while the cycle ran is served immediately;
		// otherwise wait a full interval.
		delay := s.cfg.Sync.Interval
		select {
		case <-s.syncNow:
			delay = 0
		default:
		}
		timer.Reset(delay)
	}
}

func (s *Syncer) sync(ctx context.Context) error {
	s.beginScan()

	if err := s.client.EnsureConnected(); err != nil {
		return fmt.Errorf("connect: %w", err)
	}

	remoteFiles, err := s.client.Walk(s.cfg.SFTP.RemotePath)
	if err != nil {
		s.client.Close() // force reconnect on next poll
		return fmt.Errorf("walk %s: %w", s.cfg.SFTP.RemotePath, err)
	}

	eligible, toDownload := s.selectForDownload(ctx, remoteFiles)
	s.setEligibleFiles(eligible)

	if err := s.manifest.Save(); err != nil {
		log.Printf("warning: could not save manifest after adoption: %v", err)
	}

	if len(toDownload) > 0 {
		var batchBytes int64
		for _, f := range toDownload {
			batchBytes += f.Size
		}
		log.Printf("downloading %d new/changed file(s) (of %d eligible, %d bytes)", len(toDownload), eligible, batchBytes)
		s.beginDownload(len(toDownload), batchBytes)
		result, err := s.downloadAll(ctx, toDownload)
		if err != nil {
			return err
		}
		log.Printf("batch finished: %d synced, %d skipped, %d failed of %d",
			result.Completed, result.Skipped, result.Failed, result.Attempted)
	} else {
		log.Printf("up to date — %d eligible remote file(s)", eligible)
	}

	return nil
}

func (s *Syncer) selectForDownload(ctx context.Context, remoteFiles []sftpclient.RemoteFile) (int, []sftpclient.RemoteFile) {
	var toDownload []sftpclient.RemoteFile
	eligible := 0
	for _, f := range remoteFiles {
		if ctx.Err() != nil {
			// Stopping is the caller's business, but there is no point
			// inspecting further files, and in hash mode each inspection can
			// read a remote file.
			break
		}

		if !s.matchesFilter(f.Path) {
			continue
		}
		eligible++
		entry, ok := s.manifest.Get(f.Path)
		if ok && entry.MTime.Equal(f.MTime) && entry.Size == f.Size {
			continue
		}

		if !ok {
			// A file the manifest has never seen may still be on disk from a
			// previous run, an interrupted manifest write, or a user copying it
			// there. Adopt it only when it can be shown to be the same file, so
			// an empty or truncated local file is not recorded as a synced photo.
			adoptPath := s.localPath(f.Path)
			adopted, digest, err := s.adoptable(ctx, f, adoptPath)
			switch {
			case err != nil:
				log.Printf("could not check existing local file %s: %v", adoptPath, err)
			case adopted:
				log.Printf("adopting existing local file: %s", f.Path)
				s.manifest.Set(f.Path, entryFor(f, adoptPath, digest))
				continue
			}
		}

		toDownload = append(toDownload, f)
	}
	return eligible, toDownload
}

func (s *Syncer) downloadAll(ctx context.Context, files []sftpclient.RemoteFile) (batchResult, error) {
	results := make(chan downloadOutcome, len(files))
	sem := make(chan struct{}, s.cfg.Sync.Workers)
	var wg sync.WaitGroup

	// One destination table per batch, so workers cannot choose the same path.
	destinations := newBatchDestinations(s.manifest.Entries)

	attempted := 0
	for _, f := range files {
		if s.shouldStopSubmitting(ctx) {
			break
		}

		attempted++
		wg.Add(1)
		sem <- struct{}{}
		go func(f sftpclient.RemoteFile) {
			defer wg.Done()
			defer func() { <-sem }()

			s.setCurrentFile(f.Path)

			// With no EXIF folder structure the destination does not depend on
			// the file's contents, so a collision is settled before the transfer.
			// That matters most for skipped files: without it, every poll would
			// download a file only to discard it again.
			if s.cfg.Sync.FolderStructure == "none" {
				destination, skipped, err := destinations.resolve(
					s.cfg.Sync.CollisionPolicy, f.Path, s.localPath(f.Path))
				if skipped || err != nil {
					s.recordSkipOrFailure(f.Path, err)
					results <- downloadOutcome{file: f, skipped: skipped, err: err}
					return
				}
				_ = destination // re-resolved below, which is idempotent
			}

			fp := &fileProgress{
				total:    f.Size,
				interval: byteProgressInterval,
				now:      time.Now,
				record:   func(delta, copied, total int64) { s.recordBytes(f.Path, delta, copied, total) },
			}

			// Stage the download to a temp file so we can inspect it. Transient
			// failures are retried with backoff; each attempt restarts the file
			// from the beginning, so a partial attempt is not progress.
			var digest string
			tmpPath, err := s.downloadWithRetries(ctx, f.Path, fp, func() (string, error) {
				tmp, err := s.client.DownloadTempProgress(f.Path, s.cfg.LocalPath, fp.update)
				if err != nil {
					return "", err
				}
				if s.cfg.Sync.Verify != config.VerifySHA256 {
					return tmp, nil
				}

				// Verify while the bytes are still in the staging file, so a
				// corrupted transfer never reaches the destination and the retry
				// has nothing to clean up there.
				verified, err := s.verifyDownload(ctx, f.Path, tmp)
				if err != nil {
					os.Remove(tmp)
					return "", err
				}
				digest = verified
				return tmp, nil
			})
			if err != nil {
				s.recordFileResult(f.Path, err, fp.copied)
				results <- downloadOutcome{file: f, err: err}
				return
			}

			// Try to extract the capture date from EXIF metadata.
			captureDate, exifErr := exif.Date(tmpPath)
			desired := s.localPath(f.Path)
			if exifErr == nil {
				desired = s.datePath(f.Path, captureDate)
			}

			// Settle the destination before the final rename. Resolution is
			// idempotent for a file that already holds a claim.
			finalPath, skipped, err := destinations.resolve(
				s.cfg.Sync.CollisionPolicy, f.Path, desired)
			if skipped || err != nil {
				os.Remove(tmpPath)
				s.recordSkipOrFailure(f.Path, err)
				results <- downloadOutcome{file: f, skipped: skipped, err: err}
				return
			}

			// Ensure destination directory exists, then place the file.
			if err := os.MkdirAll(filepath.Dir(finalPath), 0755); err != nil {
				os.Remove(tmpPath)
				err = fmt.Errorf("mkdir: %w", err)
				s.recordFileResult(f.Path, err, fp.copied)
				results <- downloadOutcome{file: f, err: err}
				return
			}

			if err := os.Rename(tmpPath, finalPath); err != nil {
				os.Remove(tmpPath)
				err = fmt.Errorf("rename: %w", err)
				s.recordFileResult(f.Path, err, fp.copied)
				results <- downloadOutcome{file: f, err: err}
				return
			}

			// Set the file modification time to the capture date when available.
			mtime := f.MTime
			if exifErr == nil {
				mtime = captureDate
			}
			if err := os.Chtimes(finalPath, mtime, mtime); err != nil {
				log.Printf("warning: could not set file times for %s: %v", finalPath, err)
			}

			s.recordFileResult(f.Path, nil, fp.copied)
			results <- downloadOutcome{file: f, localPath: finalPath, digest: digest}
		}(f)
	}

	wg.Wait()
	close(results)

	collected := make([]downloadOutcome, 0, len(files))
	for r := range results {
		collected = append(collected, r)
	}

	result := s.applyDownloadOutcomes(collected, attempted, len(files))

	if err := s.manifest.Save(); err != nil {
		log.Printf("warning: could not save manifest: %v", err)
	}

	if err := result.failureError(); err != nil {
		return result, err
	}
	return result, incompleteBatchError(attempted, len(files), ctx.Err())
}

// applyDownloadOutcomes records each worker's outcome and updates the manifest.
// It runs after the workers finish, so the manifest has a single writer, and it
// is separate from downloadAll so the manifest and batch bookkeeping can be
// tested without a live SFTP server.
func (s *Syncer) applyDownloadOutcomes(outcomes []downloadOutcome, attempted, total int) batchResult {
	result := batchResult{Attempted: attempted, Total: total}

	for _, r := range outcomes {
		if r.err != nil {
			result.Failed++
			result.Failures = append(result.Failures, fileFailure{Path: r.file.Path, Err: r.err})
			log.Printf("download failed %s: %v", r.file.Path, r.err)
			continue
		}
		if r.skipped {
			result.Skipped++
			continue
		}

		result.Completed++
		log.Printf("synced: %s", r.file.Path)
		s.manifest.Set(r.file.Path, entryFor(r.file, r.localPath, r.digest))
	}

	return result
}

// batchResult is what one download batch produced.
//
// It exists so a partially failed batch can be reported with a bounded summary:
// one huge batch of failures must not turn into an unbounded status or API
// response, and the detail belongs in the log.
type batchResult struct {
	Attempted int
	Total     int
	Completed int
	Skipped   int
	Failed    int
	// Failures holds one entry per failed file, in worker-completion order.
	Failures []fileFailure
}

// fileFailure is one file's failure, kept for the aggregate summary.
type fileFailure struct {
	Path string
	Err  error
}

// downloadOutcome is what one worker produced for one file.
type downloadOutcome struct {
	file      sftpclient.RemoteFile
	localPath string
	// digest is the verified content hash, empty unless sha256 mode ran.
	digest  string
	skipped bool
	err     error
}

// failureError summarises a failed batch for the job status.
//
// The summary names the first failure and counts the rest, and each error is
// bounded, so the string cannot grow with the size of the batch.
func (r batchResult) failureError() error {
	if r.Failed == 0 {
		return nil
	}

	first := r.Failures[0]
	head := fmt.Sprintf("%d of %d file(s) failed", r.Failed, r.Attempted)
	if r.Failed > 1 {
		head += fmt.Sprintf(" (%d more in the daemon log)", r.Failed-1)
	}
	head += fmt.Sprintf("; first %s", first.Path)

	// A normal error is wrapped, so a caller can still inspect the cause. An
	// error long enough to matter is truncated instead, which necessarily drops
	// the chain: the text that would have to be preserved is what is being
	// bounded. Either way the returned string is bounded by the header plus
	// maxFailureDetail, whatever the batch size was.
	if len(first.Err.Error()) <= maxFailureDetail {
		return fmt.Errorf("%s: %w", head, first.Err)
	}
	return fmt.Errorf("%s: %s", head, truncateText(first.Err.Error(), maxFailureDetail))
}

// incompleteBatchError describes a batch that stopped before every file was
// attempted. A batch that attempted every file is never an error, even when
// cancellation arrives while the last worker finishes. With a live context the
// stop was caused by a pause, which is not a failure.
func incompleteBatchError(attempted, total int, ctxErr error) error {
	if attempted >= total {
		return nil
	}
	if ctxErr != nil {
		return fmt.Errorf("batch interrupted: %d of %d file(s) not attempted: %w", total-attempted, total, ctxErr)
	}
	return errBatchPaused
}

func (s *Syncer) localPath(remotePath string) string {
	rel := strings.TrimPrefix(remotePath, s.cfg.SFTP.RemotePath)
	rel = strings.TrimPrefix(rel, "/")
	return filepath.Join(s.cfg.LocalPath, filepath.FromSlash(rel))
}

// datePath returns the local destination path derived from a capture date,
// organised according to the configured folder_structure. The filename is
// preserved from the original remote path.
func (s *Syncer) datePath(remotePath string, date time.Time) string {
	name := filepath.Base(remotePath)
	switch s.cfg.Sync.FolderStructure {
	case "year":
		return filepath.Join(s.cfg.LocalPath, date.Format("2006"), name)
	case "year_month":
		return filepath.Join(s.cfg.LocalPath, date.Format("2006"), date.Format("01"), name)
	case "year_month_day":
		return filepath.Join(s.cfg.LocalPath, date.Format("2006"), date.Format("01"), date.Format("02"), name)
	default:
		return s.localPath(remotePath)
	}
}

func (s *Syncer) matchesFilter(path string) bool {
	if len(s.cfg.Sync.Extensions) == 0 {
		return true
	}
	ext := strings.ToLower(filepath.Ext(path))
	for _, allowed := range s.cfg.Sync.Extensions {
		if strings.ToLower(allowed) == ext {
			return true
		}
	}
	return false
}
