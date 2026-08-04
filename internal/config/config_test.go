package config

import (
	"strings"
	"testing"
	"time"
)

func baseConfig() *Config {
	return &Config{
		SFTP: SFTPConfig{
			Host:       "example.com",
			Port:       22,
			User:       "test",
			RemotePath: "/photos",
		},
		LocalPath: "/tmp/sync",
		Sync: SyncConfig{
			Interval: 60 * time.Second,
			Workers:  4,
		},
	}
}

func TestValidate_FolderStructure(t *testing.T) {
	tests := []struct {
		name          string
		folder        string
		wantDefault   string
		wantErrSubstr string
	}{
		{name: "none", folder: "none", wantDefault: "none"},
		{name: "year", folder: "year", wantDefault: "year"},
		{name: "year_month", folder: "year_month", wantDefault: "year_month"},
		{name: "year_month_day", folder: "year_month_day", wantDefault: "year_month_day"},
		{name: "empty defaults to none", folder: "", wantDefault: "none"},
		{name: "invalid value", folder: "daily", wantErrSubstr: "must be one of"},
		{name: "case sensitive", folder: "YEAR", wantErrSubstr: "must be one of"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := baseConfig()
			cfg.Sync.FolderStructure = tt.folder
			err := cfg.validate()

			if tt.wantErrSubstr != "" {
				if err == nil {
					t.Fatalf("expected error containing %q, got nil", tt.wantErrSubstr)
				}
				if !strings.Contains(err.Error(), tt.wantErrSubstr) {
					t.Fatalf("expected error containing %q, got %v", tt.wantErrSubstr, err)
				}
				return
			}

			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if cfg.Sync.FolderStructure != tt.wantDefault {
				t.Fatalf("FolderStructure = %q, want %q", cfg.Sync.FolderStructure, tt.wantDefault)
			}
		})
	}
}
