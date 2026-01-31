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
