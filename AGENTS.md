# AGENTS.md

Running log of non-obvious things about this codebase for future humans
and AI agents working in this repo. Append entries; don't rewrite
history. Keep it factual. Read this before diving in.

## How to verify the whole stack autonomously

```bash
make probe           # sudo needed; per-step timeouts; exits non-zero on hang
```

The probe (`example/probe/main.go`) exercises: device creation → `mkfs.ext4`
→ mount → scripted write + `sync -f` → drop page cache + readback →
concurrent writers → unmount → remount (journal replay) → unmount → close
→ verify `/dev/ublkbN` is gone.

If a step hangs beyond the timeout the probe **panics**, which prints a
full goroutine dump from the Go runtime — this is the single most useful
artifact when diagnosing ublk-level stalls, because it tells you whether
the worker is blocked in `WaitCQE`, inside `Backend.*`, or elsewhere.

Other diagnostic commands:

```bash
pgrep -af 'example/probe' | awk '{print $1}' | xargs -r sudo kill -SIGQUIT   # manual stack dump
cat /sys/class/block/ublkb*/stat                                              # block stats
sudo dmesg | tail -40                                                         # kernel messages (ublk_drv logs here)
```

## Kernel ABI landmines (UAPI, current kernels 6.13+)

- **`devInfo.DevID` must match `ctrlCmd.DevID`** (kernel 6.17+ validation).
  We set both to `^uint32(0)` to request auto-assign. Previous code only
  set it in the ctrlCmd, which started returning `EINVAL` on 6.17.
- **`ADD_DEV` has two encodings.** The ioctl-encoded command
  (`uCmdAddDev`) is newer; the legacy `cmdAddDev` is tried as fallback.
  Expect `ENOTSUP` from the legacy path on modern kernels — that's
  normal, just means the first path succeeded.
- **`FETCH_REQ` is processed as deferred task work** starting around
  6.13. It only completes when the io_uring is entered with
  `IORING_ENTER_GETEVENTS`. That is why `worker.run()` submits via
  `SubmitAndWait()` (which passes that flag), not `Submit()`. Using plain
  `Submit()` leaves `START_DEV` hanging in the kernel waiting for the
  fetch to complete.
- **Control ring uses `SQE128`** (for `URING_CMD` passthrough of
  `ublksrv_ctrl_cmd`, which sits in `sqe->cmd` at offset 48). Data ring
  uses `SQE64` and packs the 16-byte `ublksrv_io_cmd` into the trailing
  `Cmd` field.

## Worker-goroutine discipline

- Each worker **must** call `runtime.LockOSThread()` before its first
  `io_uring_enter`. ublk binds IO credentials to the thread that first
  submitted `FETCH_REQ`. If a goroutine gets migrated between threads,
  subsequent submissions fail or go to the wrong queue.
- `FETCH_REQ` must be submitted *before* `START_DEV` is issued (kernel
  blocks `START_DEV` until the fetches arrive). The worker signals
  readiness through a channel *after* its first `SubmitAndWait()` so the
  main goroutine can proceed to `START_DEV`. See the comments in
  `worker.run()`.

## Shutdown sequencing (current, post data-race fixes)

`Device.shutdown()` ordering matters:

1. `w.ioRing.Cancel()` on each worker (eventfd wake of blocked
   `WaitCQE`). This is a main-goroutine operation, safe because the
   worker hasn't closed the ring yet.
2. `wg.Wait()` — happens-before barrier; workers have exited `run()`
   and will not touch any shared state thereafter.
3. For each worker: `w.cleanup()` to munmap `ioDescs` and `Close()` the
   ring. Done from main goroutine, so ring state writes don't race with
   reads in `Cancel()`.
4. `close(charFD)` — triggers `ublk_ch_release` in the kernel, aborting
   any stale ublk_io state so `delDev()` won't block on in-flight IOs.
5. `stopDev()`, `delDev()`.
6. Close control ring, close `ctrlFD`.

The old version interleaved these steps and race-detected between
`worker.cleanup` / `Device.shutdown` / `Ring.Cancel` / `Ring.Close`. Do
not refactor the order without rerunning `make test-integration` under
`-race` — the kernel doesn't enforce this and bugs are stochastic.

## Build tags and tooling

- **Integration tests** live in `ublk/ublk_integration_test.go` behind
  `//go:build integration`. The file's `TestMain` hard-fails (not skips)
  if not run as root or if `ublk_drv` is missing. Don't reintroduce
  `t.Skip` for these — the user explicitly wants failure, not silence.
- **golangci-lint** must know about the tag or it flags
  `memBackend.snapshot` as unused. Set via `run.build-tags: [integration]`
  in `.golangci.yml`.
- **gopls / editors** don't read `.golangci.yml`. The portable fix is
  `go env -w GOFLAGS=-tags=integration`. For VSCode specifically,
  `.vscode/settings.json` has `gopls.build.buildFlags` — but `.vscode/`
  is `.gitignore`d, so don't rely on committing it.

## CI specifics

- `ubuntu-24.04` runner has Go 1.25.8 preinstalled. The workflow passes
  `go-version: "1.25"` + `check-latest: false` so `setup-go` matches the
  preinstalled version instead of fetching 1.25.6 from scratch (~10s
  saved).
- `go.mod`'s `go 1.25.0` directive is the canonical form. `go mod tidy`
  rewrites `go 1.25` → `go 1.25.0`; don't commit the short form or the
  `lint-tidy` step will fail.
- `golangci-lint` is installed from the prebuilt tarball via the
  project's own `install.sh`, pinned by tag (`v2.11.4` currently). Going
  via `go install` compiles from source and takes ~107s instead of ~3s.
- `actions/*@<oldpin>` older than Feb 2025 hit the retired Actions Cache
  v1 service and log `Cache service responded with 400`. If you see that
  warning reappear, bump the pin.

## Data-plane details

- `maxQueueDepth = 128`, `maxSectors = 256` (128 KiB max I/O). These are
  hard-coded; changing them means re-running the full integration test
  because buffer sizing, `ioDescs` mmap offset, and kernel param struct
  all depend on the values.
- The backend is called with a `[]byte` slice whose length already
  reflects the logical IO size (`nr_sectors * 512`). Don't re-clip.

## Known observations

### ext4 + page cache timing

When poking the mount from another terminal, writes to the page cache
are **not** visible to `Backend.WriteAt` until either:
- `sync -f <mountpoint>` (or an `fsync(2)` on any fd there), or
- the kernel's periodic flush (`/proc/sys/vm/dirty_expire_centisecs`,
  default 3000 = 30s).

Plain `sync(1)` syncs every mount on the host, so it can look "stuck"
for a long time on a busy system even when nothing in ublk is wrong.
Always prefer `sync -f`.

### "scanned 6 out of 9 Go files" in CodeQL

CodeQL extractor only scans files with default build tags. The 3 it
misses are the `//go:build integration` test and the two `example/`
`main.go` packages (they live in different `package main` roots).
That's informational, not a failure.

### Default Code Scanning vs. advanced workflow

GitHub's **Default Code Scanning setup** and our `codeql.yml` advanced
workflow are mutually exclusive. If both are enabled, advanced runs fail
with `Resource not accessible by integration` when uploading SARIF (the
default setup owns that endpoint). Toggle one off in repo settings.
