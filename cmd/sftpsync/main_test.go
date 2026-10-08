package main

import (
	"testing"

	"github.com/r1chjames/sftp-sync/internal/daemon"
)

func TestFormatBytes(t *testing.T) {
	tests := []struct {
		name string
		n    int64
		want string
	}{
		{"zero", 0, "0 B"},
		{"just under a kibibyte", 1023, "1023 B"},
		{"exactly a kibibyte", 1024, "1.0 KB"},
		{"fractional kibibyte", 1536, "1.5 KB"},
		{"mebibyte", 1024 * 1024, "1.0 MB"},
		{"gibibyte", 3 * 1024 * 1024 * 1024, "3.0 GB"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := formatBytes(tt.n); got != tt.want {
				t.Fatalf("formatBytes(%d) = %q, want %q", tt.n, got, tt.want)
			}
		})
	}
}

func TestFormatByteProgress(t *testing.T) {
	tests := []struct {
		name   string
		status daemon.StatusResponse
		want   string
	}{
		{name: "unknown total", status: daemon.StatusResponse{}, want: "-"},
		{
			name:   "partial progress",
			status: daemon.StatusResponse{BytesTotal: 1000, BytesCompleted: 250},
			want:   "250 B of 1000 B (25%)",
		},
		{
			name:   "complete",
			status: daemon.StatusResponse{BytesTotal: 1000, BytesCompleted: 1000},
			want:   "1000 B of 1000 B (100%)",
		},
		{
			name:   "overshoot is clamped",
			status: daemon.StatusResponse{BytesTotal: 1000, BytesCompleted: 4000},
			want:   "1000 B of 1000 B (100%)",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := formatByteProgress(tt.status); got != tt.want {
				t.Fatalf("formatByteProgress() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestFormatBatch(t *testing.T) {
	tests := []struct {
		name   string
		status daemon.StatusResponse
		want   string
	}{
		{name: "no batch", status: daemon.StatusResponse{}, want: "-"},
		{
			name: "active batch",
			status: daemon.StatusResponse{
				BatchTotal: 5,
				Completed:  2,
				Failed:     1,
				Remaining:  2,
			},
			want: "2/5 complete, 1 failed, 2 remaining",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := formatBatch(tt.status); got != tt.want {
				t.Fatalf("formatBatch() = %q, want %q", got, tt.want)
			}
		})
	}
}
