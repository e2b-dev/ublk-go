package ublk

import (
	"errors"
	"unsafe"
)

// BufferManager handles mapping and access to IO buffers and request data.
// It abstracts the layout of the shared memory area.
type BufferManager struct {
	mmapAddr   []byte
	queueDepth uint16
	descSize   int
}

// NewBufferManager creates a new buffer manager for a queue.
func NewBufferManager(mmapAddr []byte, queueDepth uint16) *BufferManager {
	return &BufferManager{
		mmapAddr:   mmapAddr,
		queueDepth: queueDepth,
		descSize:   int(unsafe.Sizeof(UblksrvIODesc{})),
	}
}

// GetIODescBuffer returns the data buffer associated with an IO descriptor.
// The addr field in the descriptor is an offset into the buffer area.
func (bm *BufferManager) GetIODescBuffer(desc UblksrvIODesc) ([]byte, error) {
	if desc.Addr == 0 || desc.Length == 0 {
		return nil, errors.New("invalid buffer address or length")
	}

	// Layout: [Descriptors] [Request Data] [Buffers]
	// Descriptors area size
	descAreaSize := int(bm.queueDepth) * bm.descSize

	// Request data area size (approx 256 bytes per request)
	// This should match kernel driver's UBLK_MAX_IO_REQUEST_SIZE?
	// Or is it derived? Assuming a safe constant for now.
	requestAreaSize := 256 * int(bm.queueDepth)

	bufferAreaOffset := descAreaSize + requestAreaSize

	// Calculate actual offset
	bufferOffset := bufferAreaOffset + int(desc.Addr)

	if bufferOffset+int(desc.Length) > len(bm.mmapAddr) {
		return nil, errors.New("buffer offset out of bounds")
	}

	return bm.mmapAddr[bufferOffset : bufferOffset+int(desc.Length)], nil
}

// GetRequestData returns the request structure data for a specific tag.
func (bm *BufferManager) GetRequestData(tag uint16) ([]byte, error) {
	if tag >= bm.queueDepth {
		return nil, errors.New("tag exceeds queue depth")
	}

	descAreaSize := int(bm.queueDepth) * bm.descSize
	// Assuming 256 bytes per request structure
	requestOffset := descAreaSize + int(tag)*256

	if requestOffset+256 > len(bm.mmapAddr) {
		return nil, errors.New("request data offset out of bounds")
	}

	return bm.mmapAddr[requestOffset : requestOffset+256], nil
}
