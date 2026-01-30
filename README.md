# ublk-go

A Go implementation of the Linux ublk (userspace block device) driver.

## Overview

This library allows you to create block devices in userspace by implementing simple `ReadAt` and `WriteAt` functions. The ublk driver forwards block I/O requests from the kernel to your Go code, allowing you to implement custom storage backends.

## Requirements

- Linux kernel 6.0+ with ublk driver enabled
- Go 1.21+
- Root privileges (for creating block devices)

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
    BlockSize   uint64  // Logical block size (typically 512 or 4096)
    Size        uint64  // Total device size in bytes
    MaxSectors  uint32  // Maximum sectors per request
    NrHWQueues  uint16  // Number of hardware queues
    QueueDepth  uint16  // Depth of each queue
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

**Current Status:**

- ✅ Control plane (device creation, configuration) - **Complete**
- ✅ High-level API - **Complete**
- ✅ IO plane (io_uring passthrough) - **Pure Go implementation**

The library is **100% pure Go** with no CGO or C dependencies. It provides:

- Full control plane operations (add, start, stop, delete devices)
- io_uring-based I/O handling with passthrough commands
- Buffer management for efficient data transfer
- Support for multiple queues and concurrent I/O
- Zero-allocation hot paths

See `BUILD.md` for build and test instructions.

## References

- [Linux ublk documentation](https://docs.kernel.org/block/ublk.html)
- [ublksrv reference implementation](https://github.com/ublk-org/ublksrv)
- [libublk-rs Rust implementation](https://github.com/ublk-org/libublk-rs)
- [LWN: Zero-copy I/O for ublk](https://lwn.net/Articles/926118/)

## License

MIT
