package main

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/r1chjames/sftp-sync/internal/apiclient"
	"github.com/r1chjames/sftp-sync/internal/daemon"
)

func TestFormatBatch(t *testing.T) {
	tests := []struct {
		name   string
		status daemon.StatusResponse
		want   string
	}{
		{name: "no batch", status: daemon.StatusResponse{}, want: "-"},
		{
			name: "active batch",
			status: daemon.StatusResponse{
				BatchTotal: 5,
				Completed:  2,
				Failed:     1,
				Remaining:  2,
			},
			want: "2/5 complete, 1 failed, 2 remaining",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := formatBatch(tt.status); got != tt.want {
				t.Fatalf("formatBatch() = %q, want %q", got, tt.want)
			}
		})
	}
}

// fakeClient is a jobClient that records calls and returns canned results, so
// command dispatch can be tested without a daemon.
type fakeClient struct {
	jobs   []daemon.JobResponse
	err    error
	calls  []string
	lastID string
}

func (f *fakeClient) record(call, id string) {
	f.calls = append(f.calls, call)
	f.lastID = id
}

func (f *fakeClient) AddJob(configPath string) (daemon.JobResponse, error) {
	f.record("add", configPath)
	if f.err != nil {
		return daemon.JobResponse{}, f.err
	}
	return daemon.JobResponse{ID: "abc12345", ConfigPath: configPath}, nil
}

func (f *fakeClient) ListJobs() ([]daemon.JobResponse, error) {
	f.record("list", "")
	if f.err != nil {
		return nil, f.err
	}
	return f.jobs, nil
}

func (f *fakeClient) RemoveJob(id string) error {
	f.record("remove", id)
	return f.err
}

func (f *fakeClient) SyncJob(id string) (daemon.JobResponse, error) {
	f.record("sync", id)
	if f.err != nil {
		return daemon.JobResponse{}, f.err
	}
	return daemon.JobResponse{ID: id, Status: daemon.StatusResponse{Phase: "scanning"}}, nil
}

func (f *fakeClient) PauseJob(id string) (daemon.JobResponse, error) {
	f.record("pause", id)
	if f.err != nil {
		return daemon.JobResponse{}, f.err
	}
	return daemon.JobResponse{ID: id, Status: daemon.StatusResponse{Phase: "paused", Paused: true}}, nil
}

func (f *fakeClient) ResumeJob(id string) (daemon.JobResponse, error) {
	f.record("resume", id)
	if f.err != nil {
		return daemon.JobResponse{}, f.err
	}
	return daemon.JobResponse{ID: id, Status: daemon.StatusResponse{Phase: "scanning"}}, nil
}

func (f *fakeClient) Shutdown() error {
	f.record("stop", "")
	return f.err
}

func runCLI(t *testing.T, c jobClient, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code = run(args, &out, &errOut, c)
	return code, out.String(), errOut.String()
}

func TestRunRejectsMissingArguments(t *testing.T) {
	tests := []struct {
		command string
		usage   string
	}{
		{"add", "usage: sftpsync add <config-file>"},
		{"remove", "usage: sftpsync remove <id>"},
		{"rm", "usage: sftpsync remove <id>"},
		{"sync", "usage: sftpsync sync <id>"},
		{"pause", "usage: sftpsync pause <id>"},
		{"resume", "usage: sftpsync resume <id>"},
	}

	for _, tt := range tests {
		t.Run(tt.command, func(t *testing.T) {
			c := &fakeClient{}
			code, stdout, stderr := runCLI(t, c, tt.command)

			if code != exitUsage {
				t.Fatalf("exit code = %d, want %d", code, exitUsage)
			}
			if !strings.Contains(stderr, tt.usage) {
				t.Fatalf("stderr = %q, want %q", stderr, tt.usage)
			}
			if stdout != "" {
				t.Fatalf("stdout = %q, want no output", stdout)
			}
			if len(c.calls) != 0 {
				t.Fatalf("client was called with %v", c.calls)
			}
		})
	}
}

