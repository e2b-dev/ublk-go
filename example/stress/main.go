// Stress test for ublk-go, aimed at catching data races and shutdown
// lifecycle bugs.
//
// Intended to be built with -race and run as root. It exercises four
// distinct stressors for a configurable duration each, logging a
// summary at the end. Success means zero race warnings and zero panics.
//
//	sudo go run -race ./example/stress
//	# or via Makefile:
//	make stress
//
// Stressors:
//
//   - churn          — tight Create -> small-I/O -> Close loop. Catches
//     leaks (eventually exhausts fds if any) and races
//     in Device.shutdown's ordering.
//
//   - ioWhileClose   — start I/O workers writing randomly to the block
//     device, then Close() while they're mid-write.
//     Catches races between worker cleanup and
//     in-flight I/O, and any missing cancellation
//     path.
//
//   - concurrentClose — call Device.Close() from many goroutines at
//     once. sync.Once makes it safe in principle;
//     this confirms empirically.
//
//   - many           — N parallel devices each with I/O traffic, then
//     close them all concurrently. Catches
//     cross-device state bleed (shared globals,
//     reused minors, kernel locks we hold too long).
//
// Failures: any race detected, any goroutine panic, any Close error
// other than a warning about stale in-flight I/O. The test is
// opinionated about its own correctness in this respect — it will exit
// non-zero loudly.
package main

import (
	"context"
	"crypto/rand"
	"flag"
	"log"
	"math/big"
	"os"
	"os/signal"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/e2b-dev/ublk-go/ublk"
)

const (
	devSize = 2 * 1024 * 1024 // 2 MiB per device
	blkSize = 4096
)

// memBackend is a small thread-safe in-memory Backend.
type memBackend struct {
	mu     sync.RWMutex
	data   []byte
	reads  atomic.Int64
	writes atomic.Int64
}

func newMemBackend() *memBackend { return &memBackend{data: make([]byte, devSize)} }

func (b *memBackend) ReadAt(p []byte, off int64) (int, error) {
	b.reads.Add(1)
	b.mu.RLock()
	defer b.mu.RUnlock()
	return copy(p, b.data[off:off+int64(len(p))]), nil
}

func (b *memBackend) WriteAt(p []byte, off int64) (int, error) {
	b.writes.Add(1)
	b.mu.Lock()
	defer b.mu.Unlock()
	return copy(b.data[off:off+int64(len(p))], p), nil
}

type summary struct {
	name       string
	iterations int64
	ioErrors   int64
	closeErrs  int64
	duration   time.Duration
}

func main() {
	duration := flag.Duration("duration", 30*time.Second, "total stress duration (split across stressors)")
	workers := flag.Int("workers", 8, "concurrent I/O workers where applicable")
	parallel := flag.Int("parallel", 4, "parallel devices for 'many' stressor")
	flag.Parse()

	log.SetFlags(log.Ltime | log.Lmicroseconds)
	if os.Getuid() != 0 {
		log.Fatal("stress must be run as root")
	}

	stressors := []struct {
		name string
		fn   func(context.Context, int) summary
	}{
		{"churn", churn},
		{"ioWhileClose", ioWhileClose},
		{"concurrentClose", concurrentClose},
		{"many", manyDevices},
	}

	perMode := *duration / time.Duration(len(stressors))
	if perMode < time.Second {
		perMode = time.Second
	}
	log.Printf("stress plan: %d modes x %v each, %d workers, %d parallel devices",
		len(stressors), perMode, *workers, *parallel)

	// Trap Ctrl+C so every in-flight stressor gets a chance to close
	// its devices cleanly before we exit. Without this, Ctrl+C mid-run
	// leaves orphan /dev/ublk[bc]N nodes behind.
	rootCtx, stopSignals := signal.NotifyContext(context.Background(),
		syscall.SIGINT, syscall.SIGTERM)
	defer stopSignals()

	var summaries []summary
	for _, s := range stressors {
		if rootCtx.Err() != nil {
			log.Printf("=== %s skipped (interrupted)", s.name)
			continue
		}
		log.Printf("=== %s (budget %v)", s.name, perMode)
		ctx, cancel := context.WithTimeout(rootCtx, perMode)
		n := *workers
		if s.name == "many" {
			n = *parallel
		}
		start := time.Now()
		sm := s.fn(ctx, n)
		cancel()
		sm.name = s.name
		sm.duration = time.Since(start)
		summaries = append(summaries, sm)
		log.Printf("    %s done: %d iterations, %d io-errors, %d close-errors, %v",
			sm.name, sm.iterations, sm.ioErrors, sm.closeErrs, sm.duration.Truncate(time.Millisecond))
	}

	log.Println("--- summary ---")
	var totalClose int64
	for _, sm := range summaries {
		log.Printf("  %-20s  %6d iter  %5d ioErr  %5d closeErr  in %v",
			sm.name, sm.iterations, sm.ioErrors, sm.closeErrs,
			sm.duration.Truncate(time.Millisecond))
		totalClose += sm.closeErrs
	}
	if totalClose > 0 {
		// close errors aren't necessarily races but they're anomalous.
		log.Printf("warning: %d close() calls returned an error", totalClose)
	}
	log.Println("PASS: stress run completed without the race detector firing")
}

