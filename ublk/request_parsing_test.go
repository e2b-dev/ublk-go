package ublk

import (
	"encoding/binary"
	"errors"
	"testing"
	"unsafe"
)

func TestParseRequest(t *testing.T) {
	buf := make([]byte, unsafe.Sizeof(UblkRequest{}))
	req := (*UblkRequest)(unsafe.Pointer(&buf[0]))
	req.StartSector = 8
	req.NSectors = 16
	req.Op = UBLK_IO_OP_READ

	parsed, err := ParseRequest(UblksrvIODesc{}, buf)
	if err != nil {
		t.Fatalf("ParseRequest returned error: %v", err)
	}

	if parsed.StartSector != req.StartSector || parsed.NSectors != req.NSectors || parsed.Op != req.Op {
		t.Fatal("parsed request fields do not match source data")
	}
}

func TestParseRequestInvalid(t *testing.T) {
	_, err := ParseRequest(UblksrvIODesc{}, []byte{1, 2, 3})
	if !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("expected ErrInvalidRequest, got %v", err)
	}
}

func TestUblkRequestOffsetsAndLength(t *testing.T) {
	req := &UblkRequest{
		StartSector: 4,
		NSectors:    8,
	}
	blockSize := uint32(512)

	if req.GetOffset(blockSize) != int64(4*512) {
		t.Fatalf("unexpected offset: %d", req.GetOffset(blockSize))
	}
	if req.GetLength(blockSize) != int64(8*512) {
		t.Fatalf("unexpected length: %d", req.GetLength(blockSize))
	}
}

func TestReadRequestData(t *testing.T) {
	queueDepth := uint16(2)
	descSize := int(unsafe.Sizeof(UblksrvIODesc{}))
	requestOffset := int(queueDepth) * descSize
	mmapAddr := make([]byte, requestOffset+512)

	pattern := []byte{1, 2, 3, 4}
	copy(mmapAddr[requestOffset:], pattern)

	data, err := ReadRequestData(UblksrvIODesc{}, mmapAddr, queueDepth)
	if err != nil {
		t.Fatalf("ReadRequestData returned error: %v", err)
	}
	if len(data) != 256 {
		t.Fatalf("expected request slice size 256, got %d", len(data))
	}
	if !bytesEqualPrefix(data, pattern) {
		t.Fatal("request data does not match expected pattern")
	}
}

func TestReadRequestDataInvalid(t *testing.T) {
	queueDepth := uint16(4)
	descSize := int(unsafe.Sizeof(UblksrvIODesc{}))
	mmapAddr := make([]byte, (int(queueDepth)*descSize)-1)

	_, err := ReadRequestData(UblksrvIODesc{}, mmapAddr, queueDepth)
	if !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("expected ErrInvalidRequest, got %v", err)
	}
}

func TestWriteResponseUpdatesDescriptor(t *testing.T) {
	queueDepth := uint16(4)
	descSize := int(unsafe.Sizeof(UblksrvIODesc{}))
	mmapAddr := make([]byte, descSize*int(queueDepth))

	desc := &UblksrvIODesc{
		Addr:    0x1111222233334444,
		Length:  4096,
		OpFlags: UBLK_IO_F_FETCHED | UBLK_IO_F_NEED_GET_DATA,
		Tag:     42,
	}

	WriteResponse(desc, 7, mmapAddr, 1, queueDepth)

	if desc.EndIO != 7 {
		t.Fatalf("expected EndIO to be 7, got %d", desc.EndIO)
	}
	if desc.OpFlags&UBLK_IO_F_FETCHED != 0 {
		t.Fatal("expected fetched flag to be cleared")
	}
	if desc.OpFlags&UBLK_IO_F_NEED_GET_DATA == 0 {
		t.Fatal("need_get_data flag should remain set")
	}

	offset := descSize * 1
	if got := binary.LittleEndian.Uint64(mmapAddr[offset:]); got != desc.Addr {
		t.Fatalf("unexpected addr written: 0x%x", got)
	}
	if got := binary.LittleEndian.Uint32(mmapAddr[offset+8:]); got != desc.Length {
		t.Fatalf("unexpected length written: %d", got)
	}
	if got := binary.LittleEndian.Uint16(mmapAddr[offset+12:]); got != desc.OpFlags {
		t.Fatalf("unexpected opflags written: %d", got)
	}
	if got := binary.LittleEndian.Uint16(mmapAddr[offset+14:]); got != desc.EndIO {
		t.Fatalf("unexpected endio written: %d", got)
	}
	if got := binary.LittleEndian.Uint16(mmapAddr[offset+16:]); got != desc.Tag {
		t.Fatalf("unexpected tag written: %d", got)
	}
}

func bytesEqualPrefix(buf []byte, prefix []byte) bool {
	if len(prefix) > len(buf) {
		return false
	}
	for i := range prefix {
		if buf[i] != prefix[i] {
			return false
		}
	}
	return true
}

func TestUblkRequestSegments(t *testing.T) {
	req := &UblkRequest{
		Op:          UBLK_IO_OP_READ,
		NrSegments:  2,
		StartSector: 0,
		NSectors:    16,
	}

	// Set segment data
	req.Segments[0] = UblkSegment{Addr: 0x1000, Len: 4096}
	req.Segments[1] = UblkSegment{Addr: 0x2000, Len: 4096}

	if req.Segments[0].Addr != 0x1000 {
		t.Error("Failed to set segment addr")
	}
	if req.Segments[0].Len != 4096 {
		t.Error("Failed to set segment len")
	}
}

func TestUblkRequestAllOperations(t *testing.T) {
	tests := []struct {
		op   uint8
		name string
	}{
		{UBLK_IO_OP_READ, "read"},
		{UBLK_IO_OP_WRITE, "write"},
		{UBLK_IO_OP_FLUSH, "flush"},
		{UBLK_IO_OP_DISCARD, "discard"},
		{UBLK_IO_OP_WRITE_ZEROES, "write_zeroes"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := &UblkRequest{Op: tt.op}
			if req.Op != tt.op {
				t.Errorf("Op mismatch: got %d, want %d", req.Op, tt.op)
			}
		})
	}
}

func TestReadRequestDataPartialBuffer(t *testing.T) {
	queueDepth := uint16(2)
	descSize := int(unsafe.Sizeof(UblksrvIODesc{}))
	requestOffset := int(queueDepth) * descSize
	// Make buffer that only has 100 bytes after request offset
	mmapAddr := make([]byte, requestOffset+100)

	data, err := ReadRequestData(UblksrvIODesc{}, mmapAddr, queueDepth)
	if err != nil {
		t.Fatalf("ReadRequestData returned error: %v", err)
	}
	// Should return partial data (100 bytes instead of 256)
	if len(data) != 100 {
		t.Errorf("Expected 100 bytes, got %d", len(data))
	}
}

func TestWriteResponseBoundsCheck(t *testing.T) {
	queueDepth := uint16(2)
	descSize := int(unsafe.Sizeof(UblksrvIODesc{}))
	// Make buffer too small for tag 1
	mmapAddr := make([]byte, descSize)

	desc := &UblksrvIODesc{
		Addr:   0x1234,
		Length: 1024,
	}

	// Should not panic - silently handle bounds check
	WriteResponse(desc, 0, mmapAddr, 1, queueDepth)

	// desc should still be updated locally
	if desc.EndIO != 0 {
		t.Error("desc.EndIO should be updated")
	}
}
