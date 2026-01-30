package ublk

// ublk control and IO command definitions
// Based on Linux kernel ublk driver interface

const (
	UBLK_IO_OP_READ         = 0
	UBLK_IO_OP_WRITE        = 1
	UBLK_IO_OP_FLUSH        = 2
	UBLK_IO_OP_DISCARD      = 3
	UBLK_IO_OP_WRITE_ZEROES = 4
)

const (
	UBLK_IO_F_FETCHED       = 1 << 0
	UBLK_IO_F_NEED_GET_DATA = 1 << 1
)

const (
	UBLK_CMD_ADD_DEV            = 0x4701
	UBLK_CMD_DEL_DEV            = 0x4702
	UBLK_CMD_START_DEV          = 0x4703
	UBLK_CMD_STOP_DEV           = 0x4704
	UBLK_CMD_SET_PARAMS         = 0x4705
	UBLK_CMD_GET_PARAMS         = 0x4706
	UBLK_CMD_GET_QUEUE_AFFINITY = 0x4707
	UBLK_CMD_GET_DEV_INFO       = 0x4708
	UBLK_CMD_GET_DEV_INFO2      = 0x4709
)

const (
	UBLK_IO_FETCH_REQ            = 0x4750
	UBLK_IO_COMMIT_AND_FETCH_REQ = 0x4751
	UBLK_IO_NEED_GET_DATA        = 0x4752
)

const (
	UBLK_F_SUPPORT_ZERO_COPY = 1 << 0
	UBLK_F_NEED_GET_DATA     = 1 << 1
	UBLK_F_UNPRIVILEGED_DEV  = 1 << 2
	UBLK_F_PER_IO_DAEMON     = 1 << 3
	UBLK_F_AUTO_BUF_REG      = 1 << 4
)

// UblkParams represents device parameters
type UblkParams struct {
	Basic struct {
		LogicalBSize  uint32
		PhysicalBSize uint32
		IOOptBSize    uint32
		MaxSectors    uint32
		DevSectors    uint64
		ChunkSectors  uint32
		Reserved0     [3]uint32
		Reserved1     [8]uint64
	}
	IO struct {
		QueueDepth uint16
		NrHWQueues uint16
		Reserved   [60]uint8
	}
}

// UblkDevInfo represents device information
type UblkDevInfo struct {
	UblksrvCtrlDevInfo
}

// UblksrvCtrlDevInfo is the control device info structure
type UblksrvCtrlDevInfo struct {
	NrHWQueues    uint16
	QueueDepth    uint16
	State         uint16
	Pad           uint16
	MaxIOBufBytes uint32
	DevID         uint32
	Flags         uint32
	Reserved0     [32]uint8
	Reserved1     [32]uint64
}

// UblksrvIODesc represents an IO descriptor
type UblksrvIODesc struct {
	Addr     uint64
	Length   uint32
	OpFlags  uint16
	EndIO    uint16
	Tag      uint16
	Pad      uint16
	Reserved [4]uint64
}

// UblkQueueAffinity represents queue affinity information
type UblkQueueAffinity struct {
	QID      uint16
	Pad      [3]uint16
	SetSize  uint32
	Reserved [12]uint64
}
