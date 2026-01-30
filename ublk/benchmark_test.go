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
		Addr:    0x12345678,
		Length:  4096,
		OpFlags: UBLK_IO_F_FETCHED,
		Tag:     0,
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		tag := uint16(i % 128)
		worker.setIODesc(tag, desc)
		_ = worker.getIODesc(tag)
	}
}

func BenchmarkBufferManagerGetIODescBuffer(b *testing.B) {
	queueDepth := uint16(128)
	descSize := int(unsafe.Sizeof(UblksrvIODesc{}))
	descArea := int(queueDepth) * descSize
	requestArea := 256 * int(queueDepth)
	bufferSize := 512 * 1024
	total := descArea + requestArea + bufferSize

	mmapAddr := make([]byte, total)
	bm := NewBufferManager(mmapAddr, queueDepth)

	desc := UblksrvIODesc{
		Addr:   1000,
		Length: 4096,
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = bm.GetIODescBuffer(desc)
	}
}

func BenchmarkBufferManagerGetRequestData(b *testing.B) {
	queueDepth := uint16(128)
	descSize := int(unsafe.Sizeof(UblksrvIODesc{}))
	descArea := int(queueDepth) * descSize
	requestArea := 256 * int(queueDepth)
	bufferSize := 512 * 1024
	total := descArea + requestArea + bufferSize

	mmapAddr := make([]byte, total)
	bm := NewBufferManager(mmapAddr, queueDepth)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		tag := uint16(i % int(queueDepth))
		_, _ = bm.GetRequestData(tag)
	}
}

func BenchmarkParseRequest(b *testing.B) {
	buf := make([]byte, unsafe.Sizeof(UblkRequest{}))
	req := (*UblkRequest)(unsafe.Pointer(&buf[0]))
	req.StartSector = 8
	req.NSectors = 16
	req.Op = UBLK_IO_OP_READ

	desc := UblksrvIODesc{}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = ParseRequest(desc, buf)
	}
}

func BenchmarkUblkIOCommandToBytes(b *testing.B) {
	cmd := NewFetchReqCommand(0, 0, 0)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = cmd.ToBytes()
	}
}

func BenchmarkRoundUpPow2(b *testing.B) {
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = roundUpPow2(uint(i % 4096))
	}
}

func BenchmarkTestBackendReadWrite(b *testing.B) {
	backend := NewTestBackend(1024 * 1024)
	buf := make([]byte, 4096)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		offset := int64((i * 4096) % (1024 * 1024))
		_, _ = backend.WriteAt(buf, offset)
		_, _ = backend.ReadAt(buf, offset)
	}
}
