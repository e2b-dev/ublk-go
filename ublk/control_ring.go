package ublk

import (
	"errors"
	"fmt"
	"os"
	"sync"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

// controlRing is an io_uring instance dedicated to control commands.
// ublk control commands must be submitted via io_uring URING_CMD, not ioctl.
type controlRing struct {
	fd       int
	params   UringParams
	sqHead   *uint32
	sqTail   *uint32
	sqMask   *uint32
	sqArray  *uint32
	cqHead   *uint32
	cqTail   *uint32
	cqMask   *uint32
	cqCQEs   unsafe.Pointer
	sqes     unsafe.Pointer
	sqesMmap []byte
	sqMmap   []byte
	cqMmap   []byte

	mu sync.Mutex
}

// SQE128 layout for uring commands.
// With IORING_SETUP_SQE128, cmd[] starts at offset 48 and is 80 bytes.
type sqe128 struct {
	// Standard SQE fields (first 48 bytes)
	Opcode      uint8
	Flags       uint8
	IoPrio      uint16
	Fd          int32
	Off         uint64 // union: off, addr2, cmd_op (for URING_CMD - cmd_op is lower 32 bits)
	Addr        uint64 // union: addr, splice_off_in
	Len         uint32
	OpcodeFlags uint32 // union: various flags
	UserData    uint64
	BufIndex    uint16 // union: buf_index, buf_group
	Personality uint16
	SpliceFdIn  int32 // union: splice_fd_in, file_index, optlen, addr_len

	// Command area starts at offset 48 and is 80 bytes for SQE128
	Cmd [80]byte
}

const (
	sqe128Size        = 128
	ioringOpUringCmd  = 80     // IORING_OP_URING_CMD
	ioringSetupSQE128 = 1 << 8 // IORING_SETUP_SQE128
	featSingleMmap    = 1 << 0 // IORING_FEAT_SINGLE_MMAP
)

func newControlRing(entries uint32) (*controlRing, error) {
	r := &controlRing{}

	r.params = UringParams{
		Flags: ioringSetupSQE128, // Need SQE128 for URING_CMD
	}

	fd, _, errno := syscall.Syscall(unix.SYS_IO_URING_SETUP, uintptr(entries),
		uintptr(unsafe.Pointer(&r.params)), 0)
	if errno != 0 {
		return nil, fmt.Errorf("io_uring_setup: %w", errno)
	}
	r.fd = int(fd)

	if err := r.mapRings(); err != nil {
		unix.Close(r.fd)
		return nil, err
	}

	return r, nil
}

func (r *controlRing) mapRings() error {
	p := &r.params

	sqSize := uint64(p.SqOff.Array) + uint64(p.SQEntries)*4
	cqSize := uint64(p.CqOff.Cqes) + uint64(p.CQEntries)*uint64(unsafe.Sizeof(UringCQE{}))

	var err error

	// Map SQ ring
	r.sqMmap, err = unix.Mmap(r.fd, IORING_OFF_SQ_RING, int(sqSize),
		unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED|unix.MAP_POPULATE)
	if err != nil {
		return fmt.Errorf("mmap sq ring: %w", err)
	}

	// Map CQ ring (may share with SQ ring in newer kernels)
	if p.Features&featSingleMmap != 0 {
		r.cqMmap = r.sqMmap
	} else {
		r.cqMmap, err = unix.Mmap(r.fd, IORING_OFF_CQ_RING, int(cqSize),
			unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED|unix.MAP_POPULATE)
		if err != nil {
			unix.Munmap(r.sqMmap)
			return fmt.Errorf("mmap cq ring: %w", err)
		}
	}

	// Map SQEs (128 bytes each for SQE128)
	sqeSize := uint64(p.SQEntries) * sqe128Size
	r.sqesMmap, err = unix.Mmap(r.fd, IORING_OFF_SQES, int(sqeSize),
		unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED|unix.MAP_POPULATE)
	if err != nil {
		unix.Munmap(r.sqMmap)
		if r.cqMmap != nil && &r.cqMmap[0] != &r.sqMmap[0] {
			unix.Munmap(r.cqMmap)
		}
		return fmt.Errorf("mmap sqes: %w", err)
	}
	r.sqes = unsafe.Pointer(&r.sqesMmap[0])

	// Setup ring pointers
	base := unsafe.Pointer(&r.sqMmap[0])
	r.sqHead = (*uint32)(unsafe.Add(base, p.SqOff.Head))
	r.sqTail = (*uint32)(unsafe.Add(base, p.SqOff.Tail))
	r.sqMask = (*uint32)(unsafe.Add(base, p.SqOff.RingMask))
	r.sqArray = (*uint32)(unsafe.Add(base, p.SqOff.Array))

	cqBase := unsafe.Pointer(&r.cqMmap[0])
	r.cqHead = (*uint32)(unsafe.Add(cqBase, p.CqOff.Head))
	r.cqTail = (*uint32)(unsafe.Add(cqBase, p.CqOff.Tail))
	r.cqMask = (*uint32)(unsafe.Add(cqBase, p.CqOff.RingMask))
	r.cqCQEs = unsafe.Add(cqBase, p.CqOff.Cqes)

	return nil
}

// close releases resources. Called on error paths and could be exposed for graceful shutdown.
func (r *controlRing) close() { //nolint:unused // kept for future graceful shutdown
	if r.sqesMmap != nil {
		unix.Munmap(r.sqesMmap)
	}
	if r.cqMmap != nil && (r.sqMmap == nil || &r.cqMmap[0] != &r.sqMmap[0]) {
		unix.Munmap(r.cqMmap)
	}
	if r.sqMmap != nil {
		unix.Munmap(r.sqMmap)
	}
	if r.fd > 0 {
		unix.Close(r.fd)
	}
}

func (r *controlRing) getSQE(index uint32) *sqe128 {
	return (*sqe128)(unsafe.Add(r.sqes, uintptr(index)*sqe128Size))
}

// submitCmd submits a control command via io_uring and waits for completion.
func (r *controlRing) submitCmd(ctrlFd int, cmdOp uint32, cmd *UblksrvCtrlCmd) (int32, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	// Get SQ tail
	tail := *r.sqTail
	mask := *r.sqMask
	index := tail & mask

	// Fill SQE
	sqe := r.getSQE(index)
	*sqe = sqe128{} // Zero first
	sqe.Opcode = ioringOpUringCmd
	sqe.Fd = int32(ctrlFd)
	sqe.Off = uint64(cmdOp) // cmd_op is in lower 32 bits of off field

	// Copy command data to the extended area
	cmdBytes := (*[unsafe.Sizeof(UblksrvCtrlCmd{})]byte)(unsafe.Pointer(cmd))[:]
	copy(sqe.Cmd[:], cmdBytes)

	// Update SQ array and tail
	*(*uint32)(unsafe.Add(unsafe.Pointer(r.sqArray), uintptr(index)*4)) = index
	*r.sqTail = tail + 1

	// Submit and wait
	_, _, errno := syscall.Syscall6(
		unix.SYS_IO_URING_ENTER,
		uintptr(r.fd),
		1,                      // to_submit
		1,                      // min_complete
		IORING_ENTER_GETEVENTS, // flags
		0,
		0,
	)
	if errno != 0 {
		return 0, fmt.Errorf("io_uring_enter: %w", errno)
	}

	// Read CQE
	cqHead := *r.cqHead
	cqTail := *r.cqTail
	if cqHead == cqTail {
		return 0, errors.New("no CQE available")
	}

	cqe := (*UringCQE)(unsafe.Add(r.cqCQEs, uintptr(cqHead&*r.cqMask)*unsafe.Sizeof(UringCQE{})))
	res := cqe.Res
	*r.cqHead = cqHead + 1

	if res < 0 {
		return res, fmt.Errorf("command failed: %s", syscall.Errno(-res).Error())
	}

	return res, nil
}

// ctrlRingCache provides a lazily-initialized control ring.
var (
	ctrlRingCache     *controlRing
	ctrlRingCacheOnce sync.Once
	errCtrlRingCache  error
)

func getControlRing() (*controlRing, error) {
	ctrlRingCacheOnce.Do(func() {
		ctrlRingCache, errCtrlRingCache = newControlRing(32)
	})
	return ctrlRingCache, errCtrlRingCache
}

// ctrlCmd executes a control command on the given control file.
func ctrlCmd(ctrl *os.File, cmdOp uint32, cmd *UblksrvCtrlCmd) error {
	ring, err := getControlRing()
	if err != nil {
		return fmt.Errorf("get control ring: %w", err)
	}

	res, err := ring.submitCmd(int(ctrl.Fd()), cmdOp, cmd)
	if err != nil {
		return err
	}
	_ = res // Success
	return nil
}
