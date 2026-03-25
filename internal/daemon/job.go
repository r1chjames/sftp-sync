package daemon

import (
	"time"

	"github.com/r1chjames/sftp-sync/internal/syncer"
)

// Job represents a managed sync job.
type Job struct {
	ID         string    `json:"id"`
	ConfigPath string    `json:"config_path"`
	AddedAt    time.Time `json:"added_at"`
	syncer     *syncer.Syncer
}

// JobResponse is the JSON-serialisable representation of a Job including live status.
type JobResponse struct {
	ID         string         `json:"id"`
	ConfigPath string         `json:"config_path"`
	AddedAt    time.Time      `json:"added_at"`
	Status     StatusResponse `json:"status"`
}

// StatusResponse mirrors syncer.SyncStatus for JSON serialisation.
type StatusResponse struct {
	LastSync   time.Time `json:"last_sync,omitempty"`
	FilesTotal int       `json:"files_total"`
	Pending    int       `json:"pending"`
	LastError  string    `json:"last_error,omitempty"`
}

func (j *Job) toResponse() JobResponse {
	st := j.syncer.Status()
	sr := StatusResponse{
		LastSync:   st.LastSync,
		FilesTotal: st.FilesTotal,
		Pending:    st.Pending,
	}
	if st.LastError != nil {
		sr.LastError = st.LastError.Error()
	}
	return JobResponse{
		ID:         j.ID,
		ConfigPath: j.ConfigPath,
		AddedAt:    j.AddedAt,
		Status:     sr,
	}
}
