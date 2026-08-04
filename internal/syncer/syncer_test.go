package syncer

import (
	"testing"
	"time"

	"github.com/r1chjames/sftp-sync/internal/config"
)

func TestLocalPath(t *testing.T) {
	s := &Syncer{
		cfg: &config.Config{
			LocalPath: "/output",
			SFTP:      config.SFTPConfig{RemotePath: "/photos"},
		},
	}

	tests := []struct {
		name       string
		remotePath string
		want       string
	}{
		{"strips prefix", "/photos/vacation/IMG_001.jpg", "/output/vacation/IMG_001.jpg"},
		{"top-level file", "/photos/IMG_001.jpg", "/output/IMG_001.jpg"},
		{"deep nesting", "/photos/2024/06/15/IMG_001.jpg", "/output/2024/06/15/IMG_001.jpg"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := s.localPath(tt.remotePath)
			if got != tt.want {
				t.Fatalf("localPath(%q) = %q, want %q", tt.remotePath, got, tt.want)
			}
		})
	}
}

func TestDatePath(t *testing.T) {
	date := time.Date(2024, 6, 15, 14, 30, 0, 0, time.UTC)
	remotePath := "/photos/vacation/IMG_001.jpg"

	tests := []struct {
		name            string
		folderStructure string
		want            string
	}{
		{"none falls back to remote structure", "none", "/output/vacation/IMG_001.jpg"},
		{"year only", "year", "/output/2024/IMG_001.jpg"},
		{"year/month", "year_month", "/output/2024/06/IMG_001.jpg"},
		{"year/month/day", "year_month_day", "/output/2024/06/15/IMG_001.jpg"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := &Syncer{
				cfg: &config.Config{
					LocalPath: "/output",
					SFTP:      config.SFTPConfig{RemotePath: "/photos"},
					Sync:      config.SyncConfig{FolderStructure: tt.folderStructure},
				},
			}

			got := s.datePath(remotePath, date)
			if got != tt.want {
				t.Fatalf("datePath(%q) = %q, want %q", remotePath, got, tt.want)
			}
		})
	}
}

func TestMatchesFilter(t *testing.T) {
	tests := []struct {
		name       string
		extensions []string
		path       string
		want       bool
	}{
		{"no filter accepts all", nil, "/photos/anything.raw", true},
		{"empty filter accepts all", []string{}, "/photos/anything.raw", true},
		{"matching extension", []string{".jpg", ".png"}, "/photos/photo.jpg", true},
		{"case insensitive match", []string{".jpg"}, "/photos/PHOTO.JPG", true},
		{"non-matching extension", []string{".jpg", ".png"}, "/photos/photo.raw", false},
		{"no extension matches nothing", []string{".jpg"}, "/photos/photo", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := &Syncer{
				cfg: &config.Config{
					Sync: config.SyncConfig{Extensions: tt.extensions},
				},
			}

			got := s.matchesFilter(tt.path)
			if got != tt.want {
				t.Fatalf("matchesFilter(%q) = %v, want %v", tt.path, got, tt.want)
			}
		})
	}
}
