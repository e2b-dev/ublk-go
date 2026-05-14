// Fault-injection probe.
//
// Creates a ublk device whose Backend returns EIO on a configurable
// fraction of operations. Exercises the "backend errors out"
// code path that the rest of the test suite doesn't touch.
//
// Three scenarios, each with per-step timeout:
//
//  1. low-rate errors (10% of writes) — most operations succeed; the
//     block layer sees occasional EIOs. The test asserts at least some
//     Pwrite()s return an error to the caller — proving errors
//     propagate through the full stack (Backend -> worker -> io_uring
//     CQE -> kernel block layer -> /dev/ublkbN fd).
//
//  2. total failure (100% of writes) — every Pwrite to /dev/ublkbN
//     must return an error. Reads still pass. Verifies we don't
//     silently succeed when the backend is dead.
//
//  3. clean close with pending errors — after injecting writes that
//     fail, Close() must still return without hanging. Exercises
//     the shutdown path when the device is in an unhappy state.
//
// Exit 0 = all scenarios behaved correctly. Non-zero = a scenario
// misbehaved (e.g. Pwrite returned success when the backend returned
// EIO), with details.
//
// Requires root and ublk_drv.
//
//	sudo /tmp/ublk-fault
//	make fault
package main

import (
	"errors"
	"flag"
	"fmt"
	"log"
	"math/rand/v2"
	"os"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"

	"github.com/e2b-dev/ublk-go/ublk"
)

const (
	devSize = 16 * 1024 * 1024
	blkSize = 4096
)

// faultyBackend is an in-memory Backend that fails a configurable
// fraction of operations. Rates are in the range [0.0, 1.0].
type faultyBackend struct {
	mu        sync.RWMutex
	data      []byte
	readFail  atomic.Uint64 // encodes fail rate as ppm: fails if rand(1_000_000) < readFail
	writeFail atomic.Uint64
	reads     atomic.Int64
	writes    atomic.Int64
	readErrs  atomic.Int64
	writeErrs atomic.Int64
}

func newFaulty() *faultyBackend {
	return &faultyBackend{data: make([]byte, devSize)}
}

func (b *faultyBackend) setReadFail(rate float64)  { b.readFail.Store(uint64(rate * 1_000_000)) }
func (b *faultyBackend) setWriteFail(rate float64) { b.writeFail.Store(uint64(rate * 1_000_000)) }

func (b *faultyBackend) ReadAt(p []byte, off int64) (int, error) {
	b.reads.Add(1)
	if rand.Uint64N(1_000_000) < b.readFail.Load() {
		b.readErrs.Add(1)
		return 0, unix.EIO
	}
	b.mu.RLock()
	defer b.mu.RUnlock()
	return copy(p, b.data[off:off+int64(len(p))]), nil
}

