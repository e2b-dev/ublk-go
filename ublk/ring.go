package ublk

import (
	"errors"
	"fmt"
	"math/bits"
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

func WithSingleIssuer() RingOption { return func(c *ringConfig) { c.singleIssuer = true } }
func WithDeferTaskrun() RingOption {
	return func(c *ringConfig) { c.deferTaskrun = true; c.singleIssuer = true }
}
func WithCoopTaskrun() RingOption { return func(c *ringConfig) { c.coopTaskrun = true } }
func WithSQE128() RingOption      { return func(c *ringConfig) { c.sqe128 = true } }

type submissionQueue struct {
	head, tail, ringMask, ringEntries, flags, dropped *uint32
	array                                             *uint32
	sqeHead, sqeTail                                  uint32
}

type completionQueue struct {
	head, tail, ringMask, ringEntries, overflow *uint32
	cqes                                        []UringCQE
}

func NewRing(entries uint, flags uint) (*Ring, error) {
	return NewRingWithOptions(entries, flags)
}

func NewRingWithOptions(entries uint, flags uint, opts ...RingOption) (*Ring, error) {
	entries = roundUpPow2(entries)
	if entries < 1 || entries > 4096 {
		return nil, errors.New("entries must be between 1 and 4096")
	}

	cfg := &ringConfig{}
	for _, opt := range opts {
		opt(cfg)
	}

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

	params := UringParams{
		SQEntries: uint32(entries),
		CQEntries: uint32(entries * 2),
		Flags:     uint32(flags),
	}

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
		_ = ring.Close()
		return nil, err
	}

	runtime.SetFinalizer(ring, (*Ring).Close)
	return ring, nil
}

func (r *Ring) mmapRings(entries uint) error {
	sqOff := r.params.SqOff
	cqOff := r.params.CqOff

	sqRingSize := int(sqOff.Array) + int(entries)*4
	cqRingSize := int(cqOff.Cqes) + int(r.params.CQEntries)*int(SizeOfUringCQE)

	sqPtr, err := unix.Mmap(r.fd, IORING_OFF_SQ_RING, sqRingSize,
		unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED|unix.MAP_POPULATE)
	if err != nil {
		return fmt.Errorf("mmap SQ ring: %w", err)
	}
	r.mmapSQ = sqPtr

	cqPtr, err := unix.Mmap(r.fd, IORING_OFF_CQ_RING, cqRingSize,
		unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED|unix.MAP_POPULATE)
	if err != nil {
		return fmt.Errorf("mmap CQ ring: %w", err)
	}
	r.mmapCQ = cqPtr

	sqesSize := int(entries) * int(r.sqeSize)
	sqesPtr, err := unix.Mmap(r.fd, IORING_OFF_SQES, sqesSize,
		unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED|unix.MAP_POPULATE)
	if err != nil {
		return fmt.Errorf("mmap SQEs: %w", err)
	}
	r.mmapSQEs = sqesPtr

	r.sq = &submissionQueue{
		head:        (*uint32)(unsafe.Pointer(&sqPtr[sqOff.Head])),
		tail:        (*uint32)(unsafe.Pointer(&sqPtr[sqOff.Tail])),
		ringMask:    (*uint32)(unsafe.Pointer(&sqPtr[sqOff.RingMask])),
		ringEntries: (*uint32)(unsafe.Pointer(&sqPtr[sqOff.RingEntries])),
		flags:       (*uint32)(unsafe.Pointer(&sqPtr[sqOff.Flags])),
		dropped:     (*uint32)(unsafe.Pointer(&sqPtr[sqOff.Dropped])),
		array:       (*uint32)(unsafe.Pointer(&sqPtr[sqOff.Array])),
	}

	r.cq = &completionQueue{
		head:        (*uint32)(unsafe.Pointer(&cqPtr[cqOff.Head])),
		tail:        (*uint32)(unsafe.Pointer(&cqPtr[cqOff.Tail])),
		ringMask:    (*uint32)(unsafe.Pointer(&cqPtr[cqOff.RingMask])),
		ringEntries: (*uint32)(unsafe.Pointer(&cqPtr[cqOff.RingEntries])),
		overflow:    (*uint32)(unsafe.Pointer(&cqPtr[cqOff.Overflow])),
	}

	if r.sqeSize == SizeOfUringSQE {
		r.sqes = (*[1 << 20]UringSQE)(unsafe.Pointer(&sqesPtr[0]))[:entries:entries]
	}
	r.cq.cqes = (*[1 << 20]UringCQE)(unsafe.Pointer(&cqPtr[cqOff.Cqes]))[:r.params.CQEntries:r.params.CQEntries]

	return nil
}

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

func (r *Ring) GetSQE() (*UringSQE, error) {
	if r.sqeSize != SizeOfUringSQE {
		return nil, errors.New("ring uses SQE128, use GetSQE128")
	}

	head, tail := r.sq.sqeHead, r.sq.sqeTail
	if tail-head >= uint32(len(r.sqes)) {
		head = atomic.LoadUint32(r.sq.head)
		r.sq.sqeHead = head
		if tail-head >= uint32(len(r.sqes)) {
			return nil, errors.New("submission queue full")
		}
	}

	sqe := &r.sqes[tail&*r.sq.ringMask]
	*sqe = UringSQE{}
	r.sq.sqeTail++
	return sqe, nil
}

