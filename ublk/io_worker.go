package ublk

import (
	"errors"
	"fmt"
	"math"
	"unsafe"

	"golang.org/x/sys/unix"
)

const (
	IOResultOK      int32 = 0
	IOResultEIO     int32 = 5
	IOResultENOTSUP int32 = 95
)

const (
	userDataDataFlag uint64 = 1 << 63
)

func dataUserData(tag uint16) uint64 {
	return userDataDataFlag | uint64(tag)
}

func isDataUserData(userData uint64) bool {
	return userData&userDataDataFlag != 0
}

func dataUserTag(userData uint64) uint16 {
	return uint16(userData)
}

type ioWorker struct {
	device       *Device
	qid          uint16
	queueDepth   uint16
	mmapAddr     []byte
	ring         *Ring
	zcRing       *Ring
	userCopy     bool
	ioBufBytes   int
	zeroCopy     bool
	autoBufReg   bool
	zcBackend    int
	cowBackend   COWBackend
	cowOverlayFD int
	sparseReader SparseReader
	tagSubmitted []bool
	dataPending  []bool
	dataOp       []uint8
	dataFlags    []uint32
	dataLen      []uint64
	tagBuffers   [][]byte
	bufPool      []byte
	scratchBuf   []byte
}

func newIOWorker(device *Device, qid uint16, queueDepth uint16) *ioWorker {
	autoBufReg := device.flags&UBLK_F_AUTO_BUF_REG != 0
	var dataPending []bool
	var dataOp []uint8
	var dataFlags []uint32
	var dataLen []uint64
	if autoBufReg {
		dataPending = make([]bool, queueDepth)
		dataOp = make([]uint8, queueDepth)
		dataFlags = make([]uint32, queueDepth)
		dataLen = make([]uint64, queueDepth)
	}
	return &ioWorker{
		device:       device,
		qid:          qid,
		queueDepth:   queueDepth,
		userCopy:     device.HasUserCopy(),
		ioBufBytes:   int(device.info.MaxIOBufBytes),
		zeroCopy:     device.flags&UBLK_F_SUPPORT_ZERO_COPY != 0,
		autoBufReg:   autoBufReg,
		tagSubmitted: make([]bool, queueDepth),
		dataPending:  dataPending,
		dataOp:       dataOp,
		dataFlags:    dataFlags,
		dataLen:      dataLen,
	}
}

func (w *ioWorker) Init() error {
	ring, err := NewRingWithOptions(
		uint(w.queueDepth),
		0,
		WithSingleIssuer(),
		WithDeferTaskrun(),
		WithSQE128(),
	)
	if err != nil {
		ring, err = NewRingWithOptions(uint(w.queueDepth), 0, WithSQE128())
		if err != nil {
			return fmt.Errorf("queue %d: failed to create io_uring: %w", w.qid, err)
		}
	}
	w.ring = ring

	if err := w.mmapIODescs(); err != nil {
		w.Close() // Cleanup ring
		return fmt.Errorf("queue %d: failed to mmap IO descs: %w", w.qid, err)
	}

	if err := w.initZeroCopy(); err != nil {
		w.Close()
		return fmt.Errorf("queue %d: failed to init zero-copy: %w", w.qid, err)
	}

	if err := w.initBuffers(); err != nil {
		w.Close()
		return fmt.Errorf("queue %d: failed to init IO buffers: %w", w.qid, err)
	}

	charFD := int(w.device.charDevFD.Fd())
	_ = w.ring.RegisterFiles([]int{charFD}) // non-fatal if fails

	if err := w.initCOWBackend(); err != nil {
		w.Close()
		return fmt.Errorf("queue %d: failed to init COW backend: %w", w.qid, err)
	}

	if sr, ok := w.device.backend.(SparseReader); ok {
		w.sparseReader = sr
	}

	if err := w.submitAllFetchRequests(); err != nil {
		w.Close()
		return fmt.Errorf("queue %d: failed to submit initial fetch requests: %w", w.qid, err)
	}

	return nil
}

