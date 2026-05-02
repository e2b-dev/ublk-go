//go:build integration

package ublk

import (
	"bytes"
	"context"
	"errors"
	mrand "math/rand/v2"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// chaosBackend wraps an underlying Backend and probabilistically
// returns unix.EIO and/or inserts a random delay before delegating to
// the wrapped implementation. The configuration is mutable behind the
// mutex so TestChaosRecovery can disable failures mid-run.
//
// Why this is distinct from fault_integration_test.go: the existing
// fault tests only use fully-on or fully-off error modes with no
// latency. Chaos exercises partial failure rates and latency injection,
// which is the realistic failure mode for remote or unreliable storage
// backends.
type chaosBackend struct {
	inner Backend

	mu             sync.Mutex
	rng            *mrand.Rand
	writeErrorRate float64
	readErrorRate  float64
	maxDelay       time.Duration

	writes    atomic.Int64
	reads     atomic.Int64
	writeErrs atomic.Int64
	readErrs  atomic.Int64
}

func newChaosBackend(inner Backend, seed uint64, writeErrRate, readErrRate float64, maxDelay time.Duration) *chaosBackend {
	return &chaosBackend{
		inner:          inner,
		rng:            mrand.New(mrand.NewPCG(seed, seed^0x9e3779b97f4a7c15)),
		writeErrorRate: writeErrRate,
		readErrorRate:  readErrRate,
		maxDelay:       maxDelay,
	}
}

// setRates swaps the failure configuration atomically under the mutex.
// Used by TestChaosRecovery to turn chaos off mid-run and verify that
// subsequent writes/reads behave normally with no residual corruption.
func (c *chaosBackend) setRates(writeErr, readErr float64, maxDelay time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.writeErrorRate = writeErr
	c.readErrorRate = readErr
	c.maxDelay = maxDelay
}

func (c *chaosBackend) Writes() int64    { return c.writes.Load() }
func (c *chaosBackend) Reads() int64     { return c.reads.Load() }
func (c *chaosBackend) WriteErrs() int64 { return c.writeErrs.Load() }
func (c *chaosBackend) ReadErrs() int64  { return c.readErrs.Load() }

// sampleDecision returns (fail, delay) under the mutex. The PRNG is not
// concurrent-safe so the whole decision is taken under the lock; the
// actual delay sleep then happens outside to avoid serialising workers.
func (c *chaosBackend) sampleDecision(errRate float64) (bool, time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	var fail bool
	if errRate > 0 {
		fail = c.rng.Float64() < errRate
	}
	var delay time.Duration
	if c.maxDelay > 0 {
		delay = time.Duration(c.rng.Int64N(int64(c.maxDelay) + 1))
	}
	return fail, delay
}

func (c *chaosBackend) WriteAt(p []byte, off int64) (int, error) {
	c.writes.Add(1)
	fail, delay := c.sampleDecision(c.writeErrorRate)
	if delay > 0 {
		time.Sleep(delay)
	}
	if fail {
		c.writeErrs.Add(1)
		return 0, unix.EIO
	}
	return c.inner.WriteAt(p, off)
}

func (c *chaosBackend) ReadAt(p []byte, off int64) (int, error) {
	c.reads.Add(1)
	fail, delay := c.sampleDecision(c.readErrorRate)
	if delay > 0 {
		time.Sleep(delay)
	}
	if fail {
		c.readErrs.Add(1)
		return 0, unix.EIO
	}
	return c.inner.ReadAt(p, off)
}

const (
	chaosDefaultDevSize = 4 * 1024 * 1024
	chaosBlockSize      = 4096
	chaosDefaultOps     = 200
)

// TestChaosErrorsPropagateAsEIO drives ~chaosDefaultOps direct-IO block
// ops against /dev/ublkbN under a 50% write / 50% read error rate and
// verifies every returned error is unix.EIO, no call panics, and the
// observed error rate lands within a wide tolerance band around 50%.
// The tolerance is intentionally loose because the write path can
// short-circuit before WriteAt (kernel layer checks) and because we're
// only running a few hundred ops.
func TestChaosErrorsPropagateAsEIO(t *testing.T) {
	t.Parallel()

	ops := envInt(t, "UBLK_CHAOS_OPS", chaosDefaultOps)

	mem := newMemBackend(chaosDefaultDevSize)
	chaos := newChaosBackend(mem, 0x1f2e3d4c5b6a7980, 0.5, 0.5, 0)
	dev, err := New(chaos, chaosDefaultDevSize)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = dev.Close() })

	fd, err := unix.Open(dev.Path(), unix.O_RDWR|unix.O_DIRECT, 0)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = unix.Close(fd) })

	buf := alignedBuf(chaosBlockSize)
	for i := range buf {
		buf[i] = byte(i)
	}

	var writeOK, writeErr, readOK, readErr int
	maxBlocks := int64(chaosDefaultDevSize / chaosBlockSize)

	for i := range ops {
		off := (int64(i) * 7 % maxBlocks) * chaosBlockSize
		if i%2 == 0 {
			n, werr := unix.Pwrite(fd, buf, off)
			switch {
			case werr == nil && n == chaosBlockSize:
				writeOK++
			case errors.Is(werr, unix.EIO):
				writeErr++
			default:
				t.Fatalf("pwrite off=%d: n=%d err=%v (want nil or EIO)", off, n, werr)
			}
		} else {
			rbuf := alignedBuf(chaosBlockSize)
			n, rerr := unix.Pread(fd, rbuf, off)
			switch {
			case rerr == nil && n == chaosBlockSize:
				readOK++
			case errors.Is(rerr, unix.EIO):
				readErr++
			default:
				t.Fatalf("pread off=%d: n=%d err=%v (want nil or EIO)", off, n, rerr)
			}
		}
	}

	t.Logf("chaos results: writes ok=%d err=%d  reads ok=%d err=%d  (backend writes=%d errs=%d, reads=%d errs=%d)",
		writeOK, writeErr, readOK, readErr,
		chaos.Writes(), chaos.WriteErrs(), chaos.Reads(), chaos.ReadErrs())

	// The wrapper must have been exercised and must have injected some
	// errors. Wide tolerance — we just want to confirm partial failure
	// is actually happening, not a point estimate.
	totalWrites := writeOK + writeErr
	totalReads := readOK + readErr
	if totalWrites == 0 || totalReads == 0 {
		t.Fatalf("no IO observed: writes=%d reads=%d", totalWrites, totalReads)
	}
	writeErrFrac := float64(writeErr) / float64(totalWrites)
	readErrFrac := float64(readErr) / float64(totalReads)
	if writeErrFrac < 0.30 || writeErrFrac > 0.70 {
		t.Errorf("write error fraction %.2f outside [0.30, 0.70] — wrapper may not be active", writeErrFrac)
	}
	if readErrFrac < 0.30 || readErrFrac > 0.70 {
		t.Errorf("read error fraction %.2f outside [0.30, 0.70] — wrapper may not be active", readErrFrac)
	}
	if chaos.WriteErrs() == 0 || chaos.ReadErrs() == 0 {
		t.Errorf("chaos counters say no injected errors: writeErrs=%d readErrs=%d",
			chaos.WriteErrs(), chaos.ReadErrs())
	}
}

