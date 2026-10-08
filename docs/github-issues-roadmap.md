# Feature roadmap: implementation-ready GitHub issues

The issues below are ordered by dependency. Each issue is intended to be small
enough for one agent to implement and review independently.

## Milestone 1: Trustworthy live status

### Issue 1 — Add an explicit sync phase and accurate file counters

**Suggested labels:** `enhancement`, `daemon`, `syncer`

**Depends on:** nothing

#### Goal

Make job status describe what the syncer is currently doing rather than only the
result of its last polling cycle.

#### Scope

Extend `internal/syncer.SyncStatus` with:

- A typed phase with the values `idle`, `scanning`, `downloading`, `paused`, and
  `error`.
- The number of eligible remote files after extension filtering.
- The number of files selected for the current batch.
- Completed, failed, and remaining file counts for the current batch.
- The current filename, if one is being downloaded.
- The time the current scan or batch started.
- A separate `LastSuccessfulSync` timestamp.

Keep `LastError` as an `error` inside the syncer. Convert it to a string only in
`internal/daemon/job.go`, where the existing JSON boundary already lives.

Update status under the existing mutex. Return a complete value snapshot from
`Status()`; callers must not receive references to mutable status data.

#### Behavior

- Set the phase to `scanning` before connecting and walking the remote tree.
- Set the phase to `downloading` before starting a non-empty download batch.
- Set the phase to `idle` after a completely successful cycle.
- Set the phase to `error` when connection, walking, or any file download fails.
- `LastSuccessfulSync` changes only after a cycle with no failures.
- File totals count files that pass the configured extension filter, not every
  regular file returned by the SFTP walk.
- Batch counters reset when a new scan starts.
- Preserve the current error until a later cycle succeeds; merely starting a
  retry must not clear it.

#### API and CLI changes

Add matching JSON fields to `daemon.StatusResponse`. Update `sftpsync list` and
`sftpsync status` to show the phase and meaningful batch counts. Existing JSON
field names must remain compatible.

#### Tests

- Unit-test every phase transition through extracted status-update helpers or a
  testable sync-cycle dependency; do not require a real SFTP server.
- Test that filtered files are excluded from the eligible total.
- Test that a failed cycle does not update `LastSuccessfulSync`.
- Test `Job.toResponse()` with and without `LastError`.
- Run `go test ./...` and `go vet ./...`.

#### Acceptance criteria

- A job reports `scanning`, `downloading`, `idle`, or `error` at the appropriate
  point in a cycle.
- Counters change while a batch runs and remain inspectable after failure.
- A partial batch failure cannot be reported as a successful sync.

---

### Issue 2 — Report byte-level download progress

**Suggested labels:** `enhancement`, `syncer`, `sftp`

**Depends on:** Issue 1

#### Goal

Provide enough data for CLI and menu-bar percentage progress without coupling
UI code to the downloader.

#### Scope

Add byte counters to `SyncStatus` and `daemon.StatusResponse`:

- `bytes_total`
- `bytes_completed`
- `current_file_bytes_total`
- `current_file_bytes_completed`

Change `internal/sftp.Client.DownloadTemp` to accept a progress callback or add a
new progress-aware method used by the syncer. Instrument the existing copy with
a small writer wrapper; do not buffer an entire photo in memory.

#### Behavior

- Batch total bytes are calculated from `RemoteFile.Size` before downloads
  start.
- Progress updates are concurrency-safe with multiple workers.
- Aggregate completed bytes include bytes copied by all active workers.
- Per-file fields represent one active file on a best-effort basis. They may
  switch between files when multiple workers are active.
- Failed or cancelled transfers must never make completed bytes exceed total
  bytes.
- Keep the existing temp-file and atomic rename behavior.

Throttle status updates if necessary so the mutex is not acquired for every
small read. A time threshold around 100–250 ms or a byte threshold is adequate.

#### Tests

- Test progress using an in-memory reader and temporary destination.
- Test monotonic aggregate progress and exact final totals.
- Test a failed copy and context cancellation.
- Run `go test ./...` and `go vet ./...`.

