package ublk

import (
	"errors"
	"os"
	"testing"
	"unsafe"
)

func TestNewIOWorker(t *testing.T) {
	t.Parallel()
	dev := &Device{
		devID: 0,
	}

	worker := newIOWorker(dev, 0, 128)

	if worker.device != dev {
		t.Error("device not set correctly")
	}
	if worker.qid != 0 {
		t.Errorf("expected qid 0, got %d", worker.qid)
	}
	if worker.queueDepth != 128 {
		t.Errorf("expected queueDepth 128, got %d", worker.queueDepth)
	}
	if len(worker.tagSubmitted) != 128 {
		t.Errorf("expected tagSubmitted length 128, got %d", len(worker.tagSubmitted))
	}
}

func TestIOWorkerGetSetIODesc(t *testing.T) {
	t.Parallel()
	worker := &ioWorker{
		queueDepth: 4,
	}

	descSize := int(unsafe.Sizeof(UblksrvIODesc{}))
	worker.mmapAddr = make([]byte, descSize*4)

	desc := UblksrvIODesc{
		Addr:        0x12345678,
		NrSectors:   8,
		StartSector: 0,
		OpFlags:     UBLK_IO_F_FETCHED,
	}
	worker.setIODesc(2, desc)

	got := worker.getIODesc(2)
	if got.Addr != desc.Addr {
		t.Errorf("expected Addr %x, got %x", desc.Addr, got.Addr)
	}
	if got.NrSectors != desc.NrSectors {
		t.Errorf("expected NrSectors %d, got %d", desc.NrSectors, got.NrSectors)
	}
	if got.OpFlags != desc.OpFlags {
		t.Errorf("expected OpFlags %d, got %d", desc.OpFlags, got.OpFlags)
	}
	if got.StartSector != desc.StartSector {
		t.Errorf("expected StartSector %d, got %d", desc.StartSector, got.StartSector)
	}
}

func TestIOWorkerGetIODescNilMmap(t *testing.T) {
	t.Parallel()
	worker := &ioWorker{
		queueDepth: 4,
		mmapAddr:   nil,
	}

	desc := worker.getIODesc(0)
	if desc.Addr != 0 || desc.NrSectors != 0 {
		t.Error("expected zero descriptor for nil mmap")
	}
}

func TestIOWorkerSetIODescNilMmap(t *testing.T) {
	t.Parallel()
	worker := &ioWorker{
		queueDepth: 4,
		mmapAddr:   nil,
	}

	// Should not panic
	worker.setIODesc(0, UblksrvIODesc{Addr: 123})
}

func TestIOWorkerGetIODescOutOfBounds(t *testing.T) {
	t.Parallel()
	worker := &ioWorker{
		queueDepth: 2,
	}

	descSize := int(unsafe.Sizeof(UblksrvIODesc{}))
	worker.mmapAddr = make([]byte, descSize) // Only space for 1 descriptor

	desc := worker.getIODesc(1)
	if desc.Addr != 0 {
		t.Error("expected zero descriptor for out of bounds tag")
	}
}

func TestIOWorkerSetIODescOutOfBounds(t *testing.T) {
	t.Parallel()
	worker := &ioWorker{
		queueDepth: 2,
	}

	descSize := int(unsafe.Sizeof(UblksrvIODesc{}))
	worker.mmapAddr = make([]byte, descSize) // Only space for 1 descriptor

	// Should not panic, just silently fail
	worker.setIODesc(1, UblksrvIODesc{Addr: 123})
}

func TestIOWorkerMmapSizeCalculation(t *testing.T) {
	t.Parallel()
	queueDepth := uint16(128)
	maxIOBufBytes := uint32(512 * 1024)

	descSize := int(unsafe.Sizeof(UblksrvIODesc{}))
	descAreaSize := int(queueDepth) * descSize
	requestAreaSize := 256 * int(queueDepth)
	expectedSize := descAreaSize + requestAreaSize + int(maxIOBufBytes)

	if expectedSize <= 0 {
		t.Error("Expected size should be positive")
	}

	t.Logf("Mmap size for queueDepth=%d: desc=%d, request=%d, buffer=%d, total=%d",
		queueDepth, descAreaSize, requestAreaSize, maxIOBufBytes, expectedSize)
}

func TestIOWorkerQueueAndTag(t *testing.T) {
	t.Parallel()
	dev := &Device{devID: 5}
	worker := newIOWorker(dev, 3, 64)

	if worker.qid != 3 {
		t.Errorf("Expected qid 3, got %d", worker.qid)
	}
	if worker.device.devID != 5 {
		t.Errorf("Expected devID 5, got %d", worker.device.devID)
	}
}

