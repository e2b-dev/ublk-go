package ublk

import (
	"sync/atomic"
)

// Stats tracks IO operation statistics for a device.
// All fields are updated atomically and safe for concurrent access.
type Stats struct {
	// Operation counts
	Reads       atomic.Uint64
	Writes      atomic.Uint64
	Flushes     atomic.Uint64
	Discards    atomic.Uint64
	WriteZeroes atomic.Uint64

	// Byte counts
	BytesRead    atomic.Uint64
	BytesWritten atomic.Uint64

	// Error counts
	ReadErrors  atomic.Uint64
	WriteErrors atomic.Uint64
	OtherErrors atomic.Uint64
}

// Snapshot returns a copy of the current statistics.
type StatsSnapshot struct {
	Reads        uint64
	Writes       uint64
	Flushes      uint64
	Discards     uint64
	WriteZeroes  uint64
	BytesRead    uint64
	BytesWritten uint64
	ReadErrors   uint64
	WriteErrors  uint64
	OtherErrors  uint64
}

// Snapshot returns a point-in-time copy of the statistics.
func (s *Stats) Snapshot() StatsSnapshot {
	return StatsSnapshot{
		Reads:        s.Reads.Load(),
		Writes:       s.Writes.Load(),
		Flushes:      s.Flushes.Load(),
		Discards:     s.Discards.Load(),
		WriteZeroes:  s.WriteZeroes.Load(),
		BytesRead:    s.BytesRead.Load(),
		BytesWritten: s.BytesWritten.Load(),
		ReadErrors:   s.ReadErrors.Load(),
		WriteErrors:  s.WriteErrors.Load(),
		OtherErrors:  s.OtherErrors.Load(),
	}
}

// Reset clears all statistics.
func (s *Stats) Reset() {
	s.Reads.Store(0)
	s.Writes.Store(0)
	s.Flushes.Store(0)
	s.Discards.Store(0)
	s.WriteZeroes.Store(0)
	s.BytesRead.Store(0)
	s.BytesWritten.Store(0)
	s.ReadErrors.Store(0)
	s.WriteErrors.Store(0)
	s.OtherErrors.Store(0)
}

// recordOp updates statistics for a completed operation.
func (s *Stats) recordOp(op uint8, bytes uint64, success bool) {
	switch op {
	case UBLK_IO_OP_READ:
		s.Reads.Add(1)
		if success {
			s.BytesRead.Add(bytes)
		} else {
			s.ReadErrors.Add(1)
		}
	case UBLK_IO_OP_WRITE:
		s.Writes.Add(1)
		if success {
			s.BytesWritten.Add(bytes)
		} else {
			s.WriteErrors.Add(1)
		}
	case UBLK_IO_OP_FLUSH:
		s.Flushes.Add(1)
		if !success {
			s.OtherErrors.Add(1)
		}
	case UBLK_IO_OP_DISCARD:
		s.Discards.Add(1)
		if !success {
			s.OtherErrors.Add(1)
		}
	case UBLK_IO_OP_WRITE_ZEROES:
		s.WriteZeroes.Add(1)
		if success {
			s.BytesWritten.Add(bytes)
		} else {
			s.OtherErrors.Add(1)
		}
	default:
		// Unknown operation
		if !success {
			s.OtherErrors.Add(1)
		}
	}
}
