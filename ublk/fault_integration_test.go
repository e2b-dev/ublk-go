//go:build integration

package ublk

import (
	"bytes"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

const testFaultDeviceSize = 4 * 1024 * 1024

type faultBackend struct {
	reads      atomic.Int64
	writes     atomic.Int64
	failReads  atomic.Bool
	failWrites atomic.Bool
	panicRead  atomic.Bool
	panicWrite atomic.Bool
	data       []byte
}

func newFaultBackend() *faultBackend {
	return &faultBackend{data: make([]byte, testFaultDeviceSize)}
}

func (b *faultBackend) ReadAt(p []byte, off int64) (int, error) {
	b.reads.Add(1)
	if b.panicRead.Swap(false) {
		panic("backend read panic")
	}
	if b.failReads.Load() {
		return 0, unix.EIO
	}
	return copy(p, b.data[off:off+int64(len(p))]), nil
}

func (b *faultBackend) WriteAt(p []byte, off int64) (int, error) {
	b.writes.Add(1)
	if b.panicWrite.Swap(false) {
		panic("backend write panic")
	}
	if b.failWrites.Load() {
		return 0, unix.EIO
	}
	return copy(b.data[off:off+int64(len(p))], p), nil
}

// closeWithTimeout calls dev.Close and fails the test if it takes more than 5 s.
func closeWithTimeout(t *testing.T, dev *Device) {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- dev.Close() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Close: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Close hung")
	}
}

func TestBackendWriteErrorPropagates(t *testing.T) {
	t.Parallel()
	backend := newFaultBackend()
	backend.failWrites.Store(true)
	dev, err := New(backend, testFaultDeviceSize)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer closeWithTimeout(t, dev)
	fd, err := unix.Open(dev.Path(), unix.O_RDWR|unix.O_DIRECT, 0)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer unix.Close(fd)
	buf := alignedBuf(4096)
	if _, err := unix.Pwrite(fd, buf, 0); !errors.Is(err, unix.EIO) {
		t.Fatalf("Pwrite error = %v, want EIO", err)
	}
	if backend.writes.Load() == 0 {
		t.Fatal("backend WriteAt was not called")
	}
}

func TestBackendReadErrorPropagates(t *testing.T) {
	t.Parallel()
	backend := newFaultBackend()
	backend.failReads.Store(true)
	dev, err := New(backend, testFaultDeviceSize)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer closeWithTimeout(t, dev)
	fd, err := unix.Open(dev.Path(), unix.O_RDONLY|unix.O_DIRECT, 0)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer unix.Close(fd)
	buf := alignedBuf(4096)
	if _, err := unix.Pread(fd, buf, 0); !errors.Is(err, unix.EIO) {
		t.Fatalf("Pread error = %v, want EIO", err)
	}
	if backend.reads.Load() == 0 {
		t.Fatal("backend ReadAt was not called")
	}
}

func TestCloseAfterBackendErrors(t *testing.T) {
	t.Parallel()
	backend := newFaultBackend()
	backend.failWrites.Store(true)
	dev, err := New(backend, testFaultDeviceSize)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	fd, err := unix.Open(dev.Path(), unix.O_WRONLY|unix.O_DIRECT, 0)
	if err != nil {
		_ = dev.Close()
		t.Fatalf("open: %v", err)
	}
	buf := alignedBuf(4096)
	for range 4 {
		_, _ = unix.Pwrite(fd, buf, 0)
	}
	unix.Close(fd)
	closeWithTimeout(t, dev)
}

// TestTransientErrorRecovery verifies the device stays functional after a
// transient backend error: the failing I/O returns EIO, and subsequent I/O
// succeeds once the backend is healthy again.
func TestTransientErrorRecovery(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		inject func(*faultBackend)
		clear  func(*faultBackend)
		failOp func(int, []byte) error
	}{
		{
			"write",
			func(b *faultBackend) { b.failWrites.Store(true) },
			func(b *faultBackend) { b.failWrites.Store(false) },
			func(fd int, buf []byte) error { _, e := unix.Pwrite(fd, buf, 0); return e },
		},
		{
			"read",
			func(b *faultBackend) { b.failReads.Store(true) },
			func(b *faultBackend) { b.failReads.Store(false) },
			func(fd int, buf []byte) error { _, e := unix.Pread(fd, buf, 0); return e },
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			backend := newFaultBackend()
			dev, err := New(backend, testFaultDeviceSize)
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			defer closeWithTimeout(t, dev)
			fd, err := unix.Open(dev.Path(), unix.O_RDWR|unix.O_DIRECT, 0)
			if err != nil {
				t.Fatalf("open: %v", err)
			}
			defer unix.Close(fd)

			buf := alignedBuf(4096)
			for i := range buf {
				buf[i] = byte(i ^ 0xA5)
			}
			if n, err := unix.Pwrite(fd, buf, 0); err != nil || n != len(buf) {
				t.Fatalf("initial write: n=%d err=%v", n, err)
			}

			tc.inject(backend)
			if err := tc.failOp(fd, alignedBuf(4096)); !errors.Is(err, unix.EIO) {
				t.Fatalf("with error injected: got %v, want EIO", err)
			}
			tc.clear(backend)

			if n, err := unix.Pwrite(fd, buf, 0); err != nil || n != len(buf) {
				t.Fatalf("write after recovery: n=%d err=%v", n, err)
			}
			rbuf := alignedBuf(4096)
			if n, err := unix.Pread(fd, rbuf, 0); err != nil || n != len(rbuf) {
				t.Fatalf("read after recovery: n=%d err=%v", n, err)
			}
			if !bytes.Equal(rbuf, buf) {
				t.Fatal("data mismatch after recovery")
			}
		})
	}
}

// TestBackendPanicReturnsEIODeviceRecovers verifies that a backend panic is
// caught by the worker (returns EIO to the caller) and the device remains
// fully functional afterwards.
func TestBackendPanicReturnsEIODeviceRecovers(t *testing.T) {
	t.Parallel()
	backend := newFaultBackend()
	dev, err := New(backend, testFaultDeviceSize)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer closeWithTimeout(t, dev)
	fd, err := unix.Open(dev.Path(), unix.O_RDWR|unix.O_DIRECT, 0)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer unix.Close(fd)

	buf := alignedBuf(4096)
	for i := range buf {
		buf[i] = byte(i ^ 0x5A)
	}

	backend.panicWrite.Store(true)
	if _, err := unix.Pwrite(fd, buf, 0); !errors.Is(err, unix.EIO) {
		t.Fatalf("Pwrite with panicking backend: got %v, want EIO", err)
	}
	if n, err := unix.Pwrite(fd, buf, 0); err != nil || n != len(buf) {
		t.Fatalf("Pwrite after write-panic recovery: n=%d err=%v", n, err)
	}

	backend.panicRead.Store(true)
	rbuf := alignedBuf(4096)
	if _, err := unix.Pread(fd, rbuf, 0); !errors.Is(err, unix.EIO) {
		t.Fatalf("Pread with panicking backend: got %v, want EIO", err)
	}
	if n, err := unix.Pread(fd, rbuf, 0); err != nil || n != len(rbuf) {
		t.Fatalf("Pread after read-panic recovery: n=%d err=%v", n, err)
	}
	if !bytes.Equal(rbuf, buf) {
		t.Fatal("data mismatch after panic recovery")
	}
}

