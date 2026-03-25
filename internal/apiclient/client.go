package apiclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"time"

	"github.com/r1chjames/sftp-sync/internal/daemon"
)

const baseURL = "http://daemon"

// Client communicates with the sftpsyncd daemon over its Unix socket.
type Client struct {
	http *http.Client
}

func New() *Client {
	socketPath := daemon.SocketPath()
	return &Client{
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

// Ping returns nil if the daemon is reachable.
func (c *Client) Ping() error {
	resp, err := c.http.Get(baseURL + "/jobs")
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

func (c *Client) ListJobs() ([]daemon.JobResponse, error) {
	resp, err := c.http.Get(baseURL + "/jobs")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var jobs []daemon.JobResponse
	if err := json.NewDecoder(resp.Body).Decode(&jobs); err != nil {
		return nil, fmt.Errorf("decode: %w", err)
	}
	return jobs, nil
}

func (c *Client) AddJob(configPath string) (daemon.JobResponse, error) {
	body, _ := json.Marshal(map[string]string{"config_path": configPath})
	resp, err := c.http.Post(baseURL+"/jobs", "application/json", bytes.NewReader(body))
	if err != nil {
		return daemon.JobResponse{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		var b bytes.Buffer
		b.ReadFrom(resp.Body)
		return daemon.JobResponse{}, fmt.Errorf("daemon error (%d): %s", resp.StatusCode, b.String())
	}
	var job daemon.JobResponse
	if err := json.NewDecoder(resp.Body).Decode(&job); err != nil {
		return daemon.JobResponse{}, fmt.Errorf("decode: %w", err)
	}
	return job, nil
}

func (c *Client) RemoveJob(id string) error {
	req, _ := http.NewRequest(http.MethodDelete, baseURL+"/jobs/"+id, nil)
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return fmt.Errorf("job %s not found", id)
	}
	return nil
}

func (c *Client) Shutdown() error {
	resp, err := c.http.Post(baseURL+"/shutdown", "", nil)
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}
