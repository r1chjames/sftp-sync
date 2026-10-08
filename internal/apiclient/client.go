package apiclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/r1chjames/sftp-sync/internal/daemon"
)

// defaultBaseURL is the authority for requests; the transport dials the Unix
// socket regardless of the host in the URL.
const defaultBaseURL = "http://daemon"

// Client communicates with the sftpsyncd daemon over its Unix socket.
type Client struct {
	http    *http.Client
	baseURL string
}

func New() *Client {
	socketPath := daemon.SocketPath()
	return &Client{
		baseURL: defaultBaseURL,
		http: &http.Client{
			Timeout: 10 * time.Second,
			Transport: &http.Transport{
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					return (&net.Dialer{}).DialContext(ctx, "unix", socketPath)
				},
			},
		},
	}
}

// responseError converts a non-success response into an error that preserves
// the status code and the daemon's message.
func responseError(resp *http.Response) error {
	var b bytes.Buffer
	b.ReadFrom(resp.Body)
	msg := strings.TrimSpace(b.String())
	if msg == "" {
		msg = http.StatusText(resp.StatusCode)
	}
	return fmt.Errorf("daemon error (%d): %s", resp.StatusCode, msg)
}

// UnreachableError reports that the daemon could not be contacted at all, as
// opposed to answering with an error status. Callers use it to tell "the daemon
// said no" apart from "the daemon is not there".
type UnreachableError struct {
	Err error
}

func (e *UnreachableError) Error() string { return e.Err.Error() }
func (e *UnreachableError) Unwrap() error { return e.Err }

// do performs a request, classifying transport failures.
func (c *Client) do(req *http.Request) (*http.Response, error) {
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, &UnreachableError{Err: err}
	}
	return resp, nil
}

func (c *Client) get(url string) (*http.Response, error) {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	return c.do(req)
}

func (c *Client) post(url, contentType string, body io.Reader) (*http.Response, error) {
	req, err := http.NewRequest(http.MethodPost, url, body)
	if err != nil {
		return nil, err
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	return c.do(req)
}

// Ping returns nil if the daemon is reachable and serving the API.
func (c *Client) Ping() error {
	resp, err := c.get(c.baseURL + "/jobs")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return responseError(resp)
	}
	return nil
}

func (c *Client) ListJobs() ([]daemon.JobResponse, error) {
	resp, err := c.get(c.baseURL + "/jobs")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, responseError(resp)
	}
	var jobs []daemon.JobResponse
	if err := json.NewDecoder(resp.Body).Decode(&jobs); err != nil {
		return nil, fmt.Errorf("decode: %w", err)
	}
	return jobs, nil
}

func (c *Client) AddJob(configPath string) (daemon.JobResponse, error) {
	body, _ := json.Marshal(map[string]string{"config_path": configPath})
	resp, err := c.post(c.baseURL+"/jobs", "application/json", bytes.NewReader(body))
	if err != nil {
		return daemon.JobResponse{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		return daemon.JobResponse{}, responseError(resp)
	}
	var job daemon.JobResponse
	if err := json.NewDecoder(resp.Body).Decode(&job); err != nil {
		return daemon.JobResponse{}, fmt.Errorf("decode: %w", err)
	}
	return job, nil
}

// SyncJob asks the daemon to scan and download now instead of waiting for the
// next interval tick.
func (c *Client) SyncJob(id string) (daemon.JobResponse, error) {
	return c.jobAction(id, "sync")
}

// PauseJob stops the job from starting new scans and downloads.
func (c *Client) PauseJob(id string) (daemon.JobResponse, error) {
	return c.jobAction(id, "pause")
}

// ResumeJob clears the paused state and triggers an immediate scan.
func (c *Client) ResumeJob(id string) (daemon.JobResponse, error) {
	return c.jobAction(id, "resume")
}

// jobAction posts an asynchronous control action and returns the job status
// accepted by the daemon.
func (c *Client) jobAction(id, action string) (daemon.JobResponse, error) {
	resp, err := c.post(c.baseURL+"/jobs/"+id+"/"+action, "", nil)
	if err != nil {
		return daemon.JobResponse{}, err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return daemon.JobResponse{}, fmt.Errorf("job %s not found", id)
	}
	if resp.StatusCode != http.StatusAccepted {
		return daemon.JobResponse{}, responseError(resp)
	}

	var job daemon.JobResponse
	if err := json.NewDecoder(resp.Body).Decode(&job); err != nil {
		return daemon.JobResponse{}, fmt.Errorf("decode: %w", err)
	}
	return job, nil
}

func (c *Client) RemoveJob(id string) error {
	req, err := http.NewRequest(http.MethodDelete, c.baseURL+"/jobs/"+id, nil)
	if err != nil {
		return err
	}
	resp, err := c.do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return fmt.Errorf("job %s not found", id)
	}
	if resp.StatusCode != http.StatusNoContent {
		return responseError(resp)
	}
	return nil
}

func (c *Client) Shutdown() error {
	resp, err := c.post(c.baseURL+"/shutdown", "", nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return responseError(resp)
	}
	return nil
}