func (w *ioWorker) Close() {
	if w.mmapAddr != nil {
		w.munmapIODescs()
	}
	if w.zcRing != nil {
		_ = w.zcRing.Close()
		w.zcRing = nil
	}
	if w.ring != nil {
		_ = w.ring.Close()
		w.ring = nil
	}
}

func (w *ioWorker) initBuffers() error {
	bufSize := w.ioBufBytes
	if bufSize <= 0 {
		bufSize = 512 * 1024
	}
	w.ioBufBytes = bufSize

	if w.userCopy {
		w.scratchBuf = make([]byte, bufSize)
		return nil
	}
	if w.zeroCopy {
		w.scratchBuf = make([]byte, bufSize)
		return nil
	}

	if w.queueDepth == 0 {
		return errors.New("queue depth is zero")
	}
	total := int(w.queueDepth) * bufSize
	if int(w.queueDepth) != 0 && total/int(w.queueDepth) != bufSize {
		return fmt.Errorf("buffer size overflow: depth=%d size=%d", w.queueDepth, bufSize)
	}

	w.bufPool = w.makeAlignedPool(total)
	if w.bufPool == nil {
		return errors.New("failed to allocate buffer pool")
	}
	w.tagBuffers = make([][]byte, w.queueDepth)
	for tag := range int(w.queueDepth) {
		start := tag * bufSize
		w.tagBuffers[tag] = w.bufPool[start : start+bufSize]
	}
	return nil
}

func (w *ioWorker) initZeroCopy() error {
	if !w.zeroCopy {
		return nil
	}
	if w.device.backend == nil {
		return errors.New("zero-copy requires backend")
	}
	fb, ok := w.device.backend.(FixedFileBackend)
	if !ok {
		return errors.New("zero-copy requires FixedFileBackend")
	}
	f, err := fb.FixedFile()
	if err != nil {
		return fmt.Errorf("failed to get fixed file: %w", err)
	}
	if f == nil {
		return errors.New("fixed file is nil")
	}
	w.zcBackend = int(f.Fd())

	if w.autoBufReg {
		if err := w.ring.RegisterSparseBuffers(uint32(w.queueDepth)); err != nil {
			return fmt.Errorf("failed to register sparse buffers: %w", err)
		}
		return nil
	}

	entries := max(uint(w.queueDepth), 2)
	zcRing, err := NewRingWithOptions(entries, 0, WithSingleIssuer(), WithDeferTaskrun(), WithSQE128())
	if err != nil {
		zcRing, err = NewRingWithOptions(entries, 0, WithSQE128())
		if err != nil {
			return fmt.Errorf("failed to create zero-copy ring: %w", err)
		}
	}
	if err := zcRing.RegisterSparseBuffers(uint32(w.queueDepth)); err != nil {
		_ = zcRing.Close()
		return fmt.Errorf("failed to register sparse buffers: %w", err)
	}
	w.zcRing = zcRing
	return nil
}

// initCOWBackend initializes COW backend support.
func (w *ioWorker) initCOWBackend() error {
	if !w.device.cow {
		return nil
	}
	if w.device.backend == nil {
		return errors.New("COW requires backend")
	}

	cow, ok := w.device.backend.(COWBackend)
	if !ok {
		return errors.New("COW requires COWBackend interface")
	}

	overlay, err := cow.Overlay()
	if err != nil {
		return fmt.Errorf("failed to get overlay: %w", err)
	}
	if overlay == nil {
		return errors.New("overlay is nil")
	}

	w.cowBackend = cow
	w.cowOverlayFD = int(overlay.Fd())

	// Create a secondary ring for zero-copy overlay I/O
	entries := max(uint(w.queueDepth), 2)
	zcRing, err := NewRingWithOptions(entries, 0, WithSingleIssuer(), WithDeferTaskrun(), WithSQE128())
	if err != nil {
		zcRing, err = NewRingWithOptions(entries, 0, WithSQE128())
		if err != nil {
			return fmt.Errorf("failed to create COW ring: %w", err)
		}
	}

	// Register the overlay file for fixed file operations
	if err := zcRing.RegisterFiles([]int{w.cowOverlayFD}); err != nil {
		_ = zcRing.Close()
		return fmt.Errorf("failed to register overlay file: %w", err)
	}

	// Register sparse buffers for zero-copy
	if err := zcRing.RegisterSparseBuffers(uint32(w.queueDepth)); err != nil {
		_ = zcRing.Close()
		return fmt.Errorf("failed to register sparse buffers: %w", err)
	}

	w.zcRing = zcRing
	return nil
}

