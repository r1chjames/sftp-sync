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

	mux := http.NewServeMux()
	mux.HandleFunc("GET /jobs", d.handleListJobs)
	mux.HandleFunc("POST /jobs", d.handleAddJob)
	mux.HandleFunc("GET /jobs/{id}", d.handleGetJob)
	mux.HandleFunc("DELETE /jobs/{id}", d.handleRemoveJob)
	mux.HandleFunc("POST /shutdown", d.handleShutdown)

	srv := &http.Server{Handler: mux}
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
