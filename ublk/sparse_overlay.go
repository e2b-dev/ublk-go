package ublk

import (
	"fmt"
	"io"
	"iter"
	"os"
	"sync/atomic"

	"golang.org/x/sys/unix"
)

// SparseOverlay is a sparse file optimized for copy-on-write overlays.
// It uses SEEK_HOLE/SEEK_DATA for efficient dirty extent tracking without
// maintaining an in-memory bitmap.
//
// Key features:
//   - Lock-free I/O (pread/pwrite are kernel-atomic)
//   - O(extents) dirty iteration using kernel's sparse file tracking
//   - Compatible with COWBackend interface via ClassifyRange
//
// Usage:
//
//	overlay, _ := NewSparseOverlay("/path/to/overlay", size)
//	defer overlay.Close()
//
//	// Check if range is dirty
//	allDirty, allClean := overlay.ClassifyRange(offset, length)
//
//	// Iterate dirty extents
//	for extent := range overlay.DirtyExtents() {
//	    fmt.Printf("dirty: %d-%d\n", extent.Offset, extent.Offset+extent.Length)
//	}
type SparseOverlay struct {
	file *os.File
	size int64

	// Statistics (optional, can be ignored)
	writes     atomic.Uint64
	bytesWrite atomic.Uint64
}

// Extent represents a contiguous region in the file.
type Extent struct {
	Offset int64
	Length int64
}

// Segment represents a contiguous region with uniform dirty/clean status.
// Used for batched mixed reads - each segment can be read in a single I/O.
type Segment struct {
	Offset   int64 // Offset in the file/device
	Length   int64 // Length of this segment
	BufOff   int64 // Offset into the target buffer
	FromBase bool  // true = read from base (clean), false = read from overlay (dirty)
}

// NewSparseOverlay creates a new sparse overlay file.
// The file is created/truncated at the given path with the specified size.
func NewSparseOverlay(path string, size int64) (*SparseOverlay, error) {
	file, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return nil, fmt.Errorf("create overlay: %w", err)
	}

	if err := file.Truncate(size); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("truncate overlay: %w", err)
	}

	return &SparseOverlay{
		file: file,
		size: size,
	}, nil
}

// NewSparseOverlayFromFile wraps an existing file as a sparse overlay.
// The file should already be truncated to the desired size.
func NewSparseOverlayFromFile(file *os.File, size int64) *SparseOverlay {
	return &SparseOverlay{
		file: file,
		size: size,
	}
}

// File returns the underlying file for zero-copy registration.
// Implements the Overlay() method needed by COWBackend.
func (s *SparseOverlay) File() *os.File {
	return s.file
}

// Size returns the overlay size.
func (s *SparseOverlay) Size() int64 {
	return s.size
}

// ReadAt reads from the overlay file.
func (s *SparseOverlay) ReadAt(p []byte, off int64) (int, error) {
	return s.file.ReadAt(p, off)
}

// WriteAt writes to the overlay file.
func (s *SparseOverlay) WriteAt(p []byte, off int64) (int, error) {
	n, err := s.file.WriteAt(p, off)
	if err == nil {
		s.writes.Add(1)
		s.bytesWrite.Add(uint64(n))
	}
	return n, err
}

// Sync flushes the overlay to disk.
func (s *SparseOverlay) Sync() error {
	return s.file.Sync()
}

// Close closes the overlay file.
func (s *SparseOverlay) Close() error {
	return s.file.Close()
}

// ClassifyRange determines if a byte range is dirty (has data) or clean (hole).
// Returns:
//   - allDirty=true: entire range has been written
//   - allClean=true: entire range is a hole (never written)
//   - both false: range spans both data and hole regions
//
// This is compatible with COWBackend.ClassifyRange.
func (s *SparseOverlay) ClassifyRange(off, length int64) (allDirty, allClean bool) {
	if length <= 0 {
		return false, true
	}

	end := off + length
	fd := int(s.file.Fd())

	// Find first data at or after 'off'
	dataStart, err := unix.Seek(fd, off, unix.SEEK_DATA)
	if err != nil {
		// ENXIO: no data from 'off' to EOF -> entire range is clean
		return false, true
	}

	if dataStart >= end {
		// Data starts after our range -> entire range is clean
		return false, true
	}

	// There's data in our range. Check if it starts at 'off'
	if dataStart > off {
		// Hole before data -> mixed
		return false, false
	}

	// Data starts at 'off'. Find where it ends (next hole)
	holeStart, err := unix.Seek(fd, dataStart, unix.SEEK_HOLE)
	if err != nil {
		// No hole found -> data extends to EOF
		holeStart = s.size
	}

	if holeStart >= end {
		// Data extends past our range -> all dirty
		return true, false
	}

	// Hole starts before end of range -> mixed
	return false, false
}

