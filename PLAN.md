# sftpSync — Implementation Plan

## Goal

A Go daemon that watches an SFTP server for new/changed images and syncs them to a local directory. Initially headless (CLI/daemon), with clear seams for adding a Mac menu bar UI later.

## Deployment Targets

- **Phase 1:** Headless daemon — runs in terminal or as a system service / Docker container
- **Phase 2 (future):** Mac menu bar app wrapping the same core daemon

## Architecture

```
sftpSync/
├── cmd/sftpsync/main.go         # entrypoint: parse flags, load config, run daemon loop
├── internal/
│   ├── config/config.go         # YAML config loading & validation
│   ├── sftp/client.go           # SFTP connection management, remote directory walking
│   ├── sync/syncer.go           # diff logic, download orchestration (bounded goroutine pool)
│   └── state/manifest.go        # local state persistence (JSON file tracking path→mtime/size)
├── config.yaml.example          # documented example config
└── go.mod
```

## Key Dependencies

| Package | Purpose |
|---|---|
| `github.com/pkg/sftp` | SFTP client |
| `golang.org/x/crypto/ssh` | SSH transport |
| `gopkg.in/yaml.v3` | Config file parsing |

UI phase (future):
| `github.com/getlantern/systray` | Mac menu bar integration |

## Core Daemon Loop

1. Load config (SFTP host/creds, local path, poll interval, file filters)
2. Connect to SFTP server (with reconnect on error)
3. Walk remote directory → build current snapshot `{path → mtime, size}`
4. Diff against local manifest
5. Download new/changed files concurrently (bounded goroutine pool, configurable workers)
6. Write files to local directory (preserve relative paths)
7. Update manifest
8. Sleep for poll interval, repeat

## Config File (YAML)

```yaml
sftp:
  host: photos.example.com
  port: 22
  user: myuser
  # one of: password, key_path, or ssh-agent
  key_path: ~/.ssh/id_rsa
  remote_path: /photos

local_path: ~/Pictures/sftp-sync

sync:
  interval: 60s          # poll interval
  workers: 4             # concurrent download goroutines
  extensions:            # optional filter; empty = all files
    - .jpg
    - .jpeg
    - .png
    - .raw
    - .heic
```

## State Manifest

A JSON file (e.g. `~/.local/share/sftpsync/manifest.json`) mapping remote paths to last-seen mtime and size. On each poll, entries missing locally or with changed mtime/size are queued for download. No hashing on the hot path — only fall back to hash comparison if mtime is unreliable.

## Change Detection Strategy

SFTP has no push notifications. Strategy:
- **Primary:** Compare remote `mtime` + `size` against manifest
- **Fallback:** If server mtimes are unreliable, add optional SHA-256 hash comparison (config flag)
- **New files:** Any remote path not in manifest is downloaded unconditionally

## Error Handling

- SFTP connection errors: exponential backoff, reconnect, continue loop
- Individual file download errors: log and skip, retry on next poll
- Manifest write errors: log warning, continue (worst case: re-download on next run)

## Phase 2: Mac Menu Bar UI

The `sync` package exposes a simple interface:
```go
type Syncer interface {
    Start(ctx context.Context) error
    Stop()
    Status() SyncStatus   // last sync time, file count, errors
}
```

The menu bar app will:
- Instantiate and start the syncer
- Show last sync time, file count in menu
- Allow pause/resume and manual trigger
- Use `github.com/getlantern/systray`

## Implementation Order

- [ ] `go.mod` + project scaffold
- [ ] Config loading (`internal/config`)
- [ ] SFTP client with directory walking (`internal/sftp`)
- [ ] State manifest (`internal/state`)
- [ ] Sync/diff + download logic (`internal/sync`)
- [ ] Daemon loop + signal handling (`cmd/sftpsync`)
- [ ] `config.yaml.example`
- [ ] Docker support (`Dockerfile`, documentation)
- [ ] (Future) Mac menu bar UI
