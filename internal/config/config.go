package config

import (
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

type SFTPConfig struct {
	Host                 string `yaml:"host"`
	Port                 int    `yaml:"port"`
	User                 string `yaml:"user"`
	Password             string `yaml:"password"`
	KeyPath              string `yaml:"key_path"`
	RemotePath           string `yaml:"remote_path"`
	InsecureIgnoreHostKey bool  `yaml:"insecure_ignore_host_key"`
}

type SyncConfig struct {
	Interval   time.Duration `yaml:"interval"`
	Workers    int           `yaml:"workers"`
	Extensions []string      `yaml:"extensions"`
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
