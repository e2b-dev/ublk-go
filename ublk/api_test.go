package ublk

import (
	"errors"
	"testing"
)

// TestBackend is a test backend implementation.
type TestBackend struct {
	data []byte
	size int64
}

func NewTestBackend(size int64) *TestBackend {
	return &TestBackend{
		data: make([]byte, size),
		size: size,
	}
}

func (b *TestBackend) ReadAt(p []byte, off int64) (n int, err error) {
	if off >= b.size {
		return 0, errors.New("offset out of range")
	}
	end := min(off+int64(len(p)), b.size)
	n = copy(p, b.data[off:end])
	if n < len(p) {
		return n, errors.New("short read")
	}
	return n, nil
}

func (b *TestBackend) WriteAt(p []byte, off int64) (n int, err error) {
	if off < 0 {
		return 0, errors.New("invalid offset")
	}
	end := off + int64(len(p))
	if end > b.size {
		if end > int64(cap(b.data)) {
			newData := make([]byte, end)
			copy(newData, b.data)
			b.data = newData
		}
		b.size = end
	}
	n = copy(b.data[off:], p)
	return n, nil
}

func (b *TestBackend) GetData() []byte {
	return b.data[:b.size]
}

// TestNew tests the New function.
// Not parallelized: interacts with kernel resources.
func TestNew(t *testing.T) {
	backend := NewTestBackend(1024 * 1024)

	config := DefaultConfig()
	config.Size = 1024 * 1024
	config.BlockSize = 512
	config.NrHWQueues = 1
	config.QueueDepth = 64

	dev, err := New(backend, config)
	if err != nil {
		t.Logf("New returned error (expected without root/kernel): %v", err)
		return
	}

	defer func() {
		if dev != nil {
			dev.Delete()
		}
	}()

	if dev.DeviceID() < 0 {
		t.Error("Device ID should be non-negative")
	}

	blockPath := dev.BlockDevicePath()
	if blockPath == "" {
		t.Error("Block device path should not be empty")
	}

	t.Logf("Created device: %s (ID: %d)", blockPath, dev.DeviceID())
}

func TestDefaultConfig(t *testing.T) {
	t.Parallel()
	config := DefaultConfig()

	if config.BlockSize == 0 {
		t.Error("Default block size should not be zero")
	}
	if config.Size == 0 {
		t.Error("Default size should not be zero")
	}
	if config.NrHWQueues == 0 {
		t.Error("Default number of queues should not be zero")
	}
	if config.QueueDepth == 0 {
		t.Error("Default queue depth should not be zero")
	}
	if config.MaxSectors == 0 {
		t.Error("Default max sectors should not be zero")
	}
	if config.MaxIOBufBytes == 0 {
		t.Error("Default MaxIOBufBytes should not be zero")
	}

	t.Logf("Default config: BlockSize=%d, Size=%d, Queues=%d, Depth=%d, MaxIOBufBytes=%d",
		config.BlockSize, config.Size, config.NrHWQueues, config.QueueDepth, config.MaxIOBufBytes)
}

func TestBackendInterface(t *testing.T) {
	t.Parallel()
	var _ Backend = (*TestBackend)(nil)
	var _ Backend = (*ReaderAtWriterAt)(nil)
}

func TestReaderAtWriterAtNilHandling(t *testing.T) {
	t.Parallel()
	backend := &ReaderAtWriterAt{
		ReaderAt: nil,
		WriterAt: nil,
	}

	buf := make([]byte, 10)
	_, err := backend.ReadAt(buf, 0)
	if err == nil {
		t.Error("Expected EOF error for nil ReaderAt")
	}

	n, err := backend.WriteAt([]byte("test"), 0)
	if err != nil {
		t.Errorf("WriteAt with nil WriterAt should not error: %v", err)
	}
	if n != 4 {
		t.Errorf("WriteAt should return len(p), got %d", n)
	}
}

func TestReaderAtWriterAt(t *testing.T) {
	t.Parallel()
	backend := &ReaderAtWriterAt{
		ReaderAt: &TestBackend{data: []byte("test"), size: 4},
		WriterAt: &TestBackend{data: make([]byte, 10), size: 10},
	}

	buf := make([]byte, 4)
	n, err := backend.ReadAt(buf, 0)
	if err != nil {
		t.Errorf("ReadAt failed: %v", err)
	}
	if n != 4 {
		t.Errorf("Expected to read 4 bytes, got %d", n)
	}

	data := []byte("write")
	n, err = backend.WriteAt(data, 0)
	if err != nil {
		t.Errorf("WriteAt failed: %v", err)
	}
	if n != len(data) {
		t.Errorf("Expected to write %d bytes, got %d", len(data), n)
	}
}

