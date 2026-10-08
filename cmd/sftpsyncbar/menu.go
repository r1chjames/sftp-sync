//go:build darwin

package main

import (
	"fmt"
	"sync"

	"github.com/getlantern/systray"
	"github.com/r1chjames/sftp-sync/internal/apiclient"
	"github.com/r1chjames/sftp-sync/internal/daemon"
	"github.com/r1chjames/sftp-sync/internal/menubar"
)

const maxSlots = 10

// jobSlot is one job's group of menu rows.
//
// The row text is produced by the pure helpers in internal/menubar; this type
// only pushes it into systray. The job it currently displays is guarded by a
// mutex because it is written by the refresh goroutine and read by the click
// handlers, and every control action resolves its target through snapshot so a
// click always acts on the job the slot showed when it was clicked.
type jobSlot struct {
	header      *systray.MenuItem
	state       *systray.MenuItem
	currentFile *systray.MenuItem
	lastSuccess *systray.MenuItem
	errorRow    *systray.MenuItem
	actionRow   *systray.MenuItem

	pauseBtn  *systray.MenuItem
	resumeBtn *systray.MenuItem
	syncBtn   *systray.MenuItem
	removeBtn *systray.MenuItem

	mu          sync.Mutex
	job         daemon.JobResponse
	occupied    bool
	actionError string
}

// snapshot returns the job the slot currently displays, and whether the slot is
// showing anything at all.
func (s *jobSlot) snapshot() (daemon.JobResponse, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.job, s.occupied
}

// show renders a job into the slot, hiding rows that have nothing to say.
func (s *jobSlot) show(j daemon.JobResponse) {
	s.mu.Lock()
	s.job = j
	s.occupied = true
	view := menubar.JobSlotView(s.job, s.actionError)
	controls := menubar.SlotControlsFor(j.Status.Paused)
	s.mu.Unlock()

	s.render(view, controls)
}

// clear empties the slot, used when the job list shrinks.
func (s *jobSlot) clear() {
	s.mu.Lock()
	s.job = daemon.JobResponse{}
	s.occupied = false
	s.actionError = ""
	s.mu.Unlock()

	s.header.Hide()
	s.state.Hide()
	s.currentFile.Hide()
	s.lastSuccess.Hide()
	s.errorRow.Hide()
	s.actionRow.Hide()
	s.pauseBtn.Hide()
	s.resumeBtn.Hide()
	s.syncBtn.Hide()
	s.removeBtn.Hide()
}

func (s *jobSlot) render(view menubar.SlotView, controls menubar.SlotControls) {
	s.header.SetTitle(view.Header)
	s.state.SetTitle(view.State)
	s.lastSuccess.SetTitle(view.LastSuccess)
	setOptionalTitle(s.currentFile, view.CurrentFile)
	setOptionalTitle(s.errorRow, view.Error)
	setOptionalTitle(s.actionRow, view.ActionError)

	setOptionalTitle(s.pauseBtn, visibleTitle(controls.ShowPause, "  Pause"))
	setOptionalTitle(s.resumeBtn, visibleTitle(controls.ShowResume, "  Resume"))
	setOptionalTitle(s.syncBtn, "  Sync Now")
	setOptionalTitle(s.removeBtn, "  Remove Job…")

	s.header.Show()
	s.state.Show()
	s.lastSuccess.Show()
}

// visibleTitle returns the row title when the row should be visible, and "" to
// keep it hidden.
func visibleTitle(visible bool, title string) string {
	if !visible {
		return ""
	}
	return title
}

// setActionError stores a failed control request so the next render shows it.
func (s *jobSlot) setActionError(message string) {
	s.mu.Lock()
	s.actionError = message
	view := menubar.JobSlotView(s.job, s.actionError)
	s.mu.Unlock()

	setOptionalTitle(s.actionRow, view.ActionError)
}

// clearActionError drops a previous failure, so it does not outlive the action
// that produced it.
func (s *jobSlot) clearActionError() {
	s.setActionError("")
}

// setControlsEnabled disables a slot's controls while its request is in flight,
// so a double click cannot start a second request.
func (s *jobSlot) setControlsEnabled(enabled bool) {
	for _, item := range []*systray.MenuItem{s.pauseBtn, s.resumeBtn, s.syncBtn, s.removeBtn} {
		if enabled {
			item.Enable()
			continue
		}
		item.Disable()
	}
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
	moreJobs        *systray.MenuItem
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
		s.actionRow = systray.AddMenuItem("", "")
		s.actionRow.Disable()

		s.pauseBtn = systray.AddMenuItem("  Pause", "Stop starting new scans and downloads for this job")
		s.resumeBtn = systray.AddMenuItem("  Resume", "Clear the paused state and scan this job now")
		s.syncBtn = systray.AddMenuItem("  Sync Now", "Scan and download new files for this job now")
		// Remove is destructive and comes last, after the non-destructive
		// controls, so it is not clicked by accident.
		s.removeBtn = systray.AddMenuItem("  Remove Job…", "Stop and remove this job. This cannot be undone.")
		s.clear()
	}

	m.noJobs = systray.AddMenuItem("No jobs — use 'Add Job' to get started", "")
	m.noJobs.Disable()
	m.moreJobs = systray.AddMenuItem("", "")
	m.moreJobs.Disable()
	m.moreJobs.Hide()

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
			s.clear()
			continue
		}
		s.show(jobs[i])
	}

	// Never hide jobs silently.
	if notice := menubar.AdditionalJobsNotice(len(jobs), maxSlots); notice != "" {
		m.moreJobs.SetTitle(notice)
		m.moreJobs.Show()
	} else {
		m.moreJobs.Hide()
	}

	if len(jobs) == 0 {
		m.showNoJobs()
	} else {
		m.noJobs.Hide()
	}
}

