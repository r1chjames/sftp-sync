package config

import (
	"os"
	"path/filepath"
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

func TestValidate_Interval(t *testing.T) {
	tests := []struct {
		name    string
		value   time.Duration
		wantErr bool
	}{
		{name: "positive interval is accepted", value: time.Minute},
		{name: "zero interval is rejected", value: 0, wantErr: true},
		{name: "negative interval is rejected", value: -time.Second, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := baseConfig()
			cfg.Sync.Interval = tt.value
			err := cfg.validate()
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected an error, got nil")
				}
				if !strings.Contains(err.Error(), "sync.interval") {
					t.Fatalf("error = %v, want sync.interval", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func TestLoadDefaultsInterval(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	contents := "sftp:\n  host: example.com\n  user: test\n  remote_path: /photos\nlocal_path: /tmp/sync\n"
	if err := os.WriteFile(path, []byte(contents), 0644); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Sync.Interval != 60*time.Second {
		t.Fatalf("interval = %v, want 60s", cfg.Sync.Interval)
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

func TestCollisionPolicyDefaultAndValidation(t *testing.T) {
	tests := []struct {
		name    string
		policy  string
		want    string
		wantErr bool
	}{
		{name: "unspecified defaults to rename", policy: "", want: CollisionRename},
		{name: "error", policy: "error", want: CollisionError},
		{name: "skip", policy: "skip", want: CollisionSkip},
		{name: "rename", policy: "rename", want: CollisionRename},
		{name: "unknown is rejected", policy: "overwrite", wantErr: true},
		{name: "case matters", policy: "Rename", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := baseConfig()
			cfg.Sync.CollisionPolicy = tt.policy

			err := cfg.validate()

			if tt.wantErr {
				if err == nil {
					t.Fatalf("validate() = nil, want an error for %q", tt.policy)
				}
				return
			}
			if err != nil {
				t.Fatalf("validate() error = %v", err)
			}
			if cfg.Sync.CollisionPolicy != tt.want {
				t.Fatalf("policy = %q, want %q", cfg.Sync.CollisionPolicy, tt.want)
			}
		})
	}
}