func (w *ioWorker) makeAlignedPool(size int) []byte {
	if size <= 0 {
		return nil
	}
	const align = 2 * 1024 * 1024
	if size > math.MaxInt-(align-1) {
		return nil
	}
	raw := make([]byte, size+align-1)
	addr := uintptr(unsafe.Pointer(&raw[0]))
	offset := int((uintptr(align) - (addr % uintptr(align))) % uintptr(align))
	return raw[offset : offset+size]
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
			return
		}

		userData := cqe.UserData
		w.ring.SeenCQE(cqe)

		if isDataUserData(userData) {
			tag := dataUserTag(userData)
			res := w.finishAutoDataIO(tag, cqe.Res)
			if err := w.submitCommitAndFetch(tag, res); err != nil {
				continue
			}
			pendingSubmit++
			if pendingSubmit >= maxBatch || !w.ring.CQEReady() {
				_, _ = w.ring.Submit()
				pendingSubmit = 0
			}
			continue
		}

		tag := uint16(userData)
		if cqe.Res < 0 {
			select {
			case <-w.device.stopCh:
				return
			default:
				continue
			}
		}

		res, queued := w.handleRequest(tag)
		if queued {
			pendingSubmit++
			if pendingSubmit >= maxBatch || !w.ring.CQEReady() {
				_, _ = w.ring.Submit()
				pendingSubmit = 0
			}
			continue
		}

		if err := w.submitCommitAndFetch(tag, res); err != nil {
			continue
		}
		pendingSubmit++

		if pendingSubmit >= maxBatch || !w.ring.CQEReady() {
			_, _ = w.ring.Submit()
			pendingSubmit = 0
		}
	}
}

func (w *ioWorker) handleRequest(tag uint16) (int32, bool) {
	desc := w.getIODesc(tag)

	op := uint8(desc.OpFlags & 0xff)

	blockSize := w.device.blockSize()
	if blockSize <= 0 {
		return IOResultEIO, false
	}

	if desc.StartSector > uint64(math.MaxInt64)/blockSize {
		return IOResultEIO, false
	}

	offset := int64(desc.StartSector) * int64(blockSize)
	length64 := uint64(desc.NrSectors) * blockSize

	if length64 > uint64(^uint(0)) {
		return IOResultEIO, false
	}

	length := int(length64)

	if w.zeroCopy && (op == UBLK_IO_OP_READ || op == UBLK_IO_OP_WRITE) {
		if w.autoBufReg {
			if length == 0 {
				return IOResultOK, false
			}
			if err := w.submitAutoZeroCopyIO(op, desc.OpFlags, offset, length, tag); err != nil {
				return IOResultEIO, false
			}
			return 0, true
		}
		return w.executeZeroCopy(op, desc.OpFlags, offset, length, tag), false
	}

	// COW backend: route based on dirty state
	if w.cowBackend != nil && (op == UBLK_IO_OP_READ || op == UBLK_IO_OP_WRITE) {
		return w.executeCOW(op, desc.OpFlags, offset, length, tag), false
	}

	var buf []byte
	if length > 0 {
		if w.userCopy || w.zeroCopy {
			if length > len(w.scratchBuf) {
				return IOResultEIO, false
			}
			buf = w.scratchBuf[:length]
		} else {
			tagIdx := int(tag)
			if tagIdx >= len(w.tagBuffers) {
				return IOResultEIO, false
			}
			tagBuf := w.tagBuffers[tagIdx]
			if length > len(tagBuf) {
				return IOResultEIO, false
			}
			buf = tagBuf[:length]
		}
	}

	return w.executeIO(op, desc.OpFlags, buf, offset, tag), false
}

