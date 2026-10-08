// Package humanize renders job status values for people: byte sizes,
// percentages, and phase labels.
//
// It is deliberately free of dependencies beyond the standard library, and in
// particular does not import the daemon or syncer types, so both the CLI and
// the menu-bar app share one implementation and one set of clamping rules.
package humanize

import "fmt"

// Bytes renders a byte count with a binary unit prefix.
func Bytes(n int64) string {
	if n < 1024 {
		return fmt.Sprintf("%d B", n)
	}
	units := []string{"KB", "MB", "GB", "TB", "PB"}
	value := float64(n)
	i := -1
	for value >= 1024 && i < len(units)-1 {
		value /= 1024
		i++
	}
	return fmt.Sprintf("%.1f %s", value, units[i])
}

// Percent returns completion as a whole percentage between 0 and 100. ok is
// false when the total is unknown, so callers omit the percentage rather than
// showing a meaningless zero or dividing by zero.
//
// Completed bytes are clamped, so a remote file that grew mid-transfer cannot
// report more than 100%.
func Percent(completed, total int64) (int, bool) {
	if total <= 0 {
		return 0, false
	}
	if completed < 0 {
		completed = 0
	}
	if completed > total {
		completed = total
	}
	return int(completed * 100 / total), true
}

// BytePair renders progress as a percentage of a total, or "-" when the total
// is not yet known.
func BytePair(completed, total int64) string {
	if total <= 0 {
		return "-"
	}
	percent, _ := Percent(completed, total)
	clamped := completed
	if clamped < 0 {
		clamped = 0
	}
	if clamped > total {
		clamped = total
	}
	return fmt.Sprintf("%s of %s (%d%%)", Bytes(clamped), Bytes(total), percent)
}

// Phase renders a job's phase, or "idle" when it has not reported one yet.
//
// The paused state is annotated alongside the phase rather than replacing it,
// because a paused job that is still draining a batch genuinely reports the
// downloading phase.
func Phase(phase string, paused bool) string {
	if phase == "" {
		phase = "idle"
	}
	if paused && phase != "paused" {
		return phase + " (paused)"
	}
	return phase
}