// IsDirty checks if any part of the range has been written.
func (s *SparseOverlay) IsDirty(off, length int64) bool {
	allDirty, allClean := s.ClassifyRange(off, length)
	return allDirty || (!allClean) // dirty if not all clean
}

// SegmentRange partitions a byte range into contiguous segments, each either
// entirely dirty (overlay) or entirely clean (base). This enables batched
// I/O for mixed reads - instead of reading block-by-block, read each segment
// in a single operation.
//
// Example: a range with pattern base-base-dirty-dirty-base yields 3 segments:
//
//	for seg := range overlay.SegmentRange(off, length) {
//	    if seg.FromBase {
//	        base.ReadAt(buf[seg.BufOff:seg.BufOff+seg.Length], seg.Offset)
//	    } else {
//	        overlay.ReadAt(buf[seg.BufOff:seg.BufOff+seg.Length], seg.Offset)
//	    }
//	}
func (s *SparseOverlay) SegmentRange(off, length int64) iter.Seq[Segment] {
	return func(yield func(Segment) bool) {
		if length <= 0 {
			return
		}

		fd := int(s.file.Fd())
		end := off + length
		pos := off
		bufOff := int64(0)

		for pos < end {
			// Try to find data at current position
			dataStart, err := unix.Seek(fd, pos, unix.SEEK_DATA)

			if err != nil || dataStart >= end {
				// No more data in range - rest is clean (base)
				if !yield(Segment{
					Offset:   pos,
					Length:   end - pos,
					BufOff:   bufOff,
					FromBase: true,
				}) {
					return
				}
				return
			}

			// If there's a hole before data, emit base segment
			if dataStart > pos {
				holeLen := min(dataStart, end) - pos
				if !yield(Segment{
					Offset:   pos,
					Length:   holeLen,
					BufOff:   bufOff,
					FromBase: true,
				}) {
					return
				}
				bufOff += holeLen
				pos = dataStart
				if pos >= end {
					return
				}
			}

			// Find where data ends (next hole)
			holeStart, err := unix.Seek(fd, pos, unix.SEEK_HOLE)
			if err != nil {
				holeStart = s.size
			}

			// Emit overlay segment
			dataLen := min(holeStart, end) - pos
			if !yield(Segment{
				Offset:   pos,
				Length:   dataLen,
				BufOff:   bufOff,
				FromBase: false,
			}) {
				return
			}
			bufOff += dataLen
			pos += dataLen
		}
	}
}

// IsClean checks if the entire range is unwritten (hole).
func (s *SparseOverlay) IsClean(off, length int64) bool {
	_, allClean := s.ClassifyRange(off, length)
	return allClean
}

// DirtyExtents returns an iterator over all dirty (data) extents.
// Uses SEEK_HOLE/SEEK_DATA for O(extents) complexity.
//
// Example:
//
//	for extent := range overlay.DirtyExtents() {
//	    fmt.Printf("offset=%d length=%d\n", extent.Offset, extent.Length)
//	}
func (s *SparseOverlay) DirtyExtents() iter.Seq[Extent] {
	return func(yield func(Extent) bool) {
		fd := int(s.file.Fd())
		offset := int64(0)

		for offset < s.size {
			// Find next data region
			dataStart, err := unix.Seek(fd, offset, unix.SEEK_DATA)
			if err != nil {
				// ENXIO: no more data
				return
			}

			// Find end of data region (next hole)
			holeStart, err := unix.Seek(fd, dataStart, unix.SEEK_HOLE)
			if err != nil {
				holeStart = s.size
			}

			if !yield(Extent{
				Offset: dataStart,
				Length: holeStart - dataStart,
			}) {
				return
			}

			offset = holeStart
		}
	}
}

// CleanExtents returns an iterator over all clean (hole) extents.
// Uses SEEK_HOLE/SEEK_DATA for O(extents) complexity.
func (s *SparseOverlay) CleanExtents() iter.Seq[Extent] {
	return func(yield func(Extent) bool) {
		fd := int(s.file.Fd())
		offset := int64(0)

		for offset < s.size {
			// Find next hole region
			holeStart, err := unix.Seek(fd, offset, unix.SEEK_HOLE)
			if err != nil || holeStart >= s.size {
				return
			}

			// Find end of hole region (next data)
			dataStart, err := unix.Seek(fd, holeStart, unix.SEEK_DATA)
			if err != nil {
				// ENXIO: hole extends to EOF
				dataStart = s.size
			}

			if !yield(Extent{
				Offset: holeStart,
				Length: dataStart - holeStart,
			}) {
				return
			}

			offset = dataStart
		}
	}
}

