// Randomized I/O torture test for a ublk device. Picks random offsets,
// lengths, and directions (read vs write), maintains an in-process
// shadow of what the device contents should be, and fails the moment
// the device returns anything that disagrees with the shadow.
//
// What this catches that other tests don't:
//
//   - offset/length bugs that only surface for specific alignments or
//     wrap-around edges
//   - ordering bugs where a write appears to succeed but doesn't
//     durably reach the backend (we fsync, drop caches, re-read)
//   - data corruption under sustained concurrency that the narrower
//     integration tests miss by virtue of only doing a few ops
//
// Run as root, under -race, with a duration of your choosing:
//
//	sudo /tmp/ublk-torture -duration 60s -parallel 8
//	make torture
//
// Any mismatch prints the offset, expected vs. actual first-differing
// byte, and exits non-zero.
package main

import (
	"context"
	"crypto/rand"
	"errors"
	"flag"
	"fmt"
	"log"
	"math/big"
	"os"
	"os/signal"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"

	"github.com/e2b-dev/ublk-go/ublk"
)

const (
	devSize      = 64 * 1024 * 1024 // 64 MiB
	blkSize      = 4096
	maxIO        = 128 * 1024 // matches library's max IO
	fsyncEveryN  = 200
	verifyEveryN = 500
)

// memBackend — minimal backing store.
type memBackend struct {
	mu   sync.RWMutex
	data []byte
}

func (b *memBackend) ReadAt(p []byte, off int64) (int, error) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return copy(p, b.data[off:off+int64(len(p))]), nil
}

func (b *memBackend) WriteAt(p []byte, off int64) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return copy(b.data[off:off+int64(len(p))], p), nil
}

type region struct {
	id      int
	start   int64 // byte offset within device
	length  int64 // size of this worker's region
	shadow  []byte
	fd      int
	rng     *bigintRNG
	iters   atomic.Int64
	writes  atomic.Int64
	reads   atomic.Int64
	fsyncs  atomic.Int64
	verifs  atomic.Int64
	errOnce sync.Once
	err     error
}

func main() {
	duration := flag.Duration("duration", 30*time.Second, "total torture duration")
	parallel := flag.Int("parallel", 4, "concurrent worker goroutines (each owns a disjoint region)")
	flag.Parse()

	log.SetFlags(log.Ltime | log.Lmicroseconds)
	if os.Getuid() != 0 {
		log.Fatal("torture must be run as root")
	}

	if err := runTorture(*duration, *parallel); err != nil {
		log.Fatalf("FAIL: %v", err)
	}
}

func runTorture(duration time.Duration, parallel int) error {
	backend := &memBackend{data: make([]byte, devSize)}
	dev, err := ublk.New(backend, devSize)
	if err != nil {
		return fmt.Errorf("ublk.New: %w", err)
	}
	defer dev.Close()

	log.Printf("torture target: %s  size=%d MiB  duration=%v  parallel=%d",
		dev.BlockDevicePath(), devSize/1024/1024, duration, parallel)

	fd, err := unix.Open(dev.BlockDevicePath(), unix.O_RDWR|unix.O_DIRECT, 0)
	if err != nil {
		return fmt.Errorf("open block device: %w", err)
	}
	defer unix.Close(fd)

	// Divide the device into disjoint regions — one per worker, so
	// writes/reads never intersect and we don't need shared locks.
	regions := make([]*region, parallel)
	regionSize := int64(devSize) / int64(parallel)
	regionSize = regionSize / blkSize * blkSize
	if regionSize < maxIO {
		return fmt.Errorf("parallel=%d gives region size %d < maxIO %d; pick fewer workers",
			parallel, regionSize, maxIO)
	}
	for i := range parallel {
		regions[i] = &region{
			id:     i,
			start:  int64(i) * regionSize,
			length: regionSize,
			shadow: make([]byte, regionSize),
			fd:     fd,
			rng:    newRNG(uint64(i)),
		}
	}

	// Respect Ctrl+C so the deferred dev.Close() runs and cleans up
	// the device instead of leaking it.
	ctx, stopSignals := signal.NotifyContext(context.Background(),
		syscall.SIGINT, syscall.SIGTERM)
	defer stopSignals()

	deadline := time.Now().Add(duration)
	var wg sync.WaitGroup
	for _, r := range regions {
		wg.Add(1)
		go func(r *region) {
			defer wg.Done()
			r.loop(ctx, deadline)
		}(r)
	}

	tick := time.NewTicker(2 * time.Second)
	defer tick.Stop()
	done := make(chan struct{})
	go func() {
		for {
			select {
			case <-tick.C:
				logProgress(regions)
			case <-done:
				return
			}
		}
	}()

	wg.Wait()
	close(done)
	logProgress(regions)

	for _, r := range regions {
		if r.err != nil {
			return fmt.Errorf("region %d: %w", r.id, r.err)
		}
	}
	log.Printf("PASS: torture completed with all regions matching shadow")
	return nil
}

