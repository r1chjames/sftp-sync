//go:build darwin

package main

import (
	"fmt"
	"log"
	"path/filepath"
	"strings"
	"time"

	"github.com/getlantern/systray"
	"github.com/r1chjames/sftp-sync/internal/apiclient"
	"github.com/r1chjames/sftp-sync/internal/daemon"
)

const maxSlots = 10

type jobSlot struct {
	header    *systray.MenuItem
	lastSync  *systray.MenuItem
	fileCount *systray.MenuItem
	removeBtn *systray.MenuItem
	jobID     string
}

type appMenu struct {
	daemonStatus    *systray.MenuItem
	noJobs          *systray.MenuItem
	slots           [maxSlots]jobSlot
	addItem         *systray.MenuItem
	stopAllItem     *systray.MenuItem
	startDaemonItem *systray.MenuItem
	refreshItem     *systray.MenuItem
	quitItem        *systray.MenuItem
}

func buildMenu() *appMenu {
	m := &appMenu{}

	m.daemonStatus = systray.AddMenuItem("Daemon: starting…", "")
	m.daemonStatus.Disable()
	systray.AddSeparator()

	for i := range m.slots {
		s := &m.slots[i]
		s.header = systray.AddMenuItem("", "")
		s.header.Disable()
		s.lastSync = systray.AddMenuItem("", "")
		s.lastSync.Disable()
		s.fileCount = systray.AddMenuItem("", "")
		s.fileCount.Disable()
		s.removeBtn = systray.AddMenuItem("  Remove job", "Stop and remove this sync job")
		// Hide all slots initially.
		s.header.Hide()
		s.lastSync.Hide()
		s.fileCount.Hide()
		s.removeBtn.Hide()
	}

	m.noJobs = systray.AddMenuItem("No jobs — use 'Add Job' to get started", "")
	m.noJobs.Disable()

	systray.AddSeparator()
	m.addItem = systray.AddMenuItem("Add Job…", "Select a config file to start a new sync job")
	m.stopAllItem = systray.AddMenuItem("Stop All Syncs", "Stop the sync daemon and all running jobs")
	m.startDaemonItem = systray.AddMenuItem("Start Daemon", "Start the sync daemon")
	m.startDaemonItem.Hide()
	m.refreshItem = systray.AddMenuItem("Refresh Now", "Fetch latest job status")
	systray.AddSeparator()
	m.quitItem = systray.AddMenuItem("Quit", "Quit sftpsync menu bar app")

	return m
}

// update refreshes the menu to reflect the current job list.
// Safe to call from any goroutine.
func (m *appMenu) update(jobs []daemon.JobResponse, err error) {
	if err != nil {
		m.daemonStatus.SetTitle("Daemon: Not Running ●")
		m.addItem.Disable()
		m.stopAllItem.Hide()
		m.startDaemonItem.Show()
		m.showNoJobs()
		return
	}
	m.daemonStatus.SetTitle("Daemon: Running ●")
	m.addItem.Enable()
	m.stopAllItem.Show()
	m.startDaemonItem.Hide()

	// Populate visible slots.
	for i := range m.slots {
		s := &m.slots[i]
		if i >= len(jobs) {
			s.jobID = ""
			s.header.Hide()
			s.lastSync.Hide()
			s.fileCount.Hide()
			s.removeBtn.Hide()
			continue
		}
		j := jobs[i]
		s.jobID = j.ID

		name := jobDisplayName(j.ConfigPath)
		prefix := "●"
		if j.Status.LastError != "" {
			prefix = "⚠"
		}
		s.header.SetTitle(fmt.Sprintf("%s %s", prefix, name))

		lastSync := "Last sync: never"
		if !j.Status.LastSync.IsZero() {
			lastSync = "Last sync: " + j.Status.LastSync.Format("2006-01-02 15:04")
		}
		s.lastSync.SetTitle("  " + lastSync)
		s.fileCount.SetTitle(fmt.Sprintf("  Files: %d", j.Status.FilesTotal))

		s.header.Show()
		s.lastSync.Show()
		s.fileCount.Show()
		s.removeBtn.Show()
	}

	if len(jobs) == 0 {
		m.showNoJobs()
	} else {
		m.noJobs.Hide()
	}
}

