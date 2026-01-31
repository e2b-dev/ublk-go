package ublk

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestBackend is a simple in-memory backend for tests.
type TestBackend struct {
	data []byte
	size int64
}

func NewTestBackend(size int64) *TestBackend {
	return &TestBackend{data: make([]byte, size), size: size}
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
	return copy(b.data[off:], p), nil
}

func (b *TestBackend) GetData() []byte { return b.data[:b.size] }

func TestConfigValidate(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		config    Config
		expectErr bool
	}{
		{"valid default", DefaultConfig(), false},
		{"block size not power of 2", Config{BlockSize: 513, Size: 1024 * 1024, QueueDepth: 128}, true},
		{"block size too small", Config{BlockSize: 256, Size: 1024 * 1024, QueueDepth: 128}, true},
		{"size not multiple of block size", Config{BlockSize: 512, Size: 1000, QueueDepth: 128}, true},
		{"queue depth not power of 2", Config{BlockSize: 512, Size: 1024 * 1024, QueueDepth: 100}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := tt.config.validate()
			if tt.expectErr {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

func TestReaderAtWriterAtNil(t *testing.T) {
	t.Parallel()
	backend := &ReaderAtWriterAt{}

	_, err := backend.ReadAt(make([]byte, 10), 0)
	require.Error(t, err, "nil ReaderAt should error")

	n, err := backend.WriteAt([]byte("test"), 0)
	require.NoError(t, err, "nil WriterAt should succeed")
	assert.Equal(t, 4, n)
}

func TestNew(t *testing.T) {
	backend := NewTestBackend(1024 * 1024)
	config := DefaultConfig()
	config.Size = 1024 * 1024

	dev, err := New(backend, config)
	if err != nil {
		t.Skipf("New not available: %v", err)
	}
	defer dev.Delete()

	assert.GreaterOrEqual(t, dev.DeviceID(), 0)
	assert.NotEmpty(t, dev.BlockDevicePath())
}
