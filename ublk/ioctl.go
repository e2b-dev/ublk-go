package ublk

import (
	"unsafe"
)

// Linux ioctl encoding constants.
const (
	iocWrite = 1
	iocRead  = 2

	iocNrBits   = 8
	iocTypeBits = 8
	iocSizeBits = 14

	iocNrShift   = 0
	iocTypeShift = iocNrShift + iocNrBits
	iocSizeShift = iocTypeShift + iocTypeBits
	iocDirShift  = iocSizeShift + iocSizeBits
)

func ioc(dir, typ, nr, size uintptr) uintptr {
	return (dir << iocDirShift) | (typ << iocTypeShift) | (nr << iocNrShift) | (size << iocSizeShift)
}

func ior(nr, size uintptr) uintptr {
	return ioc(iocRead, ublkIoctlType, nr, size)
}

func iowr(nr, size uintptr) uintptr {
	return ioc(iocRead|iocWrite, ublkIoctlType, nr, size)
}

// ublk ioctl type.
const ublkIoctlType = 'u'

// Raw command numbers (from kernel header).
const (
	ublkCmdGetDevInfo = 0x02
	ublkCmdAddDev     = 0x04
	ublkCmdDelDev     = 0x05
	ublkCmdStartDev   = 0x06
	ublkCmdStopDev    = 0x07
	ublkCmdSetParams  = 0x08
	ublkCmdGetParams  = 0x09
)

// Raw IO command numbers (from kernel header).
const (
	ublkIOFetchReq          = 0x20
	ublkIOCommitAndFetchReq = 0x21
	ublkIORegisterIOBuf     = 0x23
	ublkIOUnregisterIOBuf   = 0x24
)

// UblksrvCtrlCmd is the control command structure passed to ioctls.
// It wraps a pointer to the actual data (like dev info or params).
type UblksrvCtrlCmd struct {
	DevID      uint32 // device ID
	QueueID    uint16 // queue ID (-1 if not for queue)
	Len        uint16 // length of data at Addr
	Addr       uint64 // pointer to data buffer
	Data       [1]uint64
	DevPathLen uint16
	Pad        uint16
	Reserved   uint32
}

// UblksrvCtrlDevInfo is the device info structure.
type UblksrvCtrlDevInfo struct {
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

// Computed ioctl commands (with _IOWR encoding for newer kernels).
var (
	ctrlCmdSize = unsafe.Sizeof(UblksrvCtrlCmd{})
	ioCmdSize   = unsafe.Sizeof(UblkIOCommand{})

	UBLK_U_CMD_ADD_DEV      = iowr(ublkCmdAddDev, ctrlCmdSize)
	UBLK_U_CMD_DEL_DEV      = iowr(ublkCmdDelDev, ctrlCmdSize)
	UBLK_U_CMD_START_DEV    = iowr(ublkCmdStartDev, ctrlCmdSize)
	UBLK_U_CMD_STOP_DEV     = iowr(ublkCmdStopDev, ctrlCmdSize)
	UBLK_U_CMD_SET_PARAMS   = iowr(ublkCmdSetParams, ctrlCmdSize)
	UBLK_U_CMD_GET_PARAMS   = ior(ublkCmdGetParams, ctrlCmdSize)
	UBLK_U_CMD_GET_DEV_INFO = ior(ublkCmdGetDevInfo, ctrlCmdSize)

	UBLK_U_IO_FETCH_REQ            = iowr(ublkIOFetchReq, ioCmdSize)
	UBLK_U_IO_COMMIT_AND_FETCH_REQ = iowr(ublkIOCommitAndFetchReq, ioCmdSize)
	UBLK_U_IO_REGISTER_IO_BUF      = iowr(ublkIORegisterIOBuf, ioCmdSize)
	UBLK_U_IO_UNREGISTER_IO_BUF    = iowr(ublkIOUnregisterIOBuf, ioCmdSize)
)

// SizeOfCtrlDevInfo returns the size of UblksrvCtrlDevInfo.
func SizeOfCtrlDevInfo() uintptr {
	return unsafe.Sizeof(UblksrvCtrlDevInfo{})
}
