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
}

// JobSlotView renders a job for the menu.
func JobSlotView(j daemon.JobResponse) SlotView {
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
	return view
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
// and never exceeds the batch size.
func attemptedFiles(st daemon.StatusResponse) int {
	attempted := st.Completed + st.Failed
	if attempted < 0 {
		return 0
	}
	if attempted > st.BatchTotal {
		return st.BatchTotal
	}
	return attempted
}

// MenuTitle returns the text shown next to the menu-bar icon: the aggregate
// percentage of every job currently downloading, and a warning marker when any
// job has failed. Summing only downloading jobs keeps the percentage honest,
// because a finished job's byte counters describe its last batch, not the work
// in progress.
func MenuTitle(jobs []daemon.JobResponse) string {
	var (
		completed int64
		total     int64
		failed    bool
	)

	for _, j := range jobs {
		if j.Status.LastError != "" {
			failed = true
		}
		if j.Status.Phase != "downloading" {
			continue
		}
		completed += j.Status.BytesCompleted
		total += j.Status.BytesTotal
	}

	parts := make([]string, 0, 2)
	if percent, ok := humanize.Percent(completed, total); ok {
		parts = append(parts, fmt.Sprintf("%d%%", percent))
	}
	if failed {
		parts = append(parts, "⚠")
	}
	return strings.Join(parts, " ")
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
