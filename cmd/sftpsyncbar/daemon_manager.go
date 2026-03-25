//go:build darwin

package main

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/r1chjames/sftp-sync/internal/apiclient"
	"github.com/r1chjames/sftp-sync/internal/config"
)

// DaemonManager finds, starts, and stops the sftpsyncd process.
type DaemonManager struct {
	client *apiclient.Client
	cmd    *exec.Cmd // non-nil only if this app started the daemon
}

func NewDaemonManager(client *apiclient.Client) *DaemonManager {
	return &DaemonManager{client: client}
}

// EnsureRunning starts sftpsyncd if it isn't already reachable.
func (m *DaemonManager) EnsureRunning() error {
	if m.client.Ping() == nil {
		return nil // already running, we don't own it
	}

	bin, err := findDaemonBinary()
	if err != nil {
		return fmt.Errorf("sftpsyncd not found: %w", err)
	}

	logPath := config.ExpandHome("~/.local/share/sftpsync/sftpsyncd.log")
	if err := os.MkdirAll(filepath.Dir(logPath), 0755); err != nil {
		return fmt.Errorf("mkdir log dir: %w", err)
	}
	logFile, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return fmt.Errorf("open log file: %w", err)
	}

	m.cmd = exec.Command(bin)
	m.cmd.Stdout = logFile
	m.cmd.Stderr = logFile
	if err := m.cmd.Start(); err != nil {
		logFile.Close()
		return fmt.Errorf("start daemon: %w", err)
	}

	// Close our handle to the log file — the child process holds its own fd.
	logFile.Close()

	// Wait up to 3 seconds for the socket to appear.
	for i := 0; i < 15; i++ {
		time.Sleep(200 * time.Millisecond)
		if m.client.Ping() == nil {
			log.Printf("daemon started (pid %d)", m.cmd.Process.Pid)
			return nil
		}
	}
	return fmt.Errorf("daemon started but socket not reachable after 3s")
}

// Shutdown stops the daemon if this app started it.
func (m *DaemonManager) Shutdown() {
	if m.cmd == nil {
		return // we don't own the daemon
	}
	if err := m.client.Shutdown(); err != nil {
		log.Printf("shutdown request failed: %v", err)
	}
	done := make(chan struct{})
	go func() {
		m.cmd.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		log.Println("daemon did not exit in time, killing")
		m.cmd.Process.Kill()
	}
}

func findDaemonBinary() (string, error) {
	// 1. Same directory as this executable. EvalSymlinks resolves macOS
	// paths that os.Executable can return through /private or temp dirs.
	if exe, err := os.Executable(); err == nil {
		if resolved, err := filepath.EvalSymlinks(exe); err == nil {
			exe = resolved
		}
		candidate := filepath.Join(filepath.Dir(exe), "sftpsyncd")
		if _, err := os.Stat(candidate); err == nil {
			return candidate, nil
		}
	}
	// 2. Working directory — covers IDE runs where the binary is copied to a
	// temp dir but cwd is the project root.
	if cwd, err := os.Getwd(); err == nil {
		candidate := filepath.Join(cwd, "sftpsyncd")
		if _, err := os.Stat(candidate); err == nil {
			return candidate, nil
		}
	}
	// 3. $PATH
	return exec.LookPath("sftpsyncd")
}

// pickConfigFile opens a native macOS file-chooser and returns the selected path.
func pickConfigFile() (string, error) {
	out, err := exec.Command("osascript", "-e",
		`POSIX path of (choose file of type {"yaml", "yml"} with prompt "Select sftpsync config file:")`).Output()
	if err != nil {
		return "", err // user cancelled or error
	}
	return strings.TrimSpace(string(out)), nil
}
