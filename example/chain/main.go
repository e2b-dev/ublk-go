// Chained-ublk integrity probe.
//
// Creates two ublk devices in the same process and stacks them:
//
//	user program
//	    │
//	    ▼  (O_DIRECT write to /dev/ublkbPROXY)
//	proxy ublk  (kernel block layer)
//	    │  io_uring, worker goroutine, Pwrite
//	    ▼  (O_DIRECT write to /dev/ublkbSTORAGE)
//	storage ublk  (kernel block layer)
//	    │  io_uring, worker goroutine, memcpy
//	    ▼
//	storageBackend.data  (plain Go []byte)
//
// The chain validates data integrity and offset preservation through
// two complete ublk stacks — two io_urings, two LockOSThread'd worker
// goroutines in the same process, and two block devices wired together
// via the kernel's own block layer.
//
// Expected result: bytes written to the proxy's block device appear,
// unchanged and at the same offset, in the storage's in-memory backend.
//
// Requires root and ublk_drv. Run with:
//
//	sudo go run ./example/chain
package main

import (
	"bytes"
	"crypto/rand"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"

	"github.com/e2b-dev/ublk-go/ublk"
)

const (
	chainSize = 16 * 1024 * 1024 // 16 MiB
	blkSize   = 4096
)

// memBackend is an in-memory Backend used as the bottom-of-stack.
type memBackend struct {
	mu         sync.RWMutex
	data       []byte
	reads      atomic.Int64
	writes     atomic.Int64
	readBytes  atomic.Int64
	writeBytes atomic.Int64
}

func (b *memBackend) ReadAt(p []byte, off int64) (int, error) {
	b.reads.Add(1)
	b.readBytes.Add(int64(len(p)))
	b.mu.RLock()
	defer b.mu.RUnlock()
	return copy(p, b.data[off:off+int64(len(p))]), nil
}

func (b *memBackend) WriteAt(p []byte, off int64) (int, error) {
	b.writes.Add(1)
	b.writeBytes.Add(int64(len(p)))
	b.mu.Lock()
	defer b.mu.Unlock()
	return copy(b.data[off:off+int64(len(p))], p), nil
}

func (b *memBackend) slice(off int64, n int) []byte {
	b.mu.RLock()
	defer b.mu.RUnlock()
	out := make([]byte, n)
	copy(out, b.data[off:off+int64(n)])
	return out
}

// fdBackend is a Backend that forwards ReadAt/WriteAt to a file
// descriptor — typically another block device (e.g. the bottom ublk's
// /dev/ublkbN). The buffers the worker hands us are already aligned,
// so O_DIRECT is fine.
type fdBackend struct {
	fd     int
	reads  atomic.Int64
	writes atomic.Int64
}

func (f *fdBackend) ReadAt(p []byte, off int64) (int, error) {
	f.reads.Add(1)
	n, err := unix.Pread(f.fd, p, off)
	if err != nil {
		return n, fmt.Errorf("fdBackend pread off=%d len=%d: %w", off, len(p), err)
	}
	return n, nil
}

func (f *fdBackend) WriteAt(p []byte, off int64) (int, error) {
	f.writes.Add(1)
	n, err := unix.Pwrite(f.fd, p, off)
	if err != nil {
		return n, fmt.Errorf("fdBackend pwrite off=%d len=%d: %w", off, len(p), err)
	}
	return n, nil
}

func main() {
	timeout := flag.Duration("step-timeout", 30*time.Second, "per-step timeout")
	flag.Parse()

	log.SetFlags(log.Ltime | log.Lmicroseconds)

	if os.Getuid() != 0 {
		log.Fatal("chain probe must be run as root")
	}

	if err := runChain(*timeout); err != nil {
		log.Fatalf("FAIL: %v", err)
	}
	log.Printf("PASS: chained ublk integrity verified end-to-end")
}

