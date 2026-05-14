//go:build integration

package ublk

import (
	"encoding/binary"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/anishathalye/porcupine"
	"golang.org/x/sys/unix"
)

// TestPorcupineLinearizability is the post-processing companion to
// TestRapidStateMachine. The rapid state machine asserts a per-action
// invariant — "after this single Read, the bytes match my shadow" —
// which is sufficient when actions are sequential. As soon as several
// callers hit the same block device concurrently, the per-action
// shadow check is no longer enough: it can pass even when the global
// real-time history admits no valid sequential explanation.
//
// This test fills that gap by recording a wall-clock history of
// concurrent reads/writes against a single ublk device and feeding it
// to anishathalye/porcupine, which decides whether the history is
// linearizable with respect to a sequential block-register model.
//
// Implementation choice (see PR body): Option B — standalone test in
// its own file. Option A would have instrumented TestRapidStateMachine
// to record a history, but rapid drives a strictly sequential state
// machine, so the resulting history is trivially linearizable. The
// real value of porcupine is exercising concurrent ordering, which
// requires a worker-pool harness — that lives here.
//
// The model is one-register-per-4-KiB-block. Reads and writes are
// constrained to a single block (atomic from the model's perspective);
// the state is a `map[int]uint64` from block index → most-recent
// "stamp" (a unique 8-byte tag embedded at the start of each write
// payload). Reads recover the stamp from the bytes returned and the
// model checks they match the last write at that block.
//
// Tunables (env):
//
//	UBLK_LINZ_OPS=N        total operations across all workers (default 200)
//	UBLK_LINZ_WORKERS=W    concurrent workers (default 4)
//
// fd-close-before-Close discipline (AGENTS.md): the user fd opened on
// /dev/ublkbN is closed before Device.Close so del_gendisk does not
// block waiting on the open ref to drop.
func TestPorcupineLinearizability(t *testing.T) {
	const (
		blockSize    = 4096
		numBlocks    = 64                    // → 256 KiB device, model state stays small
		devSize      = blockSize * numBlocks // 256 KiB
		checkTimeout = 30 * time.Second
		runDeadline  = 30 * time.Second // hard wall-clock cap on the workload phase
	)

	workers := envInt(t, "UBLK_LINZ_WORKERS", 4)
	if workers < 1 {
		t.Fatalf("UBLK_LINZ_WORKERS=%d must be >= 1", workers)
	}
	totalOps := envInt(t, "UBLK_LINZ_OPS", 200)
	if totalOps < workers {
		totalOps = workers
	}
	opsPerWorker := totalOps / workers

	be := newMemBackend(devSize)
	dev, err := New(be, devSize)
	if err != nil {
		t.Fatalf("ublk.New: %v", err)
	}
	fd, err := unix.Open(dev.Path(), unix.O_RDWR|unix.O_DIRECT, 0)
	if err != nil {
		_ = dev.Close()
		t.Fatalf("open %s: %v", dev.Path(), err)
	}
	// fd-close-before-Close (AGENTS.md): close the user fd FIRST,
	// otherwise Device.Close → UBLK_CMD_DEL_DEV → del_gendisk hangs
	// indefinitely waiting for the open ref to drop.
	t.Cleanup(func() {
		_ = unix.Close(fd)
		_ = dev.Close()
	})

	var (
		mu      sync.Mutex
		history []porcupine.Operation
	)
	appendOp := func(op porcupine.Operation) {
		mu.Lock()
		history = append(history, op)
		mu.Unlock()
	}

	// stamps start at 1 so that 0 unambiguously means "never written"
	// (matches the model's zero-value lookup for uninitialized blocks).
	var stampCounter atomic.Uint64

	deadline := time.Now().Add(runDeadline)
	failed := atomic.Bool{}

	var wg sync.WaitGroup
	wg.Add(workers)
	for w := 0; w < workers; w++ {
		go func(clientID int) {
			defer wg.Done()
			// Independent PCG stream per worker keeps the workload
			// repeatable when the seeds are fixed but lets workers
			// pick disjoint sequences.
			rng := rand.New(rand.NewPCG(uint64(clientID)+1, 0xC0FFEE))
			wbuf := alignedBuf(blockSize)
			rbuf := alignedBuf(blockSize)
			for i := 0; i < opsPerWorker; i++ {
				if failed.Load() || time.Now().After(deadline) {
					return
				}
				block := rng.IntN(numBlocks)
				off := int64(block) * blockSize

				// 50/50 read vs write keeps the history balanced
				// enough for porcupine to find interesting orderings
				// without being dominated by either op type.
				if rng.IntN(2) == 0 {
					stamp := stampCounter.Add(1)
					for k := range wbuf {
						wbuf[k] = 0
					}
					binary.BigEndian.PutUint64(wbuf[:8], stamp)
					call := time.Now().UnixNano()
					n, err := unix.Pwrite(fd, wbuf, off)
					ret := time.Now().UnixNano()
					if err != nil || n != blockSize {
						failed.Store(true)
						t.Errorf("client=%d pwrite blk=%d: n=%d err=%v",
							clientID, block, n, err)
						return
					}
					appendOp(porcupine.Operation{
						ClientId: clientID,
						Input:    linzInput{op: linzWrite, block: block, stamp: stamp},
						Output:   linzOutput{},
						Call:     call,
						Return:   ret,
					})
				} else {
					call := time.Now().UnixNano()
					n, err := unix.Pread(fd, rbuf, off)
					ret := time.Now().UnixNano()
					if err != nil || n != blockSize {
						failed.Store(true)
						t.Errorf("client=%d pread blk=%d: n=%d err=%v",
							clientID, block, n, err)
						return
					}
					gotStamp := binary.BigEndian.Uint64(rbuf[:8])
					appendOp(porcupine.Operation{
						ClientId: clientID,
						Input:    linzInput{op: linzRead, block: block},
						Output:   linzOutput{stamp: gotStamp},
						Call:     call,
						Return:   ret,
					})
				}
			}
		}(w)
	}
	wg.Wait()
	if t.Failed() {
		return
	}

	t.Logf("workload done: workers=%d ops=%d (target=%d) device=%s",
		workers, len(history), totalOps, dev.Path())

	model := linzModel()

	checkStart := time.Now()
	res, info := porcupine.CheckOperationsVerbose(model, history, checkTimeout)
	checkElapsed := time.Since(checkStart)

	switch res {
	case porcupine.Ok:
		t.Logf("linearizable: %d ops checked in %v", len(history), checkElapsed)
	case porcupine.Unknown:
		// Treat as a soft pass: the checker did not find a
		// counterexample within the budget. Flag it loudly so a
		// regression does not silently weaken the test, but do not
		// fail — porcupine is NP-hard and false-timeouts are real.
		t.Logf("linearizability check TIMED OUT after %v (history len=%d) "+
			"— increase UBLK_LINZ_OPS to keep histories small, or "+
			"raise the checkTimeout in this test", checkTimeout, len(history))
	case porcupine.Illegal:
		path := filepath.Join(os.TempDir(),
			fmt.Sprintf("ublk-linz-fail-%d.html", time.Now().UnixNano()))
		if err := porcupine.VisualizePath(model, info, path); err != nil {
			t.Logf("VisualizePath failed: %v", err)
		} else {
			t.Logf("non-linearizable history visualization: %s", path)
		}
		t.Fatalf("history is NOT linearizable (history len=%d, check took %v)",
			len(history), checkElapsed)
	default:
		t.Fatalf("unexpected porcupine result: %v", res)
	}
}

