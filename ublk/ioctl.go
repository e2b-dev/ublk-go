package ublk

import "unsafe"

// Ioctl encoding: (dir << 30) | (size << 16) | ('u' << 8) | cmd.
const (
	UBLK_CMD_GET_DEV_INFO uint32 = 0x02 // legacy
	UBLK_CMD_ADD_DEV      uint32 = 0x04
	UBLK_CMD_DEL_DEV      uint32 = 0x05
	UBLK_CMD_START_DEV    uint32 = 0x06
	UBLK_CMD_STOP_DEV     uint32 = 0x07
	UBLK_CMD_SET_PARAMS   uint32 = 0x08
	UBLK_CMD_GET_PARAMS   uint32 = 0x09

	UBLK_U_CMD_GET_DEV_INFO uintptr = 0x80207502
	UBLK_U_CMD_ADD_DEV      uintptr = 0xC0207504
	UBLK_U_CMD_DEL_DEV      uintptr = 0xC0207505
	UBLK_U_CMD_START_DEV    uintptr = 0xC0207506
	UBLK_U_CMD_STOP_DEV     uintptr = 0xC0207507
	UBLK_U_CMD_SET_PARAMS   uintptr = 0xC0207508
	UBLK_U_CMD_GET_PARAMS   uintptr = 0x80207509

	UBLK_U_IO_FETCH_REQ            uintptr = 0xC0107520
	UBLK_U_IO_COMMIT_AND_FETCH_REQ uintptr = 0xC0107521
	UBLK_U_IO_REGISTER_IO_BUF      uintptr = 0xC0107523
	UBLK_U_IO_UNREGISTER_IO_BUF    uintptr = 0xC0107524
)

type UblksrvCtrlCmd struct { // 32 bytes
	DevID      uint32
	QueueID    uint16
	Len        uint16
	Addr       uint64
	Data       [1]uint64
	DevPathLen uint16
	Pad        uint16
	Reserved   uint32
}

type UblksrvCtrlDevInfo struct { // 80 bytes
	NrHWQueues    uint16
	QueueDepth    uint16
	State         uint16
	Pad0          uint16
	MaxIOBufBytes uint32
	DevID         uint32
	UblksrvPID    int32
	Pad1          uint32
	Flags         uint64
	UblksrvFlags  uint64
	OwnerUID      uint32
	OwnerGID      uint32
	Reserved1     uint64
	Reserved2     uint64
}

const SizeOfCtrlDevInfo = unsafe.Sizeof(UblksrvCtrlDevInfo{})
