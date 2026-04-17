package ublk

import (
	"io"
	"testing"
	"unsafe"

	"golang.org/x/sys/unix"
)

type stubBackend struct {
	reads  int
	writes int

	readAt  func(p []byte, off int64) (int, error)
	writeAt func(p []byte, off int64) (int, error)
}

func (b *stubBackend) ReadAt(p []byte, off int64) (int, error) {
	b.reads++
	if b.readAt != nil {
		return b.readAt(p, off)
	}
	return len(p), nil
}

func (b *stubBackend) WriteAt(p []byte, off int64) (int, error) {
	b.writes++
	if b.writeAt != nil {
		return b.writeAt(p, off)
	}
	return len(p), nil
}

func newTestWorker(backend Backend) *worker {
	const depth = 1
	const bufSize = 4096
	w := &worker{
		dev:     &Device{backend: backend},
		depth:   depth,
		bufSize: bufSize,
		ioDescs: make([]byte, int(depth)*int(sizeofIODesc)),
		bufs:    make([][]byte, depth),
	}
	for i := range w.bufs {
		w.bufs[i] = make([]byte, bufSize)
	}
	return w
}

func (w *worker) setDesc(desc ioDesc) {
	*(*ioDesc)(unsafe.Pointer(&w.ioDescs[0])) = desc
}

func TestWorkerHandleIOUnsupportedOp(t *testing.T) {
	t.Parallel()

	backend := &stubBackend{}
	w := newTestWorker(backend)
	w.setDesc(ioDesc{
		OpFlags:   0xFF,
		NrSectors: 8,
	})

	if got := w.handleIO(0); got != -int32(unix.EOPNOTSUPP) {
		t.Fatalf("handleIO() = %d, want -EOPNOTSUPP", got)
	}
	if backend.reads != 0 || backend.writes != 0 {
		t.Fatalf("backend should not be touched for unsupported op; reads=%d writes=%d", backend.reads, backend.writes)
	}
}

func TestWorkerHandleIOZeroLength(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		op   uint32
	}{
		{name: "read", op: opRead},
		{name: "write", op: opWrite},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			backend := &stubBackend{}
			if tc.op == opRead {
				backend.readAt = func(_ []byte, _ int64) (int, error) {
					t.Fatal("ReadAt should not be called for zero-length read")
					return 0, nil
				}
			} else {
				backend.writeAt = func(_ []byte, _ int64) (int, error) {
					t.Fatal("WriteAt should not be called for zero-length write")
					return 0, nil
				}
			}
			w := newTestWorker(backend)
			w.setDesc(ioDesc{OpFlags: tc.op, NrSectors: 0, StartSector: 11})

			if got := w.handleIO(0); got != 0 {
				t.Fatalf("handleIO() = %d, want 0", got)
			}
			if backend.reads != 0 || backend.writes != 0 {
				t.Fatalf("backend should not be touched for zero-length IO; reads=%d writes=%d", backend.reads, backend.writes)
			}
		})
	}
}

func TestWorkerHandleIOTooLarge(t *testing.T) {
	t.Parallel()

	backend := &stubBackend{}
	w := newTestWorker(backend)
	w.setDesc(ioDesc{
		OpFlags:   opRead,
		NrSectors: uint32(4096/512 + 1),
	})

	if got := w.handleIO(0); got != -int32(unix.EIO) {
		t.Fatalf("handleIO() = %d, want -EIO", got)
	}
	if backend.reads != 0 || backend.writes != 0 {
		t.Fatalf("backend should not be touched for oversize IO; reads=%d writes=%d", backend.reads, backend.writes)
	}
}

func TestWorkerHandleIOShortRead(t *testing.T) {
	t.Parallel()

	backend := &stubBackend{
		readAt: func(p []byte, off int64) (int, error) {
			if off != 3*512 {
				t.Fatalf("ReadAt offset = %d, want %d", off, 3*512)
			}
			return len(p) - 512, nil
		},
	}
	w := newTestWorker(backend)
	w.setDesc(ioDesc{OpFlags: opRead, NrSectors: 8, StartSector: 3})

	if got := w.handleIO(0); got != -int32(unix.EIO) {
		t.Fatalf("handleIO() = %d, want -EIO", got)
	}
	if backend.reads != 1 {
		t.Fatalf("ReadAt calls = %d, want 1", backend.reads)
	}
}

func TestWorkerHandleIOShortWrite(t *testing.T) {
	t.Parallel()

	want := []byte("write payload")
	backend := &stubBackend{
		writeAt: func(p []byte, off int64) (int, error) {
			if off != 9*512 {
				t.Fatalf("WriteAt offset = %d, want %d", off, 9*512)
			}
			if string(p[:len(want)]) != string(want) {
				t.Fatalf("WriteAt payload = %q, want prefix %q", p[:len(want)], want)
			}
			return len(p) - 512, nil
		},
	}
	w := newTestWorker(backend)
	copy(w.bufs[0], want)
	w.setDesc(ioDesc{OpFlags: opWrite, NrSectors: 8, StartSector: 9})

	if got := w.handleIO(0); got != -int32(unix.EIO) {
		t.Fatalf("handleIO() = %d, want -EIO", got)
	}
	if backend.writes != 1 {
		t.Fatalf("WriteAt calls = %d, want 1", backend.writes)
	}
}

func TestAlignedAlloc(t *testing.T) {
	t.Parallel()

	buf := alignedAlloc(8192, 4096)
	if len(buf) != 8192 {
		t.Fatalf("len(alignedAlloc) = %d, want 8192", len(buf))
	}

	addr := uintptr(unsafe.Pointer(&buf[0]))
	if got := addr % 4096; got != 0 {
		t.Fatalf("buffer address mod 4096 = %d, want 0", got)
	}
}

func TestWorkerHandleIOReadError(t *testing.T) {
	t.Parallel()

	backend := &stubBackend{
		readAt: func(_ []byte, _ int64) (int, error) {
			return 0, io.EOF
		},
	}
	w := newTestWorker(backend)
	w.setDesc(ioDesc{OpFlags: opRead, NrSectors: 8})

	if got := w.handleIO(0); got != -int32(unix.EIO) {
		t.Fatalf("handleIO() = %d, want -EIO", got)
	}
}
