# ublk-go

Pure Go library for Linux userspace block devices via the [ublk](https://docs.kernel.org/block/ublk.html) driver and io_uring.

```
 Application
     │
     ▼
 /dev/ublkbN        (block device)
     │
     ▼
 kernel ublk driver
     │  io_uring
     ▼
 ublk-go            (this library)
     │
     ▼
 Backend.ReadAt / Backend.WriteAt
```

## Requirements

- Linux 6.0+
- `CAP_SYS_ADMIN` (root)
- `modprobe ublk_drv`

## Quick start

```go
package main

import (
    "fmt"
    "os"
    "os/signal"
    "sync"
    "syscall"

    "github.com/e2b-dev/ublk-go/ublk"
)

type memBackend struct {
    mu   sync.RWMutex
    data []byte
}

func (m *memBackend) ReadAt(p []byte, off int64) (int, error) {
    m.mu.RLock()
    defer m.mu.RUnlock()
    return copy(p, m.data[off:off+int64(len(p))]), nil
}

func (m *memBackend) WriteAt(p []byte, off int64) (int, error) {
    m.mu.Lock()
    defer m.mu.Unlock()
    return copy(m.data[off:off+int64(len(p))], p), nil
}

func (m *memBackend) Flush() error { return nil }

func main() {
    const size = 256 * 1024 * 1024
    dev, err := ublk.New(&memBackend{data: make([]byte, size)}, ublk.Config{Size: size})
    if err != nil {
        fmt.Fprintln(os.Stderr, err)
        os.Exit(1)
    }
    defer dev.Close()

    fmt.Println(dev.BlockDevicePath())

    sig := make(chan os.Signal, 1)
    signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
    <-sig
}
```

## Architecture

- **Control plane**: Commands to `/dev/ublk-control` via io_uring passthrough (`URING_CMD`) manage device lifecycle: `ADD_DEV` → `SET_PARAMS` → `START_DEV` → `STOP_DEV` → `DEL_DEV`.
- **Data plane**: One goroutine per queue, each with its own io_uring, processing `FETCH_REQ` / `COMMIT_AND_FETCH_REQ` commands on `/dev/ublkcN`. IO descriptors are memory-mapped from the char device.
- **No CGO**: All syscalls are made directly via Go's `syscall` package and `golang.org/x/sys/unix`.

## Backend interface

```go
type Backend interface {
    ReadAt(p []byte, off int64) (n int, err error)
    WriteAt(p []byte, off int64) (n int, err error)
}
```

Optionally implement `Flusher`, `Discarder`, or `WriteZeroer` for additional block operations.

## Config

| Field      | Default | Description                          |
|------------|---------|--------------------------------------|
| Size       | —       | Device size in bytes (required)      |
| BlockSize  | 512     | Logical block size (power of 2)      |
| Queues     | 1       | Number of IO queues                  |
| QueueDepth | 128     | Per-queue IO depth (power of 2)      |

## References

- [Kernel ublk documentation](https://docs.kernel.org/block/ublk.html)
- [ublksrv reference implementation](https://github.com/ublk-org/ublksrv)
- [io_uring guide](https://unixism.net/loti/)
