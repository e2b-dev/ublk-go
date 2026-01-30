# Implementation Notes

## Overview

This is a **pure Go** implementation of the Linux ublk userspace block device driver.
No CGO or C dependencies are required.

## Components

### Control Plane (`device.go`)

Full implementation of ublk control operations:

- `UBLK_CMD_ADD_DEV` - Register device with kernel
- `UBLK_CMD_SET_PARAMS` - Configure device parameters
- `UBLK_CMD_START_DEV` - Activate device
- `UBLK_CMD_STOP_DEV` - Deactivate device
- `UBLK_CMD_DEL_DEV` - Remove device

### IO Plane (`ring.go`, `io_worker.go`)

Pure Go io_uring implementation using direct syscalls:

- `io_uring_setup` - Create ring instance
- `io_uring_enter` - Submit and wait for completions
- `IORING_OP_URING_CMD` - Passthrough commands for ublk

### Buffer Management (`buffer.go`)

Efficient buffer handling:

- Memory-mapped IO descriptor area
- Request data extraction
- Buffer offset calculations

### High-Level API (`api.go`)

Simple interface for users:

- `CreateDevice()` - One-call device creation
- `Backend` interface - Just implement `ReadAt`/`WriteAt`
- `Config` struct - Device configuration

## io_uring Implementation Details

The implementation uses pure Go syscalls (no liburing):

```go
// Setup ring
syscall.Syscall(unix.SYS_IO_URING_SETUP, entries, params, 0)

// Submit and wait
syscall.Syscall6(unix.SYS_IO_URING_ENTER, fd, toSubmit, minComplete, flags, 0, 0)
```

Memory mapping for rings and SQEs:

```go
unix.Mmap(fd, IORING_OFF_SQ_RING, size, PROT_READ|PROT_WRITE, MAP_SHARED)
```

## Concurrency Model

- One io_uring instance per hardware queue
- One goroutine per tag (request slot)
- Backend must be thread-safe

## Performance

Benchmarks show zero-allocation hot paths:

```
BenchmarkGetSetIODesc           615M ops    1.9 ns/op   0 allocs
BenchmarkParseRequest          1000M ops    0.2 ns/op   0 allocs
```

## References

- [Linux ublk documentation](https://docs.kernel.org/block/ublk.html)
- [ublksrv reference implementation](https://github.com/ublk-org/ublksrv)
- [io_uring documentation](https://kernel.dk/io_uring.pdf)
