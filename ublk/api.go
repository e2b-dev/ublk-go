package ublk

import (
	"errors"
	"fmt"
	"io"
	"os"
)

type Config struct {
	BlockSize          uint64
	Size               uint64
	MaxSectors         uint32
	MaxIOBufBytes      uint32
	NrHWQueues         uint16
	QueueDepth         uint16
	ZeroCopy           bool
	AutoBufReg         bool // implies ZeroCopy
	UserCopy           bool
	MaxDiscardSectors  uint32
	MaxDiscardSegments uint32
	COW                bool
	Unprivileged       bool
}

func DefaultConfig() Config {
	return Config{
		BlockSize:     512,
		Size:          1 << 30, // 1GB
		MaxSectors:    256,
		MaxIOBufBytes: 512 * 1024,
		NrHWQueues:    1,
		QueueDepth:    128,
	}
}

func (c *Config) validate() error {
	defaults := DefaultConfig()
	if c.BlockSize == 0 {
		c.BlockSize = defaults.BlockSize
	}
	if c.Size == 0 {
		c.Size = defaults.Size
	}
	if c.MaxSectors == 0 {
		c.MaxSectors = defaults.MaxSectors
	}
	if c.MaxIOBufBytes == 0 {
		c.MaxIOBufBytes = defaults.MaxIOBufBytes
	}
	if c.NrHWQueues == 0 {
		c.NrHWQueues = defaults.NrHWQueues
	}
	if c.QueueDepth == 0 {
		c.QueueDepth = defaults.QueueDepth
	}

	if c.BlockSize < 512 || c.BlockSize&(c.BlockSize-1) != 0 {
		return fmt.Errorf("BlockSize must be >= 512 and power of 2, got %d", c.BlockSize)
	}
	if c.Size%c.BlockSize != 0 {
		return errors.New("size must be multiple of block size")
	}
	if c.QueueDepth&(c.QueueDepth-1) != 0 || c.QueueDepth > UBLK_MAX_QUEUE_DEPTH {
		return fmt.Errorf("QueueDepth must be power of 2 and <= %d", UBLK_MAX_QUEUE_DEPTH)
	}
	if c.ZeroCopy && c.UserCopy {
		return errors.New("ZeroCopy and UserCopy are mutually exclusive")
	}
	if c.AutoBufReg && c.UserCopy {
		return errors.New("AutoBufReg and UserCopy are mutually exclusive")
	}
	if c.COW && (c.ZeroCopy || c.AutoBufReg) {
		return errors.New("COW requires UserCopy mode")
	}
	if c.MaxDiscardSegments > uint32(^uint16(0)) {
		return fmt.Errorf("MaxDiscardSegments must be <= %d", ^uint16(0))
	}

	maxReqBytes := uint64(c.MaxSectors) * c.BlockSize
	if uint64(c.MaxIOBufBytes) < maxReqBytes {
		c.MaxIOBufBytes = uint32(maxReqBytes)
	}
	if uint64(c.MaxIOBufBytes) > 1<<UBLK_IO_BUF_BITS {
		return errors.New("MaxIOBufBytes exceeds limit")
	}

	return nil
}

type Backend interface {
	ReadAt(p []byte, off int64) (n int, err error)
	WriteAt(p []byte, off int64) (n int, err error)
}

type FixedFileBackend interface {
	FixedFile() (*os.File, error)
}

type (
	Flusher     interface{ Flush() error }
	Discarder   interface{ Discard(off, length int64) error }
	WriteZeroer interface{ WriteZeroes(off, length int64) error }
	FuaWriter   interface {
		WriteFua(p []byte, off int64) (n int, err error)
	}
	SparseReader    interface{ IsZeroRegion(off, length int64) bool }
	ReaderWithFlags interface {
		ReadAtWithFlags(p []byte, off int64, flags uint32) (n int, err error)
	}
	WriterWithFlags interface {
		WriteAtWithFlags(p []byte, off int64, flags uint32) (n int, err error)
	}
)

type COWBackend interface {
	Backend
	Overlay() (*os.File, error)
	ClassifyRange(off, length int64) (allDirty, allClean bool)
	ReadBaseAt(p []byte, off int64) (n int, err error)
}

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
	if config.Unprivileged {
		opts = append(opts, WithUnprivileged())
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

type ReaderAtWriterAt struct {
	io.ReaderAt
	io.WriterAt
}

func (r *ReaderAtWriterAt) ReadAt(p []byte, off int64) (int, error) {
	if r.ReaderAt == nil {
		return 0, io.EOF
	}
	return r.ReaderAt.ReadAt(p, off)
}

func (r *ReaderAtWriterAt) WriteAt(p []byte, off int64) (int, error) {
	if r.WriterAt == nil {
		return len(p), nil
	}
	return r.WriterAt.WriteAt(p, off)
}
