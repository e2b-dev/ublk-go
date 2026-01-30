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

// Legacy command constants - use UBLK_U_CMD_* from ioctl.go instead.

// IO commands for io_uring passthrough.
const (
	UBLK_IO_FETCH_REQ            = 0x4750
	UBLK_IO_COMMIT_AND_FETCH_REQ = 0x4751
	UBLK_IO_NEED_GET_DATA        = 0x4752
	UBLK_IO_REGISTER_IO_BUF      = 0x4753 // Zero-copy: register buffer
	UBLK_IO_UNREGISTER_IO_BUF    = 0x4754 // Zero-copy: unregister buffer
)

// Device feature flags (passed to UBLK_CMD_ADD_DEV).
// Values from linux/ublk_cmd.h.
const (
	UBLK_F_SUPPORT_ZERO_COPY      = 1 << 0  // Enable zero-copy support
	UBLK_F_URING_CMD_COMP_IN_TASK = 1 << 1  // Complete uring_cmd in task context
	UBLK_F_NEED_GET_DATA          = 1 << 2  // Deferred write data fetching
	UBLK_F_USER_RECOVERY          = 1 << 3  // User-space recovery support
	UBLK_F_USER_RECOVERY_REISSUE  = 1 << 4  // Reissue in-flight IOs on recovery
	UBLK_F_UNPRIVILEGED_DEV       = 1 << 5  // Allow unprivileged device control
	UBLK_F_CMD_IOCTL_ENCODE       = 1 << 6  // Encode ioctl in uring_cmd
	UBLK_F_USER_COPY              = 1 << 7  // User-space data copying
	UBLK_F_ZONED                  = 1 << 8  // Zoned block device support
	UBLK_F_USER_RECOVERY_FAIL_IO  = 1 << 9  // Fail IOs during recovery
	UBLK_F_UPDATE_SIZE            = 1 << 10 // Update device size
	UBLK_F_AUTO_BUF_REG           = 1 << 11 // Automatic buffer registration
	UBLK_F_QUIESCE                = 1 << 12 // Quiesce for recovery
	UBLK_F_PER_IO_DAEMON          = 1 << 13 // Per-IO daemon support
)

// IO descriptor flags (in UblksrvIODesc.OpFlags).
const (
	UBLK_IO_F_NEED_REG_BUF = 1 << 8 // Auto buffer registration failed, manual needed
)

// UblkParams represents device parameters.
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

// UblkDevInfo represents device information (alias for UblksrvCtrlDevInfo).
type UblkDevInfo = UblksrvCtrlDevInfo

// UblksrvIODesc represents an IO descriptor.
type UblksrvIODesc struct {
	Addr     uint64
	Length   uint32
	OpFlags  uint16
	EndIO    uint16
	Tag      uint16
	Pad      uint16
	Reserved [4]uint64
}

// UblkQueueAffinity represents queue affinity information.
type UblkQueueAffinity struct {
	QID      uint16
	Pad      [3]uint16
	SetSize  uint32
	Reserved [12]uint64
}
