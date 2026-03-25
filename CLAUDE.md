# sftpsync — Claude context

## What this project is

A Go daemon (`sftpsyncd`) that manages multiple independent SFTP sync jobs. A CLI client (`sftpsync`) submits and controls jobs over a Unix socket. Each job polls an SFTP server on its own interval and downloads new/changed files to a local directory.

## Architecture

Two binaries, one shared internal package tree:

- `cmd/sftpsyncd` — long-running daemon process
- `cmd/sftpsync` — CLI client; communicates with the daemon via HTTP over a Unix socket

Internal packages:
- `internal/daemon` — job registry, lifecycle, HTTP API server
- `internal/syncer` — polling loop, diff logic, bounded download worker pool
- `internal/sftp` — SSH/SFTP client (connect, walk, atomic download)
- `internal/state` — per-job JSON manifest (tracks mtime+size of synced files)
- `internal/config` — YAML config loading and validation

## Key design decisions

- **Unix socket** at `~/.local/share/sftpsync/daemon.sock`, `chmod 600`. HTTP protocol over the socket so standard `net/http` works on both sides with a custom `DialContext`.
- **Each job gets its own manifest** at `~/.local/share/sftpsync/jobs/<id>.json`. The daemon always overrides `config.StatePath` — the value in the config file is ignored when running via the daemon.
- **Job IDs** are 8-char random hex strings (`crypto/rand`).
- **Registry** at `~/.local/share/sftpsync/registry.json` is the source of truth for persisted jobs. Daemon restores all registry jobs on startup.
- **Shutdown is unified** through `Daemon.Shutdown()` → `d.cancel()`. This cancels all syncer contexts and triggers `srv.Shutdown` via a goroutine watching `d.ctx.Done()`. Both SIGINT/SIGTERM and `POST /shutdown` funnel through this path.
- **Go 1.22 ServeMux** — uses method+path routing (`GET /jobs/{id}`) and `r.PathValue("id")`.
- **Atomic writes everywhere** — both file downloads and manifest/registry saves use temp-file + `os.Rename`.

## API endpoints

| Method | Path | Description |
|--------|------|-------------|
| `GET` | `/jobs` | List all jobs |
| `POST` | `/jobs` | Add a job (`{"config_path": "/abs/path"}`) |
| `GET` | `/jobs/{id}` | Get one job with status |
| `DELETE` | `/jobs/{id}` | Stop and remove a job |
| `POST` | `/shutdown` | Graceful daemon shutdown |

## Building and running

```bash
go build -o sftpsyncd ./cmd/sftpsyncd
go build -o sftpsync  ./cmd/sftpsync
./sftpsyncd                          # start daemon
./sftpsync add /path/to/config.yaml  # submit a job
./sftpsync list
```

## Dependencies

- `github.com/pkg/sftp` — SFTP client
- `golang.org/x/crypto` — SSH transport, known_hosts, ssh-agent
- `gopkg.in/yaml.v3` — config parsing

No web framework, no CLI framework — stdlib only beyond these three.
