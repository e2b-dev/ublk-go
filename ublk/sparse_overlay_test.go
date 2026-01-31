package ublk

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSparseOverlay_ClassifyRange(t *testing.T) {
	s, err := NewSparseOverlay(filepath.Join(t.TempDir(), "overlay"), 1024*1024)
	require.NoError(t, err)
	defer s.Close()

	// Initially all clean
	allDirty, allClean := s.ClassifyRange(0, 4096)
	assert.False(t, allDirty)
	assert.True(t, allClean)

	// Write data
	data := make([]byte, 4096)
	_, err = s.WriteAt(data, 0)
	require.NoError(t, err)

	// Written range is dirty
	allDirty, allClean = s.ClassifyRange(0, 4096)
	assert.True(t, allDirty)
	assert.False(t, allClean)

	// Unwritten range is clean
	allDirty, allClean = s.ClassifyRange(8192, 4096)
	assert.False(t, allDirty)
	assert.True(t, allClean)

	// Spanning range is mixed
	allDirty, allClean = s.ClassifyRange(0, 8192)
	assert.False(t, allDirty)
	assert.False(t, allClean)
}

func TestSparseOverlay_DirtyExtents(t *testing.T) {
	s, err := NewSparseOverlay(filepath.Join(t.TempDir(), "overlay"), 1024*1024)
	require.NoError(t, err)
	defer s.Close()

	data := make([]byte, 4096)
	_, err = s.WriteAt(data, 0)
	require.NoError(t, err)
	_, err = s.WriteAt(data, 100*4096)
	require.NoError(t, err)

	count := 0
	for range s.DirtyExtents() {
		count++
	}
	assert.GreaterOrEqual(t, count, 1)
}

func TestSparseOverlay_ExportDiff(t *testing.T) {
	s, err := NewSparseOverlay(filepath.Join(t.TempDir(), "overlay"), 64*1024)
	require.NoError(t, err)
	defer s.Close()

	_, err = s.WriteAt([]byte("hello world"), 1000)
	require.NoError(t, err)

	var buf bytes.Buffer
	require.NoError(t, s.ExportDiff(&buf))
	assert.NotZero(t, buf.Len())
}

func TestSparseOverlay_FromFile(t *testing.T) {
	f, err := os.Create(filepath.Join(t.TempDir(), "overlay"))
	require.NoError(t, err)
	require.NoError(t, f.Truncate(4096))

	s := NewSparseOverlayFromFile(f, 4096)
	assert.Equal(t, int64(4096), s.Size())
	s.Close()
}

func TestSparseOverlay_SegmentRange(t *testing.T) {
	s, err := NewSparseOverlay(filepath.Join(t.TempDir(), "overlay"), 64*1024)
	require.NoError(t, err)
	defer s.Close()

	// Write blocks 1-2 (skip block 0)
	_, err = s.WriteAt(make([]byte, 8192), 4096)
	require.NoError(t, err)

	segments := make([]Segment, 0, 4)
	for seg := range s.SegmentRange(0, 16384) {
		segments = append(segments, seg)
	}
	assert.GreaterOrEqual(t, len(segments), 2)

	var totalLen int64
	for _, seg := range segments {
		totalLen += seg.Length
	}
	assert.Equal(t, int64(16384), totalLen)
}

func TestSegmentRangeFromBitmap(t *testing.T) {
	blockSize := int64(4096)
	dirtyBlocks := map[int64]bool{2: true, 3: true, 4: true}

	segments := make([]Segment, 0, 3)
	for seg := range SegmentRangeFromBitmap(0, 6*blockSize, blockSize, func(idx int64) bool { return dirtyBlocks[idx] }) {
		segments = append(segments, seg)
	}

	require.Len(t, segments, 3)
	// Segment 0: clean, blocks 0-1
	assert.Equal(t, int64(0), segments[0].Offset)
	assert.Equal(t, 2*blockSize, segments[0].Length)
	assert.True(t, segments[0].FromBase)
	// Segment 1: dirty, blocks 2-4
	assert.Equal(t, 2*blockSize, segments[1].Offset)
	assert.Equal(t, 3*blockSize, segments[1].Length)
	assert.False(t, segments[1].FromBase)
	// Segment 2: clean, block 5
	assert.Equal(t, 5*blockSize, segments[2].Offset)
	assert.Equal(t, blockSize, segments[2].Length)
	assert.True(t, segments[2].FromBase)
}