func runChain(stepTimeout time.Duration) error {
	// --- bottom of stack: storage ublk, in-memory backend ---
	storageBackend := &memBackend{data: make([]byte, chainSize)}

	storage, err := ublk.New(storageBackend, chainSize)
	if err != nil {
		return fmt.Errorf("create storage ublk: %w", err)
	}
	defer func() { _ = storage.Close() }()
	log.Printf("storage ublk:  %s (%d MiB, in-memory backend)", storage.BlockDevicePath(), chainSize/1024/1024)

	// Open the storage block device with O_DIRECT so the proxy's
	// forwarded I/O goes through our own Backend, not the kernel page
	// cache above it. O_DIRECT requires aligned buffers, which the
	// ublk worker always produces.
	storageFd, err := unix.Open(storage.BlockDevicePath(),
		unix.O_RDWR|unix.O_DIRECT, 0)
	if err != nil {
		return fmt.Errorf("open storage block device: %w", err)
	}
	defer unix.Close(storageFd)

	// --- top of stack: proxy ublk, forwards to storage's block dev ---
	proxyBackend := &fdBackend{fd: storageFd}

	proxy, err := ublk.New(proxyBackend, chainSize)
	if err != nil {
		return fmt.Errorf("create proxy ublk: %w", err)
	}
	defer func() { _ = proxy.Close() }()
	log.Printf("proxy ublk:    %s (%d MiB, forwards to %s)", proxy.BlockDevicePath(), chainSize/1024/1024, storage.BlockDevicePath())

	// --- user side: drive I/O into the proxy, check it shows up
	// byte-exact in the storage's in-memory backend. ---

	proxyFd, err := unix.Open(proxy.BlockDevicePath(),
		unix.O_RDWR|unix.O_DIRECT, 0)
	if err != nil {
		return fmt.Errorf("open proxy block device: %w", err)
	}
	defer unix.Close(proxyFd)

	steps := []struct {
		name string
		fn   func() error
	}{
		{
			"single-block roundtrip at offset 0",
			func() error { return roundtrip(proxyFd, storageBackend, 0, blkSize) },
		},
		{
			"single-block roundtrip at mid-device offset",
			func() error { return roundtrip(proxyFd, storageBackend, chainSize/2, blkSize) },
		},
		{
			"single-block roundtrip at last block offset",
			func() error { return roundtrip(proxyFd, storageBackend, chainSize-blkSize, blkSize) },
		},
		{
			"128 KiB roundtrip (max I/O size)",
			func() error { return roundtrip(proxyFd, storageBackend, 2*blkSize, 128*1024) },
		},
		{
			"16 interleaved 4 KiB roundtrips",
			func() error {
				for i := range 16 {
					off := int64(i) * 2 * blkSize
					if err := roundtrip(proxyFd, storageBackend, off, blkSize); err != nil {
						return fmt.Errorf("iteration %d: %w", i, err)
					}
				}
				return nil
			},
		},
		{
			"bandwidth delta matches through both stacks",
			func() error { return checkStats(proxyBackend, storageBackend) },
		},
	}
	for _, s := range steps {
		if err := runStep(s.name, s.fn, stepTimeout); err != nil {
			return err
		}
	}
	return nil
}

// roundtrip writes a random pattern to the proxy's block device at
// `off`, reads it back through the proxy, and asserts the same bytes
// appear at the same offset in the storage's raw in-memory backend.
func roundtrip(proxyFd int, storageBackend *memBackend, off int64, n int) error {
	pattern := alignedBuf(n)
	if _, err := rand.Read(pattern); err != nil {
		return err
	}

	w, err := unix.Pwrite(proxyFd, pattern, off)
	if err != nil || w != n {
		return fmt.Errorf("pwrite off=%d: n=%d err=%w", off, w, err)
	}

	got := alignedBuf(n)
	r, err := unix.Pread(proxyFd, got, off)
	if err != nil || r != n {
		return fmt.Errorf("pread off=%d: n=%d err=%w", off, r, err)
	}
	if !bytes.Equal(got, pattern) {
		return errors.New("proxy read-back mismatch")
	}

	// The strongest check: did the bytes traverse BOTH ublks and end
	// up verbatim in the storage backend at the same offset?
	stored := storageBackend.slice(off, n)
	if !bytes.Equal(stored, pattern) {
		return errors.New("storage backend does not hold bytes written through proxy")
	}
	return nil
}

func checkStats(proxy *fdBackend, storage *memBackend) error {
	proxyReads := proxy.reads.Load()
	proxyWrites := proxy.writes.Load()
	storageReads := storage.reads.Load()
	storageWrites := storage.writes.Load()

	log.Printf("    proxy:   reads=%d   writes=%d", proxyReads, proxyWrites)
	log.Printf("    storage: reads=%d   writes=%d", storageReads, storageWrites)

	// Every proxy WriteAt must itself produce at least one storage
	// WriteAt (forwarding). Ditto reads. Exact equality isn't
	// guaranteed (the kernel may split/coalesce), but storage counts
	// being strictly smaller than proxy counts would mean we dropped
	// forwarded I/O somewhere.
	if storageWrites < proxyWrites {
		return fmt.Errorf("storage writes (%d) < proxy writes (%d): lost forwarded writes",
			storageWrites, proxyWrites)
	}
	if storageReads < proxyReads {
		return fmt.Errorf("storage reads (%d) < proxy reads (%d): lost forwarded reads",
			storageReads, proxyReads)
	}
	return nil
}

func runStep(name string, fn func() error, timeout time.Duration) error {
	start := time.Now()
	log.Printf("=== %s", name)

	done := make(chan error, 1)
	go func() { done <- fn() }()

	select {
	case err := <-done:
		if err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		log.Printf("    ok in %v", time.Since(start).Truncate(time.Microsecond))
		return nil
	case <-time.After(timeout):
		panic(fmt.Sprintf("step %q hung for %v — dumping goroutines", name, timeout))
	}
}

// alignedBuf returns an n-byte slice whose first element is
// 4096-aligned — required by O_DIRECT on block devices.
func alignedBuf(n int) []byte {
	const align = 4096
	raw := make([]byte, n+align)
	addr := uintptr(unsafe.Pointer(&raw[0]))
	off := int(uintptr(align) - addr%uintptr(align))
	if off == align {
		off = 0
	}
	return raw[off : off+n]
}
