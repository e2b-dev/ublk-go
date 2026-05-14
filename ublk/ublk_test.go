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

func TestConfigValidate(t *testing.T) {
	t.Parallel()

	tcs := []struct {
		name    string
		cfg     Config
		wantErr bool
	}{
		{name: "ok minimal", cfg: Config{Size: 4096}},
		{name: "ok 4k blocks", cfg: Config{Size: 1 << 20, BlockSize: 4096}},
		{name: "ok all set", cfg: Config{Size: 1 << 20, BlockSize: 4096, QueueDepth: 64, MaxIOSize: 64 * 1024}},
		{name: "zero size", cfg: Config{Size: 0}, wantErr: true},
		{name: "size not multiple of block", cfg: Config{Size: 1000}, wantErr: true},
		{name: "block size below 512", cfg: Config{Size: 4096, BlockSize: 256}, wantErr: true},
		{name: "block size not pow2", cfg: Config{Size: 4096, BlockSize: 1024 + 512}, wantErr: true},
		{name: "queue depth too high", cfg: Config{Size: 4096, QueueDepth: maxQueueDepth + 1}, wantErr: true},
		{name: "max io not multiple of block", cfg: Config{Size: 1 << 20, BlockSize: 4096, MaxIOSize: 5000}, wantErr: true},
	}

	for _, tc := range tcs {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg := tc.cfg
			cfg.applyDefaults()
			err := cfg.validate()
			if tc.wantErr != (err != nil) {
				t.Fatalf("validate() err = %v, wantErr = %v", err, tc.wantErr)
			}
		})
	}
}

func TestNewInvalidSize(t *testing.T) {
	t.Parallel()
	backend := newMemBackend(4096)

	if _, err := New(backend, Config{Size: 0}); err == nil {
		t.Error("New(size=0) should fail")
	}
	if _, err := New(backend, Config{Size: 1000}); err == nil {
		t.Error("New(size=1000) should fail (not multiple of 512)")
	}
}

func TestNewNilBackend(t *testing.T) {
	t.Parallel()

	if _, err := New(nil, Config{Size: 4096}); err == nil {
		t.Fatal("New(nil) should fail")
	}

	var typedNil *memBackend
	if _, err := New(typedNil, Config{Size: 4096}); err == nil {
		t.Fatal("New(typed nil backend) should fail")
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
