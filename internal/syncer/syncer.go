package syncer

import (
	"context"
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

// byteProgressInterval throttles byte-progress status updates so the status
// mutex is not taken for every 32 KiB write of every worker.
const byteProgressInterval = 200 * time.Millisecond

// SyncStatus is a snapshot of the syncer's current state.
type SyncStatus struct {
	Phase                     SyncPhase
	LastSync                  time.Time
	LastSuccessfulSync        time.Time
	FilesTotal                int
	Pending                   int
	EligibleFiles             int
	BatchTotal                int
	Completed                 int
	Failed                    int
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
	cancel context.CancelFunc
	done   chan struct{}
}

func New(cfg *config.Config) *Syncer {
	return &Syncer{
		cfg:    cfg,
		client: sftpclient.New(cfg),
		status: SyncStatus{Phase: PhaseIdle},
		done:   make(chan struct{}),
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
	case ctx.Err() != nil:
		log.Printf("sync interrupted: %v", err)
	default:
		log.Printf("sync error: %v", err)
		s.markFailure(err)
	}
}

func (s *Syncer) run(ctx context.Context) {
	defer close(s.done)

	// Run immediately on startup, then on each interval tick.
	for {
		s.recordCycle(ctx, s.sync(ctx))

		select {
		case <-ctx.Done():
			s.client.Close()
			return
		case <-time.After(s.cfg.Sync.Interval):
		}
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

	eligible, toDownload := s.selectForDownload(remoteFiles)
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
		if err := s.downloadAll(ctx, toDownload); err != nil {
			return err
		}
	} else {
		log.Printf("up to date — %d eligible remote file(s)", eligible)
	}

	return nil
}

func (s *Syncer) selectForDownload(remoteFiles []sftpclient.RemoteFile) (int, []sftpclient.RemoteFile) {
	var toDownload []sftpclient.RemoteFile
	eligible := 0
	for _, f := range remoteFiles {
		if !s.matchesFilter(f.Path) {
			continue
		}
		eligible++
		entry, ok := s.manifest.Get(f.Path)
		if !ok || !entry.MTime.Equal(f.MTime) || entry.Size != f.Size {
			// For files not yet in the manifest, adopt them if they already
			// exist locally rather than re-downloading.
			if !ok {
				if _, err := os.Stat(s.localPath(f.Path)); err == nil {
					log.Printf("adopting existing local file: %s", f.Path)
					s.manifest.Set(f.Path, state.Entry{MTime: f.MTime, Size: f.Size})
					continue
				}
			}
			toDownload = append(toDownload, f)
		}
	}
	return eligible, toDownload
}

func (s *Syncer) downloadAll(ctx context.Context, files []sftpclient.RemoteFile) error {
	type result struct {
		file sftpclient.RemoteFile
		err  error
	}

	results := make(chan result, len(files))
	sem := make(chan struct{}, s.cfg.Sync.Workers)
	var wg sync.WaitGroup

	attempted := 0
	for _, f := range files {
		if ctx.Err() != nil {
			break
		}

		attempted++
		wg.Add(1)
		sem <- struct{}{}
		go func(f sftpclient.RemoteFile) {
			defer wg.Done()
			defer func() { <-sem }()

			s.setCurrentFile(f.Path)

			fp := &fileProgress{
				total:    f.Size,
				interval: byteProgressInterval,
				now:      time.Now,
				record:   func(delta, copied, total int64) { s.recordBytes(f.Path, delta, copied, total) },
			}

			// Stage the download to a temp file so we can inspect it.
			tmpPath, err := s.client.DownloadTempProgress(f.Path, s.cfg.LocalPath, fp.update)

			// Flush bytes withheld by throttling before recording any outcome,
			// so a failure subtracts exactly what this file contributed.
			fp.flush()
			if err != nil {
				s.recordFileResult(f.Path, err, fp.copied)
				results <- result{file: f, err: err}
				return
			}

			// Try to extract the capture date from EXIF metadata.
			captureDate, exifErr := exif.Date(tmpPath)
			var finalPath string
			if exifErr == nil {
				finalPath = s.datePath(f.Path, captureDate)
			} else {
				finalPath = s.localPath(f.Path)
			}

			// Ensure destination directory exists, then place the file.
			if err := os.MkdirAll(filepath.Dir(finalPath), 0755); err != nil {
				os.Remove(tmpPath)
				err = fmt.Errorf("mkdir: %w", err)
				s.recordFileResult(f.Path, err, fp.copied)
				results <- result{file: f, err: err}
				return
			}

			if err := os.Rename(tmpPath, finalPath); err != nil {
				os.Remove(tmpPath)
				err = fmt.Errorf("rename: %w", err)
				s.recordFileResult(f.Path, err, fp.copied)
				results <- result{file: f, err: err}
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
			results <- result{file: f, err: nil}
		}(f)
	}

	wg.Wait()
	close(results)

	failed := 0
	var firstFailure result

	// Update manifest serially after all downloads complete.
	for r := range results {
		if r.err != nil {
			failed++
			if failed == 1 {
				firstFailure = r
			}
			log.Printf("download failed %s: %v", r.file.Path, r.err)
			continue
		}
		log.Printf("synced: %s", r.file.Path)
		s.manifest.Set(r.file.Path, state.Entry{
			MTime: r.file.MTime,
			Size:  r.file.Size,
		})
	}

	if err := s.manifest.Save(); err != nil {
		log.Printf("warning: could not save manifest: %v", err)
	}

	if err := downloadBatchError(failed, len(files), firstFailure.file.Path, firstFailure.err); err != nil {
		return err
	}
	return batchInterruptedError(attempted, len(files), ctx.Err())
}

// batchInterruptedError reports a batch that could not be attempted in full
// because the context was cancelled. A batch that attempted every file is not
// an error even when cancellation arrives while the last worker finishes.
func batchInterruptedError(attempted, total int, ctxErr error) error {
	if ctxErr == nil || attempted >= total {
		return nil
	}
	return fmt.Errorf("batch interrupted: %d of %d file(s) not attempted: %w", total-attempted, total, ctxErr)
}

func downloadBatchError(failed, total int, firstPath string, firstErr error) error {
	if failed == 0 {
		return nil
	}
	return fmt.Errorf("%d of %d file(s) failed; first failure %s: %w", failed, total, firstPath, firstErr)
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