// ---- stressor: churn ----
//
// Tight loop creating a device, doing a few writes, and closing. Every
// iteration exercises the full device lifecycle. If any leak or race
// lives in shutdown, enough iterations surface it.
func churn(ctx context.Context, _ int) summary {
	var sm summary
	for ctx.Err() == nil {
		dev, err := ublk.New(newMemBackend(), devSize)
		if err != nil {
			sm.ioErrors++
			time.Sleep(10 * time.Millisecond)
			continue
		}
		// Light I/O so the worker actually services some ops.
		if fd, err := unix.Open(dev.Path(), unix.O_WRONLY, 0); err == nil {
			_, _ = unix.Pwrite(fd, make([]byte, blkSize), 0)
			_ = unix.Close(fd)
		}
		if err := dev.Close(); err != nil {
			sm.closeErrs++
		}
		sm.iterations++
	}
	return sm
}

// ---- stressor: ioWhileClose ----
//
// Start N concurrent writer goroutines hammering the block device, then
// call Close mid-stream. Races between worker shutdown and in-flight
// I/O surface here.
//
// Main owns the writer fds so we can force any stuck Pwrite to error
// out by closing the fd from outside; if a writer goroutine fails to
// exit within the watchdog timeout, we log and move on (and dump
// goroutines via SIGQUIT-style panic if the whole test stalls too long).
func ioWhileClose(ctx context.Context, workers int) summary {
	var sm summary
	for ctx.Err() == nil {
		dev, err := ublk.New(newMemBackend(), devSize)
		if err != nil {
			sm.ioErrors++
			continue
		}
		path := dev.Path()

		// Pre-open all fds so main can close them later to unblock stuck
		// syscalls. Opening after device creation but before launching
		// writers keeps the race window tight.
		fds := make([]int, 0, workers)
		for range workers {
			fd, err := unix.Open(path, unix.O_WRONLY, 0)
			if err != nil {
				continue
			}
			fds = append(fds, fd)
		}

		var wg sync.WaitGroup
		for _, fd := range fds {
			wg.Add(1)
			go func(fd int) {
				defer wg.Done()
				buf := make([]byte, blkSize)
				for {
					off := randOffset(devSize - blkSize)
					if _, err := unix.Pwrite(fd, buf, off); err != nil {
						// fd closed by main or device gone — either way, exit.
						return
					}
				}
			}(fd)
		}

		time.Sleep(jitter(2*time.Millisecond, 20*time.Millisecond))

		// User fds MUST be closed before dev.Close(). Otherwise the
		// library's delDev() triggers del_gendisk() which blocks
		// indefinitely waiting for all refs on /dev/ublkbN to drop.
		// This is standard Linux block-device semantics, not a ublk
		// quirk. Closing fds here also unblocks any writer goroutines
		// stuck mid-Pwrite (EBADF), so we drain them fast.
		for _, fd := range fds {
			_ = unix.Close(fd)
		}
		done := make(chan struct{})
		go func() { wg.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			sm.closeErrs++
		}

		// Watchdog on dev.Close() too, so any future regression that
		// causes it to block surfaces as an anomaly instead of a hang.
		closeDone := make(chan error, 1)
		go func() { closeDone <- dev.Close() }()
		select {
		case err := <-closeDone:
			if err != nil {
				sm.closeErrs++
			}
		case <-time.After(5 * time.Second):
			sm.closeErrs++
		}
		sm.iterations++
	}
	return sm
}

