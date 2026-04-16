// Package ublk implements Linux userspace block devices via the ublk driver and io_uring.
package ublk

import (
	"errors"
	"fmt"
)

// Backend is the interface that block device implementations must satisfy.
// ReadAt and WriteAt must be safe for concurrent use.
type Backend interface {
	ReadAt(p []byte, off int64) (n int, err error)
	WriteAt(p []byte, off int64) (n int, err error)
}

// Flusher may be optionally implemented by a Backend to handle FLUSH requests.
type Flusher interface {
	Flush() error
}

// Discarder may be optionally implemented by a Backend to handle DISCARD requests.
type Discarder interface {
	Discard(off, length int64) error
}

// WriteZeroer may be optionally implemented by a Backend to handle WRITE_ZEROES requests.
type WriteZeroer interface {
	WriteZeroes(off, length int64) error
}

// Config configures a ublk block device.
type Config struct {
	// Size of the block device in bytes. Required; must be > 0 and a multiple of BlockSize.
	Size uint64

	// BlockSize is the logical block size in bytes.
	// Must be a power of 2, >= 512. Default: 512.
	BlockSize uint32

	// Queues is the number of IO queues. Default: 1.
	Queues uint16

	// QueueDepth is the per-queue IO depth. Must be a power of 2, <= 4096. Default: 128.
	QueueDepth uint16
}

func (c *Config) setDefaults() {
	if c.BlockSize == 0 {
		c.BlockSize = 512
	}
	if c.Queues == 0 {
		c.Queues = 1
	}
	if c.QueueDepth == 0 {
		c.QueueDepth = 128
	}
}

func (c *Config) validate() error {
	if c.Size == 0 {
		return errors.New("Size must be > 0")
	}
	if c.BlockSize < 512 || c.BlockSize&(c.BlockSize-1) != 0 {
		return fmt.Errorf("BlockSize must be a power of 2 >= 512, got %d", c.BlockSize)
	}
	if c.Size%uint64(c.BlockSize) != 0 {
		return errors.New("Size must be a multiple of BlockSize")
	}
	if c.QueueDepth == 0 || c.QueueDepth&(c.QueueDepth-1) != 0 || c.QueueDepth > maxQueueDepth {
		return fmt.Errorf("QueueDepth must be a power of 2 in [1, %d], got %d", maxQueueDepth, c.QueueDepth)
	}
	if c.Queues == 0 {
		return errors.New("Queues must be > 0")
	}
	return nil
}

// New creates and starts a ublk block device backed by the given Backend.
// The block device is available at Device.BlockDevicePath() after this returns.
// Call Device.Close() to stop and remove the device.
func New(backend Backend, cfg Config) (*Device, error) {
	cfg.setDefaults()
	if err := cfg.validate(); err != nil {
		return nil, fmt.Errorf("invalid config: %w", err)
	}

	dev, err := openDevice(backend)
	if err != nil {
		return nil, err
	}

	cleanup := func() { _ = dev.shutdown() }

	maxIOBufBytes := uint32(cfg.QueueDepth) // just a temp; real value below
	// max_sectors is in 512-byte units. 128KB max IO is a sane default.
	maxSectors := uint32(256)
	maxIOBufBytes = maxSectors * 512

	if err := dev.addDev(cfg.Queues, cfg.QueueDepth, maxIOBufBytes); err != nil {
		cleanup()
		return nil, fmt.Errorf("add device: %w", err)
	}

	// The kernel may have adjusted maxIOBufBytes.
	bufSize := int(dev.info.MaxIOBufBytes)
	if bufSize == 0 {
		bufSize = int(maxIOBufBytes)
	}
	actualMaxSectors := uint32(bufSize / 512)
	if actualMaxSectors == 0 {
		actualMaxSectors = 1
	}

	if err := dev.openCharDev(); err != nil {
		cleanup()
		return nil, fmt.Errorf("open char device: %w", err)
	}

	if err := dev.setParams(cfg.Size, cfg.BlockSize, actualMaxSectors); err != nil {
		cleanup()
		return nil, fmt.Errorf("set params: %w", err)
	}

	// Initialize all workers BEFORE START_DEV.
	// Each worker creates its io_uring, mmaps IO descriptors, allocates buffers,
	// and submits initial FETCH_REQ commands. The kernel requires all queues
	// to have submitted FETCH_REQ before START_DEV will succeed.
	dev.workers = make([]*worker, cfg.Queues)
	for i := range cfg.Queues {
		w := newWorker(dev, uint16(i), cfg.QueueDepth, bufSize)
		if err := w.init(); err != nil {
			// Clean up already-initialized workers.
			for j := range i {
				dev.workers[j].cleanup()
			}
			cleanup()
			return nil, fmt.Errorf("init queue %d: %w", i, err)
		}
		dev.workers[i] = w
	}

	if err := dev.startDev(); err != nil {
		for _, w := range dev.workers {
			w.cleanup()
		}
		cleanup()
		return nil, fmt.Errorf("start device: %w", err)
	}

	// Launch worker goroutines to process IO.
	for _, w := range dev.workers {
		dev.wg.Add(1)
		go w.run()
	}

	return dev, nil
}
