package ublk

import (
	"errors"
	"fmt"
	"unsafe"

	"golang.org/x/sys/unix"
)

// IO result codes (returned in EndIO field).
const (
	IOResultOK      uint16 = 0
	IOResultEIO     uint16 = 5
	IOResultENOTSUP uint16 = 95
)

// ioWorker handles IO operations for a specific queue.
// Each queue has exactly one goroutine driving the io_uring ring.
type ioWorker struct {
	device     *Device
	qid        uint16
	queueDepth uint16
	mmapAddr   []byte // Mapped descriptor area
	ring       *Ring

	// Tag state tracking
	tagSubmitted []bool // tracks which tags have pending fetch requests
	scratchBuf   []byte // For USER_COPY data transfer
}

func newIOWorker(device *Device, qid uint16, queueDepth uint16) *ioWorker {
	// Allocate scratch buffer for USER_COPY (default 512KB)
	bufSize := int(device.info.MaxIOBufBytes)
	if bufSize == 0 {
		bufSize = 512 * 1024
	}

	return &ioWorker{
		device:       device,
		qid:          qid,
		queueDepth:   queueDepth,
		tagSubmitted: make([]bool, queueDepth),
		scratchBuf:   make([]byte, bufSize),
	}
}

// Init initializes the worker's resources and submits initial requests.
// It returns an error if initialization fails.
func (w *ioWorker) Init() error {
	// Initialize io_uring for this queue with optimized flags.
	// Each queue has a single goroutine (single issuer), so we can enable
	// SINGLE_ISSUER and DEFER_TASKRUN for reduced context switches.
	// ublk requires SQE128 for inline commands.
	ring, err := NewRingWithOptions(
		uint(w.queueDepth),
		0,
		WithSingleIssuer(),
		WithDeferTaskrun(),
		WithSQE128(),
	)
	if err != nil {
		// Fallback to basic ring if kernel doesn't support new flags
		// But we MUST have SQE128
		ring, err = NewRingWithOptions(uint(w.queueDepth), 0, WithSQE128())
		if err != nil {
			return fmt.Errorf("queue %d: failed to create io_uring: %w", w.qid, err)
		}
	}
	w.ring = ring

	// Map the IO descriptor area
	if err := w.mmapIODescs(); err != nil {
		w.Close() // Cleanup ring
		return fmt.Errorf("queue %d: failed to mmap IO descs: %w", w.qid, err)
	}

	// Register the char device fd for reduced per-IO overhead
	charFD := int(w.device.charDevFD.Fd())
	if err := w.ring.RegisterFiles([]int{charFD}); err != nil {
		// Not fatal - just means we can't use fixed files
		logf("Queue %d: fixed file registration failed (non-fatal): %v", w.qid, err)
	}

	// Submit initial FETCH_REQ for all tags
	if err := w.submitAllFetchRequests(); err != nil {
		w.Close()
		return fmt.Errorf("queue %d: failed to submit initial fetch requests: %w", w.qid, err)
	}

	return nil
}

// Close releases worker resources.
func (w *ioWorker) Close() {
	if w.mmapAddr != nil {
		w.munmapIODescs()
	}
	if w.ring != nil {
		if err := w.ring.Close(); err != nil {
			logf("Queue %d: ring close error: %v", w.qid, err)
		}
		w.ring = nil
	}
}

// Loop runs the main event loop.
func (w *ioWorker) Loop() {
	defer w.device.wg.Done()
	// Ensure cleanup if Loop exits (though Device.Stop handles graceful shutdown)
	// We rely on Device.Stop to close resources via Stop -> workers=nil

	w.eventLoop()

	// Cleanup after loop exits
	w.Close()
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
		select {
		case <-w.device.stopCh:
			return
		default:
		}

		cqe, err := w.ring.WaitCQE()
		if err != nil {
			select {
			case <-w.device.stopCh:
				return
			default:
				logf("Queue %d: WaitCQE error: %v", w.qid, err)
				return
			}
		}

		tag := uint16(cqe.UserData)
		w.ring.SeenCQE(cqe)

		if cqe.Res < 0 {
			select {
			case <-w.device.stopCh:
				return
			default:
				logf("Queue %d Tag %d: command failed: %d", w.qid, tag, cqe.Res)
				continue
			}
		}

		// Handle Request and get result
		res := w.handleRequest(tag)

		if err := w.submitCommitAndFetch(tag, res); err != nil {
			logf("Queue %d Tag %d: commitAndFetch failed: %v", w.qid, tag, err)
			continue
		}
		pendingSubmit++

		// Batch submissions
		if pendingSubmit >= maxBatch || !w.ring.CQEReady() {
			if _, err := w.ring.Submit(); err != nil {
				logf("Queue %d: submit failed: %v", w.qid, err)
			}
			pendingSubmit = 0
		}
	}
}

func (w *ioWorker) handleRequest(tag uint16) int32 {
	desc := w.getIODesc(tag)

	// Op is lower 8 bits, Flags are upper 24 bits
	op := uint8(desc.OpFlags & 0xff)
	// flags := desc.OpFlags >> 8

	blockSize := w.device.params.Basic.LogicalBSize
	if blockSize == 0 {
		blockSize = 512
	}

	// Use fields from UblksrvIODesc (match ublk_drv logic)
	offset := int64(desc.StartSector) * int64(blockSize)
	length := int(desc.NrSectors) * int(blockSize)

	// Guard against buffer overflow
	if length > len(w.scratchBuf) {
		logf("Queue %d Tag %d: IO length %d > scratch buffer %d", w.qid, tag, length, len(w.scratchBuf))
		return int32(IOResultEIO)
	}

	buf := w.scratchBuf[:length]

	// Execute IO with USER_COPY handling
	return w.executeIO(op, buf, offset, tag)
}