// TestChaosCloseTerminatesUnderLatency verifies Device.Close()
// terminates promptly even when the backend is inserting up-to-50ms of
// random latency into every call and two goroutines are actively
// hammering the block device. Mirrors the pattern from
// TestCloseAfterBackendErrors.
func TestChaosCloseTerminatesUnderLatency(t *testing.T) {
	t.Parallel()

	duration := envDuration(t, "UBLK_CHAOS_DURATION", 1*time.Second)

	mem := newMemBackend(chaosDefaultDevSize)
	chaos := newChaosBackend(mem, 0xabad1deadeadbeef, 0, 0, 50*time.Millisecond)
	dev, err := New(chaos, chaosDefaultDevSize)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	path := dev.Path()
	ctx, cancel := context.WithCancel(context.Background())

	var wg sync.WaitGroup
	for worker := range 2 {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			fd, err := unix.Open(path, unix.O_RDWR|unix.O_DIRECT, 0)
			if err != nil {
				return
			}
			defer unix.Close(fd)
			buf := alignedBuf(chaosBlockSize)
			for i := range buf {
				buf[i] = byte(id + i)
			}
			maxBlocks := int64(chaosDefaultDevSize / chaosBlockSize)
			for i := int64(0); ctx.Err() == nil; i++ {
				off := ((i + int64(id)*31) % maxBlocks) * chaosBlockSize
				if i%2 == 0 {
					_, _ = unix.Pwrite(fd, buf, off)
				} else {
					rbuf := alignedBuf(chaosBlockSize)
					_, _ = unix.Pread(fd, rbuf, off)
				}
			}
		}(worker)
	}

	time.Sleep(duration)
	cancel()
	wg.Wait()

	// AGENTS.md: user fds must be closed before Device.Close(). The
	// per-goroutine deferred unix.Close(fd) above handles that; we
	// also joined them with wg.Wait() before reaching here, so no
	// /dev/ublkbN fds are open from this test when Close runs.
	done := make(chan error, 1)
	go func() { done <- dev.Close() }()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Close: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Device.Close did not return within 10s under chaos latency")
	}
}

