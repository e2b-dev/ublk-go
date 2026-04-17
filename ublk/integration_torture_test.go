//go:build integration

package ublk

import (
	"context"
	"crypto/rand"
	"fmt"
	"math/big"
	"os"
	"os/signal"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// TestTortureRandomIO is the former `example/torture` harness.
//
// N workers own disjoint byte-ranges of one device. Each worker loops
// random (offset, length, direction) ops against /dev/ublkbN via
// O_DIRECT and maintains an in-process shadow buffer of what the
// region should contain. Any read whose bytes disagree with the shadow
// fails the test. Periodic fsync + full-region rescans exercise the
// write-through and readback paths.
//
// Duration and parallelism are controlled by env vars so the test fits
// into the standard go-test timeout by default:
//
//	UBLK_TORTURE_DURATION=30s   (default)
//	UBLK_TORTURE_PARALLEL=4     (default)
//
// For a longer soak run:  UBLK_TORTURE_DURATION=10m go test -run TestTortureRandomIO ...
func TestTortureRandomIO(t *testing.T) {
	duration := envDuration(t, "UBLK_TORTURE_DURATION", 30*time.Second)
	parallel := envInt(t, "UBLK_TORTURE_PARALLEL", 4)

	const (
		devSize      = 64 * 1024 * 1024
		blkSize      = 4096
		maxIOBytes   = 128 * 1024
		fsyncEveryN  = 200
		verifyEveryN = 500
	)

	be := newMemBackend(devSize)
	dev, err := New(be, devSize)
	if err != nil {
		t.Fatalf("ublk.New: %v", err)
	}
	t.Cleanup(func() { _ = dev.Close() })

	fd, err := unix.Open(dev.BlockDevicePath(), unix.O_RDWR|unix.O_DIRECT, 0)
	if err != nil {
		t.Fatalf("open block device: %v", err)
	}
	t.Cleanup(func() { _ = unix.Close(fd) })

	regionSize := int64(devSize/parallel) / blkSize * blkSize
	if regionSize < maxIOBytes {
		t.Fatalf("parallel=%d gives region size %d < maxIO %d", parallel, regionSize, maxIOBytes)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	deadline := time.Now().Add(duration)

	type region struct {
		id      int
		start   int64
		length  int64
		shadow  []byte
		iters   atomic.Int64
		writes  atomic.Int64
		reads   atomic.Int64
		fsyncs  atomic.Int64
		verifs  atomic.Int64
		errOnce sync.Once
		err     error
	}

	regions := make([]*region, parallel)
	for i := range parallel {
		regions[i] = &region{
			id:     i,
			start:  int64(i) * regionSize,
			length: regionSize,
			shadow: make([]byte, regionSize),
		}
	}

	t.Logf("torture target: %s  size=%d MiB  duration=%v  parallel=%d",
		dev.BlockDevicePath(), devSize/1024/1024, duration, parallel)

	var wg sync.WaitGroup
	for _, r := range regions {
		wg.Add(1)
		go func(r *region) {
			defer wg.Done()
			rng := tortureRNG(uint64(r.id))
			for time.Now().Before(deadline) {
				if r.err != nil || ctx.Err() != nil {
					return
				}
				it := r.iters.Add(1)

				if it%verifyEveryN == 0 {
					if err := tortureVerifyRegion(fd, r.start, r.length, r.shadow); err != nil {
						r.errOnce.Do(func() { r.err = err })
						return
					}
					r.verifs.Add(1)
				}
				if it%fsyncEveryN == 0 {
					if err := unix.Fsync(fd); err != nil {
						r.errOnce.Do(func() { r.err = fmt.Errorf("fsync: %w", err) })
						return
					}
					r.fsyncs.Add(1)
				}

				length := int(rng(int64(maxIOBytes/blkSize))+1) * blkSize
				maxStart := r.length - int64(length)
				offInRegion := rng(maxStart/blkSize+1) * blkSize
				off := r.start + offInRegion

				if rng(2) == 1 {
					buf := e2eAlignedBuf(length)
					if _, err := rand.Read(buf); err != nil {
						r.errOnce.Do(func() { r.err = err })
						return
					}
					n, err := unix.Pwrite(fd, buf, off)
					if err != nil || n != length {
						r.errOnce.Do(func() {
							r.err = fmt.Errorf("pwrite off=%d len=%d: n=%d err=%w",
								off, length, n, err)
						})
						return
					}
					copy(r.shadow[offInRegion:], buf)
					r.writes.Add(1)
				} else {
					buf := e2eAlignedBuf(length)
					n, err := unix.Pread(fd, buf, off)
					if err != nil || n != length {
						r.errOnce.Do(func() {
							r.err = fmt.Errorf("pread off=%d len=%d: n=%d err=%w",
								off, length, n, err)
						})
						return
					}
					want := r.shadow[offInRegion : offInRegion+int64(length)]
					if diff := firstDiff(buf, want); diff >= 0 {
						r.errOnce.Do(func() {
							r.err = fmt.Errorf("READ mismatch at abs offset %d: shadow=0x%02x got=0x%02x",
								off+int64(diff), want[diff], buf[diff])
						})
						return
					}
					r.reads.Add(1)
				}
			}
		}(r)
	}

	// Periodic progress (only visible in -v).
	tick := time.NewTicker(2 * time.Second)
	done := make(chan struct{})
	go func() {
		defer close(done)
		wg.Wait()
	}()
loop:
	for {
		select {
		case <-tick.C:
			var its, w, r, f, v int64
			for _, g := range regions {
				its += g.iters.Load()
				w += g.writes.Load()
				r += g.reads.Load()
				f += g.fsyncs.Load()
				v += g.verifs.Load()
			}
			t.Logf("progress: %d iterations  %d writes  %d reads  %d fsyncs  %d full-verifies",
				its, w, r, f, v)
		case <-done:
			break loop
		}
	}
	tick.Stop()

	for _, r := range regions {
		if r.err != nil {
			t.Fatalf("region %d: %v", r.id, r.err)
		}
	}
	t.Log("torture completed with all regions matching shadow")
}

// tortureVerifyRegion reads the entire region back in maxIO-sized
// chunks and compares against the shadow.
func tortureVerifyRegion(fd int, start, length int64, shadow []byte) error {
	const maxIO = 128 * 1024
	buf := e2eAlignedBuf(maxIO)
	for pos := int64(0); pos < length; pos += int64(len(buf)) {
		n := int64(len(buf))
		if pos+n > length {
			n = length - pos
		}
		got := buf[:n]
		read, err := unix.Pread(fd, got, start+pos)
		if err != nil || int64(read) != n {
			return fmt.Errorf("verify pread pos=%d: n=%d err=%w", pos, read, err)
		}
		want := shadow[pos : pos+n]
		if diff := firstDiff(got, want); diff >= 0 {
			return fmt.Errorf("VERIFY mismatch at abs off %d: shadow=0x%02x got=0x%02x",
				start+pos+int64(diff), want[diff], got[diff])
		}
	}
	return nil
}

// tortureRNG returns a crypto-random int in [0, upper). Seed is
// ignored (crypto/rand doesn't take a seed); the parameter remains as
// documentation of which worker is asking, in case we ever want a
// deterministic PRNG here.
func tortureRNG(_ uint64) func(int64) int64 {
	return func(upper int64) int64 {
		if upper <= 0 {
			return 0
		}
		n, err := rand.Int(rand.Reader, big.NewInt(upper))
		if err != nil {
			return 0
		}
		return n.Int64()
	}
}

func envDuration(t *testing.T, name string, def time.Duration) time.Duration {
	t.Helper()
	s := os.Getenv(name)
	if s == "" {
		return def
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		t.Fatalf("%s: invalid duration %q: %v", name, s, err)
	}
	return d
}

func envInt(t *testing.T, name string, def int) int {
	t.Helper()
	s := os.Getenv(name)
	if s == "" {
		return def
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		t.Fatalf("%s: invalid int %q: %v", name, s, err)
	}
	return n
}
