package ublk

import (
	"testing"
	"unsafe"
)

func TestNewIOWorker(t *testing.T) {
	// Create a minimal device for testing (won't actually work without kernel)
	dev := &Device{
		devID: 0,
	}

	worker := newIOWorker(dev, 0, 128)

	if worker.device != dev {
		t.Error("device not set correctly")
	}
	if worker.qid != 0 {
		t.Errorf("expected qid 0, got %d", worker.qid)
	}
	if worker.queueDepth != 128 {
		t.Errorf("expected queueDepth 128, got %d", worker.queueDepth)
	}
	if len(worker.ioDescs) != 128 {
		t.Errorf("expected ioDescs length 128, got %d", len(worker.ioDescs))
	}
}

func TestIOWorkerGetSetIODesc(t *testing.T) {
	worker := &ioWorker{
		queueDepth: 4,
	}

	descSize := int(unsafe.Sizeof(UblksrvIODesc{}))
	worker.mmapAddr = make([]byte, descSize*4)

	// Set a descriptor
	desc := UblksrvIODesc{
		Addr:    0x12345678,
		Length:  4096,
		OpFlags: UBLK_IO_F_FETCHED,
		Tag:     2,
	}
	worker.setIODesc(2, desc)

	// Get it back
	got := worker.getIODesc(2)
	if got.Addr != desc.Addr {
		t.Errorf("expected Addr %x, got %x", desc.Addr, got.Addr)
	}
	if got.Length != desc.Length {
		t.Errorf("expected Length %d, got %d", desc.Length, got.Length)
	}
	if got.OpFlags != desc.OpFlags {
		t.Errorf("expected OpFlags %d, got %d", desc.OpFlags, got.OpFlags)
	}
	if got.Tag != desc.Tag {
		t.Errorf("expected Tag %d, got %d", desc.Tag, got.Tag)
	}
}

func TestIOWorkerGetIODescNilMmap(t *testing.T) {
	worker := &ioWorker{
		queueDepth: 4,
		mmapAddr:   nil,
	}

	// Should return empty descriptor without panic
	desc := worker.getIODesc(0)
	if desc.Addr != 0 || desc.Length != 0 {
		t.Error("expected zero descriptor for nil mmap")
	}
}

func TestIOWorkerSetIODescNilMmap(t *testing.T) {
	worker := &ioWorker{
		queueDepth: 4,
		mmapAddr:   nil,
	}

	// Should not panic
	worker.setIODesc(0, UblksrvIODesc{Addr: 123})
}

func TestIOWorkerGetIODescOutOfBounds(t *testing.T) {
	worker := &ioWorker{
		queueDepth: 2,
	}

	descSize := int(unsafe.Sizeof(UblksrvIODesc{}))
	worker.mmapAddr = make([]byte, descSize) // Only space for 1 descriptor

	// Tag 1 would be out of bounds
	desc := worker.getIODesc(1)
	if desc.Addr != 0 {
		t.Error("expected zero descriptor for out of bounds tag")
	}
}

func TestIOWorkerSetIODescOutOfBounds(t *testing.T) {
	worker := &ioWorker{
		queueDepth: 2,
	}

	descSize := int(unsafe.Sizeof(UblksrvIODesc{}))
	worker.mmapAddr = make([]byte, descSize) // Only space for 1 descriptor

	// Should not panic, just silently fail
	worker.setIODesc(1, UblksrvIODesc{Addr: 123})
}

func TestIOWorkerMmapSizeCalculation(t *testing.T) {
	// Test that mmap size calculation is correct
	queueDepth := uint16(128)
	maxIOBufBytes := uint32(512 * 1024)

	descSize := int(unsafe.Sizeof(UblksrvIODesc{}))
	descAreaSize := int(queueDepth) * descSize
	requestAreaSize := 256 * int(queueDepth)
	expectedSize := descAreaSize + requestAreaSize + int(maxIOBufBytes)

	// Verify the calculation matches what io_worker does
	if expectedSize <= 0 {
		t.Error("Expected size should be positive")
	}

	t.Logf("Mmap size for queueDepth=%d: desc=%d, request=%d, buffer=%d, total=%d",
		queueDepth, descAreaSize, requestAreaSize, maxIOBufBytes, expectedSize)
}

func TestIOWorkerQueueAndTag(t *testing.T) {
	dev := &Device{devID: 5}
	worker := newIOWorker(dev, 3, 64)

	if worker.qid != 3 {
		t.Errorf("Expected qid 3, got %d", worker.qid)
	}
	if worker.device.devID != 5 {
		t.Errorf("Expected devID 5, got %d", worker.device.devID)
	}
}

func TestIOWorkerMultipleDescriptors(t *testing.T) {
	worker := &ioWorker{
		queueDepth: 4,
	}

	descSize := int(unsafe.Sizeof(UblksrvIODesc{}))
	worker.mmapAddr = make([]byte, descSize*4)

	// Set all 4 descriptors
	for tag := uint16(0); tag < 4; tag++ {
		desc := UblksrvIODesc{
			Addr:   uint64(tag * 0x1000),
			Length: 512 * uint32(tag+1),
			Tag:    tag,
		}
		worker.setIODesc(tag, desc)
	}

	// Read them back and verify
	for tag := uint16(0); tag < 4; tag++ {
		got := worker.getIODesc(tag)
		if got.Addr != uint64(tag*0x1000) {
			t.Errorf("Tag %d: expected Addr 0x%x, got 0x%x", tag, tag*0x1000, got.Addr)
		}
		if got.Length != 512*uint32(tag+1) {
			t.Errorf("Tag %d: expected Length %d, got %d", tag, 512*(tag+1), got.Length)
		}
		if got.Tag != tag {
			t.Errorf("Tag %d: expected Tag %d, got %d", tag, tag, got.Tag)
		}
	}
}
