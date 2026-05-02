package uring

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// FuzzRingSubmit drives the SQ/CQ head-tail arithmetic with arbitrary
// submission patterns. The fuzzer input is a byte stream that we slice
// into "submit-batch" sizes; each batch fills the SQ with NOP SQEs
// carrying unique UserData values, calls Submit, then drains the CQ
// and verifies that every UserData submitted comes back exactly once.
//
// This is the regression net for any future refactor of nextSQE,
// flushSQ, WaitCQE, PeekCQE, or SeenCQE — bugs in that arithmetic
// would manifest as duplicated, missing, or reordered CQEs even
// though every test (including TestManyCycles) currently uses a
// uniform full-fill / full-drain cadence.
//
// No kernel module or root required.
func FuzzRingSubmit(f *testing.F) {
	f.Add([]byte{1, 1, 1, 1})
	f.Add([]byte{4, 4, 4, 4})
	f.Add([]byte{8, 7, 6, 5, 4, 3, 2, 1})
	f.Add([]byte{16, 1, 16, 1, 16, 1})
	f.Add([]byte{0, 0, 1, 0, 0, 1})

	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) == 0 {
			return
		}
		const ringSize = 16

		r, err := New(ringSize)
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		defer r.Close()

		var nextID uint64 = 1
		seen := make(map[uint64]int)
		// Cap iterations to keep individual fuzz inputs cheap.
		maxBatches := len(data)
		if maxBatches > 64 {
			maxBatches = 64
		}
		for bi := 0; bi < maxBatches; bi++ {
			// Each input byte determines a batch fill count in
			// [0, ringSize]. 0 exercises the "Submit with empty SQ"
			// path; ringSize+1 would deliberately overflow which we
			// don't want to test here (that's the ring-full case
			// already covered by TestNewSQE128 / TestManyCycles).
			batch := int(data[bi]) % (ringSize + 1)
			for i := 0; i < batch; i++ {
				sqe := r.GetSQE64()
				if sqe == nil {
					// Should not happen because we just emptied the
					// CQ in the previous iteration. If it does, the
					// SQ accounting is wrong.
					t.Fatalf("batch %d slot %d: GetSQE64 returned nil with empty CQ", bi, i)
				}
				sqe.Opcode = 0 // NOP
				sqe.UserData = nextID
				nextID++
			}

			n, err := r.Submit()
			if err != nil {
				t.Fatalf("batch %d Submit: %v", bi, err)
			}
			if n != batch {
				t.Fatalf("batch %d Submit returned %d, expected %d", bi, n, batch)
			}

			// Drain exactly `batch` CQEs. Anything else means we
			// either lost or duplicated one.
			for i := 0; i < batch; i++ {
				cqe, err := r.WaitCQE()
				if err != nil {
					t.Fatalf("batch %d wait %d: %v", bi, i, err)
				}
				if cqe.Res != 0 {
					t.Fatalf("batch %d wait %d: NOP res=%d", bi, i, cqe.Res)
				}
				seen[cqe.UserData]++
				r.SeenCQE()
			}
		}

		// Every submitted UserData must appear exactly once.
		for id := uint64(1); id < nextID; id++ {
			c, ok := seen[id]
			if !ok {
				t.Fatalf("UserData=%d never received", id)
			}
			if c != 1 {
				t.Fatalf("UserData=%d returned %d times, want 1", id, c)
			}
		}
		for id, count := range seen {
			if id < 1 || id >= nextID {
				t.Fatalf("unexpected UserData=%d (count %d) in CQE stream", id, count)
			}
		}
	})
}

// FuzzRingCancel guards the WaitCQE-vs-Cancel race documented in
// AGENTS.md ("Ring.Cancel must be observable from the busy path").
//
// For each fuzz input we:
//  1. Spin up two producers that submit NOP SQEs in tight loops.
//  2. Spin up one consumer that calls WaitCQE in a loop until it
//     returns ErrCancelled (or fails).
//  3. After a fuzz-derived delay, call Cancel.
//  4. Assert that the consumer observes the cancel within a bounded
//     time window even though the CQ is being kept non-empty.
//
// Without the cancelled-flag check at the top of WaitCQE, this test
// fails by hanging until the test deadline. With it, the consumer
// returns within milliseconds.
//
// No kernel module or root required.
func FuzzRingCancel(f *testing.F) {
	f.Add(uint8(0))
	f.Add(uint8(1))
	f.Add(uint8(5))
	f.Add(uint8(50))
	f.Add(uint8(200))

	f.Fuzz(func(t *testing.T, delayBits uint8) {
		// Map fuzz input to a delay in [0, 5] ms. Wider ranges turn
		// the fuzzer into a soak test, which is not what we want here
		// — we want adversarial timing around the WaitCQE entry.
		delay := time.Duration(int(delayBits)%6) * time.Millisecond

		const ringSize = 16
		r, err := New(ringSize)
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		defer r.Close()

		var (
			stopProducers atomic.Bool
			producerWg    sync.WaitGroup
		)

		// Two producer goroutines hammer the SQ. We use a mutex-free
		// pattern: each producer takes its own slot using GetSQE64;
		// if the SQ is full it backs off briefly and retries. This
		// is enough to keep the CQ non-empty during the cancel race.
		var sqMu sync.Mutex
		producer := func() {
			defer producerWg.Done()
			for !stopProducers.Load() {
				sqMu.Lock()
				sqe := r.GetSQE64()
				if sqe == nil {
					sqMu.Unlock()
					time.Sleep(50 * time.Microsecond)
					continue
				}
				sqe.Opcode = 0
				sqe.UserData = 0xCA11
				_, err := r.Submit()
				sqMu.Unlock()
				if err != nil {
					return
				}
			}
		}
		producerWg.Add(2)
		go producer()
		go producer()

		consumerDone := make(chan error, 1)
		go func() {
			for {
				cqe, err := r.WaitCQE()
				if err != nil {
					consumerDone <- err
					return
				}
				_ = cqe
				r.SeenCQE()
			}
		}()

		time.Sleep(delay)
		r.Cancel()

		select {
		case err := <-consumerDone:
			if !errors.Is(err, ErrCancelled) {
				t.Fatalf("consumer returned %v, want ErrCancelled", err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("consumer did not observe Cancel within 2s")
		}

		stopProducers.Store(true)
		producerWg.Wait()
	})
}
