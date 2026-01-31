// Package ublk provides a pure Go implementation for creating Linux
// userspace block devices using the ublk driver.
//
// The ublk driver (available in Linux 6.0+) allows userspace programs to
// implement block device backends. This package handles the low-level
// io_uring communication with the kernel, allowing you to focus on
// implementing your storage backend.
//
// # Quick Start
//
// Implement the Backend interface and call CreateDevice:
//
//	type MyBackend struct { /* ... */ }
//
//	func (b *MyBackend) ReadAt(p []byte, off int64) (int, error) {
//	    // Read data from your storage
//	}
//
//	func (b *MyBackend) WriteAt(p []byte, off int64) (int, error) {
//	    // Write data to your storage
//	}
//
//	func main() {
//	    backend := &MyBackend{}
//	    config := ublk.DefaultConfig()
//	    config.Size = 1 * 1024 * 1024 * 1024 // 1GB
//
//	    dev, err := ublk.CreateDevice(backend, config)
//	    if err != nil {
//	        log.Fatal(err)
//	    }
//	    defer dev.Delete()
//
//	    fmt.Printf("Device available at: %s\n", dev.BlockDevicePath())
//	    // Block device is now usable...
//	}
//
// # Requirements
//
// - Linux kernel 6.0+ with ublk driver enabled
// - Root privileges (for creating block devices)
//
// # Thread Safety
//
// Device operations are thread-safe. The Backend implementation must also
// be thread-safe as it may be called concurrently from multiple goroutines.
package ublk

import (
	"errors"
	"fmt"
	"io"
	"os"
)

// Config holds configuration for creating a ublk device.
type Config struct {
	// BlockSize is the logical block size (typically 512 or 4096)
	BlockSize uint64

	// Size is the total size of the device in bytes
	Size uint64

	// MaxSectors is the maximum sectors per request
	MaxSectors uint32

	// MaxIOBufBytes is the maximum IO buffer size per request in bytes.
	// If zero, it is auto-sized to at least MaxSectors*BlockSize (minimum 512KB).
	MaxIOBufBytes uint32

	// NrHWQueues is the number of hardware queues (default: 1)
	NrHWQueues uint16

	// QueueDepth is the depth of each queue (default: 128)
	QueueDepth uint16

	// ZeroCopy enables zero-copy mode (requires CAP_SYS_ADMIN, kernel 6.x+).
	// When enabled, IO buffers are registered with io_uring to avoid data copying.
	ZeroCopy bool

	// AutoBufReg enables automatic buffer registration (requires kernel support).
	// This is a faster zero-copy path and implies ZeroCopy.
	AutoBufReg bool

	// UserCopy enables user-copy mode where pread/pwrite on the char device
	// transfers data instead of using the mmap buffer. This allows:
	// - Skipping data transfer for FLUSH/DISCARD (just complete, no copy)
	// - Direct copy to/from application buffers
	// - More control over when data is transferred
	UserCopy bool

	// MaxDiscardSectors is the maximum sectors per discard request (optional).
	// If set to > 0, DISCARD/TRIM support is enabled.
	MaxDiscardSectors uint32

	// MaxDiscardSegments is the maximum number of segments in a discard request (default: 1).
	MaxDiscardSegments uint32

	// DisableStats disables per-operation statistics tracking.
	// When true, device.Stats() will return zero values.
	// This eliminates atomic counter overhead on every I/O operation.
	DisableStats bool

	// COW enables copy-on-write mode.
	// Requires a backend that implements COWBackend interface.
	// When enabled:
	//   - Writes: Zero-copy to overlay file
	//   - Dirty block reads: Zero-copy from overlay file
	//   - Clean block reads: User-copy from base via ReadBaseAt
	// This is ideal for VM snapshots, container images, etc. where the base is
	// compressed or in-memory, but the overlay is a regular file.
	COW bool
}

// DefaultConfig returns a default configuration.
func DefaultConfig() Config {
	return Config{
		BlockSize:     512,
		Size:          1024 * 1024 * 1024, // 1GB
		MaxSectors:    256,
		MaxIOBufBytes: 512 * 1024,
		NrHWQueues:    1,
		QueueDepth:    128,
	}
}

