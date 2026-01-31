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

type Config struct {
	BlockSize          uint64 // Logical block size (typically 512 or 4096)
	Size               uint64 // Total device size in bytes
	MaxSectors         uint32 // Maximum sectors per request
	MaxIOBufBytes      uint32 // Max IO buffer per request (0 = auto-sized from MaxSectors*BlockSize)
	NrHWQueues         uint16 // Number of hardware queues (default: 1)
	QueueDepth         uint16 // Depth of each queue (default: 128)
	ZeroCopy           bool   // Register IO buffers with io_uring (requires CAP_SYS_ADMIN)
	AutoBufReg         bool   // Kernel manages buffer registration (implies ZeroCopy)
	UserCopy           bool   // Use pread/pwrite instead of mmap buffer
	MaxDiscardSectors  uint32 // Max sectors per discard (0 = disabled)
	MaxDiscardSegments uint32
	COW                bool // Copy-on-write mode (requires COWBackend)
}

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

func (c *Config) validate() error {
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

	if c.BlockSize < 512 {
		return fmt.Errorf("BlockSize must be at least 512, got %d", c.BlockSize)
	}
	if c.BlockSize&(c.BlockSize-1) != 0 {
		return fmt.Errorf("BlockSize must be a power of 2, got %d", c.BlockSize)
	}

	if c.Size%c.BlockSize != 0 {
		return fmt.Errorf("Size (%d) must be a multiple of BlockSize (%d)", c.Size, c.BlockSize)
	}

	if c.Size == 0 {
		return errors.New("Size must be greater than 0")
	}

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

// Backend is the storage backend interface. Optionally implement Flusher, Discarder, WriteZeroer.
type Backend interface {
	ReadAt(p []byte, off int64) (n int, err error)
	WriteAt(p []byte, off int64) (n int, err error)
}

// FixedFileBackend returns a file for zero-copy IO. Required when Config.ZeroCopy is enabled.
type FixedFileBackend interface {
	FixedFile() (*os.File, error)
}

type Flusher interface {
	Flush() error
}

type Discarder interface {
	Discard(offset, length int64) error
}
type WriteZeroer interface {
	WriteZeroes(offset, length int64) error
}

// FuaWriter handles Force Unit Access writes. Falls back to WriteAt+Flush if not implemented.
type FuaWriter interface {
	WriteFua(p []byte, off int64) (n int, err error)
}

// SparseReader identifies zero regions to skip backend I/O.
type SparseReader interface {
	IsZeroRegion(off, length int64) bool
}

type ReaderWithFlags interface {
	ReadAtWithFlags(p []byte, off int64, flags uint32) (n int, err error)
}
type WriterWithFlags interface {
	WriteAtWithFlags(p []byte, off int64, flags uint32) (n int, err error)
}

// COWBackend enables copy-on-write: zero-copy for overlay, user-copy for base reads.
type COWBackend interface {
	Backend
	Overlay() (*os.File, error)
	ClassifyRange(off, length int64) (allDirty, allClean bool)
	ReadBaseAt(p []byte, off int64) (n int, err error)
}

// New creates, configures, and starts a ublk device.
func New(backend Backend, config Config) (*Device, error) {
	if err := config.validate(); err != nil {
		return nil, fmt.Errorf("invalid config: %w", err)
	}

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
		opts = append(opts, WithUserCopy(), WithCOW())
	}
	if config.MaxIOBufBytes > 0 {
		opts = append(opts, WithMaxIOBufBytes(config.MaxIOBufBytes))
	}
	dev, err := NewDeviceWithBackend(backend, opts...)
	if err != nil {
		return nil, fmt.Errorf("failed to create device: %w", err)
	}

	if err = dev.Add(config.NrHWQueues, config.QueueDepth); err != nil {
		_ = dev.Delete()
		return nil, fmt.Errorf("failed to add device: %w", err)
	}
	if err = dev.SetParams(config.BlockSize, config.Size, config.MaxSectors, config.MaxDiscardSectors, config.MaxDiscardSegments); err != nil {
		_ = dev.Delete()
		return nil, fmt.Errorf("failed to set params: %w", err)
	}
	if err = dev.Start(); err != nil {
		_ = dev.Delete()
		return nil, fmt.Errorf("failed to start device: %w", err)
	}

	return dev, nil
}

// ReaderAtWriterAt implements Backend using io.ReaderAt and io.WriterAt.
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
