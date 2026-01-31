package ublk

import (
	"errors"
	"fmt"
	"math/bits"
	"os"
	"sync"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

var (
	ErrDeviceNotStarted     = errors.New("device not started")
	ErrDeviceAlreadyStarted = errors.New("device already started")
	ErrInvalidParameters    = errors.New("invalid parameters")
	ErrCharDevNotOpen       = errors.New("char device not opened")
	ErrInvalidRequest       = errors.New("invalid request")

	controlDevicePath = "/dev/ublk-control" // overridable for tests
)

type Device struct {
	devID     int
	controlFD *os.File

	mu        sync.RWMutex
	charDevFD *os.File
	params    UblkParams
	info      UblksrvCtrlDevInfo
	added     bool
	started   bool
	stopped   bool

	maxIOBufBytes uint32
	backend       Backend
	readAt        func([]byte, int64) (int, error)
	writeAt       func([]byte, int64) (int, error)

	flags         uint64
	cow           bool
	ioctlEncoding bool

	workers []*ioWorker
	wg      sync.WaitGroup
	stopCh  chan struct{}
}

type DeviceOption func(*Device)

func WithZeroCopy() DeviceOption {
	return func(d *Device) {
		d.flags |= UBLK_F_SUPPORT_ZERO_COPY
	}
}

func WithAutoBufReg() DeviceOption {
	return func(d *Device) {
		d.flags |= UBLK_F_AUTO_BUF_REG | UBLK_F_SUPPORT_ZERO_COPY
	}
}

func WithUserCopy() DeviceOption {
	return func(d *Device) {
		d.flags |= UBLK_F_USER_COPY
	}
}

func WithMaxIOBufBytes(size uint32) DeviceOption {
	return func(d *Device) {
		d.maxIOBufBytes = size
	}
}

func WithCOW() DeviceOption {
	return func(d *Device) {
		d.cow = true
	}
}

func WithUnprivileged() DeviceOption {
	return func(d *Device) {
		d.flags |= UBLK_F_UNPRIVILEGED_DEV | UBLK_F_USER_COPY
	}
}

func NewDeviceWithBackend(backend Backend, opts ...DeviceOption) (*Device, error) {
	controlFD, err := os.OpenFile(controlDevicePath, os.O_RDWR, 0)
	if err != nil {
		return nil, fmt.Errorf("failed to open control device: %w", err)
	}

	d := &Device{
		devID:     -1,
		controlFD: controlFD,
		backend:   backend,
		readAt:    backend.ReadAt,
		writeAt:   backend.WriteAt,
		flags:     UBLK_F_CMD_IOCTL_ENCODE,
		stopCh:    make(chan struct{}),
	}

	for _, opt := range opts {
		opt(d)
	}

	return d, nil
}

func (d *Device) Add(nrHWQueues, queueDepth uint16) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	if d.started {
		return ErrDeviceAlreadyStarted
	}
	if d.flags&UBLK_F_SUPPORT_ZERO_COPY != 0 && d.flags&UBLK_F_USER_COPY != 0 {
		return errors.New("ZeroCopy cannot be used with UserCopy")
	}
	if d.flags&UBLK_F_SUPPORT_ZERO_COPY != 0 {
		if d.backend == nil {
			return errors.New("ZeroCopy requires a backend that implements FixedFileBackend")
		}
		if _, ok := d.backend.(FixedFileBackend); !ok {
			return errors.New("ZeroCopy requires a backend that implements FixedFileBackend")
		}
	}

	maxIOBufBytes := d.maxIOBufBytes
	if maxIOBufBytes == 0 {
		maxIOBufBytes = 512 * 1024
	}

	info := UblksrvCtrlDevInfo{
		NrHWQueues:    nrHWQueues,
		QueueDepth:    queueDepth,
		MaxIOBufBytes: maxIOBufBytes,
		Flags:         d.flags | UBLK_F_CMD_IOCTL_ENCODE,
	}

	cmd := UblksrvCtrlCmd{
		DevID:   ^uint32(0), // -1 = allocate new
		QueueID: ^uint16(0),
		Addr:    uint64(uintptr(unsafe.Pointer(&info))),
		Len:     uint16(SizeOfCtrlDevInfo),
	}

	fmt.Printf("[DEBUG] Before ADD_DEV: info.DevID=%d, cmd.DevID=%d\n", info.DevID, cmd.DevID)
	
	err := d.ctrlCommand(uint32(UBLK_U_CMD_ADD_DEV), &cmd)
	if err != nil {
		info.Flags = d.flags
		cmd.Addr = uint64(uintptr(unsafe.Pointer(&info)))
		err = d.ctrlCommand(UBLK_CMD_ADD_DEV, &cmd)
		if err != nil {
			return fmt.Errorf("failed to add device: %w", err)
		}
		d.ioctlEncoding = false
	} else {
		d.flags |= UBLK_F_CMD_IOCTL_ENCODE
		d.ioctlEncoding = true
	}

	fmt.Printf("[DEBUG] After ADD_DEV: info.DevID=%d, info.State=%d\n", info.DevID, info.State)
	
	d.info = info
	d.devID = int(info.DevID)
	d.added = true

	fmt.Printf("[DEBUG] ADD_DEV succeeded: devID=%d, ioctlEncoding=%v, flags=0x%x\n", d.devID, d.ioctlEncoding, d.flags)

	return d.openCharDevice()
}