#### Acceptance criteria

- Status consumers can calculate a stable batch percentage.
- A completed successful batch reports equal total and completed bytes.
- Atomic downloads still leave no final partial file after failure.

---

### Issue 3 — Add daemon/API status integration tests

**Suggested labels:** `testing`, `daemon`, `api`

**Depends on:** Issues 1–2

#### Goal

Protect the status contract before controls and UI are built on top of it.

#### Scope

Add test seams for constructing a daemon job with a controllable sync status.
Use `httptest` for handlers where possible; tests must not depend on the user's
real home directory, Unix socket, SSH agent, or SFTP server.

Cover:

- `GET /jobs` with zero, one, and multiple jobs.
- `GET /jobs/{id}` success and not found.
- JSON encoding of phases, counters, timestamps, and `last_error`.
- API client handling of non-2xx responses and malformed JSON.
- Deterministic job ordering, preferably by `AddedAt` and then ID, so CLI and UI
  do not reorder randomly on every refresh.

#### Acceptance criteria

- Status response behavior is exercised through HTTP, not only struct methods.
- Tests are deterministic and pass under `go test -race ./...`.
- No production endpoint behavior changes beyond deterministic ordering and
  correct error handling.

## Milestone 2: Per-job controls

### Issue 4 — Add a coalescing “sync now” trigger to the syncer

**Suggested labels:** `enhancement`, `syncer`

**Depends on:** Issue 1

#### Goal

Allow an immediate poll without restarting a job or waiting for its interval.

#### Scope

Refactor `Syncer.run` so it waits on a reusable timer, context cancellation, and
an immediate-sync channel. Add a public `SyncNow()` method.

#### Behavior

- `SyncNow()` wakes an idle syncer immediately.
- Calls made during an active cycle coalesce into at most one additional cycle.
- Repeated calls never run two cycles concurrently for the same job.
- Cancellation stops the timer and closes the SFTP connection.
- Avoid `time.After` in the permanent loop so abandoned timers are not created.

#### Tests

Use a fake cycle function or client abstraction to verify immediate triggering,
coalescing, no overlap, interval operation, and cancellation.

#### Acceptance criteria

- An idle job begins scanning promptly after `SyncNow()`.
- Ten requests during one active cycle cause no more than one follow-up cycle.
- `go test -race ./...` finds no race.

---

### Issue 5 — Add runtime pause and resume to the syncer

**Suggested labels:** `enhancement`, `syncer`

**Depends on:** Issues 1 and 4

#### Goal

Pause a job without deleting it or shutting down the daemon.

#### Scope

Add `Pause()`, `Resume()`, and `IsPaused()` to `Syncer` using explicit channels
or mutex-protected state. Do not implement pause by stopping and recreating the
syncer.

#### Behavior

- Pause prevents new scans and new downloads from starting.
- Downloads already copying may finish and be committed atomically.
- Once current workers finish, status becomes `paused`.
- Resume changes the job to an active state and triggers an immediate scan.
- Pause and resume are idempotent.
- `SyncNow()` while paused records at most one request and runs it after resume.
- `Stop()` must work from idle, active, and paused states without blocking.

Document the finish-current-file behavior in method comments.

#### Tests

Test pausing while idle, scanning, and downloading; resume; repeated calls;
stop-while-paused; and race safety.

#### Acceptance criteria

- A paused job performs no new remote operations.
- In-flight files are not left at final paths partially written.
- Pause/resume cannot deadlock shutdown.

---

### Issue 6 — Expose sync, pause, and resume through daemon HTTP and API client

**Suggested labels:** `enhancement`, `daemon`, `api`

**Depends on:** Issues 4–5

#### Goal

Make per-job controls available to all clients.

#### Scope

Add endpoints:

```text
POST /jobs/{id}/sync
POST /jobs/{id}/pause
POST /jobs/{id}/resume
```

Add corresponding methods to `internal/apiclient.Client`.

#### Behavior

