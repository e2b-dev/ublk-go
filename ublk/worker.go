package ublk

import (
	"runtime"
	"unsafe"

	"golang.org/x/sys/unix"
)

type worker struct {
	dev     *Device
	qid     uint16
	depth   uint16
	bufSize int

	ioRing  *ring
	ioDescs []byte
	bufPool []byte
	bufs    [][]byte
}

func newWorker(dev *Device, qid, depth uint16, bufSize int) *worker {
	return &worker{
		dev:     dev,
		qid:     qid,
		depth:   depth,
		bufSize: bufSize,
	}
}

// init creates the IO ring, mmaps descriptors, allocates buffers,
// prepares and submits the initial FETCH_REQ SQEs.
func (w *worker) init() error {
	var err error

	w.ioRing, err = newIORing(uint32(w.depth))
	if err != nil {
		return err
	}

	if err := w.mmapDescs(); err != nil {
		w.cleanup()
		return err
	}

	w.allocBuffers()

	for tag := uint16(0); tag < w.depth; tag++ {
		w.prepareFetch(tag)
	}

	if _, err := w.ioRing.submit(); err != nil {
		w.cleanup()
		return err
	}

	return nil
}

// run is the IO event loop. Must be called on its own goroutine.
func (w *worker) run() {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	defer w.dev.wg.Done()
	defer w.cleanup()

	for {
		c, err := w.ioRing.waitCQE()
		if err != nil {
			return
		}

		for {
			tag := uint16(c.UserData)
			res := c.Res
			w.ioRing.seenCQE()

			if res < 0 {
				return
			}

			result := w.handleIO(tag)
			w.prepareCommitAndFetch(tag, result)

			c = w.ioRing.peekCQE()
			if c == nil {
				break
			}
		}

		if _, err := w.ioRing.submit(); err != nil {
			return
		}
	}
}

func (w *worker) mmapDescs() error {
	mmapSize := int(w.depth) * int(sizeofIODesc)
	mmapOff := int64(w.qid) * int64(maxQueueDepth) * int64(sizeofIODesc)

	data, err := unix.Mmap(
		w.dev.charFD,
		mmapOff,
		mmapSize,
		unix.PROT_READ,
		unix.MAP_SHARED|unix.MAP_POPULATE,
	)
	if err != nil {
		return err
	}
	w.ioDescs = data
	return nil
}

func (w *worker) allocBuffers() {
	total := int(w.depth) * w.bufSize
	w.bufPool = alignedAlloc(total, 4096)
	w.bufs = make([][]byte, w.depth)
	for i := range w.depth {
		off := int(i) * w.bufSize
		w.bufs[i] = w.bufPool[off : off+w.bufSize]
	}
}

func (w *worker) handleIO(tag uint16) int32 {
	desc := w.getDesc(tag)
	op := desc.OpFlags & 0xFF

	offset := int64(desc.StartSector) * 512
	length := int(desc.NrSectors) * 512

	if length > w.bufSize {
		return -int32(unix.EIO)
	}

	switch op {
	case opRead:
		if length == 0 {
			return 0
		}
		buf := w.bufs[tag][:length]
		n, err := w.dev.backend.ReadAt(buf, offset)
		if err != nil || n != length {
			return -int32(unix.EIO)
		}
		return int32(n)

	case opWrite:
		if length == 0 {
			return 0
		}
		buf := w.bufs[tag][:length]
		n, err := w.dev.backend.WriteAt(buf, offset)
		if err != nil || n != length {
			return -int32(unix.EIO)
		}
		return int32(n)

	case opFlush:
		if f, ok := w.dev.backend.(Flusher); ok {
			if err := f.Flush(); err != nil {
				return -int32(unix.EIO)
			}
		}
		return 0

	case opDiscard:
		if d, ok := w.dev.backend.(Discarder); ok {
			if err := d.Discard(offset, int64(length)); err != nil {
				return -int32(unix.EIO)
			}
			return 0
		}
		return -int32(unix.EOPNOTSUPP)

	case opWriteZeroes:
		if wz, ok := w.dev.backend.(WriteZeroer); ok {
			if err := wz.WriteZeroes(offset, int64(length)); err != nil {
				return -int32(unix.EIO)
			}
			return 0
		}
		return -int32(unix.EOPNOTSUPP)

	default:
		return -int32(unix.EOPNOTSUPP)
	}
}

func (w *worker) prepareFetch(tag uint16) {
	sqe := w.ioRing.getSQE64()
	sqe.Opcode = opUringCmd
	sqe.Fd = int32(w.dev.charFD)
	sqe.Off = uint64(uIOFetchReq)
	sqe.UserData = uint64(tag)

	cmd := ioCmd{
		QID:  w.qid,
		Tag:  tag,
		Addr: uint64(uintptr(unsafe.Pointer(&w.bufs[tag][0]))),
	}
	src := (*[unsafe.Sizeof(ioCmd{})]byte)(unsafe.Pointer(&cmd))
	copy(sqe.Cmd[:], src[:])
}

func (w *worker) prepareCommitAndFetch(tag uint16, result int32) {
	sqe := w.ioRing.getSQE64()
	sqe.Opcode = opUringCmd
	sqe.Fd = int32(w.dev.charFD)
	sqe.Off = uint64(uIOCommitAndFetchReq)
	sqe.UserData = uint64(tag)

	cmd := ioCmd{
		QID:    w.qid,
		Tag:    tag,
		Result: result,
		Addr:   uint64(uintptr(unsafe.Pointer(&w.bufs[tag][0]))),
	}
	src := (*[unsafe.Sizeof(ioCmd{})]byte)(unsafe.Pointer(&cmd))
	copy(sqe.Cmd[:], src[:])
}

func (w *worker) getDesc(tag uint16) ioDesc {
	off := uintptr(tag) * sizeofIODesc
	return *(*ioDesc)(unsafe.Pointer(&w.ioDescs[off]))
}

func (w *worker) cleanup() {
	if w.ioDescs != nil {
		_ = unix.Munmap(w.ioDescs)
		w.ioDescs = nil
	}
	if w.ioRing != nil {
		_ = w.ioRing.close()
		w.ioRing = nil
	}
}

func alignedAlloc(size, align int) []byte {
	raw := make([]byte, size+align)
	addr := uintptr(unsafe.Pointer(&raw[0]))
	offset := int((uintptr(align) - addr%uintptr(align)) % uintptr(align))
	return raw[offset : offset+size]
}
