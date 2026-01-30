package ublk

import (
	"errors"
	"fmt"
	"unsafe"

	"golang.org/x/sys/unix"
)

// ioWorker handles IO operations for a specific queue
type ioWorker struct {
	device     *Device
	qid        uint16
	queueDepth uint16
	ioDescs    []UblksrvIODesc
	mmapAddr   []byte
	ring       *Ring
	bufferMgr  *BufferManager
}

func newIOWorker(device *Device, qid uint16, queueDepth uint16) *ioWorker {
	return &ioWorker{
		device:     device,
		qid:        qid,
		queueDepth: queueDepth,
		ioDescs:    make([]UblksrvIODesc, queueDepth),
	}
}

func (w *ioWorker) run() {
	defer w.device.wg.Done()

	// Initialize io_uring for this queue
	ring, err := NewRing(uint(w.queueDepth), 0)
	if err != nil {
		logf("Queue %d: failed to create io_uring: %v", w.qid, err)
		return
	}
	w.ring = ring
	defer ring.Close()

	// Map the IO descriptor area
	err = w.mmapIODescs()
	if err != nil {
		logf("Queue %d: failed to mmap IO descs: %v", w.qid, err)
		return
	}
	defer w.munmapIODescs()

	// Initialize buffer manager
	w.bufferMgr = NewBufferManager(w.mmapAddr, w.queueDepth)

	// Handle each tag in the queue
	for tag := uint16(0); tag < w.queueDepth; tag++ {
		w.device.wg.Add(1)
		go w.handleTag(tag)
	}

	// Wait for stop signal
	<-w.device.stopCh
}

func (w *ioWorker) handleTag(tag uint16) {
	defer w.device.wg.Done()

	// 1. Initial Fetch
	// This submits the FETCH_REQ and waits for the kernel to send an IO request.
	// The call blocks until an IO request is ready or error occurs.
	err := w.fetchReq(tag)
	if err != nil {
		// If we can't fetch the first request, we can't proceed.
		// This might happen if the device is stopping.
		return
	}

	// 2. Processing Loop
	for {
		// Check if we should stop
		select {
		case <-w.device.stopCh:
			return
		default:
		}

		// At this point, we have a request ready (from fetchReq or previous commitAndFetch)
		// Get the descriptor from the mmap'd area
		desc := w.getIODesc(tag)

		// Handle the IO operation (Read/Write)
		// This reads/writes data to the buffer and updates the desc result
		w.handleIO(tag, desc)

		// 3. Commit Result and Fetch Next
		// This submits COMMIT_AND_FETCH_REQ which:
		// - Returns the result of the current IO to kernel
		// - Queues a new FETCH request for the next IO
		// - Blocks until the NEXT IO request is ready
		err := w.commitAndFetch(tag, desc)
		if err != nil {
			// Error implies device is stopped or ring is broken
			return
		}
	}
}

func (w *ioWorker) handleIO(tag uint16, desc UblksrvIODesc) {
	// Read the request data from the mmap'd area
	requestData, err := w.bufferMgr.GetRequestData(tag)
	if err != nil {
		logf("Queue %d Tag %d: failed to read request data: %v", w.qid, tag, err)
		desc.EndIO = 1 // Error
		w.setIODesc(tag, desc)
		return
	}

	// Parse the request
	req, err := ParseRequest(desc, requestData)
	if err != nil {
		logf("Queue %d Tag %d: failed to parse request: %v", w.qid, tag, err)
		desc.EndIO = 1 // Error
		w.setIODesc(tag, desc)
		return
	}

	// Calculate offset and length
	blockSize := uint32(w.device.params.Basic.LogicalBSize)
	offset := req.GetOffset(blockSize)
	length := req.GetLength(blockSize)

	// Get the buffer from the buffer manager
	buf, err := w.bufferMgr.GetIODescBuffer(desc)
	if err != nil {
		logf("Queue %d Tag %d: failed to get buffer: %v", w.qid, tag, err)
		desc.EndIO = 1 // Error
		w.setIODesc(tag, desc)
		return
	}

	// Ensure buffer is large enough
	if len(buf) < int(length) {
		logf("Queue %d Tag %d: buffer too small: got %d, need %d", w.qid, tag, len(buf), length)
		desc.EndIO = 1 // Error
		w.setIODesc(tag, desc)
		return
	}

	var n int
	var ioErr error

	// Handle the operation
	switch req.Op {
	case UBLK_IO_OP_READ:
		n, ioErr = w.device.readAt(buf[:length], offset)
		if ioErr == nil && n == int(length) {
			desc.EndIO = 0 // Success
		} else {
			desc.EndIO = 1 // Error
		}
	case UBLK_IO_OP_WRITE:
		// For write, data should already be in the buffer
		n, ioErr = w.device.writeAt(buf[:length], offset)
		if ioErr == nil && n == int(length) {
			desc.EndIO = 0 // Success
		} else {
			desc.EndIO = 1 // Error
		}
	case UBLK_IO_OP_FLUSH:
		// Flush is typically a no-op for most backends
		desc.EndIO = 0 // Success
	case UBLK_IO_OP_DISCARD, UBLK_IO_OP_WRITE_ZEROES:
		// These operations may not be supported by all backends
		desc.EndIO = 1 // Unsupported for now
	default:
		desc.EndIO = 1 // Unsupported operation
	}

	// Clear the fetched flag
	desc.OpFlags &^= UBLK_IO_F_FETCHED

	// Update descriptor
	w.setIODesc(tag, desc)
}

