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
)

// Config holds configuration for creating a ublk device.
type Config struct {
	// BlockSize is the logical block size (typically 512 or 4096)
	BlockSize uint64

	// Size is the total size of the device in bytes
	Size uint64

	// MaxSectors is the maximum sectors per request
	MaxSectors uint32

	// NrHWQueues is the number of hardware queues (default: 1)
	NrHWQueues uint16

	// QueueDepth is the depth of each queue (default: 128)
	QueueDepth uint16

	// ZeroCopy enables zero-copy mode (requires CAP_SYS_ADMIN, kernel 6.x+).
	// When enabled, IO buffers are registered with io_uring to avoid data copying.
	ZeroCopy bool

	// AutoBufReg enables automatic buffer registration (implies ZeroCopy).
	// This simplifies zero-copy by having the kernel automatically register
	// and unregister buffers, eliminating manual buffer management.
	AutoBufReg bool

	// UserRecovery enables user-space recovery on ublk server crash.
	// The block device survives server restarts without data loss.
	UserRecovery bool

	// Unprivileged enables unprivileged device control (container-aware).
	Unprivileged bool
}

// DefaultConfig returns a default configuration.
func DefaultConfig() Config {
	return Config{
		BlockSize:  512,
		Size:       1024 * 1024 * 1024, // 1GB
		MaxSectors: 256,
		NrHWQueues: 1,
		QueueDepth: 128,
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

// CreateDevice creates and starts a ublk device with the given backend.
// The backend's extended interfaces (Flusher, Discarder, WriteZeroer) will be
// used automatically if implemented.
func CreateDevice(backend Backend, config Config) (*Device, error) {
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
	if config.UserRecovery {
		opts = append(opts, WithUserRecovery())
	}
	if config.Unprivileged {
		opts = append(opts, WithUnprivileged())
	}

	// Create device with backend and options
	dev, err := NewDeviceWithBackend(backend, opts...)
	if err != nil {
		return nil, fmt.Errorf("failed to create device: %w", err)
	}

	// Add the device
	err = dev.Add(config.NrHWQueues, config.QueueDepth)
	if err != nil {
		dev.Delete()
		return nil, fmt.Errorf("failed to add device: %w", err)
	}

	// Set parameters
	err = dev.SetParams(config.BlockSize, config.Size, config.MaxSectors)
	if err != nil {
		dev.Delete()
		return nil, fmt.Errorf("failed to set params: %w", err)
	}

	// Start the device
	err = dev.Start()
	if err != nil {
		dev.Delete()
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
