# Performance Optimizations

This document covers performance tuning for ublk-go, particularly for mmap-based access patterns.

## Transparent Huge Pages (THP) Optimization

When applications mmap a ublk block device (e.g., `/dev/ublkb0`), the Linux kernel uses the page cache. With THP enabled, the kernel may allocate 2MB huge pages instead of 4KB pages.

### How THP Interacts with ublk

1. **Page cache allocation**: The kernel may use 2MB folios for caching mmap'd regions
2. **I/O granularity**: Regardless of THP, I/O requests to ublk are limited by `MaxSectors` and `MaxIOBufBytes`
3. **Writeback**: When dirty pages are flushed (via `msync` or background writeback), the block layer respects device limits

### Default Configuration

```go
config := ublk.DefaultConfig()
// MaxSectors: 256 (128KB with 512-byte sectors)
// MaxIOBufBytes: 512KB (default)
```

This means even with THP allocating 2MB pages, I/O to your backend happens in 128KB chunks.

### Optimizing for THP

To align I/O size with THP's 2MB pages:

#### Step 1: Configure MaxSectors

```go
config := ublk.DefaultConfig()
config.BlockSize = 4096    // 4KB blocks (recommended for THP)
config.MaxSectors = 512    // 512 * 4096 = 2MB max I/O
```

#### Step 2: Increase MaxIOBufBytes

```go
config.MaxIOBufBytes = 2 * 1024 * 1024 // 2MB instead of 512KB
```

#### Step 3: Adjust Queue Depth for Memory

Larger buffers consume more memory per queue:

| MaxIOBufBytes | QueueDepth | Memory per Queue |
|---------------|------------|------------------|
| 512KB         | 128        | 64MB             |
| 2MB           | 128        | 256MB            |
| 2MB           | 32         | 64MB             |

Consider reducing `QueueDepth` when using larger buffers:

```go
config.QueueDepth = 32  // Reduce from 128 to control memory
```

### Post-Creation Tuning via sysfs

Some parameters can be adjusted after device creation:

```bash
# View current settings
cat /sys/block/ublkb0/queue/max_sectors_kb

# Adjust (limited by device-advertised maximum)
echo 2048 > /sys/block/ublkb0/queue/max_sectors_kb

# Read-ahead (helps sequential mmap access)
echo 2048 > /sys/block/ublkb0/queue/read_ahead_kb
```

## msync Optimization

When using mmap with `msync()` for persistence, I/O efficiency depends on:

1. **Sync granularity**: `msync` flushes dirty pages in the specified range
2. **I/O coalescing**: The block layer merges adjacent dirty pages into larger requests
3. **Max request size**: Limited by `max_sectors_kb`

### Recommendations for msync Workloads

```go
config := ublk.DefaultConfig()
config.BlockSize = 4096
config.MaxSectors = 512      // Allow 2MB I/Os
config.NrHWQueues = 2        // Parallel I/O queues
config.QueueDepth = 64       // Balance parallelism and memory
```

### msync Flags

| Flag | Behavior | Use Case |
|------|----------|----------|
| `MS_SYNC` | Synchronous writeback, blocks until complete | Durability guarantee needed |
| `MS_ASYNC` | Schedule writeback, returns immediately | Background persistence |
| `MS_INVALIDATE` | Invalidate cached pages | See backend changes |

For performance, prefer `MS_ASYNC` when possible and batch your syncs:

```go
// Instead of syncing each write:
for i := range items {
    copy(data[offset:], items[i])
    unix.Msync(data[offset:offset+len(items[i])], unix.MS_SYNC) // Slow
}

// Batch writes and sync once:
for i := range items {
    copy(data[offset:], items[i])
    offset += len(items[i])
}
unix.Msync(data[start:offset], unix.MS_SYNC) // Single sync
```

## Block Size Selection

| Block Size | Pros | Cons |
|------------|------|------|
| 512 bytes | Compatible with all tools | More metadata overhead |
| 4096 bytes | Matches page size, better for mmap | Some tools assume 512 |

For mmap-heavy workloads, use 4KB blocks:

```go
config.BlockSize = 4096
```

## Zero-Copy Mode

For high-throughput workloads, enable zero-copy to avoid buffer copies:

```go
config.ZeroCopy = true    // Manual buffer registration
// or (preferred when supported by the kernel)
config.AutoBufReg = true
```

Zero-copy registers I/O buffers with io_uring, eliminating copies between kernel and userspace.

## Queue Configuration

### Number of Queues (NrHWQueues)

- **Single queue (1)**: Simpler, good for sequential workloads
- **Multiple queues (2-4)**: Better for parallel I/O, mmap with multiple threads

```go
config.NrHWQueues = uint16(runtime.NumCPU())  // One per CPU
```

### Queue Depth

Higher depth allows more in-flight I/Os but uses more memory:

```go
config.QueueDepth = 64   // Good balance for most workloads
config.QueueDepth = 256  // High-IOPS workloads
```

## Summary: Recommended Configurations

### General Purpose

```go
config := ublk.DefaultConfig()
config.BlockSize = 4096
config.MaxSectors = 256   // 1MB max I/O
config.NrHWQueues = 2
config.QueueDepth = 128
```

### THP-Optimized mmap

```go
config := ublk.DefaultConfig()
config.BlockSize = 4096
config.MaxSectors = 512   // 2MB max I/O (requires MaxIOBufBytes increase)
config.MaxIOBufBytes = 2 * 1024 * 1024
config.NrHWQueues = 2
config.QueueDepth = 32    // Reduced for memory
```

### High-Throughput Sequential

```go
config := ublk.DefaultConfig()
config.BlockSize = 4096
config.MaxSectors = 512
config.NrHWQueues = 4
config.QueueDepth = 64
config.AutoBufReg = true  // Zero-copy
```

### Low-Latency Random I/O

```go
config := ublk.DefaultConfig()
config.BlockSize = 4096
config.MaxSectors = 32    // Smaller I/Os complete faster
config.NrHWQueues = 4
config.QueueDepth = 256   // High parallelism
```

## Future Considerations

### Empty Page Handling

Currently, all read requests go through the userspace backend, even for regions known to be empty/zeroed. A future optimization could add explicit empty page tracking, allowing the backend to quickly return zeros for pre-declared empty regions without actual I/O. This would be particularly beneficial for sparse devices or after DISCARD operations.
