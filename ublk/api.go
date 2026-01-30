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
	"fmt"
	"io"
)

// Config holds configuration for creating a ublk device
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
}

// DefaultConfig returns a default configuration
func DefaultConfig() Config {
	return Config{
		BlockSize:  512,
		Size:       1024 * 1024 * 1024, // 1GB
		MaxSectors: 256,
		NrHWQueues: 1,
		QueueDepth: 128,
	}
}

// Backend represents the storage backend for the ublk device
type Backend interface {
	// ReadAt reads len(p) bytes from offset off
	ReadAt(p []byte, off int64) (n int, err error)

	// WriteAt writes len(p) bytes at offset off
	WriteAt(p []byte, off int64) (n int, err error)
}

// CreateDevice creates and starts a ublk device with the given backend
func CreateDevice(backend Backend, config Config) (*Device, error) {
	if config.BlockSize == 0 {
		config.BlockSize = 512
	}
	if config.Size == 0 {
		config.Size = 1024 * 1024 * 1024 // 1GB default
	}
	if config.MaxSectors == 0 {
		config.MaxSectors = 256
	}
	if config.NrHWQueues == 0 {
		config.NrHWQueues = 1
	}
	if config.QueueDepth == 0 {
		config.QueueDepth = 128
	}

	// Create device with backend functions
	dev, err := NewDevice(backend.ReadAt, backend.WriteAt)
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
// using io.ReaderAt and io.WriterAt
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
