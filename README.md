# ublk-go

[![CI](https://github.com/e2b-dev/ublk-go/actions/workflows/ci.yml/badge.svg)](https://github.com/e2b-dev/ublk-go/actions/workflows/ci.yml)
[![codecov](https://codecov.io/gh/e2b-dev/ublk-go/branch/main/graph/badge.svg)](https://app.codecov.io/gh/e2b-dev/ublk-go)
[![Go Reference](https://pkg.go.dev/badge/github.com/e2b-dev/ublk-go.svg)](https://pkg.go.dev/github.com/e2b-dev/ublk-go)
[![License](https://img.shields.io/badge/license-Apache%202.0-blue.svg)](LICENSE)

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

## Install

```bash
go get github.com/e2b-dev/ublk-go@latest
```

Pin to a tagged release once there is one:

```bash
go get github.com/e2b-dev/ublk-go@v0.1.0
```

In code, import the subpackage:

```go
import "github.com/e2b-dev/ublk-go/ublk"
```

The only symbols you need from day one are `Backend`, `New`, and
`(*Device).Path` / `(*Device).Close`. The lower-level io_uring wrapper
at `github.com/e2b-dev/ublk-go/ublk/uring` is intentionally separate —
most users don't touch it.

> [!IMPORTANT]
> **The kernel default is only 64 ublk devices system-wide.** Raise it
> before doing anything that creates more:
>
> ```bash
> echo 'options ublk_drv ublks_max=4096' | sudo tee /etc/modprobe.d/ublk.conf
> sudo rmmod ublk_drv && sudo modprobe ublk_drv
> cat /sys/module/ublk_drv/parameters/ublks_max   # verify: 4096
> ```
>
> Also: each `ublk.Device` holds 3 fds internally, so `ulimit -n 65536`
> (or systemd `LimitNOFILE=65536`) is recommended for any process
> creating many devices.
>
> See [Production setup](#production-setup-recommended-for-serious-use)
> below for udev tuning and the drop-in config files.

## Production setup (recommended for serious use)

Three limits to raise for any non-trivial deployment. With the defaults
you will hit one of them well before you get to a hundred devices:

| Limit | Default | Recommended | Where |
|---|---:|---:|---|
| `ublks_max` (module parameter) | 64 | **4096** | `/etc/modprobe.d/ublk.conf` |
| udev CHANGE-event inotify | on | off | `/etc/udev/rules.d/97-ublk-device.rules` |
| `RLIMIT_NOFILE` (fd limit) of the **process** using the library | 1024–4096 | **65536+** | shell / systemd unit / ulimit |

The last one matters because each `ublk.Device` holds **three fds**
internally (control fd, char fd, io_uring fd) — 500 devices = ~1500 fds
just from the library, plus one per open `/dev/ublkbN` you do from user
code. Crossing the default `ulimit -n` surfaces as `ublk.New` returning
`"too many open files"` or the io_uring setup failing partway through
`New`, which is very confusing if you don't know to look at it.

Drop-in config files are tracked in [`etc/`](etc):

```bash
# Raise the kernel-side limit (ublks_max=4096).
sudo install -m0644 etc/ublk.conf              /etc/modprobe.d/
sudo install -m0644 etc/97-ublk-device.rules   /etc/udev/rules.d/
sudo rmmod ublk_drv && sudo modprobe ublk_drv
sudo udevadm control --reload-rules && sudo udevadm trigger

# Raise the process-side fd limit. Shell session:
ulimit -n 65536
# or persistently for a systemd service, add to the unit:
#     [Service]
#     LimitNOFILE=65536
```

Verify:

```bash
cat /sys/module/ublk_drv/parameters/ublks_max   # 4096
ulimit -n                                       # 65536 (or higher)
```

Theoretical kernel ceiling is `UBLK_MINORS = 1 << MINORBITS ≈ 1 M`;
practical `ublks_max` is whatever your workload needs. Each idle
device costs a handful of KB of kernel memory and one minor number.

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

## License

[Apache License 2.0](LICENSE) © 2026 e2b-dev
