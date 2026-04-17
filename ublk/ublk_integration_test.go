//go:build integration

package ublk

import (
	"bytes"
	"crypto/rand"
	"fmt"
	"math/big"
	"os"
	"sync"
	"testing"
	"unsafe"

	"golang.org/x/sys/unix"
)

// TestMain hard-fails the integration suite if it cannot run. Build with
// `-tags=integration` and run as root on a host with ublk_drv loaded.
func TestMain(m *testing.M) {
	if os.Getuid() != 0 {
		fmt.Fprintln(os.Stderr, "integration tests must run as root")
		os.Exit(1)
	}
	if _, err := os.Stat("/dev/ublk-control"); err != nil {
		fmt.Fprintf(os.Stderr, "integration tests require ublk_drv: %v\n", err)
		os.Exit(1)
	}
	os.Exit(m.Run())
}

func makeDevice(t *testing.T, size uint64) (*Device, *memBackend) {
	t.Helper()
	backend := newMemBackend(int(size))
	dev, err := New(backend, size)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { dev.Close() })
	return dev, backend
}

func openBlkDev(t *testing.T, path string, flags int) int {
	t.Helper()
	fd, err := unix.Open(path, flags|unix.O_DIRECT, 0)
	if err != nil {
		fd, err = unix.Open(path, flags|unix.O_SYNC, 0)
		if err != nil {
			t.Fatalf("open %s: %v", path, err)
		}
	}
	t.Cleanup(func() { unix.Close(fd) })
	return fd
}

func TestWritePathEndToEnd(t *testing.T) {
	t.Parallel()
	const size = 4 * 1024 * 1024
	dev, backend := makeDevice(t, size)
	fd := openBlkDev(t, dev.BlockDevicePath(), unix.O_RDWR)

	pattern := make([]byte, 4096)
	for i := range pattern {
		pattern[i] = byte(i*7 + 13)
	}

	offsets := []int64{0, 4096, 2 * 4096, 1024 * 1024, size - 4096}
	for _, off := range offsets {
		n, err := unix.Pwrite(fd, pattern, off)
		if err != nil || n != len(pattern) {
			t.Fatalf("pwrite at %d: n=%d err=%v", off, n, err)
		}
	}

	for _, off := range offsets {
		got := make([]byte, 4096)
		backend.ReadAt(got, off)
		if !bytes.Equal(got, pattern) {
			t.Errorf("backend data mismatch at offset %d", off)
		}
	}

	if backend.writes.Load() == 0 {
		t.Error("backend.WriteAt was never called")
	}
}

func TestReadPathEndToEnd(t *testing.T) {
	t.Parallel()
	const size = 4 * 1024 * 1024
	dev, backend := makeDevice(t, size)
	fd := openBlkDev(t, dev.BlockDevicePath(), unix.O_RDONLY)

	pattern := make([]byte, 4096)
	rand.Read(pattern)
	off := int64(512 * 1024)
	backend.WriteAt(pattern, off)

	// Drop page cache.
	flushFd, err := unix.Open(dev.BlockDevicePath(), unix.O_RDONLY, 0)
	if err == nil {
		unix.Syscall(unix.SYS_IOCTL, uintptr(flushFd), 0x1261, 0)
		unix.Close(flushFd)
	}

	got := make([]byte, 4096)
	n, err := unix.Pread(fd, got, off)
	if err != nil || n != 4096 {
		t.Fatalf("pread: n=%d err=%v", n, err)
	}

	if !bytes.Equal(got, pattern) {
		t.Error("block device returned different data than backend")
	}

	if backend.reads.Load() == 0 {
		t.Error("backend.ReadAt was never called")
	}
}

func TestFullDeviceIntegrity(t *testing.T) {
	t.Parallel()
	const size = 2 * 1024 * 1024
	dev, backend := makeDevice(t, size)
	fd := openBlkDev(t, dev.BlockDevicePath(), unix.O_RDWR)

	const blk = 4096
	for off := int64(0); off < size; off += blk {
		buf := make([]byte, blk)
		for i := range buf {
			buf[i] = byte(off/blk) ^ byte(i)
		}
		if n, err := unix.Pwrite(fd, buf, off); err != nil || n != blk {
			t.Fatalf("pwrite at %d: n=%d err=%v", off, n, err)
		}
	}

	unix.Fsync(fd)

	snap := backend.snapshot()
	for off := int64(0); off < size; off += blk {
		got := make([]byte, blk)
		n, err := unix.Pread(fd, got, off)
		if err != nil || n != blk {
			t.Fatalf("pread at %d: n=%d err=%v", off, n, err)
		}
		expected := snap[off : off+blk]
		if !bytes.Equal(got, expected) {
			t.Fatalf("data mismatch at offset %d (first diff at byte %d)",
				off, firstDiff(got, expected))
		}
	}
}

