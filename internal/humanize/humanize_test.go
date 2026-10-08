package humanize

import "testing"

func TestBytes(t *testing.T) {
	tests := []struct {
		name string
		n    int64
		want string
	}{
		{"zero", 0, "0 B"},
		{"negative", -1, "-1 B"},
		{"just under a kibibyte", 1023, "1023 B"},
		{"exactly a kibibyte", 1024, "1.0 KB"},
		{"fractional kibibyte", 1536, "1.5 KB"},
		{"mebibyte", 1024 * 1024, "1.0 MB"},
		{"gibibyte", 3 * 1024 * 1024 * 1024, "3.0 GB"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Bytes(tt.n); got != tt.want {
				t.Fatalf("Bytes(%d) = %q, want %q", tt.n, got, tt.want)
			}
		})
	}
}

func TestPercent(t *testing.T) {
	tests := []struct {
		name      string
		completed int64
		total     int64
		want      int
		wantOK    bool
	}{
		{name: "unknown total", completed: 0, total: 0, wantOK: false},
		{name: "negative total", completed: 0, total: -5, wantOK: false},
		{name: "nothing done", completed: 0, total: 100, want: 0, wantOK: true},
		{name: "partial", completed: 18, total: 64, want: 28, wantOK: true},
		{name: "complete", completed: 64, total: 64, want: 100, wantOK: true},
		{name: "overshoot is clamped", completed: 4096, total: 1024, want: 100, wantOK: true},
		{name: "negative completed is clamped", completed: -10, total: 100, want: 0, wantOK: true},
		{name: "rounds down", completed: 1, total: 3, want: 33, wantOK: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := Percent(tt.completed, tt.total)
			if ok != tt.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tt.wantOK)
			}
			if got != tt.want {
				t.Fatalf("Percent(%d, %d) = %d, want %d", tt.completed, tt.total, got, tt.want)
			}
		})
	}
}

func TestPercentNeverExceedsOneHundred(t *testing.T) {
	for _, completed := range []int64{0, 1, 50, 99, 100, 101, 1 << 40} {
		for _, total := range []int64{1, 99, 100, 1 << 40} {
			got, ok := Percent(completed, total)
			if !ok {
				t.Fatalf("Percent(%d, %d) reported no percentage", completed, total)
			}
			if got < 0 || got > 100 {
				t.Fatalf("Percent(%d, %d) = %d, want 0..100", completed, total, got)
			}
		}
	}
}

func TestBytePair(t *testing.T) {
	tests := []struct {
		name      string
		completed int64
		total     int64
		want      string
	}{
		{name: "unknown total", completed: 0, total: 0, want: "-"},
		{name: "nothing yet", completed: 0, total: 2048, want: "0 B of 2.0 KB (0%)"},
		{name: "partial progress", completed: 250, total: 1000, want: "250 B of 1000 B (25%)"},
		{name: "complete", completed: 1000, total: 1000, want: "1000 B of 1000 B (100%)"},
		{name: "overshoot is clamped", completed: 4000, total: 1000, want: "1000 B of 1000 B (100%)"},
		{name: "negative is clamped", completed: -5, total: 1000, want: "0 B of 1000 B (0%)"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := BytePair(tt.completed, tt.total); got != tt.want {
				t.Fatalf("BytePair(%d, %d) = %q, want %q", tt.completed, tt.total, got, tt.want)
			}
		})
	}
}

func TestPhase(t *testing.T) {
	tests := []struct {
		name   string
		phase  string
		paused bool
		want   string
	}{
		{"unreported phase", "", false, "idle"},
		{"idle", "idle", false, "idle"},
		{"scanning", "scanning", false, "scanning"},
		{"downloading", "downloading", false, "downloading"},
		{"settled pause", "paused", true, "paused"},
		{"draining a batch", "downloading", true, "downloading (paused)"},
		{"failing while paused", "error", true, "error (paused)"},
		{"error while active", "error", false, "error"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Phase(tt.phase, tt.paused); got != tt.want {
				t.Fatalf("Phase(%q, %v) = %q, want %q", tt.phase, tt.paused, got, tt.want)
			}
		})
	}
}
