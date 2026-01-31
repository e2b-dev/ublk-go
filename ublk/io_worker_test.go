package ublk

import (
	"errors"
	"os"
	"testing"
	"unsafe"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestIOWorkerGetSetIODesc(t *testing.T) {
	t.Parallel()
	w := &ioWorker{queueDepth: 4}
	w.mmapAddr = make([]byte, int(SizeOfUblksrvIODesc)*4)

	desc := UblksrvIODesc{Addr: 0x12345678, NrSectors: 8, StartSector: 100, OpFlags: UBLK_IO_F_FUA}
	w.setIODesc(2, desc)

	got := w.getIODesc(2)
	assert.Equal(t, desc.Addr, got.Addr)
	assert.Equal(t, desc.NrSectors, got.NrSectors)
	assert.Equal(t, desc.StartSector, got.StartSector)
	assert.Equal(t, desc.OpFlags, got.OpFlags)
}

func TestIOWorkerDescBoundaryConditions(t *testing.T) {
	t.Parallel()
	// Nil mmap should not panic
	w := &ioWorker{queueDepth: 4, mmapAddr: nil}
	assert.Zero(t, w.getIODesc(0).Addr)
	w.setIODesc(0, UblksrvIODesc{Addr: 123}) // Should not panic

	// Out of bounds should not panic
	w2 := &ioWorker{queueDepth: 2}
	w2.mmapAddr = make([]byte, int(SizeOfUblksrvIODesc)) // Only 1 descriptor
	assert.Zero(t, w2.getIODesc(1).Addr)
	w2.setIODesc(1, UblksrvIODesc{Addr: 123}) // Should not panic
}

// setIODesc is a test helper to write descriptors to mmap area.
func (w *ioWorker) setIODesc(tag uint16, desc UblksrvIODesc) {
	if w.mmapAddr == nil {
		return
	}
	descSize := int(SizeOfUblksrvIODesc)
	offset := int(tag) * descSize
	if offset+descSize > len(w.mmapAddr) {
		return
	}
	*(*UblksrvIODesc)(unsafe.Pointer(&w.mmapAddr[offset])) = desc
}

// MockBackend for testing IO operations.
type MockBackend struct {
	ReadAtFunc      func(p []byte, off int64) (int, error)
	WriteAtFunc     func(p []byte, off int64) (int, error)
	FlushFunc       func() error
	DiscardFunc     func(off, length int64) error
	WriteZeroesFunc func(off, length int64) error
}

func (m *MockBackend) ReadAt(p []byte, off int64) (int, error) {
	if m.ReadAtFunc != nil {
		return m.ReadAtFunc(p, off)
	}
	return len(p), nil
}

func (m *MockBackend) WriteAt(p []byte, off int64) (int, error) {
	if m.WriteAtFunc != nil {
		return m.WriteAtFunc(p, off)
	}
	return len(p), nil
}

func (m *MockBackend) Flush() error {
	if m.FlushFunc != nil {
		return m.FlushFunc()
	}
	return nil
}

func (m *MockBackend) Discard(off, length int64) error {
	if m.DiscardFunc != nil {
		return m.DiscardFunc(off, length)
	}
	return nil
}

func (m *MockBackend) WriteZeroes(off, length int64) error {
	if m.WriteZeroesFunc != nil {
		return m.WriteZeroesFunc(off, length)
	}
	return nil
}

func TestExecuteIOFlush(t *testing.T) {
	t.Parallel()
	flushed := false
	backend := &MockBackend{FlushFunc: func() error { flushed = true; return nil }}
	w := &ioWorker{device: &Device{backend: backend}}

	assert.Equal(t, IOResultOK, w.executeIO(UBLK_IO_OP_FLUSH, 0, nil, 0, 0))
	assert.True(t, flushed)
}

func TestExecuteIOFlushError(t *testing.T) {
	t.Parallel()
	backend := &MockBackend{FlushFunc: func() error { return errors.New("fail") }}
	w := &ioWorker{device: &Device{backend: backend}}
	assert.Equal(t, IOResultEIO, w.executeIO(UBLK_IO_OP_FLUSH, 0, nil, 0, 0))
}

func TestExecuteIODiscard(t *testing.T) {
	t.Parallel()
	var gotOff, gotLen int64
	backend := &MockBackend{DiscardFunc: func(off, length int64) error { gotOff, gotLen = off, length; return nil }}
	w := &ioWorker{device: &Device{backend: backend}}

	assert.Equal(t, IOResultOK, w.executeIO(UBLK_IO_OP_DISCARD, 0, make([]byte, 4096), 1024, 0))
	assert.Equal(t, int64(1024), gotOff)
	assert.Equal(t, int64(4096), gotLen)
}

func TestExecuteIOWriteZeroes(t *testing.T) {
	t.Parallel()
	var gotOff, gotLen int64
	backend := &MockBackend{WriteZeroesFunc: func(off, length int64) error { gotOff, gotLen = off, length; return nil }}
	w := &ioWorker{device: &Device{backend: backend}}

	assert.Equal(t, IOResultOK, w.executeIO(UBLK_IO_OP_WRITE_ZEROES, 0, make([]byte, 2048), 512, 0))
	assert.Equal(t, int64(512), gotOff)
	assert.Equal(t, int64(2048), gotLen)
}

func TestExecuteIOUnsupported(t *testing.T) {
	t.Parallel()
	w := &ioWorker{device: &Device{backend: &MockBackend{}}}
	assert.Equal(t, IOResultENOTSUP, w.executeIO(0xFF, 0, nil, 0, 0))
}

func TestExecuteIOReadWrite(t *testing.T) {
	tmp, err := os.CreateTemp(t.TempDir(), "ublk_char")
	require.NoError(t, err)
	defer tmp.Close()

	// Test READ: backend data -> char device
	dev := &Device{
		charDevFD: tmp,
		readAt:    func(p []byte, _ int64) (int, error) { copy(p, []byte("READDATA")); return len(p), nil },
	}
	w := &ioWorker{device: dev, userCopy: true, scratchBuf: make([]byte, 4096)}

	assert.Equal(t, IOResultOK, w.executeIO(UBLK_IO_OP_READ, 0, make([]byte, 8), 0, 1))

	got := make([]byte, 8)
	_, err = tmp.ReadAt(got, ublkUserCopyPos(0, 1))
	require.NoError(t, err)
	assert.Equal(t, "READDATA", string(got))

	// Test WRITE: char device -> backend
	devOffset := ublkUserCopyPos(0, 2)
	_, err = tmp.WriteAt([]byte("WRITEDATA"), devOffset)
	require.NoError(t, err)

	var writtenData []byte
	dev.writeAt = func(p []byte, _ int64) (int, error) {
		writtenData = make([]byte, len(p))
		copy(writtenData, p)
		return len(p), nil
	}

	assert.Equal(t, IOResultOK, w.executeIO(UBLK_IO_OP_WRITE, 0, make([]byte, 9), 0, 2))
	assert.Equal(t, "WRITEDATA", string(writtenData))
}

func TestExecuteIOErrors(t *testing.T) {
	tmp, err := os.CreateTemp(t.TempDir(), "ublk_char")
	require.NoError(t, err)
	defer tmp.Close()

	// Backend read error
	dev := &Device{
		charDevFD: tmp,
		readAt:    func(_ []byte, _ int64) (int, error) { return 0, errors.New("fail") },
	}
	w := &ioWorker{device: dev, userCopy: true}
	assert.Equal(t, IOResultEIO, w.executeIO(UBLK_IO_OP_READ, 0, make([]byte, 10), 0, 1))

	// Backend write error - pre-fill char dev
	devOffset := ublkUserCopyPos(0, 1)
	_, err = tmp.WriteAt(make([]byte, 10), devOffset)
	require.NoError(t, err)

	dev.writeAt = func(_ []byte, _ int64) (int, error) { return 0, errors.New("fail") }
	assert.Equal(t, IOResultEIO, w.executeIO(UBLK_IO_OP_WRITE, 0, make([]byte, 10), 0, 1))
}

// BackendWithFlags for testing flag propagation.
type BackendWithFlags struct {
	MockBackend

	readFlags  uint32
	writeFlags uint32
}

func (b *BackendWithFlags) ReadAtWithFlags(p []byte, off int64, flags uint32) (int, error) {
	b.readFlags = flags
	return b.ReadAt(p, off)
}

func (b *BackendWithFlags) WriteAtWithFlags(p []byte, off int64, flags uint32) (int, error) {
	b.writeFlags = flags
	return b.WriteAt(p, off)
}

func TestExecuteIOWithFlags(t *testing.T) {
	tmp, err := os.CreateTemp(t.TempDir(), "ublk_char")
	require.NoError(t, err)
	defer tmp.Close()

	_, err = tmp.WriteAt(make([]byte, 1024), ublkUserCopyPos(0, 1))
	require.NoError(t, err)

	backend := &BackendWithFlags{}
	dev := &Device{charDevFD: tmp, backend: backend, readAt: backend.ReadAt, writeAt: backend.WriteAt}
	w := &ioWorker{device: dev, userCopy: true}

	w.executeIO(UBLK_IO_OP_READ, 0x100, make([]byte, 8), 0, 1)
	assert.Equal(t, uint32(0x100), backend.readFlags)

	w.executeIO(UBLK_IO_OP_WRITE, 0x200, []byte("TEST"), 0, 1)
	assert.Equal(t, uint32(0x200), backend.writeFlags)
}
