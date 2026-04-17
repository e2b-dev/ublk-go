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

### io_uring tuning

- `IORING_SETUP_SINGLE_ISSUER` + `IORING_SETUP_DEFER_TASKRUN` on the IO ring
- Fixed file registration for the char device fd (avoids per-IO fget/fput)
- Batch CQE processing (already partially done)

### THP-aligned IO

For mmap workloads with Transparent Huge Pages, increase max IO size to 2MB
aligned with THP pages.

### Tunable sysfs parameters

After device creation, adjust via sysfs:

```bash
echo 2048 > /sys/block/ublkb0/queue/max_sectors_kb
echo 2048 > /sys/block/ublkb0/queue/read_ahead_kb
```
