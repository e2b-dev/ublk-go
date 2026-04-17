package ublk

import (
	"bytes"
	"crypto/rand"
	"fmt"
	"io"
	"math/big"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"unsafe"

	"golang.org/x/sys/unix"
)

// ---------------------------------------------------------------------------
// ABI correctness: struct sizes must exactly match the kernel UAPI or every
// ioctl / mmap / io_uring command silently corrupts memory.
// ---------------------------------------------------------------------------

func TestKernelABI(t *testing.T) {
	if unsafe.Sizeof(ctrlCmd{}) != 32 {
		t.Fatalf("ctrlCmd is %d bytes, kernel expects 32", unsafe.Sizeof(ctrlCmd{}))
	}
	if unsafe.Sizeof(devInfo{}) != 64 {
		t.Fatalf("devInfo is %d bytes, kernel expects 64", unsafe.Sizeof(devInfo{}))
	}
	if unsafe.Sizeof(ioCmd{}) != 16 {
		t.Fatalf("ioCmd is %d bytes, kernel expects 16", unsafe.Sizeof(ioCmd{}))
	}
	if unsafe.Sizeof(ioDesc{}) != 24 {
		t.Fatalf("ioDesc is %d bytes, kernel expects 24", unsafe.Sizeof(ioDesc{}))
	}
	if unsafe.Sizeof(sqe128{}) != 128 {
		t.Fatalf("sqe128 is %d bytes, kernel expects 128", unsafe.Sizeof(sqe128{}))
	}
	if unsafe.Sizeof(sqe64{}) != 64 {
		t.Fatalf("sqe64 is %d bytes, kernel expects 64", unsafe.Sizeof(sqe64{}))
	}
	if unsafe.Sizeof(cqe{}) != 16 {
		t.Fatalf("cqe is %d bytes, kernel expects 16 (not 32!)", unsafe.Sizeof(cqe{}))
	}

	// sqe128.Cmd must be at byte 48 — that's where io_uring_sqe_cmd() reads.
	var s sqe128
	off := uintptr(unsafe.Pointer(&s.Cmd[0])) - uintptr(unsafe.Pointer(&s))
	if off != 48 {
		t.Fatalf("sqe128.Cmd at offset %d, kernel expects 48", off)
	}
}

// ---------------------------------------------------------------------------
// io_uring smoke: create a ring and do a real NOP round-trip through the
// kernel — verifies our mmap layout, SQE/CQE indexing, submit, and wait.
// ---------------------------------------------------------------------------

func TestIoUringNOPRoundTrip(t *testing.T) {
	r, err := newIORing(16)
	if err != nil {
		t.Fatalf("newIORing: %v", err)
	}
	defer r.close()

	for i := range 16 {
		sqe := r.getSQE64()
		if sqe == nil {
			t.Fatalf("getSQE64 nil at %d", i)
		}
		sqe.Opcode = 0 // IORING_OP_NOP
		sqe.UserData = uint64(i) + 1
	}

	n, err := r.submit()
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	if n != 16 {
		t.Fatalf("submitted %d, want 16", n)
	}

	seen := make(map[uint64]bool)
	for range 16 {
		c, err := r.waitCQE()
		if err != nil {
			t.Fatalf("waitCQE: %v", err)
		}
		if c.Res != 0 {
			t.Errorf("NOP returned %d", c.Res)
		}
		seen[c.UserData] = true
		r.seenCQE()
	}

	for i := uint64(1); i <= 16; i++ {
		if !seen[i] {
			t.Errorf("never got CQE for UserData=%d", i)
		}
	}
}

// ---------------------------------------------------------------------------
// io_uring sustained: verify SQE reuse works across many submit/wait cycles
// (exercises head/tail wrap-around in both SQ and CQ).
// ---------------------------------------------------------------------------

func TestIoUringManyCycles(t *testing.T) {
	r, err := newIORing(4)
	if err != nil {
		t.Fatalf("newIORing: %v", err)
	}
	defer r.close()

	for cycle := range 200 {
		for i := range int(r.sqEntries) {
			sqe := r.getSQE64()
			if sqe == nil {
				t.Fatalf("cycle %d: getSQE64 nil at %d", cycle, i)
			}
			sqe.Opcode = 0
			sqe.UserData = uint64(cycle*1000 + i)
		}
		if _, err := r.submit(); err != nil {
			t.Fatalf("cycle %d: submit: %v", cycle, err)
		}
		for range r.sqEntries {
			c, err := r.waitCQE()
			if err != nil {
				t.Fatalf("cycle %d: waitCQE: %v", cycle, err)
			}
			if c.Res != 0 {
				t.Fatalf("cycle %d: NOP res=%d", cycle, c.Res)
			}
			r.seenCQE()
		}
	}
}

// ===========================================================================
// Integration tests — require root and ublk_drv loaded.
// ===========================================================================