- Return `202 Accepted` for a newly accepted asynchronous action.
- Return `404 Not Found` for an unknown job.
- Repeated pause/resume calls remain successful and idempotent.
- Validate response status codes in every API client method, including existing
  methods that currently treat some non-2xx responses as success.
- Keep the Unix socket as the trust boundary; do not add TCP listening.

#### Tests

Add handler and API client tests for success, unknown jobs, repeated actions,
and daemon errors.

#### Acceptance criteria

- Each endpoint controls only the selected job.
- HTTP failures are returned to callers with useful context.
- Existing add/list/get/remove/shutdown behavior remains compatible.

---

### Issue 7 — Persist paused state in the job registry

**Suggested labels:** `enhancement`, `daemon`, `persistence`

**Depends on:** Issues 5–6

#### Goal

Ensure a deliberately paused job stays paused after daemon or machine restart.

#### Scope

Add a `paused` boolean to `registryEntry`. Update registry save/load and job
response state. Keep backward compatibility: absent `paused` means false.

On restore, create and start the syncer without allowing an initial scan for a
paused job. Avoid a start-then-pause race.

#### Failure behavior

If changing runtime state succeeds but saving the registry fails, return an
error and restore the previous runtime state when practical. Do not silently
claim the state was persisted.

#### Tests

- Load an old registry without `paused`.
- Round-trip paused and active jobs.
- Restore a paused job and prove no cycle starts.
- Exercise registry write failure if a filesystem seam is available.

#### Acceptance criteria

- Paused state survives daemon restart.
- Existing registry files continue to load.
- Restored paused jobs make no SFTP connection until resumed.

---

### Issue 8 — Add sync, pause, and resume commands to the CLI

**Suggested labels:** `enhancement`, `cli`

**Depends on:** Issue 6

#### Goal

Offer all per-job controls without requiring the macOS app.

#### Scope

Add:

```text
sftpsync sync <id>
sftpsync pause <id>
sftpsync resume <id>
```

Update usage text and job detail output. Show phase, file progress, byte
progress, paused state, last successful sync, and errors. Use a small formatter
for human-readable byte values rather than adding a CLI framework.

#### Tests

Extract argument dispatch or output formatting enough to unit-test command
validation and status formatting without calling `os.Exit`.

#### Acceptance criteria

- Each command reports the accepted action and job ID.
- Missing IDs and unknown jobs return non-zero with actionable text.
- Status output remains readable when no batch has run yet.

## Milestone 3: Live macOS menu-bar experience

### Issue 9 — Render current job state and progress in the menu

**Suggested labels:** `enhancement`, `macos`, `ui`

**Depends on:** Issues 1–3

#### Goal

Make the menu useful as an up-to-date status view.

#### Scope

Extend each `jobSlot` in `cmd/sftpsyncbar/menu.go` to display:

- Current phase.
- File progress, for example `18 of 64`.
- Byte percentage when total bytes are known.
- Current filename while downloading.
- Last successful sync.
- Full latest error text in a dedicated row.

Replace the fixed 30-second refresh policy with adaptive polling:

- Every 1–2 seconds while any job is scanning or downloading.
- Every 30 seconds while all jobs are idle or paused.
- Immediate refresh after a user action.

Rename `Refresh Now` to `Refresh Status` because it does not start a sync.

#### Constraints

`getlantern/systray` does not provide a native progress-bar control. Render
progress as text and, if supported reliably, an aggregate percentage in the
menu-bar title. Do not introduce a second macOS UI framework in this issue.

#### Tests

Extract pure functions for status text, percentages, and refresh interval.
Test zero totals, active progress, completion, pause, and error rendering.

#### Acceptance criteria

- Opening the menu during a transfer shows status no more than two seconds old.
- Idle polling remains low-frequency.
- No division-by-zero or percentage above 100 is displayed.

---

### Issue 10 — Add per-job controls to the menu-bar app

**Suggested labels:** `enhancement`, `macos`, `ui`

**Depends on:** Issues 6 and 9

#### Goal

