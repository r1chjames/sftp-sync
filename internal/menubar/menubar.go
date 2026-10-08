// Package menubar holds the macOS menu-bar app's presentation and polling
// logic.
//
// It is deliberately free of systray and cgo so it compiles and is tested on
// every platform, not only on macOS. The systray bindings in
// cmd/sftpsyncbar are a thin shell over this package.
package menubar

import (
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/r1chjames/sftp-sync/internal/daemon"
	"github.com/r1chjames/sftp-sync/internal/humanize"
)

// Polling intervals. A transfer is worth watching closely; an inactive daemon
// is not worth waking up for. Both are chosen so a transfer status is never
// more than two seconds stale.
const (
	activeRefreshInterval = 1 * time.Second
	idleRefreshInterval   = 30 * time.Second
)

// activePhases are the phases during which a job is doing work, and therefore
// during which the menu polls quickly.
var activePhases = map[string]bool{
	"scanning":    true,
	"downloading": true,
}

// RefreshInterval returns how long to wait before polling again.
//
// An error means the daemon is unreachable, which is usually a daemon that is
// starting up after a menu action, so it polls quickly to reflect recovery.
func RefreshInterval(jobs []daemon.JobResponse, err error) time.Duration {
	if err != nil {
		return activeRefreshInterval
	}
	for _, j := range jobs {
		if activePhases[j.Status.Phase] {
			return activeRefreshInterval
		}
	}
	return idleRefreshInterval
}

// SlotView is everything a single job's menu slot displays. Empty fields are
// rows the menu hides.
type SlotView struct {
	Header      string // "● photos"
	State       string // "  downloading · 18 of 64 files · 28%"
	CurrentFile string // "  IMG_0042.CR3 — 900.0 KB of 3.1 MB (28%)"
	LastSuccess string // "  Last success: 2024-06-15 11:00"
	Error       string // "  ⚠ copy failed: permission denied"
	ActionError string // "  ⚠ pause failed: daemon unreachable"
}

// JobSlotView renders a job for the menu. actionError is the result of a
// control request made from this slot, and is shown alongside the daemon's own
// last error rather than replacing it: a failed pause and a failed sync are
// different problems.
func JobSlotView(j daemon.JobResponse, actionError string) SlotView {
	st := j.Status
	view := SlotView{
		Header:      fmt.Sprintf("%s %s", StatusPrefix(st), JobDisplayName(j.ConfigPath)),
		State:       "  " + StateText(st),
		LastSuccess: "  Last success: " + MenuTime(st.LastSuccessfulSync),
	}
	if st.CurrentFile != "" {
		view.CurrentFile = "  " + filepath.Base(st.CurrentFile)
		if progress := humanize.BytePair(st.CurrentFileBytesCompleted, st.CurrentFileBytesTotal); progress != "-" {
			view.CurrentFile += " — " + progress
		}
	}
	if st.LastError != "" {
		// The full error text goes in its own row: systray items do not wrap,
		// and a truncated error is the least useful thing to show.
		view.Error = "  ⚠ " + st.LastError
	}
	if actionError != "" {
		view.ActionError = "  ⚠ " + actionError
	}
	return view
}

// SlotControls describes which control rows a slot shows. A job offers Pause
// while it is active and Resume while it is paused, never both.
type SlotControls struct {
	ShowPause  bool
	ShowResume bool
}

// SlotControlsFor returns the controls appropriate to a job's paused state.
func SlotControlsFor(paused bool) SlotControls {
	return SlotControls{ShowPause: !paused, ShowResume: paused}
}

// ActionFailure renders a failed control request for display in the job's own
// menu section, so a failure is visible rather than only logged.
func ActionFailure(action string, err error) string {
	if err == nil {
		return ""
	}
	return fmt.Sprintf("%s failed: %v", action, err)
}

// AdditionalJobsNotice reports jobs that do not fit in the menu. Hiding jobs
// without saying so is not acceptable, so the menu states how many are missing.
func AdditionalJobsNotice(total, shown int) string {
	if total <= shown {
		return ""
	}
	missing := total - shown
	if missing == 1 {
		return "1 additional job not shown"
	}
	return fmt.Sprintf("%d additional jobs not shown", missing)
}

// StatusPrefix summarises a job at a glance.
func StatusPrefix(st daemon.StatusResponse) string {
	switch {
	case st.LastError != "":
		return "⚠"
	case st.Paused:
		return "⏸"
	case activePhases[st.Phase]:
		return "⟳"
	default:
		return "●"
	}
}

// StateText renders one row describing a job's activity: phase, file progress,
// and byte percentage when a total is known.
func StateText(st daemon.StatusResponse) string {
	parts := []string{humanize.Phase(st.Phase, st.Paused)}

	switch {
	case st.Phase == "downloading" && st.BatchTotal > 0:
		parts = append(parts, fmt.Sprintf("%d of %d files", attemptedFiles(st), st.BatchTotal))
	case st.FilesTotal > 0:
		parts = append(parts, fmt.Sprintf("%d files", st.FilesTotal))
	}

	if percent, ok := humanize.Percent(st.BytesCompleted, st.BytesTotal); ok {
		parts = append(parts, fmt.Sprintf("%d%%", percent))
	}
	return strings.Join(parts, " · ")
}