func canRunIntegration(t *testing.T) {
	t.Helper()
	if os.Getuid() != 0 {
		t.Skip("requires root (run: sudo go test -v -count=1 ./ublk/)")
	}
	if _, err := os.Stat("/dev/ublk-control"); err != nil {
		t.Skip("ublk_drv not loaded (run: sudo modprobe ublk_drv)")
	}
}


// memBackend is a concurrency-safe in-memory block device.
type memBackend struct {
	mu     sync.RWMutex
	data   []byte
	writes atomic.Int64
	reads  atomic.Int64
}

func newMemBackend(size int) *memBackend {
	return &memBackend{data: make([]byte, size)}
}

func (m *memBackend) ReadAt(p []byte, off int64) (int, error) {
	m.reads.Add(1)
	m.mu.RLock()
	defer m.mu.RUnlock()
	if off < 0 || int(off) >= len(m.data) {
		return 0, io.EOF
	}
	return copy(p, m.data[off:]), nil
}

func (m *memBackend) WriteAt(p []byte, off int64) (int, error) {
	m.writes.Add(1)
	m.mu.Lock()
	defer m.mu.Unlock()
	if off < 0 || int(off) >= len(m.data) {
		return 0, io.ErrShortWrite
	}
	return copy(m.data[off:], p), nil
}

// snapshot returns a copy of the backend data for comparison.
func (m *memBackend) snapshot() []byte {
	m.mu.RLock()
	defer m.mu.RUnlock()
	s := make([]byte, len(m.data))
	copy(s, m.data)
	return s
}

func makeDevice(t *testing.T, size uint64) (*Device, *memBackend) {
	t.Helper()
	canRunIntegration(t)
	backend := newMemBackend(int(size))
	dev, err := New(backend, size)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { dev.Close() })
	return dev, backend
}

// openBlkDev opens the block device path for direct IO, falling back to O_SYNC.
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

// ---------------------------------------------------------------------------
// Test: write data through the block device, then read the backend directly
// to prove the write path works end-to-end through the kernel.
// ---------------------------------------------------------------------------

