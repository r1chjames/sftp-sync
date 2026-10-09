package sftp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/pkg/sftp"
	"github.com/r1chjames/sftp-sync/internal/config"
	"github.com/r1chjames/sftp-sync/internal/verify"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
	"golang.org/x/crypto/ssh/knownhosts"
)

// StagingPrefix is the name prefix of the temp files a download writes before
// it is moved into place. Cleanup code matches this prefix, so the two must not
// drift apart.
const StagingPrefix = ".sftpsync-"

// RemoteFile represents a file discovered on the SFTP server.
type RemoteFile struct {
	Path  string
	MTime time.Time
	Size  int64
}

// Client wraps an SFTP connection and provides high-level operations.
//
// It is used concurrently by the download workers, so the connection fields are
// guarded by mu. The sftp.Client itself multiplexes concurrent requests, which is
// what makes parallel downloads possible, but replacing or closing it is not safe
// while another goroutine is using it: every access goes through current, and
// closing happens only under the lock.
type Client struct {
	cfg *config.Config

	mu    sync.Mutex
	conn  *ssh.Client
	sftpc *sftp.Client
	// stale is set after a transport failure. The underlying connection cannot
	// be closed from the worker that noticed the failure without disturbing the
	// transfers other workers are running, so it is marked instead and replaced
	// by the next worker that needs a connection.
	stale bool
}

func New(cfg *config.Config) *Client {
	return &Client{cfg: cfg}
}

// Connect establishes the SSH and SFTP connections. Safe to call if already
// connected — it will close the existing connection first.
func (c *Client) Connect() error {
	// Held for the whole dial: the workers that noticed a stale connection would
	// otherwise all attempt to replace it at once, and the last one would win
	// while the others' new connections leaked.
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.connectLocked()
}

func (c *Client) connectLocked() error {
	c.closeLocked()

	authMethods, err := c.authMethods()
	if err != nil {
		return fmt.Errorf("auth: %w", err)
	}

	hostKey, err := c.hostKeyCallback()
	if err != nil {
		return fmt.Errorf("host key: %w", err)
	}

	sshCfg := &ssh.ClientConfig{
		User:            c.cfg.SFTP.User,
		Auth:            authMethods,
		HostKeyCallback: hostKey,
		Timeout:         30 * time.Second,
	}

	addr := fmt.Sprintf("%s:%d", c.cfg.SFTP.Host, c.cfg.SFTP.Port)
	conn, err := ssh.Dial("tcp", addr, sshCfg)
	if err != nil {
		return fmt.Errorf("ssh dial %s: %w", addr, err)
	}

	sftpc, err := sftp.NewClient(conn)
	if err != nil {
		conn.Close()
		return fmt.Errorf("sftp client: %w", err)
	}

	c.conn = conn
	c.sftpc = sftpc
	c.stale = false
	return nil
}

// IsConnected returns true if the connection appears healthy.
func (c *Client) IsConnected() bool {
	client := c.current()
	if client == nil {
		return false
	}
	_, err := client.Getwd()
	return err == nil
}

// EnsureConnected reconnects only if the current connection is unhealthy.
func (c *Client) EnsureConnected() error {
	c.mu.Lock()
	stale := c.stale
	c.mu.Unlock()

	// A connection marked stale by a transport failure is replaced even if it
	// still answers a probe: the failure was real, and the next transfer is the
	// one that would hit it again.
	if !stale && c.IsConnected() {
		return nil
	}
	return c.Connect()
}

// MarkStale records that the current connection failed in transit and must be
// replaced before it is used again. It does not close anything, because other
// workers may still be mid-transfer on the same connection.
func (c *Client) MarkStale() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.stale = true
}

// current returns the SFTP client, or nil when there is none.
func (c *Client) current() *sftp.Client {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.sftpc
}

// ready returns a usable SFTP client, reconnecting first when the connection is
// missing or was marked stale by a transport failure.
//
// It deliberately does not probe the connection: it is called once per file, and
// an extra round trip per file to ask whether the connection is still there
// would cost more than the retry that already handles a connection that has
// quietly died.
func (c *Client) ready() (*sftp.Client, error) {
	c.mu.Lock()
	if c.sftpc != nil && !c.stale {
		client := c.sftpc
		c.mu.Unlock()
		return client, nil
	}
	if err := c.connectLocked(); err != nil {
		c.mu.Unlock()
		return nil, err
	}
	client := c.sftpc
	c.mu.Unlock()
	return client, nil
}

// Close shuts down the SFTP and SSH connections.
func (c *Client) Close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closeLocked()
}

