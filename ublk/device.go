package ublk

import (
	"errors"
	"fmt"
	"log"
	"math/bits"
	"os"
	"sync"
	"unsafe"

	"golang.org/x/sys/unix"
)

var (
	ErrDeviceNotStarted     = errors.New("device not started")
	ErrDeviceAlreadyStarted = errors.New("device already started")
	ErrInvalidParameters    = errors.New("invalid parameters")
	ErrCharDevNotOpen       = errors.New("char device not opened")
	ErrInvalidRequest       = errors.New("invalid request")

	// controlDevicePath is the path to the ublk control device.
	// It is a variable to allow overriding in tests.
	controlDevicePath = "/dev/ublk-control"
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
	added     bool
	started   bool
	stopped   bool // prevents double-close of stopCh

	// Configurable I/O buffer size per request
	maxIOBufBytes uint32

	// Backend reference for extended operations (Flush, Discard, etc.)
	backend Backend

	// IO handlers (immutable after creation)
	readAt  func([]byte, int64) (int, error)
	writeAt func([]byte, int64) (int, error)

	// Feature flags
	flags        uint64
	cow          bool // COW mode: zero-copy overlay + user-copy base
	disableStats bool // Skip statistics recording

	// Statistics (only updated if !disableStats)
	stats Stats

	// Worker management
	workers []*ioWorker
	wg      sync.WaitGroup
	stopCh  chan struct{}
}

// Stats returns the device's IO statistics.
// Returns nil if stats collection is disabled.
func (d *Device) Stats() *Stats {
	if d.disableStats {
		return nil
	}
	return &d.stats
}

