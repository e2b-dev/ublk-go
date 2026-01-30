package ublk

import "unsafe"

// UringSQE represents an io_uring submission queue entry.
type UringSQE struct {
	Opcode      uint8
	Flags       uint8
	Ioprio      uint16
	Fd          int32
	Off         uint64
	Addr        uint64
	Len         uint32
	OpFlags     uint32
	UserData    uint64
	BufIndex    uint16
	Personality uint16
	SpliceFdIn  int32
	Addr2       [2]uint64
}

// UringSQE128 represents a 128-byte io_uring submission queue entry.
// With IORING_SETUP_SQE128, the SQE size is doubled to 128 bytes.
// The extra space is used for commands (like ublk) that need more than a pointer.
type UringSQE128 struct {
	// Standard SQE fields (first 48 bytes)
	Opcode      uint8
	Flags       uint8
	Ioprio      uint16
	Fd          int32
	Off         uint64
	Addr        uint64
	Len         uint32
	OpFlags     uint32
	UserData    uint64
	BufIndex    uint16
	Personality uint16
	SpliceFdIn  int32

	// Extended 80 bytes for commands (starts at offset 48)
	// This covers the area of addr2/pad (16 bytes) + extended 64 bytes
	Cmd [80]byte
}

// SizeOfUringSQE128 returns the size of struct io_uring_sqe (128 bytes).
func SizeOfUringSQE128() uintptr {
	return unsafe.Sizeof(UringSQE128{})
}

// UringCQE represents an io_uring completion queue entry.
type UringCQE struct {
	UserData uint64
	Res      int32
	Flags    uint32
	BigCQE   [2]uint64
}

// UringParams represents io_uring parameters.
type UringParams struct {
	SQEntries    uint32
	CQEntries    uint32
	Flags        uint32
	SqThreadCPU  uint32
	SqThreadIdle uint32
	Features     uint32
	WqFd         uint32
	Resv         [3]uint32
	SqOff        UringParamsSQ
	CqOff        UringParamsCQ
}

type UringParamsSQ struct {
	Head        uint32
	Tail        uint32
	RingMask    uint32
	RingEntries uint32
	Flags       uint32
	Dropped     uint32
	Array       uint32
	Resv1       uint32
	Resv2       uint64
}

type UringParamsCQ struct {
	Head        uint32
	Tail        uint32
	RingMask    uint32
	RingEntries uint32
	Overflow    uint32
	Cqes        uint32
	Flags       uint32
	Resv1       uint32
	Resv2       uint64
}

// SizeOfUringSQE returns the size of struct io_uring_sqe.
func SizeOfUringSQE() uintptr {
	return unsafe.Sizeof(UringSQE{})
}

// SizeOfUringCQE returns the size of struct io_uring_cqe.
func SizeOfUringCQE() uintptr {
	return unsafe.Sizeof(UringCQE{})
}

// SizeOfUringParams returns the size of struct io_uring_params.
func SizeOfUringParams() uintptr {
	return unsafe.Sizeof(UringParams{})
}
