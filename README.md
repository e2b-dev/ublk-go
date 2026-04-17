# ublk-go

[![CI](https://github.com/e2b-dev/ublk-go/actions/workflows/ci.yml/badge.svg)](https://github.com/e2b-dev/ublk-go/actions/workflows/ci.yml)
[![codecov](https://codecov.io/gh/e2b-dev/ublk-go/branch/main/graph/badge.svg)](https://codecov.io/gh/e2b-dev/ublk-go)
[![Go Reference](https://pkg.go.dev/badge/github.com/e2b-dev/ublk-go.svg)](https://pkg.go.dev/github.com/e2b-dev/ublk-go)

> Coverage profiles are also uploaded as a `coverage` build artifact on
> every CI run — open any workflow run → Artifacts → download to view
> `unit.html` / `integration.html` locally. The Codecov badge above
> will start working as soon as the repo is public (tokenless OIDC
> upload) or a `CODECOV_TOKEN` secret is added (private repo).

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

## Production setup (recommended for serious use)

By default `ublk_drv` only lets **64 devices** exist system-wide at a
time — and the counter is bumped on `ADD_DEV` regardless of process
privileges (the "unprivileged" in the module's own parameter
description is misleading; the check is global). If you plan to run
more than a handful of devices, or if you run tests that leak devices
after crashes, raise the limit at module load.

Also, udev's default policy watches every block device with inotify,
which is pure overhead for userspace block devices. NBD has long
recommended disabling it for the same reason.

Drop-in config files are tracked in [`contrib/`](contrib):

```bash
sudo install -m0644 contrib/ublk.conf              /etc/modprobe.d/
sudo install -m0644 contrib/97-ublk-device.rules   /etc/udev/rules.d/

sudo rmmod ublk_drv && sudo modprobe ublk_drv
sudo udevadm control --reload-rules && sudo udevadm trigger
```

Verify:

```bash
cat /sys/module/ublk_drv/parameters/ublks_max   # should print 4096
```

Theoretical hard ceiling is `UBLK_MINORS = 1 << MINORBITS ≈ 1 M`;
practical values are whatever your workload needs. Each idle device is
a small amount of kernel memory and a minor number — cheap.

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

    fmt.Println(dev.Path())

    sig := make(chan os.Signal, 1)
    signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
    <-sig
}
```

Run with `sudo go run ./example`. The block device appears at `/dev/ublkbN` and can be used like any other block device (`mkfs`, `mount`, `dd`, etc.).

For an end-to-end demo that formats, mounts, writes a file, and prints backend I/O counts at each phase, run `sudo go run ./example/fsdemo`.

## API

```go
// Backend is the storage backing the block device. It embeds io.ReaderAt
// and io.WriterAt; their standard concurrency contract (disjoint ranges
// are safe) applies here.
type Backend interface {
    io.ReaderAt
    io.WriterAt
}

func New(backend Backend, size uint64) (*Device, error)
func (*Device) Path() string
func (*Device) Close() error
```

`size` must be a positive multiple of 512. The block device uses
512-byte logical blocks, 128KB max IO, and a single queue with depth 128.

### Closing a device

**Close any fd you've opened to `/dev/ublkbN` before calling
`Device.Close()`.** `Close()` internally issues `UBLK_CMD_DEL_DEV`,
which is backed by the kernel's `del_gendisk()` — and that blocks
until every open fd on the block device has been released. This is
standard Linux block-device teardown, not a ublk quirk, but it means
the following deadlocks:

```go
fd, _ := unix.Open(dev.Path(), unix.O_RDWR, 0)
// ... some work ...
dev.Close()        // ← hangs forever; del_gendisk waits for `fd`
unix.Close(fd)     // never reached
```

Correct order:

```go
fd, _ := unix.Open(dev.Path(), unix.O_RDWR, 0)
// ... some work ...
unix.Close(fd)     // release the block device first
dev.Close()        // now Close can proceed
```

If you have many fds spread across goroutines, close them all before
calling `Device.Close()`. A running mount also counts; unmount before
closing the device.

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
