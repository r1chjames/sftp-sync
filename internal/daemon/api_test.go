package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/r1chjames/sftp-sync/internal/syncer"
)

func testJob(id string, addedAt time.Time, status syncer.SyncStatus) *Job {
	return &Job{
		ID:         id,
		ConfigPath: "/tmp/" + id + ".yaml",
		AddedAt:    addedAt,
		syncer:     stubJobSyncer{status: status},
	}
}

// newTestDaemon builds a daemon whose registry lives in a temp dir, so tests
// never touch the real data directory, Unix socket, SSH agent, or SFTP server.
func newTestDaemon(t *testing.T, jobs ...*Job) *Daemon {
	t.Helper()
	d := &Daemon{
		ctx:          context.Background(),
		cancel:       func() {},
		registryPath: filepath.Join(t.TempDir(), "registry.json"),
		jobs:         make(map[string]*Job, len(jobs)),
	}
	for _, j := range jobs {
		d.jobs[j.ID] = j
	}
	return d
}

func serve(t *testing.T, d *Daemon, method, target string, body string) *httptest.ResponseRecorder {
	t.Helper()
	var reader *strings.Reader
	if body == "" {
		reader = strings.NewReader("")
	} else {
		reader = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, target, reader)
	rec := httptest.NewRecorder()
	d.newMux().ServeHTTP(rec, req)
	return rec
}

func decodeList(t *testing.T, rec *httptest.ResponseRecorder) []JobResponse {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d (body %q)", rec.Code, http.StatusOK, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("content type = %q, want application/json", ct)
	}
	var jobs []JobResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &jobs); err != nil {
		t.Fatalf("decode list: %v (body %q)", err, rec.Body.String())
	}
	return jobs
}

func TestListJobsEmpty(t *testing.T) {
	rec := serve(t, newTestDaemon(t), http.MethodGet, "/jobs", "")
	jobs := decodeList(t, rec)
	if len(jobs) != 0 {
		t.Fatalf("jobs = %+v, want empty", jobs)
	}
	// An empty list must serialise as [] so clients can decode it uniformly.
	if got := strings.TrimSpace(rec.Body.String()); got != "[]" {
		t.Fatalf("body = %q, want []", got)
	}
}

func TestListJobsSingle(t *testing.T) {
	added := time.Date(2024, 6, 15, 12, 0, 0, 0, time.UTC)
	d := newTestDaemon(t, testJob("aaaaaaaa", added, syncer.SyncStatus{
		Phase:      syncer.PhaseDownloading,
		FilesTotal: 7,
		BatchTotal: 3,
		Completed:  1,
		Failed:     0,
		Remaining:  2,
	}))

	jobs := decodeList(t, serve(t, d, http.MethodGet, "/jobs", ""))
	if len(jobs) != 1 {
		t.Fatalf("jobs = %+v, want 1", jobs)
	}
	if jobs[0].ID != "aaaaaaaa" || jobs[0].ConfigPath != "/tmp/aaaaaaaa.yaml" || !jobs[0].AddedAt.Equal(added) {
		t.Fatalf("job identity not serialised: %+v", jobs[0])
	}
}

func TestListJobsOrderIsStableAcrossRefreshes(t *testing.T) {
	base := time.Date(2024, 6, 15, 12, 0, 0, 0, time.UTC)
	// Two jobs share a timestamp so the ID tie-break is exercised, and the map
	// is populated in an order that differs from the expected output.
	d := newTestDaemon(t,
		testJob("cccccccc", base.Add(time.Minute), syncer.SyncStatus{}),
		testJob("bbbbbbbb", base, syncer.SyncStatus{}),
		testJob("aaaaaaaa", base, syncer.SyncStatus{}),
	)

	want := []string{"aaaaaaaa", "bbbbbbbb", "cccccccc"}
	for refresh := 0; refresh < 10; refresh++ {
		jobs := decodeList(t, serve(t, d, http.MethodGet, "/jobs", ""))
		if len(jobs) != len(want) {
			t.Fatalf("refresh %d: jobs = %d, want %d", refresh, len(jobs), len(want))
		}
		for i, id := range want {
			if jobs[i].ID != id {
				t.Fatalf("refresh %d: order = %v, want %v", refresh, ids(jobs), want)
			}
		}
	}
}

func ids(jobs []JobResponse) []string {
	out := make([]string, 0, len(jobs))
	for _, j := range jobs {
		out = append(out, j.ID)
	}
	return out
}

func TestGetJob(t *testing.T) {
	added := time.Date(2024, 6, 15, 12, 0, 0, 0, time.UTC)
	d := newTestDaemon(t, testJob("abc12345", added, syncer.SyncStatus{
		Phase:      syncer.PhaseError,
		FilesTotal: 4,
		LastError:  errors.New("copy failed"),
	}))

	rec := serve(t, d, http.MethodGet, "/jobs/abc12345", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	var job JobResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &job); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if job.ID != "abc12345" || job.Status.Phase != "error" || job.Status.LastError != "copy failed" {
		t.Fatalf("job = %+v", job)
	}
}

