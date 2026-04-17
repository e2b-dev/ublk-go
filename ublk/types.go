package ublk

import "unsafe"

const (
	opRead  = 0
	opWrite = 1

	flagCmdIoctlEncode = 1 << 6

	maxQueueDepth = 4096

	paramTypeBasic = 1 << 0
)

const (
	cmdAddDev    uint32 = 0x04
	cmdDelDev    uint32 = 0x05
	cmdStartDev  uint32 = 0x06
	cmdStopDev   uint32 = 0x07
	cmdSetParams uint32 = 0x08
)

const (
	uCmdAddDev    uint32 = 0xC0207504
	uCmdDelDev    uint32 = 0xC0207505
	uCmdStartDev  uint32 = 0xC0207506
	uCmdStopDev   uint32 = 0xC0207507
	uCmdSetParams uint32 = 0xC0207508
)

const (
	uIOFetchReq          uint32 = 0xC0107520
	uIOCommitAndFetchReq uint32 = 0xC0107521
)

type ctrlCmd struct {
	DevID      uint32
	QueueID    uint16
	Len        uint16
	Addr       uint64
	Data       [1]uint64
	DevPathLen uint16
	Pad        uint16
	Reserved   uint32
}

type devInfo struct {
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

type ioCmd struct {
	QID    uint16
	Tag    uint16
	Result int32
	Addr   uint64
}

type ioDesc struct {
	OpFlags     uint32
	NrSectors   uint32
	StartSector uint64
	Addr        uint64
}

type ublkParams struct {
	Len     uint32
	Types   uint32
	Basic   paramBasic
	Discard paramDiscard
	Devt    paramDevt
	Zoned   paramZoned
}

type paramBasic struct {
	Attrs            uint32
	LogicalBSShift   uint8
	PhysicalBSShift  uint8
	IOOptShift       uint8
	IOMinShift       uint8
	MaxSectors       uint32
	ChunkSectors     uint32
	DevSectors       uint64
	VirtBoundaryMask uint64
}

type paramDiscard struct {
	DiscardAlignment      uint32
	DiscardGranularity    uint32
	MaxDiscardSectors     uint32
	MaxWriteZeroesSectors uint32
	MaxDiscardSegments    uint16
	Reserved0             uint16
}

type paramDevt struct {
	CharMajor uint32
	CharMinor uint32
	DiskMajor uint32
	DiskMinor uint32
}

type paramZoned struct {
	MaxOpenZones         uint32
	MaxActiveZones       uint32
	MaxZoneAppendSectors uint32
	Reserved             [20]uint8
}

var (
	sizeofDevInfo = unsafe.Sizeof(devInfo{})
	sizeofIODesc  = unsafe.Sizeof(ioDesc{})
)