// TestConfigValidation tests configuration validation.
// Not parallelized: interacts with kernel resources.
func TestConfigValidation(t *testing.T) {
	backend := NewTestBackend(1024)

	tests := []struct {
		name   string
		config Config
		valid  bool
	}{
		{
			name: "valid config",
			config: Config{
				BlockSize:  512,
				Size:       1024 * 1024,
				MaxSectors: 256,
				NrHWQueues: 1,
				QueueDepth: 128,
			},
			valid: true,
		},
		{
			name: "zero block size",
			config: Config{
				BlockSize:  0,
				Size:       1024,
				MaxSectors: 256,
				NrHWQueues: 1,
				QueueDepth: 128,
			},
			valid: true,
		},
		{
			name: "zero size",
			config: Config{
				BlockSize:  512,
				Size:       0,
				MaxSectors: 256,
				NrHWQueues: 1,
				QueueDepth: 128,
			},
			valid: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := New(backend, tt.config)
			if tt.valid {
				if err != nil {
					t.Logf("Expected error (no root/kernel): %v", err)
				}
			} else {
				if err == nil {
					t.Error("Expected error for invalid config")
				}
			}
		})
	}
}

// TestDeviceLifecycle tests the device lifecycle (without actually creating).
// Not parallelized: interacts with kernel resources.
func TestDeviceLifecycle(t *testing.T) {
	backend := NewTestBackend(1024)

	dev, err := NewDevice(backend.ReadAt, backend.WriteAt)
	if err != nil {
		t.Logf("NewDevice error (expected without kernel): %v", err)
		return
	}

	err = dev.Add(1, 64)
	if err != nil {
		t.Logf("Add error (expected without root): %v", err)
		return
	}

	err = dev.SetParams(512, 1024*1024, 256, 0, 0)
	if err != nil {
		t.Logf("SetParams error: %v", err)
		return
	}

	err = dev.Start()
	if err != nil {
		t.Logf("Start error (expected without root): %v", err)
		return
	}

	err = dev.Stop()
	if err != nil {
		t.Logf("Stop error: %v", err)
	}

	err = dev.Delete()
	if err != nil {
		t.Logf("Delete error: %v", err)
	}
}

func TestBackendOperations(t *testing.T) {
	t.Parallel()
	backend := NewTestBackend(1024)

	data := []byte("Hello, ublk!")
	n, err := backend.WriteAt(data, 0)
	if err != nil {
		t.Fatalf("WriteAt failed: %v", err)
	}
	if n != len(data) {
		t.Errorf("Expected to write %d bytes, got %d", len(data), n)
	}

	buf := make([]byte, len(data))
	n, err = backend.ReadAt(buf, 0)
	if err != nil {
		t.Fatalf("ReadAt failed: %v", err)
	}
	if n != len(data) {
		t.Errorf("Expected to read %d bytes, got %d", len(data), n)
	}
	if string(buf) != string(data) {
		t.Error("Read data doesn't match written data")
	}

	_, err = backend.ReadAt(make([]byte, 10), 2000)
	if err == nil {
		t.Error("Expected error when reading beyond size")
	}
}

// TestConfigDefaults tests that New applies defaults.
// Not parallelized: interacts with kernel resources.
func TestConfigDefaults(t *testing.T) {
	backend := NewTestBackend(1024)

	config := Config{
		Size: 1024 * 1024,
	}

	defaultConfig := DefaultConfig()
	if defaultConfig.BlockSize == 0 {
		t.Error("Default block size should not be zero")
	}
	if defaultConfig.NrHWQueues == 0 {
		t.Error("Default number of queues should not be zero")
	}
	if defaultConfig.QueueDepth == 0 {
		t.Error("Default queue depth should not be zero")
	}

	_, err := New(backend, config)
	if err != nil {
		t.Logf("New error (expected): %v", err)
	}
}

