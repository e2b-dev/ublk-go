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

### Control Plane (`device.go`)

Manages device lifecycle via ioctl on `/dev/ublk-control`:

| Operation | ioctl | Description |
|-----------|-------|-------------|
| Add | `UBLK_CMD_ADD_DEV` | Register device with kernel |
| SetParams | `UBLK_CMD_SET_PARAMS` | Configure size, block size |
| Start | `UBLK_CMD_START_DEV` | Activate device |
| Stop | `UBLK_CMD_STOP_DEV` | Deactivate device |
| Delete | `UBLK_CMD_DEL_DEV` | Remove device |

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
- 1 goroutine per tag (request slot)
- Backend `ReadAt`/`WriteAt` called from worker goroutines
- **Backend implementations must be thread-safe**

## Error Handling

- Control operations return wrapped errors with context
- IO errors set `EndIO` flag in descriptor
- Configurable logging via `DefaultLogger` variable

## Performance

Zero-allocation hot paths verified by benchmarks:

```
BenchmarkGetSetIODesc           615M ops    1.9 ns/op   0 allocs
BenchmarkBufferManagerGetIODescBuffer  585M ops  2.0 ns/op  0 allocs
BenchmarkParseRequest          1000M ops    0.2 ns/op   0 allocs
```
