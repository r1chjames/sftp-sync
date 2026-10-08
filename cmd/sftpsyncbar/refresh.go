//go:build darwin

package main

import (
	"time"

	"github.com/r1chjames/sftp-sync/internal/apiclient"
	"github.com/r1chjames/sftp-sync/internal/daemon"
	"github.com/r1chjames/sftp-sync/internal/menubar"
)

type refresher struct {
	trigger chan struct{}
}

func newRefresher() *refresher {
	return &refresher{trigger: make(chan struct{}, 1)}
}

// start launches the background polling goroutine.
//
// The interval is adaptive: refreshInterval (see status.go) returns one second
// while any job is scanning or downloading, so an open menu never shows status
// more than two seconds old, and 30 seconds while everything is idle or paused,
// so an inactive app stays quiet.
func (r *refresher) start(client *apiclient.Client, update func([]daemon.JobResponse, error)) {
	go func() {
		// The timer starts at 0 so the first fetch happens immediately.
		timer := time.NewTimer(0)
		defer timer.Stop()

		for {
			jobs, err := client.ListJobs()
			update(jobs, err)

			// Reused across iterations rather than recreated, so a click that
			// wakes the loop does not leave a pending timer behind.
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			timer.Reset(menubar.RefreshInterval(jobs, err))

			select {
			case <-timer.C:
			case <-r.trigger:
			}
		}
	}()
}

// now triggers an immediate refresh without waiting for the next tick. It never
// blocks: one pending request is enough to run a refresh as soon as possible.
func (r *refresher) now() {
	select {
	case r.trigger <- struct{}{}:
	default:
	}
}
