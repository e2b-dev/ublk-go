package ublk

import (
	"errors"
	"fmt"
	"runtime"
	"sync/atomic"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

type Ring struct {
	fd            int
	sq            *submissionQueue
	cq            *completionQueue
	sqes          []UringSQE
	params        UringParams
	flags         uint
	fixedFiles    []int32
	hasFixedFiles bool
	mmapSQ        []byte
	mmapCQ        []byte
	mmapSQEs      []byte
	sqeSize       uintptr
}

type RingOption func(*ringConfig)

type ringConfig struct {
	singleIssuer bool
	deferTaskrun bool
	coopTaskrun  bool
	sqe128       bool
}

func WithSingleIssuer() RingOption {
	return func(c *ringConfig) {
		c.singleIssuer = true
	}
}

func WithDeferTaskrun() RingOption {
	return func(c *ringConfig) {
		c.deferTaskrun = true
		c.singleIssuer = true // Required
	}
}

func WithCoopTaskrun() RingOption {
	return func(c *ringConfig) {
		c.coopTaskrun = true
	}
}

func WithSQE128() RingOption {
	return func(c *ringConfig) {
		c.sqe128 = true
	}
}

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

type completionQueue struct {
	head        *uint32
	tail        *uint32
	ringMask    *uint32
	ringEntries *uint32
	overflow    *uint32
	cqes        []UringCQE
}

// NewRing creates a new io_uring instance.
func NewRing(entries uint, flags uint) (*Ring, error) {
	return NewRingWithOptions(entries, flags)
}

// NewRingWithOptions creates a new io_uring instance with optional performance settings.
func NewRingWithOptions(entries uint, flags uint, opts ...RingOption) (*Ring, error) {
	entries = roundUpPow2(entries)
	if entries < 1 || entries > 4096 {
		return nil, errors.New("entries must be between 1 and 4096")
	}

	// Apply options
	cfg := &ringConfig{}
	for _, opt := range opts {
		opt(cfg)
	}

	// Add performance flags based on options
	if cfg.singleIssuer {
		flags |= IORING_SETUP_SINGLE_ISSUER
	}
	if cfg.deferTaskrun {
		flags |= IORING_SETUP_DEFER_TASKRUN
	}
	if cfg.coopTaskrun {
		flags |= IORING_SETUP_COOP_TASKRUN
	}
	if cfg.sqe128 {
		flags |= IORING_SETUP_SQE128
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
		return nil, fmt.Errorf("io_uring_setup failed: %w", errno)
	}

	ring := &Ring{
		fd:      int(fd),
		params:  params,
		flags:   flags,
		sqeSize: SizeOfUringSQE,
	}

	if cfg.sqe128 {
		ring.sqeSize = SizeOfUringSQE128
	}

	if err := ring.mmapRings(entries); err != nil {
		_ = ring.Close() // Cleanup on error, best-effort
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
	cqRingSize := int(cqOff.Cqes) + int(r.params.CQEntries)*int(SizeOfUringCQE)

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
	sqesSize := int(entries) * int(r.sqeSize)
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
	// Note: r.sqes is NOT initialized here anymore if using SQE128, or we can keep it for 64-bit backward compat
	// But clearer to just rely on mmapSQEs byte slice and helper methods
	if r.sqeSize == SizeOfUringSQE {
		r.sqes = (*[1 << 20]UringSQE)(unsafe.Pointer(&sqesPtr[0]))[:entries:entries]
	}
	r.cq.cqes = (*[1 << 20]UringCQE)(unsafe.Pointer(&cqPtr[cqOff.Cqes]))[:r.params.CQEntries:r.params.CQEntries]

	return nil
}

// Close releases resources associated with the ring.
func (r *Ring) Close() error {
	if r.fd < 0 {
		return nil
	}

	var errs []error

	if r.mmapSQ != nil {
		if err := unix.Munmap(r.mmapSQ); err != nil {
			errs = append(errs, fmt.Errorf("munmap SQ: %w", err))
		}
		r.mmapSQ = nil
	}
	if r.mmapCQ != nil {
		if err := unix.Munmap(r.mmapCQ); err != nil {
			errs = append(errs, fmt.Errorf("munmap CQ: %w", err))
		}
		r.mmapCQ = nil
	}
	if r.mmapSQEs != nil {
		if err := unix.Munmap(r.mmapSQEs); err != nil {
			errs = append(errs, fmt.Errorf("munmap SQEs: %w", err))
		}
		r.mmapSQEs = nil
	}

	if err := unix.Close(r.fd); err != nil {
		errs = append(errs, fmt.Errorf("close fd: %w", err))
	}
	r.fd = -1

	return errors.Join(errs...)
}

// GetSQE gets a new submission queue entry (standard 64-byte).
// Returns error if ring was initialized with SQE128.
func (r *Ring) GetSQE() (*UringSQE, error) {
	if r.sqeSize != SizeOfUringSQE {
		return nil, errors.New("Ring initialized with SQE128, use GetSQE128")
	}

	head := r.sq.sqeHead
	tail := r.sq.sqeTail

	if tail-head >= uint32(len(r.sqes)) {
		// Refresh head from kernel to see if entries have been consumed
		head = atomic.LoadUint32(r.sq.head)
		r.sq.sqeHead = head
		if tail-head >= uint32(len(r.sqes)) {
			return nil, errors.New("submission queue full")
		}
	}

	sqe := &r.sqes[tail&*r.sq.ringMask]
	*sqe = UringSQE{} // Clear entry
	r.sq.sqeTail++

	return sqe, nil
}

// GetSQE128 gets a new 128-byte submission queue entry.
// Returns error if ring was NOT initialized with SQE128.
func (r *Ring) GetSQE128() (*UringSQE128, error) {
	if r.sqeSize != SizeOfUringSQE128 {
		return nil, errors.New("Ring not initialized with SQE128")
	}

	head := r.sq.sqeHead
	tail := r.sq.sqeTail
	entries := uint32(len(r.mmapSQEs)) / uint32(r.sqeSize)

	if tail-head >= entries {
		// Refresh head from kernel
		head = atomic.LoadUint32(r.sq.head)
		r.sq.sqeHead = head
		if tail-head >= entries {
			return nil, errors.New("submission queue full")
		}
	}

	idx := tail & *r.sq.ringMask
	offset := uintptr(idx) * r.sqeSize
	sqe := (*UringSQE128)(unsafe.Pointer(&r.mmapSQEs[offset]))

	*sqe = UringSQE128{} // Clear entry
	r.sq.sqeTail++

	return sqe, nil
}

// Submit submits all pending SQEs.
func (r *Ring) Submit() (int, error) {
	tail := r.sq.sqeTail
	head := r.sq.sqeHead
	count := tail - head

	if count == 0 {
		return 0, nil
	}

	// Fill the SQ ring array
	mask := *r.sq.ringMask
	for i := range count {
		idx := (head + i) & mask
		*(*uint32)(unsafe.Add(unsafe.Pointer(r.sq.array), idx*4)) = idx
	}

	// Update tail with a store barrier to ensure SQE writes are visible
	atomic.StoreUint32(r.sq.tail, tail)
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
		return 0, fmt.Errorf("io_uring_enter failed: %w", errno)
	}

	return int(ret), nil
}

// WaitCQE waits for a completion queue entry.
func (r *Ring) WaitCQE() (*UringCQE, error) {
	for {
		// Check if CQE is available using atomic load
		head := atomic.LoadUint32(r.cq.head)
		tail := atomic.LoadUint32(r.cq.tail)
		if head != tail {
			mask := atomic.LoadUint32(r.cq.ringMask)
			cqe := &r.cq.cqes[head&mask]
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
			return nil, fmt.Errorf("wait failed: %w", errno)
		}
	}
}

// SeenCQE advances the completion queue head.
func (r *Ring) SeenCQE(_ *UringCQE) {
	atomic.AddUint32(r.cq.head, 1)
}

// CQEReady returns true if there are completions ready to be processed.
// This is a non-blocking check useful for batch processing.
func (r *Ring) CQEReady() bool {
	head := atomic.LoadUint32(r.cq.head)
	tail := atomic.LoadUint32(r.cq.tail)
	return head != tail
}

// io_uring_register opcodes.
const (
	IORING_REGISTER_FILES        = 2
	IORING_UNREGISTER_FILES      = 3
	IORING_REGISTER_FILES_UPDATE = 6
	IORING_REGISTER_BUFFERS2     = 15
)

const (
	IORING_RSRC_REGISTER_SPARSE = 1 << 0
)

// RegisterFiles registers file descriptors for use with IOSQE_FIXED_FILE.
// Once registered, use the index (0, 1, 2...) as the fd in SQEs with
// the IOSQE_FIXED_FILE flag set. This reduces per-I/O overhead.
func (r *Ring) RegisterFiles(fds []int) error {
	if len(fds) == 0 {
		return errors.New("no files to register")
	}

	// Convert to int32 array for syscall
	files := make([]int32, len(fds))
	for i, fd := range fds {
		files[i] = int32(fd)
	}

	_, _, errno := syscall.Syscall6(
		unix.SYS_IO_URING_REGISTER,
		uintptr(r.fd),
		IORING_REGISTER_FILES,
		uintptr(unsafe.Pointer(&files[0])),
		uintptr(len(files)),
		0, 0,
	)
	if errno != 0 {
		return fmt.Errorf("io_uring_register files failed: %w", errno)
	}

	r.fixedFiles = files
	r.hasFixedFiles = true
	return nil
}

type ioUringBufReg struct {
	RingAddr    uint64
	RingEntries uint32
	Bgid        uint16
	Flags       uint16
	Resv        [3]uint64
}

// RegisterSparseBuffers creates a sparse fixed-buffer table for this ring.
// Entries is the number of buffer slots to reserve.
func (r *Ring) RegisterSparseBuffers(entries uint32) error {
	if entries == 0 {
		return errors.New("entries must be > 0")
	}

	reg := ioUringBufReg{
		RingEntries: entries,
		Bgid:        0,
		Flags:       IORING_RSRC_REGISTER_SPARSE,
	}

	_, _, errno := syscall.Syscall6(
		unix.SYS_IO_URING_REGISTER,
		uintptr(r.fd),
		IORING_REGISTER_BUFFERS2,
		uintptr(unsafe.Pointer(&reg)),
		1,
		0, 0,
	)
	if errno != 0 {
		return fmt.Errorf("io_uring_register buffers2 failed: %w", errno)
	}
	return nil
}

// UnregisterFiles unregisters all previously registered files.
func (r *Ring) UnregisterFiles() error {
	if !r.hasFixedFiles {
		return nil
	}

	_, _, errno := syscall.Syscall6(
		unix.SYS_IO_URING_REGISTER,
		uintptr(r.fd),
		IORING_UNREGISTER_FILES,
		0, 0, 0, 0,
	)
	if errno != 0 {
		return fmt.Errorf("io_uring_unregister files failed: %w", errno)
	}

	r.fixedFiles = nil
	r.hasFixedFiles = false
	return nil
}

// HasFixedFiles returns true if files have been registered.
func (r *Ring) HasFixedFiles() bool {
	return r.hasFixedFiles
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
