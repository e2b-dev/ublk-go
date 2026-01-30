package ublk

import (
	"errors"
	"fmt"
	"os"
	"sync"
	"syscall"
	"unsafe"
)

var (
	ErrDeviceNotStarted     = errors.New("device not started")
	ErrDeviceAlreadyStarted = errors.New("device already started")
	ErrInvalidParameters    = errors.New("invalid parameters")
	ErrCharDevNotOpen       = errors.New("char device not opened")
)

// Device represents a ublk block device.
// It manages the lifecycle of the device (Add, Start, Stop, Delete)
// and coordinates IO workers.
type Device struct {
	// Immutable fields
	devID     int
	controlFD *os.File // /dev/ublk-control

	// Mutable state protected by mu
	mu        sync.RWMutex
	charDevFD *os.File // /dev/ublkc*
	params    UblkParams
	info      UblksrvCtrlDevInfo
	started   bool

	// IO handlers (immutable after creation)
	readAt  func([]byte, int64) (int, error)
	writeAt func([]byte, int64) (int, error)

	// Worker management
	workers []*ioWorker
	wg      sync.WaitGroup
	stopCh  chan struct{}
}

// NewDevice creates a new ublk device instance.
// It opens the control device but does not create the ublk device yet.
func NewDevice(readAt func([]byte, int64) (int, error), writeAt func([]byte, int64) (int, error)) (*Device, error) {
	controlFD, err := os.OpenFile("/dev/ublk-control", os.O_RDWR, 0)
	if err != nil {
		return nil, fmt.Errorf("failed to open control device: %w", err)
	}

	return &Device{
		controlFD: controlFD,
		readAt:    readAt,
		writeAt:   writeAt,
		stopCh:    make(chan struct{}),
	}, nil
}

// Add registers the device with the kernel (UBLK_CMD_ADD_DEV).
// It opens the character device (/dev/ublkc*) for communication.
func (d *Device) Add(nrHWQueues, queueDepth uint16) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	if d.started {
		return ErrDeviceAlreadyStarted
	}

	info := UblksrvCtrlDevInfo{
		NrHWQueues:    nrHWQueues,
		QueueDepth:    queueDepth,
		MaxIOBufBytes: 512 * 1024, // 512KB default
	}

	if err := d.ioctl(UBLK_CMD_ADD_DEV, uintptr(unsafe.Pointer(&info))); err != nil {
		return fmt.Errorf("failed to add device: %w", err)
	}

	d.info = info
	d.devID = int(info.DevID)

	return d.openCharDevice()
}

func (d *Device) openCharDevice() error {
	charDevPath := fmt.Sprintf("/dev/ublkc%d", d.devID)
	fd, err := os.OpenFile(charDevPath, os.O_RDWR, 0)
	if err != nil {
		return fmt.Errorf("failed to open char device %s: %w", charDevPath, err)
	}
	d.charDevFD = fd
	return nil
}

// SetParams configures the device parameters (UBLK_CMD_SET_PARAMS).
func (d *Device) SetParams(blockSize, size uint64, maxSectors uint32) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	if d.started {
		return ErrDeviceAlreadyStarted
	}

	params := UblkParams{}
	params.Basic.LogicalBSize = uint32(blockSize)
	params.Basic.PhysicalBSize = uint32(blockSize)
	params.Basic.IOOptBSize = uint32(blockSize)
	params.Basic.MaxSectors = maxSectors
	params.Basic.DevSectors = size / blockSize
	params.IO.QueueDepth = d.info.QueueDepth
	params.IO.NrHWQueues = d.info.NrHWQueues

	if err := d.ioctl(UBLK_CMD_SET_PARAMS, uintptr(unsafe.Pointer(&params))); err != nil {
		return fmt.Errorf("failed to set params: %w", err)
	}

	d.params = params
	return nil
}

// Start activates the device (UBLK_CMD_START_DEV) and starts IO workers.
func (d *Device) Start() error {
	d.mu.Lock()
	defer d.mu.Unlock()

	if d.started {
		return ErrDeviceAlreadyStarted
	}
	if d.charDevFD == nil {
		return ErrCharDevNotOpen
	}

	if err := d.ioctl(UBLK_CMD_START_DEV, 0); err != nil {
		return fmt.Errorf("failed to start device: %w", err)
	}

	d.started = true
	d.startWorkers()
	return nil
}

func (d *Device) startWorkers() {
	d.workers = make([]*ioWorker, d.info.NrHWQueues)
	for i := uint16(0); i < d.info.NrHWQueues; i++ {
		worker := newIOWorker(d, i, d.info.QueueDepth)
		d.workers[i] = worker
		d.wg.Add(1)
		go worker.run()
	}
}

// Stop deactivates the device (UBLK_CMD_STOP_DEV) and stops IO workers.
func (d *Device) Stop() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.stopLocked()
}

// stopLocked performs the actual stop logic. Caller must hold d.mu.
func (d *Device) stopLocked() error {
	if !d.started {
		return nil
	}

	// 1. Tell kernel to stop sending requests
	// This cancels pending IOs and unblocks workers waiting on ring
	if err := d.ioctl(UBLK_CMD_STOP_DEV, 0); err != nil {
		logf("Warning: UBLK_CMD_STOP_DEV failed: %v", err)
	}

	// 2. Signal workers to stop (cleanup)
	close(d.stopCh)

	// 3. Wait for workers to exit
	d.wg.Wait()

	// Reset for potential restart
	d.stopCh = make(chan struct{})
	d.workers = nil
	d.started = false

	return nil
}

// Delete removes the device (UBLK_CMD_DEL_DEV) and closes file descriptors.
func (d *Device) Delete() error {
	d.mu.Lock()
	defer d.mu.Unlock()

	if d.started {
		// Try to stop gracefully, ignore errors as we are deleting
		_ = d.stopLocked()
	}

	if d.charDevFD != nil {
		d.charDevFD.Close()
		d.charDevFD = nil
	}

	if err := d.ioctl(UBLK_CMD_DEL_DEV, 0); err != nil {
		return fmt.Errorf("failed to delete device: %w", err)
	}

	if d.controlFD != nil {
		d.controlFD.Close()
		d.controlFD = nil
	}

	return nil
}

// BlockDevicePath returns the path to the block device (e.g., /dev/ublkb0).
func (d *Device) BlockDevicePath() string {
	return fmt.Sprintf("/dev/ublkb%d", d.devID)
}

// DeviceID returns the ublk device ID.
func (d *Device) DeviceID() int {
	return d.devID
}

func (d *Device) ioctl(cmd uintptr, arg uintptr) error {
	if d.controlFD == nil {
		return errors.New("control device closed")
	}
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, d.controlFD.Fd(), cmd, arg)
	if errno != 0 {
		return errno
	}
	return nil
}