func (w *ioWorker) executeZeroCopy(op uint8, flags uint32, offset int64, length int, tag uint16) int32 {
	if w.zcRing == nil || w.zcBackend <= 0 {
		return IOResultEIO
	}
	if length < 0 {
		return IOResultEIO
	}

	bufIndex := tag
	if err := w.zcRegisterBuf(bufIndex, tag); err != nil {
		return IOResultEIO
	}

	res := w.zcFixedIO(op, offset, length, bufIndex, flags)

	if err := w.zcUnregisterBuf(bufIndex, tag); err != nil {
		return IOResultEIO
	}

	return res
}

// executeCOW handles COW requests with hybrid zero-copy/user-copy routing.
// - Writes: zero-copy to overlay
// - Dirty reads: zero-copy from overlay
// - Clean reads: user-copy from base via ReadCleanAt
// - Mixed reads: user-copy (could be optimized to split).
func (w *ioWorker) executeCOW(op uint8, flags uint32, offset int64, length int, tag uint16) int32 {
	if w.cowBackend == nil || w.zcRing == nil {
		return IOResultEIO
	}

	// Writes always go to overlay (zero-copy)
	if op == UBLK_IO_OP_WRITE {
		return w.cowWriteOverlay(flags, offset, length, tag)
	}

	// Read: check dirty state
	allDirty, allClean := w.cowBackend.ClassifyRange(offset, int64(length))

	if allDirty {
		// All dirty: zero-copy from overlay
		return w.cowReadOverlay(flags, offset, length, tag)
	}

	if allClean {
		// All clean: user-copy from base
		return w.cowReadBase(flags, offset, length, tag)
	}

	// Mixed: user-copy for now (could optimize with split later)
	// This reads from both sources and assembles in buffer
	return w.cowReadMixed(flags, offset, length, tag)
}

func (w *ioWorker) cowWriteOverlay(_ uint32, offset int64, length int, tag uint16) int32 {
	bufIndex := tag
	if err := w.zcRegisterBuf(bufIndex, tag); err != nil {
		return IOResultEIO
	}
	res := w.cowFixedIO(UBLK_IO_OP_WRITE, offset, length, bufIndex)
	if err := w.zcUnregisterBuf(bufIndex, tag); err != nil {
		return IOResultEIO
	}
	return res
}

func (w *ioWorker) cowReadOverlay(_ uint32, offset int64, length int, tag uint16) int32 {
	bufIndex := tag
	if err := w.zcRegisterBuf(bufIndex, tag); err != nil {
		return IOResultEIO
	}
	res := w.cowFixedIO(UBLK_IO_OP_READ, offset, length, bufIndex)
	if err := w.zcUnregisterBuf(bufIndex, tag); err != nil {
		return IOResultEIO
	}
	return res
}

func (w *ioWorker) cowReadBase(_ uint32, offset int64, length int, tag uint16) int32 {
	if length > len(w.scratchBuf) {
		return IOResultEIO
	}
	buf := w.scratchBuf[:length]
	n, err := w.cowBackend.ReadBaseAt(buf, offset)
	if err != nil {
		return IOResultEIO
	}
	for i := n; i < length; i++ {
		buf[i] = 0
	}
	bufOffset := int64(tag) * int64(w.ioBufBytes)
	if _, err = w.device.charDevFD.WriteAt(buf, bufOffset); err != nil {
		return IOResultEIO
	}
	return IOResultOK
}

func (w *ioWorker) cowReadMixed(_ uint32, offset int64, length int, tag uint16) int32 {
	if length > len(w.scratchBuf) {
		return IOResultEIO
	}
	buf := w.scratchBuf[:length]
	n, err := w.cowBackend.ReadAt(buf, offset)
	if err != nil {
		return IOResultEIO
	}
	for i := n; i < length; i++ {
		buf[i] = 0
	}

	bufOffset := int64(tag) * int64(w.ioBufBytes)
	if _, err = w.device.charDevFD.WriteAt(buf, bufOffset); err != nil {
		return IOResultEIO
	}
	return IOResultOK
}

