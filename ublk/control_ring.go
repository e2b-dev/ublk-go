package ublk

import (
	"fmt"
	"os"
	"sync"
	"syscall"
	"unsafe"
)

// controlRing is an io_uring instance dedicated to control commands.
// ublk control commands must be submitted via io_uring URING_CMD, not ioctl.
type controlRing struct {
	ring *Ring
	mu   sync.Mutex
}

const (
	ioringOpUringCmd = 80 // IORING_OP_URING_CMD
)

func newControlRing(entries uint32) (*controlRing, error) {
	// Initialize ring with SQE128 support
	ring, err := NewRingWithOptions(uint(entries), 0, WithSQE128())
	if err != nil {
		return nil, fmt.Errorf("failed to create control ring: %w", err)
	}

	return &controlRing{
		ring: ring,
	}, nil
}

// submitCmd submits a control command via io_uring and waits for completion.
func (r *controlRing) submitCmd(ctrlFd int, cmdOp uint32, cmd *UblksrvCtrlCmd) (int32, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	// getSQE128
	sqe, err := r.ring.GetSQE128()
	if err != nil {
		return 0, fmt.Errorf("GetSQE128 failed: %w", err)
	}

	sqe.Opcode = ioringOpUringCmd
	sqe.Fd = int32(ctrlFd)
	sqe.Off = uint64(cmdOp) // cmd_op is in lower 32 bits of off field

	// Copy command data to the extended area
	cmdBytes := (*[unsafe.Sizeof(UblksrvCtrlCmd{})]byte)(unsafe.Pointer(cmd))[:]
	copy(sqe.Cmd[:], cmdBytes)

	// Submit
	if _, err := r.ring.Submit(); err != nil {
		return 0, fmt.Errorf("submit failed: %w", err)
	}

	// Wait for completion
	cqe, err := r.ring.WaitCQE()
	if err != nil {
		return 0, fmt.Errorf("WaitCQE failed: %w", err)
	}

	res := cqe.Res
	r.ring.SeenCQE(cqe)

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
