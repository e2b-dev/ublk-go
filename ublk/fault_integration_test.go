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
	data       []byte
}

func newFaultBackend() *faultBackend {
	return &faultBackend{data: make([]byte, testFaultDeviceSize)}
}

func (b *faultBackend) ReadAt(p []byte, off int64) (int, error) {
	b.reads.Add(1)
	if b.failReads.Load() {
		return 0, unix.EIO
	}
	return copy(p, b.data[off:off+int64(len(p))]), nil
}

func (b *faultBackend) WriteAt(p []byte, off int64) (int, error) {
	b.writes.Add(1)
	if b.failWrites.Load() {
		return 0, unix.EIO
	}
	return copy(b.data[off:off+int64(len(p))], p), nil
}

// panicOnceBackend panics on the next read or write call when the
// corresponding flag is set. The flag is atomically cleared on the
// triggering call, so subsequent calls proceed normally.
type panicOnceBackend struct {
	data       []byte
	panicRead  atomic.Bool
	panicWrite atomic.Bool
}

func newPanicOnceBackend() *panicOnceBackend {
	return &panicOnceBackend{data: make([]byte, testFaultDeviceSize)}
}

func (b *panicOnceBackend) ReadAt(p []byte, off int64) (int, error) {
	if b.panicRead.Swap(false) {
		panic("simulated backend read panic")
	}
	return copy(p, b.data[off:off+int64(len(p))]), nil
}

func (b *panicOnceBackend) WriteAt(p []byte, off int64) (int, error) {
	if b.panicWrite.Swap(false) {
		panic("simulated backend write panic")
	}
	return copy(b.data[off:off+int64(len(p))], p), nil
}

func TestBackendWriteErrorPropagates(t *testing.T) {
	t.Parallel()

	backend := newFaultBackend()
	backend.failWrites.Store(true)
	dev, err := New(backend, testFaultDeviceSize)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer dev.Close()

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
	defer dev.Close()

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
	if err := unix.Close(fd); err != nil {
		t.Fatalf("close fd: %v", err)
	}

	done := make(chan error, 1)
	go func() { done <- dev.Close() }()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Close: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Close hung after backend I/O failures")
	}
}

// TestTransientWriteErrorRecovery verifies that the device is still
// fully functional after a transient backend write failure. This is
// the ublk analogue of the NBD "backend write error → transient EIO
// (device recovers)" scenario.
func TestTransientWriteErrorRecovery(t *testing.T) {
	t.Parallel()

	backend := newFaultBackend()
	dev, err := New(backend, testFaultDeviceSize)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	fd, err := unix.Open(dev.Path(), unix.O_RDWR|unix.O_DIRECT, 0)
	if err != nil {
		_ = dev.Close()
		t.Fatalf("open: %v", err)
	}

	buf := alignedBuf(4096)
	for i := range buf {
		buf[i] = byte(i)
	}

	// Inject a write failure.
	backend.failWrites.Store(true)
	if _, err := unix.Pwrite(fd, buf, 0); !errors.Is(err, unix.EIO) {
		unix.Close(fd)
		_ = dev.Close()
		t.Fatalf("Pwrite with failWrites: got %v, want EIO", err)
	}

	// Restore the backend — the device must still accept writes.
	backend.failWrites.Store(false)
	if n, err := unix.Pwrite(fd, buf, 0); err != nil || n != len(buf) {
		unix.Close(fd)
		_ = dev.Close()
		t.Fatalf("Pwrite after write-error recovery: n=%d err=%v", n, err)
	}

	// Read back to confirm data arrived.
	rbuf := alignedBuf(4096)
	if n, err := unix.Pread(fd, rbuf, 0); err != nil || n != len(rbuf) {
		unix.Close(fd)
		_ = dev.Close()
		t.Fatalf("Pread after write-error recovery: n=%d err=%v", n, err)
	}
	if !bytes.Equal(rbuf, buf) {
		unix.Close(fd)
		_ = dev.Close()
		t.Fatal("data mismatch after write-error recovery")
	}

	unix.Close(fd)

	done := make(chan error, 1)
	go func() { done <- dev.Close() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Close after write-error recovery: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Close hung after write-error recovery")
	}
}

