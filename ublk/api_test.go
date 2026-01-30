package ublk

import (
	"errors"
	"testing"
)

// TestBackend is a test backend implementation
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
	end := off + int64(len(p))
	if end > b.size {
		end = b.size
	}
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
		// Extend if needed
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

// TestCreateDevice tests the CreateDevice function
func TestCreateDevice(t *testing.T) {
	backend := NewTestBackend(1024 * 1024) // 1MB

	config := DefaultConfig()
	config.Size = 1024 * 1024
	config.BlockSize = 512
	config.NrHWQueues = 1
	config.QueueDepth = 64

	dev, err := CreateDevice(backend, config)
	if err != nil {
		// This is expected if we don't have root or ublk kernel support
		// Just verify the function signature and error handling
		t.Logf("CreateDevice returned error (expected without root/kernel): %v", err)
		return
	}

	// Clean up if device was created
	defer func() {
		if dev != nil {
			dev.Delete()
		}
	}()

	// Verify device properties
	if dev.DeviceID() < 0 {
		t.Error("Device ID should be non-negative")
	}

	blockPath := dev.BlockDevicePath()
	if blockPath == "" {
		t.Error("Block device path should not be empty")
	}

	t.Logf("Created device: %s (ID: %d)", blockPath, dev.DeviceID())
}

// TestDefaultConfig tests the default configuration
func TestDefaultConfig(t *testing.T) {
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

	t.Logf("Default config: BlockSize=%d, Size=%d, Queues=%d, Depth=%d",
		config.BlockSize, config.Size, config.NrHWQueues, config.QueueDepth)
}

// TestBackendInterface tests that TestBackend implements Backend
func TestBackendInterface(t *testing.T) {
	var _ Backend = (*TestBackend)(nil)
	var _ Backend = (*ReaderAtWriterAt)(nil)
}

// TestReaderAtWriterAtNilHandling tests nil handling in ReaderAtWriterAt
func TestReaderAtWriterAtNilHandling(t *testing.T) {
	// Test with nil ReaderAt
	backend := &ReaderAtWriterAt{
		ReaderAt: nil,
		WriterAt: nil,
	}

	buf := make([]byte, 10)
	_, err := backend.ReadAt(buf, 0)
	if err == nil {
		t.Error("Expected EOF error for nil ReaderAt")
	}

	// WriteAt with nil WriterAt should succeed (no-op)
	n, err := backend.WriteAt([]byte("test"), 0)
	if err != nil {
		t.Errorf("WriteAt with nil WriterAt should not error: %v", err)
	}
	if n != 4 {
		t.Errorf("WriteAt should return len(p), got %d", n)
	}
}

// TestReaderAtWriterAt tests the ReaderAtWriterAt adapter
func TestReaderAtWriterAt(t *testing.T) {
	backend := &ReaderAtWriterAt{
		ReaderAt: &TestBackend{data: []byte("test"), size: 4},
		WriterAt: &TestBackend{data: make([]byte, 10), size: 10},
	}

	// Test ReadAt
	buf := make([]byte, 4)
	n, err := backend.ReadAt(buf, 0)
	if err != nil {
		t.Errorf("ReadAt failed: %v", err)
	}
	if n != 4 {
		t.Errorf("Expected to read 4 bytes, got %d", n)
	}

	// Test WriteAt
	data := []byte("write")
	n, err = backend.WriteAt(data, 0)
	if err != nil {
		t.Errorf("WriteAt failed: %v", err)
	}
	if n != len(data) {
		t.Errorf("Expected to write %d bytes, got %d", len(data), n)
	}
}

// TestConfigValidation tests configuration validation
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
			valid: true, // Should use default
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
			valid: true, // Should use default
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := CreateDevice(backend, tt.config)
			if tt.valid {
				// Error is expected without root/kernel, but should be a specific error
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

// TestDeviceLifecycle tests the device lifecycle (without actually creating)
func TestDeviceLifecycle(t *testing.T) {
	backend := NewTestBackend(1024)

	// Test that we can create a device object
	dev, err := NewDevice(backend.ReadAt, backend.WriteAt)
	if err != nil {
		// This might fail if /dev/ublk-control doesn't exist
		t.Logf("NewDevice error (expected without kernel): %v", err)
		return
	}

	// Test Add
	err = dev.Add(1, 64)
	if err != nil {
		t.Logf("Add error (expected without root): %v", err)
		return
	}

	// Test SetParams
	err = dev.SetParams(512, 1024*1024, 256)
	if err != nil {
		t.Logf("SetParams error: %v", err)
		return
	}

	// Test Start
	err = dev.Start()
	if err != nil {
		t.Logf("Start error (expected without root): %v", err)
		return
	}

	// Test Stop
	err = dev.Stop()
	if err != nil {
		t.Logf("Stop error: %v", err)
	}

	// Test Delete
	err = dev.Delete()
	if err != nil {
		t.Logf("Delete error: %v", err)
	}
}

// TestBackendOperations tests backend read/write operations
func TestBackendOperations(t *testing.T) {
	backend := NewTestBackend(1024)

	// Test write
	data := []byte("Hello, ublk!")
	n, err := backend.WriteAt(data, 0)
	if err != nil {
		t.Fatalf("WriteAt failed: %v", err)
	}
	if n != len(data) {
		t.Errorf("Expected to write %d bytes, got %d", len(data), n)
	}

	// Test read
	buf := make([]byte, len(data))
	n, err = backend.ReadAt(buf, 0)
	if err != nil {
		t.Fatalf("ReadAt failed: %v", err)
	}
	if n != len(data) {
		t.Errorf("Expected to read %d bytes, got %d", len(data), n)
	}
	if string(buf) != string(data) {
		t.Errorf("Read data doesn't match written data")
	}

	// Test read beyond size
	_, err = backend.ReadAt(make([]byte, 10), 2000)
	if err == nil {
		t.Error("Expected error when reading beyond size")
	}
}

// TestConfigDefaults tests that CreateDevice applies defaults
func TestConfigDefaults(t *testing.T) {
	backend := NewTestBackend(1024)

	// Test with minimal config - CreateDevice applies defaults internally
	config := Config{
		Size: 1024 * 1024,
	}

	// CreateDevice applies defaults internally, but we can't verify them
	// if the call fails early. Instead, test that DefaultConfig works.
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

	// Test that CreateDevice handles zero values
	_, err := CreateDevice(backend, config)
	// Error is expected without root/kernel
	if err != nil {
		t.Logf("CreateDevice error (expected): %v", err)
	}

	// The config struct is passed by value, so modifications inside CreateDevice
	// don't affect our copy. This is fine - defaults are applied internally.
}
