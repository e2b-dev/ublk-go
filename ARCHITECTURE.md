# Architecture

## Overview

A Go library for creating Linux userspace block devices using the ublk driver.
Uses CGO to import io_uring constants from liburing headers.

## Design Principles

1. **Direct syscalls** - io_uring via syscalls, constants from liburing headers
2. **Zero Allocations** - Hot paths are allocation-free
3. **Clean Separation** - Control plane, IO plane, and buffer management are modular
4. **Thread Safety** - All public APIs are safe for concurrent use

## Package Structure

```
ublk/
├── api.go                   # High-level API (CreateDevice, Backend interface)
├── device.go                # Device lifecycle management
├── control_ring.go          # Control-plane io_uring for /dev/ublk-control
├── ring.go                  # io_uring implementation (syscalls)
├── io_worker.go             # Per-queue IO handling
├── ioctl.go                 # Ioctl encoding helpers and control structs
├── types.go                 # ublk constants and types
├── uring_types.go           # io_uring struct definitions
├── uring_constants.go       # CGO constants (liburing headers)
├── ublk_cmd.go              # ublk command structures
├── stats.go                 # IO statistics and observability
```

## Data Flow

```
┌─────────────────┐     ┌──────────────────┐     ┌─────────────────┐
│   Application   │────▶│   ublk-go        │────▶│  Your Backend   │
│  (filesystem,   │     │                  │     │  (ReadAt/       │
│   dd, etc.)     │     │  ┌────────────┐  │     │   WriteAt)      │
└────────┬────────┘     │  │ io_uring   │  │     └─────────────────┘
         │              │  │ ring.go    │  │
         ▼              │  └─────┬──────┘  │
┌─────────────────┐     │        │         │
│  /dev/ublkbN    │     │  ┌─────▼──────┐  │
│  (block device) │     │  │ io_worker  │  │
└────────┬────────┘     │  │ goroutines │  │
         │              │  └────────────┘  │
         ▼              └──────────────────┘
┌─────────────────┐
│  Linux Kernel   │
│  ublk driver    │
└─────────────────┘
```

## Components

### Control Plane (`device.go`, `control_ring.go`)

Manages device lifecycle via io_uring URING_CMD on `/dev/ublk-control`:

| Operation | Command | Description |
|-----------|---------|-------------|
| Add | `UBLK_U_CMD_ADD_DEV` | Register device with kernel |
| SetParams | `UBLK_U_CMD_SET_PARAMS` | Configure size, block size |
| Start | `UBLK_U_CMD_START_DEV` | Activate device |
| Stop | `UBLK_U_CMD_STOP_DEV` | Deactivate device |
| Delete | `UBLK_U_CMD_DEL_DEV` | Remove device |

**Note:** Control commands use io_uring passthrough (`IORING_OP_URING_CMD`), not ioctl.
Requires `IORING_SETUP_SQE128` for the 80-byte command payload.

### IO Plane (`ring.go`, `io_worker.go`)

io_uring using direct syscalls:

- `SYS_IO_URING_SETUP` - Create ring instance
- `SYS_IO_URING_ENTER` - Submit and wait for completions
- `IORING_OP_URING_CMD` - ublk passthrough commands

### Buffer Management (`io_worker.go`)

The ublk driver exposes a shared mmap region containing an array of
`UblksrvIODesc` structures (one per tag). ublk-go mmaps only this
descriptor array and allocates per-tag IO buffers in Go memory.

```
mmap(/dev/ublkcN)
┌────────────────────────────────────────────┐
│ IO Descriptors (queueDepth entries)        │
└────────────────────────────────────────────┘

Go heap
┌────────────────────────────────────────────┐
│ Per-tag IO buffers (queueDepth × MaxIOBufBytes) │
└────────────────────────────────────────────┘
```

For each FETCH/COMMIT command, the buffer address is passed via
`ublksrv_io_cmd.addr` so the kernel can copy data to/from that buffer.
When `UBLK_F_USER_COPY` is enabled, ublk-go skips buffer addresses and
uses `pread()/pwrite()` on `/dev/ublkcN` instead.

### High-Level API (`api.go`)

- `CreateDevice(backend, config)` - One-call device creation
- `Backend` interface - Just implement `ReadAt`/`WriteAt`
- `Config` struct - Device size, block size, queue configuration

## Concurrency Model

- 1 io_uring instance per hardware queue
- 1 goroutine per queue (single issuer pattern)
- Batched CQE processing (up to 16 completions per submit)
- Fixed file descriptors registered to reduce per-IO overhead
- Backend `ReadAt`/`WriteAt` called from worker goroutines
- **Backend implementations must be thread-safe**

## Error Handling

- Control operations return wrapped errors with context
- IO errors set `EndIO` flag in descriptor
- Configurable logging via `DefaultLogger` variable

## Performance

Hot paths are covered by benchmarks (see `ublk/benchmark_test.go`):

```
BenchmarkGetSetIODesc
BenchmarkUblkIOCommandToBytes
BenchmarkRoundUpPow2
BenchmarkTestBackendReadWrite
```

### io_uring Optimizations

| Feature | Kernel | Benefit |
|---------|--------|---------|
| `IORING_SETUP_SINGLE_ISSUER` | 6.0+ | Single-thread optimization |
| `IORING_SETUP_DEFER_TASKRUN` | 6.1+ | Reduced context switches |
| Fixed file registration | 5.1+ | Avoids per-IO fd lookup |
| Batched CQE processing | - | Fewer io_uring_enter() calls |

### ublk Features

| Feature | Description |
|---------|-------------|
| `UBLK_F_SUPPORT_ZERO_COPY` | Register buffers to avoid copies |
| `UBLK_F_AUTO_BUF_REG` | Automatic buffer management |
| `UBLK_F_USER_COPY` | Skip copy for FLUSH/DISCARD |

## Observability

The `Stats` struct provides atomic counters:

- Operation counts: reads, writes, flushes, discards, write_zeroes
- Byte counts: bytes_read, bytes_written
- Error counts: read_errors, write_errors, other_errors

Access via `device.Stats().Snapshot()`.
