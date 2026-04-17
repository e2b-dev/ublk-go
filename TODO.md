# Future Work

Features and optimizations to add on top of the current minimal implementation.

## Performance

### Zero-Copy Mode

Eliminates buffer copies between kernel and userspace by registering IO buffers
with io_uring. Requires a backend that exposes a fixed file descriptor
(`FixedFileBackend`).

```go
config.ZeroCopy = true
```

### Auto Buffer Registration

Kernel-managed buffer registration (UBLK_F_AUTO_BUF_REG). Simpler than manual
zero-copy — the kernel automatically registers/unregisters request buffers.

### io_uring Tuning

- `IORING_SETUP_SINGLE_ISSUER` + `IORING_SETUP_DEFER_TASKRUN` for the IO ring
  (requires `runtime.LockOSThread`, which we already do)
- Fixed file registration for the char device fd (avoids fget/fput per IO)
- Batch CQE processing (already partially done)

### THP-Aligned IO

For mmap workloads with Transparent Huge Pages, increase `MaxSectors` to allow
2MB IOs aligned with THP:

```go
config.MaxSectors = 512       // 2MB with 4K blocks
config.MaxIOBufBytes = 2 << 20
config.QueueDepth = 32        // reduce to control memory
```

### Tunable sysfs Parameters

After device creation, adjust via sysfs:

```bash
echo 2048 > /sys/block/ublkb0/queue/max_sectors_kb
echo 2048 > /sys/block/ublkb0/queue/read_ahead_kb
```

## Features

### User Copy Mode

Data transfer via pread/pwrite on the char device instead of mmap buffers.
Required for unprivileged mode. Set `UBLK_F_USER_COPY`.

### Unprivileged Devices

Allow non-root users to create ublk devices (`UBLK_F_UNPRIVILEGED_DEV`).
Requires udev rules for `/dev/ublk-control` permissions and user-copy mode.

### User Recovery

Device survives server crashes and can be recovered by a new process
(`UBLK_F_USER_RECOVERY`, `UBLK_F_USER_RECOVERY_REISSUE`).

### COW (Copy-on-Write) Backend

Support for overlay-based backends where reads can come from a shared base image
and writes go to a per-device overlay. Previous API:

```go
type COWBackend interface {
    Backend
    Overlay() (*os.File, error)
    ClassifyRange(off, length int64) (allDirty, allClean bool)
    ReadBaseAt(p []byte, off int64) (n int, err error)
}
```

### Extended Backend Interfaces

Optional interfaces the backend can implement for richer block semantics:

- `FuaWriter` — write with Force Unit Access (bypasses volatile cache)
- `SparseReader` — report zero regions to avoid unnecessary reads
- `ReaderWithFlags` / `WriterWithFlags` — pass raw ublk IO flags to backend
- `Discarder` — handle DISCARD (trim) requests (already supported)
- `WriteZeroer` — handle WRITE_ZEROES requests (already supported)

### Zoned Block Device Support

Expose the device as a zoned block device (`UBLK_F_ZONED`) for SMR/ZNS
workloads.

### Multiple Queue Affinity

Use `UBLK_CMD_GET_QUEUE_AFFINITY` to pin worker goroutines to the CPUs the
kernel assigns to each queue.

## Config Fields to Add

Fields from the previous implementation that should be re-added to `Config`:

```go
type Config struct {
    // ... existing fields ...

    MaxSectors    uint32 // max IO size in 512-byte sectors (default: 256 = 128KB)
    MaxIOBufBytes uint32 // per-tag buffer size (default: computed from MaxSectors)
    ZeroCopy      bool   // enable zero-copy (requires FixedFileBackend)
    AutoBufReg    bool   // enable auto buffer registration
    UserCopy      bool   // use pread/pwrite for data transfer
    Unprivileged  bool   // create unprivileged device
}
```

## msync Optimization Notes

For mmap workloads, batch syncs for better performance:

```go
// Slow: sync after each write
for _, item := range items {
    copy(data[off:], item)
    unix.Msync(data[off:off+len(item)], unix.MS_SYNC)
}

// Fast: batch writes, sync once
for _, item := range items {
    copy(data[off:], item)
    off += len(item)
}
unix.Msync(data[start:off], unix.MS_SYNC)
```

Prefer `MS_ASYNC` when strict durability isn't needed.
