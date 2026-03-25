//go:build darwin

package main

import (
	"time"

	"github.com/r1chjames/sftp-sync/internal/apiclient"
	"github.com/r1chjames/sftp-sync/internal/daemon"
)

type refresher struct {
	trigger chan struct{}
}

func newRefresher() *refresher {
	return &refresher{trigger: make(chan struct{}, 1)}
}

// start launches the background polling goroutine. On each tick (and
// immediately on the first call) it fetches the job list and calls update.
func (r *refresher) start(client *apiclient.Client, update func([]daemon.JobResponse, error)) {
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			jobs, err := client.ListJobs()
			update(jobs, err)
			select {
			case <-ticker.C:
			case <-r.trigger:
			}
		}
	}()
}

// now triggers an immediate refresh without waiting for the next tick.
func (r *refresher) now() {
	select {
	case r.trigger <- struct{}{}:
	default:
	}
}