func TestSparseOverlay_ReadMixed(t *testing.T) {
	s, err := NewSparseOverlay(filepath.Join(t.TempDir(), "overlay"), 16*1024)
	require.NoError(t, err)
	defer s.Close()

	_, err = s.WriteAt([]byte("OVERLAY!"), 4096)
	require.NoError(t, err)

	baseData := bytes.Repeat([]byte("BASE"), 4096)
	buf := make([]byte, 8192)
	n, err := s.ReadMixed(buf, 0, bytes.NewReader(baseData))
	require.NoError(t, err)
	assert.Equal(t, 8192, n)
	assert.Equal(t, "OVERLAY!", string(buf[4096:4096+8]))
}

func TestSparseOverlay_ReadMixedVectored(t *testing.T) {
	dir := t.TempDir()

	// Create base file
	baseFile, err := os.Create(filepath.Join(dir, "base"))
	require.NoError(t, err)
	defer baseFile.Close()
	_, err = baseFile.Write(bytes.Repeat([]byte("BASE"), 4096))
	require.NoError(t, err)

	// Create overlay
	s, err := NewSparseOverlay(filepath.Join(dir, "overlay"), 16*1024)
	require.NoError(t, err)
	defer s.Close()

	_, err = s.WriteAt([]byte("DIRTY!!!"), 4096)
	require.NoError(t, err)

	buf := make([]byte, 8192)
	n, err := s.ReadMixedVectored(buf, 0, int(baseFile.Fd()))
	require.NoError(t, err)
	assert.Equal(t, 8192, n)
	assert.Equal(t, "BASE", string(buf[0:4]))
	assert.Equal(t, "DIRTY!!!", string(buf[4096:4096+8]))
}

func TestSparseOverlay_IsZeroRegion(t *testing.T) {
	s, err := NewSparseOverlay(filepath.Join(t.TempDir(), "overlay"), 64*1024)
	require.NoError(t, err)
	defer s.Close()

	assert.True(t, s.IsZeroRegion(0, 4096), "unwritten region should be zero")
	assert.True(t, s.IsZeroRegion(0, 64*1024), "entire file should be zero")

	_, err = s.WriteAt([]byte("data"), 4096)
	require.NoError(t, err)

	assert.False(t, s.IsZeroRegion(4096, 4), "written region should not be zero")
	assert.True(t, s.IsZeroRegion(0, 4096), "before write should still be zero")
	assert.True(t, s.IsZeroRegion(8192, 4096), "after write should still be zero")
	assert.False(t, s.IsZeroRegion(0, 8192), "mixed region should not be zero")
}

func TestPreadvContiguousGroups(t *testing.T) {
	f, err := os.Create(filepath.Join(t.TempDir(), "testfile"))
	require.NoError(t, err)
	defer f.Close()

	_, err = f.WriteString("AAAABBBBCCCCDDDD")
	require.NoError(t, err)

	buf1, buf2 := make([]byte, 4), make([]byte, 4)
	segs := []vectoredSegment{{fileOffset: 0, bufSlice: buf1}, {fileOffset: 8, bufSlice: buf2}}

	n, err := preadvContiguousGroups(int(f.Fd()), segs)
	require.NoError(t, err)
	assert.Equal(t, 8, n)
	assert.Equal(t, "AAAA", string(buf1))
	assert.Equal(t, "CCCC", string(buf2))
}
