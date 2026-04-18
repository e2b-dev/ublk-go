# Future Work

Features and optimizations to add on top of the current minimal implementation.

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
