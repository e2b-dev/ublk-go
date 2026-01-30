package ublk

import (
	"bytes"
	"testing"
	"unsafe"
)

func allocateTestMmap(queueDepth uint16, bufferSize int) ([]byte, int) {
	descSize := int(unsafe.Sizeof(UblksrvIODesc{}))
	descArea := int(queueDepth) * descSize
	requestArea := 256 * int(queueDepth)
	bufferOffset := descArea + requestArea
	total := bufferOffset + bufferSize
	return make([]byte, total), bufferOffset
}

func TestBufferManagerGetIODescBuffer(t *testing.T) {
	queueDepth := uint16(4)
	mmapAddr, bufferOffset := allocateTestMmap(queueDepth, 512)
	payload := []byte("sample-buffer-data")

	copy(mmapAddr[bufferOffset+100:], payload)

	bm := NewBufferManager(mmapAddr, queueDepth)
	desc := UblksrvIODesc{
		Addr:   100,
		Length: uint32(len(payload)),
	}

	buf, err := bm.GetIODescBuffer(desc)
	if err != nil {
		t.Fatalf("GetIODescBuffer returned error: %v", err)
	}
	if !bytes.Equal(buf, payload) {
		t.Fatalf("Expected payload %q, got %q", payload, buf)
	}
}

func TestBufferManagerGetIODescBufferInvalid(t *testing.T) {
	queueDepth := uint16(2)
	mmapAddr, _ := allocateTestMmap(queueDepth, 64)
	bm := NewBufferManager(mmapAddr, queueDepth)

	_, err := bm.GetIODescBuffer(UblksrvIODesc{})
	if err == nil {
		t.Fatal("expected error for zero addr/length")
	}

	desc := UblksrvIODesc{
		Addr:   1024,
		Length: 32,
	}
	_, err = bm.GetIODescBuffer(desc)
	if err == nil {
		t.Fatal("expected error for buffer past mmap")
	}
}

func TestBufferManagerGetRequestData(t *testing.T) {
	queueDepth := uint16(4)
	mmapAddr, bufferOffset := allocateTestMmap(queueDepth, 512)
	_ = bufferOffset

	descSize := int(unsafe.Sizeof(UblksrvIODesc{}))
	requestOffset := descSize*int(queueDepth) + 2*256
	expected := bytes.Repeat([]byte{0xAB}, 256)
	copy(mmapAddr[requestOffset:requestOffset+256], expected)

	bm := NewBufferManager(mmapAddr, queueDepth)
	data, err := bm.GetRequestData(2)
	if err != nil {
		t.Fatalf("GetRequestData returned error: %v", err)
	}
	if !bytes.Equal(data, expected) {
		t.Fatal("request data contents mismatch")
	}
}

func TestBufferManagerGetRequestDataInvalidTag(t *testing.T) {
	queueDepth := uint16(2)
	mmapAddr, _ := allocateTestMmap(queueDepth, 128)
	bm := NewBufferManager(mmapAddr, queueDepth)

	if _, err := bm.GetRequestData(queueDepth); err == nil {
		t.Fatal("expected error for tag >= queue depth")
	}
}

func TestBufferManagerDescSize(t *testing.T) {
	queueDepth := uint16(4)
	mmapAddr, _ := allocateTestMmap(queueDepth, 256)
	bm := NewBufferManager(mmapAddr, queueDepth)

	// Verify descSize is set correctly
	if bm.descSize == 0 {
		t.Error("descSize should not be zero")
	}
}

func TestBufferManagerGetRequestDataSmallMmap(t *testing.T) {
	queueDepth := uint16(4)
	descSize := int(unsafe.Sizeof(UblksrvIODesc{}))
	// Make mmap too small to hold request data
	mmapAddr := make([]byte, descSize*int(queueDepth)+100) // Not enough for 256 bytes per request

	bm := NewBufferManager(mmapAddr, queueDepth)

	// Should fail for tag that would exceed bounds
	_, err := bm.GetRequestData(3)
	if err == nil {
		t.Fatal("expected error for request data out of bounds")
	}
}
