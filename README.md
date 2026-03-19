# sftpSync

A lightweight Go daemon that polls an SFTP server and syncs new or changed image files to a local directory.

## Features

- Polls on a configurable interval (no SFTP push support required)
- Tracks remote file state via a local JSON manifest (mtime + size)
- Concurrent downloads with a bounded goroutine pool
- Atomic file writes (temp-file + rename — no partial files)
- SSH auth via password, private key, or ssh-agent
- Host key verification against `~/.ssh/known_hosts`
- File extension filtering (e.g. sync only `.jpg`, `.heic`, `.raw`)
- Graceful shutdown on SIGINT / SIGTERM

## Requirements

- Go 1.22+
- SSH access to an SFTP server

## Installation

```bash
git clone <repo>
cd sftpSync
go build -o sftpsync ./cmd/sftpsync
```

## Configuration

Copy the example config and edit it:

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

## Usage

```bash
./sftpsync -config config.yaml
```

The daemon runs until interrupted. On each poll it:

1. Connects to the SFTP server (reconnects automatically if the connection drops)
2. Walks the remote path recursively
3. Downloads any files that are new or have changed (different mtime or size)
4. Updates the local manifest

Manifest location defaults to `~/.local/share/sftpsync/manifest.json` and can be overridden with `state_path` in the config.

## Running as a service

**launchd (macOS):**

Create `~/Library/LaunchAgents/com.sftpsync.plist`:

```xml
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>com.sftpsync</string>
    <key>ProgramArguments</key>
    <array>
        <string>/usr/local/bin/sftpsync</string>
        <string>-config</string>
        <string>/Users/you/.config/sftpsync/config.yaml</string>
    </array>
    <key>RunAtLoad</key>
    <true/>
    <key>KeepAlive</key>
    <true/>
    <key>StandardOutPath</key>
    <string>/usr/local/var/log/sftpsync.log</string>
    <key>StandardErrorPath</key>
    <string>/usr/local/var/log/sftpsync.log</string>
</dict>
</plist>
```

```bash
launchctl load ~/Library/LaunchAgents/com.sftpsync.plist
```

**Docker:**

```dockerfile
FROM golang:1.22-alpine AS builder
WORKDIR /app
COPY . .
RUN go build -o sftpsync ./cmd/sftpsync

FROM alpine:latest
RUN apk add --no-cache ca-certificates
WORKDIR /app
COPY --from=builder /app/sftpsync .
ENTRYPOINT ["./sftpsync", "-config", "/config/config.yaml"]
```

```bash
docker build -t sftpsync .
docker run -d \
  -v /path/to/config.yaml:/config/config.yaml \
  -v /path/to/photos:/photos \
  sftpsync
```

## Project structure

```
sftpSync/
├── cmd/sftpsync/main.go          # entrypoint, signal handling
├── internal/
│   ├── config/config.go          # YAML config loading and validation
│   ├── sftp/client.go            # SSH/SFTP connection, walk, download
│   ├── state/manifest.go         # local sync state (JSON)
│   └── syncer/syncer.go          # poll loop, diff logic, worker pool
├── config.yaml.example
└── PLAN.md                       # architecture and roadmap
```

## Roadmap

- [ ] Docker support (`Dockerfile`)
- [ ] Structured logging
- [ ] Mac menu bar UI (`github.com/getlantern/systray`)
- [ ] Deletion sync (remove local files deleted on remote)
- [ ] Hash-based change detection fallback (for servers with unreliable mtimes)