// cowFixedIO performs fixed I/O to overlay via io_uring.
func (w *ioWorker) cowFixedIO(op uint8, offset int64, length int, bufIndex uint16) int32 {
	sqe, err := w.zcRing.GetSQE()
	if err != nil {
		return IOResultEIO
	}

	switch op {
	case UBLK_IO_OP_READ:
		sqe.Opcode = IORING_OP_READ_FIXED
	case UBLK_IO_OP_WRITE:
		sqe.Opcode = IORING_OP_WRITE_FIXED
	default:
		return IOResultEIO
	}

	sqe.Fd = 0 // Fixed file index 0 (overlay)
	sqe.Off = uint64(offset)
	sqe.Len = uint32(length)
	sqe.BufIndex = bufIndex
	sqe.Flags = IOSQE_FIXED_FILE
	sqe.UserData = uint64(bufIndex)

	// Submit and wait
	if _, err := w.zcRing.Submit(); err != nil {
		return IOResultEIO
	}

	cqe, err := w.zcRing.WaitCQE()
	if err != nil {
		return IOResultEIO
	}

	res := cqe.Res
	w.zcRing.SeenCQE(cqe)

	if res < 0 {
		return IOResultEIO
	}

	return IOResultOK
}

func (w *ioWorker) submitAutoZeroCopyIO(op uint8, flags uint32, offset int64, length int, tag uint16) error {
	if !w.autoBufReg {
		return errors.New("auto buffer registration not enabled")
	}
	if w.zcBackend <= 0 {
		return errors.New("zero-copy backend not initialized")
	}
	tagIdx := int(tag)
	if tagIdx >= len(w.dataPending) {
		return errors.New("tag out of range")
	}
	if w.dataPending[tagIdx] {
		return errors.New("data IO already pending")
	}

	sqe, err := w.ring.GetSQE128()
	if err != nil {
		return err
	}

	switch op {
	case UBLK_IO_OP_READ:
		sqe.Opcode = IORING_OP_READ_FIXED
	case UBLK_IO_OP_WRITE:
		sqe.Opcode = IORING_OP_WRITE_FIXED
	default:
		return errors.New("unsupported zero-copy op")
	}

	sqe.Fd = int32(w.zcBackend)
	sqe.Off = uint64(offset)
	sqe.Len = uint32(length)
	sqe.BufIndex = tag
	sqe.UserData = dataUserData(tag)

	w.dataPending[tagIdx] = true
	w.dataOp[tagIdx] = op
	w.dataFlags[tagIdx] = flags
	w.dataLen[tagIdx] = uint64(length)
	return nil
}

func (w *ioWorker) finishAutoDataIO(tag uint16, cqeRes int32) int32 {
	tagIdx := int(tag)
	if tagIdx >= len(w.dataPending) || !w.dataPending[tagIdx] {
		return IOResultEIO
	}

	op := w.dataOp[tagIdx]
	flags := w.dataFlags[tagIdx]
	length := w.dataLen[tagIdx]
	w.dataPending[tagIdx] = false

	res := IOResultOK
	if cqeRes < 0 || uint64(cqeRes) != length {
		res = IOResultEIO
	}

	if res == IOResultOK && op == UBLK_IO_OP_WRITE && (flags&UBLK_IO_F_FUA) != 0 {
		if flusher, ok := w.device.backend.(Flusher); ok {
			if err := flusher.Flush(); err != nil {
				res = IOResultEIO
			}
		}
	}

	return res
}