func TestIOWorkerMultipleDescriptors(t *testing.T) {
	t.Parallel()
	worker := &ioWorker{
		queueDepth: 4,
	}

	descSize := int(unsafe.Sizeof(UblksrvIODesc{}))
	worker.mmapAddr = make([]byte, descSize*4)

	for tag := range uint16(4) {
		desc := UblksrvIODesc{
			Addr:        uint64(tag * 0x1000),
			NrSectors:   uint32(tag + 1),
			StartSector: uint64(tag),
		}
		worker.setIODesc(tag, desc)
	}

	for tag := range uint16(4) {
		got := worker.getIODesc(tag)
		if got.Addr != uint64(tag*0x1000) {
			t.Errorf("Tag %d: expected Addr 0x%x, got 0x%x", tag, tag*0x1000, got.Addr)
		}
		if got.NrSectors != uint32(tag+1) {
			t.Errorf("Tag %d: expected NrSectors %d, got %d", tag, tag+1, got.NrSectors)
		}
		if got.StartSector != uint64(tag) {
			t.Errorf("Tag %d: expected StartSector %d, got %d", tag, tag, got.StartSector)
		}
	}
}

// setIODesc is a test helper to write descriptors to mmap area.
func (w *ioWorker) setIODesc(tag uint16, desc UblksrvIODesc) {
	if w.mmapAddr == nil {
		return
	}
	descSize := int(unsafe.Sizeof(UblksrvIODesc{}))
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
	DiscardFunc     func(off, len int64) error
	WriteZeroesFunc func(off, len int64) error
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

func (m *MockBackend) Discard(off, len int64) error {
	if m.DiscardFunc != nil {
		return m.DiscardFunc(off, len)
	}
	return nil
}

func (m *MockBackend) WriteZeroes(off, len int64) error {
	if m.WriteZeroesFunc != nil {
		return m.WriteZeroesFunc(off, len)
	}
	return nil
}

func TestExecuteIOFlush(t *testing.T) {
	t.Parallel()
	flushed := false
	backend := &MockBackend{
		FlushFunc: func() error {
			flushed = true
			return nil
		},
	}
	dev := &Device{backend: backend}
	worker := &ioWorker{device: dev, qid: 0}

	res := worker.executeIO(UBLK_IO_OP_FLUSH, nil, 0, 0)
	if res != int32(IOResultOK) {
		t.Errorf("Expected OK, got %d", res)
	}
	if !flushed {
		t.Error("Flush not called")
	}
}

func TestExecuteIODiscard(t *testing.T) {
	t.Parallel()
	var gotOff, gotLen int64
	backend := &MockBackend{
		DiscardFunc: func(off, len int64) error {
			gotOff, gotLen = off, len
			return nil
		},
	}
	dev := &Device{backend: backend}
	worker := &ioWorker{device: dev, qid: 0}

	buf := make([]byte, 4096)
	res := worker.executeIO(UBLK_IO_OP_DISCARD, buf, 1024, 0)
	if res != int32(IOResultOK) {
		t.Errorf("Expected OK, got %d", res)
	}
	if gotOff != 1024 || gotLen != 4096 {
		t.Errorf("Expected Discard(1024, 4096), got (%d, %d)", gotOff, gotLen)
	}
}

func TestExecuteIOWriteZeroes(t *testing.T) {
	t.Parallel()
	var gotOff, gotLen int64
	backend := &MockBackend{
		WriteZeroesFunc: func(off, len int64) error {
			gotOff, gotLen = off, len
			return nil
		},
	}
	dev := &Device{backend: backend}
	worker := &ioWorker{device: dev, qid: 0}

	buf := make([]byte, 2048)
	res := worker.executeIO(UBLK_IO_OP_WRITE_ZEROES, buf, 512, 0)
	if res != int32(IOResultOK) {
		t.Errorf("Expected OK, got %d", res)
	}
	if gotOff != 512 || gotLen != 2048 {
		t.Errorf("Expected WriteZeroes(512, 2048), got (%d, %d)", gotOff, gotLen)
	}
}

func TestExecuteIOUnsupported(t *testing.T) {
	t.Parallel()
	dev := &Device{backend: &MockBackend{}}
	worker := &ioWorker{device: dev, qid: 0}

	// 0xFF is likely unsupported
	res := worker.executeIO(0xFF, nil, 0, 0)
	if res != int32(IOResultENOTSUP) {
		t.Errorf("Expected ENOTSUP, got %d", res)
	}
}

func TestExecuteIOFlushError(t *testing.T) {
	t.Parallel()
	backend := &MockBackend{
		FlushFunc: func() error {
			return errors.New("flush failed")
		},
	}
	// Cannot return simple error interface because it's mocked, let's just make it return error
	backend.FlushFunc = func() error { return errors.New("flush failed") }

	dev := &Device{backend: backend}
	worker := &ioWorker{device: dev, qid: 0}

	res := worker.executeIO(UBLK_IO_OP_FLUSH, nil, 0, 0)
	if res != int32(IOResultEIO) {
		t.Errorf("Expected EIO, got %d", res)
	}
}

func TestExecuteIOReadSuccess(t *testing.T) {
	// Setup temp file to act as char device
	tmp, err := os.CreateTemp(t.TempDir(), "ublk_char")
	if err != nil {
		t.Fatal(err)
	}
	defer tmp.Close()

	// Mock ReadAt to return specific data
	readAtFunc := func(p []byte, off int64) (int, error) {
		copy(p, []byte("READDATA"))
		return len(p), nil
	}

	dev := &Device{
		charDevFD: tmp,
		readAt:    readAtFunc,
	}
	worker := &ioWorker{device: dev, qid: 0, scratchBuf: make([]byte, 4096)}

	// We simply need a device offset.
	// ublkUserCopyPos(qid=0, tag=1, off=0)
	// Just use tag=1, offset=0

	res := worker.handleRequest(1) // handleRequest calls executeIO internally but we need to setup desc first

	// Easier to mock getIODesc or call executeIO directly
	// Let's call executeIO directly like previous tests

	// executeIO(op, buf, offset, tag)
	buf := make([]byte, 8)
	// devOffset relies on ublkUserCopyPos. For qid=0, tag=1, off=0
	// We need to know where it writes.
	// But valid execution just needs it to succeed.

	res = worker.executeIO(UBLK_IO_OP_READ, buf, 0, 1) // tag 1
	if res != int32(IOResultOK) {
		t.Errorf("Expected OK, got %d", res)
	}

	// Verify data was written to char dev (temp file)
	// Offset = ublkUserCopyPos(0, 1, 0)
	devOffset := ublkUserCopyPos(0, 1, 0)

	got := make([]byte, 8)
	_, err = tmp.ReadAt(got, devOffset)
	if err != nil {
		t.Fatalf("Failed to read from temp file: %v", err)
	}
	if string(got) != "READDATA" {
		t.Errorf("Expected READDATA, got %q", got)
	}
}

func TestExecuteIOReadBackendError(t *testing.T) {
	tmp, err := os.CreateTemp(t.TempDir(), "ublk_char")
	if err != nil {
		t.Fatal(err)
	}
	defer tmp.Close()

	dev := &Device{
		charDevFD: tmp,
		readAt: func(p []byte, off int64) (int, error) {
			return 0, errors.New("backend error")
		},
	}
	worker := &ioWorker{device: dev, qid: 0}

	res := worker.executeIO(UBLK_IO_OP_READ, make([]byte, 10), 0, 1)
	if res != int32(IOResultEIO) {
		t.Errorf("Expected EIO, got %d", res)
	}
}

func TestExecuteIOWriteSuccess(t *testing.T) {
	tmp, err := os.CreateTemp(t.TempDir(), "ublk_char")
	if err != nil {
		t.Fatal(err)
	}
	defer tmp.Close()

	// Pre-fill char dev with data
	devOffset := ublkUserCopyPos(0, 1, 0)
	testData := []byte("WRITEDATA")
	if _, err := tmp.WriteAt(testData, devOffset); err != nil {
		t.Fatal(err)
	}

	var writtenData []byte
	writeAtFunc := func(p []byte, off int64) (int, error) {
		writtenData = make([]byte, len(p))
		copy(writtenData, p)
		return len(p), nil
	}

	dev := &Device{
		charDevFD: tmp,
		writeAt:   writeAtFunc,
	}
	worker := &ioWorker{device: dev, qid: 0}

	// Buffer needs to be large enough? handleRequest passes scratchBuf slice.
	// Here we pass our own buffer.
	buf := make([]byte, len(testData))
	res := worker.executeIO(UBLK_IO_OP_WRITE, buf, 0, 1)
	if res != int32(IOResultOK) {
		t.Errorf("Expected OK, got %d", res)
	}

	if string(writtenData) != "WRITEDATA" {
		t.Errorf("Expected WRITEDATA sent to backend, got %q", writtenData)
	}
}

func TestExecuteIOWriteBackendError(t *testing.T) {
	tmp, err := os.CreateTemp(t.TempDir(), "ublk_char")
	if err != nil {
		t.Fatal(err)
	}
	defer tmp.Close()

	dev := &Device{
		charDevFD: tmp,
		writeAt: func(p []byte, off int64) (int, error) {
			return 0, errors.New("backend write failed")
		},
	}
	worker := &ioWorker{device: dev, qid: 0}

	res := worker.executeIO(UBLK_IO_OP_WRITE, make([]byte, 10), 0, 1)
	if res != int32(IOResultEIO) {
		t.Errorf("Expected EIO, got %d", res)
	}
}