func (w *ioWorker) executeIO(op uint8, buf []byte, offset int64, tag uint16) int32 {
	// Offset for pread/pwrite to char device (USER_COPY)
	// Must match driver's expectation: UBLKSRV_IO_BUF_OFFSET + encoded(qid, tag, offset)
	devOffset := ublkUserCopyPos(w.qid, tag, 0)

	switch op {
	case UBLK_IO_OP_READ:
		// READ: Server reads from backend -> User Buffer
		n, err := w.device.readAt(buf, offset)
		if err != nil || n != len(buf) {
			return int32(IOResultEIO)
		}

		// Then Copy User Buffer -> Driver (pwrite to char device)
		_, err = w.device.charDevFD.WriteAt(buf, devOffset)
		if err != nil {
			logf("Queue %d Tag %d: pwrite to char dev failed: %v", w.qid, tag, err)
			return int32(IOResultEIO)
		}
		return int32(IOResultOK)

	case UBLK_IO_OP_WRITE:
		// WRITE: Copy Driver -> User Buffer (pread from char device)
		_, err := w.device.charDevFD.ReadAt(buf, devOffset)
		if err != nil {
			logf("Queue %d Tag %d: pread from char dev failed: %v", w.qid, tag, err)
			return int32(IOResultEIO)
		}

		// Then Write User Buffer -> Backend
		n, err := w.device.writeAt(buf, offset)
		if err != nil || n != len(buf) {
			return int32(IOResultEIO)
		}
		return int32(IOResultOK)

	case UBLK_IO_OP_FLUSH:
		if flusher, ok := w.device.backend.(Flusher); ok {
			if err := flusher.Flush(); err != nil {
				return int32(IOResultEIO)
			}
		}
		return int32(IOResultOK)

	case UBLK_IO_OP_DISCARD:
		if discarder, ok := w.device.backend.(Discarder); ok {
			if err := discarder.Discard(offset, int64(len(buf))); err != nil {
				return int32(IOResultEIO)
			}
			return int32(IOResultOK)
		}
		return int32(IOResultENOTSUP)

	case UBLK_IO_OP_WRITE_ZEROES:
		if wz, ok := w.device.backend.(WriteZeroer); ok {
			if err := wz.WriteZeroes(offset, int64(len(buf))); err != nil {
				return int32(IOResultEIO)
			}
			return int32(IOResultOK)
		}
		clear(buf)
		n, err := w.device.writeAt(buf, offset)
		if err != nil || n != len(buf) {
			return int32(IOResultEIO)
		}
		return int32(IOResultOK)

	default:
		return int32(IOResultENOTSUP) // Unsupported operation
	}
}

// submitFetchReq prepares a FETCH_REQ SQE for a tag.
func (w *ioWorker) submitFetchReq(tag uint16) error {
	sqe, err := w.ring.GetSQE128()
	if err != nil {
		return fmt.Errorf("failed to get SQE128: %w", err)
	}

	cmd, op := NewFetchReqCommand(w.qid, tag)
	w.prepareSQE(sqe, cmd, op, tag)
	return nil
}

// submitCommitAndFetch prepares a COMMIT_AND_FETCH_REQ SQE for a tag.
func (w *ioWorker) submitCommitAndFetch(tag uint16, result int32) error {
	sqe, err := w.ring.GetSQE128()
	if err != nil {
		return fmt.Errorf("failed to get SQE128: %w", err)
	}

	cmd, op := NewCommitAndFetchReqCommand(w.qid, tag, uint64(result))
	w.prepareSQE(sqe, cmd, op, tag)
	return nil
}

// prepareSQE fills in an SQE with a ublk command.
func (w *ioWorker) prepareSQE(sqe *UringSQE128, cmd *UblkIOCommand, op uint32, tag uint16) {
	cmdData := cmd.ToBytes()

	sqe.Opcode = IORING_OP_URING_CMD
	sqe.Off = uint64(op) // cmd_op is in lower 32 bits of off field

	// For SQE128, command goes into sqe.Cmd (offset 48-128), not Addr.
	// ublk driver expects the command structure in the second half of SQE.
	copy(sqe.Cmd[:], cmdData)

	sqe.Len = uint32(cmd.Size()) // Still set len just in case
	sqe.UserData = uint64(tag)

	// Use fixed file if registered (reduces per-IO overhead)
	if w.ring.HasFixedFiles() {
		sqe.Fd = 0 // Index into registered files array
		sqe.Flags = IOSQE_FIXED_FILE
	} else {
		sqe.Fd = int32(w.device.charDevFD.Fd())
	}
}

func (w *ioWorker) mmapIODescs() error {
	descSize := int(unsafe.Sizeof(UblksrvIODesc{}))
	// We only map the descriptor area. The driver writes to this area, we read from it.
	// For USER_COPY, data is transferred via pread/pwrite, so we don't need to map the IO buffer area.
	totalSize := int(w.queueDepth) * descSize

	mmapAddr, err := unix.Mmap(
		int(w.device.charDevFD.Fd()),
		0,
		totalSize,
		unix.PROT_READ, // Descriptors are read-only for server
		unix.MAP_SHARED|unix.MAP_POPULATE,
	)
	if err != nil {
		return err
	}

	w.mmapAddr = mmapAddr
	return nil
}

func (w *ioWorker) munmapIODescs() {
	if w.mmapAddr != nil {
		if err := unix.Munmap(w.mmapAddr); err != nil {
			logf("Queue %d: munmap error: %v", w.qid, err)
		}
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

// ErrRingNotInitialized is returned when ring operations are attempted before initialization.
var ErrRingNotInitialized = errors.New("io_uring not initialized")