func (w *ioWorker) fetchReq(tag uint16) error {
	if w.ring == nil {
		return errors.New("io_uring not initialized")
	}

	sqe, err := w.ring.GetSQE()
	if err != nil {
		return fmt.Errorf("failed to get SQE: %w", err)
	}

	cmd := NewFetchReqCommand(w.device.devID, w.qid, tag)
	w.submitCommand(sqe, cmd, tag)

	return w.waitForCompletion()
}

func (w *ioWorker) commitAndFetch(tag uint16, desc UblksrvIODesc) error {
	if w.ring == nil {
		return errors.New("io_uring not initialized")
	}

	sqe, err := w.ring.GetSQE()
	if err != nil {
		return fmt.Errorf("failed to get SQE: %w", err)
	}

	cmd := NewCommitAndFetchReqCommand(w.device.devID, w.qid, tag, uint64(desc.EndIO))
	w.submitCommand(sqe, cmd, tag)

	return w.waitForCompletion()
}

func (w *ioWorker) submitCommand(sqe *UringSQE, cmd *UblkIOCommand, tag uint16) {
	cmdData := cmd.ToBytes()

	sqe.Opcode = IORING_OP_URING_CMD
	sqe.Fd = int32(w.device.charDevFD.Fd())
	sqe.Addr = uint64(uintptr(unsafe.Pointer(&cmdData[0])))
	sqe.Len = uint32(cmd.Size())
	sqe.UserData = uint64(tag)

	w.ring.Submit()
}

func (w *ioWorker) waitForCompletion() error {
	cqe, err := w.ring.WaitCQE()
	if err != nil {
		return err
	}
	defer w.ring.SeenCQE(cqe)

	if cqe.Res < 0 {
		return fmt.Errorf("command failed: %d", cqe.Res)
	}
	return nil
}

func (w *ioWorker) mmapIODescs() error {
	// Calculate the total mmap size needed:
	// Layout: [IO Descriptors] [Request Data] [Buffers]
	descSize := int(unsafe.Sizeof(UblksrvIODesc{}))
	descAreaSize := int(w.queueDepth) * descSize
	requestAreaSize := 256 * int(w.queueDepth) // 256 bytes per request structure

	// Buffer area size comes from device info
	maxIOBufBytes := int(w.device.info.MaxIOBufBytes)
	if maxIOBufBytes == 0 {
		maxIOBufBytes = 512 * 1024 // Default 512KB if not set
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
	offset := int(tag) * int(unsafe.Sizeof(UblksrvIODesc{}))
	if offset+int(unsafe.Sizeof(UblksrvIODesc{})) > len(w.mmapAddr) {
		return UblksrvIODesc{}
	}
	return *(*UblksrvIODesc)(unsafe.Pointer(&w.mmapAddr[offset]))
}

func (w *ioWorker) setIODesc(tag uint16, desc UblksrvIODesc) {
	if w.mmapAddr == nil {
		return
	}
	offset := int(tag) * int(unsafe.Sizeof(UblksrvIODesc{}))
	if offset+int(unsafe.Sizeof(UblksrvIODesc{})) > len(w.mmapAddr) {
		return
	}
	*(*UblksrvIODesc)(unsafe.Pointer(&w.mmapAddr[offset])) = desc
}