func TestGetJobNotFound(t *testing.T) {
	rec := serve(t, newTestDaemon(t), http.MethodGet, "/jobs/missing1", "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusNotFound)
	}
}

func TestJobResponseJSONContract(t *testing.T) {
	started := time.Date(2024, 6, 15, 12, 0, 5, 0, time.UTC)
	lastSuccess := time.Date(2024, 6, 15, 11, 0, 5, 0, time.UTC)
	d := newTestDaemon(t, testJob("abc12345", started, syncer.SyncStatus{
		Phase:                     syncer.PhaseDownloading,
		LastSync:                  started,
		LastSuccessfulSync:        lastSuccess,
		FilesTotal:                9,
		EligibleFiles:             9,
		Pending:                   2,
		BatchTotal:                3,
		Completed:                 1,
		Failed:                    1,
		Remaining:                 1,
		BytesTotal:                2048,
		BytesCompleted:            1024,
		CurrentFile:               "/photos/IMG_0001.CR3",
		CurrentFileBytesTotal:     700,
		CurrentFileBytesCompleted: 350,
		StartedAt:                 started,
		LastError:                 errors.New("copy failed"),
	}))

	var raw map[string]any
	rec := serve(t, d, http.MethodGet, "/jobs/abc12345", "")
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode: %v", err)
	}

	status, ok := raw["status"].(map[string]any)
	if !ok {
		t.Fatalf("status missing from payload: %v", raw)
	}
	for _, key := range []string{
		"phase", "last_sync", "last_successful_sync", "files_total", "pending",
		"eligible_files", "batch_total", "completed", "failed", "remaining",
		"bytes_total", "bytes_completed", "current_file",
		"current_file_bytes_total", "current_file_bytes_completed", "started_at",
		"last_error",
	} {
		if _, ok := status[key]; !ok {
			t.Fatalf("status key %q missing from payload: %v", key, status)
		}
	}
	if status["phase"] != "downloading" {
		t.Fatalf("phase = %v, want downloading", status["phase"])
	}
	if status["batch_total"].(float64) != 3 || status["completed"].(float64) != 1 || status["failed"].(float64) != 1 {
		t.Fatalf("counters = %v", status)
	}
	if status["bytes_total"].(float64) != 2048 || status["bytes_completed"].(float64) != 1024 {
		t.Fatalf("bytes = %v", status)
	}
	if status["last_error"] != "copy failed" {
		t.Fatalf("last_error = %v, want copy failed", status["last_error"])
	}
	if !strings.HasPrefix(status["last_sync"].(string), "2024-06-15T12:00:05") {
		t.Fatalf("last_sync = %v", status["last_sync"])
	}
	if !strings.HasPrefix(status["last_successful_sync"].(string), "2024-06-15T11:00:05") {
		t.Fatalf("last_successful_sync = %v", status["last_successful_sync"])
	}
}

func TestJobResponseOmitsEmptyOptionalFields(t *testing.T) {
	d := newTestDaemon(t, testJob("abc12345", time.Now(), syncer.SyncStatus{Phase: syncer.PhaseIdle}))

	rec := serve(t, d, http.MethodGet, "/jobs/abc12345", "")
	var raw map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode: %v", err)
	}

	status := raw["status"].(map[string]any)
	for _, key := range []string{"last_error", "current_file"} {
		if _, ok := status[key]; ok {
			t.Fatalf("empty %q should be omitted: %v", key, status)
		}
	}
	if status["phase"] != "idle" {
		t.Fatalf("phase = %v, want idle", status["phase"])
	}
}

func TestRemoveJobEndpoint(t *testing.T) {
	d := newTestDaemon(t, testJob("abc12345", time.Now(), syncer.SyncStatus{}))

	rec := serve(t, d, http.MethodDelete, "/jobs/abc12345", "")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusNoContent)
	}
	if _, ok := d.GetJob("abc12345"); ok {
		t.Fatal("job still present after delete")
	}
	if jobs := decodeList(t, serve(t, d, http.MethodGet, "/jobs", "")); len(jobs) != 0 {
		t.Fatalf("jobs = %+v, want empty after delete", jobs)
	}

	rec = serve(t, d, http.MethodDelete, "/jobs/abc12345", "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("second delete status = %d, want %d", rec.Code, http.StatusNotFound)
	}
}

func TestAddJobRejectsInvalidRequests(t *testing.T) {
	d := newTestDaemon(t)

	tests := []struct {
		name string
		body string
	}{
		{name: "malformed json", body: "{"},
		{name: "missing config path", body: "{}"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := serve(t, d, http.MethodPost, "/jobs", tt.body)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want %d (body %q)", rec.Code, http.StatusBadRequest, rec.Body.String())
			}
		})
	}
}

func TestShutdownEndpointDoesNotBlock(t *testing.T) {
	d := newTestDaemon(t)
	rec := serve(t, d, http.MethodPost, "/shutdown", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
}