func (m *appMenu) showNoJobs() {
	for i := range m.slots {
		m.slots[i].clear()
	}
	m.moreJobs.Hide()
	m.noJobs.Show()
}

// menuEvent binds one clickable menu row to the action it runs.
type menuEvent struct {
	ch <-chan struct{}
	// resolve samples everything the action depends on at the moment the click
	// arrives. It returns nil when there is nothing to do.
	resolve func() func()
}

// immediate binds a row whose action does not depend on mutable state.
func immediate(ch <-chan struct{}, handle func()) menuEvent {
	return menuEvent{ch: ch, resolve: func() func() { return handle }}
}

// slotAction binds a per-job control row.
//
// The job shown in the slot is sampled here, when the click is received, and
// the resulting action closes over that value. A refresh landing between the
// click and the request therefore cannot redirect the click onto whichever job
// the slot shows next.
func slotAction(ch <-chan struct{}, slot *jobSlot, act func(daemon.JobResponse) func()) menuEvent {
	return menuEvent{ch: ch, resolve: func() func() {
		job, ok := slot.snapshot()
		if !ok {
			return nil
		}
		return act(job)
	}}
}

// menuEvents builds the dispatch table for every clickable row.
//
// This replaces a hand-written select with a case per menu item, which no
// longer scales: ten job slots with four controls each would need forty cases.
func (m *appMenu) menuEvents(r *refresher, client *apiclient.Client, mgr *DaemonManager) []menuEvent {
	events := []menuEvent{
		immediate(m.addItem.ClickedCh, func() { handleAdd(client, r) }),
		immediate(m.stopAllItem.ClickedCh, func() { handleStopAll(client, r) }),
		immediate(m.refreshItem.ClickedCh, r.now),
		immediate(m.quitItem.ClickedCh, systray.Quit),
		immediate(m.startDaemonItem.ClickedCh, func() {
			mgr.EnsureRunning()
			r.now()
		}),
	}

	for i := range m.slots {
		slot := &m.slots[i]
		events = append(events,
			slotAction(slot.pauseBtn.ClickedCh, slot, func(job daemon.JobResponse) func() {
				return func() { m.control(slot, job, "pause", client, r) }
			}),
			slotAction(slot.resumeBtn.ClickedCh, slot, func(job daemon.JobResponse) func() {
				return func() { m.control(slot, job, "resume", client, r) }
			}),
			slotAction(slot.syncBtn.ClickedCh, slot, func(job daemon.JobResponse) func() {
				return func() { m.control(slot, job, "sync", client, r) }
			}),
			slotAction(slot.removeBtn.ClickedCh, slot, func(job daemon.JobResponse) func() {
				return func() { m.remove(slot, job, client, r) }
			}),
		)
	}
	return events
}

// eventLoop handles menu item clicks. Must run in a goroutine.
//
// Each row's channel is forwarded into one channel by a goroutine of its own,
// which keeps dispatch a single receive instead of one select case per row. The
// forwarder resolves the click's target before queueing it.
func (m *appMenu) eventLoop(r *refresher, client *apiclient.Client, mgr *DaemonManager) {
	clicks := make(chan func())

	for _, e := range m.menuEvents(r, client, mgr) {
		go func(e menuEvent) {
			for range e.ch {
				if act := e.resolve(); act != nil {
					clicks <- act
				}
			}
		}(e)
	}

	// Handlers run in their own goroutine so a slow request never blocks the
	// next click.
	for handle := range clicks {
		go handle()
	}
}

// control runs a non-destructive per-job action on the job the user clicked.
//
// The controls are disabled for the duration of the request and an immediate
// refresh follows, so the slot shows the new state promptly. A failure is shown
// in the job's own section, not only logged.
func (m *appMenu) control(slot *jobSlot, job daemon.JobResponse, action string, client *apiclient.Client, r *refresher) {
	slot.setControlsEnabled(false)
	slot.clearActionError()

	var err error
	switch action {
	case "pause":
		_, err = client.PauseJob(job.ID)
	case "resume":
		_, err = client.ResumeJob(job.ID)
	case "sync":
		_, err = client.SyncJob(job.ID)
	default:
		err = fmt.Errorf("unknown action %q", action)
	}

	if failure := menubar.ActionFailure(action, err); failure != "" {
		slot.setActionError(failure)
	}
	slot.setControlsEnabled(true)
	r.now()
}

// remove runs the destructive per-job action.
func (m *appMenu) remove(slot *jobSlot, job daemon.JobResponse, client *apiclient.Client, r *refresher) {
	slot.setControlsEnabled(false)
	slot.clearActionError()

	if failure := menubar.ActionFailure("remove", client.RemoveJob(job.ID)); failure != "" {
		slot.setActionError(failure)
	}
	slot.setControlsEnabled(true)
	r.now()
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

func handleStopAll(client *apiclient.Client, r *refresher) {
	if err := client.Shutdown(); err != nil {
		// The daemon is already gone; that is what the menu wanted anyway.
		r.now()
		return
	}
	r.now()
}
