# ublk-go

[![CI](https://github.com/e2b-dev/ublk-go/actions/workflows/ci.yml/badge.svg)](https://github.com/e2b-dev/ublk-go/actions/workflows/ci.yml)
[![codecov](https://codecov.io/gh/e2b-dev/ublk-go/branch/main/graph/badge.svg)](https://codecov.io/gh/e2b-dev/ublk-go)
[![Go Reference](https://pkg.go.dev/badge/github.com/e2b-dev/ublk-go.svg)](https://pkg.go.dev/github.com/e2b-dev/ublk-go)

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

- Linux 6.0+ (tested on 6.17)
- `CAP_SYS_ADMIN` (root)
- `ublk_drv` kernel module loaded (`modprobe ublk_drv`)

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

func main() {
    const size = 256 * 1024 * 1024
    dev, err := ublk.New(&memBackend{data: make([]byte, size)}, size)
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

Run with `sudo go run ./example`. The block device appears at `/dev/ublkbN` and can be used like any other block device (`mkfs`, `mount`, `dd`, etc.).

For an end-to-end demo that formats, mounts, writes a file, and prints backend I/O counts at each phase, run `sudo go run ./example/fsdemo`.

## API

```go
type Backend interface {
    ReadAt(p []byte, off int64) (n int, err error)
    WriteAt(p []byte, off int64) (n int, err error)
}

func New(backend Backend, size uint64) (*Device, error)
func (*Device) BlockDevicePath() string
func (*Device) Close() error
```

`size` must be a positive multiple of 512. The block device uses 512-byte logical blocks, 128KB max IO, and a single queue with depth 128.

## Architecture

- **Control plane**: Commands to `/dev/ublk-control` via io_uring passthrough (`URING_CMD`) manage device lifecycle: ADD_DEV → SET_PARAMS → START_DEV → STOP_DEV → DEL_DEV.
- **Data plane**: One worker goroutine pinned to an OS thread (required by the ublk protocol), processing FETCH_REQ / COMMIT_AND_FETCH_REQ commands on its own io_uring on `/dev/ublkcN`. IO descriptors are memory-mapped from the char device.
- **No CGO**: All syscalls are made directly via `golang.org/x/sys/unix`.
- **Shutdown**: `Close()` triggers `ublk_ch_release` in the kernel, cancels the worker via eventfd+epoll, sends STOP+DEL, releases all fds.

## Packages

- `github.com/e2b-dev/ublk-go/ublk` — main library (`New`, `Device`, `Backend`)
- `github.com/e2b-dev/ublk-go/ublk/uring` — standalone io_uring wrapper used by the library

## Development

```bash
make test              # unit + integration (integration uses sudo)
make test-unit         # unit tests only (no root needed)
make test-integration  # integration tests only (requires root + ublk_drv)
make cover             # unit + integration with coverage profiles in ./coverage/
make cover-html        # open HTML coverage report in your browser
make lint              # gofmt check, go mod tidy check, golangci-lint, go mod verify
make fmt               # format code and tidy go.mod
make hooks             # install the repo's pre-commit hook (optional)
```

Integration tests live behind `//go:build integration`. If your editor / gopls
hides `ublk_integration_test.go`, tell the Go toolchain about the tag once:

```bash
go env -w GOFLAGS=-tags=integration
```

## Future work

See [TODO.md](TODO.md) for planned features (zero-copy, user recovery, zoned devices, etc.).

## References

- [Kernel ublk documentation](https://docs.kernel.org/block/ublk.html)
- [ublksrv reference implementation](https://github.com/ublk-org/ublksrv)
- [io_uring guide](https://unixism.net/loti/)
