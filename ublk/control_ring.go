package ublk

import (
	"fmt"
	"os"
	"sync"
	"syscall"
	"unsafe"
)

type controlRing struct {
	ring *Ring
	mu   sync.Mutex
}

func newControlRing(entries uint32) (*controlRing, error) {
	ring, err := NewRingWithOptions(uint(entries), 0, WithSQE128())
	if err != nil {
		return nil, fmt.Errorf("failed to create control ring: %w", err)
	}

	return &controlRing{
		ring: ring,
	}, nil
}

func (r *controlRing) submitCmd(ctrlFd int, cmdOp uint32, cmd *UblksrvCtrlCmd) (int32, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	sqe, err := r.ring.GetSQE128()
	if err != nil {
		return 0, fmt.Errorf("GetSQE128 failed: %w", err)
	}

	sqe.Opcode = IORING_OP_URING_CMD
	sqe.Fd = int32(ctrlFd)
	sqe.Off = uint64(cmdOp)
	sqe.Addr = uint64(uintptr(unsafe.Pointer(cmd))) // Point to cmd in userspace

	if _, err := r.ring.Submit(); err != nil {
		return 0, fmt.Errorf("submit failed: %w", err)
	}

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

func ctrlCmd(ctrl *os.File, cmdOp uint32, cmd *UblksrvCtrlCmd) error {
	ring, err := getControlRing()
	if err != nil {
		return fmt.Errorf("get control ring: %w", err)
	}
	res, err := ring.submitCmd(int(ctrl.Fd()), cmdOp, cmd)
	fmt.Printf("[DEBUG] ctrlCmd op=0x%x res=%d err=%v\n", cmdOp, res, err)
	return err
}
