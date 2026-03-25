package daemon

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/r1chjames/sftp-sync/internal/config"
	"github.com/r1chjames/sftp-sync/internal/syncer"
)

const dataDir = "~/.local/share/sftpsync"

// SocketPath returns the path to the daemon's Unix socket.
func SocketPath() string {
	return config.ExpandHome(dataDir + "/daemon.sock")
}

// Daemon manages a collection of independent sync jobs.
type Daemon struct {
	registryPath string
	ctx          context.Context
	cancel       context.CancelFunc
	mu           sync.RWMutex
	jobs         map[string]*Job
}

// New creates a Daemon. Call Start to restore persisted jobs, then ServeAPI to
// accept control connections.
func New() *Daemon {
	ctx, cancel := context.WithCancel(context.Background())
	return &Daemon{
		registryPath: config.ExpandHome(dataDir + "/registry.json"),
		ctx:          ctx,
		cancel:       cancel,
		jobs:         make(map[string]*Job),
	}
}

// Start restores jobs from the persisted registry and starts their syncers.
func (d *Daemon) Start() error {
	return d.loadRegistry()
}

// Shutdown stops all running syncers and cancels the daemon context.
func (d *Daemon) Shutdown() {
	d.mu.RLock()
	jobs := make([]*Job, 0, len(d.jobs))
	for _, j := range d.jobs {
		jobs = append(jobs, j)
	}
	d.mu.RUnlock()

	for _, j := range jobs {
		j.syncer.Stop()
	}
	d.cancel()
}

// AddJob loads the config at configPath, starts a new Syncer, and persists
// the job to the registry.
func (d *Daemon) AddJob(configPath string) (*Job, error) {
	cfg, err := config.Load(configPath)
	if err != nil {
		return nil, fmt.Errorf("load config: %w", err)
	}

	id := newID()
	// Override state path so each job has its own manifest.
	cfg.StatePath = config.ExpandHome(dataDir + "/jobs/" + id + ".json")

	s := syncer.New(cfg)
	if err := s.Start(d.ctx); err != nil {
		return nil, fmt.Errorf("start syncer: %w", err)
	}

	job := &Job{
		ID:         id,
		ConfigPath: configPath,
		AddedAt:    time.Now(),
		syncer:     s,
	}

	d.mu.Lock()
	d.jobs[id] = job
	d.mu.Unlock()

	return job, d.saveRegistry()
}

// RemoveJob stops and removes the job with the given ID.
func (d *Daemon) RemoveJob(id string) error {
	d.mu.Lock()
	job, ok := d.jobs[id]
	if !ok {
		d.mu.Unlock()
		return fmt.Errorf("job %s not found", id)
	}
	delete(d.jobs, id)
	d.mu.Unlock()

	go job.syncer.Stop()
	return d.saveRegistry()
}

// ListJobs returns all current jobs.
func (d *Daemon) ListJobs() []*Job {
	d.mu.RLock()
	defer d.mu.RUnlock()
	jobs := make([]*Job, 0, len(d.jobs))
	for _, j := range d.jobs {
		jobs = append(jobs, j)
	}
	return jobs
}

// GetJob returns the job with the given ID.
func (d *Daemon) GetJob(id string) (*Job, bool) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	j, ok := d.jobs[id]
	return j, ok
}

type registryFile struct {
	Jobs []registryEntry `json:"jobs"`
}

type registryEntry struct {
	ID         string    `json:"id"`
	ConfigPath string    `json:"config_path"`
	AddedAt    time.Time `json:"added_at"`
}

func (d *Daemon) loadRegistry() error {
	data, err := os.ReadFile(d.registryPath)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read registry: %w", err)
	}

	var reg registryFile
	if err := json.Unmarshal(data, &reg); err != nil {
		return fmt.Errorf("parse registry: %w", err)
	}

	for _, entry := range reg.Jobs {
		cfg, err := config.Load(entry.ConfigPath)
		if err != nil {
			log.Printf("skipping job %s (%s): %v", entry.ID, entry.ConfigPath, err)
			continue
		}
		cfg.StatePath = config.ExpandHome(dataDir + "/jobs/" + entry.ID + ".json")

		s := syncer.New(cfg)
		if err := s.Start(d.ctx); err != nil {
			log.Printf("skipping job %s: start: %v", entry.ID, err)
			continue
		}

		d.jobs[entry.ID] = &Job{
			ID:         entry.ID,
			ConfigPath: entry.ConfigPath,
			AddedAt:    entry.AddedAt,
			syncer:     s,
		}
		log.Printf("restored job %s (%s)", entry.ID, entry.ConfigPath)
	}
	return nil
}

func (d *Daemon) saveRegistry() error {
	d.mu.RLock()
	var reg registryFile
	for _, j := range d.jobs {
		reg.Jobs = append(reg.Jobs, registryEntry{
			ID:         j.ID,
			ConfigPath: j.ConfigPath,
			AddedAt:    j.AddedAt,
		})
	}
	d.mu.RUnlock()

	if err := os.MkdirAll(filepath.Dir(d.registryPath), 0755); err != nil {
		return fmt.Errorf("mkdir: %w", err)
	}

	data, err := json.MarshalIndent(reg, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}

	tmp := d.registryPath + ".tmp"
	if err := os.WriteFile(tmp, data, 0644); err != nil {
		return fmt.Errorf("write: %w", err)
	}
	return os.Rename(tmp, d.registryPath)
}

func newID() string {
	b := make([]byte, 4)
	rand.Read(b)
	return hex.EncodeToString(b)
}
