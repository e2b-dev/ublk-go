package ublk

import (
	"errors"
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

type stubZeroer struct {
	stubBackend
	zeroes        int
	writeZeroesAt func(off, length int64) (int, error)
}

func (b *stubZeroer) WriteZeroesAt(off, length int64) (int, error) {
	b.zeroes++
	if b.writeZeroesAt != nil {
		return b.writeZeroesAt(off, length)
	}
	return int(length), nil
}

func newTestWorker(backend Backend) *worker {
	const depth = 1
	const bufSize = 4096
	dev := &Device{backend: backend}
	if zw, ok := backend.(ZeroWriter); ok {
		dev.zeroer = zw
	}
	w := &worker{
		dev:     dev,
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
					return 0, errors.New("ReadAt should not be called for zero-length read")
				}
			} else {
				backend.writeAt = func(_ []byte, _ int64) (int, error) {
					return 0, errors.New("WriteAt should not be called for zero-length write")
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

func TestWorkerHandleIODiscardWithoutZeroer(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		op   uint32
	}{
		{name: "discard", op: opDiscard},
		{name: "write_zeroes", op: opWriteZeroes},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			backend := &stubBackend{}
			w := newTestWorker(backend)
			w.setDesc(ioDesc{OpFlags: tc.op, NrSectors: 8, StartSector: 1})

			if got := w.handleIO(0); got != -int32(unix.EOPNOTSUPP) {
				t.Fatalf("handleIO() = %d, want -EOPNOTSUPP", got)
			}
			if backend.reads != 0 || backend.writes != 0 {
				t.Fatalf("backend should not be touched; reads=%d writes=%d", backend.reads, backend.writes)
			}
		})
	}
}

func TestWorkerHandleIOZeroerOK(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		op   uint32
	}{
		{name: "discard", op: opDiscard},
		{name: "write_zeroes", op: opWriteZeroes},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			const sectors = 8
			const startSector = 11
			var gotOff, gotLen int64
			z := &stubZeroer{
				writeZeroesAt: func(off, length int64) (int, error) {
					gotOff, gotLen = off, length
					return int(length), nil
				},
			}
			w := newTestWorker(z)
			w.setDesc(ioDesc{OpFlags: tc.op, NrSectors: sectors, StartSector: startSector})

			if got := w.handleIO(0); got != sectors*512 {
				t.Fatalf("handleIO() = %d, want %d", got, sectors*512)
			}
			if gotOff != startSector*512 || gotLen != sectors*512 {
				t.Fatalf("WriteZeroesAt(off=%d, len=%d) want (%d, %d)", gotOff, gotLen, startSector*512, sectors*512)
			}
			if z.zeroes != 1 {
				t.Fatalf("WriteZeroesAt calls = %d, want 1", z.zeroes)
			}
		})
	}
}

func TestWorkerHandleIOZeroerLargeRequest(t *testing.T) {
	t.Parallel()

	// DISCARD / WRITE_ZEROES must not be capped by the data-buffer size,
	// since the kernel does not transfer any data for these ops.
	const sectors = 4096 // 2 MiB, well above the test worker bufSize (4 KiB)
	z := &stubZeroer{
		writeZeroesAt: func(_, length int64) (int, error) { return int(length), nil },
	}
	w := newTestWorker(z)
	w.setDesc(ioDesc{OpFlags: opWriteZeroes, NrSectors: sectors})

	if got := w.handleIO(0); got != sectors*512 {
		t.Fatalf("handleIO() = %d, want %d", got, sectors*512)
	}
}

func TestWorkerHandleIOZeroerError(t *testing.T) {
	t.Parallel()

	z := &stubZeroer{
		writeZeroesAt: func(_, _ int64) (int, error) { return 0, io.EOF },
	}
	w := newTestWorker(z)
	w.setDesc(ioDesc{OpFlags: opDiscard, NrSectors: 8})

	if got := w.handleIO(0); got != -int32(unix.EIO) {
		t.Fatalf("handleIO() = %d, want -EIO", got)
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
