// Package ublk provides Linux userspace block devices via the ublk driver.
package ublk

import "fmt"

// Backend must be safe for concurrent use.
type Backend interface {
	ReadAt(p []byte, off int64) (n int, err error)
	WriteAt(p []byte, off int64) (n int, err error)
}

// New creates a ublk block device. Size must be a positive multiple of 512.
// Call Device.Close() to stop and remove the device.
func New(backend Backend, size uint64) (*Device, error) {
	if size == 0 || size%512 != 0 {
		return nil, fmt.Errorf("size must be > 0 and a multiple of 512")
	}

	dev, err := openDevice(backend)
	if err != nil {
		return nil, err
	}
	cleanup := func() { _ = dev.shutdown() }

	const (
		queueDepth    = 128
		maxSectors    = 256 // 128KB max IO, in 512-byte units
		maxIOBufBytes = maxSectors * 512
	)

	if err := dev.addDev(1, queueDepth, maxIOBufBytes); err != nil {
		cleanup()
		return nil, fmt.Errorf("add device: %w", err)
	}

	bufSize := int(dev.info.MaxIOBufBytes)
	if bufSize == 0 {
		bufSize = maxIOBufBytes
	}

	if err := dev.openCharDev(); err != nil {
		cleanup()
		return nil, fmt.Errorf("open char device: %w", err)
	}

	if err := dev.setParams(size, uint32(bufSize/512)); err != nil {
		cleanup()
		return nil, fmt.Errorf("set params: %w", err)
	}

	w := newWorker(dev, 0, queueDepth, bufSize)
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