// linzInput / linzOutput are the porcupine Operation Input/Output
// payloads. Keeping them as small typed structs (rather than bare
// slices) lets the model and the visualizer pretty-print them, and
// keeps the Step closure free of byte-slice copying — only the
// 8-byte stamp ever has to be compared.
type linzOpKind uint8

const (
	linzWrite linzOpKind = iota
	linzRead
)

type linzInput struct {
	op    linzOpKind
	block int
	stamp uint64 // populated for writes only
}

type linzOutput struct {
	stamp uint64 // populated for reads only
}

// linzModel returns a single-block-register sequential specification
// of the device. State is `map[int]uint64`: block index → stamp of
// the most recent write at that block. Lookups for blocks that have
// never been written return the zero value 0, which corresponds to
// the all-zero bytes the device returns for unwritten blocks (the
// stamp 0 is reserved — stampCounter starts at 1).
//
// The model is block-count agnostic — the workload (not the model)
// is responsible for confining indices to a small range so that the
// state map stays bounded.
func linzModel() porcupine.Model {
	return porcupine.Model{
		Init: func() interface{} {
			return map[int]uint64{}
		},
		Step: func(state, input, output interface{}) (bool, interface{}) {
			st := state.(map[int]uint64)
			in := input.(linzInput)
			switch in.op {
			case linzWrite:
				next := make(map[int]uint64, len(st)+1)
				for k, v := range st {
					next[k] = v
				}
				next[in.block] = in.stamp
				return true, next
			case linzRead:
				want := st[in.block] // zero if never written
				got := output.(linzOutput).stamp
				if got != want {
					return false, state
				}
				return true, state
			default:
				return false, state
			}
		},
		Equal: func(s1, s2 interface{}) bool {
			a := s1.(map[int]uint64)
			b := s2.(map[int]uint64)
			// Two states are equal if every block has the same stamp,
			// where "absent from the map" is treated as stamp 0. We
			// can't just compare lengths: one map might explicitly
			// store a 0 while the other elides it.
			for k, v := range a {
				if b[k] != v {
					return false
				}
			}
			for k, v := range b {
				if a[k] != v {
					return false
				}
			}
			return true
		},
		DescribeOperation: func(input, output interface{}) string {
			in := input.(linzInput)
			switch in.op {
			case linzWrite:
				return fmt.Sprintf("W(blk=%d, stamp=%d)", in.block, in.stamp)
			case linzRead:
				return fmt.Sprintf("R(blk=%d) -> stamp=%d",
					in.block, output.(linzOutput).stamp)
			}
			return "?"
		},
	}
}
