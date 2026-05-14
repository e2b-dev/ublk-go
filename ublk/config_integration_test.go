//go:build integration

package ublk

import (
	"bytes"
	"crypto/rand"
	"testing"
	"unsafe"

	"golang.org/x/sys/unix"
)

// TestConfig4KBlockSize asserts the kernel honours BlockSize=4096:
// BLKBSZGET on the device returns 4096, IOs are sized correctly, and
// the round-trip data path still works.
func TestConfig4KBlockSize(t *testing.T) {
	t.Parallel()
	const size = 4 * 1024 * 1024

	backend := newMemBackend(size)
	dev, err := New(backend, Config{Size: size, BlockSize: 4096})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { dev.Close() })

	fd := openBlkDev(t, dev.Path(), unix.O_RDWR)

	const blkBszGet = 0x80081270 // _IOR(0x12, 112, size_t)
	var bs uint64
	if _, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), blkBszGet, uintptr(unsafe.Pointer(&bs))); errno != 0 {
		t.Fatalf("BLKBSZGET: %v", errno)
	}
	if bs != 4096 {
		t.Fatalf("BLKBSZGET = %d, want 4096", bs)
	}

	want := make([]byte, 4096)
	rand.Read(want)
	if n, err := unix.Pwrite(fd, want, 8192); err != nil || n != 4096 {
		t.Fatalf("pwrite: n=%d err=%v", n, err)
	}
	got := make([]byte, 4096)
	backend.ReadAt(got, 8192)
	if !bytes.Equal(got, want) {
		t.Fatal("round-trip mismatch with 4k blocks")
	}
}

func TestConfigCustomQueueDepth(t *testing.T) {
	t.Parallel()
	const size = 1 * 1024 * 1024
	const depth = 32

	backend := newMemBackend(size)
	dev, err := New(backend, Config{Size: size, QueueDepth: depth})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { dev.Close() })

	if got := dev.workers[0].depth; got != depth {
		t.Fatalf("worker depth = %d, want %d", got, depth)
	}

	fd := openBlkDev(t, dev.Path(), unix.O_RDWR)
	buf := make([]byte, 4096)
	if _, err := unix.Pwrite(fd, buf, 0); err != nil {
		t.Fatalf("pwrite: %v", err)
	}
}

func TestConfigCustomMaxIO(t *testing.T) {
	t.Parallel()
	const size = 2 * 1024 * 1024
	const maxIO = 16 * 1024

	backend := newMemBackend(size)
	dev, err := New(backend, Config{Size: size, MaxIOSize: maxIO})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { dev.Close() })

	if got := dev.workers[0].bufSize; got != maxIO {
		t.Fatalf("worker bufSize = %d, want %d", got, maxIO)
	}

	// BLKSECTGET returns max_sectors in 512-byte units; the kernel
	// clamps the request queue limit at our cfg.MaxIOSize.
	fd := openBlkDev(t, dev.Path(), unix.O_RDWR)
	const blkSectGet = 0x1267
	var sectors uint64
	if _, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), blkSectGet, uintptr(unsafe.Pointer(&sectors))); errno != 0 {
		t.Fatalf("BLKSECTGET: %v", errno)
	}
	if sectors*512 > maxIO {
		t.Fatalf("BLKSECTGET reports %d bytes, exceeds MaxIOSize=%d", sectors*512, maxIO)
	}
}
