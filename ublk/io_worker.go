package ublk

import (
	"errors"
	"fmt"
	"unsafe"

	"golang.org/x/sys/unix"
)

// IO result codes (returned in EndIO field).
// These map to negated Linux errno values where applicable.
const (
	IOResultOK      uint16 = 0  // Success
	IOResultEIO     uint16 = 5  // I/O error (EIO)
	IOResultENOTSUP uint16 = 95 // Operation not supported (EOPNOTSUPP)
)

// ioWorker handles IO operations for a specific queue.
// Each queue has exactly one goroutine driving the io_uring ring.
type ioWorker struct {
	device     *Device
	qid        uint16
	queueDepth uint16
	mmapAddr   []byte
	ring       *Ring
	bufferMgr  *BufferManager

	// Tag state tracking
	tagSubmitted []bool // tracks which tags have pending fetch requests
}

func newIOWorker(device *Device, qid uint16, queueDepth uint16) *ioWorker {
	return &ioWorker{
		device:       device,
		qid:          qid,
		queueDepth:   queueDepth,
		tagSubmitted: make([]bool, queueDepth),
	}
}

func (w *ioWorker) run() {
	defer w.device.wg.Done()

	// Initialize io_uring for this queue with optimized flags.
	// Each queue has a single goroutine (single issuer), so we can enable
	// SINGLE_ISSUER and DEFER_TASKRUN for reduced context switches.
	ring, err := NewRingWithOptions(
		uint(w.queueDepth),
		0,
		WithSingleIssuer(),
		WithDeferTaskrun(),
	)
	if err != nil {
		// Fallback to basic ring if kernel doesn't support new flags
		ring, err = NewRing(uint(w.queueDepth), 0)
		if err != nil {
			logf("Queue %d: failed to create io_uring: %v", w.qid, err)
			return
		}
	}
	w.ring = ring
	defer ring.Close()

	// Map the IO descriptor area
	if err := w.mmapIODescs(); err != nil {
		logf("Queue %d: failed to mmap IO descs: %v", w.qid, err)
		return
	}
	defer w.munmapIODescs()

	// Initialize buffer manager
	w.bufferMgr = NewBufferManager(w.mmapAddr, w.queueDepth)

	// Submit initial FETCH_REQ for all tags
	if err := w.submitAllFetchRequests(); err != nil {
		logf("Queue %d: failed to submit initial fetch requests: %v", w.qid, err)
		return
	}

	// Main event loop - single goroutine drives the ring
	w.eventLoop()
}

// submitAllFetchRequests submits FETCH_REQ for all tags in the queue.
func (w *ioWorker) submitAllFetchRequests() error {
	for tag := range w.queueDepth {
		if err := w.submitFetchReq(tag); err != nil {
			return fmt.Errorf("tag %d: %w", tag, err)
		}
		w.tagSubmitted[tag] = true
	}

	// Submit all at once
	if _, err := w.ring.Submit(); err != nil {
		return fmt.Errorf("submit failed: %w", err)
	}

	return nil
}

// eventLoop processes completions and handles IO requests.
// This is the main loop that runs until the device is stopped.
func (w *ioWorker) eventLoop() {
	// Batch processing: handle multiple CQEs before submitting
	const maxBatch = 16
	pendingSubmit := 0

	for {
		// Check for stop signal (non-blocking)
		select {
		case <-w.device.stopCh:
			return
		default:
		}

		// Wait for a completion
		cqe, err := w.ring.WaitCQE()
		if err != nil {
			// Check if we were interrupted by stop
			select {
			case <-w.device.stopCh:
				return
			default:
				logf("Queue %d: WaitCQE error: %v", w.qid, err)
				return
			}
		}

		// Extract tag from user data
		tag := uint16(cqe.UserData)

		// Mark CQE as seen before processing
		w.ring.SeenCQE(cqe)

		// Check result
		if cqe.Res < 0 {
			// Negative result typically means device is stopping
			select {
			case <-w.device.stopCh:
				return
			default:
				logf("Queue %d Tag %d: command failed: %d", w.qid, tag, cqe.Res)
				continue
			}
		}

		// We received a request - process it
		w.handleRequest(tag)

		// Submit COMMIT_AND_FETCH to complete this request and fetch next
		desc := w.getIODesc(tag)
		if err := w.submitCommitAndFetch(tag, desc); err != nil {
			logf("Queue %d Tag %d: commitAndFetch failed: %v", w.qid, tag, err)
			continue
		}
		pendingSubmit++

		// Batch submissions: submit when we have enough pending or no more CQEs ready
		if pendingSubmit >= maxBatch || !w.ring.CQEReady() {
			if _, err := w.ring.Submit(); err != nil {
				logf("Queue %d: submit failed: %v", w.qid, err)
			}
			pendingSubmit = 0
		}
	}
}

