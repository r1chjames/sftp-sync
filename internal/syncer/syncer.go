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

// SyncStatus is a snapshot of the syncer's current state.
type SyncStatus struct {
	LastSync   time.Time
	FilesTotal int
	Pending    int
	LastError  error
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
	defer s.mu.RUnlock()
	return s.status
}

func (s *Syncer) run(ctx context.Context) {
	defer close(s.done)

	// Run immediately on startup, then on each interval tick.
	for {
		if err := s.sync(ctx); err != nil {
			log.Printf("sync error: %v", err)
			s.mu.Lock()
			s.status.LastError = err
			s.mu.Unlock()
		}

		select {
		case <-ctx.Done():
			s.client.Close()
			return
		case <-time.After(s.cfg.Sync.Interval):
		}
	}
}

func (s *Syncer) sync(ctx context.Context) error {
	if err := s.client.EnsureConnected(); err != nil {
		return fmt.Errorf("connect: %w", err)
	}

	remoteFiles, err := s.client.Walk(s.cfg.SFTP.RemotePath)
	if err != nil {
		s.client.Close() // force reconnect on next poll
		return fmt.Errorf("walk %s: %w", s.cfg.SFTP.RemotePath, err)
	}

	var toDownload []sftpclient.RemoteFile
	for _, f := range remoteFiles {
		if !s.matchesFilter(f.Path) {
			continue
		}
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

	if err := s.manifest.Save(); err != nil {
		log.Printf("warning: could not save manifest after adoption: %v", err)
	}

	if len(toDownload) > 0 {
		log.Printf("downloading %d new/changed file(s) (of %d total)", len(toDownload), len(remoteFiles))
		s.downloadAll(ctx, toDownload)
	} else {
		log.Printf("up to date — %d remote file(s)", len(remoteFiles))
	}

	s.mu.Lock()
	s.status.LastSync = time.Now()
	s.status.FilesTotal = len(remoteFiles)
	s.status.Pending = 0
	s.status.LastError = nil
	s.mu.Unlock()

	return nil
}

func (s *Syncer) downloadAll(ctx context.Context, files []sftpclient.RemoteFile) {
	type result struct {
		file sftpclient.RemoteFile
		err  error
	}

	results := make(chan result, len(files))
	sem := make(chan struct{}, s.cfg.Sync.Workers)
	var wg sync.WaitGroup

	for _, f := range files {
		if ctx.Err() != nil {
			break
		}

		wg.Add(1)
		sem <- struct{}{}
		go func(f sftpclient.RemoteFile) {
			defer wg.Done()
			defer func() { <-sem }()

			// Stage the download to a temp file so we can inspect it.
			tmpPath, err := s.client.DownloadTemp(f.Path, s.cfg.LocalPath)
			if err != nil {
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
				results <- result{file: f, err: fmt.Errorf("mkdir: %w", err)}
				return
			}

			if err := os.Rename(tmpPath, finalPath); err != nil {
				os.Remove(tmpPath)
				results <- result{file: f, err: fmt.Errorf("rename: %w", err)}
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

			results <- result{file: f, err: nil}
		}(f)
	}

	wg.Wait()
	close(results)

	// Update manifest serially after all downloads complete.
	for r := range results {
		if r.err != nil {
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
