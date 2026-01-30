# Architecture

## Overview

A pure Go library for creating Linux userspace block devices using the ublk driver.
No CGO or C dependencies required.

## Design Principles

1. **Pure Go** - No CGO required, direct syscalls for io_uring
2. **Zero Allocations** - Hot paths are allocation-free
3. **Clean Separation** - Control plane, IO plane, and buffer management are modular
4. **Thread Safety** - All public APIs are safe for concurrent use

## Package Structure

```
ublk/
├── api.go                   # High-level API (CreateDevice, Backend interface)
├── device.go                # Device lifecycle management
├── ring.go                  # Pure Go io_uring implementation
├── io_worker.go             # Per-queue IO handling
├── buffer.go                # Buffer management
├── request.go               # Request parsing
├── types.go                 # ublk constants and types
├── uring_types.go           # io_uring struct definitions
├── uring_constants_pure.go  # Pure Go constants (default)
├── uring_constants.go       # CGO constants (optional)
├── ublk_cmd.go              # ublk command structures
├── stats.go                 # IO statistics and observability
└── log.go                   # Configurable logging
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

Pure Go io_uring using direct syscalls:

- `SYS_IO_URING_SETUP` - Create ring instance
- `SYS_IO_URING_ENTER` - Submit and wait for completions
- `IORING_OP_URING_CMD` - ublk passthrough commands

### Buffer Management (`buffer.go`)

Memory layout of mmap'd area from `/dev/ublkcN`:

```
┌─────────────────────┐  offset 0
│  IO Descriptors     │  queueDepth × sizeof(UblksrvIODesc)
├─────────────────────┤
│  Request Data       │  queueDepth × 256 bytes
├─────────────────────┤
│  Buffers            │  MaxIOBufBytes
└─────────────────────┘
```

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

Zero-allocation hot paths verified by benchmarks:

```
BenchmarkGetSetIODesc           617M ops    2.1 ns/op   0 allocs
BenchmarkBufferManagerGetIODescBuffer  578M ops  2.0 ns/op  0 allocs
BenchmarkParseRequest           812M ops    1.4 ns/op   0 allocs
BenchmarkUblkIOCommandToBytes  1000M ops    1.0 ns/op   0 allocs
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
| `UBLK_F_USER_RECOVERY` | Survive server restarts |

## Observability

The `Stats` struct provides atomic counters:

- Operation counts: reads, writes, flushes, discards, write_zeroes
- Byte counts: bytes_read, bytes_written
- Error counts: read_errors, write_errors, other_errors

Access via `device.Stats().Snapshot()`.
