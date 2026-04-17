package ublk

import (
	"fmt"
	"os"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"github.com/e2b-dev/ublk-go/ublk/uring"
	"golang.org/x/sys/unix"
)

type Device struct {
	id        int
	ctrlFD    int
	charFD    int
	ctrlRing  *uring.Ring
	info      devInfo
	backend   Backend
	useIoctl  bool
	workers   []*worker
	wg        sync.WaitGroup
	closeOnce sync.Once
}

func openDevice(backend Backend) (*Device, error) {
	fd, err := unix.Open("/dev/ublk-control", unix.O_RDWR, 0)
	if err != nil {
		return nil, fmt.Errorf("open /dev/ublk-control: %w", err)
	}

	ctrlRing, err := uring.NewSQE128(4)
	if err != nil {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("create control ring: %w", err)
	}

	return &Device{
		id:       -1,
		ctrlFD:   fd,
		charFD:   -1,
		ctrlRing: ctrlRing,
		backend:  backend,
	}, nil
}

func (d *Device) addDev(queues, depth uint16, maxIOBuf uint32) error {
	info := devInfo{
		NrHWQueues:    queues,
		QueueDepth:    depth,
		MaxIOBufBytes: maxIOBuf,
		DevID:         ^uint32(0), // must match cmd.DevID (kernel 6.17+)
		Flags:         flagCmdIoctlEncode,
	}

	cmd := ctrlCmd{
		DevID:   ^uint32(0),
		QueueID: ^uint16(0),
		Addr:    uint64(uintptr(unsafe.Pointer(&info))),
		Len:     uint16(sizeofDevInfo),
	}

	err := d.ctrlCommand(uCmdAddDev, &cmd)
	if err != nil {
		info.Flags = 0
		cmd.Addr = uint64(uintptr(unsafe.Pointer(&info)))
		if err2 := d.ctrlCommand(cmdAddDev, &cmd); err2 != nil {
			return fmt.Errorf("ADD_DEV failed: ioctl-encoded: %w; legacy: %w", err, err2)
		}
		d.useIoctl = false
	} else {
		d.useIoctl = true
	}

	d.info = info
	d.id = int(info.DevID)
	return nil
}

func (d *Device) openCharDev() error {
	path := fmt.Sprintf("/dev/ublkc%d", d.id)
	var fd int
	var err error

	for range 50 {
		fd, err = unix.Open(path, unix.O_RDWR|unix.O_CLOEXEC, 0)
		if err == nil {
			d.charFD = fd
			return nil
		}
		if !os.IsNotExist(err) {
			return fmt.Errorf("open %s: %w", path, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	return fmt.Errorf("char device %s not created: %w", path, err)
}

func (d *Device) setParams(size uint64, maxSectors uint32) error {
	params := ublkParams{
		Len:   uint32(unsafe.Sizeof(ublkParams{})),
		Types: paramTypeBasic,
		Basic: paramBasic{
			LogicalBSShift:  9,
			PhysicalBSShift: 9,
			IOOptShift:      9,
			IOMinShift:      9,
			MaxSectors:      maxSectors,
			DevSectors:      size / 512,
		},
	}

	cmd := ctrlCmd{
		DevID:   uint32(d.id),
		QueueID: ^uint16(0),
		Addr:    uint64(uintptr(unsafe.Pointer(&params))),
		Len:     uint16(params.Len),
	}

	return d.ctrlCommand(d.ctrlOp(uCmdSetParams, cmdSetParams), &cmd)
}

func (d *Device) startDev() error {
	cmd := ctrlCmd{
		DevID:   uint32(d.id),
		QueueID: ^uint16(0),
	}
	cmd.Data[0] = uint64(os.Getpid())
	return d.ctrlCommand(d.ctrlOp(uCmdStartDev, cmdStartDev), &cmd)
}

func (d *Device) stopDev() error {
	cmd := ctrlCmd{DevID: uint32(d.id), QueueID: ^uint16(0)}
	return d.ctrlCommand(d.ctrlOp(uCmdStopDev, cmdStopDev), &cmd)
}

func (d *Device) delDev() error {
	cmd := ctrlCmd{DevID: uint32(d.id), QueueID: ^uint16(0)}
	return d.ctrlCommand(d.ctrlOp(uCmdDelDev, cmdDelDev), &cmd)
}

func (d *Device) ctrlCommand(cmdOp uint32, cmd *ctrlCmd) error {
	sqe := d.ctrlRing.GetSQE128()
	if sqe == nil {
		return fmt.Errorf("control ring SQ full")
	}

	sqe.Opcode = uring.OpUringCmd
	sqe.Fd = int32(d.ctrlFD)
	sqe.Off = uint64(cmdOp)

	// Kernel reads ctrl command from sqe->cmd (offset 48), not sqe->addr.
	src := (*[unsafe.Sizeof(ctrlCmd{})]byte)(unsafe.Pointer(cmd))
	copy(sqe.Cmd[:], src[:])

	if _, err := d.ctrlRing.Submit(); err != nil {
		return err
	}

	cqe, err := d.ctrlRing.WaitCQE()
	if err != nil {
		return err
	}
	res := cqe.Res
	d.ctrlRing.SeenCQE()

	if res < 0 {
		return fmt.Errorf("ublk control cmd 0x%x: %w", cmdOp, syscall.Errno(-res))
	}
	return nil
}

func (d *Device) ctrlOp(ioctlCmd, legacyCmd uint32) uint32 {
	if d.useIoctl {
		return ioctlCmd
	}
	return legacyCmd
}

// BlockDevicePath returns the path to the block device (e.g., /dev/ublkb0).
func (d *Device) BlockDevicePath() string {
	if d.id < 0 {
		return ""
	}
	return fmt.Sprintf("/dev/ublkb%d", d.id)
}

// Close stops and removes the ublk device, releasing all resources.
func (d *Device) Close() (retErr error) {
	d.closeOnce.Do(func() {
		retErr = d.shutdown()
	})
	return
}

func (d *Device) shutdown() error {
	// Closing the char fd triggers ublk_ch_release in the kernel, which
	// cancels pending FETCH_REQ and generates -ENODEV CQEs on worker rings.
	if d.charFD >= 0 {
		_ = unix.Close(d.charFD)
		d.charFD = -1
	}

	d.wg.Wait()

	_ = d.stopDev()
	var err error
	if d.id >= 0 {
		err = d.delDev()
	}

	if d.ctrlRing != nil {
		_ = d.ctrlRing.Close()
		d.ctrlRing = nil
	}
	if d.ctrlFD >= 0 {
		_ = unix.Close(d.ctrlFD)
		d.ctrlFD = -1
	}

	return err
}
