package menubar

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/r1chjames/sftp-sync/internal/daemon"
)

func job(id string, status daemon.StatusResponse) daemon.JobResponse {
	return daemon.JobResponse{ID: id, ConfigPath: "/home/me/" + id + ".yaml", Status: status}
}

func TestRefreshIntervalWhileActive(t *testing.T) {
	for _, phase := range []string{"scanning", "downloading"} {
		t.Run(phase, func(t *testing.T) {
			jobs := []daemon.JobResponse{
				job("aaaaaaaa", daemon.StatusResponse{Phase: "idle"}),
				job("bbbbbbbb", daemon.StatusResponse{Phase: phase}),
			}
			if got := RefreshInterval(jobs, nil); got != activeRefreshInterval {
				t.Fatalf("interval = %v, want %v", got, activeRefreshInterval)
			}
		})
	}
}

func TestRefreshIntervalWhileInactive(t *testing.T) {
	tests := []struct {
		name string
		jobs []daemon.JobResponse
	}{
		{name: "no jobs"},
		{name: "idle", jobs: []daemon.JobResponse{job("aaaaaaaa", daemon.StatusResponse{Phase: "idle"})}},
		{name: "paused", jobs: []daemon.JobResponse{job("aaaaaaaa", daemon.StatusResponse{Phase: "paused", Paused: true})}},
		{name: "error", jobs: []daemon.JobResponse{job("aaaaaaaa", daemon.StatusResponse{Phase: "error", LastError: "boom"})}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := RefreshInterval(tt.jobs, nil); got != idleRefreshInterval {
				t.Fatalf("interval = %v, want %v", got, idleRefreshInterval)
			}
		})
	}
}

// TestRefreshIntervalWhilePausedButDraining covers the one case where the
// paused flag and the phase disagree. Files already copying are still moving,
// so the menu keeps polling quickly until the phase settles on paused; that is
// what makes the settled state appear promptly rather than up to 30 seconds
// later.
func TestRefreshIntervalWhilePausedButDraining(t *testing.T) {
	jobs := []daemon.JobResponse{job("aaaaaaaa", daemon.StatusResponse{
		Phase: "downloading", Paused: true, BatchTotal: 4, Completed: 1,
	})}

	if got := RefreshInterval(jobs, nil); got != activeRefreshInterval {
		t.Fatalf("interval = %v, want %v while a batch is still draining", got, activeRefreshInterval)
	}
}

func TestRefreshIntervalWhileDaemonUnreachable(t *testing.T) {
	if got := RefreshInterval(nil, errors.New("dial unix: no such file or directory")); got != activeRefreshInterval {
		t.Fatalf("interval = %v, want %v so the menu recovers promptly", got, activeRefreshInterval)
	}
}

// TestActiveRefreshIntervalMeetsStalenessBound pins the acceptance criterion:
// opening the menu during a transfer must show status no more than two seconds
// old, which a one-second poll interval satisfies.
func TestActiveRefreshIntervalMeetsStalenessBound(t *testing.T) {
	if activeRefreshInterval > 2*time.Second {
		t.Fatalf("active interval = %v, want at most 2s", activeRefreshInterval)
	}
	if idleRefreshInterval < 10*time.Second {
		t.Fatalf("idle interval = %v, want a low-frequency poll", idleRefreshInterval)
	}
}

func TestJobSlotViewWithoutABatchYet(t *testing.T) {
	view := JobSlotView(job("photos", daemon.StatusResponse{Phase: "idle", FilesTotal: 3}), "")

	if view.Header != "● photos" {
		t.Fatalf("header = %q", view.Header)
	}
	if view.State != "  idle · 3 files" {
		t.Fatalf("state = %q", view.State)
	}
	if view.LastSuccess != "  Last success: never" {
		t.Fatalf("last success = %q", view.LastSuccess)
	}
	if view.CurrentFile != "" {
		t.Fatalf("current file = %q, want no row", view.CurrentFile)
	}
	if view.Error != "" {
		t.Fatalf("error = %q, want no row", view.Error)
	}
}

func TestJobSlotViewWhileDownloading(t *testing.T) {
	view := JobSlotView(job("photos", daemon.StatusResponse{
		Phase:                     "downloading",
		FilesTotal:                64,
		BatchTotal:                64,
		Completed:                 18,
		Remaining:                 46,
		BytesTotal:                5 * 1024 * 1024,
		BytesCompleted:            1476395,
		CurrentFile:               "/photos/2024/IMG_0042.CR3",
		CurrentFileBytesTotal:     3276800,
		CurrentFileBytesCompleted: 921600,
		LastSuccessfulSync:        time.Date(2024, 6, 15, 11, 0, 0, 0, time.UTC),
	}), "")

	if view.Header != "⟳ photos" {
		t.Fatalf("header = %q", view.Header)
	}
	if view.State != "  downloading · 18 of 64 files · 28%" {
		t.Fatalf("state = %q", view.State)
	}
	if view.CurrentFile != "  IMG_0042.CR3 — 900.0 KB of 3.1 MB (28%)" {
		t.Fatalf("current file = %q", view.CurrentFile)
	}
	if view.LastSuccess != "  Last success: 2024-06-15 11:00" {
		t.Fatalf("last success = %q", view.LastSuccess)
	}
	if view.Error != "" {
		t.Fatalf("error = %q, want no row", view.Error)
	}
}

