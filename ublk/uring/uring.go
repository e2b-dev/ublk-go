// Package uring provides a minimal pure-Go io_uring wrapper.
package uring

import (
	"errors"
	"fmt"
	"sync/atomic"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

const (
	OpUringCmd = 46

	setupSQE128      = 1 << 10
	enterGetevents   = 1 << 0

	offSQRing = 0x00000000
	offCQRing = 0x08000000
	offSQEs   = 0x10000000
)

type SQE128 struct {
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

type SQE64 struct {
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
	Cmd         [16]byte
}

type CQE struct {
	UserData uint64
	Res      int32
	Flags    uint32
}

type params struct {
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
	Head, Tail, RingMask, RingEntries, Flags, Dropped, Array uint32
	Resv1                                                    uint32
	Resv2                                                    uint64
}

type cqOffsets struct {
	Head, Tail, RingMask, RingEntries, Overflow, Cqes, Flags uint32
	Resv1                                                    uint32
	Resv2                                                    uint64
}

var (
	sqe128Size = unsafe.Sizeof(SQE128{})
	sqe64Size  = unsafe.Sizeof(SQE64{})
	cqeSize    = unsafe.Sizeof(CQE{})
)

// Ring is an io_uring instance.
type Ring struct {
	fd      int
	p       params
	sqeSize uintptr

	sqHead, sqTail, sqMask, sqFlags *uint32
	sqArray                         unsafe.Pointer
	sqeBase                         unsafe.Pointer
	sqeLocalHead, sqeLocalTail      uint32
	sqEntries                       uint32

	cqHead, cqTail, cqMask *uint32
	cqeBase                unsafe.Pointer
	cqEntries              uint32

	mmapSQ, mmapCQ, mmapSQEs []byte
}

// NewSQE128 creates a ring with 128-byte SQEs.
func NewSQE128(entries uint32) (*Ring, error) {
	return setup(roundUp2(entries), setupSQE128, sqe128Size)
}

// New creates a ring with standard 64-byte SQEs.
func New(entries uint32) (*Ring, error) {
	return setup(roundUp2(entries), 0, sqe64Size)
}

func setup(entries, flags uint32, sqeSz uintptr) (*Ring, error) {
	p := params{Flags: flags}

	fd, _, errno := syscall.Syscall(
		unix.SYS_IO_URING_SETUP,
		uintptr(entries),
		uintptr(unsafe.Pointer(&p)),
		0,
	)
	if errno != 0 {
		return nil, fmt.Errorf("io_uring_setup: %w", errno)
	}

	r := &Ring{
		fd:        int(fd),
		p:         p,
		sqeSize:   sqeSz,
		sqEntries: p.SQEntries,
		cqEntries: p.CQEntries,
	}

	if err := r.mmapRings(); err != nil {
		_ = r.Close()
		return nil, err
	}

	return r, nil
}

func (r *Ring) mmapRings() error {
	sq := r.p.SqOff
	cq := r.p.CqOff

	sqRingSize := int(sq.Array) + int(r.sqEntries)*4
	cqRingSize := int(cq.Cqes) + int(r.cqEntries)*int(cqeSize)
	sqesSize := int(r.sqEntries) * int(r.sqeSize)

	var err error

	r.mmapSQ, err = unix.Mmap(r.fd, offSQRing, sqRingSize,
		unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED|unix.MAP_POPULATE)
	if err != nil {
		return fmt.Errorf("mmap SQ ring: %w", err)
	}

	r.mmapCQ, err = unix.Mmap(r.fd, offCQRing, cqRingSize,
		unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED|unix.MAP_POPULATE)
	if err != nil {
		return fmt.Errorf("mmap CQ ring: %w", err)
	}

	r.mmapSQEs, err = unix.Mmap(r.fd, offSQEs, sqesSize,
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

// SQEntries returns the SQ size.
func (r *Ring) SQEntries() uint32 { return r.sqEntries }

// Close releases all ring resources.
func (r *Ring) Close() error {
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

func (r *Ring) nextSQE() unsafe.Pointer {
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
	r.sqeLocalTail++
	return unsafe.Add(r.sqeBase, uintptr(idx)*r.sqeSize)
}

// GetSQE128 returns a zeroed 128-byte SQE, or nil if full.
func (r *Ring) GetSQE128() *SQE128 {
	ptr := r.nextSQE()
	if ptr == nil {
		return nil
	}
	sqe := (*SQE128)(ptr)
	*sqe = SQE128{}
	return sqe
}

// GetSQE64 returns a zeroed 64-byte SQE, or nil if full.
func (r *Ring) GetSQE64() *SQE64 {
	ptr := r.nextSQE()
	if ptr == nil {
		return nil
	}
	sqe := (*SQE64)(ptr)
	*sqe = SQE64{}
	return sqe
}

func (r *Ring) flushSQ() uint32 {
	head := r.sqeLocalHead
	tail := r.sqeLocalTail
	count := tail - head
	if count == 0 {
		return 0
	}

	mask := atomic.LoadUint32(r.sqMask)
	for i := uint32(0); i < count; i++ {
		idx := (head + i) & mask
		*(*uint32)(unsafe.Add(r.sqArray, uintptr(idx)*4)) = idx
	}

	atomic.StoreUint32(r.sqTail, tail)
	r.sqeLocalHead = tail
	return count
}

// Submit flushes pending SQEs to the kernel.
func (r *Ring) Submit() (int, error) {
	count := r.flushSQ()
	if count == 0 {
		return 0, nil
	}

	ret, _, errno := syscall.Syscall6(
		unix.SYS_IO_URING_ENTER,
		uintptr(r.fd), uintptr(count), 0, 0, 0, 0,
	)
	if errno != 0 {
		return 0, fmt.Errorf("io_uring_enter submit: %w", errno)
	}
	return int(ret), nil
}

// SubmitAndWait submits SQEs and processes task work.
func (r *Ring) SubmitAndWait() (int, error) {
	count := r.flushSQ()

	ret, _, errno := syscall.Syscall6(
		unix.SYS_IO_URING_ENTER,
		uintptr(r.fd), uintptr(count), 0, enterGetevents, 0, 0,
	)
	if errno != 0 {
		return 0, fmt.Errorf("io_uring_enter submit+wait: %w", errno)
	}
	return int(ret), nil
}

// WaitCQE blocks until a CQE is available.
func (r *Ring) WaitCQE() (*CQE, error) {
	for {
		head := atomic.LoadUint32(r.cqHead)
		tail := atomic.LoadUint32(r.cqTail)
		if head != tail {
			idx := head & atomic.LoadUint32(r.cqMask)
			return (*CQE)(unsafe.Add(r.cqeBase, uintptr(idx)*cqeSize)), nil
		}

		_, _, errno := syscall.Syscall6(
			unix.SYS_IO_URING_ENTER,
			uintptr(r.fd), 0, 1, enterGetevents, 0, 0,
		)
		if errno != 0 && errno != unix.EINTR {
			return nil, fmt.Errorf("io_uring_enter wait: %w", errno)
		}
	}
}

// PeekCQE returns the next CQE without blocking, or nil.
func (r *Ring) PeekCQE() *CQE {
	head := atomic.LoadUint32(r.cqHead)
	tail := atomic.LoadUint32(r.cqTail)
	if head == tail {
		return nil
	}
	idx := head & atomic.LoadUint32(r.cqMask)
	return (*CQE)(unsafe.Add(r.cqeBase, uintptr(idx)*cqeSize))
}

// SeenCQE advances the CQ head.
func (r *Ring) SeenCQE() {
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
