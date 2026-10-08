package daemon

import (
	"time"

	"github.com/r1chjames/sftp-sync/internal/syncer"
)

// jobSyncer is the part of a syncer a managed job needs. It is an interface so
// the daemon, its HTTP handlers, and its tests can run without SFTP.
type jobSyncer interface {
	Status() syncer.SyncStatus
	Stop()
	SyncNow()
	Pause()
	Resume()
	IsPaused() bool
}

// Job represents a managed sync job.
type Job struct {
	ID         string    `json:"id"`
	ConfigPath string    `json:"config_path"`
	AddedAt    time.Time `json:"added_at"`
	syncer     jobSyncer
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
	Phase              string    `json:"phase"`
	Paused             bool      `json:"paused"`
	LastSync           time.Time `json:"last_sync,omitempty"`
	LastSuccessfulSync time.Time `json:"last_successful_sync,omitempty"`
	FilesTotal         int       `json:"files_total"`
	Pending            int       `json:"pending"`
	EligibleFiles      int       `json:"eligible_files"`
	BatchTotal         int       `json:"batch_total"`
	Completed          int       `json:"completed"`
	Failed             int       `json:"failed"`
	Remaining          int       `json:"remaining"`
	BytesTotal         int64     `json:"bytes_total"`
	BytesCompleted     int64     `json:"bytes_completed"`

	CurrentFile               string    `json:"current_file,omitempty"`
	CurrentFileBytesTotal     int64     `json:"current_file_bytes_total"`
	CurrentFileBytesCompleted int64     `json:"current_file_bytes_completed"`
	StartedAt                 time.Time `json:"started_at,omitempty"`
	LastError                 string    `json:"last_error,omitempty"`
}

func (j *Job) toResponse() JobResponse {
	st := j.syncer.Status()
	sr := StatusResponse{
		Phase:              string(st.Phase),
		Paused:             st.Paused,
		LastSync:           st.LastSync,
		LastSuccessfulSync: st.LastSuccessfulSync,
		FilesTotal:         st.FilesTotal,
		Pending:            st.Pending,
		EligibleFiles:      st.EligibleFiles,
		BatchTotal:         st.BatchTotal,
		Completed:          st.Completed,
		Failed:             st.Failed,
		Remaining:          st.Remaining,
		BytesTotal:         st.BytesTotal,
		BytesCompleted:     st.BytesCompleted,

		CurrentFile:               st.CurrentFile,
		CurrentFileBytesTotal:     st.CurrentFileBytesTotal,
		CurrentFileBytesCompleted: st.CurrentFileBytesCompleted,
		StartedAt:                 st.StartedAt,
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
