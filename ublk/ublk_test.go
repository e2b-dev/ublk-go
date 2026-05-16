package ublk

import (
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"unsafe"
)

func TestUblkStructSizes(t *testing.T) {
	t.Parallel()
	if unsafe.Sizeof(ctrlCmd{}) != 32 {
		t.Fatalf("ctrlCmd is %d bytes, kernel expects 32", unsafe.Sizeof(ctrlCmd{}))
	}
	if unsafe.Sizeof(devInfo{}) != 64 {
		t.Fatalf("devInfo is %d bytes, kernel expects 64", unsafe.Sizeof(devInfo{}))
	}
	if unsafe.Sizeof(ioCmd{}) != 16 {
		t.Fatalf("ioCmd is %d bytes, kernel expects 16", unsafe.Sizeof(ioCmd{}))
	}
	if unsafe.Sizeof(ioDesc{}) != 24 {
		t.Fatalf("ioDesc is %d bytes, kernel expects 24", unsafe.Sizeof(ioDesc{}))
	}
}

func TestNewValidation(t *testing.T) {
	t.Parallel()

	backend := newMemBackend(4096)
	tcs := []struct {
		name string
		size uint64
		opts []Option
	}{
		{name: "zero size", size: 0},
		{name: "size not multiple of block", size: 1000},
		{name: "block size below 512", size: 4096, opts: []Option{WithBlockSize(256)}},
		{name: "block size not pow2", size: 4096, opts: []Option{WithBlockSize(1024 + 512)}},
		{name: "queue depth too high", size: 4096, opts: []Option{WithQueueDepth(maxQueueDepth + 1)}},
		{name: "max IO not multiple of block", size: 1 << 20, opts: []Option{WithBlockSize(4096), WithMaxIOSize(5000)}},
		{name: "max IO exceeds 1 MiB cap", size: 2 << 20, opts: []Option{WithMaxIOSize(maxMaxIOSize + 512)}},
	}
	for _, tc := range tcs {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if _, err := New(backend, tc.size, tc.opts...); err == nil {
				t.Errorf("New(size=%d, %+v) returned nil error", tc.size, tc.opts)
			}
		})
	}
}

func TestNewNilBackend(t *testing.T) {
	t.Parallel()
	if _, err := New(nil, 4096); err == nil {
		t.Fatal("New(nil) should fail")
	}
	var typedNil *memBackend
	if _, err := New(typedNil, 4096); err == nil {
		t.Fatal("New(typed-nil backend) should fail")
	}
}

type memBackend struct {
	mu     sync.RWMutex
	data   []byte
	writes atomic.Int64
	reads  atomic.Int64
}

func newMemBackend(size int) *memBackend {
	return &memBackend{data: make([]byte, size)}
}

func (m *memBackend) ReadAt(p []byte, off int64) (int, error) {
	m.reads.Add(1)
	m.mu.RLock()
	defer m.mu.RUnlock()
	if off < 0 || int(off) >= len(m.data) {
		return 0, io.EOF
	}
	return copy(p, m.data[off:]), nil
}

func (m *memBackend) WriteAt(p []byte, off int64) (int, error) {
	m.writes.Add(1)
	m.mu.Lock()
	defer m.mu.Unlock()
	if off < 0 || int(off) >= len(m.data) {
		return 0, io.ErrShortWrite
	}
	return copy(m.data[off:], p), nil
}

func (m *memBackend) snapshot() []byte {
	m.mu.RLock()
	defer m.mu.RUnlock()
	s := make([]byte, len(m.data))
	copy(s, m.data)
	return s
}