func (b *faultyBackend) WriteAt(p []byte, off int64) (int, error) {
	b.writes.Add(1)
	if rand.Uint64N(1_000_000) < b.writeFail.Load() {
		b.writeErrs.Add(1)
		return 0, unix.EIO
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return copy(b.data[off:off+int64(len(p))], p), nil
}

func main() {
	timeout := flag.Duration("step-timeout", 30*time.Second, "per-scenario timeout")
	flag.Parse()

	log.SetFlags(log.Ltime | log.Lmicroseconds)
	if os.Getuid() != 0 {
		log.Fatal("fault must be run as root")
	}

	scenarios := []struct {
		name string
		fn   func() error
	}{
		{"low-rate write errors propagate to caller", lowRate},
		{"total write failure causes every Pwrite to fail", totalWriteFail},
		{"total read failure causes every Pread to fail", totalReadFail},
		{"Close succeeds with pending backend errors", closeWithErrors},
	}
	for _, s := range scenarios {
		if err := runStep(s.name, s.fn, *timeout); err != nil {
			log.Fatalf("FAIL: %v", err)
		}
	}
	log.Println("PASS: all fault-injection scenarios behaved as expected")
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

// lowRate: 10% write failure rate. Do 200 writes, expect a non-trivial
// fraction of Pwrites to return EIO. This proves backend errors
// actually propagate up to userspace, not silently swallowed.
func lowRate() error {
	b := newFaulty()
	b.setWriteFail(0.10)

	dev, err := ublk.New(b, devSize)
	if err != nil {
		return fmt.Errorf("ublk.New: %w", err)
	}
	defer dev.Close()

	fd, err := unix.Open(dev.Path(), unix.O_RDWR|unix.O_DIRECT, 0)
	if err != nil {
		return err
	}
	defer unix.Close(fd)

	buf := alignedBlock()
	const attempts = 200
	var callerSawErr int
	for i := range attempts {
		off := int64(i%((devSize-blkSize)/blkSize)) * blkSize
		if _, err := unix.Pwrite(fd, buf, off); err != nil {
			callerSawErr++
		}
	}

	log.Printf("    backend: writes=%d  injected-errors=%d",
		b.writes.Load(), b.writeErrs.Load())
	log.Printf("    caller: attempts=%d  Pwrite-errors-observed=%d",
		attempts, callerSawErr)

	// Sanity: backend should have seen roughly 'attempts' writes
	// (maybe slightly more if kernel retries).
	if b.writes.Load() < attempts {
		return fmt.Errorf("backend saw %d writes but caller attempted %d", b.writes.Load(), attempts)
	}
	if b.writeErrs.Load() == 0 {
		return errors.New("backend injected no errors at 10%% rate (RNG issue?)")
	}
	// The kernel may retry failed I/Os, so caller might see fewer
	// errors than the backend injected. What we must not allow is
	// "all writes succeeded to the caller despite backend failures".
	if callerSawErr == 0 {
		return errors.New("backend returned EIO but NO Pwrite failed — errors are being swallowed")
	}
	return nil
}

// totalWriteFail: 100% write-fail. All Pwrites to /dev/ublkbN must
// return an error.
func totalWriteFail() error {
	b := newFaulty()
	b.setWriteFail(1.0)

	dev, err := ublk.New(b, devSize)
	if err != nil {
		return fmt.Errorf("ublk.New: %w", err)
	}
	defer dev.Close()

	fd, err := unix.Open(dev.Path(), unix.O_RDWR|unix.O_DIRECT, 0)
	if err != nil {
		return err
	}
	defer unix.Close(fd)

	buf := alignedBlock()
	// Just a few attempts — kernel may retry each a handful of times.
	const attempts = 5
	var callerSawErr int
	for range attempts {
		if _, err := unix.Pwrite(fd, buf, 0); err != nil {
			callerSawErr++
		}
	}
	log.Printf("    caller: attempts=%d  Pwrite-errors-observed=%d",
		attempts, callerSawErr)
	if callerSawErr < attempts {
		return fmt.Errorf("only %d/%d Pwrites failed; expected all to fail when backend returns 100%% EIO",
			callerSawErr, attempts)
	}
	return nil
}

// totalReadFail: 100% read-fail. All Pread from /dev/ublkbN must
// return an error.
func totalReadFail() error {
	b := newFaulty()
	b.setReadFail(1.0)

	dev, err := ublk.New(b, devSize)
	if err != nil {
		return fmt.Errorf("ublk.New: %w", err)
	}
	defer dev.Close()

	fd, err := unix.Open(dev.Path(), unix.O_RDONLY|unix.O_DIRECT, 0)
	if err != nil {
		return err
	}
	defer unix.Close(fd)

	buf := alignedBlock()
	const attempts = 5
	var callerSawErr int
	for range attempts {
		if _, err := unix.Pread(fd, buf, 0); err != nil {
			callerSawErr++
		}
	}
	log.Printf("    caller: attempts=%d  Pread-errors-observed=%d",
		attempts, callerSawErr)
	if callerSawErr < attempts {
		return fmt.Errorf("only %d/%d Preads failed; expected all to fail when backend returns 100%% EIO",
			callerSawErr, attempts)
	}
	return nil
}

// closeWithErrors: after submitting some writes that all fail, Close()
// must still return promptly. This catches shutdown-path bugs that
// only surface when the device has error state.
func closeWithErrors() error {
	b := newFaulty()
	b.setWriteFail(1.0)

	dev, err := ublk.New(b, devSize)
	if err != nil {
		return fmt.Errorf("ublk.New: %w", err)
	}

	fd, err := unix.Open(dev.Path(), unix.O_WRONLY|unix.O_DIRECT, 0)
	if err != nil {
		_ = dev.Close()
		return err
	}

	// Submit a handful of writes that will all EIO.
	buf := alignedBlock()
	for range 10 {
		_, _ = unix.Pwrite(fd, buf, 0)
	}
	_ = unix.Close(fd)

	// Close must return within a sane wall-clock.
	closeStart := time.Now()
	closeDone := make(chan error, 1)
	go func() { closeDone <- dev.Close() }()
	select {
	case err := <-closeDone:
		if err != nil {
			return fmt.Errorf("dev.Close: %w", err)
		}
		log.Printf("    dev.Close returned in %v after failed writes",
			time.Since(closeStart).Truncate(time.Microsecond))
	case <-time.After(10 * time.Second):
		return errors.New("dev.Close hung for 10s after injected write failures")
	}
	return nil
}

// alignedBlock returns a single 4 KiB block suitable for O_DIRECT.
func alignedBlock() []byte {
	const n = blkSize
	const align = 4096
	raw := make([]byte, n+align)
	addr := uintptr(unsafe.Pointer(&raw[0]))
	off := int(uintptr(align) - addr%uintptr(align))
	if off == align {
		off = 0
	}
	return raw[off : off+n]
}
