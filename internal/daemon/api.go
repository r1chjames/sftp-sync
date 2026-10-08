package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
)

// ServeAPI starts an HTTP server on the Unix socket and blocks until the
// daemon context is cancelled or the server encounters a fatal error.
func (d *Daemon) ServeAPI() error {
	socketPath := SocketPath()
	if err := os.MkdirAll(filepath.Dir(socketPath), 0755); err != nil {
		return fmt.Errorf("mkdir socket dir: %w", err)
	}
	// Remove any stale socket from a previous run.
	os.Remove(socketPath)

	ln, err := net.Listen("unix", socketPath)
	if err != nil {
		return fmt.Errorf("listen unix %s: %w", socketPath, err)
	}
	if err := os.Chmod(socketPath, 0600); err != nil {
		ln.Close()
		return fmt.Errorf("chmod socket: %w", err)
	}
	defer os.Remove(socketPath)

	srv := &http.Server{Handler: d.newMux()}
	go func() {
		<-d.ctx.Done()
		srv.Shutdown(context.Background())
	}()

	log.Printf("API listening on %s", socketPath)
	if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

// newMux builds the daemon's HTTP routes. It is separate from ServeAPI so
// tests can exercise the API without binding a Unix socket.
func (d *Daemon) newMux() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /jobs", d.handleListJobs)
	mux.HandleFunc("POST /jobs", d.handleAddJob)
	mux.HandleFunc("GET /jobs/{id}", d.handleGetJob)
	mux.HandleFunc("DELETE /jobs/{id}", d.handleRemoveJob)
	mux.HandleFunc("POST /jobs/{id}/sync", d.handleSyncJob)
	mux.HandleFunc("POST /jobs/{id}/pause", d.handlePauseJob)
	mux.HandleFunc("POST /jobs/{id}/resume", d.handleResumeJob)
	mux.HandleFunc("POST /shutdown", d.handleShutdown)
	return mux
}

func (d *Daemon) handleSyncJob(w http.ResponseWriter, r *http.Request) {
	d.jobControl(w, r, "sync")
}

func (d *Daemon) handlePauseJob(w http.ResponseWriter, r *http.Request) {
	d.jobControl(w, r, "pause")
}

func (d *Daemon) handleResumeJob(w http.ResponseWriter, r *http.Request) {
	d.jobControl(w, r, "resume")
}

// jobControl applies an asynchronous control action to the job named in the
// path and returns the job's status after the request was accepted. The action
// is applied only to the selected job; unknown jobs are reported as missing.
func (d *Daemon) jobControl(w http.ResponseWriter, r *http.Request, action string) {
	id := r.PathValue("id")
	job, ok := d.GetJob(id)
	if !ok {
		http.Error(w, fmt.Sprintf("job %s not found", id), http.StatusNotFound)
		return
	}

	prevPaused := job.syncer.IsPaused()

	switch action {
	case "sync":
		job.syncer.SyncNow()
	case "pause":
		job.syncer.Pause()
	case "resume":
		job.syncer.Resume()
	default:
		http.Error(w, "unsupported action: "+action, http.StatusInternalServerError)
		return
	}

	// A changed paused state must be durable, so a job comes back after a
	// restart in the state the user asked for. An idempotent call changes
	// nothing and therefore needs no write.
	if action != "sync" && job.syncer.IsPaused() != prevPaused {
		if err := d.saveRegistry(); err != nil {
			// Roll the runtime state back so it cannot disagree with the
			// registry, and report the failure instead of claiming success.
			if prevPaused {
				job.syncer.Pause()
			} else {
				job.syncer.Resume()
			}
			http.Error(w, fmt.Sprintf("persist %s for job %s: %v", action, id, err), http.StatusInternalServerError)
			return
		}
	}

	writeJSON(w, http.StatusAccepted, job.toResponse())
}

func (d *Daemon) handleListJobs(w http.ResponseWriter, r *http.Request) {
	jobs := d.ListJobs()
	resp := make([]JobResponse, 0, len(jobs))
	for _, j := range jobs {
		resp = append(resp, j.toResponse())
	}
	writeJSON(w, http.StatusOK, resp)
}

func (d *Daemon) handleAddJob(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ConfigPath string `json:"config_path"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON: "+err.Error(), http.StatusBadRequest)
		return
	}
	if req.ConfigPath == "" {
		http.Error(w, "config_path is required", http.StatusBadRequest)
		return
	}

	job, err := d.AddJob(req.ConfigPath)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusCreated, job.toResponse())
}

func (d *Daemon) handleGetJob(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	job, ok := d.GetJob(id)
	if !ok {
		http.Error(w, "job not found", http.StatusNotFound)
		return
	}
	writeJSON(w, http.StatusOK, job.toResponse())
}

func (d *Daemon) handleRemoveJob(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := d.RemoveJob(id); err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (d *Daemon) handleShutdown(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	go d.Shutdown()
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("writeJSON: %v", err)
	}
}