// TestTransientReadErrorRecovery verifies that the device is still
// fully functional after a transient backend read failure. This is
// the ublk analogue of the NBD "backend read error → transient EIO
// (device recovers)" scenario.
func TestTransientReadErrorRecovery(t *testing.T) {
	t.Parallel()

	backend := newFaultBackend()
	dev, err := New(backend, testFaultDeviceSize)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	fd, err := unix.Open(dev.Path(), unix.O_RDWR|unix.O_DIRECT, 0)
	if err != nil {
		_ = dev.Close()
		t.Fatalf("open: %v", err)
	}

	// Write a known pattern while reads are healthy.
	wbuf := alignedBuf(4096)
	for i := range wbuf {
		wbuf[i] = byte(i ^ 0xA5)
	}
	if n, err := unix.Pwrite(fd, wbuf, 0); err != nil || n != len(wbuf) {
		unix.Close(fd)
		_ = dev.Close()
		t.Fatalf("initial write: n=%d err=%v", n, err)
	}

	// Inject a read failure.
	backend.failReads.Store(true)
	rbuf := alignedBuf(4096)
	if _, err := unix.Pread(fd, rbuf, 0); !errors.Is(err, unix.EIO) {
		unix.Close(fd)
		_ = dev.Close()
		t.Fatalf("Pread with failReads: got %v, want EIO", err)
	}

	// Restore the backend — the device must still serve reads.
	backend.failReads.Store(false)
	if n, err := unix.Pread(fd, rbuf, 0); err != nil || n != len(rbuf) {
		unix.Close(fd)
		_ = dev.Close()
		t.Fatalf("Pread after read-error recovery: n=%d err=%v", n, err)
	}
	if !bytes.Equal(rbuf, wbuf) {
		unix.Close(fd)
		_ = dev.Close()
		t.Fatal("data mismatch after read-error recovery")
	}

	unix.Close(fd)

	done := make(chan error, 1)
	go func() { done <- dev.Close() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Close after read-error recovery: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Close hung after read-error recovery")
	}
}

// TestBackendPanicReturnsEIODeviceRecovers verifies that a panic inside
// backend.ReadAt or backend.WriteAt is caught by the worker, the I/O
// request fails with EIO, and the device remains fully functional
// (additional I/O succeeds and Close does not hang). This is the ublk
// analogue of the NBD K1 "dispatcher goroutine panic crashes
// orchestrator" bug.
func TestBackendPanicReturnsEIODeviceRecovers(t *testing.T) {
	t.Parallel()

	backend := newPanicOnceBackend()
	dev, err := New(backend, testFaultDeviceSize)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	fd, err := unix.Open(dev.Path(), unix.O_RDWR|unix.O_DIRECT, 0)
	if err != nil {
		_ = dev.Close()
		t.Fatalf("open: %v", err)
	}

	wbuf := alignedBuf(4096)
	for i := range wbuf {
		wbuf[i] = byte(i ^ 0x5A)
	}

	// A write panic must surface as EIO, not a process crash.
	backend.panicWrite.Store(true)
	if _, err := unix.Pwrite(fd, wbuf, 0); !errors.Is(err, unix.EIO) {
		unix.Close(fd)
		_ = dev.Close()
		t.Fatalf("Pwrite with panicking backend: got %v, want EIO", err)
	}

	// The device must still accept writes after the panic.
	if n, err := unix.Pwrite(fd, wbuf, 0); err != nil || n != len(wbuf) {
		unix.Close(fd)
		_ = dev.Close()
		t.Fatalf("Pwrite after write-panic recovery: n=%d err=%v", n, err)
	}

	// A read panic must also surface as EIO, not a crash.
	backend.panicRead.Store(true)
	rbuf := alignedBuf(4096)
	if _, err := unix.Pread(fd, rbuf, 0); !errors.Is(err, unix.EIO) {
		unix.Close(fd)
		_ = dev.Close()
		t.Fatalf("Pread with panicking backend: got %v, want EIO", err)
	}

	// Reads must work normally after the panic.
	if n, err := unix.Pread(fd, rbuf, 0); err != nil || n != len(rbuf) {
		unix.Close(fd)
		_ = dev.Close()
		t.Fatalf("Pread after read-panic recovery: n=%d err=%v", n, err)
	}
	if !bytes.Equal(rbuf, wbuf) {
		unix.Close(fd)
		_ = dev.Close()
		t.Fatal("data mismatch after panic recovery")
	}

	unix.Close(fd)

	done := make(chan error, 1)
	go func() { done <- dev.Close() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Close after panic recovery: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Close hung after backend panic recovery")
	}
}

