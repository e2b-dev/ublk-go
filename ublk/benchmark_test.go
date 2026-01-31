package ublk

import (
	"testing"
	"unsafe"
)

func BenchmarkGetSetIODesc(b *testing.B) {
	w := &ioWorker{queueDepth: 128}
	w.mmapAddr = make([]byte, int(unsafe.Sizeof(UblksrvIODesc{}))*128)
	desc := UblksrvIODesc{StartSector: 0, NrSectors: 8, OpFlags: UBLK_IO_F_FUA}

	b.ResetTimer()
	for i := range b.N {
		tag := uint16(i % 128)
		w.setIODesc(tag, desc)
		_ = w.getIODesc(tag)
	}
}

func BenchmarkUblkIOCommandToBytes(b *testing.B) {
	cmd, _ := NewFetchReqCommand(1, 1, 0)
	b.ResetTimer()
	for b.Loop() {
		_ = cmd.ToBytes()
	}
}

func BenchmarkRoundUpPow2(b *testing.B) {
	for i := range b.N {
		_ = roundUpPow2(uint(i % 4096))
	}
}

func BenchmarkTestBackendReadWrite(b *testing.B) {
	backend := NewTestBackend(1024 * 1024)
	buf := make([]byte, 4096)

	b.ResetTimer()
	for i := range b.N {
		offset := int64((i * 4096) % (1024 * 1024))
		_, _ = backend.WriteAt(buf, offset)
		_, _ = backend.ReadAt(buf, offset)
	}
}
