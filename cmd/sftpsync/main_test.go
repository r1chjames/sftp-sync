package main

import (
	"testing"

	"github.com/r1chjames/sftp-sync/internal/daemon"
)

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
