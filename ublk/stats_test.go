package ublk

import (
	"sync"
	"testing"
)

func TestStatsRecordOp(t *testing.T) {
	t.Parallel()
	s := &Stats{}

	// Test successful read
	s.recordOp(UBLK_IO_OP_READ, 4096, true)
	if s.Reads.Load() != 1 {
		t.Errorf("Expected 1 read, got %d", s.Reads.Load())
	}
	if s.BytesRead.Load() != 4096 {
		t.Errorf("Expected 4096 bytes read, got %d", s.BytesRead.Load())
	}

	// Test failed read
	s.recordOp(UBLK_IO_OP_READ, 1024, false)
	if s.Reads.Load() != 2 {
		t.Errorf("Expected 2 reads, got %d", s.Reads.Load())
	}
	if s.ReadErrors.Load() != 1 {
		t.Errorf("Expected 1 read error, got %d", s.ReadErrors.Load())
	}

	// Test successful write
	s.recordOp(UBLK_IO_OP_WRITE, 8192, true)
	if s.Writes.Load() != 1 {
		t.Errorf("Expected 1 write, got %d", s.Writes.Load())
	}
	if s.BytesWritten.Load() != 8192 {
		t.Errorf("Expected 8192 bytes written, got %d", s.BytesWritten.Load())
	}

	// Test flush
	s.recordOp(UBLK_IO_OP_FLUSH, 0, true)
	if s.Flushes.Load() != 1 {
		t.Errorf("Expected 1 flush, got %d", s.Flushes.Load())
	}

	// Test discard
	s.recordOp(UBLK_IO_OP_DISCARD, 0, true)
	if s.Discards.Load() != 1 {
		t.Errorf("Expected 1 discard, got %d", s.Discards.Load())
	}

	// Test write zeroes
	s.recordOp(UBLK_IO_OP_WRITE_ZEROES, 16384, true)
	if s.WriteZeroes.Load() != 1 {
		t.Errorf("Expected 1 write zeroes, got %d", s.WriteZeroes.Load())
	}
}

func TestStatsSnapshot(t *testing.T) {
	t.Parallel()
	s := &Stats{}
	s.Reads.Store(100)
	s.Writes.Store(50)
	s.BytesRead.Store(1024 * 1024)
	s.BytesWritten.Store(512 * 1024)

	snap := s.Snapshot()
	if snap.Reads != 100 {
		t.Errorf("Expected 100 reads in snapshot, got %d", snap.Reads)
	}
	if snap.Writes != 50 {
		t.Errorf("Expected 50 writes in snapshot, got %d", snap.Writes)
	}
}

func TestStatsReset(t *testing.T) {
	t.Parallel()
	s := &Stats{}
	s.Reads.Store(100)
	s.Writes.Store(50)
	s.ReadErrors.Store(5)

	s.Reset()

	if s.Reads.Load() != 0 {
		t.Error("Reset should clear reads")
	}
	if s.Writes.Load() != 0 {
		t.Error("Reset should clear writes")
	}
	if s.ReadErrors.Load() != 0 {
		t.Error("Reset should clear errors")
	}
}

func TestIOResultCodes(t *testing.T) {
	t.Parallel()
	if IOResultOK != 0 {
		t.Errorf("IOResultOK should be 0, got %d", IOResultOK)
	}
	if IOResultEIO != 5 {
		t.Errorf("IOResultEIO should be 5, got %d", IOResultEIO)
	}
	if IOResultENOTSUP != 95 {
		t.Errorf("IOResultENOTSUP should be 95, got %d", IOResultENOTSUP)
	}
}

func TestStatsConcurrent(t *testing.T) {
	t.Parallel()
	s := &Stats{}
	const goroutines = 100
	const opsPerGoroutine = 1000

	var wg sync.WaitGroup
	wg.Add(goroutines)

	for range goroutines {
		go func() {
			defer wg.Done()
			for range opsPerGoroutine {
				s.recordOp(UBLK_IO_OP_READ, 4096, true)
				s.recordOp(UBLK_IO_OP_WRITE, 4096, true)
				_ = s.Snapshot()
			}
		}()
	}

	wg.Wait()

	snap := s.Snapshot()
	expectedOps := uint64(goroutines * opsPerGoroutine)

	if snap.Reads != expectedOps {
		t.Errorf("Expected %d reads, got %d", expectedOps, snap.Reads)
	}
	if snap.Writes != expectedOps {
		t.Errorf("Expected %d writes, got %d", expectedOps, snap.Writes)
	}
	if snap.BytesRead != expectedOps*4096 {
		t.Errorf("Expected %d bytes read, got %d", expectedOps*4096, snap.BytesRead)
	}
}
