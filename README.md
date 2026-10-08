# sftpsync

A lightweight Go daemon that manages multiple SFTP sync jobs, each with its own configuration. A companion CLI submits and controls jobs over a Unix socket.

## Features

- Multiple independent sync jobs, each targeting a different SFTP server or path
- Jobs persist across daemon restarts, including paused state
- Pause and resume a job at any time without losing progress
- Sync on demand with `sftpsync sync <id>` instead of waiting for the interval
- Live per-job status: phase, file and byte progress, current file, last success, last error
- macOS menu-bar app with adaptive polling and an aggregate progress percentage
- Polls on a configurable interval per job (no SFTP push support required)
- Tracks remote file state via a local JSON manifest (mtime + size)
- Concurrent downloads with a bounded goroutine pool
- Atomic file writes (temp-file + rename — no partial files)
- SSH auth via password, private key, or ssh-agent
- Host key verification against `~/.ssh/known_hosts`
- File extension filtering (e.g. sync only `.jpg`, `.heic`, `.raw`)
- Graceful shutdown on SIGINT / SIGTERM

## Requirements

- macOS or Linux
- SSH access to an SFTP server

## Installation

### macOS / Linux (recommended)

```bash
curl -fsSL https://raw.githubusercontent.com/r1chjames/sftp-sync/main/install.sh | bash
```

This installs `sftpsyncd` and `sftpsync` to `/usr/local/bin`. On macOS it also installs `sftpsyncbar.app` to `/Applications` and registers a LaunchAgent so the daemon starts at login.

To install a specific version:

```bash
curl -fsSL https://raw.githubusercontent.com/r1chjames/sftp-sync/main/install.sh | bash -s v0.1.8
```

### Build from source

```bash
git clone https://github.com/r1chjames/sftp-sync
cd sftp-sync
go build -o sftpsyncd ./cmd/sftpsyncd   # daemon
go build -o sftpsync  ./cmd/sftpsync    # CLI client
```

## Quick start

Submit a sync job using a config file:

```bash
sftpsync add /path/to/config.yaml
```

List and manage jobs:

```bash
sftpsync list
sftpsync status
sftpsync status <id>
sftpsync sync <id>      # scan and download now
sftpsync pause <id>     # stop starting new work
sftpsync resume <id>    # clear the paused state and scan now
sftpsync remove <id>
sftpsync stop           # shut down the daemon
```

`sftpsync status <id>` reports the phase, whether the job is paused, file and
batch progress, byte progress with a percentage, the file being downloaded, the
last successful sync, and the latest error, for example:

```text
id:            abc12345
config:        /home/me/photos.yaml
phase:         downloading
paused:        no
last sync:     2024-06-15 12:00:05
last success:  2024-06-15 11:00:05
files:         64
eligible:      64
batch:         18/64 complete, 0 failed, 46 remaining
bytes:         1.4 MB of 5.0 MB (28%)
current file:  IMG_0042.CR3
current bytes: 900.0 KB of 3.1 MB (28%)
```

Commands exit `0` on success, `1` when the request fails, and `2` on an invalid
command line (for example a missing job ID).

### Job controls

The daemon accepts per-job control requests over the same Unix socket:

```text
POST /jobs/{id}/sync     # scan and download now instead of waiting for the interval
POST /jobs/{id}/pause    # stop starting new scans and downloads
POST /jobs/{id}/resume   # clear the paused state and scan immediately
```

Each returns `202 Accepted` with the job's updated status, or `404 Not Found`
if the job is unknown. Pausing and resuming are idempotent, and pausing does
not abort work already in flight: files being copied are committed atomically,
and files not yet started are picked up on resume.

Paused state is stored in the registry, so a paused job stays paused across a
daemon restart and never reconnects until it is resumed. If that state cannot
be written, the request fails and the job's runtime state is rolled back rather
than reported as persisted.

The Unix socket (mode `0600`) is the only trust boundary — the daemon does not
listen on TCP.

## Configuration

Each job is configured via its own YAML file. Copy the example and edit it:

```bash
cp config.yaml.example config.yaml
```

```yaml
sftp:
  host: photos.example.com
  port: 22
  user: myuser
  key_path: ~/.ssh/id_rsa   # or use password:, or leave both blank for ssh-agent
  remote_path: /photos

local_path: ~/Pictures/sftp-sync

sync:
  interval: 60s    # how often to poll
  workers: 4       # concurrent downloads
  extensions:      # omit to sync all files
    - .jpg
    - .jpeg
    - .png
    - .heic
    - .raw
```