func (c *Client) closeLocked() {
	if c.sftpc != nil {
		c.sftpc.Close()
		c.sftpc = nil
	}
	if c.conn != nil {
		c.conn.Close()
		c.conn = nil
	}
	c.stale = false
}

// HashRemote streams a remote file and returns its SHA-256 digest in
// lower-case hex. It is how a transfer or an adopted file is checked against
// what the server is actually offering, and it reads the whole file.
func (c *Client) HashRemote(ctx context.Context, remotePath string) (string, error) {
	client, err := c.ready()
	if err != nil {
		return "", err
	}

	src, err := client.Open(remotePath)
	if err != nil {
		return "", fmt.Errorf("open remote: %w", err)
	}
	defer src.Close()

	return verify.SHA256Stream(ctx, src)
}

// errNoStat stands in for a stat that returned no information at all.
var errNoStat = errors.New("no file information")

// maxWalkFailures bounds how many unreadable paths are kept in a walk result.
// A broken subtree can produce one failure per entry, and the status summary must
// not grow with it.
const maxWalkFailures = 20

// WalkFailure is one path a walk could not read.
type WalkFailure struct {
	Path string
	Err  error
}

// WalkResult is what a recursive walk found, including what it could not read.
//
// The distinction matters: a walk that skipped an unreadable subtree is not an
// inventory of the server, so no caller may treat it as one — least of all a
// future feature that deletes local files the remote no longer has.
type WalkResult struct {
	Files []RemoteFile
	// Failures holds up to maxWalkFailures of the unreadable paths.
	Failures []WalkFailure
	// FailureCount is the total number of unreadable paths, including any
	// beyond the cap.
	FailureCount int
}

// Complete reports whether every path the walk visited could be read.
func (w WalkResult) Complete() bool { return w.FailureCount == 0 }

// Summary describes a partial walk in a bounded string, naming the first failure
// and counting the rest. Every failed path is logged as it is found.
func (w WalkResult) Summary() string {
	if w.Complete() {
		return ""
	}

	summary := fmt.Sprintf("remote scan incomplete: %d path(s) could not be read; first %s",
		w.FailureCount, w.Failures[0].Path)
	if detail := w.Failures[0].Err.Error(); detail != "" {
		summary += ": " + detail
	}
	if w.FailureCount > 1 {
		summary += fmt.Sprintf(" (%d more in the daemon log)", w.FailureCount-1)
	}
	return summary
}

// walkSteps is the part of *sftp.Walker that a walk uses, so the aggregation —
// including the failure cap and the cancellation check — can be tested without a
// server.
type walkSteps interface {
	Step() bool
	Path() string
	Stat() os.FileInfo
	Err() error
}

// Walk returns all regular files under remotePath recursively.
//
// An unreadable entry does not stop the walk: the files that could be read are
// returned, and the failures are reported in the result so the cycle can say it
// only saw part of the server. The error return is reserved for a walk that could
// not start at all, or one interrupted by cancellation.
func (c *Client) Walk(ctx context.Context, remotePath string) (WalkResult, error) {
	client, err := c.ready()
	if err != nil {
		return WalkResult{}, err
	}

	return walkFiles(ctx, remotePath, client.Walk(remotePath))
}

// walkFiles performs the walk and collects what it found.
func walkFiles(ctx context.Context, root string, walker walkSteps) (WalkResult, error) {
	var result WalkResult

	for walker.Step() {
		if err := ctx.Err(); err != nil {
			// Stopping mid-walk leaves the inventory incomplete, so the error is
			// reported rather than returned as a shorter list of files.
			return result, err
		}

		path := walker.Path()
		if err := walker.Err(); err != nil {
			if path == root {
				// The root of the walk is unreadable, so there is nothing to report
				// on: this is the configuration pointing at a path that does not
				// exist, not a partial scan.
				return result, fmt.Errorf("unreadable: %w", err)
			}

			result.FailureCount++
			if len(result.Failures) < maxWalkFailures {
				result.Failures = append(result.Failures, WalkFailure{Path: path, Err: err})
			}
			continue
		}

		info := walker.Stat()
		if info == nil {
			// A stat that returned nothing is as unreadable as one that errored.
			result.FailureCount++
			if len(result.Failures) < maxWalkFailures {
				result.Failures = append(result.Failures, WalkFailure{Path: path, Err: errNoStat})
			}
			continue
		}
		if info.IsDir() {
			continue
		}

		result.Files = append(result.Files, RemoteFile{
			Path:  path,
			MTime: info.ModTime(),
			Size:  info.Size(),
		})
	}

	return result, nil
}