Control a selected job directly from its menu section.

#### Scope

Add per-slot actions:

- `Pause` when active.
- `Resume` when paused.
- `Sync Now`.
- Keep `Remove Job` separate and clearly destructive.

Update the event loop and API calls. Disable controls while their request is in
flight, then refresh immediately. If a request fails, show the failure in that
job's menu section instead of only writing a log line.

Remove the repetitive ten-case select if a clearer event-dispatch pattern can
be introduced without changing UI behavior. If the ten-job slot limit remains,
show `N additional jobs not shown`; silently hiding jobs is not acceptable.

#### Acceptance criteria

- Controls always target the job currently displayed in their slot.
- Rapid refresh/reordering cannot make a click act on a different job.
- Failed actions are visible to the user.
- Pause/resume state updates promptly.

---

### Issue 11 — Add menu-bar activity/error indicators and local actions

**Suggested labels:** `enhancement`, `macos`, `ui`

**Depends on:** Issue 9

#### Goal

Communicate important state without requiring users to inspect daemon logs
manually.

#### Scope

- Set an active, idle, paused, or error menu-bar title/icon state.
- Add `Reveal Destination in Finder` per job.
- Add `Open Daemon Log` and `Copy Error` actions.
- Show add-job and daemon-start failures in the menu.
- Keep the app a menu-bar-only application.

A config-derived destination path must be supplied safely by the daemon/API or
loaded through a dedicated read-only response field. Do not parse arbitrary YAML
inside a shell command. Launch Finder with `exec.Command("open", path)` and pass
the path as an argument, not through a shell.

#### Acceptance criteria

- Errors are visible from the menu and copyable.
- Reveal Destination opens the configured local directory safely.
- No user-controlled path is interpolated into a shell command.

## Milestone 4: Photo-library safety

### Issue 12 — Prevent filename collisions in EXIF-organized folders

**Suggested labels:** `bug`, `data-safety`, `syncer`

**Depends on:** Milestone 1

#### Goal

Prevent two remote photos with the same basename and capture-date folder from
silently replacing one another.

#### Scope

Add a validated config option:

```yaml
sync:
  collision_policy: rename # error | skip | rename
```

Default to `rename` for new/unspecified configs. Before final rename:

- `error`: fail the file if a different destination already exists.
- `skip`: leave the existing file and record a clear skipped result.
- `rename`: choose a deterministic suffix such as `IMG_0001-2.JPG`.

The manifest must record the chosen local destination for each remote path so a
later update replaces the same mapped file rather than choosing another suffix.
This requires a backward-compatible manifest schema addition.

Do not use check-then-rename without accounting for concurrent workers choosing
the same destination. Destination reservation must be concurrency-safe.

#### Tests

Cover same basename/same date, concurrent conflicts, repeated sync, updates to a
previously renamed file, each policy, and old manifests without local paths.

#### Acceptance criteria

- No policy silently overwrites an unrelated local photo.
- Repeated syncs use stable destination names.
- Old manifests continue to load.

---

### Issue 13 — Preserve per-file failures and retry transient downloads

**Suggested labels:** `reliability`, `syncer`

**Depends on:** Issues 1–2

#### Goal

Make partial failures visible and recover automatically from short network
interruptions.

#### Scope

- Make `downloadAll` return a structured batch result.
- Keep failed paths out of the success manifest.
- Preserve an aggregate error summary in job status.
- Retry transient open/copy/network failures with bounded exponential backoff
  and jitter.
- Add config for maximum attempts with a conservative default such as 3.
- Do not retry deterministic local failures such as invalid destination paths or
  permission denial unless they are classified as transient.
- Close the SFTP connection after transport failures so the next attempt can
  reconnect.

Keep detailed file errors in logs while bounding the status/API error string so
very large failed batches cannot create unbounded responses.

#### Tests

Use scripted fakes to cover fail-then-succeed, exhausted retries,
non-retryable errors, cancellation during backoff, manifest updates, and batch
status.

#### Acceptance criteria