// validate checks and applies defaults to the configuration.
func (c *Config) validate() error {
	// Apply defaults
	if c.BlockSize == 0 {
		c.BlockSize = 512
	}
	if c.Size == 0 {
		c.Size = 1024 * 1024 * 1024
	}
	if c.MaxSectors == 0 {
		c.MaxSectors = 256
	}
	if c.MaxIOBufBytes == 0 {
		c.MaxIOBufBytes = 512 * 1024
	}
	if c.NrHWQueues == 0 {
		c.NrHWQueues = 1
	}
	if c.QueueDepth == 0 {
		c.QueueDepth = 128
	}

	// Validate BlockSize is power of 2 and at least 512
	if c.BlockSize < 512 {
		return fmt.Errorf("BlockSize must be at least 512, got %d", c.BlockSize)
	}
	if c.BlockSize&(c.BlockSize-1) != 0 {
		return fmt.Errorf("BlockSize must be a power of 2, got %d", c.BlockSize)
	}

	// Validate Size is multiple of BlockSize
	if c.Size%c.BlockSize != 0 {
		return fmt.Errorf("Size (%d) must be a multiple of BlockSize (%d)", c.Size, c.BlockSize)
	}

	// Validate Size is not zero
	if c.Size == 0 {
		return errors.New("Size must be greater than 0")
	}

	// Validate queue depth is power of 2
	if c.QueueDepth&(c.QueueDepth-1) != 0 {
		return fmt.Errorf("QueueDepth must be a power of 2, got %d", c.QueueDepth)
	}
	if c.QueueDepth > UBLK_MAX_QUEUE_DEPTH {
		return fmt.Errorf("QueueDepth must be <= %d, got %d", UBLK_MAX_QUEUE_DEPTH, c.QueueDepth)
	}

	if c.ZeroCopy && c.UserCopy {
		return errors.New("ZeroCopy cannot be used with UserCopy")
	}
	if c.AutoBufReg && c.UserCopy {
		return errors.New("AutoBufReg cannot be used with UserCopy")
	}
	if c.COW && (c.ZeroCopy || c.AutoBufReg) {
		return errors.New("COW cannot be used with ZeroCopy or AutoBufReg")
	}
	if c.MaxDiscardSegments > uint32(^uint16(0)) {
		return fmt.Errorf("MaxDiscardSegments must be <= %d, got %d", ^uint16(0), c.MaxDiscardSegments)
	}

	maxReqBytes := uint64(c.MaxSectors) * c.BlockSize
	if maxReqBytes == 0 {
		return errors.New("MaxSectors must be greater than 0")
	}
	if uint64(c.MaxIOBufBytes) < maxReqBytes {
		c.MaxIOBufBytes = uint32(maxReqBytes)
	}
	maxAllowed := uint64(1) << UBLK_IO_BUF_BITS
	if uint64(c.MaxIOBufBytes) > maxAllowed {
		return fmt.Errorf("MaxIOBufBytes must be <= %d bytes, got %d", maxAllowed, c.MaxIOBufBytes)
	}

	return nil
}

// Backend represents the storage backend for the ublk device.
// Implement ReadAt and WriteAt for basic functionality.
// For advanced operations, also implement Flusher, Discarder, or WriteZeroer.
type Backend interface {
	// ReadAt reads len(p) bytes from offset off
	ReadAt(p []byte, off int64) (n int, err error)

	// WriteAt writes len(p) bytes at offset off
	WriteAt(p []byte, off int64) (n int, err error)
}

// FixedFileBackend is an optional interface for zero-copy IO.
// It must return a file descriptor that supports io_uring read/write.
// Required when Config.ZeroCopy is enabled.
type FixedFileBackend interface {
	FixedFile() (*os.File, error)
}

// Flusher is an optional interface for backends that support flushing.
type Flusher interface {
	// Flush ensures all written data is persisted to stable storage.
	Flush() error
}

// Discarder is an optional interface for backends that support discard/trim.
type Discarder interface {
	// Discard indicates that the data in the given range is no longer needed.
	// The backend may deallocate or zero this region.
	Discard(offset, length int64) error
}

// WriteZeroer is an optional interface for backends that can efficiently write zeroes.
type WriteZeroer interface {
	// WriteZeroes writes zeroes to the given range.
	// This may be more efficient than writing a buffer of zeroes.
	WriteZeroes(offset, length int64) error
}

// FuaWriter is an optional interface for backends that support Force Unit Access (FUA) writes.
// If implemented, WriteFua is called for writes with the FUA flag set.
// If not implemented, the device falls back to WriteAt followed by Flush.
type FuaWriter interface {
	// WriteFua writes len(p) bytes at offset off and ensures data is persisted before returning.
	WriteFua(p []byte, off int64) (n int, err error)
}