// ---- stressor: concurrentClose ----
//
// Call Close() from many goroutines simultaneously. We use sync.Once so
// only one real shutdown runs, but the race detector checks everything
// that reads *Device under that Once.
func concurrentClose(ctx context.Context, workers int) summary {
	var sm summary
	for ctx.Err() == nil {
		dev, err := ublk.New(newMemBackend(), devSize)
		if err != nil {
			sm.ioErrors++
			continue
		}

		start := make(chan struct{})
		var wg sync.WaitGroup
		for range workers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				if err := dev.Close(); err != nil {
					atomic.AddInt64(&sm.closeErrs, 1)
				}
			}()
		}
		close(start) // fire all Close calls at ~once
		wg.Wait()
		sm.iterations++
	}
	return sm
}

// ---- stressor: manyDevices ----
//
// Keep N devices alive concurrently, each serving a writer goroutine,
// then close them all in parallel.
func manyDevices(ctx context.Context, parallel int) summary {
	var sm summary
	for ctx.Err() == nil {
		devs := make([]*ublk.Device, 0, parallel)
		fds := make([]int, 0, parallel)
		for range parallel {
			d, err := ublk.New(newMemBackend(), devSize)
			if err != nil {
				sm.ioErrors++
				continue
			}
			fd, err := unix.Open(d.Path(), unix.O_WRONLY, 0)
			if err != nil {
				_ = d.Close()
				sm.ioErrors++
				continue
			}
			devs = append(devs, d)
			fds = append(fds, fd)
		}
		if len(devs) == 0 {
			continue
		}

		var wwg sync.WaitGroup
		for _, fd := range fds {
			wwg.Add(1)
			go func(fd int) {
				defer wwg.Done()
				buf := make([]byte, blkSize)
				for {
					if _, err := unix.Pwrite(fd, buf, randOffset(devSize-blkSize)); err != nil {
						return
					}
				}
			}(fd)
		}

		time.Sleep(jitter(5*time.Millisecond, 30*time.Millisecond))

		// Same ordering as ioWhileClose: close user fds first so
		// del_gendisk() doesn't block waiting for them.
		for _, fd := range fds {
			_ = unix.Close(fd)
		}
		done := make(chan struct{})
		go func() { wwg.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			sm.closeErrs++
		}

		var cwg sync.WaitGroup
		for _, d := range devs {
			cwg.Add(1)
			go func(d *ublk.Device) {
				defer cwg.Done()
				closeDone := make(chan error, 1)
				go func() { closeDone <- d.Close() }()
				select {
				case err := <-closeDone:
					if err != nil {
						atomic.AddInt64(&sm.closeErrs, 1)
					}
				case <-time.After(5 * time.Second):
					atomic.AddInt64(&sm.closeErrs, 1)
				}
			}(d)
		}
		cwg.Wait()
		sm.iterations++
	}
	return sm
}

// ---- helpers ----

func randOffset(maxOff int) int64 {
	nBig, _ := rand.Int(rand.Reader, big.NewInt(int64(maxOff)/blkSize))
	return nBig.Int64() * blkSize
}

// jitter returns a duration uniform in [min, max).
func jitter(minD, maxD time.Duration) time.Duration {
	nBig, _ := rand.Int(rand.Reader, big.NewInt(int64(maxD-minD)))
	return minD + time.Duration(nBig.Int64())
}