// DirtyBytes returns the total bytes written (sum of data extent lengths).
func (s *SparseOverlay) DirtyBytes() int64 {
	var total int64
	for ext := range s.DirtyExtents() {
		total += ext.Length
	}
	return total
}

// DirtyExtentCount returns the number of dirty extents.
func (s *SparseOverlay) DirtyExtentCount() int {
	count := 0
	for range s.DirtyExtents() {
		count++
	}
	return count
}

// ExportDiff writes all dirty data to the given writer.
// Format: [offset:8][length:8][data:length]...
func (s *SparseOverlay) ExportDiff(w io.Writer) error {
	buf := make([]byte, 256*1024)

	for ext := range s.DirtyExtents() {
		// Write header
		header := make([]byte, 16)
		putUint64LE(header[0:8], uint64(ext.Offset))
		putUint64LE(header[8:16], uint64(ext.Length))
		if _, err := w.Write(header); err != nil {
			return err
		}

		// Write data
		remaining := ext.Length
		offset := ext.Offset
		for remaining > 0 {
			toRead := min(remaining, int64(len(buf)))
			n, err := s.file.ReadAt(buf[:toRead], offset)
			if err != nil && err != io.EOF {
				return err
			}
			if _, err := w.Write(buf[:n]); err != nil {
				return err
			}
			remaining -= int64(n)
			offset += int64(n)
		}
	}

	return nil
}

// Stats returns write statistics.
func (s *SparseOverlay) Stats() (writes, bytesWritten uint64) {
	return s.writes.Load(), s.bytesWrite.Load()
}

func putUint64LE(b []byte, v uint64) {
	b[0] = byte(v)
	b[1] = byte(v >> 8)
	b[2] = byte(v >> 16)
	b[3] = byte(v >> 24)
	b[4] = byte(v >> 32)
	b[5] = byte(v >> 40)
	b[6] = byte(v >> 48)
	b[7] = byte(v >> 56)
}

// SegmentRangeFromBitmap partitions a byte range into contiguous segments
// based on an in-memory dirty bitmap. Each segment is either entirely dirty
// (overlay) or entirely clean (base).
//
// Parameters:
//   - off: starting offset in bytes
//   - length: range length in bytes
//   - blockSize: size of each block in the bitmap
//   - isDirty: function that returns true if a block index is dirty
//
// Example with atomic bitmap:
//
//	isDirty := func(blockIdx int64) bool {
//	    word := atomic.LoadUint64(&bitmap[blockIdx/64])
//	    return word&(1<<(blockIdx%64)) != 0
//	}
//	for seg := range ublk.SegmentRangeFromBitmap(off, length, blockSize, isDirty) {
//	    // process segment
//	}
func SegmentRangeFromBitmap(off, length, blockSize int64, isDirty func(blockIdx int64) bool) iter.Seq[Segment] {
	return func(yield func(Segment) bool) {
		if length <= 0 {
			return
		}

		end := off + length
		pos := off
		bufOff := int64(0)

		for pos < end {
			startBlock := pos / blockSize
			currentDirty := isDirty(startBlock)
			segStart := pos

			// Extend segment while blocks have same dirty status
			for pos < end {
				blockIdx := pos / blockSize
				if isDirty(blockIdx) != currentDirty {
					break
				}
				// Move to next block boundary or end
				nextBoundary := (blockIdx + 1) * blockSize
				pos = min(nextBoundary, end)
			}

			if !yield(Segment{
				Offset:   segStart,
				Length:   pos - segStart,
				BufOff:   bufOff,
				FromBase: !currentDirty,
			}) {
				return
			}
			bufOff += pos - segStart
		}
	}
}

// ReadMixed reads a mixed range by batching contiguous segments from the same source.
// This is a convenience wrapper around SegmentRange that performs the actual reads.
//
// Parameters:
//   - buf: destination buffer
//   - off: starting offset
//   - overlay: sparse overlay for dirty regions
//   - baseReader: reader for clean regions (typically the base image)
//
// Returns the number of bytes read and any error.
func (s *SparseOverlay) ReadMixed(buf []byte, off int64, baseReader io.ReaderAt) (int, error) {
	totalRead := 0

	for seg := range s.SegmentRange(off, int64(len(buf))) {
		segBuf := buf[seg.BufOff : seg.BufOff+seg.Length]
		var n int
		var err error

		if seg.FromBase {
			n, err = baseReader.ReadAt(segBuf, seg.Offset)
		} else {
			n, err = s.file.ReadAt(segBuf, seg.Offset)
		}

		totalRead += n
		if err != nil && err != io.EOF {
			return totalRead, err
		}
	}

	return totalRead, nil
}