// Download copies a remote file to localPath, writing atomically via a temp file.
func (c *Client) Download(remotePath, localPath string) error {
	if err := os.MkdirAll(filepath.Dir(localPath), 0755); err != nil {
		return fmt.Errorf("mkdir: %w", err)
	}

	tmpPath, err := c.DownloadTemp(remotePath, filepath.Dir(localPath))
	if err != nil {
		return err
	}
	defer os.Remove(tmpPath)

	return os.Rename(tmpPath, localPath)
}

// DownloadTemp downloads a remote file into stagingDir and returns the path to
// the temp file. The caller is responsible for removing or renaming the file.
func (c *Client) DownloadTemp(remotePath, stagingDir string) (string, error) {
	return c.DownloadTempProgress(remotePath, stagingDir, nil)
}

// DownloadTempProgress behaves like DownloadTemp and additionally reports the
// cumulative number of bytes copied. progress may be nil. If progress returns
// an error the transfer stops and the temp file is removed.
func (c *Client) DownloadTempProgress(remotePath, stagingDir string, progress func(copied int64) error) (string, error) {
	client, err := c.ready()
	if err != nil {
		return "", err
	}

	src, err := client.Open(remotePath)
	if err != nil {
		return "", fmt.Errorf("open remote: %w", err)
	}
	defer src.Close()

	if err := os.MkdirAll(stagingDir, 0755); err != nil {
		return "", fmt.Errorf("mkdir: %w", err)
	}

	dst, err := os.CreateTemp(stagingDir, StagingPrefix+"*")
	if err != nil {
		return "", fmt.Errorf("create temp: %w", err)
	}
	defer dst.Close()

	if _, err := copyWithProgress(dst, src, progress); err != nil {
		os.Remove(dst.Name())
		return "", fmt.Errorf("copy: %w", err)
	}

	if err := dst.Close(); err != nil {
		os.Remove(dst.Name())
		return "", fmt.Errorf("close temp: %w", err)
	}

	return dst.Name(), nil
}

// progressWriter counts bytes written and reports the running total after each
// write. It deliberately holds no state beyond the counter so concurrent
// workers cannot interfere with each other.
type progressWriter struct {
	w        io.Writer
	progress func(copied int64) error
	copied   int64
}

func (p *progressWriter) Write(b []byte) (int, error) {
	n, err := p.w.Write(b)
	if n > 0 {
		p.copied += int64(n)
		if p.progress != nil {
			if perr := p.progress(p.copied); perr != nil {
				return n, perr
			}
		}
	}
	return n, err
}

// copyWithProgress copies src into dst and reports cumulative bytes written.
// It streams through io.Copy, so it never buffers a whole photo in memory.
func copyWithProgress(dst io.Writer, src io.Reader, progress func(copied int64) error) (int64, error) {
	pw := &progressWriter{w: dst, progress: progress}
	return io.Copy(pw, src)
}

func (c *Client) authMethods() ([]ssh.AuthMethod, error) {
	var methods []ssh.AuthMethod

	if c.cfg.SFTP.Password != "" {
		methods = append(methods, ssh.Password(c.cfg.SFTP.Password))
	}

	if c.cfg.SFTP.KeyPath != "" {
		key, err := os.ReadFile(c.cfg.SFTP.KeyPath)
		if err != nil {
			return nil, fmt.Errorf("read key file: %w", err)
		}
		signer, err := ssh.ParsePrivateKey(key)
		if err != nil {
			return nil, fmt.Errorf("parse private key: %w", err)
		}
		methods = append(methods, ssh.PublicKeys(signer))
	}

	// Try ssh-agent if available (common on macOS)
	if sock := os.Getenv("SSH_AUTH_SOCK"); sock != "" {
		conn, err := net.Dial("unix", sock)
		if err == nil {
			methods = append(methods, ssh.PublicKeysCallback(agent.NewClient(conn).Signers))
		}
	}

	if len(methods) == 0 {
		return nil, fmt.Errorf("no auth method available: set sftp.password, sftp.key_path, or ensure ssh-agent is running (SSH_AUTH_SOCK)")
	}
	return methods, nil
}

func (c *Client) hostKeyCallback() (ssh.HostKeyCallback, error) {
	if c.cfg.SFTP.InsecureIgnoreHostKey {
		fmt.Fprintln(os.Stderr, "WARNING: host key verification is disabled")
		return ssh.InsecureIgnoreHostKey(), nil
	}

	knownHostsPath := config.ExpandHome("~/.ssh/known_hosts")
	cb, err := knownhosts.New(knownHostsPath)
	if err != nil {
		return nil, fmt.Errorf("loading known_hosts (%s): %w — add the host first with `ssh-keyscan`", knownHostsPath, err)
	}
	return cb, nil
}