func (d *Device) Flags() uint64       { return d.flags }
func (d *Device) HasZeroCopy() bool   { return d.flags&UBLK_F_SUPPORT_ZERO_COPY != 0 }
func (d *Device) HasAutoBufReg() bool { return d.flags&UBLK_F_AUTO_BUF_REG != 0 }
func (d *Device) HasUserCopy() bool   { return d.flags&UBLK_F_USER_COPY != 0 }

func (d *Device) openCharDevice() error {
	charDevPath := fmt.Sprintf("/dev/ublkc%d", d.devID)
	var fd *os.File
	var err error
	for range 50 { // wait up to 500ms for udev
		fd, err = os.OpenFile(charDevPath, os.O_RDWR, 0)
		if err == nil {
			break
		}
		if !os.IsNotExist(err) {
			return fmt.Errorf("failed to open char device %s: %w", charDevPath, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err != nil {
		return fmt.Errorf("char device %s not created by udev: %w", charDevPath, err)
	}
	d.charDevFD = fd
	return nil
}

func (d *Device) SetParams(blockSize, size uint64, maxSectors uint32, discardSectors, discardSegs uint32) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	if d.started {
		return ErrDeviceAlreadyStarted
	}
	if blockSize == 0 || blockSize&(blockSize-1) != 0 {
		return ErrInvalidParameters
	}
	if size == 0 || size%blockSize != 0 {
		return ErrInvalidParameters
	}
	if maxSectors == 0 {
		return ErrInvalidParameters
	}

	params := UblkParams{}
	blockShift := uint8(bits.TrailingZeros64(blockSize))
	params.Len = uint32(unsafe.Sizeof(params))
	params.Types = UBLK_PARAM_TYPE_BASIC
	params.Basic.LogicalBSShift = blockShift
	params.Basic.PhysicalBSShift = blockShift
	params.Basic.IOOptShift = blockShift
	params.Basic.IOMinShift = blockShift
	params.Basic.MaxSectors = maxSectors
	params.Basic.DevSectors = size / blockSize

	if discardSectors > 0 {
		params.Types |= UBLK_PARAM_TYPE_DISCARD
		params.Discard.DiscardAlignment = uint32(blockSize)
		params.Discard.DiscardGranularity = uint32(blockSize)
		params.Discard.MaxDiscardSectors = discardSectors
		params.Discard.MaxWriteZeroesSectors = discardSectors
		if discardSegs == 0 {
			discardSegs = 1
		}
		params.Discard.MaxDiscardSegments = uint16(discardSegs)
	}

	cmd := UblksrvCtrlCmd{
		DevID:   uint32(d.devID),
		QueueID: ^uint16(0), // -1 for non-queue commands
		Addr:    uint64(uintptr(unsafe.Pointer(&params))),
		Len:     uint16(params.Len),
	}

	if err := d.ctrlCommand(d.cmdOpFor(UBLK_U_CMD_SET_PARAMS), &cmd); err != nil {
		return fmt.Errorf("failed to set params: %w (requires CAP_SYS_ADMIN)", err)
	}

	d.params = params
	return nil
}

func (d *Device) Start() error {
	d.mu.Lock()
	defer d.mu.Unlock()

	if d.started {
		return ErrDeviceAlreadyStarted
	}
	if d.charDevFD == nil {
		return ErrCharDevNotOpen
	}

	cmd := UblksrvCtrlCmd{
		DevID:   uint32(d.devID),
		QueueID: ^uint16(0), // -1 for non-queue commands
	}

	if err := d.ctrlCommand(d.cmdOpFor(UBLK_U_CMD_START_DEV), &cmd); err != nil {
		return fmt.Errorf("failed to start device: %w", err)
	}

	d.started = true
	if err := d.startWorkers(); err != nil {
		stopErr := d.ctrlCommand(d.cmdOpFor(UBLK_U_CMD_STOP_DEV), &cmd)
		d.started = false
		if stopErr != nil {
			return errors.Join(err, fmt.Errorf("failed to stop device after worker init error: %w", stopErr))
		}
		return err
	}
	return nil
}

func (d *Device) startWorkers() error {
	d.workers = make([]*ioWorker, d.info.NrHWQueues)
	for i := range int(d.info.NrHWQueues) {
		worker := newIOWorker(d, uint16(i), d.info.QueueDepth)
		if err := worker.Init(); err != nil {
			for j := range i {
				if d.workers[j] != nil {
					d.workers[j].Close()
				}
			}
			return fmt.Errorf("worker %d init failed: %w", i, err)
		}
		d.workers[i] = worker
	}
	for _, worker := range d.workers {
		d.wg.Add(1)
		go worker.Loop()
	}

	return nil
}

func (d *Device) Stop() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.stopLocked()
}

func (d *Device) stopLocked() error { // caller must hold d.mu
	if !d.started || d.stopped {
		return nil
	}

	cmd := UblksrvCtrlCmd{
		DevID:   uint32(d.devID),
		QueueID: ^uint16(0),
	}
	var stopErr error
	if err := d.ctrlCommand(d.cmdOpFor(UBLK_U_CMD_STOP_DEV), &cmd); err != nil {
		stopErr = fmt.Errorf("UBLK_U_CMD_STOP_DEV failed: %w", err)
	}

	if !d.stopped {
		close(d.stopCh)
		d.stopped = true
	}

	d.wg.Wait()

	d.stopCh = make(chan struct{})
	d.stopped = false
	d.workers = nil
	d.started = false

	return stopErr
}

func (d *Device) Delete() error {
	d.mu.Lock()
	defer d.mu.Unlock()

	if d.started {
		_ = d.stopLocked()
	}

	if d.charDevFD != nil {
		_ = d.charDevFD.Close()
		d.charDevFD = nil
	}

	if d.added && d.devID >= 0 {
		cmd := UblksrvCtrlCmd{
			DevID:   uint32(d.devID),
			QueueID: ^uint16(0),
		}
		if err := d.ctrlCommand(d.cmdOpFor(UBLK_U_CMD_DEL_DEV), &cmd); err != nil {
			return fmt.Errorf("failed to delete device: %w", err)
		}
		d.added = false
		d.devID = -1
	}

	if d.controlFD != nil {
		_ = d.controlFD.Close()
		d.controlFD = nil
	}

	return nil
}

func (d *Device) BlockDevicePath() string {
	if d.devID < 0 {
		return ""
	}
	return fmt.Sprintf("/dev/ublkb%d", d.devID)
}

func (d *Device) DeviceID() int {
	return d.devID
}

func (d *Device) blockSize() uint64 {
	shift := d.params.Basic.LogicalBSShift
	if shift == 0 {
		return 512
	}
	return 1 << shift
}

// Sync opens the block device, calls fsync(), triggering FLUSH to backend.
func (d *Device) Sync() error {
	d.mu.RLock()
	if !d.started {
		d.mu.RUnlock()
		return ErrDeviceNotStarted
	}
	devPath := d.BlockDevicePath()
	d.mu.RUnlock()

	fd, err := unix.Open(devPath, unix.O_RDONLY, 0)
	if err != nil {
		return fmt.Errorf("failed to open block device for sync: %w", err)
	}
	defer func() { _ = unix.Close(fd) }()

	if err := unix.Fsync(fd); err != nil {
		return fmt.Errorf("fsync failed: %w", err)
	}

	return nil
}

const ioctlBLKFLSBUF = 0x1261

// FlushBuffers clears kernel page cache for this device (BLKFLSBUF ioctl).
// Unlike Sync(), this does NOT send FLUSH to the backend.
func (d *Device) FlushBuffers() error {
	d.mu.RLock()
	if !d.started {
		d.mu.RUnlock()
		return ErrDeviceNotStarted
	}
	devPath := d.BlockDevicePath()
	d.mu.RUnlock()

	fd, err := unix.Open(devPath, unix.O_RDONLY, 0)
	if err != nil {
		return fmt.Errorf("failed to open block device for buffer flush: %w", err)
	}
	defer func() { _ = unix.Close(fd) }()

	if _, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), ioctlBLKFLSBUF, 0); errno != 0 {
		return fmt.Errorf("BLKFLSBUF ioctl failed: %w", errno)
	}

	return nil
}

func (d *Device) ctrlCommand(cmdOp uint32, cmd *UblksrvCtrlCmd) error {
	if d.controlFD == nil {
		return errors.New("control device closed")
	}
	return ctrlCmd(d.controlFD, cmdOp, cmd)
}

func (d *Device) cmdOpFor(ioctlEncodedCmd uintptr) uint32 {
	if d.ioctlEncoding {
		return uint32(ioctlEncodedCmd)
	}
	switch ioctlEncodedCmd {
	case UBLK_U_CMD_SET_PARAMS:
		return UBLK_CMD_SET_PARAMS
	case UBLK_U_CMD_GET_PARAMS:
		return UBLK_CMD_GET_PARAMS
	case UBLK_U_CMD_START_DEV:
		return UBLK_CMD_START_DEV
	case UBLK_U_CMD_STOP_DEV:
		return UBLK_CMD_STOP_DEV
	case UBLK_U_CMD_DEL_DEV:
		return UBLK_CMD_DEL_DEV
	default:
		return uint32(ioctlEncodedCmd)
	}
}