func TestConfigValidate(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		config    Config
		expectErr bool
	}{
		{
			name:      "valid default config",
			config:    DefaultConfig(),
			expectErr: false,
		},
		{
			name:      "valid with defaults applied",
			config:    Config{},
			expectErr: false,
		},
		{
			name: "invalid block size not power of 2",
			config: Config{
				BlockSize:  513,
				Size:       1024 * 1024,
				QueueDepth: 128,
			},
			expectErr: true,
		},
		{
			name: "invalid block size too small",
			config: Config{
				BlockSize:  256,
				Size:       1024 * 1024,
				QueueDepth: 128,
			},
			expectErr: true,
		},
		{
			name: "invalid size not multiple of block size",
			config: Config{
				BlockSize:  512,
				Size:       1000,
				QueueDepth: 128,
			},
			expectErr: true,
		},
		{
			name: "invalid queue depth not power of 2",
			config: Config{
				BlockSize:  512,
				Size:       1024 * 1024,
				QueueDepth: 100,
			},
			expectErr: true,
		},
		{
			name: "valid 4K block size",
			config: Config{
				BlockSize:  4096,
				Size:       4096 * 1024,
				QueueDepth: 64,
				NrHWQueues: 2,
			},
			expectErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := tt.config.validate()
			if tt.expectErr && err == nil {
				t.Error("Expected validation error, got nil")
			}
			if !tt.expectErr && err != nil {
				t.Errorf("Expected no error, got: %v", err)
			}
		})
	}
}

func TestExtendedBackendInterfaces(t *testing.T) {
	t.Parallel()
	var _ Backend = (*TestBackend)(nil)
	var _ Backend = (*ReaderAtWriterAt)(nil)

	var backend Backend = NewTestBackend(1024)

	_, hasFlusher := backend.(Flusher)
	_, hasDiscarder := backend.(Discarder)
	_, hasWriteZeroer := backend.(WriteZeroer)

	if hasFlusher {
		t.Error("TestBackend should not implement Flusher")
	}
	if hasDiscarder {
		t.Error("TestBackend should not implement Discarder")
	}
	if hasWriteZeroer {
		t.Error("TestBackend should not implement WriteZeroer")
	}
}

// ExtendedTestBackend implements all optional interfaces.
type ExtendedTestBackend struct {
	TestBackend

	flushed     bool
	discarded   bool
	zeroWritten bool
	fuaWritten  bool
	readFlags   uint32
	writeFlags  uint32
}

func (b *ExtendedTestBackend) Flush() error {
	b.flushed = true
	return nil
}

func (b *ExtendedTestBackend) Discard(_, _ int64) error {
	b.discarded = true
	return nil
}

func (b *ExtendedTestBackend) WriteZeroes(_, _ int64) error {
	b.zeroWritten = true
	return nil
}

func (b *ExtendedTestBackend) WriteFua(p []byte, off int64) (int, error) {
	b.fuaWritten = true
	return b.WriteAt(p, off)
}

func (b *ExtendedTestBackend) ReadAtWithFlags(p []byte, off int64, flags uint32) (int, error) {
	b.readFlags = flags
	return b.ReadAt(p, off)
}

func (b *ExtendedTestBackend) WriteAtWithFlags(p []byte, off int64, flags uint32) (int, error) {
	b.writeFlags = flags
	return b.WriteAt(p, off)
}

func TestExtendedBackendImplementation(t *testing.T) {
	t.Parallel()
	backend := &ExtendedTestBackend{
		TestBackend: *NewTestBackend(1024),
	}

	var _ Backend = backend
	var _ Flusher = backend
	var _ Discarder = backend
	var _ WriteZeroer = backend
	var _ FuaWriter = backend
	var _ ReaderWithFlags = backend
	var _ WriterWithFlags = backend

	if err := backend.Flush(); err != nil {
		t.Errorf("Flush failed: %v", err)
	}
	if !backend.flushed {
		t.Error("Flush was not called")
	}

	if err := backend.Discard(0, 100); err != nil {
		t.Errorf("Discard failed: %v", err)
	}
	if !backend.discarded {
		t.Error("Discard was not called")
	}

	if err := backend.WriteZeroes(0, 100); err != nil {
		t.Errorf("WriteZeroes failed: %v", err)
	}
	if !backend.zeroWritten {
		t.Error("WriteZeroes was not called")
	}

	n, err := backend.WriteFua([]byte("test"), 0)
	if err != nil {
		t.Errorf("WriteFua failed: %v", err)
	}
	if n != 4 {
		t.Errorf("WriteFua returned short write: %d", n)
	}
	if !backend.fuaWritten {
		t.Error("WriteFua was not called")
	}

	// Test Flags
	_, err = backend.ReadAtWithFlags(make([]byte, 10), 0, 123)
	if err != nil {
		t.Errorf("ReadAtWithFlags failed: %v", err)
	}
	if backend.readFlags != 123 {
		t.Errorf("Read flags mismatch: expected 123, got %d", backend.readFlags)
	}

	_, err = backend.WriteAtWithFlags([]byte("test"), 0, 456)
	if err != nil {
		t.Errorf("WriteAtWithFlags failed: %v", err)
	}
	if backend.writeFlags != 456 {
		t.Errorf("Write flags mismatch: expected 456, got %d", backend.writeFlags)
	}
}
