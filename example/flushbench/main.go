// flushbench — diagnose *where* time is spent during fs flushes.
//
// Creates a ublk device, formats/mounts it, then runs a sequence of
// operations (write + fsync, write + syncfs, write + drop_caches) and
// prints a per-backend-call trace with microsecond timestamps plus
// per-operation wall-clock.
//
// If you see a large idle gap (hundreds of ms to seconds) between
// backend calls during a flush, the bottleneck is in the kernel
// (jbd2 commit interval, writeback pacing, etc.) and our code is
// idle waiting for kernel-side work. If instead you see continuous
// calls but each taking many ms, the bottleneck is in our stack.
//
// Requires root and ublk_drv.
//
//	sudo go run ./example/flushbench
package main

import (
	"bytes"
	"fmt"
	"log"
	"os"
	"os/exec"
	"sync"
	"sync/atomic"
	"time"

	"github.com/e2b-dev/ublk-go/ublk"
)

const devSize = 64 * 1024 * 1024

type trace struct {
	t0 time.Time
	ev []event
	mu sync.Mutex
}

type event struct {
	at   time.Duration
	op   string // "R" or "W"
	off  int64
	size int
}

func (t *trace) push(op string, off int64, size int) {
	at := time.Since(t.t0)
	t.mu.Lock()
	t.ev = append(t.ev, event{at: at, op: op, off: off, size: size})
	t.mu.Unlock()
}

const printSamples = 10

// printSince prints every event from since the given mark. Also prints
// gap stats so we can see idle periods in the backend.
func (t *trace) printSince(mark time.Duration) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(t.ev) == 0 {
		log.Printf("  (no backend calls)")
		return
	}
	first := -1
	for i, e := range t.ev {
		if e.at >= mark {
			first = i
			break
		}
	}
	if first < 0 {
		log.Printf("  (no backend calls in this window)")
		return
	}
	last := len(t.ev) - 1
	var maxGap time.Duration
	for i := first + 1; i <= last; i++ {
		gap := t.ev[i].at - t.ev[i-1].at
		if gap > maxGap {
			maxGap = gap
		}
	}
	log.Printf("  %d backend calls, max gap between consecutive = %v",
		last-first+1, maxGap.Truncate(time.Microsecond))
	// Print up to printSamples samples spread evenly across the window.
	step := 1
	if last-first+1 > printSamples {
		step = (last - first + 1) / printSamples
	}
	for i := first; i <= last; i += step {
		e := t.ev[i]
		log.Printf("    %10v  %s off=%-10d len=%d",
			e.at.Truncate(time.Microsecond), e.op, e.off, e.size)
	}
}

type tracingBackend struct {
	mu     sync.RWMutex
	data   []byte
	tr     *trace
	reads  atomic.Int64
	writes atomic.Int64
}

func (b *tracingBackend) ReadAt(p []byte, off int64) (int, error) {
	b.reads.Add(1)
	b.tr.push("R", off, len(p))
	b.mu.RLock()
	defer b.mu.RUnlock()
	return copy(p, b.data[off:off+int64(len(p))]), nil
}

func (b *tracingBackend) WriteAt(p []byte, off int64) (int, error) {
	b.writes.Add(1)
	b.tr.push("W", off, len(p))
	b.mu.Lock()
	defer b.mu.Unlock()
	return copy(b.data[off:off+int64(len(p))], p), nil
}

func main() {
	log.SetFlags(log.Ltime | log.Lmicroseconds)
	if os.Getuid() != 0 {
		log.Fatal("flushbench must run as root")
	}
	if err := run(); err != nil {
		log.Fatalf("FAIL: %v", err)
	}
}

func run() error {
	tr := &trace{t0: time.Now()}
	be := &tracingBackend{data: make([]byte, devSize), tr: tr}

	dev, err := ublk.New(be, devSize)
	if err != nil {
		return fmt.Errorf("ublk.New: %w", err)
	}
	defer dev.Close()

	if err := shell("mkfs.ext4", "-q", "-F", dev.Path()); err != nil {
		return err
	}
	mp, err := os.MkdirTemp("", "ublk-flushbench-*")
	if err != nil {
		return err
	}
	defer os.Remove(mp)
	if err := shell("mount", dev.Path(), mp); err != nil {
		return err
	}
	defer shell("umount", mp)

	// Don't reset tr.t0 here — the worker goroutine is already running
	// and reading t0; reassigning would race with it. The trace origin
	// stays at main()'s start; timestamps include the ~tens of ms spent
	// in mkfs/mount, which is actually informative.
	log.Printf("--- mkfs+mount done; experiments start now ---")

	// ---- experiment 1: write + fsync on that file ----
	mark := time.Since(tr.t0)
	content := bytes.Repeat([]byte("A"), 512*1024) // 512 KiB
	measure("write(512KiB) + fsync", func() error {
		f, err := os.Create(mp + "/a.bin")
		if err != nil {
			return err
		}
		defer f.Close()
		if _, err := f.Write(content); err != nil {
			return err
		}
		return f.Sync()
	})
	tr.printSince(mark)

	// ---- experiment 2: write, no explicit sync, just wait for kernel flusher ----
	mark = time.Since(tr.t0)
	content2 := bytes.Repeat([]byte("B"), 2*1024*1024) // 2 MiB
	measure("write(2MiB)", func() error {
		return os.WriteFile(mp+"/b.bin", content2, 0o644)
	})
	// Wait long enough to observe the kernel's bdi writeback fire
	// (/proc/sys/vm/dirty_writeback_centisecs default = 500 = 5s).
	measure("sleep 6s, observe async writeback", func() error {
		time.Sleep(6 * time.Second)
		return nil
	})
	tr.printSince(mark)

	// ---- experiment 3: write + syncfs (scoped to our mount) ----
	mark = time.Since(tr.t0)
	content3 := bytes.Repeat([]byte("C"), 2*1024*1024)
	measure("write(2MiB) + sync -f", func() error {
		if err := os.WriteFile(mp+"/c.bin", content3, 0o644); err != nil {
			return err
		}
		return shell("sync", "-f", mp)
	})
	tr.printSince(mark)

	// ---- experiment 4: drop_caches right after another dirty write ----
	// Mirrors what the probe does; expected to be slow on fresh dirty
	// metadata because jbd2 is forced to commit synchronously.
	mark = time.Since(tr.t0)
	content4 := bytes.Repeat([]byte("D"), 2*1024*1024)
	measure("write(2MiB) + drop_caches=3", func() error {
		if err := os.WriteFile(mp+"/d.bin", content4, 0o644); err != nil {
			return err
		}
		return os.WriteFile("/proc/sys/vm/drop_caches", []byte("3"), 0)
	})
	tr.printSince(mark)

	// ---- experiment 5: flush FIRST, then drop_caches (should be fast) ----
	mark = time.Since(tr.t0)
	measure("sync -f; drop_caches=3", func() error {
		if err := shell("sync", "-f", mp); err != nil {
			return err
		}
		return os.WriteFile("/proc/sys/vm/drop_caches", []byte("3"), 0)
	})
	tr.printSince(mark)

	return nil
}

func measure(name string, fn func() error) {
	start := time.Now()
	err := fn()
	dur := time.Since(start).Truncate(time.Microsecond)
	if err != nil {
		log.Printf("=== %-40s FAIL %v (%v)", name, err, dur)
	} else {
		log.Printf("=== %-40s ok   (%v)", name, dur)
	}
}

func shell(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}
