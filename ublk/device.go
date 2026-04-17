package ublk

import (
	"fmt"
	"os"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

// Device represents a ublk block device.
type Device struct {
	id        int
	ctrlFD    int
	charFD    int
	ctrlRing  *ring
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

	ctrlRing, err := newCtrlRing(4)
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
		DevID:         ^uint32(0), // kernel 6.17+ requires info.dev_id == header.dev_id
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

func (d *Device) setParams(size uint64, blockSize uint32, maxSectors uint32) error {
	blockShift := trailingZeros32(blockSize)

	var attrs uint32
	if _, ok := d.backend.(Flusher); ok {
		attrs |= attrVolatileCache
	}

	params := ublkParams{
		Len:   uint32(unsafe.Sizeof(ublkParams{})),
		Types: paramTypeBasic,
		Basic: paramBasic{
			Attrs:           attrs,
			LogicalBSShift:  blockShift,
			PhysicalBSShift: blockShift,
			IOOptShift:      blockShift,
			IOMinShift:      blockShift,
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
	cmd := ctrlCmd{
		DevID:   uint32(d.id),
		QueueID: ^uint16(0),
	}
	return d.ctrlCommand(d.ctrlOp(uCmdStopDev, cmdStopDev), &cmd)
}

func (d *Device) delDev() error {
	cmd := ctrlCmd{
		DevID:   uint32(d.id),
		QueueID: ^uint16(0),
	}
	return d.ctrlCommand(d.ctrlOp(uCmdDelDev, cmdDelDev), &cmd)
}

// ctrlCommand submits a control command via io_uring passthrough.
func (d *Device) ctrlCommand(cmdOp uint32, cmd *ctrlCmd) error {
	sqe := d.ctrlRing.getSQE128()
	if sqe == nil {
		return fmt.Errorf("control ring SQ full")
	}

	sqe.Opcode = opUringCmd
	sqe.Fd = int32(d.ctrlFD)
	sqe.Off = uint64(cmdOp)

	// The kernel reads the ctrl command from sqe->cmd, not sqe->addr.
	src := (*[unsafe.Sizeof(ctrlCmd{})]byte)(unsafe.Pointer(cmd))
	copy(sqe.Cmd[:], src[:])

	if _, err := d.ctrlRing.submit(); err != nil {
		return err
	}

	cqe, err := d.ctrlRing.waitCQE()
	if err != nil {
		return err
	}

	res := cqe.Res
	d.ctrlRing.seenCQE()

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

// DeviceID returns the ublk device ID.
func (d *Device) DeviceID() int {
	return d.id
}

// Close stops and removes the ublk device, releasing all resources.
func (d *Device) Close() (retErr error) {
	d.closeOnce.Do(func() {
		retErr = d.shutdown()
	})
	return
}

func (d *Device) shutdown() error {
	// Stop the device; this causes the kernel to complete pending FETCH_REQ
	// with -ENODEV, which makes the workers exit.
	_ = d.stopDev()

	// Wait for all workers to finish.
	d.wg.Wait()

	if d.charFD >= 0 {
		_ = unix.Close(d.charFD)
		d.charFD = -1
	}

	var err error
	if d.id >= 0 {
		err = d.delDev()
	}

	if d.ctrlRing != nil {
		_ = d.ctrlRing.close()
		d.ctrlRing = nil
	}
	if d.ctrlFD >= 0 {
		_ = unix.Close(d.ctrlFD)
		d.ctrlFD = -1
	}

	return err
}

func trailingZeros32(v uint32) uint8 {
	if v == 0 {
		return 0
	}
	var n uint8
	for v&1 == 0 {
		n++
		v >>= 1
	}
	return n
}
