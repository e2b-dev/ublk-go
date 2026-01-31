package ublk

import (
	"fmt"
	"io"
	"iter"
	"os"

	"golang.org/x/sys/unix"
)

// SparseOverlay uses SEEK_HOLE/SEEK_DATA for efficient dirty extent tracking.
type SparseOverlay struct {
	file *os.File
	size int64
}

type Extent struct {
	Offset int64
	Length int64
}

// Segment represents a contiguous region with uniform dirty/clean status.
type Segment struct {
	Offset   int64
	Length   int64
	BufOff   int64
	FromBase bool // true = read from base (clean)
}

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

func NewSparseOverlayFromFile(file *os.File, size int64) *SparseOverlay {
	return &SparseOverlay{
		file: file,
		size: size,
	}
}

func (s *SparseOverlay) File() *os.File                           { return s.file }
func (s *SparseOverlay) Size() int64                              { return s.size }
func (s *SparseOverlay) ReadAt(p []byte, off int64) (int, error)  { return s.file.ReadAt(p, off) }
func (s *SparseOverlay) WriteAt(p []byte, off int64) (int, error) { return s.file.WriteAt(p, off) }

func (s *SparseOverlay) Sync() error  { return s.file.Sync() }
func (s *SparseOverlay) Close() error { return s.file.Close() }

// ClassifyRange returns (allDirty, allClean) for the byte range.
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

func (s *SparseOverlay) IsClean(off, length int64) bool {
	_, allClean := s.ClassifyRange(off, length)
	return allClean
}

// IsZeroRegion implements SparseReader. Holes read as zeros.
func (s *SparseOverlay) IsZeroRegion(off, length int64) bool {
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

func (s *SparseOverlay) DirtyBytes() int64 {
	var total int64
	for ext := range s.DirtyExtents() {
		total += ext.Length
	}
	return total
}

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

// iovMax is the maximum number of iovec entries per preadv/pwritev call.
// Linux default is 1024 (UIO_MAXIOV). We use a conservative value.
const iovMax = 1024

// vectoredSegment holds segment info for vectored I/O grouping.
type vectoredSegment struct {
	fileOffset int64 // offset in source file
	bufSlice   []byte
}

// ReadMixedVectored reads a mixed range using preadv for efficiency.
// It groups non-contiguous segments by source and issues one preadv per source,
// reducing syscall overhead when there are many small scattered segments.
//
// This is more efficient than ReadMixed when:
//   - Many small non-contiguous segments from the same source
//   - File-backed storage (not memfd where syscall overhead is minimal)
//   - Fragmented access patterns
//
// Parameters:
//   - buf: destination buffer
//   - off: starting offset
//   - baseFd: file descriptor for base image
//
// Returns the number of bytes read and any error.
func (s *SparseOverlay) ReadMixedVectored(buf []byte, off int64, baseFd int) (int, error) {
	// Collect segments grouped by source
	var baseSegs, overlaySegs []vectoredSegment

	for seg := range s.SegmentRange(off, int64(len(buf))) {
		vs := vectoredSegment{
			fileOffset: seg.Offset,
			bufSlice:   buf[seg.BufOff : seg.BufOff+seg.Length],
		}
		if seg.FromBase {
			baseSegs = append(baseSegs, vs)
		} else {
			overlaySegs = append(overlaySegs, vs)
		}
	}

	totalRead := 0

	// Read from base using preadv
	n, err := preadvSegments(baseFd, baseSegs)
	totalRead += n
	if err != nil {
		return totalRead, err
	}

	// Read from overlay using preadv
	n, err = preadvSegments(int(s.file.Fd()), overlaySegs)
	totalRead += n
	if err != nil {
		return totalRead, err
	}

	return totalRead, nil
}

// preadvSegments reads multiple non-contiguous segments using preadv.
// Handles IOV_MAX limit by splitting into multiple calls if needed.
func preadvSegments(fd int, segs []vectoredSegment) (int, error) {
	if len(segs) == 0 {
		return 0, nil
	}

	totalRead := 0

	// Process segments in chunks of iovMax
	for i := 0; i < len(segs); i += iovMax {
		end := min(i+iovMax, len(segs))
		chunk := segs[i:end]

		// For preadv, all iovecs share the same file offset, so we can only
		// batch segments that are contiguous in the file. For non-contiguous
		// segments, we need separate preadv calls.
		//
		// Group contiguous segments for each preadv call.
		n, err := preadvContiguousGroups(fd, chunk)
		totalRead += n
		if err != nil {
			return totalRead, err
		}
	}

	return totalRead, nil
}

// preadvContiguousGroups issues preadv calls for groups of contiguous segments.
// Since preadv reads sequentially from a single offset, we group segments that
// are contiguous in the file.
func preadvContiguousGroups(fd int, segs []vectoredSegment) (int, error) {
	if len(segs) == 0 {
		return 0, nil
	}

	totalRead := 0
	groupStart := 0

	for i := 1; i <= len(segs); i++ {
		// Check if this segment is contiguous with previous
		isContiguous := false
		if i < len(segs) {
			prevEnd := segs[i-1].fileOffset + int64(len(segs[i-1].bufSlice))
			isContiguous = segs[i].fileOffset == prevEnd
		}

		if !isContiguous {
			// Flush current group
			group := segs[groupStart:i]
			iovecs := make([][]byte, len(group))
			for j, seg := range group {
				iovecs[j] = seg.bufSlice
			}

			n, err := unix.Preadv(fd, iovecs, group[0].fileOffset)
			totalRead += n
			if err != nil {
				return totalRead, err
			}

			groupStart = i
		}
	}

	return totalRead, nil
}
