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
	r, err := newRing(16)
	if err != nil {
		t.Fatalf("newRing: %v", err)
	}
	defer r.close()

	// Submit 16 NOPs with distinct user data, verify all 16 come back.
	for i := range 16 {
		sqe := r.getSQE()
		if sqe == nil {
			t.Fatalf("getSQE nil at %d", i)
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
	r, err := newRing(4)
	if err != nil {
		t.Fatalf("newRing: %v", err)
	}
	defer r.close()

	for cycle := range 200 {
		for i := range int(r.sqEntries) {
			sqe := r.getSQE()
			if sqe == nil {
				t.Fatalf("cycle %d: getSQE nil at %d", cycle, i)
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
	mu      sync.RWMutex
	data    []byte
	flushes atomic.Int64
	writes  atomic.Int64
	reads   atomic.Int64
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

func (m *memBackend) Flush() error {
	m.flushes.Add(1)
	return nil
}

// snapshot returns a copy of the backend data for comparison.
func (m *memBackend) snapshot() []byte {
	m.mu.RLock()
	defer m.mu.RUnlock()
	s := make([]byte, len(m.data))
	copy(s, m.data)
	return s
}

// makeDevice creates a ublk device and registers cleanup.
func makeDevice(t *testing.T, size uint64, cfg Config) (*Device, *memBackend) {
	t.Helper()
	canRunIntegration(t)
	backend := newMemBackend(int(size))
	cfg.Size = size
	dev, err := New(backend, cfg)
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
	dev, backend := makeDevice(t, size, Config{})
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
	dev, backend := makeDevice(t, size, Config{})
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
	dev, backend := makeDevice(t, size, Config{})
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
	dev, backend := makeDevice(t, size, Config{})
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
	dev, _ := makeDevice(t, size, Config{})
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
// Test: fsync reaches the backend's Flush method.
// ---------------------------------------------------------------------------

func TestFsyncCallsBackendFlush(t *testing.T) {
	const size = 4 * 1024 * 1024
	dev, backend := makeDevice(t, size, Config{})

	fd := openBlkDev(t, dev.BlockDevicePath(), unix.O_RDWR)

	buf := make([]byte, 4096)
	unix.Pwrite(fd, buf, 0)

	before := backend.flushes.Load()

	if err := unix.Fsync(fd); err != nil {
		t.Fatalf("fsync: %v", err)
	}

	after := backend.flushes.Load()
	if after <= before {
		t.Errorf("fsync did not trigger Flush (before=%d after=%d)", before, after)
	}
}

// ---------------------------------------------------------------------------
// Test: multiple create/destroy cycles — no leaked fds, no kernel complaints.
// ---------------------------------------------------------------------------

func TestRepeatedCreateDestroy(t *testing.T) {
	canRunIntegration(t)

	for cycle := range 5 {
		backend := newMemBackend(2 * 1024 * 1024)
		dev, err := New(backend, Config{Size: 2 * 1024 * 1024})
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
// Test: 4K block size — verifies DevSectors = size/512 (not size/blockSize).
// ---------------------------------------------------------------------------

func TestBlockSize4096(t *testing.T) {
	const size = 4 * 1024 * 1024
	dev, backend := makeDevice(t, size, Config{BlockSize: 4096})

	fd := openBlkDev(t, dev.BlockDevicePath(), unix.O_RDWR)

	// Write 4K of random data at offset 0.
	wbuf := make([]byte, 4096)
	rand.Read(wbuf)
	if n, err := unix.Pwrite(fd, wbuf, 0); err != nil || n != 4096 {
		t.Fatalf("pwrite: n=%d err=%v", n, err)
	}

	// Write 4K at offset 8K.
	wbuf2 := make([]byte, 4096)
	rand.Read(wbuf2)
	if n, err := unix.Pwrite(fd, wbuf2, 8192); err != nil || n != 4096 {
		t.Fatalf("pwrite at 8K: n=%d err=%v", n, err)
	}

	// Verify both writes landed in the backend.
	got1 := make([]byte, 4096)
	backend.ReadAt(got1, 0)
	if !bytes.Equal(got1, wbuf) {
		t.Error("4K block: data mismatch at offset 0")
	}

	got2 := make([]byte, 4096)
	backend.ReadAt(got2, 8192)
	if !bytes.Equal(got2, wbuf2) {
		t.Error("4K block: data mismatch at offset 8K")
	}
}

// ---------------------------------------------------------------------------
// Test: multi-queue device works under parallel load.
// ---------------------------------------------------------------------------

func TestMultiQueue(t *testing.T) {
	const size = 8 * 1024 * 1024
	dev, backend := makeDevice(t, size, Config{Queues: 2, QueueDepth: 64})
	path := dev.BlockDevicePath()

	const goroutines = 4
	const writes = 64
	const blk = 4096

	var wg sync.WaitGroup
	for g := range goroutines {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			fd, _ := unix.Open(path, unix.O_WRONLY|unix.O_SYNC, 0)
			defer unix.Close(fd)
			for i := range writes {
				off := int64(id*writes+i) * blk
				buf := make([]byte, blk)
				buf[0] = byte(id)
				buf[1] = byte(i)
				unix.Pwrite(fd, buf, off)
			}
		}(g)
	}
	wg.Wait()

	// Spot-check: verify first two bytes of each block in the backend.
	for g := range goroutines {
		for i := range writes {
			off := int64(g*writes+i) * blk
			got := make([]byte, 2)
			backend.ReadAt(got, off)
			if got[0] != byte(g) || got[1] != byte(i) {
				t.Errorf("block at %d: got [%d,%d] want [%d,%d]",
					off, got[0], got[1], g, i)
			}
		}
	}
}

// ---------------------------------------------------------------------------
// Test: random IO with full data verification.
// ---------------------------------------------------------------------------

func TestRandomIOVerified(t *testing.T) {
	const size = 4 * 1024 * 1024
	dev, backend := makeDevice(t, size, Config{})
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
	dev, err := New(backend, Config{Size: 2 * 1024 * 1024})
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
