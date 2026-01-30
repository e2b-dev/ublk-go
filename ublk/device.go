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
	stopped   bool // prevents double-close of stopCh

	// Backend reference for extended operations (Flush, Discard, etc.)
	backend Backend

	// IO handlers (immutable after creation)
	readAt  func([]byte, int64) (int, error)
	writeAt func([]byte, int64) (int, error)

	// Feature flags
	flags uint32

	// Worker management
	workers []*ioWorker
	wg      sync.WaitGroup
	stopCh  chan struct{}
}

// DeviceOption configures device creation.
type DeviceOption func(*Device)

// WithZeroCopy enables zero-copy support (requires CAP_SYS_ADMIN).
// This enables UBLK_F_SUPPORT_ZERO_COPY which allows the ublk server
// to register IO buffers and avoid data copying.
func WithZeroCopy() DeviceOption {
	return func(d *Device) {
		d.flags |= UBLK_F_SUPPORT_ZERO_COPY
	}
}

// WithAutoBufReg enables automatic buffer registration (kernel 6.x+).
// This simplifies zero-copy by automatically registering and unregistering
// buffers, eliminating manual REGISTER_IO_BUF/UNREGISTER_IO_BUF commands.
func WithAutoBufReg() DeviceOption {
	return func(d *Device) {
		d.flags |= UBLK_F_AUTO_BUF_REG | UBLK_F_SUPPORT_ZERO_COPY
	}
}

// WithUserRecovery enables user-space recovery on ublk server crash.
// The device survives server restarts without losing the block device.
func WithUserRecovery() DeviceOption {
	return func(d *Device) {
		d.flags |= UBLK_F_USER_RECOVERY
	}
}

// WithUnprivileged enables unprivileged device control (container-aware).
func WithUnprivileged() DeviceOption {
	return func(d *Device) {
		d.flags |= UBLK_F_UNPRIVILEGED_DEV
	}
}

// WithUserCopy enables user-copy mode (requires CAP_SYS_ADMIN).
// In this mode, the ublk server uses pread()/pwrite() on the character device
// to transfer data instead of using the mmap buffer. This allows:
//   - Skipping data transfer entirely for operations that don't need it (FLUSH, DISCARD)
//   - Copying directly to/from application buffers without intermediate copies
//   - More control over when and if data is transferred
//
// For WRITE requests: call pread(chardev, buf, len, tag*bufsize) to get data.
// For READ requests: call pwrite(chardev, buf, len, tag*bufsize) to send data.
// For FLUSH/DISCARD: just complete the request, no pread/pwrite needed.
func WithUserCopy() DeviceOption {
	return func(d *Device) {
		d.flags |= UBLK_F_USER_COPY
	}
}

// NewDevice creates a new ublk device instance.
// It opens the control device but does not create the ublk device yet.
//
// Deprecated: Use NewDeviceWithBackend for full functionality including Flush/Discard.
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

// NewDeviceWithBackend creates a new ublk device instance with a Backend.
// This allows support for extended operations like Flush and Discard.
// Optional DeviceOption arguments can be passed to configure features like
// zero-copy, user recovery, and unprivileged mode.
func NewDeviceWithBackend(backend Backend, opts ...DeviceOption) (*Device, error) {
	controlFD, err := os.OpenFile("/dev/ublk-control", os.O_RDWR, 0)
	if err != nil {
		return nil, fmt.Errorf("failed to open control device: %w", err)
	}

	d := &Device{
		controlFD: controlFD,
		backend:   backend,
		readAt:    backend.ReadAt,
		writeAt:   backend.WriteAt,
		stopCh:    make(chan struct{}),
	}

	// Apply options
	for _, opt := range opts {
		opt(d)
	}

	return d, nil
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
		Flags:         d.flags,    // Apply configured feature flags
	}

	if err := d.ioctl(UBLK_CMD_ADD_DEV, uintptr(unsafe.Pointer(&info))); err != nil {
		return fmt.Errorf("failed to add device: %w", err)
	}

	d.info = info
	d.devID = int(info.DevID)

	return d.openCharDevice()
}

// Flags returns the configured device feature flags.
func (d *Device) Flags() uint32 {
	return d.flags
}

// HasZeroCopy returns true if zero-copy support is enabled.
func (d *Device) HasZeroCopy() bool {
	return d.flags&UBLK_F_SUPPORT_ZERO_COPY != 0
}

// HasAutoBufReg returns true if automatic buffer registration is enabled.
func (d *Device) HasAutoBufReg() bool {
	return d.flags&UBLK_F_AUTO_BUF_REG != 0
}

// HasUserCopy returns true if user-copy mode is enabled.
func (d *Device) HasUserCopy() bool {
	return d.flags&UBLK_F_USER_COPY != 0
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
	for i := range d.info.NrHWQueues {
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
	if !d.started || d.stopped {
		return nil
	}

	// 1. Tell kernel to stop sending requests
	// This cancels pending IOs and unblocks workers waiting on ring
	var stopErr error
	if err := d.ioctl(UBLK_CMD_STOP_DEV, 0); err != nil {
		stopErr = fmt.Errorf("UBLK_CMD_STOP_DEV failed: %w", err)
	}

	// 2. Signal workers to stop (only once)
	if !d.stopped {
		close(d.stopCh)
		d.stopped = true
	}

	// 3. Wait for workers to exit
	d.wg.Wait()

	// Reset for potential restart
	d.stopCh = make(chan struct{})
	d.stopped = false
	d.workers = nil
	d.started = false

	return stopErr
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