func TestRunRejectsUnknownCommandAndNoArguments(t *testing.T) {
	c := &fakeClient{}

	code, _, stderr := runCLI(t, c, "frobnicate")
	if code != exitUsage {
		t.Fatalf("exit code = %d, want %d", code, exitUsage)
	}
	if !strings.Contains(stderr, "unknown command: frobnicate") || !strings.Contains(stderr, "usage: sftpsync") {
		t.Fatalf("stderr = %q", stderr)
	}

	code, _, stderr = runCLI(t, c)
	if code != exitUsage {
		t.Fatalf("exit code with no arguments = %d, want %d", code, exitUsage)
	}
	if !strings.Contains(stderr, "usage: sftpsync") {
		t.Fatalf("stderr = %q", stderr)
	}
}

func TestRunControlCommands(t *testing.T) {
	for _, command := range []string{"sync", "pause", "resume"} {
		t.Run(command, func(t *testing.T) {
			c := &fakeClient{}
			code, stdout, stderr := runCLI(t, c, command, "abc12345")

			if code != exitOK {
				t.Fatalf("exit code = %d, want %d (stderr %q)", code, exitOK, stderr)
			}
			want := command + " requested for job abc12345\n"
			if stdout != want {
				t.Fatalf("stdout = %q, want %q", stdout, want)
			}
			if len(c.calls) != 1 || c.calls[0] != command || c.lastID != "abc12345" {
				t.Fatalf("calls = %v, last ID = %q", c.calls, c.lastID)
			}
		})
	}
}

func TestRunControlUnknownJob(t *testing.T) {
	for _, command := range []string{"sync", "pause", "resume"} {
		t.Run(command, func(t *testing.T) {
			c := &fakeClient{err: errors.New("job nope1234 not found")}
			code, stdout, stderr := runCLI(t, c, command, "nope1234")

			if code != exitFailure {
				t.Fatalf("exit code = %d, want %d", code, exitFailure)
			}
			if stdout != "" {
				t.Fatalf("stdout = %q, want no output", stdout)
			}
			if !strings.Contains(stderr, "job nope1234 not found") {
				t.Fatalf("stderr = %q, want an actionable message", stderr)
			}
		})
	}
}

func TestRunStatusUnknownJob(t *testing.T) {
	c := &fakeClient{jobs: []daemon.JobResponse{{ID: "aaaaaaaa"}}}
	code, _, stderr := runCLI(t, c, "status", "nope1234")

	if code != exitFailure {
		t.Fatalf("exit code = %d, want %d", code, exitFailure)
	}
	if !strings.Contains(stderr, "job nope1234 not found") {
		t.Fatalf("stderr = %q", stderr)
	}
}

func TestRunListAndStatusReportUnreachableDaemon(t *testing.T) {
	for _, args := range [][]string{{"list"}, {"ls"}, {"status"}, {"status", "abc12345"}} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			c := &fakeClient{err: &apiclient.UnreachableError{Err: errors.New("dial unix: connection refused")}}
			code, _, stderr := runCLI(t, c, args...)

			if code != exitFailure {
				t.Fatalf("exit code = %d, want %d", code, exitFailure)
			}
			if !strings.Contains(stderr, "cannot reach daemon (is it running?)") || !strings.Contains(stderr, "connection refused") {
				t.Fatalf("stderr = %q, want the daemon hint and the cause", stderr)
			}
		})
	}
}

func TestRunControlReportsUnreachableDaemon(t *testing.T) {
	c := &fakeClient{err: &apiclient.UnreachableError{Err: errors.New("dial unix: connection refused")}}
	code, _, stderr := runCLI(t, c, "pause", "abc12345")

	if code != exitFailure {
		t.Fatalf("exit code = %d, want %d", code, exitFailure)
	}
	if !strings.Contains(stderr, "cannot reach daemon (is it running?)") {
		t.Fatalf("stderr = %q, want the daemon hint", stderr)
	}
}

