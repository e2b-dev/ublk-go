package ublk

import (
	"errors"
	"fmt"
	"sync/atomic"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

// io_uring constants from linux/io_uring.h.
const (
	opUringCmd = 46

	ioringSetupCoopTaskrun = 1 << 8
	ioringSetupSQE128      = 1 << 10

	ioringEnterGetevents = 1 << 0

	iosqeFixedFile = 1 << 0

	ioringOffSQRing = 0x00000000
	ioringOffCQRing = 0x08000000
	ioringOffSQEs   = 0x10000000
)

// sqe128 is a 128-byte submission queue entry with 80 bytes for passthrough cmd data.
type sqe128 struct {
	Opcode      uint8
	Flags       uint8
	Ioprio      uint16
	Fd          int32
	Off         uint64
	Addr        uint64
	Len         uint32
	OpFlags     uint32
	UserData    uint64
	BufIndex    uint16
	Personality uint16
	SpliceFdIn  int32
	Cmd         [80]byte
}

// cqe is a 16-byte completion queue entry.
type cqe struct {
	UserData uint64
	Res      int32
	Flags    uint32
}

type uringParams struct {
	SQEntries    uint32
	CQEntries    uint32
	Flags        uint32
	SqThreadCPU  uint32
	SqThreadIdle uint32
	Features     uint32
	WqFd         uint32
	Resv         [3]uint32
	SqOff        sqOffsets
	CqOff        cqOffsets
}

type sqOffsets struct {
	Head        uint32
	Tail        uint32
	RingMask    uint32
	RingEntries uint32
	Flags       uint32
	Dropped     uint32
	Array       uint32
	Resv1       uint32
	Resv2       uint64
}

type cqOffsets struct {
	Head        uint32
	Tail        uint32
	RingMask    uint32
	RingEntries uint32
	Overflow    uint32
	Cqes        uint32
	Flags       uint32
	Resv1       uint32
	Resv2       uint64
}

const (
	sqe128Size = unsafe.Sizeof(sqe128{})
	cqeSize    = unsafe.Sizeof(cqe{})
)

type ring struct {
	fd     int
	params uringParams

	// SQ state
	sqHead, sqTail, sqMask, sqFlags *uint32
	sqArray                         unsafe.Pointer
	sqeBase                         unsafe.Pointer
	sqeLocalHead, sqeLocalTail      uint32
	sqEntries                       uint32

	// CQ state
	cqHead, cqTail, cqMask *uint32
	cqeBase                unsafe.Pointer
	cqEntries              uint32

	// mmap regions for cleanup
	mmapSQ, mmapCQ, mmapSQEs []byte
}

func newRing(entries uint32) (*ring, error) {
	entries = roundUp2(entries)
	return setupRing(entries, ioringSetupSQE128)
}

func setupRing(entries, flags uint32) (*ring, error) {
	p := uringParams{
		Flags: flags,
	}

	fd, _, errno := syscall.Syscall(
		unix.SYS_IO_URING_SETUP,
		uintptr(entries),
		uintptr(unsafe.Pointer(&p)),
		0,
	)
	if errno != 0 {
		return nil, fmt.Errorf("io_uring_setup: %w", errno)
	}

	r := &ring{
		fd:        int(fd),
		params:    p,
		sqEntries: p.SQEntries,
		cqEntries: p.CQEntries,
	}

	if err := r.mmapRings(); err != nil {
		_ = r.close()
		return nil, err
	}

	return r, nil
}

func (r *ring) mmapRings() error {
	sq := r.params.SqOff
	cq := r.params.CqOff

	sqRingSize := int(sq.Array) + int(r.sqEntries)*4
	cqRingSize := int(cq.Cqes) + int(r.cqEntries)*int(cqeSize)
	sqesSize := int(r.sqEntries) * int(sqe128Size)

	var err error

	r.mmapSQ, err = unix.Mmap(r.fd, ioringOffSQRing, sqRingSize,
		unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED|unix.MAP_POPULATE)
	if err != nil {
		return fmt.Errorf("mmap SQ ring: %w", err)
	}

	r.mmapCQ, err = unix.Mmap(r.fd, ioringOffCQRing, cqRingSize,
		unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED|unix.MAP_POPULATE)
	if err != nil {
		return fmt.Errorf("mmap CQ ring: %w", err)
	}

	r.mmapSQEs, err = unix.Mmap(r.fd, ioringOffSQEs, sqesSize,
		unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED|unix.MAP_POPULATE)
	if err != nil {
		return fmt.Errorf("mmap SQEs: %w", err)
	}

	base := unsafe.Pointer(&r.mmapSQ[0])
	r.sqHead = (*uint32)(unsafe.Add(base, sq.Head))
	r.sqTail = (*uint32)(unsafe.Add(base, sq.Tail))
	r.sqMask = (*uint32)(unsafe.Add(base, sq.RingMask))
	r.sqFlags = (*uint32)(unsafe.Add(base, sq.Flags))
	r.sqArray = unsafe.Add(base, sq.Array)

	cqBase := unsafe.Pointer(&r.mmapCQ[0])
	r.cqHead = (*uint32)(unsafe.Add(cqBase, cq.Head))
	r.cqTail = (*uint32)(unsafe.Add(cqBase, cq.Tail))
	r.cqMask = (*uint32)(unsafe.Add(cqBase, cq.RingMask))
	r.cqeBase = unsafe.Add(cqBase, cq.Cqes)

	r.sqeBase = unsafe.Pointer(&r.mmapSQEs[0])

	return nil
}

func (r *ring) close() error {
	var errs []error
	for _, m := range [][]byte{r.mmapSQ, r.mmapCQ, r.mmapSQEs} {
		if m != nil {
			if err := unix.Munmap(m); err != nil {
				errs = append(errs, err)
			}
		}
	}
	if r.fd >= 0 {
		if err := unix.Close(r.fd); err != nil {
			errs = append(errs, err)
		}
		r.fd = -1
	}
	return errors.Join(errs...)
}

// getSQE returns the next available SQE, zeroed. Returns nil if the SQ is full.
func (r *ring) getSQE() *sqe128 {
	head := r.sqeLocalHead
	tail := r.sqeLocalTail

	if tail-head >= r.sqEntries {
		head = atomic.LoadUint32(r.sqHead)
		r.sqeLocalHead = head
		if tail-head >= r.sqEntries {
			return nil
		}
	}

	idx := tail & atomic.LoadUint32(r.sqMask)
	sqePtr := (*sqe128)(unsafe.Add(r.sqeBase, uintptr(idx)*sqe128Size))
	*sqePtr = sqe128{}
	r.sqeLocalTail++
	return sqePtr
}

// submit flushes pending SQEs to the kernel.
func (r *ring) submit() (int, error) {
	head := r.sqeLocalHead
	tail := r.sqeLocalTail
	count := tail - head
	if count == 0 {
		return 0, nil
	}

	mask := atomic.LoadUint32(r.sqMask)
	for i := uint32(0); i < count; i++ {
		idx := (head + i) & mask
		*(*uint32)(unsafe.Add(r.sqArray, uintptr(idx)*4)) = idx
	}

	atomic.StoreUint32(r.sqTail, tail)
	r.sqeLocalHead = tail

	ret, _, errno := syscall.Syscall6(
		unix.SYS_IO_URING_ENTER,
		uintptr(r.fd), uintptr(count), 0, 0, 0, 0,
	)
	if errno != 0 {
		return 0, fmt.Errorf("io_uring_enter submit: %w", errno)
	}
	return int(ret), nil
}

// waitCQE blocks until at least one CQE is available and returns it.
func (r *ring) waitCQE() (*cqe, error) {
	for {
		head := atomic.LoadUint32(r.cqHead)
		tail := atomic.LoadUint32(r.cqTail)
		if head != tail {
			idx := head & atomic.LoadUint32(r.cqMask)
			return (*cqe)(unsafe.Add(r.cqeBase, uintptr(idx)*cqeSize)), nil
		}

		_, _, errno := syscall.Syscall6(
			unix.SYS_IO_URING_ENTER,
			uintptr(r.fd), 0, 1, ioringEnterGetevents, 0, 0,
		)
		if errno != 0 && errno != unix.EINTR {
			return nil, fmt.Errorf("io_uring_enter wait: %w", errno)
		}
	}
}

// peekCQE returns the next CQE without blocking, or nil if none ready.
func (r *ring) peekCQE() *cqe {
	head := atomic.LoadUint32(r.cqHead)
	tail := atomic.LoadUint32(r.cqTail)
	if head == tail {
		return nil
	}
	idx := head & atomic.LoadUint32(r.cqMask)
	return (*cqe)(unsafe.Add(r.cqeBase, uintptr(idx)*cqeSize))
}

// seenCQE advances the CQ head by one, releasing the current CQE.
func (r *ring) seenCQE() {
	atomic.AddUint32(r.cqHead, 1)
}

func roundUp2(v uint32) uint32 {
	if v == 0 {
		return 1
	}
	v--
	v |= v >> 1
	v |= v >> 2
	v |= v >> 4
	v |= v >> 8
	v |= v >> 16
	return v + 1
}
