//go:build darwin

package main

import (
	"github.com/getlantern/systray"
	"github.com/r1chjames/sftp-sync/internal/apiclient"
	"github.com/r1chjames/sftp-sync/internal/daemon"
	"github.com/r1chjames/sftp-sync/internal/menubar"
)

const maxSlots = 10

// jobSlot is one job's group of menu rows. The row text is produced by the pure
// helpers in status.go; this type only pushes it into systray.
type jobSlot struct {
	header      *systray.MenuItem
	state       *systray.MenuItem
	currentFile *systray.MenuItem
	lastSuccess *systray.MenuItem
	errorRow    *systray.MenuItem
	removeBtn   *systray.MenuItem
	jobID       string
}

func (s *jobSlot) hide() {
	s.header.Hide()
	s.state.Hide()
	s.currentFile.Hide()
	s.lastSuccess.Hide()
	s.errorRow.Hide()
	s.removeBtn.Hide()
}

// show renders a job into the slot, hiding rows that have nothing to say.
func (s *jobSlot) show(j daemon.JobResponse) {
	s.jobID = j.ID
	view := menubar.JobSlotView(j)

	s.header.SetTitle(view.Header)
	s.state.SetTitle(view.State)
	s.lastSuccess.SetTitle(view.LastSuccess)

	setOptionalTitle(s.currentFile, view.CurrentFile)
	setOptionalTitle(s.errorRow, view.Error)

	s.header.Show()
	s.state.Show()
	s.lastSuccess.Show()
	s.removeBtn.Show()
}

// setOptionalTitle shows a row only when it has content, so empty rows never
// take up space in the menu.
func setOptionalTitle(item *systray.MenuItem, title string) {
	if title == "" {
		item.Hide()
		return
	}
	item.SetTitle(title)
	item.Show()
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
		s.state = systray.AddMenuItem("", "")
		s.state.Disable()
		s.currentFile = systray.AddMenuItem("", "")
		s.currentFile.Disable()
		s.lastSuccess = systray.AddMenuItem("", "")
		s.lastSuccess.Disable()
		s.errorRow = systray.AddMenuItem("", "")
		s.errorRow.Disable()
		s.removeBtn = systray.AddMenuItem("  Remove job", "Stop and remove this sync job")
		s.hide()
	}

	m.noJobs = systray.AddMenuItem("No jobs — use 'Add Job' to get started", "")
	m.noJobs.Disable()

	systray.AddSeparator()
	m.addItem = systray.AddMenuItem("Add Job…", "Select a config file to start a new sync job")
	m.stopAllItem = systray.AddMenuItem("Stop All Syncs", "Stop the sync daemon and all running jobs")
	m.startDaemonItem = systray.AddMenuItem("Start Daemon", "Start the sync daemon")
	m.startDaemonItem.Hide()
	m.refreshItem = systray.AddMenuItem("Refresh Status", "Fetch the latest job status now")
	systray.AddSeparator()
	m.quitItem = systray.AddMenuItem("Quit", "Quit sftpsync menu bar app")

	return m
}

// update refreshes the menu to reflect the current job list.
// Safe to call from any goroutine.
func (m *appMenu) update(jobs []daemon.JobResponse, err error) {
	// The title shows an aggregate percentage while jobs are downloading, and
	// an empty title restores the icon-only menu bar.
	systray.SetTitle(menubar.MenuTitle(jobs))

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

	for i := range m.slots {
		s := &m.slots[i]
		if i >= len(jobs) {
			s.jobID = ""
			s.hide()
			continue
		}
		s.show(jobs[i])
	}

	if len(jobs) == 0 {
		m.showNoJobs()
	} else {
		m.noJobs.Hide()
	}
}

func (m *appMenu) showNoJobs() {
	for i := range m.slots {
		m.slots[i].jobID = ""
		m.slots[i].hide()
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
	if err := client.Shutdown(); err != nil {
		// The daemon is already gone; that is what the menu wanted anyway.
		r.now()
		return
	}
	r.now()
}

func (m *appMenu) handleStartDaemon(mgr *DaemonManager, r *refresher) {
	mgr.EnsureRunning()
	r.now()
}