func TestJobSlotViewIncludesFailuresInProgress(t *testing.T) {
	view := JobSlotView(job("photos", daemon.StatusResponse{
		Phase:      "downloading",
		BatchTotal: 10,
		Completed:  7,
		Failed:     2,
		Remaining:  1,
	}), "")

	if !strings.Contains(view.State, "9 of 10 files") {
		t.Fatalf("state = %q, want failed files counted as attempted", view.State)
	}
}

func TestJobSlotViewOnCompletion(t *testing.T) {
	view := JobSlotView(job("photos", daemon.StatusResponse{
		Phase:              "idle",
		FilesTotal:         64,
		BatchTotal:         64,
		Completed:          64,
		BytesTotal:         1024,
		BytesCompleted:     1024,
		LastSuccessfulSync: time.Date(2024, 6, 15, 11, 0, 0, 0, time.UTC),
	}), "")

	if view.Header != "● photos" {
		t.Fatalf("header = %q", view.Header)
	}
	if view.State != "  idle · 64 files · 100%" {
		t.Fatalf("state = %q", view.State)
	}
	if view.CurrentFile != "" {
		t.Fatalf("current file = %q, want no row after completion", view.CurrentFile)
	}
}

func TestJobSlotViewWhilePaused(t *testing.T) {
	// Settled: the phase is paused and nothing is moving.
	settled := JobSlotView(job("photos", daemon.StatusResponse{Phase: "paused", Paused: true, FilesTotal: 64}), "")
	if settled.Header != "⏸ photos" {
		t.Fatalf("header = %q", settled.Header)
	}
	if settled.State != "  paused · 64 files" {
		t.Fatalf("state = %q", settled.State)
	}

	// Draining: a pause was requested while a batch is still finishing.
	draining := JobSlotView(job("photos", daemon.StatusResponse{
		Phase: "downloading", Paused: true, BatchTotal: 4, Completed: 1, BytesTotal: 100, BytesCompleted: 25,
	}), "")
	if draining.Header != "⏸ photos" {
		t.Fatalf("header = %q", draining.Header)
	}
	if draining.State != "  downloading (paused) · 1 of 4 files · 25%" {
		t.Fatalf("state = %q", draining.State)
	}
}

func TestJobSlotViewWithNoTotals(t *testing.T) {
	// A job that has never completed a batch must not render "0 of 0" or 0%.
	view := JobSlotView(job("photos", daemon.StatusResponse{Phase: "scanning"}), "")

	if view.State != "  scanning" {
		t.Fatalf("state = %q", view.State)
	}
	if strings.Contains(view.State, "%") || strings.Contains(view.State, "of") {
		t.Fatalf("state = %q, want no percentage or file ratio", view.State)
	}
}

func TestJobSlotViewWithError(t *testing.T) {
	full := "connect: auth: read key file: open /home/me/.ssh/id_ed25519: no such file or directory"
	view := JobSlotView(job("photos", daemon.StatusResponse{
		Phase:              "error",
		FilesTotal:         64,
		LastError:          full,
		LastSuccessfulSync: time.Date(2024, 6, 15, 11, 0, 0, 0, time.UTC),
	}), "")

	if view.Header != "⚠ photos" {
		t.Fatalf("header = %q", view.Header)
	}
	if view.State != "  error · 64 files" {
		t.Fatalf("state = %q", view.State)
	}
	if view.Error != "  ⚠ "+full {
		t.Fatalf("error = %q, want the full text untruncated", view.Error)
	}
}