// handleRequest processes an IO request for a specific tag.
func (w *ioWorker) handleRequest(tag uint16) {
	desc := w.getIODesc(tag)

	// Get request data
	requestData, err := w.bufferMgr.GetRequestData(tag)
	if err != nil {
		logf("Queue %d Tag %d: failed to get request data: %v", w.qid, tag, err)
		desc.EndIO = IOResultEIO
		w.setIODesc(tag, desc)
		return
	}

	// Parse the request
	req, err := ParseRequest(desc, requestData)
	if err != nil {
		logf("Queue %d Tag %d: failed to parse request: %v", w.qid, tag, err)
		desc.EndIO = IOResultEIO
		w.setIODesc(tag, desc)
		return
	}

	// Calculate offset and length
	blockSize := w.device.params.Basic.LogicalBSize
	if blockSize == 0 {
		blockSize = 512 // fallback
	}
	offset := req.GetOffset(blockSize)
	length := req.GetLength(blockSize)

	// Get the buffer
	buf, err := w.bufferMgr.GetIODescBuffer(desc)
	if err != nil {
		logf("Queue %d Tag %d: failed to get buffer: %v", w.qid, tag, err)
		desc.EndIO = IOResultEIO
		w.setIODesc(tag, desc)
		return
	}

	// Validate buffer size
	if len(buf) < int(length) {
		logf("Queue %d Tag %d: buffer too small: got %d, need %d", w.qid, tag, len(buf), length)
		desc.EndIO = IOResultEIO
		w.setIODesc(tag, desc)
		return
	}

	// Handle the operation
	desc.EndIO = w.executeIO(req.Op, buf[:length], offset)
	desc.OpFlags &^= UBLK_IO_F_FETCHED

	// Record statistics
	w.device.stats.recordOp(req.Op, uint64(length), desc.EndIO == IOResultOK)

	w.setIODesc(tag, desc)
}

// executeIO performs the actual IO operation and returns the result code.
func (w *ioWorker) executeIO(op uint8, buf []byte, offset int64) uint16 {
	switch op {
	case UBLK_IO_OP_READ:
		n, err := w.device.readAt(buf, offset)
		if err != nil || n != len(buf) {
			return IOResultEIO
		}
		return IOResultOK

	case UBLK_IO_OP_WRITE:
		n, err := w.device.writeAt(buf, offset)
		if err != nil || n != len(buf) {
			return IOResultEIO
		}
		return IOResultOK

	case UBLK_IO_OP_FLUSH:
		// Check if backend supports Flush
		if flusher, ok := w.device.backend.(Flusher); ok {
			if err := flusher.Flush(); err != nil {
				return IOResultEIO
			}
		}
		return IOResultOK // Success (or no-op if not supported)

	case UBLK_IO_OP_DISCARD:
		// Check if backend supports Discard
		if discarder, ok := w.device.backend.(Discarder); ok {
			if err := discarder.Discard(offset, int64(len(buf))); err != nil {
				return IOResultEIO
			}
			return IOResultOK
		}
		return IOResultENOTSUP // Not supported

	case UBLK_IO_OP_WRITE_ZEROES:
		// Check if backend supports WriteZeroes
		if wz, ok := w.device.backend.(WriteZeroer); ok {
			if err := wz.WriteZeroes(offset, int64(len(buf))); err != nil {
				return IOResultEIO
			}
			return IOResultOK
		}
		// Fallback: write actual zeroes using clear() for efficiency
		clear(buf)
		n, err := w.device.writeAt(buf, offset)
		if err != nil || n != len(buf) {
			return IOResultEIO
		}
		return IOResultOK

	default:
		return IOResultENOTSUP // Unsupported operation
	}
}

