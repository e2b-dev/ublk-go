# Future Work

Features and optimizations to add on top of the current minimal implementation.

## Reference implementations

When picking up any of the items in this document, cross-reference these
ublk implementations:

- [`semistrict/go-ublk`](https://github.com/semistrict/go-ublk) — another
  pure-Go ublk library. Covers multi-queue, USER_COPY mode, zero-copy with
  auto-buf-reg, queue affinity pinning, GET_PARAMS / GET_FEATURES /
  GET_QUEUE_AFFINITY, and an "easy" handler that adapts `io.ReaderAt`
  plus optional `Flusher`/`Discarder`/`Zeroer` interfaces.
- [SPDK ublk target](https://spdk.io/doc/ublk.html) — production C
  implementation used by [Longhorn v2 data
  engine](https://github.com/longhorn/longhorn/issues/9456). Validates
  our `runtime.LockOSThread`-per-queue model: SPDK explicitly notes the
  kernel constraint that "one ublk device queue can be only processed
  in the context of system thread which initialized it". They pin one
  spdk_thread per reactor and assign queues round-robin — same model
  we'd land on for multi-queue.
- [`PhanLe1010/libublk-rs (longhorn branch)`](https://github.com/PhanLe1010/libublk-rs/tree/longhorn) —
  Rust PoC the Longhorn team built before settling on SPDK. Useful for
  comparing how the Rust ecosystem models the FETCH/COMMIT lifecycle.
- [`ublk-org/ublksrv`](https://github.com/ublk-org/ublksrv) — the
  reference C implementation maintained alongside the kernel. Source of
  truth for kernel-feature interaction patterns (BATCH_IO,
  DEFER_TASKRUN, idle-buffer madvise, etc.).

### Reference io_uring libraries (Go)

We ship our own minimal `io_uring` wrapper in `ublk/uring/` rather than
depending on a generic Go library. The reason is that ublk requires
**128-byte SQEs (`IORING_SETUP_SQE128`)** and **`IORING_OP_URING_CMD`
(opcode 46)** to carry the embedded `ublksrv_io_cmd` payload, and none
of the popular Go ports expose both first-class. `semistrict/go-ublk`
reached the same conclusion and also hand-rolled its own.

That said, two Go ports are still worth using as **reference for ABI
correctness** when binding new kernel features:

- [`pawelgaczynski/giouring`](https://github.com/pawelgaczynski/giouring)
  — most complete pure-Go port of `liburing`, naming maps almost 1:1 to
  the C API. Best place to cross-check struct layouts and constants
  (`io_uring_buf_ring`, `Probe`, `RsrcRegister`, etc.) before we add
  things like zero-copy provided buffers.
- [`godzie44/go-uring`](https://github.com/godzie44/go-uring) — closer
  to a `liburing` port with reactor pattern; useful reference for the
  Go memory-model discussion (`amd64_atomic` build tag, `seq_cst` vs
  `acq_rel` analysis) if we ever profile and find atomics on the SQ/CQ
  head/tail are a bottleneck.

Other libraries we evaluated and ruled out for ublk:
[`Iceber/iouring-go`](https://github.com/iceber/iouring-go) (most
popular but networking-focused, 64-byte SQE only),
[`ii64/gouring`](https://github.com/ii64/gouring) (also 64-byte SQE,
no URING_CMD).

**Action item:** whenever we touch `ublk/uring/uring.go` or the SQE128
layout, cross-check struct offsets and any new opcodes against
`giouring`'s definitions to catch ABI drift. Cheap insurance.

## Ungraceful-exit device leaks

When our test harnesses (or any user program using the library) is
**terminated without running `Device.Close()`**, `/dev/ublkbN` and
`/dev/ublkcN` nodes accumulate on the system. The clean-shutdown path
— i.e. `Device.Close()` actually being called — works correctly: we
verified this with 152 consecutive stress-churn iterations, every
probe run, and every torture run. Zero leaks when Close runs.

Leaks happen when:

1. **`SIGKILL`** on the process (our `sigkill` test intentionally does
   this — the scenario is "what if your process crashes mid-I/O").
2. **`Ctrl+C`** on a harness binary that doesn't trap `SIGINT`. Go's
   default signal handler terminates immediately without running
   `defer`s, so `dev.Close()` is never invoked.
3. **`kill -9`** from elsewhere.

In all three cases the kernel must tear the device down on its own
via `ublk_ch_release` (triggered by fd close on process exit). On
kernel 6.17.0 (Ubuntu 25.10) that async path can wedge processes in
`D` state indefinitely, leaving device nodes behind. We haven't traced
this to a specific kernel commit — ublk_drv was heavily refactored in
Sept 2025 (commits 25c028aa7915, 97e8ba31b8f1, 225dc96f35af,
b749965edda8), multiple regressions fixed in 6.18. The ungraceful-exit
cleanup path may or may not be among them.

**What we should do next:**

- [x] Make our long-running harnesses (stress, torture, flushbench)
  trap `SIGINT` / `SIGTERM` and run `Device.Close()` on exit. This
  eliminates the 95% case (Ctrl+C during a session).
- [ ] Confirm on a fresh reboot that running the full test matrix
  without any interruption leaves zero stale devices. That would
  pin the remaining leak budget to "only SIGKILL", which is the
  one we can't do anything about from userspace.
- [ ] If the remaining "SIGKILL → orphan device" symptoms persist on
  newer kernels (6.18+), produce a minimal repro and post to
  `linux-block@vger.kernel.org` / Ming Lei.
- [ ] Optionally: warn in `ublk.New` if `/sys/class/ublk-char/` is
  approaching `ublks_max` (default 64). Gives users a signal before
  `New()` starts failing silently due to accumulated orphans.
- [ ] Expose a `ublk.CleanupOrphans()` (or `ublkctl` CLI) that scans
  `/sys/class/ublk-char/` and force-deletes leaked devices via
  `/dev/ublk-control` `DEL_DEV[_ASYNC]`. This is exactly the pain point
  Longhorn flags in
  [issue #10738](https://github.com/longhorn/longhorn/issues/10738):
  *"When instance manager pod crash, it might leave the orphan UBLK
  device. Currently, it is difficult to remove these orphan UBLK devices
  and sometime a reboot is needed."* — they have no userspace cleanup
  path either, so a small Go helper that wraps DEL_DEV by ID would be
  genuinely novel value vs. existing libraries (semistrict/SPDK don't
  ship this either).

## Robustness / kernel-ABI hygiene

### Compute ioctl-encoded opcodes from `_IOR`/`_IOWR` macros

We currently hardcode the ioctl-encoded ublk command opcodes in
`ublk/types.go` (`uCmdAddDev = 0xC0207504`, etc.). The constant `0x20` in
each value is `sizeof(struct ublksrv_ctrl_cmd) = 32`, baked into the
ioctl encoding. If the kernel ever extends `ublksrv_ctrl_cmd` (the
explicit `Reserved1`/`Reserved2` fields exist precisely so it can grow),
the size shifts and our hardcoded opcodes silently become wrong while
the kernel returns `EINVAL`.

Mirror the pattern used by [`semistrict/go-ublk`](https://github.com/semistrict/go-ublk)
in [`const.go`](https://github.com/semistrict/go-ublk/blob/main/const.go):
define `ioc(dir, type, nr, size)` and derive each `uCmd*` /  `uIO*`
opcode from `iowr(ublkType, nr, unsafe.Sizeof(ctrlCmd{}))`. Pure
mechanical refactor; no behavior change today, future-proofs against
struct extensions.

### Handle non-read/non-write IO ops explicitly

`worker.handleIO` currently returns `EOPNOTSUPP` for everything except
`OP_READ`/`OP_WRITE`. We don't currently set `UBLK_ATTR_VOLATILE_CACHE`
or `MaxDiscardSectors`, so the kernel block layer shouldn't emit
`OP_FLUSH` / `OP_DISCARD` / `OP_WRITE_ZEROES` against us — but if a
user touches the sysfs knobs (`echo 1 > .../discard_max_bytes`) or
mounts a filesystem that issues unconditional flushes, they'll get
unexplained EIO. Cheap fix when we add the optional backend interfaces:

- `OP_FLUSH` (2) → `0` when no `Flusher`, otherwise call it
- `OP_DISCARD` (3) → `0` (or call `Discarder` once exposed)
- `OP_WRITE_ZEROES` (5) → `0` (or call `Zeroer`)

Cross-ref: semistrict's `IOOp` enum in
[`const.go`](https://github.com/semistrict/go-ublk/blob/main/const.go)
and the `ReaderAtHandler` adapter in
[`easy.go`](https://github.com/semistrict/go-ublk/blob/main/easy.go).

### Async STOP_DEV pattern

We currently issue STOP_DEV synchronously on the same control ring used
for ADD/SET/DEL. That works because the kernel responds with the CQE
once the worker thread releases the char fd (which we drive ourselves).
Once we go multi-queue, the cleaner pattern (used by semistrict in
[`ctrl.go`](https://github.com/semistrict/go-ublk/blob/main/ctrl.go)) is:

1. Submit STOP_DEV without waiting → kernel injects ENODEV into pending
   FETCHes for every queue.
2. Wait for all per-queue serve goroutines to drain.
3. *Then* wait for the STOP_DEV CQE.

Keeps the abort signal symmetric across queues and avoids relying on
char-fd close to drive the abort path.

### Friendlier error when `ublk_drv` isn't loaded

Today, opening `/dev/ublk-control` on a host without the module
returns a bare `ENOENT` from `unix.Open`, which surfaces as a
confusing `"open /dev/ublk-control: no such file or directory"`.
Detect this case in `openDevice` and wrap it with a hint:

> ublk control device not found; load the kernel module with
> `modprobe ublk_drv` (requires Linux 6.0+ and CAP_SYS_ADMIN)

Longhorn went so far as to ship dedicated CLI tooling
([`longhornctl ... --enable-spdk`](https://github.com/longhorn/longhorn/issues/11803),
[longhorn/cli PR #321](https://github.com/longhorn/cli/pull/321))
just to auto-`modprobe` this on cluster nodes. We can't (and
shouldn't) call modprobe from a library, but a clear pointer in
the error message saves users a Google search.

### Backend error and panic observability

**The problem.** `worker.handleIO` currently swallows all backend errors
silently. When `Backend.ReadAt`/`WriteAt` returns an error the worker
returns `-EIO` to the kernel but nothing surfaces to Go userspace beyond
a `slog.Default()` log line (and only for panics — ordinary errors are
fully silent). There is no way for the caller of `ublk.New` to know the
device is degraded or to trigger cleanup.

**Why you can't call `dev.Close()` directly from the IO path.**
`Close()` calls `shutdown()`, which calls `w.ioRing.Cancel()` and then
`wg.Wait()`. The worker goroutine executing `handleIO` is the same
goroutine counted in the `WaitGroup`, so `wg.Wait()` deadlocks if called
from inside the worker. Any notification must be *asynchronous* — the
worker signals outward, and a separate goroutine calls `Close()`.

**Options considered:**

1. **`WithErrorHandler(fn func(err error))` option** — the library calls
   `fn` from the worker on any backend error or recovered panic. `fn`
   must be non-blocking (e.g. send to a buffered channel, set an
   `atomic.Value`). The user's goroutine monitors that and calls
   `dev.Close()`. Small, additive, follows the existing `WithLogger`
   option shape. Gives the caller policy control (close on first error,
   close after N, only on writes, etc.). One downside: two separate hooks
   (`WithLogger` for structured log output, `WithErrorHandler` for
   programmatic reaction) feel redundant and raise the question of whether
   `WithLogger` is even necessary once there is a proper handler.

2. **`Device.Done() <-chan error` method** — the library closes (or sends
   on) an internal channel the first time any worker error occurs,
   carrying the first non-nil error as the channel value. Caller does
   `select { case err := <-dev.Done(): ... }` alongside their own work.
   Idiomatic Go (mirrors `context.Done()`, `net/http.Server.Shutdown`
   notifications, etc.). Slightly more opinionated than a callback:
   only the *first* error is surfaced, subsequent ones are dropped
   (acceptable if the policy is "close on first error"). Channel
   management needs care across `Close()` calls.

3. **Backend-wrapper approach (no library changes)** — user wraps their
   `Backend` in a proxy that counts errors and fires a notification. No
   library surface change required. Awkward in practice: the wrapper
   needs a reference to `*Device` (which doesn't exist until `New`
   returns), requiring a deferred setter on the wrapper, and the wrapper
   can't distinguish panics recovered by the library from ordinary errors.

**Open question: is `WithLogger` still needed?**
If we add a proper error callback or `Done()` channel, callers can log
errors themselves with whatever structure and routing they prefer. The
current `Logger` interface exists solely so the panic-recovery path has
*somewhere* to emit before returning `-EIO`. It may be better to fold
panic information into the error passed to the handler and drop
`WithLogger` entirely, keeping the public API surface smaller. This is
worth deciding before implementing either option.

**Recommendation.** Implement option 2 (`Done() <-chan error`) as the
primary interface — it composes naturally with `context`, `select`, and
`errgroup`. Add the first-error value so callers see *why* the device
failed, not just *that* it failed. Reconsider `WithLogger` at the same
time and likely remove it.

**Connection to metrics.** The error count by errno that the planned
`Metrics` interface would expose (see "Metrics interface" below) is
essentially the same counter as the error handler would fire. When both
features are implemented, the error handler becomes the hook that drives
both the `Metrics.RecordIO(…, err)` call *and* the `Done()` channel
close. Design them together.

### Safety review note for contributors

Document the unsafe-pointer discipline in the package: any address
embedded in a SQE (`cmd.Addr`, buffer pointers in IO commands) must be
either heap-anchored through a long-lived field or pinned via
`runtime.Pinner` for the duration of the syscall. The bug we just fixed
in `addDev`/`setParams` is the canonical example. See
[semistrict's `ctrl.go`](https://github.com/semistrict/go-ublk/blob/main/ctrl.go)
which uses `runtime.Pinner` consistently.

## Build

### CGO for kernel constants

Import ublk/io_uring constants and struct sizes directly from the kernel's
`linux/ublk_cmd.h` header via CGO instead of hardcoding them. Eliminates the
risk of constants drifting from the running kernel.

## Features

### Configurable block size and queue depth

The current API takes only `size`. Add a `Config` struct or options to expose:

- block size (currently hardcoded 512)
- queue depth (currently hardcoded 128)
- max IO size (currently hardcoded 128KB)
- number of queues (currently hardcoded 1)

### Multiple queues

One IO queue per CPU with `UBLK_CMD_GET_QUEUE_AFFINITY` and pinned worker
goroutines. Required for higher throughput.

### Flusher / Discarder / WriteZeroer

Optional interfaces the backend can implement for richer block semantics:

```go
type Flusher interface { Flush() error }
type Discarder interface { Discard(off, length int64) error }
type WriteZeroer interface { WriteZeroes(off, length int64) error }
```

Set `UBLK_ATTR_VOLATILE_CACHE` when backend implements `Flusher`, declare
`MaxDiscardSectors` when it implements `Discarder`, etc.

Cross-ref: semistrict's [`ReaderAtHandler`](https://github.com/semistrict/go-ublk/blob/main/easy.go)
already implements this exact pattern (`Flusher`, `Syncer`, `Discarder`,
`Zeroer`) with optional-interface assertion. Worth mirroring the API
shape so users have a familiar surface.

### Diagnostic / introspection commands

Wire up the read-only control commands so callers can inspect device
state without round-tripping through sysfs:

- `UBLK_CMD_GET_DEV_INFO` / `GET_DEV_INFO2`
- `UBLK_CMD_GET_PARAMS` (we currently only have SET_PARAMS)
- `UBLK_CMD_GET_QUEUE_AFFINITY`
- `UBLK_CMD_GET_FEATURES` (lets the library detect kernel capability
  matrix at runtime instead of trial-and-error like our current
  ioctl-encoded → legacy fallback in `addDev`)

Cross-ref: semistrict implements all of these in
[`ctrl.go`](https://github.com/semistrict/go-ublk/blob/main/ctrl.go) and
[`affinity.go`](https://github.com/semistrict/go-ublk/blob/main/affinity.go).

### Update size / quiesce / try-stop

Newer ublk control commands worth surfacing once we have a stable
multi-queue base:

- `UBLK_U_CMD_UPDATE_SIZE` (`UBLK_F_UPDATE_SIZE`) — live device resize.
- `UBLK_U_CMD_QUIESCE_DEV` (`UBLK_F_QUIESCE`) — pause IO for live
  server upgrade.
- `UBLK_U_CMD_TRY_STOP_DEV` (`UBLK_F_SAFE_STOP_DEV`) — only stop if no
  openers (avoids the documented "leaked fd → Close hangs forever"
  footgun without changing semantics the way DEL_DEV_ASYNC does).

### User Copy Mode

Data transfer via `pread`/`pwrite` on the char device instead of mmap buffers.
Set `UBLK_F_USER_COPY`. Required for unprivileged mode.

### Unprivileged Devices

Allow non-root users to create ublk devices (`UBLK_F_UNPRIVILEGED_DEV`).
Requires udev rules for `/dev/ublk-control` permissions and user-copy mode.

### User Recovery

Device survives server crashes and can be recovered by a new process
(`UBLK_F_USER_RECOVERY`, `UBLK_F_USER_RECOVERY_REISSUE`).

### COW (Copy-on-Write) Backend

Support for overlay-based backends where reads can come from a shared base
image and writes go to a per-device overlay.

### Zoned Block Device Support

Expose the device as a zoned block device (`UBLK_F_ZONED`) for SMR/ZNS
workloads.

## Performance

### Zero-Copy Mode

Eliminate buffer copies between kernel and userspace by registering IO buffers
with io_uring (`UBLK_F_SUPPORT_ZERO_COPY`). Requires a backend that exposes a
fixed file descriptor.

### Auto Buffer Registration

Kernel-managed buffer registration (`UBLK_F_AUTO_BUF_REG`). Simpler than manual
zero-copy.

### Single-syscall IO loop

Replace the current two-syscall worker loop (`Submit()` + `epoll_wait` in
`WaitCQE`) with a single `io_uring_enter(fd, count, 1, GETEVENTS)` call that
both submits SQEs and waits for the next CQE. The reference C implementation
(ublksrv) uses `io_uring_submit_and_wait_timeout()` which does exactly this.

Benefits:

- Saves one syscall per IO iteration (currently Submit → epoll_wait → two
  kernel transitions)
- Explicitly processes io_uring task work via GETEVENTS (currently relies on
  implicit task_work processing during syscall return, which only works
  without `IORING_SETUP_DEFER_TASKRUN`)
- Enables `DEFER_TASKRUN` support (see below)

Requires reworking the cancellation mechanism: currently `Ring.Cancel()` uses
an eventfd registered with epoll to wake `WaitCQE`. With the single-syscall
approach, cancellation would need to use either an `IORING_OP_ASYNC_CANCEL`
SQE, a timeout, or a registered eventfd read SQE in the io_uring itself.

Cross-ref: ublksrv uses `io_uring_submit_and_wait_timeout()` with `wait_nr=1`.

### io_uring tuning

- `IORING_SETUP_SINGLE_ISSUER` + `IORING_SETUP_DEFER_TASKRUN` on the IO ring.
  **Note**: `DEFER_TASKRUN` requires using `SubmitAndWait` (with GETEVENTS) in
  the worker loop instead of plain `Submit()`, because deferred task work is
  only processed during `io_uring_enter` with GETEVENTS — not during syscall
  return. Without this change, COMMIT_AND_FETCH_REQ completions would stall.
  Prerequisite: the single-syscall IO loop above.
- Fixed file registration for the char device fd (avoids per-IO fget/fput)
- `IORING_SETUP_NO_SQARRAY` — skip SQ array indirection entirely (kernel 6.6+).
  Currently `flushSQ()` writes identity-mapped entries to `sqArray` on every
  flush; with `NO_SQARRAY` these writes are unnecessary and the mmap is smaller.
- Batch CQE processing (already partially done)

### Batch IO mode

`UBLK_F_BATCH_IO` (kernel 6.x+) — three-phase protocol with multishot fetch
and batched commits. The C reference (ublksrv) implements double-buffered
commit arrays: while buffer[0] commits to the kernel, buffer[1] fills with new
results, then swap. Significantly reduces per-IO overhead for high-IOPS
workloads.

Cross-ref: ublksrv PR #177 adds UBLK_F_BATCH_IO support.

### Non-blocking close (DEL_DEV_ASYNC)

`UBLK_CMD_DEL_DEV_ASYNC` (kernel 6.1+) marks the device for deletion and
returns immediately — `/dev/ublkbN` disappears later when the refcount drops.
This avoids the current behavior where `Device.Close()` blocks in
`del_gendisk` until all user fds to the block device are released. The async
variant changes Close's semantics (the node may still exist on return) but
prevents the "leaked fd → Close hangs forever" footgun.

Cross-ref: ublksrv supports this; we document the issue in AGENTS.md.

### Idle buffer page discard

After an idle timeout (e.g. 20 seconds with no IO), call
`madvise(MADV_DONTNEED)` on IO buffer pages to release physical memory back to
the OS. The C reference (ublksrv) does this when
`io_uring_submit_and_wait_timeout` returns `-ETIME` with zero CQEs and an
empty SQ. Useful for long-lived devices that experience bursty IO patterns.

Cross-ref: ublksrv idle detection in `ublksrv_process_io()`.

### CQ overflow detection

Monitor the CQ overflow counter (`sqOffsets.Overflow`) and log a warning or
return a diagnostic error when CQ entries are dropped. Currently we rely on the
CQ being large enough (2× queue depth = 256 entries) and processing CQEs fast
enough, but under pathological conditions (slow backend + full queue depth)
overflow is theoretically possible.

For the `IORING_SETUP_CQ_NODROP` variant, overflow requires calling
`io_uring_enter` with `GETEVENTS` to flush backed-up entries.

Cross-ref: tokio-rs/io-uring issue #302 documents CQ overflow with NODROP.

### Pre-populate SQ array at setup

Fill `sqArray` with identity-mapped entries once during ring setup instead of
writing them on every `flushSQ()` call. liburing does this — it populates
`sq_array[i] = i` at init time and never touches the array during flush.
Micro-optimization; reduces per-flush writes by `queue_depth` stores.

### THP-aligned IO

For mmap workloads with Transparent Huge Pages, increase max IO size to 2MB
aligned with THP pages.

### Tunable sysfs parameters

After device creation, adjust via sysfs:

```bash
echo 2048 > /sys/block/ublkb0/queue/max_sectors_kb
echo 2048 > /sys/block/ublkb0/queue/read_ahead_kb
```

## Observability

### Metrics interface

Add an optional `Metrics` interface (or hook) so callers can instrument
the IO path without importing a specific metrics library:

```go
type Metrics interface {
    RecordIO(op string, bytes int, latency time.Duration, err error)
}
```

Pass via a `WithMetrics(Metrics) Option` (same pattern as `WithLogger`).
This lets users wire Prometheus, OpenTelemetry, or any other collector
without the library taking a hard dependency on any of them. The hot path
should check once at startup whether the interface is non-nil rather than
doing an interface call on every IO when metrics are disabled.

Useful counters to expose: ops/sec per opcode (read/write/flush/discard),
bytes/sec, per-op latency histogram, error count by errno, queue depth
utilisation, backend panic count.

## Testing

### Property-based / model-based state machine tests (`rapid`)

Add [`pgregory.net/rapid`](https://pkg.go.dev/pgregory.net/rapid) as a test
dependency and write a state machine test (via `rapid.T.Repeat`) for the
device lifecycle and data plane.

The test defines:
- A **model** — a simple in-memory map of `offset → bytes` representing what
  the device should contain.
- A set of **commands** generated randomly by `rapid`: `Write(offset, data)`,
  `Read(offset, len)`, `Fsync`, `Close`, `Create` (new device), plus
  multi-device commands to check cross-device isolation.
- **Invariants** checked after every command:
  - A `Read` must return the bytes from the most recent `Write` at that range.
  - Bytes written to device N must never appear on device M.
  - `Close` must terminate within a bounded time and leave no device node.
  - `Close` called multiple times must not panic or hang.

`rapid` automatically **shrinks** any failing sequence to its minimal
reproducer, which is the primary advantage over the existing
`TestTortureRandomIO` (which is a long-running soak, not a shrinker).

The test lives alongside the existing integration tests
(`//go:build integration`, runs as root with `ublk_drv` loaded). Duration and
parallelism are controlled by env vars so it fits in the normal CI timeout by
default.

**Why this is distinct from `TestTortureRandomIO`:** torture is a
long-running soak with fixed structure (N workers, disjoint regions). The
`rapid` state machine generates arbitrary command sequences including
lifecycle transitions (create/close mid-stream) and multiple devices, and
produces a reproducible minimal failing case when it finds a bug.

### Go native fuzz tests for `ublk/uring/`

Add `FuzzXxx` functions to `ublk/uring/uring_test.go` targeting the ring
management code. Unlike the integration tests above, these run without root or
`ublk_drv` and can be run overnight with `go test -fuzz=.`:

- **`FuzzRingSubmit`** — takes a `[]byte` seed, interprets it as a sequence of
  (opcode, userData) pairs, submits them as NOP SQEs, drains the CQE ring,
  and checks that every submitted `UserData` is returned exactly once. Verifies
  that `flushSQ`, `WaitCQE`, and the ring head/tail arithmetic don't corrupt
  or lose entries under arbitrary submission patterns.
- **`FuzzRingCancel`** — drives concurrent `Submit` and `Cancel` from two
  goroutines with random interleaving seeded by the fuzzer input. Checks that
  `WaitCQE` always returns after `Cancel` is called, even when the CQ has
  zero or many ready entries (regression guard for the fast-path cancel race
  described in AGENTS.md).

These tests run as normal unit tests (`go test ./ublk/uring/`) using seed
corpus entries; the `-fuzz` flag enables coverage-guided mutation. No kernel
or root access needed. Corpus entries that find new coverage are committed to
`ublk/uring/testdata/fuzz/`.

### Linearizability checking (extension of the `rapid` state machine test)

Once the `rapid` state machine test (above) is in place, instrument it to
record a history of operations with wall-clock timestamps and result values,
then feed the history to
[`anishathalye/porcupine`](https://github.com/anishathalye/porcupine) — a Go
linearizability checker.

This formally answers: "does every read return the value of the last completed
write that precedes it in real time, for all concurrent orderings?" — the same
check Jepsen runs for distributed databases, applied here to a single block
device with concurrent callers.

The check is a post-processing step on the same test run; it adds no test
infrastructure beyond adding `porcupine` as a test dependency and a history
recorder around the model commands. If the `rapid` state machine tests pass
but the linearizability check fails, it means concurrent reads and writes
are producing a result that has no valid sequential explanation — a subtle
correctness bug not caught by per-operation assertions.

### Syzkaller for kernel-level ublk fuzzing

[syzkaller](https://github.com/google/syzkaller) is Google's
coverage-guided Linux syscall fuzzer. It generates random sequences of ioctl
calls against kernel interfaces using hand-written descriptions (syzlang),
running inside VMs with a specially-built kernel (kcov + KASAN + KCSAN).

For ublk-go this means writing syzlang descriptions for:
- `UBLK_CMD_ADD_DEV`, `UBLK_CMD_START_DEV`, `UBLK_CMD_STOP_DEV`,
  `UBLK_CMD_DEL_DEV`, `UBLK_CMD_SET_PARAMS` on `/dev/ublk-control`
- `UBLK_IO_FETCH_REQ`, `UBLK_IO_COMMIT_AND_FETCH_REQ` on `/dev/ublkcN` via
  `IORING_OP_URING_CMD`

Running syzkaller against these interfaces would find kernel bugs in `ublk_drv`
triggered by the specific usage patterns of ublk-go (e.g., racing ADD_DEV with
DEL_DEV, submitting malformed COMMIT payloads, crashing the process during
START_DEV). Any bugs found would be reported upstream to linux-block and cc'd
to Ming Lei.

Setup cost is high (instrumented kernel build, VM image, syz-manager
configuration) and the output is kernel bugs rather than library bugs. Worth
doing if ublk-go is deployed in production at scale and you want confidence in
the kernel driver itself.
