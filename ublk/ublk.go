// Package ublk provides Linux userspace block devices via the ublk driver.
package ublk

import (
	"fmt"
	"io"
	"math/bits"
	"reflect"
)

// Backend is the storage that the block device is backed by. It must
// satisfy io.ReaderAt and io.WriterAt, whose contracts already require
// that concurrent calls on disjoint ranges are safe — we rely on that
// to let the kernel submit IO with queue depth 128.
type Backend interface {
	io.ReaderAt
	io.WriterAt
}

// Discarder is an optional Backend capability. When implemented, the
// kernel may issue DISCARD against the device; the range contents are
// undefined after a successful call.
type Discarder interface {
	DiscardAt(off, length int64) (int, error)
}

// ZeroWriter is an optional Backend capability. When implemented, the
// kernel may issue WRITE_ZEROES against the device; subsequent reads
// of the range must return zeros.
type ZeroWriter interface {
	WriteZeroesAt(off, length int64, flags ZeroFlags) (int, error)
}

// ZeroFlags are per-op modifiers for [ZeroWriter.WriteZeroesAt].
type ZeroFlags uint8

const (
	// ZeroNoUnmap signals that the caller wants the range to remain
	// physically allocated (no hole punching / unmap).
	ZeroNoUnmap ZeroFlags = 1 << iota
)

// Config configures a new [Device]. Size is required; every other field
// has a sensible default when left at its zero value.
type Config struct {
	// Size is the device size in bytes. Required, > 0, multiple of BlockSize.
	Size uint64

	// BlockSize is the logical/physical block size in bytes. Must be a
	// power of two and >= 512. Defaults to 512.
	BlockSize uint32

	// QueueDepth is the kernel io_uring queue depth (in-flight IOs).
	// Must be in [1, 4096]. Defaults to 128.
	QueueDepth uint16

	// MaxIOSize is the largest single IO the kernel may submit, in bytes.
	// Must be a positive multiple of BlockSize. Defaults to 128 KiB.
	MaxIOSize uint32
}

const (
	defaultBlockSize  uint32 = 512
	defaultQueueDepth uint16 = 128
	defaultMaxIOSize  uint32 = 128 * 1024
)

func (c *Config) applyDefaults() {
	if c.BlockSize == 0 {
		c.BlockSize = defaultBlockSize
	}
	if c.QueueDepth == 0 {
		c.QueueDepth = defaultQueueDepth
	}
	if c.MaxIOSize == 0 {
		c.MaxIOSize = defaultMaxIOSize
	}
}

func (c *Config) validate() error {
	if c.Size == 0 {
		return fmt.Errorf("size must be > 0")
	}
	if c.BlockSize < 512 || bits.OnesCount32(c.BlockSize) != 1 {
		return fmt.Errorf("block size must be a power of two >= 512, got %d", c.BlockSize)
	}
	if c.Size%uint64(c.BlockSize) != 0 {
		return fmt.Errorf("size (%d) must be a multiple of block size (%d)", c.Size, c.BlockSize)
	}
	if c.QueueDepth < 1 || c.QueueDepth > maxQueueDepth {
		return fmt.Errorf("queue depth must be in [1, %d], got %d", maxQueueDepth, c.QueueDepth)
	}
	if c.MaxIOSize == 0 || c.MaxIOSize%c.BlockSize != 0 {
		return fmt.Errorf("max IO size (%d) must be a positive multiple of block size (%d)", c.MaxIOSize, c.BlockSize)
	}
	return nil
}

// New creates a ublk block device. backend must be non-nil. cfg.Size
// is required; other fields default when zero — see [Config].
// Call Device.Close() to stop and remove the device.
func New(backend Backend, cfg Config) (*Device, error) {
	if isNilBackend(backend) {
		return nil, fmt.Errorf("backend must not be nil")
	}
	cfg.applyDefaults()
	if err := cfg.validate(); err != nil {
		return nil, err
	}

	dev, err := openDevice(backend)
	if err != nil {
		return nil, err
	}
	cleanup := func() { _ = dev.shutdown() }

	if err := dev.addDev(1, cfg.QueueDepth, cfg.MaxIOSize); err != nil {
		cleanup()
		return nil, fmt.Errorf("add device: %w", err)
	}

	bufSize := int(dev.info.MaxIOBufBytes)
	if bufSize == 0 {
		bufSize = int(cfg.MaxIOSize)
	}

	if err := dev.openCharDev(); err != nil {
		cleanup()
		return nil, fmt.Errorf("open char device: %w", err)
	}

	if err := dev.setParams(cfg, uint32(bufSize)/512); err != nil {
		cleanup()
		return nil, fmt.Errorf("set params: %w", err)
	}

	w := newWorker(dev, 0, cfg.QueueDepth, bufSize)
	if err := w.init(); err != nil {
		cleanup()
		return nil, fmt.Errorf("init queue: %w", err)
	}
	dev.workers = []*worker{w}

	ready := make(chan error, 1)
	dev.wg.Add(1)
	go w.run(ready)

	if err := <-ready; err != nil {
		cleanup()
		return nil, fmt.Errorf("submit FETCH: %w", err)
	}

	if err := dev.startDev(); err != nil {
		cleanup()
		return nil, fmt.Errorf("start device: %w", err)
	}

	return dev, nil
}

func isNilBackend(backend Backend) bool {
	if backend == nil {
		return true
	}

	v := reflect.ValueOf(backend)
	switch v.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return v.IsNil()
	default:
		return false
	}
}
