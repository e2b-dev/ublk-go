package ublk

import (
	"errors"
	"unsafe"
)

// ErrInvalidRequest is returned when request data is malformed or too small.
var ErrInvalidRequest = errors.New("invalid request")

// UblkRequest represents a ublk IO request from the kernel.
// This structure is embedded in the io descriptor.
type UblkRequest struct {
	// Request header
	Op          uint8
	Flags       uint8
	Ioprio      uint16
	NSectors    uint32
	StartSector uint64

	// Data segments (bvec)
	NrSegments uint16
	Segments   [16]UblkSegment // Max 16 segments
}

// UblkSegment represents a single segment in a request.
type UblkSegment struct {
	Addr uint64
	Len  uint32
	Pad  uint32
}

// ParseRequest parses a ublk request from the IO descriptor data.
func ParseRequest(_ UblksrvIODesc, data []byte) (*UblkRequest, error) {
	if len(data) < int(unsafe.Sizeof(UblkRequest{})) {
		return nil, ErrInvalidRequest
	}
	req := (*UblkRequest)(unsafe.Pointer(&data[0]))
	return req, nil
}

// GetOffset calculates the byte offset from the request.
func (r *UblkRequest) GetOffset(blockSize uint32) int64 {
	return int64(r.StartSector) * int64(blockSize)
}

// GetLength calculates the total length in bytes.
func (r *UblkRequest) GetLength(blockSize uint32) int64 {
	return int64(r.NSectors) * int64(blockSize)
}