// ReaderWithFlags is an optional interface for backends that support reading with op_flags.
type ReaderWithFlags interface {
	ReadAtWithFlags(p []byte, off int64, flags uint32) (n int, err error)
}

// WriterWithFlags is an optional interface for backends that support writing with op_flags.
// Note: FUA is handled specifically by FuaWriter, but WriterWithFlags can handle others.
type WriterWithFlags interface {
	WriteAtWithFlags(p []byte, off int64, flags uint32) (n int, err error)
}

// COWBackend is an optional interface for copy-on-write backends.
// It enables zero-copy I/O for the overlay file while using user-copy for base reads.
//
// I/O routing:
//   - Writes: Zero-copy to overlay file via io_uring
//   - Dirty block reads: Zero-copy from overlay file
//   - Clean block reads: User-copy from base via ReadBaseAt
//
// This is ideal for scenarios where the base image is in-memory, compressed,
// or fetched from network, but the overlay is a regular file.
type COWBackend interface {
	Backend

	// Overlay returns the overlay file for zero-copy I/O.
	// All writes go to this file, and dirty block reads come from it.
	Overlay() (*os.File, error)

	// ClassifyRange determines the dirty state of a byte range.
	// Returns:
	//   - allDirty=true:  entire range is dirty, use overlay (zero-copy)
	//   - allClean=true:  entire range is clean, use base (user-copy)
	//   - both false:     mixed range, requires per-block routing
	ClassifyRange(off, length int64) (allDirty, allClean bool)

	// ReadBaseAt reads base (clean) data into p at offset off.
	// Called for clean blocks that haven't been written to.
	// Implementations typically decompress or fetch from remote storage.
	ReadBaseAt(p []byte, off int64) (n int, err error)
}

// New creates and starts a ublk device with the given backend and configuration.
// The backend's extended interfaces (Flusher, Discarder, WriteZeroer) will be
// used automatically if implemented.
//
// This is the primary way to create a ublk device. It handles all setup:
// device creation, parameter configuration, and starting the I/O workers.
func New(backend Backend, config Config) (*Device, error) {
	// Validate and apply defaults
	if err := config.validate(); err != nil {
		return nil, fmt.Errorf("invalid config: %w", err)
	}

	// Build device options from config
	var opts []DeviceOption
	if config.AutoBufReg {
		opts = append(opts, WithAutoBufReg())
	} else if config.ZeroCopy {
		opts = append(opts, WithZeroCopy())
	}
	if config.UserCopy {
		opts = append(opts, WithUserCopy())
	}
	if config.COW {
		// COW uses user-copy for control, with per-request zero-copy routing
		opts = append(opts, WithUserCopy(), WithCOW())
	}
	if config.MaxIOBufBytes > 0 {
		opts = append(opts, WithMaxIOBufBytes(config.MaxIOBufBytes))
	}
	if config.DisableStats {
		opts = append(opts, WithDisableStats())
	}
	// Create device with backend and options
	dev, err := NewDeviceWithBackend(backend, opts...)
	if err != nil {
		return nil, fmt.Errorf("failed to create device: %w", err)
	}

	// Add the device
	err = dev.Add(config.NrHWQueues, config.QueueDepth)
	if err != nil {
		_ = dev.Delete() // Cleanup on error, best-effort
		return nil, fmt.Errorf("failed to add device: %w", err)
	}

	// Set parameters
	err = dev.SetParams(config.BlockSize, config.Size, config.MaxSectors, config.MaxDiscardSectors, config.MaxDiscardSegments)
	if err != nil {
		_ = dev.Delete() // Cleanup on error, best-effort
		return nil, fmt.Errorf("failed to set params: %w", err)
	}

	// Start the device
	err = dev.Start()
	if err != nil {
		_ = dev.Delete() // Cleanup on error, best-effort
		return nil, fmt.Errorf("failed to start device: %w", err)
	}

	return dev, nil
}

// ReaderAtWriterAt is a convenience type that implements Backend
// using io.ReaderAt and io.WriterAt.
type ReaderAtWriterAt struct {
	ReaderAt io.ReaderAt
	WriterAt io.WriterAt
}

func (r *ReaderAtWriterAt) ReadAt(p []byte, off int64) (n int, err error) {
	if r.ReaderAt == nil {
		return 0, io.EOF
	}
	return r.ReaderAt.ReadAt(p, off)
}

func (r *ReaderAtWriterAt) WriteAt(p []byte, off int64) (n int, err error) {
	if r.WriterAt == nil {
		return len(p), nil
	}
	return r.WriterAt.WriteAt(p, off)
}
