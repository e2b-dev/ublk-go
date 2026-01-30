package ublk

import (
	"errors"
	"fmt"
	"runtime"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

// Ring represents an io_uring instance
// It manages the submission and completion queues and provides
// methods to submit operations and wait for completions.
type Ring struct {
	fd     int
	sq     *submissionQueue
	cq     *completionQueue
	sqes   []UringSQE
	params UringParams

	// Mmap tracking for cleanup
	mmapSQ   []byte
	mmapCQ   []byte
	mmapSQEs []byte
}

// submissionQueue represents the mapped submission queue
type submissionQueue struct {
	head        *uint32
	tail        *uint32
	ringMask    *uint32
	ringEntries *uint32
	flags       *uint32
	dropped     *uint32
	array       *uint32
	sqeHead     uint32
	sqeTail     uint32
}

// completionQueue represents the mapped completion queue
type completionQueue struct {
	head        *uint32
	tail        *uint32
	ringMask    *uint32
	ringEntries *uint32
	overflow    *uint32
	cqes        []UringCQE
}

// NewRing creates a new io_uring instance
func NewRing(entries uint, flags uint) (*Ring, error) {
	entries = roundUpPow2(entries)
	if entries < 1 || entries > 4096 {
		return nil, errors.New("entries must be between 1 and 4096")
	}

	// Create params struct
	params := UringParams{
		SQEntries: uint32(entries),
		CQEntries: uint32(entries * 2),
		Flags:     uint32(flags),
	}

	// Call io_uring_setup syscall
	fd, _, errno := syscall.Syscall(unix.SYS_IO_URING_SETUP, uintptr(entries), uintptr(unsafe.Pointer(&params)), 0)
	if errno != 0 {
		return nil, fmt.Errorf("io_uring_setup failed: %v", errno)
	}

	ring := &Ring{
		fd:     int(fd),
		params: params,
	}

	if err := ring.mmapRings(entries); err != nil {
		ring.Close()
		return nil, err
	}

	runtime.SetFinalizer(ring, (*Ring).Close)
	return ring, nil
}

func (r *Ring) mmapRings(entries uint) error {
	sqOff := r.params.SqOff
	cqOff := r.params.CqOff

	// Calculate ring sizes
	sqRingSize := int(sqOff.Array) + int(entries)*4
	cqRingSize := int(cqOff.Cqes) + int(r.params.CQEntries)*int(SizeOfUringCQE())

	// Map SQ ring
	sqPtr, err := unix.Mmap(r.fd, IORING_OFF_SQ_RING, sqRingSize,
		unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED|unix.MAP_POPULATE)
	if err != nil {
		return fmt.Errorf("failed to mmap SQ ring: %w", err)
	}
	r.mmapSQ = sqPtr

	// Map CQ ring
	cqPtr, err := unix.Mmap(r.fd, IORING_OFF_CQ_RING, cqRingSize,
		unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED|unix.MAP_POPULATE)
	if err != nil {
		return fmt.Errorf("failed to mmap CQ ring: %w", err)
	}
	r.mmapCQ = cqPtr

	// Map SQEs
	sqeSize := int(SizeOfUringSQE())
	sqesSize := int(entries) * sqeSize
	sqesPtr, err := unix.Mmap(r.fd, IORING_OFF_SQES, sqesSize,
		unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED|unix.MAP_POPULATE)
	if err != nil {
		return fmt.Errorf("failed to mmap SQEs: %w", err)
	}
	r.mmapSQEs = sqesPtr

	// Initialize SQ structure
	r.sq = &submissionQueue{
		head:        (*uint32)(unsafe.Pointer(&sqPtr[sqOff.Head])),
		tail:        (*uint32)(unsafe.Pointer(&sqPtr[sqOff.Tail])),
		ringMask:    (*uint32)(unsafe.Pointer(&sqPtr[sqOff.RingMask])),
		ringEntries: (*uint32)(unsafe.Pointer(&sqPtr[sqOff.RingEntries])),
		flags:       (*uint32)(unsafe.Pointer(&sqPtr[sqOff.Flags])),
		dropped:     (*uint32)(unsafe.Pointer(&sqPtr[sqOff.Dropped])),
		array:       (*uint32)(unsafe.Pointer(&sqPtr[sqOff.Array])),
	}

	// Initialize CQ structure
	r.cq = &completionQueue{
		head:        (*uint32)(unsafe.Pointer(&cqPtr[cqOff.Head])),
		tail:        (*uint32)(unsafe.Pointer(&cqPtr[cqOff.Tail])),
		ringMask:    (*uint32)(unsafe.Pointer(&cqPtr[cqOff.RingMask])),
		ringEntries: (*uint32)(unsafe.Pointer(&cqPtr[cqOff.RingEntries])),
		overflow:    (*uint32)(unsafe.Pointer(&cqPtr[cqOff.Overflow])),
	}

	// Slice casting - keep pointer conversion in single expression to satisfy go vet
	r.sqes = (*[1 << 20]UringSQE)(unsafe.Pointer(&sqesPtr[0]))[:entries:entries]
	r.cq.cqes = (*[1 << 20]UringCQE)(unsafe.Pointer(&cqPtr[cqOff.Cqes]))[:r.params.CQEntries:r.params.CQEntries]

	return nil
}

// Close releases resources associated with the ring
func (r *Ring) Close() error {
	if r.fd < 0 {
		return nil
	}

	if r.mmapSQ != nil {
		unix.Munmap(r.mmapSQ)
	}
	if r.mmapCQ != nil {
		unix.Munmap(r.mmapCQ)
	}
	if r.mmapSQEs != nil {
		unix.Munmap(r.mmapSQEs)
	}

	unix.Close(r.fd)
	r.fd = -1
	return nil
}

// GetSQE gets a new submission queue entry
func (r *Ring) GetSQE() (*UringSQE, error) {
	head := r.sq.sqeHead
	tail := r.sq.sqeTail

	if tail-head >= uint32(len(r.sqes)) {
		return nil, errors.New("submission queue full")
	}

	sqe := &r.sqes[tail&*r.sq.ringMask]
	*sqe = UringSQE{} // Clear entry
	r.sq.sqeTail++

	return sqe, nil
}

// Submit submits all pending SQEs
func (r *Ring) Submit() (int, error) {
	tail := r.sq.sqeTail
	head := r.sq.sqeHead
	count := tail - head

	if count == 0 {
		return 0, nil
	}

	// Fill the SQ ring array
	mask := *r.sq.ringMask
	for i := uint32(0); i < count; i++ {
		idx := (head + i) & mask
		offset := uintptr(idx) * 4
		*(*uint32)(unsafe.Pointer(uintptr(unsafe.Pointer(r.sq.array)) + offset)) = idx
	}

	// Update tail and our local head
	// Memory barrier needed here ideally
	*r.sq.tail = tail
	r.sq.sqeHead = tail

	// Enter kernel
	ret, _, errno := syscall.Syscall6(
		unix.SYS_IO_URING_ENTER,
		uintptr(r.fd),
		uintptr(count),
		0,
		0,
		0,
		0,
	)

	if errno != 0 {
		return 0, fmt.Errorf("io_uring_enter failed: %v", errno)
	}

	return int(ret), nil
}

// WaitCQE waits for a completion queue entry
func (r *Ring) WaitCQE() (*UringCQE, error) {
	for {
		// Check if CQE is available
		head := *r.cq.head
		if head != *r.cq.tail {
			cqe := &r.cq.cqes[head&*r.cq.ringMask]
			return cqe, nil
		}

		// Wait for event
		_, _, errno := syscall.Syscall6(
			unix.SYS_IO_URING_ENTER,
			uintptr(r.fd),
			0,
			1, // min_complete
			IORING_ENTER_GETEVENTS,
			0,
			0,
		)

		if errno != 0 && errno != unix.EINTR {
			return nil, fmt.Errorf("wait failed: %v", errno)
		}
	}
}

// SeenCQE advances the completion queue head
func (r *Ring) SeenCQE(cqe *UringCQE) {
	*r.cq.head++
}

// Helpers

func roundUpPow2(n uint) uint {
	if n == 0 {
		return 1
	}
	n--
	n |= n >> 1
	n |= n >> 2
	n |= n >> 4
	n |= n >> 8
	n |= n >> 16
	n++
	return n
}