func TestMenuTitleAggregatesOnlyActiveDownloads(t *testing.T) {
	tests := []struct {
		name string
		jobs []daemon.JobResponse
		want string
	}{
		{name: "no jobs", want: ""},
		{
			name: "idle job keeps the icon alone",
			jobs: []daemon.JobResponse{job("a", daemon.StatusResponse{
				Phase: "idle", BytesTotal: 1000, BytesCompleted: 1000,
			})},
			want: "",
		},
		{
			name: "single download",
			jobs: []daemon.JobResponse{job("a", daemon.StatusResponse{
				Phase: "downloading", BytesTotal: 1000, BytesCompleted: 250,
			})},
			want: "25%",
		},
		{
			name: "two downloads are summed",
			jobs: []daemon.JobResponse{
				job("a", daemon.StatusResponse{Phase: "downloading", BytesTotal: 1000, BytesCompleted: 500}),
				job("b", daemon.StatusResponse{Phase: "downloading", BytesTotal: 3000, BytesCompleted: 500}),
			},
			want: "25%",
		},
		{
			name: "a completed job does not dilute the active total",
			jobs: []daemon.JobResponse{
				job("a", daemon.StatusResponse{Phase: "downloading", BytesTotal: 1000, BytesCompleted: 500}),
				job("b", daemon.StatusResponse{Phase: "idle", BytesTotal: 1000, BytesCompleted: 1000}),
			},
			want: "50%",
		},
		{
			name: "unknown totals stay unreported",
			jobs: []daemon.JobResponse{job("a", daemon.StatusResponse{Phase: "downloading"})},
			want: "",
		},
		{
			name: "failure marker",
			jobs: []daemon.JobResponse{job("a", daemon.StatusResponse{Phase: "error", LastError: "boom"})},
			want: "⚠",
		},
		{
			name: "failure marker beside progress",
			jobs: []daemon.JobResponse{
				job("a", daemon.StatusResponse{Phase: "downloading", BytesTotal: 100, BytesCompleted: 10}),
				job("b", daemon.StatusResponse{Phase: "error", LastError: "boom"}),
			},
			want: "10% ⚠",
		},
		{
			name: "overshoot cannot exceed one hundred percent",
			jobs: []daemon.JobResponse{job("a", daemon.StatusResponse{
				Phase: "downloading", BytesTotal: 100, BytesCompleted: 9999,
			})},
			want: "100%",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := MenuTitle(tt.jobs); got != tt.want {
				t.Fatalf("MenuTitle() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestJobDisplayName(t *testing.T) {
	tests := map[string]string{
		"/home/me/photos.yaml":            "photos",
		"/home/me/photos.yml":             "photos",
		"/home/me/photos":                 "photos",
		"/home/me/archive.photos.yaml":    "archive.photos",
		"relative/path/to/nested.yml":     "nested",
		"/home/me/.hidden.yaml":           ".hidden",
		"/home/me/dir.with.dots/cfg.yaml": "cfg",
	}

	for path, want := range tests {
		if got := JobDisplayName(path); got != want {
			t.Fatalf("JobDisplayName(%q) = %q, want %q", path, got, want)
		}
	}
}

func TestStatusPrefix(t *testing.T) {
	tests := []struct {
		name   string
		status daemon.StatusResponse
		want   string
	}{
		{"idle", daemon.StatusResponse{Phase: "idle"}, "●"},
		{"scanning", daemon.StatusResponse{Phase: "scanning"}, "⟳"},
		{"downloading", daemon.StatusResponse{Phase: "downloading"}, "⟳"},
		{"paused", daemon.StatusResponse{Phase: "paused", Paused: true}, "⏸"},
		{"paused while draining", daemon.StatusResponse{Phase: "downloading", Paused: true}, "⏸"},
		{"error", daemon.StatusResponse{Phase: "error", LastError: "boom"}, "⚠"},
		{"error while paused is still an error", daemon.StatusResponse{Phase: "error", Paused: true, LastError: "boom"}, "⚠"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := StatusPrefix(tt.status); got != tt.want {
				t.Fatalf("StatusPrefix() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestSlotControlsFor(t *testing.T) {
	active := SlotControlsFor(false)
	if !active.ShowPause || active.ShowResume {
		t.Fatalf("active controls = %+v, want Pause only", active)
	}

	paused := SlotControlsFor(true)
	if paused.ShowPause || !paused.ShowResume {
		t.Fatalf("paused controls = %+v, want Resume only", paused)
	}
}

func TestJobSlotViewShowsActionFailureBesideTheSyncError(t *testing.T) {
	view := JobSlotView(job("photos", daemon.StatusResponse{
		Phase:     "error",
		LastError: "connect: auth: permission denied",
	}), "pause failed: persist pause for job abc12345: permission denied")

	if view.Error != "  ⚠ connect: auth: permission denied" {
		t.Fatalf("error = %q, want the daemon's own error kept", view.Error)
	}
	if view.ActionError != "  ⚠ pause failed: persist pause for job abc12345: permission denied" {
		t.Fatalf("action error = %q", view.ActionError)
	}
}

func TestJobSlotViewWithoutActionFailure(t *testing.T) {
	view := JobSlotView(job("photos", daemon.StatusResponse{Phase: "idle"}), "")

	if view.ActionError != "" {
		t.Fatalf("action error = %q, want no row", view.ActionError)
	}
}

func TestActionFailure(t *testing.T) {
	if got := ActionFailure("pause", errors.New("daemon unreachable")); got != "pause failed: daemon unreachable" {
		t.Fatalf("ActionFailure() = %q", got)
	}
	if got := ActionFailure("resume", nil); got != "" {
		t.Fatalf("ActionFailure() with no error = %q, want empty", got)
	}
}

func TestAdditionalJobsNotice(t *testing.T) {
	tests := []struct {
		name  string
		total int
		shown int
		want  string
	}{
		{name: "nothing to report", total: 3, shown: 10, want: ""},
		{name: "exactly full", total: 10, shown: 10, want: ""},
		{name: "none at all", total: 0, shown: 10, want: ""},
		{name: "one hidden", total: 11, shown: 10, want: "1 additional job not shown"},
		{name: "several hidden", total: 14, shown: 10, want: "4 additional jobs not shown"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := AdditionalJobsNotice(tt.total, tt.shown); got != tt.want {
				t.Fatalf("AdditionalJobsNotice(%d, %d) = %q, want %q", tt.total, tt.shown, got, tt.want)
			}
		})
	}
}