// recordStats records an operation if stats collection is enabled.
func (d *Device) recordStats(op uint8, bytes uint64, success bool) {
	if !d.disableStats {
		d.stats.recordOp(op, bytes, success)
	}
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

// WithAutoBufReg enables automatic buffer registration (requires kernel support).
// This simplifies zero-copy by having the kernel manage buffer registration.
func WithAutoBufReg() DeviceOption {
	return func(d *Device) {
		d.flags |= UBLK_F_AUTO_BUF_REG | UBLK_F_SUPPORT_ZERO_COPY
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

// WithMaxIOBufBytes sets the maximum IO buffer size (per request) in bytes.
func WithMaxIOBufBytes(size uint32) DeviceOption {
	return func(d *Device) {
		d.maxIOBufBytes = size
	}
}

// WithCOW enables copy-on-write mode.
// Uses zero-copy for overlay I/O and user-copy for base reads.
// Requires a backend that implements COWBackend.
func WithCOW() DeviceOption {
	return func(d *Device) {
		d.cow = true
	}
}

// WithDisableStats disables per-operation statistics tracking.
// This eliminates atomic counter overhead on every I/O operation.
func WithDisableStats() DeviceOption {
	return func(d *Device) {
		d.disableStats = true
	}
}

// NewDevice creates a new ublk device instance.
// It opens the control device but does not create the ublk device yet.
//
// Deprecated: Use NewDeviceWithBackend for full functionality including Flush/Discard.
func NewDevice(readAt func([]byte, int64) (int, error), writeAt func([]byte, int64) (int, error)) (*Device, error) {
	controlFD, err := os.OpenFile(controlDevicePath, os.O_RDWR, 0)
	if err != nil {
		return nil, fmt.Errorf("failed to open control device: %w", err)
	}

	return &Device{
		devID:     -1,
		controlFD: controlFD,
		readAt:    readAt,
		writeAt:   writeAt,
		flags:     UBLK_F_CMD_IOCTL_ENCODE,
		stopCh:    make(chan struct{}),
	}, nil
}

// NewDeviceWithBackend creates a new ublk device instance with a Backend.
// This allows support for extended operations like Flush and Discard.
// Optional DeviceOption arguments can be passed to configure features like
// zero-copy, user recovery, and unprivileged mode.
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

	// Apply options
	for _, opt := range opts {
		opt(d)
	}

	return d, nil
}

// Add registers the device with the kernel (UBLK_U_CMD_ADD_DEV).
// It opens the character device (/dev/ublkc*) for communication.
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

	// Prepare device info
	d.flags |= UBLK_F_CMD_IOCTL_ENCODE
	maxIOBufBytes := d.maxIOBufBytes
	if maxIOBufBytes == 0 {
		maxIOBufBytes = 512 * 1024
	}
	info := UblksrvCtrlDevInfo{
		NrHWQueues:    nrHWQueues,
		QueueDepth:    queueDepth,
		MaxIOBufBytes: maxIOBufBytes,
		Flags:         d.flags,
	}

	// Wrap in control command structure
	cmd := UblksrvCtrlCmd{
		DevID:   ^uint32(0), // -1 means allocate new device ID
		QueueID: ^uint16(0), // -1 for non-queue commands
		Addr:    uint64(uintptr(unsafe.Pointer(&info))),
		Len:     uint16(SizeOfCtrlDevInfo()),
	}

	// Use ioctl-encoded command (required when CONFIG_BLKDEV_UBLK_LEGACY_OPCODES is not set)
	if err := d.ctrlCommand(uint32(UBLK_U_CMD_ADD_DEV), &cmd); err != nil {
		return fmt.Errorf("failed to add device: %w", err)
	}

	d.info = info
	d.devID = int(info.DevID)
	d.added = true

	return d.openCharDevice()
}

// Flags returns the configured device feature flags.
func (d *Device) Flags() uint64 {
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

// SetParams configures the device parameters (UBLK_U_CMD_SET_PARAMS).
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

	if err := d.ctrlCommand(uint32(UBLK_U_CMD_SET_PARAMS), &cmd); err != nil {
		return fmt.Errorf("failed to set params: %w (requires CAP_SYS_ADMIN)", err)
	}

	d.params = params
	return nil
}

// Start activates the device (UBLK_U_CMD_START_DEV) and starts IO workers.
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

	if err := d.ctrlCommand(uint32(UBLK_U_CMD_START_DEV), &cmd); err != nil {
		return fmt.Errorf("failed to start device: %w", err)
	}

	d.started = true
	if err := d.startWorkers(); err != nil {
		stopErr := d.ctrlCommand(uint32(UBLK_U_CMD_STOP_DEV), &cmd)
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

	// Initialize all workers synchronously
	for i := range int(d.info.NrHWQueues) {
		worker := newIOWorker(d, uint16(i), d.info.QueueDepth)
		if err := worker.Init(); err != nil {
			// Cleanup already initialized workers
			for j := range i {
				if d.workers[j] != nil {
					d.workers[j].Close()
				}
			}
			return fmt.Errorf("worker %d init failed: %w", i, err)
		}
		d.workers[i] = worker
	}

	// Start event loops
	for _, worker := range d.workers {
		d.wg.Add(1)
		go worker.Loop()
	}

	return nil
}

// Stop deactivates the device (UBLK_U_CMD_STOP_DEV) and stops IO workers.
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
	cmd := UblksrvCtrlCmd{
		DevID:   uint32(d.devID),
		QueueID: ^uint16(0), // -1 for non-queue commands
	}
	var stopErr error
	if err := d.ctrlCommand(uint32(UBLK_U_CMD_STOP_DEV), &cmd); err != nil {
		stopErr = fmt.Errorf("UBLK_U_CMD_STOP_DEV failed: %w", err)
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

// Delete removes the device (UBLK_U_CMD_DEL_DEV) and closes file descriptors.
func (d *Device) Delete() error {
	d.mu.Lock()
	defer d.mu.Unlock()

	if d.started {
		// Try to stop gracefully, ignore errors as we are deleting
		_ = d.stopLocked()
	}

	if d.charDevFD != nil {
		if err := d.charDevFD.Close(); err != nil {
			log.Printf("Device %d: char dev close error: %v", d.devID, err)
		}
		d.charDevFD = nil
	}

	if d.added && d.devID >= 0 {
		cmd := UblksrvCtrlCmd{
			DevID:   uint32(d.devID),
			QueueID: ^uint16(0), // -1 for non-queue commands
		}
		if err := d.ctrlCommand(uint32(UBLK_U_CMD_DEL_DEV), &cmd); err != nil {
			return fmt.Errorf("failed to delete device: %w", err)
		}
		d.added = false
		d.devID = -1
	}

	if d.controlFD != nil {
		if err := d.controlFD.Close(); err != nil {
			log.Printf("Device %d: control fd close error: %v", d.devID, err)
		}
		d.controlFD = nil
	}

	return nil
}

// BlockDevicePath returns the path to the block device (e.g., /dev/ublkb0).
func (d *Device) BlockDevicePath() string {
	if d.devID < 0 {
		return ""
	}
	return fmt.Sprintf("/dev/ublkb%d", d.devID)
}

// DeviceID returns the ublk device ID.
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

// Sync flushes all cached data to the backend and waits for acknowledgment.
// This opens the block device, calls fsync(), and waits for completion.
// When fsync is called, the kernel sends FLUSH requests through ublk,
// which are handled by calling the backend's Flush() method (if implemented).
//
// This is a synchronous operation - when it returns successfully, all data
// that was written before the call is guaranteed to be persisted to the
// backend's stable storage.
//
// Returns an error if the device is not started or if fsync fails.
func (d *Device) Sync() error {
	d.mu.RLock()
	if !d.started {
		d.mu.RUnlock()
		return ErrDeviceNotStarted
	}
	devPath := d.BlockDevicePath()
	d.mu.RUnlock()

	// Open the block device
	fd, err := unix.Open(devPath, unix.O_RDONLY, 0)
	if err != nil {
		return fmt.Errorf("failed to open block device for sync: %w", err)
	}
	defer func() { _ = unix.Close(fd) }()

	// fsync triggers FLUSH requests through the block layer to ublk
	if err := unix.Fsync(fd); err != nil {
		return fmt.Errorf("fsync failed: %w", err)
	}

	return nil
}

// BLKFLSBUF is the ioctl number for flushing block device buffers.
const ioctlBLKFLSBUF = 0x1261

// FlushBuffers invalidates the kernel's buffer cache for this block device.
// This uses the BLKFLSBUF ioctl to flush and invalidate cached data.
//
// Unlike Sync(), this does NOT send a FLUSH command to the backend.
// It only clears the kernel's page cache for the device, which is useful for:
//   - Invalidating stale cached reads after external changes
//   - Preparing a device for removal
//   - Testing/benchmarking without cache effects
//
// For durability guarantees (ensuring data reaches stable storage),
// use Sync() instead.
//
// Returns an error if the device is not started or if the ioctl fails.
func (d *Device) FlushBuffers() error {
	d.mu.RLock()
	if !d.started {
		d.mu.RUnlock()
		return ErrDeviceNotStarted
	}
	devPath := d.BlockDevicePath()
	d.mu.RUnlock()

	// Open the block device
	fd, err := unix.Open(devPath, unix.O_RDONLY, 0)
	if err != nil {
		return fmt.Errorf("failed to open block device for buffer flush: %w", err)
	}
	defer func() { _ = unix.Close(fd) }()

	// BLKFLSBUF flushes the buffer cache (does not send FLUSH to device)
	if _, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), ioctlBLKFLSBUF, 0); errno != 0 {
		return fmt.Errorf("BLKFLSBUF ioctl failed: %w", errno)
	}

	return nil
}

// ctrlCommand executes a control command via io_uring.
func (d *Device) ctrlCommand(cmdOp uint32, cmd *UblksrvCtrlCmd) error {
	if d.controlFD == nil {
		return errors.New("control device closed")
	}
	return ctrlCmd(d.controlFD, cmdOp, cmd)
}