// submitFetchReq prepares a FETCH_REQ SQE for a tag.
func (w *ioWorker) submitFetchReq(tag uint16) error {
	sqe, err := w.ring.GetSQE()
	if err != nil {
		return fmt.Errorf("failed to get SQE: %w", err)
	}

	cmd := NewFetchReqCommand(w.device.devID, w.qid, tag)
	w.prepareSQE(sqe, cmd, tag)
	return nil
}

// submitCommitAndFetch prepares a COMMIT_AND_FETCH_REQ SQE for a tag.
func (w *ioWorker) submitCommitAndFetch(tag uint16, desc UblksrvIODesc) error {
	sqe, err := w.ring.GetSQE()
	if err != nil {
		return fmt.Errorf("failed to get SQE: %w", err)
	}

	cmd := NewCommitAndFetchReqCommand(w.device.devID, w.qid, tag, uint64(desc.EndIO))
	w.prepareSQE(sqe, cmd, tag)
	return nil
}

// prepareSQE fills in an SQE with a ublk command.
func (w *ioWorker) prepareSQE(sqe *UringSQE, cmd *UblkIOCommand, tag uint16) {
	cmdData := cmd.ToBytes()

	sqe.Opcode = IORING_OP_URING_CMD
	sqe.Fd = int32(w.device.charDevFD.Fd())
	sqe.Addr = uint64(uintptr(unsafe.Pointer(&cmdData[0])))
	sqe.Len = uint32(cmd.Size())
	sqe.UserData = uint64(tag)
}

func (w *ioWorker) mmapIODescs() error {
	descSize := int(unsafe.Sizeof(UblksrvIODesc{}))
	descAreaSize := int(w.queueDepth) * descSize
	requestAreaSize := requestDataSize * int(w.queueDepth)

	maxIOBufBytes := int(w.device.info.MaxIOBufBytes)
	if maxIOBufBytes == 0 {
		maxIOBufBytes = 512 * 1024
	}

	totalSize := descAreaSize + requestAreaSize + maxIOBufBytes

	mmapAddr, err := unix.Mmap(
		int(w.device.charDevFD.Fd()),
		0,
		totalSize,
		unix.PROT_READ|unix.PROT_WRITE,
		unix.MAP_SHARED,
	)
	if err != nil {
		return err
	}

	w.mmapAddr = mmapAddr
	return nil
}

func (w *ioWorker) munmapIODescs() {
	if w.mmapAddr != nil {
		unix.Munmap(w.mmapAddr)
		w.mmapAddr = nil
	}
}

func (w *ioWorker) getIODesc(tag uint16) UblksrvIODesc {
	if w.mmapAddr == nil {
		return UblksrvIODesc{}
	}
	descSize := int(unsafe.Sizeof(UblksrvIODesc{}))
	offset := int(tag) * descSize
	if offset+descSize > len(w.mmapAddr) {
		return UblksrvIODesc{}
	}
	return *(*UblksrvIODesc)(unsafe.Pointer(&w.mmapAddr[offset]))
}

func (w *ioWorker) setIODesc(tag uint16, desc UblksrvIODesc) {
	if w.mmapAddr == nil {
		return
	}
	descSize := int(unsafe.Sizeof(UblksrvIODesc{}))
	offset := int(tag) * descSize
	if offset+descSize > len(w.mmapAddr) {
		return
	}
	*(*UblksrvIODesc)(unsafe.Pointer(&w.mmapAddr[offset])) = desc
}

// ErrRingNotInitialized is returned when ring operations are attempted before initialization.
var ErrRingNotInitialized = errors.New("io_uring not initialized")