- A partially failed batch reports `error`, a failed count, and useful context.
- Successful files are not downloaded again.
- Failed files are retried on a later cycle.
- Shutdown interrupts retry waits promptly.

---

### Issue 14 — Verify existing files before adopting them into the manifest

**Suggested labels:** `data-safety`, `syncer`, `state`

**Depends on:** Issue 12

#### Goal

Avoid treating a wrong, empty, or truncated local file as a successfully synced
photo merely because its path exists.

#### Scope

When a remote file is absent from the manifest but its expected local path
exists:

- Adopt it only when its size matches the remote size.
- If sizes differ, apply the configured collision policy or download safely;
  never mark it synced without verification.
- Add an optional config mode for SHA-256 verification after download and during
  adoption where the remote server supports reading the file.
- Persist a local hash only when hash mode is enabled.

Hashing should stream data and respect cancellation. Do not make remote hashing
a requirement for the default mtime+size mode.

#### Tests

Cover matching size, mismatched size, zero-byte local file, hash match/mismatch,
cancellation, and backward-compatible manifests.

#### Acceptance criteria

- Path existence alone never creates a manifest success entry.
- Default verification does not add a second remote read.
- Optional hash verification detects same-size corruption.

---

### Issue 15 — Detect incomplete remote scans and clean stale staging files

**Suggested labels:** `reliability`, `data-safety`, `sftp`

**Depends on:** Issue 1

#### Goal

Distinguish a complete remote inventory from a partial walk and recover cleanly
from interrupted transfers.

#### Scope

- Change `sftp.Client.Walk` to return or aggregate entry errors instead of
  silently skipping every unreadable path.
- Mark the cycle as failed/partial when any subtree cannot be read.
- Include a bounded summary in status and detailed paths in logs.
- On job startup, remove only stale files matching the app's staging-file
  pattern inside that job's local destination.
- Use a conservative age threshold so active temp files from another process are
  not removed.
- Report insufficient local disk space before starting a batch when available
  on the platform; failure to query space should be non-fatal.

This issue is a prerequisite for any future remote-deletion mirroring. A partial
scan must never be considered evidence that a remote file was deleted.

#### Tests

Cover partial walk results, error aggregation limits, staging-file age and name
checks, cancellation, and disk-space warning behavior.

#### Acceptance criteria

- Unreadable remote entries make the cycle visibly partial/failed.
- Cleanup cannot delete normal photos or fresh staging files.
- No deletion-sync feature treats a partial scan as authoritative.

## Milestone 5: Photo workflow enhancements

Create these only after the safety milestone is complete. Each should be scoped
as a separate issue when scheduled:

1. **Completion and failure notifications:** macOS notifications with imported,
   skipped, and failed counts; coalesce notifications per batch.
2. **Include/exclude glob rules:** apply normalized remote-relative paths and
   test precedence between extensions, includes, and excludes.
3. **Live Photo and RAW+JPEG grouping:** identify same-directory files sharing a
   stem, keep grouped files in the same date destination, and display groups as
   one logical item without changing manifest identity.
4. **Date-range initial import:** optional inclusive capture/mtime bounds with a
   dry-run count before downloading.
5. **Bandwidth and schedule controls:** per-job byte-rate limit, quiet hours,
   cancellation-safe waiting, and explicit timezone handling.

Remote deletion, local deletion propagation, and two-way sync are intentionally
out of scope until incomplete-scan detection, collision handling, and manifest
verification are deployed and proven.

## Definition of done for every issue

- Preserve atomic temp-file-to-final-file behavior.
- Add focused unit/integration tests for changed behavior.
- Run `gofmt` on changed Go files.
- Run `go test ./...`, `go test -race ./...`, and `go vet ./...`.
- Update `README.md`, `config.yaml.example`, and CLI usage when user-visible
  behavior or configuration changes.
- Maintain backward compatibility for registry, manifest, and JSON response
  fields unless the issue explicitly documents a migration.
- Do not add a web/CLI framework for functionality supported by the standard
  library and current dependencies.
