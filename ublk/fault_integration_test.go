//go:build integration

package ublk

import (
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
	failReads  bool
	failWrites bool
	data       []byte
}

func newFaultBackend() *faultBackend {
	return &faultBackend{data: make([]byte, testFaultDeviceSize)}
}

func (b *faultBackend) ReadAt(p []byte, off int64) (int, error) {
	b.reads.Add(1)
	if b.failReads {
		return 0, unix.EIO
	}
	return copy(p, b.data[off:off+int64(len(p))]), nil
}

func (b *faultBackend) WriteAt(p []byte, off int64) (int, error) {
	b.writes.Add(1)
	if b.failWrites {
		return 0, unix.EIO
	}
	return copy(b.data[off:off+int64(len(p))], p), nil
}

func TestBackendWriteErrorPropagates(t *testing.T) {
	t.Parallel()

	backend := newFaultBackend()
	backend.failWrites = true
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
	backend.failReads = true
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
	backend.failWrites = true
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
