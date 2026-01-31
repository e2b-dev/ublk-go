package ublk

// ublk control and IO command definitions
// Based on Linux kernel ublk driver interface

const (
	UBLK_IO_OP_READ         = 0
	UBLK_IO_OP_WRITE        = 1
	UBLK_IO_OP_FLUSH        = 2
	UBLK_IO_OP_DISCARD      = 3
	UBLK_IO_OP_WRITE_ZEROES = 5
)

const (
	// IO flags in ublksrv_io_desc.op_flags (upper 24 bits).
	UBLK_IO_F_FUA = 1 << 13
)

// Device feature flags (passed to UBLK_U_CMD_ADD_DEV).
// Values from linux/ublk_cmd.h.
const (
	UBLK_F_SUPPORT_ZERO_COPY = 1 << 0  // Enable zero-copy support
	UBLK_F_CMD_IOCTL_ENCODE  = 1 << 6  // Encode ioctl in uring_cmd
	UBLK_F_USER_COPY         = 1 << 7  // User-space data copying
	UBLK_F_AUTO_BUF_REG      = 1 << 11 // Automatic buffer registration
)

// Constants for USER_COPY encoded offset (from ublk_cmd.h).
const (
	UBLK_MAX_QUEUE_DEPTH = 4096

	UBLK_IO_BUF_BITS = 25
	UBLK_TAG_BITS    = 16
	UBLK_QID_BITS    = 12

	UBLK_TAG_OFF = UBLK_IO_BUF_BITS
	UBLK_QID_OFF = UBLK_TAG_OFF + UBLK_TAG_BITS

	UBLKSRV_IO_BUF_OFFSET = 0x80000000
)

func ublkUserCopyPos(qid, tag uint16) int64 {
	return int64(UBLKSRV_IO_BUF_OFFSET) +
		((int64(qid) << UBLK_QID_OFF) |
			(int64(tag) << UBLK_TAG_OFF))
}

// Ublk params types (ublk_params.types bitset).
const (
	UBLK_PARAM_TYPE_BASIC   = 1 << 0
	UBLK_PARAM_TYPE_DISCARD = 1 << 1
)

// UblkParams represents device parameters (matches struct ublk_params).
type UblkParams struct {
	Len   uint32
	Types uint32

	Basic    UblkParamBasic
	Discard  UblkParamDiscard
	Devt     UblkParamDevt
	Zoned    UblkParamZoned
	DMAAlign UblkParamDMAAlign
	Segment  UblkParamSegment
}

type UblkParamBasic struct {
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

type UblkParamDiscard struct {
	DiscardAlignment      uint32
	DiscardGranularity    uint32
	MaxDiscardSectors     uint32
	MaxWriteZeroesSectors uint32
	MaxDiscardSegments    uint16
	Reserved0             uint16
}

type UblkParamDevt struct {
	CharMajor uint32
	CharMinor uint32
	DiskMajor uint32
	DiskMinor uint32
}

type UblkParamZoned struct {
	MaxOpenZones         uint32
	MaxActiveZones       uint32
	MaxZoneAppendSectors uint32
	Reserved             [20]uint8
}

type UblkParamDMAAlign struct {
	Alignment uint32
	Pad       [4]uint8
}

type UblkParamSegment struct {
	SegBoundaryMask uint64
	MaxSegmentSize  uint32
	MaxSegments     uint16
	Pad             [2]uint8
}

// UblkDevInfo represents device information (alias for UblksrvCtrlDevInfo).
type UblkDevInfo = UblksrvCtrlDevInfo

// UblksrvIODesc represents an IO descriptor (from kernel to server).
// Matches struct ublksrv_io_desc in linux/ublk_cmd.h.
type UblksrvIODesc struct {
	// op: bit 0-7, flags: bit 8-31
	OpFlags uint32

	// union: nr_sectors or nr_zones
	NrSectors uint32

	// start sector for this io
	StartSector uint64

	// buffer address in ublksrv vm space, from ublk driver
	Addr uint64
}

// UblkQueueAffinity represents queue affinity information.
type UblkQueueAffinity struct {
	QID      uint16
	Pad      [3]uint16
	SetSize  uint32
	Reserved [12]uint64
}
