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
	discards atomic.Int64
	zeroes   atomic.Int64
}

func newZeroBackend() *zeroBackend {
	z := &zeroBackend{}
	z.data = make([]byte, testZeroesDeviceSize)
	return z
}

func (z *zeroBackend) DiscardAt(off, length int64) (int, error) {
	z.discards.Add(1)
	return z.zero(off, length)
}

func (z *zeroBackend) WriteZeroesAt(off, length int64, _ ZeroFlags) (int, error) {
	z.zeroes.Add(1)
	return z.zero(off, length)
}

func (z *zeroBackend) zero(off, length int64) (int, error) {
	z.mu.Lock()
	defer z.mu.Unlock()
	if off < 0 || off+length > int64(len(z.data)) {
		return 0, unix.EIO
	}
	clear(z.data[off : off+length])
	return int(length), nil
}

func blkRange(t *testing.T, fd int, op uintptr, off, length uint64) error {
	t.Helper()
	rng := [2]uint64{off, length}
	_, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), op, uintptr(unsafe.Pointer(&rng[0])))
	if errno == 0 {
		return nil
	}
	return errno
}

// BLKDISCARD must return EOPNOTSUPP when Discarder isn't implemented.
// BLKZEROOUT is omitted: when WRITE_ZEROES isn't advertised the kernel
// silently falls back to regular WRITE ops, so it succeeds either way.
func TestBackendWithoutDiscarderRejectsDiscard(t *testing.T) {
	t.Parallel()
	dev, _ := makeDevice(t, testZeroesDeviceSize)
	fd := openBlkDev(t, dev.Path(), unix.O_RDWR)
	if err := blkRange(t, fd, unix.BLKDISCARD, 0, 4096); err == nil {
		t.Fatal("BLKDISCARD unexpectedly succeeded without Discarder support")
	}
}

func TestBlkDiscardAndZeroOut(t *testing.T) {
	t.Parallel()
	backend := newZeroBackend()
	dev, err := New(backend, testZeroesDeviceSize)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { dev.Close() })

	fd := openBlkDev(t, dev.Path(), unix.O_RDWR)

	seed := bytes.Repeat([]byte{0xAB}, testZeroesDeviceSize)
	if n, err := unix.Pwrite(fd, seed, 0); err != nil || n != len(seed) {
		t.Fatalf("seed pwrite: n=%d err=%v", n, err)
	}

	// 1 MiB discard is 8× the data-plane buffer (128 KiB) — proves the
	// no-buffer path isn't capped by bufSize.
	if err := blkRange(t, fd, unix.BLKDISCARD, 0, 1<<20); err != nil {
		t.Fatalf("BLKDISCARD: %v", err)
	}
	if backend.discards.Load() == 0 {
		t.Fatal("DiscardAt was not called")
	}

	if err := blkRange(t, fd, unix.BLKZEROOUT, 2<<20, 1<<20); err != nil {
		t.Fatalf("BLKZEROOUT: %v", err)
	}
	if backend.zeroes.Load() == 0 {
		t.Fatal("WriteZeroesAt was not called")
	}

	check := func(name string, off, length int64) {
		got := make([]byte, length)
		backend.ReadAt(got, off)
		if !bytes.Equal(got, make([]byte, length)) {
			t.Fatalf("%s: range [%d,%d) not zeroed", name, off, off+length)
		}
	}
	check("BLKDISCARD", 0, 1<<20)
	check("BLKZEROOUT", 2<<20, 1<<20)

	// Untouched region must still hold the seed pattern.
	mid := make([]byte, 4096)
	backend.ReadAt(mid, 1<<20)
	if !bytes.Equal(mid, seed[:4096]) {
		t.Fatal("unrelated region was modified")
	}
}
