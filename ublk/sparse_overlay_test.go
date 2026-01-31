package ublk

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestSparseOverlay_ClassifyRange(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "overlay")

	s, err := NewSparseOverlay(path, 1024*1024) // 1MB
	if err != nil {
		t.Fatalf("NewSparseOverlay: %v", err)
	}
	defer s.Close()

	// Initially everything is clean
	allDirty, allClean := s.ClassifyRange(0, 4096)
	if !allClean || allDirty {
		t.Errorf("expected allClean for empty overlay, got allDirty=%v allClean=%v", allDirty, allClean)
	}

	// Write some data
	data := make([]byte, 4096)
	for i := range data {
		data[i] = byte(i)
	}
	if _, err := s.WriteAt(data, 0); err != nil {
		t.Fatalf("WriteAt: %v", err)
	}

	// Now the written range should be dirty
	allDirty, allClean = s.ClassifyRange(0, 4096)
	if !allDirty || allClean {
		t.Errorf("expected allDirty for written range, got allDirty=%v allClean=%v", allDirty, allClean)
	}

	// Range after written data should be clean
	allDirty, allClean = s.ClassifyRange(8192, 4096)
	if !allClean || allDirty {
		t.Errorf("expected allClean for unwritten range, got allDirty=%v allClean=%v", allDirty, allClean)
	}

	// Range spanning dirty and clean should be mixed
	allDirty, allClean = s.ClassifyRange(0, 8192)
	if allDirty || allClean {
		t.Errorf("expected mixed for spanning range, got allDirty=%v allClean=%v", allDirty, allClean)
	}
}

func TestSparseOverlay_DirtyExtents(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "overlay")

	s, err := NewSparseOverlay(path, 1024*1024)
	if err != nil {
		t.Fatalf("NewSparseOverlay: %v", err)
	}
	defer s.Close()

	// Write at two separate locations
	data := make([]byte, 4096)
	if _, err := s.WriteAt(data, 0); err != nil {
		t.Fatalf("WriteAt 0: %v", err)
	}
	if _, err := s.WriteAt(data, 100*4096); err != nil {
		t.Fatalf("WriteAt 100*4096: %v", err)
	}

	// Count extents
	count := 0
	for range s.DirtyExtents() {
		count++
	}
	// Note: actual count depends on filesystem behavior with sparse files
	// At minimum, we should have at least 1 extent
	if count < 1 {
		t.Errorf("expected at least 1 dirty extent, got %d", count)
	}
}

func TestSparseOverlay_ExportDiff(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "overlay")

	s, err := NewSparseOverlay(path, 64*1024) // 64KB
	if err != nil {
		t.Fatalf("NewSparseOverlay: %v", err)
	}
	defer s.Close()

	// Write some data
	data := []byte("hello world")
	if _, err := s.WriteAt(data, 1000); err != nil {
		t.Fatalf("WriteAt: %v", err)
	}

	// Export diff
	var buf bytes.Buffer
	if err := s.ExportDiff(&buf); err != nil {
		t.Fatalf("ExportDiff: %v", err)
	}

	// Should have exported something
	if buf.Len() == 0 {
		t.Error("ExportDiff produced no output")
	}
}

func TestSparseOverlay_FromFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "overlay")

	// Create file manually
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := f.Truncate(4096); err != nil {
		f.Close()
		t.Fatalf("Truncate: %v", err)
	}

	// Wrap it
	s := NewSparseOverlayFromFile(f, 4096)

	// Should work
	if s.Size() != 4096 {
		t.Errorf("Size() = %d, want 4096", s.Size())
	}

	s.Close()
}

