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

			localPath := s.localPath(f.Path)
			err := s.client.Download(f.Path, localPath)
			results <- result{file: f, err: err}
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