func (r *Ring) GetSQE128() (*UringSQE128, error) {
	if r.sqeSize != SizeOfUringSQE128 {
		return nil, errors.New("ring not initialized with SQE128")
	}

	head, tail := r.sq.sqeHead, r.sq.sqeTail
	entries := uint32(len(r.mmapSQEs)) / uint32(r.sqeSize)

	if tail-head >= entries {
		head = atomic.LoadUint32(r.sq.head)
		r.sq.sqeHead = head
		if tail-head >= entries {
			return nil, errors.New("submission queue full")
		}
	}

	idx := tail & *r.sq.ringMask
	offset := uintptr(idx) * r.sqeSize
	sqe := (*UringSQE128)(unsafe.Pointer(&r.mmapSQEs[offset]))
	*sqe = UringSQE128{}
	r.sq.sqeTail++
	return sqe, nil
}

func (r *Ring) Submit() (int, error) {
	tail, head := r.sq.sqeTail, r.sq.sqeHead
	count := tail - head
	if count == 0 {
		return 0, nil
	}

	mask := *r.sq.ringMask
	for i := range count {
		idx := (head + i) & mask
		*(*uint32)(unsafe.Add(unsafe.Pointer(r.sq.array), idx*4)) = idx
	}

	atomic.StoreUint32(r.sq.tail, tail)
	r.sq.sqeHead = tail

	ret, _, errno := syscall.Syscall6(unix.SYS_IO_URING_ENTER, uintptr(r.fd), uintptr(count), 0, 0, 0, 0)
	if errno != 0 {
		return 0, fmt.Errorf("io_uring_enter failed: %w", errno)
	}
	return int(ret), nil
}

func (r *Ring) WaitCQE() (*UringCQE, error) {
	for {
		head := atomic.LoadUint32(r.cq.head)
		tail := atomic.LoadUint32(r.cq.tail)
		if head != tail {
			return &r.cq.cqes[head&atomic.LoadUint32(r.cq.ringMask)], nil
		}

		_, _, errno := syscall.Syscall6(unix.SYS_IO_URING_ENTER, uintptr(r.fd), 0, 1, IORING_ENTER_GETEVENTS, 0, 0)
		if errno != 0 && errno != unix.EINTR {
			return nil, fmt.Errorf("wait failed: %w", errno)
		}
	}
}

func (r *Ring) SeenCQE(_ *UringCQE) { atomic.AddUint32(r.cq.head, 1) }

func (r *Ring) CQEReady() bool {
	return atomic.LoadUint32(r.cq.head) != atomic.LoadUint32(r.cq.tail)
}

const (
	IORING_REGISTER_FILES        = 2
	IORING_UNREGISTER_FILES      = 3
	IORING_REGISTER_FILES_UPDATE = 6
	IORING_REGISTER_BUFFERS2     = 15
	IORING_RSRC_REGISTER_SPARSE  = 1 << 0
)

func (r *Ring) RegisterFiles(fds []int) error {
	if len(fds) == 0 {
		return errors.New("no files to register")
	}

	files := make([]int32, len(fds))
	for i, fd := range fds {
		files[i] = int32(fd)
	}

	_, _, errno := syscall.Syscall6(unix.SYS_IO_URING_REGISTER, uintptr(r.fd), IORING_REGISTER_FILES,
		uintptr(unsafe.Pointer(&files[0])), uintptr(len(files)), 0, 0)
	if errno != 0 {
		return fmt.Errorf("register files: %w", errno)
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

func (r *Ring) RegisterSparseBuffers(entries uint32) error {
	if entries == 0 {
		return errors.New("entries must be > 0")
	}

	reg := ioUringBufReg{RingEntries: entries, Flags: IORING_RSRC_REGISTER_SPARSE}
	_, _, errno := syscall.Syscall6(unix.SYS_IO_URING_REGISTER, uintptr(r.fd), IORING_REGISTER_BUFFERS2,
		uintptr(unsafe.Pointer(&reg)), 1, 0, 0)
	if errno != 0 {
		return fmt.Errorf("register buffers2: %w", errno)
	}
	return nil
}

func (r *Ring) UnregisterFiles() error {
	if !r.hasFixedFiles {
		return nil
	}

	_, _, errno := syscall.Syscall6(unix.SYS_IO_URING_REGISTER, uintptr(r.fd), IORING_UNREGISTER_FILES, 0, 0, 0, 0)
	if errno != 0 {
		return fmt.Errorf("unregister files: %w", errno)
	}

	r.fixedFiles = nil
	r.hasFixedFiles = false
	return nil
}

func (r *Ring) HasFixedFiles() bool { return r.hasFixedFiles }

func roundUpPow2(n uint) uint {
	if n <= 1 {
		return 1
	}
	return 1 << bits.Len(n-1)
}
