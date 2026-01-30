package ublk

import (
	"errors"
	"testing"
	"unsafe"
)

func TestParseRequest(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
	_, err := ParseRequest(UblksrvIODesc{}, []byte{1, 2, 3})
	if !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("expected ErrInvalidRequest, got %v", err)
	}
}

func TestUblkRequestOffsetsAndLength(t *testing.T) {
	t.Parallel()
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

func TestUblkRequestSegments(t *testing.T) {
	t.Parallel()
	req := &UblkRequest{
		Op:          UBLK_IO_OP_READ,
		NrSegments:  2,
		StartSector: 0,
		NSectors:    16,
	}

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
	t.Parallel()
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
			t.Parallel()
			req := &UblkRequest{Op: tt.op}
			if req.Op != tt.op {
				t.Errorf("Op mismatch: got %d, want %d", req.Op, tt.op)
			}
		})
	}
}

func TestUblkRequestLargeValues(t *testing.T) {
	t.Parallel()
	req := &UblkRequest{
		StartSector: 0xFFFFFFFFFFFFFFFF,
		NSectors:    0xFFFFFFFF,
	}
	blockSize := uint32(4096)

	offset := req.GetOffset(blockSize)
	length := req.GetLength(blockSize)

	if offset == 0 || length == 0 {
		t.Error("Large values should produce non-zero results")
	}
}

func TestUblkSegmentStructure(t *testing.T) {
	t.Parallel()
	seg := UblkSegment{
		Addr: 0x123456789ABCDEF0,
		Len:  0xDEADBEEF,
		Pad:  0,
	}

	if seg.Addr != 0x123456789ABCDEF0 {
		t.Error("Segment Addr mismatch")
	}
	if seg.Len != 0xDEADBEEF {
		t.Error("Segment Len mismatch")
	}
}

func TestParseRequestEmptyData(t *testing.T) {
	t.Parallel()
	_, err := ParseRequest(UblksrvIODesc{}, []byte{})
	if !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("expected ErrInvalidRequest for empty data, got %v", err)
	}
}

func TestParseRequestNilData(t *testing.T) {
	t.Parallel()
	_, err := ParseRequest(UblksrvIODesc{}, nil)
	if !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("expected ErrInvalidRequest for nil data, got %v", err)
	}
}
