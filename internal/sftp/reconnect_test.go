package sftp

import (
	"strings"
	"sync"
	"testing"

	"github.com/r1chjames/sftp-sync/internal/config"
)

// unreachableClient points at a closed port so a reconnect attempt fails fast
// and observably, without needing a server.
func unreachableClient(t *testing.T) *Client {
	t.Helper()
	return New(&config.Config{
		SFTP: config.SFTPConfig{
			Host:                  "127.0.0.1",
			Port:                  1,
			User:                  "test",
			Password:              "test",
			InsecureIgnoreHostKey: true,
		},
	})
}

// TestEnsureConnectedReconnectsAfterMarkStale is the requirement that a
// transport failure leads to a new connection on the next attempt.
func TestEnsureConnectedReconnectsAfterMarkStale(t *testing.T) {
	c := unreachableClient(t)

	c.MarkStale()

	err := c.EnsureConnected()
	if err == nil {
		t.Fatal("EnsureConnected() = nil, want a reconnect attempt to the unreachable host")
	}
	if !strings.Contains(err.Error(), "ssh dial") {
		t.Fatalf("error = %v, want a dial failure: the client must try to reconnect", err)
	}
}

// TestMarkStaleIsHarmlessWithoutAConnection covers the first-transport-failure
// case, where there is nothing to mark.
func TestMarkStaleIsHarmlessWithoutAConnection(t *testing.T) {
	c := unreachableClient(t)

	c.MarkStale()
	c.MarkStale()

	if c.IsConnected() {
		t.Fatal("IsConnected() = true, want false without a connection")
	}
	if _, err := c.ready(); err == nil {
		t.Fatal("ready() = nil, want a connection error")
	}
}

// TestCloseClearsStaleness checks that a deliberate close leaves the client in a
// clean state rather than remembering a failure that no longer applies.
func TestCloseClearsStaleness(t *testing.T) {
	c := unreachableClient(t)

	c.MarkStale()
	c.Close()

	c.mu.Lock()
	defer c.mu.Unlock()
	if c.stale {
		t.Fatal("stale = true after Close, want the flag cleared")
	}
	if c.sftpc != nil || c.conn != nil {
		t.Fatal("Close left a connection behind")
	}
}

// TestClientIsSafeForConcurrentUse exercises the locking around the connection
// fields. Run with -race to check it properly; without -race it still catches a
// nil dereference from a connection swapped out while another caller uses it.
func TestClientIsSafeForConcurrentUse(t *testing.T) {
	c := unreachableClient(t)

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			switch i % 4 {
			case 0:
				c.MarkStale()
			case 1:
				c.Close()
			case 2:
				c.IsConnected()
			case 3:
				// ready() attempts a dial, which fails fast on the closed port.
				_, _ = c.ready()
			}
		}(i)
	}
	wg.Wait()
}