// attemptedFiles counts files a batch has finished with, successfully or not,
// and never exceeds the batch size. Skipped files count as finished: the
// collision policy decided their outcome, so they are not still in flight.
func attemptedFiles(st daemon.StatusResponse) int {
	attempted := st.Completed + st.Failed + st.Skipped
	if attempted < 0 {
		return 0
	}
	if attempted > st.BatchTotal {
		return st.BatchTotal
	}
	return attempted
}

// MenuState is the overall state the menu bar communicates.
type MenuState string

// Menu states, most urgent first.
const (
	StateIdle   MenuState = "idle"
	StatePaused MenuState = "paused"
	StateActive MenuState = "active"
	StateError  MenuState = "error"
)

// MenuStatus is what the menu bar shows for the whole app.
type MenuStatus struct {
	State   MenuState
	Title   string // shown beside the icon; "" leaves the icon alone
	Tooltip string // shown on hover
}

// MenuStatusFor summarises every job.
//
// The state resolves to the most urgent thing happening: an error outranks
// active transfers, which outrank a paused job, which outranks idle. The title
// carries the aggregate percentage of jobs currently downloading, because byte
// counters of a finished job describe its last batch, not the work in progress.
func MenuStatusFor(jobs []daemon.JobResponse) MenuStatus {
	var (
		completed int64
		total     int64
		active    int
		paused    int
		failed    int
	)

	for _, j := range jobs {
		switch {
		case j.Status.LastError != "":
			failed++
		case activePhases[j.Status.Phase]:
			// A paused job that is still draining a batch is counted as active:
			// files are still moving, and the settled state is what should be
			// reported once it arrives.
			active++
		case j.Status.Paused:
			paused++
		}

		if j.Status.Phase == "downloading" {
			completed += j.Status.BytesCompleted
			total += j.Status.BytesTotal
		}
	}

	state := StateIdle
	switch {
	case failed > 0:
		state = StateError
	case active > 0:
		state = StateActive
	case paused > 0:
		state = StatePaused
	}

	parts := make([]string, 0, 2)
	switch state {
	case StateError:
		parts = append(parts, "⚠")
	case StateActive:
		parts = append(parts, "⟳")
	case StatePaused:
		parts = append(parts, "⏸")
	}
	if percent, ok := humanize.Percent(completed, total); ok {
		parts = append(parts, fmt.Sprintf("%d%%", percent))
	}

	return MenuStatus{
		State:   state,
		Title:   strings.Join(parts, " "),
		Tooltip: menuTooltip(len(jobs), active, paused, failed),
	}
}

func menuTooltip(jobs, active, paused, failed int) string {
	if jobs == 0 {
		return "sftpsync — no jobs"
	}

	parts := []string{fmt.Sprintf("%d %s", jobs, plural(jobs, "job", "jobs"))}
	if active > 0 {
		parts = append(parts, fmt.Sprintf("%d active", active))
	}
	if paused > 0 {
		parts = append(parts, fmt.Sprintf("%d paused", paused))
	}
	if failed > 0 {
		parts = append(parts, fmt.Sprintf("%d with errors", failed))
	}
	return "sftpsync — " + strings.Join(parts, ", ")
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// RevealableDestination returns the destination directory to reveal, or "" when
// the job has none reported. An empty path is refused rather than passed to the
// system opener, which would open an unrelated location.
func RevealableDestination(j daemon.JobResponse) string {
	if strings.TrimSpace(j.LocalPath) == "" {
		return ""
	}
	return j.LocalPath
}

// RevealCommand returns the program and arguments used to reveal a path in the
// desktop file manager.
//
// The path is a single argv entry and is never passed through a shell, so a path
// containing spaces, quotes, or shell metacharacters cannot be interpreted as
// additional commands.
func RevealCommand(path string) (string, []string) {
	return "open", []string{path}
}

// CopyCommand returns the program used to put text on the clipboard. The text is
// written to the program's standard input, never as an argument or through a
// shell.
func CopyCommand() (string, []string) {
	return "pbcopy", nil
}

// CopyableError returns the error text for a job, for copying to the clipboard:
// the daemon's latest error and any failure from a menu action, one per line.
func CopyableError(j daemon.JobResponse, actionError string) string {
	lines := make([]string, 0, 2)
	if j.Status.LastError != "" {
		lines = append(lines, j.Status.LastError)
	}
	if actionError != "" {
		lines = append(lines, actionError)
	}
	return strings.Join(lines, "\n")
}

// JobDisplayName derives a short, human label from a config file path.
func JobDisplayName(configPath string) string {
	base := filepath.Base(configPath)
	return strings.TrimSuffix(base, filepath.Ext(base))
}

// MenuTime renders a timestamp for the menu, or "never" when it is unset.
func MenuTime(t time.Time) string {
	if t.IsZero() {
		return "never"
	}
	return t.Format("2006-01-02 15:04")
}
