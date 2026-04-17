# AGENTS.md

Running log of non-obvious things about this codebase for future humans
and AI agents working in this repo. Append entries; don't rewrite
history. Keep it factual. Read this before diving in.

## How to verify the whole stack autonomously

```bash
make probe           # sudo needed; per-step timeouts; exits non-zero on hang
make chain           # sudo needed; stacks two ublks (proxy -> storage)
make stress          # sudo needed; race-detector stress (create/close churn, IO-while-close, etc.)
make torture         # sudo needed; randomised I/O with shadow-buffer verification (fuzz-style)
make flushbench      # sudo needed; microsecond trace of backend calls during flush operations
```

The probe (`example/probe/main.go`) exercises both sides of the stack:

- **Device-level** (direct I/O, bypasses page cache): `BLKGETSIZE64` size
  check; pre-mkfs zero-read; random-block write/read roundtrip that also
  verifies the backend's raw storage holds the same bytes at the same
  offset (proves kernel ↔ userspace offset mapping is 1:1).
- **Filesystem-level**: `mkfs.ext4` → mount → scripted write + `sync -f`
  (asserts backend writes > 0) → `fsync` alone (also asserts backend
  writes > 0) → drop caches + readback (asserts backend reads > 0) →
  scan the backend for the magic pattern (proves filesystem reads
  ultimately come from our in-memory storage) → concurrent writers →
  remount (journal replay) → umount → close → verify `/dev/ublkbN` gone.

If a step hangs beyond the timeout the probe **panics**, which prints a
full goroutine dump from the Go runtime — this is the single most useful
artifact when diagnosing ublk-level stalls, because it tells you whether
the worker is blocked in `WaitCQE`, inside `Backend.*`, or elsewhere.

`make chain` (`example/chain/main.go`) creates two ublk devices in the
same process: a *storage* ublk with an in-memory backend, then a *proxy*
ublk whose Backend forwards `Pread`/`Pwrite` calls to the storage's
block device (opened `O_DIRECT`). I/O written to the proxy's block
device must appear byte-for-byte at the same offset in the storage's
in-memory backend. This validates two complete ublk stacks running
side-by-side, two `LockOSThread`'d workers, and cross-device data
integrity. If this test passes, composition works.

`make torture` (`example/torture/main.go`) is the fuzz-style integrity
test. Each of N worker goroutines owns a disjoint region of the device;
each picks a random (offset, length) inside its region and a random
direction (read or write); on write it updates an in-memory shadow of
what the device should contain; on read it compares the result against
the shadow and fails the run (non-zero exit, with first-differing byte
offset) on any mismatch. Periodic `fsync` and full-region reverify runs
exercise the write-through and journaling paths. Run for minutes, not
seconds, to find subtle ordering bugs.

`make stress` (`example/stress/main.go`) runs four stressors against
`-race`-instrumented library code:

- **churn** — tight `New`→small-I/O→`Close` loop, catches leaks and
  shutdown-order races.
- **ioWhileClose** — I/O goroutines hammer the block device; `Close()`
  mid-stream. Catches races between worker cleanup and in-flight I/O.
- **concurrentClose** — N goroutines call `Close()` at once. Confirms
  the `sync.Once` guard is sufficient.
- **many** — N devices alive simultaneously with writer goroutines,
  closed in parallel. Catches cross-device state bleed.

Any race-detector warning fails the run (non-zero exit). Run for
longer (`-duration 5m`) before a release or after touching shutdown
code.

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

## Coverage

`make cover` produces `coverage/unit.out` + `coverage/integration.out`.
`make cover-html` opens the integration profile in a browser.

CI (`.github/workflows/ci.yml`, `test` job on amd64) uploads both
profiles to Codecov via `codecov/codecov-action@v5.5.4`. For a public
repo the upload is tokenless — it uses GitHub OIDC, which is why the
`test` job carries `permissions: id-token: write`. If the badge stops
updating, check that permission and the Codecov integration on the
repo's settings page; everything else is automatic.

Bare unit tests alone give ~25% coverage because most of the library
needs root + ublk_drv loaded to exercise. The integration test binary
pushes the total near ~80% once merged with the unit profile on
Codecov's side.

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

### drop_caches latency is kernel-side, not ours

`drop_caches=3` **does not** flush dirty pages (contrary to folklore —
the kernel just drops what's already clean; see `fs/drop_caches.c`).
If a `drop_caches` call appears to take several seconds, what's
actually happening is the kernel's **bdi writeback thread** firing on
its own timer (`/proc/sys/vm/dirty_writeback_centisecs`, default 500
cs = 5 s) during the same wall-clock window, and the backend sees those
writes attributed to the `drop_caches` step by a naive benchmark.

The practical fix is to `sync -f <mountpoint>` *before* any call that
requires a clean filesystem — then `drop_caches` runs in ~150 ms and
no background writeback interferes.

`make flushbench` empirically confirms: max gap between consecutive
backend calls while our stack is active is ≤4.3 ms. Seconds-level
stalls always attribute to kernel writeback timing, not our code.

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