func (m *appMenu) showNoJobs() {
	for i := range m.slots {
		m.slots[i].header.Hide()
		m.slots[i].lastSync.Hide()
		m.slots[i].fileCount.Hide()
		m.slots[i].removeBtn.Hide()
	}
	m.noJobs.Show()
}

// eventLoop handles menu item clicks. Must run in a goroutine.
func (m *appMenu) eventLoop(r *refresher, client *apiclient.Client, mgr *DaemonManager) {
	// Build a channel->slot index map for remove buttons.
	type removeCase struct {
		ch  <-chan struct{}
		idx int
	}
	removes := make([]removeCase, maxSlots)
	for i := range m.slots {
		removes[i] = removeCase{ch: m.slots[i].removeBtn.ClickedCh, idx: i}
	}

	for {
		// We use a select with all known channels. Go's select picks a ready
		// case at random, which is fine here.
		select {
		case <-m.addItem.ClickedCh:
			go handleAdd(client, r)
		case <-m.stopAllItem.ClickedCh:
			go handleStopAll(client, r)
		case <-m.startDaemonItem.ClickedCh:
			go m.handleStartDaemon(mgr, r)
		case <-m.refreshItem.ClickedCh:
			r.now()
		case <-m.quitItem.ClickedCh:
			systray.Quit()
		case <-removes[0].ch:
			go handleRemove(client, r, m.slots[0].jobID)
		case <-removes[1].ch:
			go handleRemove(client, r, m.slots[1].jobID)
		case <-removes[2].ch:
			go handleRemove(client, r, m.slots[2].jobID)
		case <-removes[3].ch:
			go handleRemove(client, r, m.slots[3].jobID)
		case <-removes[4].ch:
			go handleRemove(client, r, m.slots[4].jobID)
		case <-removes[5].ch:
			go handleRemove(client, r, m.slots[5].jobID)
		case <-removes[6].ch:
			go handleRemove(client, r, m.slots[6].jobID)
		case <-removes[7].ch:
			go handleRemove(client, r, m.slots[7].jobID)
		case <-removes[8].ch:
			go handleRemove(client, r, m.slots[8].jobID)
		case <-removes[9].ch:
			go handleRemove(client, r, m.slots[9].jobID)
		}
	}
}

func handleAdd(client *apiclient.Client, r *refresher) {
	path, err := pickConfigFile()
	if err != nil {
		return // user cancelled
	}
	if _, err := client.AddJob(path); err != nil {
		// TODO: show error in menu
		return
	}
	r.now()
}

func handleRemove(client *apiclient.Client, r *refresher, jobID string) {
	if jobID == "" {
		return
	}
	client.RemoveJob(jobID)
	r.now()
}

func handleStopAll(client *apiclient.Client, r *refresher) {
	client.Shutdown()
	// Wait for the daemon to fully stop before refreshing, so the Start
	// button only appears once the socket is gone and EnsureRunning will
	// actually launch a new process rather than seeing a live (dying) daemon.
	for i := 0; i < 25; i++ {
		time.Sleep(200 * time.Millisecond)
		if client.Ping() != nil {
			break
		}
	}
	r.now()
}

func (m *appMenu) handleStartDaemon(mgr *DaemonManager, r *refresher) {
	m.daemonStatus.SetTitle("Daemon: Starting…")
	if err := mgr.EnsureRunning(); err != nil {
		log.Printf("start daemon: %v", err)
		m.daemonStatus.SetTitle("Daemon: Failed to start — " + err.Error())
		return
	}
	r.now()
}

// jobDisplayName derives a human-readable name from a config file path.
// e.g. "/home/user/photos.example.com.yaml" → "photos.example.com"
func jobDisplayName(configPath string) string {
	base := filepath.Base(configPath)
	return strings.TrimSuffix(base, filepath.Ext(base))
}
