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

// Option overrides a [New] default.
type Option func(*config)

type config struct {
	blockSize  uint32
	queueDepth uint16
	maxIOSize  uint32
}

const (
	defaultBlockSize  uint32 = 512
	defaultQueueDepth uint16 = 128
	defaultMaxIOSize  uint32 = 128 * 1024
	maxMaxIOSize      uint32 = 1 << 20 // 1 MiB; conservative cap, raise deliberately
)

// WithBlockSize sets the logical/physical block size (power of two, >= 512).
func WithBlockSize(bs uint32) Option { return func(c *config) { c.blockSize = bs } }

// WithQueueDepth sets the io_uring queue depth (1..[maxQueueDepth]).
func WithQueueDepth(d uint16) Option { return func(c *config) { c.queueDepth = d } }

// WithMaxIOSize sets the largest single IO in bytes (multiple of block size).
func WithMaxIOSize(n uint32) Option { return func(c *config) { c.maxIOSize = n } }

func (c config) validate(size uint64) error {
	if size == 0 {
		return fmt.Errorf("size must be > 0")
	}
	if c.blockSize < 512 || bits.OnesCount32(c.blockSize) != 1 {
		return fmt.Errorf("block size must be a power of two >= 512, got %d", c.blockSize)
	}
	if size%uint64(c.blockSize) != 0 {
		return fmt.Errorf("size (%d) must be a multiple of block size (%d)", size, c.blockSize)
	}
	if c.queueDepth < 1 || c.queueDepth > maxQueueDepth {
		return fmt.Errorf("queue depth must be in [1, %d], got %d", maxQueueDepth, c.queueDepth)
	}
	if c.maxIOSize == 0 || c.maxIOSize%c.blockSize != 0 {
		return fmt.Errorf("max IO size (%d) must be a positive multiple of block size (%d)", c.maxIOSize, c.blockSize)
	}
	if c.maxIOSize > maxMaxIOSize {
		return fmt.Errorf("max IO size (%d) exceeds 1 MiB cap (%d)", c.maxIOSize, maxMaxIOSize)
	}
	return nil
}

// New creates a ublk block device. backend must be non-nil. Size must be a
// positive multiple of the block size (default 512). Call Device.Close() to
// stop and remove the device.
func New(backend Backend, size uint64, opts ...Option) (*Device, error) {
	if isNilBackend(backend) {
		return nil, fmt.Errorf("backend must not be nil")
	}
	cfg := config{
		blockSize:  defaultBlockSize,
		queueDepth: defaultQueueDepth,
		maxIOSize:  defaultMaxIOSize,
	}
	for _, opt := range opts {
		opt(&cfg)
	}
	if err := cfg.validate(size); err != nil {
		return nil, err
	}

	dev, err := openDevice(backend)
	if err != nil {
		return nil, err
	}
	cleanup := func() { _ = dev.shutdown() }

	if err := dev.addDev(1, cfg.queueDepth, cfg.maxIOSize); err != nil {
		cleanup()
		return nil, fmt.Errorf("add device: %w", err)
	}

	bufSize := int(dev.info.MaxIOBufBytes)
	if bufSize == 0 {
		bufSize = int(cfg.maxIOSize)
	}

	if err := dev.openCharDev(); err != nil {
		cleanup()
		return nil, fmt.Errorf("open char device: %w", err)
	}

	if err := dev.setParams(size, cfg, uint32(bufSize)/512); err != nil {
		cleanup()
		return nil, fmt.Errorf("set params: %w", err)
	}

	w := newWorker(dev, 0, cfg.queueDepth, bufSize)
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
