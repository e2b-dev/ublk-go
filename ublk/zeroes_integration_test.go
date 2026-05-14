//go:build integration

package ublk

import (
	"bytes"
	"sync/atomic"
	"testing"
	"unsafe"

	"golang.org/x/sys/unix"
)

const testZeroesDeviceSize = 4 * 1024 * 1024

type zeroBackend struct {
	memBackend
	zeroes atomic.Int64
}

func newZeroBackend(size int) *zeroBackend {
	z := &zeroBackend{}
	z.data = make([]byte, size)
	return z
}

func (z *zeroBackend) WriteZeroesAt(off, length int64) (int, error) {
	z.zeroes.Add(1)
	z.mu.Lock()
	defer z.mu.Unlock()
	if off < 0 || off+length > int64(len(z.data)) {
		return 0, unix.EIO
	}
	clear(z.data[off : off+length])
	return int(length), nil
}

// blkRange invokes a BLK* ioctl that takes a uint64[2] {offset, length}.
func blkRange(t *testing.T, fd int, op uintptr, off, length uint64) {
	t.Helper()
	rng := [2]uint64{off, length}
	if _, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), op, uintptr(unsafe.Pointer(&rng[0]))); errno != 0 {
		t.Fatalf("ioctl 0x%x: %v", op, errno)
	}
}

func TestBackendWithoutZeroerRejectsDiscard(t *testing.T) {
	t.Parallel()
	dev, _ := makeDevice(t, testZeroesDeviceSize)
	fd, err := unix.Open(dev.Path(), unix.O_RDWR, 0)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer unix.Close(fd)

	rng := [2]uint64{0, 4096}
	_, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), unix.BLKDISCARD, uintptr(unsafe.Pointer(&rng[0])))
	if errno == 0 {
		t.Fatalf("BLKDISCARD unexpectedly succeeded on a backend without ZeroWriter")
	}
}

func TestBlkDiscardRoundTrip(t *testing.T) {
	t.Parallel()
	backend := newZeroBackend(testZeroesDeviceSize)
	dev, err := New(backend, Config{Size: testZeroesDeviceSize})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { dev.Close() })

	fd := openBlkDev(t, dev.Path(), unix.O_RDWR)

	pattern := bytes.Repeat([]byte{0xAB}, 4096)
	for off := int64(0); off < testZeroesDeviceSize; off += 4096 {
		if n, err := unix.Pwrite(fd, pattern, off); err != nil || n != 4096 {
			t.Fatalf("seed pwrite at %d: n=%d err=%v", off, n, err)
		}
	}

	const discardOff = 8 * 4096
	const discardLen = 16 * 4096
	blkRange(t, fd, unix.BLKDISCARD, discardOff, discardLen)

	if backend.zeroes.Load() == 0 {
		t.Fatal("backend.WriteZeroesAt was never called for BLKDISCARD")
	}

	got := make([]byte, discardLen)
	backend.ReadAt(got, discardOff)
	if !bytes.Equal(got, make([]byte, discardLen)) {
		t.Fatalf("backend region [%d,%d) not zeroed after BLKDISCARD", discardOff, discardOff+discardLen)
	}

	// Surrounding bytes must be preserved.
	pre := make([]byte, 4096)
	backend.ReadAt(pre, discardOff-4096)
	post := make([]byte, 4096)
	backend.ReadAt(post, discardOff+discardLen)
	if !bytes.Equal(pre, pattern) || !bytes.Equal(post, pattern) {
		t.Fatal("BLKDISCARD touched neighbouring blocks")
	}
}

func TestBlkZeroOutRoundTrip(t *testing.T) {
	t.Parallel()
	backend := newZeroBackend(testZeroesDeviceSize)
	dev, err := New(backend, Config{Size: testZeroesDeviceSize})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { dev.Close() })

	fd := openBlkDev(t, dev.Path(), unix.O_RDWR)

	pattern := bytes.Repeat([]byte{0xCD}, 4096)
	for off := int64(0); off < testZeroesDeviceSize; off += 4096 {
		if n, err := unix.Pwrite(fd, pattern, off); err != nil || n != 4096 {
			t.Fatalf("seed pwrite at %d: n=%d err=%v", off, n, err)
		}
	}

	const zeroOff = 4 * 4096
	const zeroLen = 32 * 4096
	blkRange(t, fd, unix.BLKZEROOUT, zeroOff, zeroLen)

	if backend.zeroes.Load() == 0 {
		t.Fatal("backend.WriteZeroesAt was never called for BLKZEROOUT")
	}

	got := make([]byte, zeroLen)
	backend.ReadAt(got, zeroOff)
	if !bytes.Equal(got, make([]byte, zeroLen)) {
		t.Fatalf("backend region [%d,%d) not zeroed after BLKZEROOUT", zeroOff, zeroOff+zeroLen)
	}
}

func TestBlkDiscardLargerThanMaxBuf(t *testing.T) {
	t.Parallel()

	// 1 MiB discard is 8× larger than the data-plane buffer (128 KiB);
	// the worker must dispatch it without splitting through w.bufs.
	backend := newZeroBackend(testZeroesDeviceSize)
	dev, err := New(backend, Config{Size: testZeroesDeviceSize})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { dev.Close() })

	fd := openBlkDev(t, dev.Path(), unix.O_RDWR)
	blkRange(t, fd, unix.BLKDISCARD, 0, 1<<20)

	if backend.zeroes.Load() == 0 {
		t.Fatal("backend.WriteZeroesAt was never called for large BLKDISCARD")
	}
}
