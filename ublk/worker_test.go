package ublk

import (
	"io"
	"log/slog"
	"testing"
	"unsafe"

	"golang.org/x/sys/unix"
)

type stubBackend struct {
	readAt  func([]byte, int64) (int, error)
	writeAt func([]byte, int64) (int, error)
}

func (b *stubBackend) ReadAt(p []byte, off int64) (int, error) {
	if b.readAt != nil {
		return b.readAt(p, off)
	}
	return len(p), nil
}

func (b *stubBackend) WriteAt(p []byte, off int64) (int, error) {
	if b.writeAt != nil {
		return b.writeAt(p, off)
	}
	return len(p), nil
}

func newTestWorker(backend Backend) *worker {
	const depth, bufSize = 1, 4096
	w := &worker{
		dev:     &Device{backend: backend, log: slog.Default()},
		depth:   depth,
		bufSize: bufSize,
		ioDescs: make([]byte, int(depth)*int(sizeofIODesc)),
		bufs:    [][]byte{make([]byte, bufSize)},
	}
	return w
}

func (w *worker) setDesc(desc ioDesc) {
	*(*ioDesc)(unsafe.Pointer(&w.ioDescs[0])) = desc
}

func TestWorkerHandleIO(t *testing.T) {
	t.Parallel()

	eio := -int32(unix.EIO)
	shortBy1 := func(p []byte, _ int64) (int, error) { return len(p) - 1, nil }

	for _, tc := range []struct {
		name    string
		desc    ioDesc
		readAt  func([]byte, int64) (int, error)
		writeAt func([]byte, int64) (int, error)
		want    int32
	}{
		{"unsupported op", ioDesc{OpFlags: 0xFF, NrSectors: 8}, nil, nil, -int32(unix.EOPNOTSUPP)},
		{"read zero-length", ioDesc{OpFlags: opRead}, nil, nil, 0},
		{"write zero-length", ioDesc{OpFlags: opWrite}, nil, nil, 0},
		{"too large", ioDesc{OpFlags: opRead, NrSectors: uint32(4096/512 + 1)}, nil, nil, eio},
		{"short read", ioDesc{OpFlags: opRead, NrSectors: 8}, shortBy1, nil, eio},
		{"short write", ioDesc{OpFlags: opWrite, NrSectors: 8}, nil, shortBy1, eio},
		{"read error", ioDesc{OpFlags: opRead, NrSectors: 8}, func(_ []byte, _ int64) (int, error) { return 0, io.EOF }, nil, eio},
		{"read panic", ioDesc{OpFlags: opRead, NrSectors: 8}, func(_ []byte, _ int64) (int, error) { panic("oops") }, nil, eio},
		{"write panic", ioDesc{OpFlags: opWrite, NrSectors: 8}, nil, func(_ []byte, _ int64) (int, error) { panic("oops") }, eio},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			w := newTestWorker(&stubBackend{readAt: tc.readAt, writeAt: tc.writeAt})
			w.setDesc(tc.desc)
			if got := w.handleIO(0); got != tc.want {
				t.Fatalf("handleIO() = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestAlignedAlloc(t *testing.T) {
	t.Parallel()
	buf := alignedAlloc(8192, 4096)
	if len(buf) != 8192 {
		t.Fatalf("len = %d, want 8192", len(buf))
	}
	if uintptr(unsafe.Pointer(&buf[0]))%4096 != 0 {
		t.Fatal("buffer not 4096-aligned")
	}
}
