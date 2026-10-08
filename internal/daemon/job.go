package daemon

import (
	"time"

	"github.com/r1chjames/sftp-sync/internal/syncer"
)

type jobSyncer interface {
	Status() syncer.SyncStatus
	Stop()
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
	LastSync           time.Time `json:"last_sync,omitempty"`
	LastSuccessfulSync time.Time `json:"last_successful_sync,omitempty"`
	FilesTotal         int       `json:"files_total"`
	Pending            int       `json:"pending"`
	EligibleFiles      int       `json:"eligible_files"`
	BatchTotal         int       `json:"batch_total"`
	Completed          int       `json:"completed"`
	Failed             int       `json:"failed"`
	Remaining          int       `json:"remaining"`
	CurrentFile        string    `json:"current_file,omitempty"`
	StartedAt          time.Time `json:"started_at,omitempty"`
	LastError          string    `json:"last_error,omitempty"`
}

func (j *Job) toResponse() JobResponse {
	st := j.syncer.Status()
	sr := StatusResponse{
		Phase:              string(st.Phase),
		LastSync:           st.LastSync,
		LastSuccessfulSync: st.LastSuccessfulSync,
		FilesTotal:         st.FilesTotal,
		Pending:            st.Pending,
		EligibleFiles:      st.EligibleFiles,
		BatchTotal:         st.BatchTotal,
		Completed:          st.Completed,
		Failed:             st.Failed,
		Remaining:          st.Remaining,
		CurrentFile:        st.CurrentFile,
		StartedAt:          st.StartedAt,
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
