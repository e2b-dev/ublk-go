package ublk

import "unsafe"

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

// UringSQE128 is a 128-byte SQE with extended command space.
type UringSQE128 struct {
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
	Cmd         [80]byte // extended command space
}

const SizeOfUringSQE128 = unsafe.Sizeof(UringSQE128{})

type UringCQE struct {
	UserData uint64
	Res      int32
	Flags    uint32
	BigCQE   [2]uint64
}

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

const (
	SizeOfUringSQE    = unsafe.Sizeof(UringSQE{})
	SizeOfUringCQE    = unsafe.Sizeof(UringCQE{})
	SizeOfUringParams = unsafe.Sizeof(UringParams{})
)
