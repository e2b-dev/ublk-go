# ublk-go

Pure Go library for creating Linux userspace block devices using the ublk driver.

## How It Works

```
Application (dd, mkfs, etc.)
        │
        ▼
  /dev/ublkbN (block device)
        │
        ▼
  Linux kernel ublk driver
        │
        ▼
  ublk-go (io_uring)
        │
        ▼
  Your Backend (ReadAt/WriteAt)
```

The kernel forwards block I/O requests to your Go code via io_uring. You implement `ReadAt` and `WriteAt`, ublk-go handles the rest.

## Requirements

- Linux kernel 6.0+ with ublk driver
- Root privileges (CAP_SYS_ADMIN)

## Quick Example

```go
package main

import (
    "fmt"
    "github.com/e2b-dev/ublk-go/ublk"
)

type MemoryBackend struct {
    data []byte
}

func (b *MemoryBackend) ReadAt(p []byte, off int64) (int, error) {
    copy(p, b.data[off:])
    return len(p), nil
}

func (b *MemoryBackend) WriteAt(p []byte, off int64) (int, error) {
    copy(b.data[off:], p)
    return len(p), nil
}

func main() {
    size := int64(16 * 1024 * 1024) // 16MB
    backend := &MemoryBackend{data: make([]byte, size)}

    config := ublk.DefaultConfig()
    config.Size = uint64(size)

    dev, err := ublk.New(backend, config)
    if err != nil {
        panic(err)
    }
    defer dev.Delete()

    fmt.Printf("Device ready: %s\n", dev.BlockDevicePath())
    select {} // keep running
}
```

Run with:

```bash
sudo modprobe ublk_drv
sudo go run main.go
```

## Key Points

- **Direct syscalls** - io_uring via syscalls, no CGO required for runtime
- **One goroutine per queue** - single issuer pattern for io_uring
- **Backend must be thread-safe** - called from worker goroutines
- **Zero-copy available** - `Config.ZeroCopy` with `FixedFileBackend`

## References

- [Linux ublk documentation](https://docs.kernel.org/block/ublk.html)
- [ublksrv - C reference](https://github.com/ublk-org/ublksrv)
- [io_uring guide](https://unixism.net/loti/)
