package apiclient

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// newTestClient points a client at an httptest server instead of the real Unix
// socket, so no daemon, socket, or home directory is required.
func newTestClient(t *testing.T, h http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return &Client{http: srv.Client(), baseURL: srv.URL}
}

func TestListJobsSuccess(t *testing.T) {
	body := `[{"id":"aaaaaaaa","config_path":"/tmp/a.yaml","status":{"phase":"idle","files_total":2,"bytes_total":10,"bytes_completed":5}}]`
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/jobs" || r.Method != http.MethodGet {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(body))
	})

	jobs, err := c.ListJobs()
	if err != nil {
		t.Fatalf("ListJobs: %v", err)
	}
	if len(jobs) != 1 || jobs[0].ID != "aaaaaaaa" {
		t.Fatalf("jobs = %+v", jobs)
	}
	if jobs[0].Status.Phase != "idle" || jobs[0].Status.BytesTotal != 10 || jobs[0].Status.BytesCompleted != 5 {
		t.Fatalf("status = %+v", jobs[0].Status)
	}
}

func TestListJobsReturnsDaemonErrorOnNon2xx(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "registry unavailable", http.StatusInternalServerError)
	})

	_, err := c.ListJobs()
	if err == nil {
		t.Fatal("ListJobs returned nil error for a 500 response")
	}
	if !strings.Contains(err.Error(), "500") || !strings.Contains(err.Error(), "registry unavailable") {
		t.Fatalf("error = %v, want status code and daemon message", err)
	}
}

func TestListJobsRejectsMalformedJSON(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"not":"an array"`))
	})

	_, err := c.ListJobs()
	if err == nil {
		t.Fatal("ListJobs returned nil error for malformed JSON")
	}
	if !strings.Contains(err.Error(), "decode") {
		t.Fatalf("error = %v, want a decode error", err)
	}
}

func TestAddJobValidatesStatus(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "load config: no such file", http.StatusInternalServerError)
	})

	_, err := c.AddJob("/tmp/missing.yaml")
	if err == nil {
		t.Fatal("AddJob returned nil error for a 500 response")
	}
	if !strings.Contains(err.Error(), "500") || !strings.Contains(err.Error(), "no such file") {
		t.Fatalf("error = %v, want status code and daemon message", err)
	}
}

func TestRemoveJobSuccess(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})

	if err := c.RemoveJob("abc12345"); err != nil {
		t.Fatalf("RemoveJob: %v", err)
	}
}

func TestRemoveJobNotFound(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "job not found", http.StatusNotFound)
	})

	err := c.RemoveJob("abc12345")
	if err == nil {
		t.Fatal("RemoveJob returned nil error for a 404 response")
	}
	if !strings.Contains(err.Error(), "not found") || !strings.Contains(err.Error(), "abc12345") {
		t.Fatalf("error = %v, want job id and not found", err)
	}
}

func TestRemoveJobServerError(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "save registry: permission denied", http.StatusInternalServerError)
	})

	err := c.RemoveJob("abc12345")
	if err == nil {
		t.Fatal("RemoveJob returned nil error for a 500 response")
	}
	if !strings.Contains(err.Error(), "500") || !strings.Contains(err.Error(), "permission denied") {
		t.Fatalf("error = %v, want status code and daemon message", err)
	}
}

func TestShutdownValidatesStatus(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	if err := c.Shutdown(); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}

	failing := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "already shutting down", http.StatusServiceUnavailable)
	})
	if err := failing.Shutdown(); err == nil {
		t.Fatal("Shutdown returned nil error for a 503 response")
	} else if !strings.Contains(err.Error(), "503") {
		t.Fatalf("error = %v, want status code", err)
	}
}

func TestPingValidatesStatus(t *testing.T) {
	healthy := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("[]"))
	})
	if err := healthy.Ping(); err != nil {
		t.Fatalf("Ping: %v", err)
	}

	unhealthy := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "not ready", http.StatusInternalServerError)
	})
	if err := unhealthy.Ping(); err == nil {
		t.Fatal("Ping returned nil error for a 500 response")
	}
}

func TestResponseErrorFallsBackToStatusText(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	})

	_, err := c.ListJobs()
	if err == nil {
		t.Fatal("ListJobs returned nil error for a 403 response")
	}
	if !strings.Contains(err.Error(), "403") || !strings.Contains(err.Error(), "Forbidden") {
		t.Fatalf("error = %v, want status code and status text", err)
	}
}

func TestTransportErrorIsReturned(t *testing.T) {
	c := &Client{http: &http.Client{}, baseURL: "http://127.0.0.1:1"}

	if _, err := c.ListJobs(); err == nil {
		t.Fatal("ListJobs returned nil error for an unreachable daemon")
	}
}
