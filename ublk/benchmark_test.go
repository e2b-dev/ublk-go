package ublk

import (
	"testing"
	"unsafe"
)

func BenchmarkGetSetIODesc(b *testing.B) {
	worker := &ioWorker{
		queueDepth: 128,
	}

	descSize := int(unsafe.Sizeof(UblksrvIODesc{}))
	worker.mmapAddr = make([]byte, descSize*128)

	desc := UblksrvIODesc{
		// Updated to match new struct fields
		StartSector: 0,
		NrSectors:   8,
		OpFlags:     UBLK_IO_F_FUA,
	}

	b.ResetTimer()
	for i := range b.N {
		tag := uint16(i % 128)
		worker.setIODesc(tag, desc)
		_ = worker.getIODesc(tag)
	}
}

func BenchmarkUblkIOCommandToBytes(b *testing.B) {
	cmd, _ := NewFetchReqCommand(1, 1, 0) // qid=1, tag=1
	b.ResetTimer()
	for b.Loop() {
		_ = cmd.ToBytes()
	}
}

func BenchmarkRoundUpPow2(b *testing.B) {
	b.ResetTimer()
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