func TestFailureMessageKeepsDaemonErrorsVerbatim(t *testing.T) {
	if got := failureMessage(errors.New("job abc12345 not found")); got != "job abc12345 not found" {
		t.Fatalf("failureMessage = %q, want the original error", got)
	}
}

func TestRunListEmpty(t *testing.T) {
	code, stdout, stderr := runCLI(t, &fakeClient{}, "list")
	if code != exitOK {
		t.Fatalf("exit code = %d, want %d (stderr %q)", code, exitOK, stderr)
	}
	if stdout != "no jobs\n" {
		t.Fatalf("stdout = %q, want %q", stdout, "no jobs\n")
	}
}

func TestRunStatusDetailWithNoBatchYet(t *testing.T) {
	job := daemon.JobResponse{
		ID:         "abc12345",
		ConfigPath: "/tmp/a.yaml",
		Status:     daemon.StatusResponse{Phase: "idle"},
	}

	code, stdout, stderr := runCLI(t, &fakeClient{jobs: []daemon.JobResponse{job}}, "status", "abc12345")
	if code != exitOK {
		t.Fatalf("exit code = %d, want %d (stderr %q)", code, exitOK, stderr)
	}

	// A job that has never run a batch must still render cleanly.
	for _, want := range []string{
		"phase:         idle",
		"paused:        no",
		"last sync:     never",
		"last success:  never",
		"batch:         -",
		"bytes:         -",
	} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("stdout missing %q:\n%s", want, stdout)
		}
	}
	for _, unwanted := range []string{"0 of 0", "current file", "error:"} {
		if strings.Contains(stdout, unwanted) {
			t.Fatalf("stdout should not contain %q:\n%s", unwanted, stdout)
		}
	}
}

func TestRunStatusShowsPausedAndErrorRows(t *testing.T) {
	job := daemon.JobResponse{
		ID: "abc12345",
		Status: daemon.StatusResponse{
			Phase:          "downloading",
			Paused:         true,
			BatchTotal:     4,
			Completed:      1,
			Failed:         1,
			Remaining:      2,
			BytesTotal:     2048,
			BytesCompleted: 1024,
			LastError:      "copy failed",
		},
	}

	_, stdout, _ := runCLI(t, &fakeClient{jobs: []daemon.JobResponse{job}}, "status", "abc12345")

	for _, want := range []string{
		"phase:         downloading (paused)",
		"paused:        yes",
		"batch:         1/4 complete, 1 failed, 2 remaining",
		"bytes:         1.0 KB of 2.0 KB (50%)",
		"error:         copy failed",
	} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("stdout missing %q:\n%s", want, stdout)
		}
	}
}

func TestRunStatusWithoutIDPrintsEveryJob(t *testing.T) {
	jobs := []daemon.JobResponse{
		{ID: "aaaaaaaa", Status: daemon.StatusResponse{Phase: "idle"}},
		{ID: "bbbbbbbb", Status: daemon.StatusResponse{Phase: "paused", Paused: true}},
	}

	code, stdout, stderr := runCLI(t, &fakeClient{jobs: jobs}, "status")
	if code != exitOK {
		t.Fatalf("exit code = %d, want %d (stderr %q)", code, exitOK, stderr)
	}
	for _, id := range []string{"aaaaaaaa", "bbbbbbbb"} {
		if !strings.Contains(stdout, "id:            "+id) {
			t.Fatalf("stdout missing job %s:\n%s", id, stdout)
		}
	}
	if !strings.Contains(stdout, "paused:        yes") {
		t.Fatalf("stdout missing the paused state:\n%s", stdout)
	}
}

func TestRunRemoveReportsID(t *testing.T) {
	c := &fakeClient{}
	code, stdout, stderr := runCLI(t, c, "rm", "abc12345")
	if code != exitOK {
		t.Fatalf("exit code = %d, want %d (stderr %q)", code, exitOK, stderr)
	}
	if stdout != "removed job abc12345\n" {
		t.Fatalf("stdout = %q", stdout)
	}
	if len(c.calls) != 1 || c.calls[0] != "remove" {
		t.Fatalf("calls = %v", c.calls)
	}
}