You can have as many config files as you like — one per SFTP source — and submit them all to the same running daemon.

### Authentication

The following methods are tried in order:

1. **Password** — set `sftp.password`
2. **Private key** — set `sftp.key_path` (e.g. `~/.ssh/id_rsa`)
3. **ssh-agent** — used automatically if `SSH_AUTH_SOCK` is set (default on macOS)

### Host key verification

By default, the server's host key is verified against `~/.ssh/known_hosts`. Add the host first if needed:

```bash
ssh-keyscan photos.example.com >> ~/.ssh/known_hosts
```

To disable verification (not recommended):

```yaml
sftp:
  insecure_ignore_host_key: true
```

## Data directory

The daemon stores all runtime state under `~/.local/share/sftpsync/`:

| Path | Contents |
|------|----------|
| `registry.json` | Persisted list of jobs (restored on startup) |
| `jobs/<id>.json` | Per-job sync manifest |
| `daemon.sock` | Unix socket (present only while daemon is running) |

## Running as a service

On macOS the install script handles this automatically — a LaunchAgent is registered so `sftpsyncd` starts at login.

On Linux, create a systemd user service:

```ini
# ~/.config/systemd/user/sftpsyncd.service
[Unit]
Description=sftpsync daemon

[Service]
ExecStart=/usr/local/bin/sftpsyncd
Restart=on-failure

[Install]
WantedBy=default.target
```

```bash
systemctl --user enable --now sftpsyncd
```

## Project structure

```
sftpsync/
├── cmd/
│   ├── sftpsyncd/main.go         # daemon binary
│   ├── sftpsync/main.go          # CLI client binary
│   └── sftpsyncbar/              # macOS menu-bar app
│       ├── status.go             # pure status/refresh logic (cross-platform)
│       ├── menu.go               # systray menu construction
│       └── refresh.go            # adaptive polling
├── internal/
│   ├── apiclient/client.go       # HTTP-over-Unix-socket client
│   ├── config/config.go          # YAML config loading and validation
│   ├── daemon/
│   │   ├── daemon.go             # job registry, lifecycle management
│   │   ├── api.go                # HTTP API over Unix socket
│   │   └── job.go                # Job type and JSON response types
│   ├── humanize/humanize.go      # byte, percentage, and phase formatting
│   ├── sftp/client.go            # SSH/SFTP connection, walk, download
│   ├── state/manifest.go         # per-job sync state (JSON)
│   └── syncer/syncer.go          # poll loop, diff logic, worker pool
└── config.yaml.example
```

## Menu bar app (macOS)

`sftpsyncbar` shows every job in the menu bar: phase, file progress (`18 of 64
files`), byte percentage, the file currently downloading, the last successful
sync, and the full text of the latest error in its own row. The menu-bar title
carries the overall state — `⟳ 42%` while transferring, `⏸` when paused, `⚠` when
a job has failed, and nothing while idle — and hovering shows a summary such as
`sftpsync — 3 jobs, 1 active, 1 paused, 1 with errors`.

The menu polls once a second while any job is scanning or downloading, and every
30 seconds when every job is idle or paused, so an open menu is never more than
two seconds out of date while work is happening and an inactive app stays quiet.
`Refresh Status` fetches immediately; it does not start a sync.

Each job has its own controls: `Pause` while it is active, `Resume` while it is
paused, `Sync Now` to scan immediately, `Reveal Destination in Finder` to open
the configured local directory, `Copy Error` to put the latest error on the
clipboard, and `Remove Job…` last and clearly labelled because it is destructive.
Controls are disabled while their request is in flight, and the menu refreshes as
soon as it finishes. A failed action is shown in that job's own section rather
than only being logged, alongside the daemon's latest sync error. Failures that
belong to no single job, such as a config file that could not be added or a
daemon that would not start, are shown at the top of the menu.

`Open Daemon Log` opens `~/.local/share/sftpsync/sftpsyncd.log`, which is where
the daemon's output goes when the menu-bar app starts it.

The destination directory comes from the daemon's `local_path` field in the job
list response, so the app never reads or parses the config file itself, and the
path is passed to the system opener as a single argument rather than through a
shell.

The menu shows up to 10 jobs. If there are more, it says how many are not shown
rather than hiding them silently.

## Roadmap

- [ ] Structured logging
- [ ] Deletion sync (remove local files deleted on remote)
- [ ] Hash-based change detection fallback (for servers with unreliable mtimes)