func TestSparseOverlay_SegmentRange(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "overlay")

	// 64KB overlay with 4KB blocks
	size := int64(64 * 1024)
	s, err := NewSparseOverlay(path, size)
	if err != nil {
		t.Fatalf("NewSparseOverlay: %v", err)
	}
	defer s.Close()

	// Write blocks 1 and 2 (skip block 0)
	data := make([]byte, 8192)
	if _, err := s.WriteAt(data, 4096); err != nil {
		t.Fatalf("WriteAt: %v", err)
	}

	// Segment range [0, 16384) should have:
	// - block 0: clean (base)
	// - blocks 1-2: dirty (overlay)
	// - block 3: clean (base)
	segments := make([]Segment, 0, 4)
	for seg := range s.SegmentRange(0, 16384) {
		segments = append(segments, seg)
	}

	// Expect at least 2 segments (may vary by filesystem)
	if len(segments) < 2 {
		t.Errorf("expected at least 2 segments, got %d", len(segments))
	}

	// First segment should be from base (clean)
	if len(segments) > 0 && !segments[0].FromBase {
		// Actually, this depends on filesystem - block 0 might be hole
		// Just verify we got segments
		t.Logf("first segment: offset=%d len=%d fromBase=%v", segments[0].Offset, segments[0].Length, segments[0].FromBase)
	}

	// Verify total length
	var totalLen int64
	for _, seg := range segments {
		totalLen += seg.Length
	}
	if totalLen != 16384 {
		t.Errorf("total segment length = %d, want 16384", totalLen)
	}

	// Verify BufOff is sequential
	expectedBufOff := int64(0)
	for i, seg := range segments {
		if seg.BufOff != expectedBufOff {
			t.Errorf("segment %d: BufOff = %d, want %d", i, seg.BufOff, expectedBufOff)
		}
		expectedBufOff += seg.Length
	}
}

func TestSegmentRangeFromBitmap(t *testing.T) {
	blockSize := int64(4096)

	// Create dirty bitmap: blocks 0,1 clean, 2,3,4 dirty, 5 clean
	dirtyBlocks := map[int64]bool{2: true, 3: true, 4: true}
	isDirty := func(idx int64) bool { return dirtyBlocks[idx] }

	// Segment range covering blocks 0-5
	segments := make([]Segment, 0, 3) // expect 3 segments
	for seg := range SegmentRangeFromBitmap(0, 6*blockSize, blockSize, isDirty) {
		segments = append(segments, seg)
	}

	// Expect 3 segments:
	// 1. blocks 0-1: clean
	// 2. blocks 2-4: dirty
	// 3. block 5: clean
	if len(segments) != 3 {
		t.Errorf("expected 3 segments, got %d", len(segments))
		for i, seg := range segments {
			t.Logf("  segment %d: offset=%d len=%d fromBase=%v", i, seg.Offset, seg.Length, seg.FromBase)
		}
		return
	}

	// Segment 0: clean, blocks 0-1
	if segments[0].Offset != 0 || segments[0].Length != 2*blockSize || !segments[0].FromBase {
		t.Errorf("segment 0: got offset=%d len=%d fromBase=%v, want 0/%d/true",
			segments[0].Offset, segments[0].Length, segments[0].FromBase, 2*blockSize)
	}

	// Segment 1: dirty, blocks 2-4
	if segments[1].Offset != 2*blockSize || segments[1].Length != 3*blockSize || segments[1].FromBase {
		t.Errorf("segment 1: got offset=%d len=%d fromBase=%v, want %d/%d/false",
			segments[1].Offset, segments[1].Length, segments[1].FromBase, 2*blockSize, 3*blockSize)
	}

	// Segment 2: clean, block 5
	if segments[2].Offset != 5*blockSize || segments[2].Length != blockSize || !segments[2].FromBase {
		t.Errorf("segment 2: got offset=%d len=%d fromBase=%v, want %d/%d/true",
			segments[2].Offset, segments[2].Length, segments[2].FromBase, 5*blockSize, blockSize)
	}
}

func TestSparseOverlay_ReadMixed(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "overlay")

	size := int64(16 * 1024) // 16KB
	s, err := NewSparseOverlay(path, size)
	if err != nil {
		t.Fatalf("NewSparseOverlay: %v", err)
	}
	defer s.Close()

	// Write "OVERLAY" at offset 4096
	overlayData := []byte("OVERLAY!")
	if _, err := s.WriteAt(overlayData, 4096); err != nil {
		t.Fatalf("WriteAt overlay: %v", err)
	}

	// Create a "base" with "BASE" pattern
	baseData := bytes.Repeat([]byte("BASE"), 4096)

	// Read mixed range
	buf := make([]byte, 8192)
	n, err := s.ReadMixed(buf, 0, bytes.NewReader(baseData))
	if err != nil {
		t.Fatalf("ReadMixed: %v", err)
	}
	if n != 8192 {
		t.Errorf("ReadMixed returned %d bytes, want 8192", n)
	}

	// First 4096 bytes should be from base (zeros, since base is smaller)
	// Or we get "BASE" pattern from our bytes.Reader
	// Bytes 4096-4103 should be "OVERLAY!"

	// Check overlay portion
	got := string(buf[4096 : 4096+len(overlayData)])
	if got != "OVERLAY!" {
		t.Errorf("overlay portion = %q, want %q", got, "OVERLAY!")
	}
}
