//go:build integration

package ublk

import (
	"bytes"
	"crypto/rand"
	"testing"
	"unsafe"

	"golang.org/x/sys/unix"
)

// TestNewWithOptions verifies that custom block size, queue depth and
// max IO size all propagate end-to-end to the kernel and the worker.
func TestNewWithOptions(t *testing.T) {
	t.Parallel()
	const size = 4 * 1024 * 1024
	const depth = 32
	const maxIO = 16 * 1024

	backend := newMemBackend(size)
	dev, err := New(backend, size,
		WithBlockSize(4096),
		WithQueueDepth(depth),
		WithMaxIOSize(maxIO),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { dev.Close() })

	if got := dev.workers[0].depth; got != depth {
		t.Errorf("worker.depth = %d, want %d", got, depth)
	}
	if got := dev.workers[0].bufSize; got != maxIO {
		t.Errorf("worker.bufSize = %d, want %d", got, maxIO)
	}

	fd := openBlkDev(t, dev.Path(), unix.O_RDWR)

	const blkBszGet = 0x80081270
	var bs uint64
	if _, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), blkBszGet, uintptr(unsafe.Pointer(&bs))); errno != 0 {
		t.Fatalf("BLKBSZGET: %v", errno)
	}
	if bs != 4096 {
		t.Errorf("BLKBSZGET = %d, want 4096", bs)
	}

	const blkSectGet = 0x1267
	var sectors uint64
	if _, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), blkSectGet, uintptr(unsafe.Pointer(&sectors))); errno != 0 {
		t.Fatalf("BLKSECTGET: %v", errno)
	}
	if sectors*512 > maxIO {
		t.Errorf("BLKSECTGET = %d bytes, exceeds WithMaxIOSize(%d)", sectors*512, maxIO)
	}

	want := make([]byte, 4096)
	rand.Read(want)
	if n, err := unix.Pwrite(fd, want, 8192); err != nil || n != 4096 {
		t.Fatalf("pwrite: n=%d err=%v", n, err)
	}
	got := make([]byte, 4096)
	backend.ReadAt(got, 8192)
	if !bytes.Equal(got, want) {
		t.Error("round-trip data mismatch")
	}
}