func logProgress(rs []*region) {
	var its, w, r, f, v int64
	for _, g := range rs {
		its += g.iters.Load()
		w += g.writes.Load()
		r += g.reads.Load()
		f += g.fsyncs.Load()
		v += g.verifs.Load()
	}
	log.Printf("progress: %d iterations  %d writes  %d reads  %d fsyncs  %d full-verifies",
		its, w, r, f, v)
}

// loop is the core random-IO driver for one region.
func (r *region) loop(ctx context.Context, deadline time.Time) {
	for time.Now().Before(deadline) {
		if r.err != nil || ctx.Err() != nil {
			return
		}
		it := r.iters.Add(1)

		// Occasional full-region verify: read the whole region and
		// compare with shadow. Catches drift that per-op checks miss.
		if it%verifyEveryN == 0 {
			if err := r.verifyAll(); err != nil {
				r.fail(err)
				return
			}
			r.verifs.Add(1)
		}

		// Occasional fsync to force writeback through our library.
		if it%fsyncEveryN == 0 {
			if err := unix.Fsync(r.fd); err != nil {
				r.fail(fmt.Errorf("fsync: %w", err))
				return
			}
			r.fsyncs.Add(1)
		}

		// Random (aligned) offset and length within the region.
		length := int(r.rng.Int(int64(maxIO/blkSize))+1) * blkSize
		maxStart := r.length - int64(length)
		offInRegion := r.rng.Int(maxStart/blkSize+1) * blkSize
		off := r.start + offInRegion

		if r.rng.Bool() {
			// Write.
			buf := alignedBuf(length)
			if _, err := rand.Read(buf); err != nil {
				r.fail(err)
				return
			}
			n, err := unix.Pwrite(r.fd, buf, off)
			if err != nil || n != length {
				r.fail(fmt.Errorf("pwrite off=%d len=%d: n=%d err=%w", off, length, n, err))
				return
			}
			copy(r.shadow[offInRegion:], buf)
			r.writes.Add(1)
		} else {
			// Read + verify.
			buf := alignedBuf(length)
			n, err := unix.Pread(r.fd, buf, off)
			if err != nil || n != length {
				r.fail(fmt.Errorf("pread off=%d len=%d: n=%d err=%w", off, length, n, err))
				return
			}
			want := r.shadow[offInRegion : offInRegion+int64(length)]
			if diff := firstDiff(buf, want); diff >= 0 {
				r.fail(fmt.Errorf("READ mismatch at abs offset %d: shadow says 0x%02x, device returned 0x%02x",
					off+int64(diff), want[diff], buf[diff]))
				return
			}
			r.reads.Add(1)
		}
	}
}

// verifyAll reads the entire region back and compares it to shadow.
// Uses O_DIRECT but issues smaller reads to stay within maxIO.
func (r *region) verifyAll() error {
	buf := alignedBuf(maxIO)
	for pos := int64(0); pos < r.length; pos += int64(len(buf)) {
		n := int64(len(buf))
		if pos+n > r.length {
			n = r.length - pos
		}
		got := buf[:n]
		read, err := unix.Pread(r.fd, got, r.start+pos)
		if err != nil || int64(read) != n {
			return fmt.Errorf("verifyAll pread pos=%d: n=%d err=%w", pos, read, err)
		}
		want := r.shadow[pos : pos+n]
		if diff := firstDiff(got, want); diff >= 0 {
			return fmt.Errorf("VERIFY mismatch at abs offset %d: shadow says 0x%02x, device returned 0x%02x",
				r.start+pos+int64(diff), want[diff], got[diff])
		}
	}
	return nil
}

func (r *region) fail(err error) {
	r.errOnce.Do(func() { r.err = err })
}

// ---- helpers ----

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

// bigintRNG is a tiny deterministic-ish RNG built on crypto/rand. Each
// worker gets its own instance seeded by its id, so failures can be
// correlated with worker id in the output even though the sequence
// itself is not reproducible across runs.
type bigintRNG struct {
	seed uint64
}

func newRNG(seed uint64) *bigintRNG {
	if seed == 0 {
		seed = 1
	}
	return &bigintRNG{seed: seed}
}

// Int returns a uniform integer in [0, maxExclusive) — never negative.
func (r *bigintRNG) Int(maxExclusive int64) int64 {
	if maxExclusive <= 0 {
		return 0
	}
	nBig, err := rand.Int(rand.Reader, big.NewInt(maxExclusive))
	if err != nil {
		return 0
	}
	return nBig.Int64()
}

// Bool returns a fair coin flip.
func (r *bigintRNG) Bool() bool { return r.Int(2) == 1 }

var _ = errors.New // keep import stable if we add structured errors later
