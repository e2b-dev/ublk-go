# ublk-go

A Go implementation of the Linux ublk (userspace block device) driver.

## Overview

This library allows you to create block devices in userspace by implementing simple `ReadAt` and `WriteAt` functions. The ublk driver forwards block I/O requests from the kernel to your Go code, allowing you to implement custom storage backends.

## Requirements

- Linux kernel 6.0+ with ublk driver enabled (6.1+ for best performance)
- Go 1.25+
- Root privileges (for creating block devices)
- liburing development headers (via pkg-config) and CGO-enabled Go toolchain

## Installation

```bash
go get github.com/ublk-go/ublk
```

## Quick Start

```go
package main

import (
    "fmt"
    
    "github.com/ublk-go/ublk/ublk"
)

// Implement the Backend interface
type MyBackend struct {
    // your storage implementation
}

func (b *MyBackend) ReadAt(p []byte, off int64) (n int, err error) {
    // Read len(p) bytes from offset off
    return len(p), nil
}

func (b *MyBackend) WriteAt(p []byte, off int64) (n int, err error) {
    // Write len(p) bytes at offset off
    return len(p), nil
}

func main() {
    backend := &MyBackend{}
    
    config := ublk.DefaultConfig()
    config.Size = 1024 * 1024 * 1024 // 1GB
    config.BlockSize = 512
    
    dev, err := ublk.CreateDevice(backend, config)
    if err != nil {
        panic(err)
    }
    
    // Device is now available at /dev/ublkb{N}
    fmt.Printf("Device: %s\n", dev.BlockDevicePath())
    
    // ... use the device ...
    
    // Clean up
    dev.Delete()
}
```

## API

### Backend Interface

Your storage backend must implement:

```go
type Backend interface {
    ReadAt(p []byte, off int64) (n int, err error)
    WriteAt(p []byte, off int64) (n int, err error)
}
```

### Config

```go
type Config struct {
    BlockSize    uint64  // Logical block size (typically 512 or 4096)
    Size         uint64  // Total device size in bytes
    MaxSectors   uint32  // Maximum sectors per request
    MaxIOBufBytes uint32 // Maximum IO buffer size per request (bytes)
    NrHWQueues   uint16  // Number of hardware queues
    QueueDepth   uint16  // Depth of each queue
    
    // Advanced features (kernel 6.x+)
    ZeroCopy     bool    // Enable zero-copy mode (requires FixedFileBackend)
    AutoBufReg   bool    // Automatic buffer registration (requires kernel support)
    UserCopy     bool    // Use pread/pwrite path for data transfer
    MaxDiscardSectors uint32 // Max sectors per discard request
    MaxDiscardSegments uint32 // Max segments per discard request
}
```

Notes:
- `ZeroCopy` requires a backend that implements `FixedFileBackend`. It registers request buffers into an
  io_uring fixed-buffer table and uses `IORING_OP_{READ,WRITE}_FIXED` for data transfer.
- `AutoBufReg` is the preferred zero-copy mode when supported by the kernel.

### Optional Backend Interfaces

For advanced operations, implement these optional interfaces:

```go
// Flusher for cache flush support
type Flusher interface {
    Flush() error
}

// Discarder for TRIM/discard support
type Discarder interface {
    Discard(offset, length int64) error
}

// WriteZeroer for efficient zero-filling
type WriteZeroer interface {
    WriteZeroes(offset, length int64) error
}

// FixedFileBackend enables zero-copy IO using io_uring fixed buffers.
// Required when Config.ZeroCopy is enabled.
type FixedFileBackend interface {
    FixedFile() (*os.File, error)
}
```

### Device Methods

- `BlockDevicePath() string` - Returns the path to the block device (e.g., `/dev/ublkb0`)
- `DeviceID() int` - Returns the device ID
- `Stop() error` - Stops the device
- `Delete() error` - Removes the device

## Example

See `example/main.go` for a complete example with an in-memory backend.

## Status

Go implementation with CGO-backed io_uring constants (liburing headers).

See `BUILD.md` for build instructions and `ARCHITECTURE.md` for design details.

## References

- [Linux ublk documentation](https://docs.kernel.org/block/ublk.html)
- [ublksrv reference implementation](https://github.com/ublk-org/ublksrv)
- [libublk-rs Rust implementation](https://github.com/ublk-org/libublk-rs)
- [LWN: Zero-copy I/O for ublk](https://lwn.net/Articles/926118/)

## License

MIT