func (w *ioWorker) executeIO(op uint8, flags uint32, buf []byte, offset int64, tag uint16) int32 {
	// Check for FUA flag
	isFua := (flags & UBLK_IO_F_FUA) != 0

	switch op {
	case UBLK_IO_OP_READ:
		// Sparse optimization: return zeros for empty regions without I/O
		if w.sparseReader != nil && len(buf) > 0 && w.sparseReader.IsZeroRegion(offset, int64(len(buf))) {
			clear(buf)
			if w.userCopy {
				devOffset := ublkUserCopyPos(w.qid, tag)
				if _, err := w.device.charDevFD.WriteAt(buf, devOffset); err != nil {
					return IOResultEIO
				}
			}
			return IOResultOK
		}

		// READ: Server reads from backend -> User Buffer
		var n int
		var err error
		if rwf, ok := w.device.backend.(ReaderWithFlags); ok {
			n, err = rwf.ReadAtWithFlags(buf, offset, flags)
		} else {
			n, err = w.device.readAt(buf, offset)
		}

		if err != nil || n != len(buf) {
			return IOResultEIO
		}

		if w.userCopy && len(buf) > 0 {
			if w.device.charDevFD == nil {
				return IOResultEIO
			}
			devOffset := ublkUserCopyPos(w.qid, tag)
			if nn, err := w.device.charDevFD.WriteAt(buf, devOffset); err != nil || nn != len(buf) {
				return IOResultEIO
			}
		}
		return IOResultOK

	case UBLK_IO_OP_WRITE:
		var n int
		var err error
		if w.userCopy && len(buf) > 0 {
			if w.device.charDevFD == nil {
				return IOResultEIO
			}
			devOffset := ublkUserCopyPos(w.qid, tag)
			if nn, err := w.device.charDevFD.ReadAt(buf, devOffset); err != nil || nn != len(buf) {
				return IOResultEIO
			}
		}

		// Handle FUA optimization
		if isFua {
			if fuaWriter, ok := w.device.backend.(FuaWriter); ok {
				n, err = fuaWriter.WriteFua(buf, offset)
				if err != nil || n != len(buf) {
					return IOResultEIO
				}
				return IOResultOK
			}
		}

		// Normal Write (with flags if supported)
		if wwf, ok := w.device.backend.(WriterWithFlags); ok {
			n, err = wwf.WriteAtWithFlags(buf, offset, flags)
		} else {
			n, err = w.device.writeAt(buf, offset)
		}

		if err != nil || n != len(buf) {
			return IOResultEIO
		}

		// Fallback FUA: Flush after write
		if isFua {
			if flusher, ok := w.device.backend.(Flusher); ok {
				if err := flusher.Flush(); err != nil {
					return IOResultEIO
				}
			}
		}

		return IOResultOK

	case UBLK_IO_OP_FLUSH:
		if flusher, ok := w.device.backend.(Flusher); ok {
			if err := flusher.Flush(); err != nil {
				return IOResultEIO
			}
		}
		return IOResultOK

	case UBLK_IO_OP_DISCARD:
		if discarder, ok := w.device.backend.(Discarder); ok {
			if err := discarder.Discard(offset, int64(len(buf))); err != nil {
				return IOResultEIO
			}
			return IOResultOK
		}
		return IOResultENOTSUP

	case UBLK_IO_OP_WRITE_ZEROES:
		if wz, ok := w.device.backend.(WriteZeroer); ok {
			if err := wz.WriteZeroes(offset, int64(len(buf))); err != nil {
				return IOResultEIO
			}
			return IOResultOK
		}
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

func (w *ioWorker) zcRegisterBuf(bufIndex uint16, tag uint16) error {
	cmd, op := NewRegisterIOBufCommand(w.qid, tag, uint64(bufIndex))
	return w.submitZCUringCmd(cmd, op)
}

func (w *ioWorker) zcUnregisterBuf(bufIndex uint16, tag uint16) error {
	cmd, op := NewUnregisterIOBufCommand(w.qid, tag, uint64(bufIndex))
	return w.submitZCUringCmd(cmd, op)
}

func (w *ioWorker) submitZCUringCmd(cmd *UblkIOCommand, op uint32) error {
	if w.zcRing == nil {
		return errors.New("zero-copy ring not initialized")
	}
	sqe, err := w.zcRing.GetSQE128()
	if err != nil {
		return err
	}

	cmdData := cmd.ToBytes()
	sqe.Opcode = IORING_OP_URING_CMD
	sqe.Off = uint64(op)
	copy(sqe.Cmd[:], cmdData)
	sqe.Len = uint32(cmd.Size())
	sqe.Fd = int32(w.device.charDevFD.Fd())

	if _, err := w.zcRing.Submit(); err != nil {
		return err
	}
	cqe, err := w.zcRing.WaitCQE()
	if err != nil {
		return err
	}
	res := cqe.Res
	w.zcRing.SeenCQE(cqe)
	if res < 0 {
		return fmt.Errorf("uring_cmd failed: %d", res)
	}
	return nil
}

func (w *ioWorker) zcFixedIO(op uint8, offset int64, length int, bufIndex uint16, flags uint32) int32 {
	if length == 0 {
		return IOResultOK
	}
	if w.zcRing == nil || w.zcBackend <= 0 {
		return IOResultEIO
	}

	sqe, err := w.zcRing.GetSQE128()
	if err != nil {
		return IOResultEIO
	}

	switch op {
	case UBLK_IO_OP_READ:
		sqe.Opcode = IORING_OP_READ_FIXED
	case UBLK_IO_OP_WRITE:
		sqe.Opcode = IORING_OP_WRITE_FIXED
	default:
		return IOResultENOTSUP
	}

	sqe.Fd = int32(w.zcBackend)
	sqe.Off = uint64(offset)
	sqe.Len = uint32(length)
	sqe.BufIndex = bufIndex

	if _, err := w.zcRing.Submit(); err != nil {
		return IOResultEIO
	}
	cqe, err := w.zcRing.WaitCQE()
	if err != nil {
		return IOResultEIO
	}
	res := cqe.Res
	w.zcRing.SeenCQE(cqe)

	if res < 0 || res != int32(length) {
		return IOResultEIO
	}

	if op == UBLK_IO_OP_WRITE && (flags&UBLK_IO_F_FUA) != 0 {
		if flusher, ok := w.device.backend.(Flusher); ok {
			if err := flusher.Flush(); err != nil {
				return IOResultEIO
			}
		}
	}

	return IOResultOK
}

// submitFetchReq prepares a FETCH_REQ SQE for a tag.
func (w *ioWorker) submitFetchReq(tag uint16) error {
	sqe, err := w.ring.GetSQE128()
	if err != nil {
		return fmt.Errorf("failed to get SQE128: %w", err)
	}

	addr := w.ioBufferAddr(tag)
	cmd, op := NewFetchReqCommand(w.qid, tag, addr)
	w.prepareSQE(sqe, cmd, op, tag)
	return nil
}

// submitCommitAndFetch prepares a COMMIT_AND_FETCH_REQ SQE for a tag.
func (w *ioWorker) submitCommitAndFetch(tag uint16, result int32) error {
	sqe, err := w.ring.GetSQE128()
	if err != nil {
		return fmt.Errorf("failed to get SQE128: %w", err)
	}

	addr := w.ioBufferAddr(tag)
	cmd, op := NewCommitAndFetchReqCommand(w.qid, tag, result, addr)
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
	if w.autoBufReg && (op == uint32(UBLK_U_IO_FETCH_REQ) || op == uint32(UBLK_U_IO_COMMIT_AND_FETCH_REQ)) {
		sqe.Addr = autoBufRegAddr(tag, 0)
	}

	// Use fixed file if registered (reduces per-IO overhead)
	if w.ring.HasFixedFiles() {
		sqe.Fd = 0 // Index into registered files array
		sqe.Flags = IOSQE_FIXED_FILE
	} else {
		sqe.Fd = int32(w.device.charDevFD.Fd())
	}
}

func autoBufRegAddr(index uint16, flags uint8) uint64 {
	return uint64(index) | uint64(flags)<<16
}

func (w *ioWorker) ioBufferAddr(tag uint16) uint64 {
	if w.userCopy {
		return 0
	}
	tagIdx := int(tag)
	if tagIdx >= len(w.tagBuffers) {
		return 0
	}
	buf := w.tagBuffers[tagIdx]
	if len(buf) == 0 {
		return 0
	}
	return uint64(uintptr(unsafe.Pointer(&buf[0])))
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
		_ = unix.Munmap(w.mmapAddr)
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
