package ublk

import (
	"io"
	"log/slog"
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

func TestNewInvalidSize(t *testing.T) {
	t.Parallel()
	backend := newMemBackend(4096)

	if _, err := New(backend, 0); err == nil {
		t.Error("New(size=0) should fail")
	}
	if _, err := New(backend, 1000); err == nil {
		t.Error("New(size=1000) should fail (not multiple of 512)")
	}
}

func TestWithLoggerNilIsNoOp(t *testing.T) {
	t.Parallel()
	d := &Device{log: slog.Default()}
	WithLogger(nil)(d)
	if d.log == nil {
		t.Fatal("WithLogger(nil) must not clear the logger")
	}
}

func TestNewNilBackend(t *testing.T) {
	t.Parallel()

	if _, err := New(nil, 4096); err == nil {
		t.Fatal("New(nil, 4096) should fail")
	}

	var typedNil *memBackend
	if _, err := New(typedNil, 4096); err == nil {
		t.Fatal("New(typed nil backend, 4096) should fail")
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
