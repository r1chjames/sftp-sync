package sftp

import (
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"time"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
	"golang.org/x/crypto/ssh/knownhosts"
	"github.com/r1chjames/sftp-sync/internal/config"
)

// RemoteFile represents a file discovered on the SFTP server.
type RemoteFile struct {
	Path  string
	MTime time.Time
	Size  int64
}

// Client wraps an SFTP connection and provides high-level operations.
type Client struct {
	cfg    *config.Config
	conn   *ssh.Client
	sftpc  *sftp.Client
}

func New(cfg *config.Config) *Client {
	return &Client{cfg: cfg}
}

// Connect establishes the SSH and SFTP connections. Safe to call if already
// connected — it will close the existing connection first.
func (c *Client) Connect() error {
	c.Close()

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
	return nil
}

// IsConnected returns true if the connection appears healthy.
func (c *Client) IsConnected() bool {
	if c.sftpc == nil {
		return false
	}
	_, err := c.sftpc.Getwd()
	return err == nil
}

// EnsureConnected reconnects only if the current connection is unhealthy.
func (c *Client) EnsureConnected() error {
	if c.IsConnected() {
		return nil
	}
	return c.Connect()
}

// Close shuts down the SFTP and SSH connections.
func (c *Client) Close() {
	if c.sftpc != nil {
		c.sftpc.Close()
		c.sftpc = nil
	}
	if c.conn != nil {
		c.conn.Close()
		c.conn = nil
	}
}

// Walk returns all regular files under remotePath recursively.
func (c *Client) Walk(remotePath string) ([]RemoteFile, error) {
	walker := c.sftpc.Walk(remotePath)
	var files []RemoteFile
	for walker.Step() {
		if err := walker.Err(); err != nil {
			continue // skip unreadable entries
		}
		info := walker.Stat()
		if info.IsDir() {
			continue
		}
		files = append(files, RemoteFile{
			Path:  walker.Path(),
			MTime: info.ModTime(),
			Size:  info.Size(),
		})
	}
	return files, nil
}

// Download copies a remote file to localPath, writing atomically via a temp file.
func (c *Client) Download(remotePath, localPath string) error {
	if err := os.MkdirAll(filepath.Dir(localPath), 0755); err != nil {
		return fmt.Errorf("mkdir: %w", err)
	}

	src, err := c.sftpc.Open(remotePath)
	if err != nil {
		return fmt.Errorf("open remote: %w", err)
	}
	defer src.Close()

	dst, err := os.CreateTemp(filepath.Dir(localPath), ".sftpsync-*")
	if err != nil {
		return fmt.Errorf("create temp: %w", err)
	}
	tmpPath := dst.Name()
	defer func() {
		dst.Close()
		os.Remove(tmpPath) // no-op if already renamed
	}()

	if _, err := io.Copy(dst, src); err != nil {
		return fmt.Errorf("copy: %w", err)
	}
	dst.Close()

	return os.Rename(tmpPath, localPath)
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