func TestWritePathEndToEnd(t *testing.T) {
	const size = 4 * 1024 * 1024
	dev, backend := makeDevice(t, size)
	fd := openBlkDev(t, dev.BlockDevicePath(), unix.O_RDWR)

	pattern := make([]byte, 4096)
	for i := range pattern {
		pattern[i] = byte(i*7 + 13)
	}

	// Write via block device at several offsets.
	offsets := []int64{0, 4096, 2 * 4096, 1024 * 1024, size - 4096}
	for _, off := range offsets {
		n, err := unix.Pwrite(fd, pattern, off)
		if err != nil || n != len(pattern) {
			t.Fatalf("pwrite at %d: n=%d err=%v", off, n, err)
		}
	}

	// Read directly from the backend to verify kernel → io_uring → backend path.
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

// ---------------------------------------------------------------------------
// Test: write data directly into the backend, then read through the block
// device to prove the read path works end-to-end.
// ---------------------------------------------------------------------------

func TestReadPathEndToEnd(t *testing.T) {
	const size = 4 * 1024 * 1024
	dev, backend := makeDevice(t, size)
	fd := openBlkDev(t, dev.BlockDevicePath(), unix.O_RDONLY)

	// Plant known data directly in the backend.
	pattern := make([]byte, 4096)
	rand.Read(pattern)

	off := int64(512 * 1024) // 512 KB in
	backend.WriteAt(pattern, off)

	// Drop page cache so the kernel must fetch from the ublk device.
	flushFd, err := unix.Open(dev.BlockDevicePath(), unix.O_RDONLY, 0)
	if err == nil {
		unix.Syscall(unix.SYS_IOCTL, uintptr(flushFd), 0x1261 /* BLKFLSBUF */, 0)
		unix.Close(flushFd)
	}

	got := make([]byte, 4096)
	n, err := unix.Pread(fd, got, off)
	if err != nil || n != 4096 {
		t.Fatalf("pread: n=%d err=%v", n, err)
	}

	if !bytes.Equal(got, pattern) {
		t.Error("block device returned different data than what's in the backend")
	}

	if backend.reads.Load() == 0 {
		t.Error("backend.ReadAt was never called")
	}
}

// ---------------------------------------------------------------------------
// Test: data integrity over the full device — fill every byte, fsync, read
// back, compare against the backend snapshot.
// ---------------------------------------------------------------------------

func TestFullDeviceIntegrity(t *testing.T) {
	const size = 2 * 1024 * 1024
	dev, backend := makeDevice(t, size)
	fd := openBlkDev(t, dev.BlockDevicePath(), unix.O_RDWR)

	// Fill with block-number-based pattern.
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

	// Read back every block from the block device and compare to backend.
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

// ---------------------------------------------------------------------------
// Test: concurrent writers to non-overlapping regions, then verify all data.
// This exercises the io_uring event loop under real concurrent kernel IO.
// ---------------------------------------------------------------------------

func TestConcurrentWriters(t *testing.T) {
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
				// Unique per worker+block pattern.
				for j := range buf {
					buf[j] = byte(id ^ i ^ j)
				}
				off := base + int64(i)*blk
				if n, err := unix.Pwrite(fd, buf, off); err != nil || n != blk {
					errs <- fmt.Errorf("worker %d write at %d: n=%d err=%v", id, off, n, err)
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

	// Verify every block in the backend.
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

// ---------------------------------------------------------------------------
// Test: concurrent readers and writers — proves no crashes, no data races,
// and the io_uring loop stays alive under mixed load.
// ---------------------------------------------------------------------------

func TestConcurrentMixedIO(t *testing.T) {
	const size = 8 * 1024 * 1024
	dev, _ := makeDevice(t, size)
	path := dev.BlockDevicePath()

	const goroutines = 16
	const opsPerG = 100
	const blk = 4096

	var wg sync.WaitGroup
	var errCount atomic.Int64

	for g := range goroutines {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			fd, err := unix.Open(path, unix.O_RDWR|unix.O_SYNC, 0)
			if err != nil {
				errCount.Add(1)
				return
			}
			defer unix.Close(fd)

			maxBlocks := size / blk
			buf := make([]byte, blk)

			for i := range opsPerG {
				nBig, _ := rand.Int(rand.Reader, big.NewInt(int64(maxBlocks)))
				off := nBig.Int64() * blk

				if (id+i)%2 == 0 {
					rand.Read(buf)
					if _, err := unix.Pwrite(fd, buf, off); err != nil {
						errCount.Add(1)
					}
				} else {
					if _, err := unix.Pread(fd, buf, off); err != nil {
						errCount.Add(1)
					}
				}
			}
		}(g)
	}

	wg.Wait()
	if n := errCount.Load(); n > 0 {
		t.Errorf("%d IO errors under concurrent mixed load", n)
	}
}

// ---------------------------------------------------------------------------
// Test: multiple create/destroy cycles — no leaked fds, no kernel complaints.
// ---------------------------------------------------------------------------

func TestRepeatedCreateDestroy(t *testing.T) {
	canRunIntegration(t)

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

		// Do a small IO to prove it works.
		fd, err := unix.Open(path, unix.O_RDWR|unix.O_SYNC, 0)
		if err != nil {
			t.Fatalf("cycle %d open: %v", cycle, err)
		}
		buf := []byte("cycle")
		unix.Pwrite(fd, buf, 0)
		unix.Close(fd)

		if err := dev.Close(); err != nil {
			t.Fatalf("cycle %d Close: %v", cycle, err)
		}
	}
}

// ---------------------------------------------------------------------------
// Test: random IO with full data verification.
// ---------------------------------------------------------------------------

func TestRandomIOVerified(t *testing.T) {
	const size = 4 * 1024 * 1024
	dev, backend := makeDevice(t, size)
	fd := openBlkDev(t, dev.BlockDevicePath(), unix.O_RDWR)

	const blk = 4096
	const iterations = 200

	// Track what we wrote at each block offset.
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

	// Verify every written block matches the backend.
	for off, expected := range written {
		got := make([]byte, blk)
		backend.ReadAt(got, off)
		if !bytes.Equal(got, expected) {
			t.Fatalf("random IO: backend mismatch at offset %d (first diff at byte %d)",
				off, firstDiff(got, expected))
		}
	}
}

// ---------------------------------------------------------------------------
// Test: close is idempotent and the block device disappears.
// ---------------------------------------------------------------------------

func TestCloseIdempotent(t *testing.T) {
	canRunIntegration(t)

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

	// Block device should be gone.
	if _, err := os.Stat(path); err == nil {
		t.Errorf("block device %s still exists after Close", path)
	}
}

// ---------------------------------------------------------------------------
// Test: write at the very last block of the device.
// ---------------------------------------------------------------------------

func TestLastBlock(t *testing.T) {
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

// ---------------------------------------------------------------------------
// Test: write the same block twice, verify latest data wins.
// ---------------------------------------------------------------------------

func TestOverwrite(t *testing.T) {
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

// ---------------------------------------------------------------------------
// Test: write through block device, read back through block device (not just
// backend) to verify the full round-trip through the kernel.
// ---------------------------------------------------------------------------

func TestWriteThenReadViaBlockDev(t *testing.T) {
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

// ---------------------------------------------------------------------------
// Test: verify the device reports the correct size.
// ---------------------------------------------------------------------------

func TestDeviceSize(t *testing.T) {
	const size = 8 * 1024 * 1024
	dev, _ := makeDevice(t, size)

	fd, err := unix.Open(dev.BlockDevicePath(), unix.O_RDONLY, 0)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer unix.Close(fd)

	var blkSize uint64
	_, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), 0x80081272 /* BLKGETSIZE64 */, uintptr(unsafe.Pointer(&blkSize)))
	if errno != 0 {
		t.Fatalf("BLKGETSIZE64: %v", errno)
	}
	if blkSize != size {
		t.Errorf("device size = %d, want %d", blkSize, size)
	}
}

// ---------------------------------------------------------------------------

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