func TestConcurrentWriters(t *testing.T) {
	t.Parallel()
	const size = 16 * 1024 * 1024
	dev, backend := makeDevice(t, size)
	path := dev.BlockDevicePath()

	const workers = 8
	const blocksPerWorker = 128
	const blk = 4096

	var wg sync.WaitGroup
	errs := make(chan error, workers)

	for w := range workers {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			fd, err := unix.Open(path, unix.O_WRONLY|unix.O_SYNC, 0)
			if err != nil {
				errs <- fmt.Errorf("worker %d open: %w", id, err)
				return
			}
			defer unix.Close(fd)

			base := int64(id) * blocksPerWorker * blk
			for i := range blocksPerWorker {
				buf := make([]byte, blk)
				for j := range buf {
					buf[j] = byte(id ^ i ^ j)
				}
				off := base + int64(i)*blk
				if n, err := unix.Pwrite(fd, buf, off); err != nil || n != blk {
					errs <- fmt.Errorf("worker %d write at %d: n=%d err=%w", id, off, n, err)
					return
				}
			}
		}(w)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}

	for w := range workers {
		base := int64(w) * blocksPerWorker * blk
		for i := range blocksPerWorker {
			off := base + int64(i)*blk
			got := make([]byte, blk)
			backend.ReadAt(got, off)
			for j, b := range got {
				want := byte(w ^ i ^ j)
				if b != want {
					t.Fatalf("worker %d block %d byte %d: got 0x%02x want 0x%02x",
						w, i, j, b, want)
				}
			}
		}
	}
}

func TestRepeatedCreateDestroy(t *testing.T) {
	t.Parallel()
	for cycle := range 5 {
		backend := newMemBackend(2 * 1024 * 1024)
		dev, err := New(backend, 2*1024*1024)
		if err != nil {
			t.Fatalf("cycle %d New: %v", cycle, err)
		}

		path := dev.BlockDevicePath()
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("cycle %d: block device missing: %v", cycle, err)
		}

		fd, err := unix.Open(path, unix.O_RDWR|unix.O_SYNC, 0)
		if err != nil {
			t.Fatalf("cycle %d open: %v", cycle, err)
		}
		unix.Pwrite(fd, []byte("cycle"), 0)
		unix.Close(fd)

		if err := dev.Close(); err != nil {
			t.Fatalf("cycle %d Close: %v", cycle, err)
		}
	}
}

func TestRandomIOVerified(t *testing.T) {
	t.Parallel()
	const size = 4 * 1024 * 1024
	dev, backend := makeDevice(t, size)
	fd := openBlkDev(t, dev.BlockDevicePath(), unix.O_RDWR)

	const blk = 4096
	const iterations = 200

	written := make(map[int64][]byte)
	maxBlocks := int64(size / blk)

	for range iterations {
		nBig, _ := rand.Int(rand.Reader, big.NewInt(maxBlocks))
		off := nBig.Int64() * blk

		buf := make([]byte, blk)
		rand.Read(buf)

		if n, err := unix.Pwrite(fd, buf, off); err != nil || n != blk {
			t.Fatalf("pwrite at %d: n=%d err=%v", off, n, err)
		}
		written[off] = buf
	}

	for off, expected := range written {
		got := make([]byte, blk)
		backend.ReadAt(got, off)
		if !bytes.Equal(got, expected) {
			t.Fatalf("random IO: backend mismatch at offset %d (first diff at byte %d)",
				off, firstDiff(got, expected))
		}
	}
}

func TestCloseIdempotent(t *testing.T) {
	t.Parallel()
	backend := newMemBackend(2 * 1024 * 1024)
	dev, err := New(backend, 2*1024*1024)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	path := dev.BlockDevicePath()

	for i := range 3 {
		if err := dev.Close(); err != nil {
			t.Fatalf("Close #%d: %v", i, err)
		}
	}

	if _, err := os.Stat(path); err == nil {
		t.Errorf("block device %s still exists after Close", path)
	}
}

func TestLastBlock(t *testing.T) {
	t.Parallel()
	const size = 2 * 1024 * 1024
	dev, backend := makeDevice(t, size)
	fd := openBlkDev(t, dev.BlockDevicePath(), unix.O_RDWR)

	const blk = 4096
	lastOff := int64(size - blk)

	wbuf := make([]byte, blk)
	rand.Read(wbuf)
	if n, err := unix.Pwrite(fd, wbuf, lastOff); err != nil || n != blk {
		t.Fatalf("pwrite last block: n=%d err=%v", n, err)
	}

	got := make([]byte, blk)
	backend.ReadAt(got, lastOff)
	if !bytes.Equal(got, wbuf) {
		t.Error("last block write mismatch")
	}
}

