package ublk

import (
	"encoding/binary"
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
func ParseRequest(desc UblksrvIODesc, data []byte) (*UblkRequest, error) {
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

// ReadRequestData reads the request data structure.
// The actual location depends on ublk implementation.
func ReadRequestData(desc UblksrvIODesc, mmapAddr []byte, queueDepth uint16) ([]byte, error) {
	descSize := int(unsafe.Sizeof(UblksrvIODesc{}))
	requestAreaOffset := int(queueDepth) * descSize

	if requestAreaOffset >= len(mmapAddr) {
		return nil, ErrInvalidRequest
	}

	requestSize := requestDataSize
	if requestAreaOffset+requestSize > len(mmapAddr) {
		requestSize = len(mmapAddr) - requestAreaOffset
	}

	return mmapAddr[requestAreaOffset : requestAreaOffset+requestSize], nil
}

// WriteResponse writes the response back to the mmap'd descriptor area.
func WriteResponse(desc *UblksrvIODesc, result int32, mmapAddr []byte, tag uint16, queueDepth uint16) {
	desc.EndIO = uint16(result)
	desc.OpFlags &^= UBLK_IO_F_FETCHED

	descSize := int(unsafe.Sizeof(UblksrvIODesc{}))
	offset := int(tag) * descSize

	if offset+descSize <= len(mmapAddr) {
		binary.LittleEndian.PutUint64(mmapAddr[offset:], desc.Addr)
		binary.LittleEndian.PutUint32(mmapAddr[offset+8:], desc.Length)
		binary.LittleEndian.PutUint16(mmapAddr[offset+12:], desc.OpFlags)
		binary.LittleEndian.PutUint16(mmapAddr[offset+14:], desc.EndIO)
		binary.LittleEndian.PutUint16(mmapAddr[offset+16:], desc.Tag)
	}
}