// TestChaosRecovery asserts that after the chaos wrapper is flipped
// from "fail every write" to passthrough, subsequent writes and reads
// return the correct data with no residual corruption from the error
// phase. This catches bugs where a failed write leaves the worker,
// ring, or shadow state in an inconsistent position that taints later
// operations.
func TestChaosRecovery(t *testing.T) {
	t.Parallel()

	const n = 16

	mem := newMemBackend(chaosDefaultDevSize)
	chaos := newChaosBackend(mem, 0xcafef00dd15ea5e, 1.0, 0, 0)
	dev, err := New(chaos, chaosDefaultDevSize)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = dev.Close() })

	fd, err := unix.Open(dev.Path(), unix.O_RDWR|unix.O_DIRECT, 0)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = unix.Close(fd) })

	buf := alignedBuf(chaosBlockSize)
	for i := range buf {
		buf[i] = 0xAA
	}

	// Phase 1: every write must fail with EIO.
	for i := range n {
		off := int64(i) * chaosBlockSize
		_, werr := unix.Pwrite(fd, buf, off)
		if !errors.Is(werr, unix.EIO) {
			t.Fatalf("phase1 pwrite off=%d: err=%v, want EIO", off, werr)
		}
	}
	if chaos.WriteErrs() == 0 {
		t.Fatal("phase1: chaos backend reports 0 write errors")
	}

	// Flip to passthrough.
	chaos.setRates(0, 0, 0)

	// Phase 2: write unique patterns and verify each roundtrips.
	patterns := make([][]byte, n)
	for i := range n {
		p := alignedBuf(chaosBlockSize)
		for j := range p {
			p[j] = byte((i * 37) ^ j)
		}
		patterns[i] = p

		off := int64(i) * chaosBlockSize
		if wn, werr := unix.Pwrite(fd, p, off); werr != nil || wn != chaosBlockSize {
			t.Fatalf("phase2 pwrite off=%d: n=%d err=%v", off, wn, werr)
		}
	}

	for i := range n {
		off := int64(i) * chaosBlockSize
		got := alignedBuf(chaosBlockSize)
		rn, rerr := unix.Pread(fd, got, off)
		if rerr != nil || rn != chaosBlockSize {
			t.Fatalf("phase2 pread off=%d: n=%d err=%v", off, rn, rerr)
		}
		if !bytes.Equal(got, patterns[i]) {
			t.Fatalf("phase2 roundtrip mismatch at block %d (first diff at byte %d)",
				i, firstDiff(got, patterns[i]))
		}
	}
}