func TestOverwrite(t *testing.T) {
	t.Parallel()
	const size = 2 * 1024 * 1024
	dev, backend := makeDevice(t, size)
	fd := openBlkDev(t, dev.BlockDevicePath(), unix.O_RDWR)

	const blk = 4096
	buf1 := make([]byte, blk)
	buf2 := make([]byte, blk)
	for i := range buf1 {
		buf1[i] = 0xAA
		buf2[i] = 0x55
	}

	unix.Pwrite(fd, buf1, 0)
	unix.Pwrite(fd, buf2, 0)

	got := make([]byte, blk)
	backend.ReadAt(got, 0)
	if !bytes.Equal(got, buf2) {
		t.Error("overwrite: second write did not take effect")
	}
}

func TestWriteThenReadViaBlockDev(t *testing.T) {
	t.Parallel()
	const size = 2 * 1024 * 1024
	dev, _ := makeDevice(t, size)
	fd := openBlkDev(t, dev.BlockDevicePath(), unix.O_RDWR)

	const blk = 4096
	offsets := []int64{0, blk, 16 * blk, size/2 - blk, size - blk}

	for _, off := range offsets {
		wbuf := make([]byte, blk)
		rand.Read(wbuf)

		if n, err := unix.Pwrite(fd, wbuf, off); err != nil || n != blk {
			t.Fatalf("pwrite at %d: n=%d err=%v", off, n, err)
		}

		rbuf := make([]byte, blk)
		if n, err := unix.Pread(fd, rbuf, off); err != nil || n != blk {
			t.Fatalf("pread at %d: n=%d err=%v", off, n, err)
		}

		if !bytes.Equal(wbuf, rbuf) {
			t.Fatalf("round-trip mismatch at offset %d", off)
		}
	}
}

func TestDeviceSize(t *testing.T) {
	t.Parallel()
	const size = 8 * 1024 * 1024
	dev, _ := makeDevice(t, size)

	fd, err := unix.Open(dev.BlockDevicePath(), unix.O_RDONLY, 0)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer unix.Close(fd)

	var blkSize uint64
	_, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), 0x80081272, uintptr(unsafe.Pointer(&blkSize)))
	if errno != 0 {
		t.Fatalf("BLKGETSIZE64: %v", errno)
	}
	if blkSize != size {
		t.Errorf("device size = %d, want %d", blkSize, size)
	}
}

func TestSmallestDevice(t *testing.T) {
	t.Parallel()
	const size = 512
	dev, backend := makeDevice(t, size)
	fd := openBlkDev(t, dev.BlockDevicePath(), unix.O_RDWR)

	wbuf := make([]byte, 512)
	for i := range wbuf {
		wbuf[i] = 0xCD
	}
	if n, err := unix.Pwrite(fd, wbuf, 0); err != nil || n != 512 {
		t.Fatalf("pwrite: n=%d err=%v", n, err)
	}

	got := make([]byte, 512)
	backend.ReadAt(got, 0)
	if !bytes.Equal(got, wbuf) {
		t.Error("smallest device write mismatch")
	}
}

func TestReadUnwrittenRegion(t *testing.T) {
	t.Parallel()
	const size = 2 * 1024 * 1024
	dev, _ := makeDevice(t, size)
	fd := openBlkDev(t, dev.BlockDevicePath(), unix.O_RDONLY)

	buf := make([]byte, 4096)
	n, err := unix.Pread(fd, buf, 0)
	if err != nil || n != 4096 {
		t.Fatalf("pread: n=%d err=%v", n, err)
	}

	zeros := make([]byte, 4096)
	if !bytes.Equal(buf, zeros) {
		t.Error("unwritten region should be zeros")
	}
}

func TestBlockDevicePath(t *testing.T) {
	t.Parallel()
	backend := newMemBackend(1024 * 1024)
	dev, err := New(backend, 1024*1024)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer dev.Close()

	path := dev.BlockDevicePath()
	if path == "" {
		t.Fatal("BlockDevicePath is empty")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("block device %s does not exist: %v", path, err)
	}
}

func TestMultipleDevices(t *testing.T) {
	t.Parallel()
	const n = 3
	devs := make([]*Device, n)
	for i := range n {
		backend := newMemBackend(1024 * 1024)
		dev, err := New(backend, 1024*1024)
		if err != nil {
			for j := range i {
				devs[j].Close()
			}
			t.Fatalf("New device %d: %v", i, err)
		}
		devs[i] = dev
	}

	paths := make(map[string]bool)
	for _, d := range devs {
		p := d.BlockDevicePath()
		if paths[p] {
			t.Errorf("duplicate path: %s", p)
		}
		paths[p] = true
	}

	for i := len(devs) - 1; i >= 0; i-- {
		if err := devs[i].Close(); err != nil {
			t.Errorf("Close device %d: %v", i, err)
		}
	}
}

func firstDiff(a, b []byte) int {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	for i := range n {
		if a[i] != b[i] {
			return i
		}
	}
	if len(a) != len(b) {
		return n
	}
	return -1
}
