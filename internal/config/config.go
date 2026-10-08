package config

import (
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

type SFTPConfig struct {
	Host                  string `yaml:"host"`
	Port                  int    `yaml:"port"`
	User                  string `yaml:"user"`
	Password              string `yaml:"password"`
	KeyPath               string `yaml:"key_path"`
	RemotePath            string `yaml:"remote_path"`
	InsecureIgnoreHostKey bool   `yaml:"insecure_ignore_host_key"`
}

// Collision policies, applied when the destination for a downloaded file is
// already occupied by something else.
const (
	// CollisionError fails the file.
	CollisionError = "error"
	// CollisionSkip leaves the existing file alone and records a skip.
	CollisionSkip = "skip"
	// CollisionRename writes IMG_0001-2.JPG beside an existing IMG_0001.JPG.
	CollisionRename = "rename"
)

// Verification modes, deciding how far the daemon goes to be sure that a local
// file and the remote file it represents are the same file.
const (
	// VerifySize trusts the manifest's modification time and size.
	VerifySize = "size"
	// VerifySHA256 compares content hashes, at the cost of reading the remote
	// file again.
	VerifySHA256 = "sha256"
)

type SyncConfig struct {
	Interval        time.Duration `yaml:"interval"`
	Workers         int           `yaml:"workers"`
	Extensions      []string      `yaml:"extensions"`
	FolderStructure string        `yaml:"folder_structure"` // "none", "year", "year_month", or "year_month_day"
	CollisionPolicy string        `yaml:"collision_policy"` // "error", "skip", or "rename"
	MaxAttempts     int           `yaml:"max_attempts"`     // per-file transfer attempts, including the first
	Verify          string        `yaml:"verify"`           // "size" or "sha256"
}

type Config struct {
	SFTP      SFTPConfig `yaml:"sftp"`
	LocalPath string     `yaml:"local_path"`
	Sync      SyncConfig `yaml:"sync"`
	StatePath string     `yaml:"state_path"`
}

func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading config: %w", err)
	}

	cfg := &Config{
		SFTP: SFTPConfig{Port: 22},
		Sync: SyncConfig{
			Interval: 60 * time.Second,
			Workers:  4,
		},
	}

	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("parsing config: %w", err)
	}

	cfg.LocalPath = ExpandHome(cfg.LocalPath)
	cfg.SFTP.KeyPath = ExpandHome(cfg.SFTP.KeyPath)
	if cfg.StatePath == "" {
		cfg.StatePath = ExpandHome("~/.local/share/sftpsync/manifest.json")
	} else {
		cfg.StatePath = ExpandHome(cfg.StatePath)
	}

	return cfg, cfg.validate()
}

func (c *Config) validate() error {
	if c.SFTP.Host == "" {
		return fmt.Errorf("sftp.host is required")
	}
	if c.SFTP.User == "" {
		return fmt.Errorf("sftp.user is required")
	}
	if c.SFTP.RemotePath == "" {
		return fmt.Errorf("sftp.remote_path is required")
	}
	if c.LocalPath == "" {
		return fmt.Errorf("local_path is required")
	}
	if c.Sync.Workers <= 0 {
		c.Sync.Workers = 4
	}
	if c.Sync.Interval <= 0 {
		return fmt.Errorf("sync.interval must be greater than zero")
	}
	if c.Sync.FolderStructure == "" {
		c.Sync.FolderStructure = "none"
	}
	switch c.Sync.FolderStructure {
	case "none", "year", "year_month", "year_month_day":
	default:
		return fmt.Errorf("sync.folder_structure must be one of: none, year, year_month, year_month_day")
	}
	if c.Sync.CollisionPolicy == "" {
		// Renaming is the only policy that cannot lose a photo: it neither
		// overwrites an unrelated file nor leaves one unsynced, so it is the
		// default for configs that predate the option.
		c.Sync.CollisionPolicy = CollisionRename
	}
	switch c.Sync.CollisionPolicy {
	case CollisionError, CollisionSkip, CollisionRename:
	default:
		return fmt.Errorf("sync.collision_policy must be one of: %s, %s, %s",
			CollisionError, CollisionSkip, CollisionRename)
	}
	if c.Sync.MaxAttempts == 0 {
		// Three attempts survives a brief interruption without holding a worker
		// on a dead server for long.
		c.Sync.MaxAttempts = 3
	}
	if c.Sync.MaxAttempts < 1 || c.Sync.MaxAttempts > 10 {
		return fmt.Errorf("sync.max_attempts must be between 1 and 10")
	}
	if c.Sync.Verify == "" {
		// Trusting the manifest's modification time and size costs nothing extra
		// and is what every sync before this option did.
		c.Sync.Verify = VerifySize
	}
	switch c.Sync.Verify {
	case VerifySize, VerifySHA256:
	default:
		return fmt.Errorf("sync.verify must be one of: %s, %s", VerifySize, VerifySHA256)
	}
	return nil
}

// ExpandHome replaces a leading ~ with the current user's home directory.
func ExpandHome(path string) string {
	if len(path) == 0 || path[0] != '~' {
		return path
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return path
	}
	return home + path[1:]
}
