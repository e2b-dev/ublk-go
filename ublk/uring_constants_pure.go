//go:build !cgo

package ublk

// io_uring constants - stable kernel ABI values.
// These values are from the Linux kernel io_uring interface and do not change.

// io_uring opcodes.
const (
	IORING_OP_URING_CMD = 46 // Added in kernel 5.19
)

// IORING_SETUP flags for io_uring_setup().
const (
	IORING_SETUP_IOPOLL        = 1 << 0  // Use polling for IO completion
	IORING_SETUP_SQPOLL        = 1 << 1  // Use kernel thread for SQ polling
	IORING_SETUP_SQ_AFF        = 1 << 2  // SQ thread CPU affinity
	IORING_SETUP_CQSIZE        = 1 << 3  // Custom CQ size
	IORING_SETUP_CLAMP         = 1 << 4  // Clamp entries to limits
	IORING_SETUP_ATTACH_WQ     = 1 << 5  // Attach to existing workqueue
	IORING_SETUP_R_DISABLED    = 1 << 6  // Start with ring disabled
	IORING_SETUP_SUBMIT_ALL    = 1 << 7  // Submit all queued SQEs on enter
	IORING_SETUP_COOP_TASKRUN  = 1 << 8  // Cooperative task running (kernel 6.0+)
	IORING_SETUP_TASKRUN_FLAG  = 1 << 9  // Set IORING_SQ_TASKRUN on task work
	IORING_SETUP_SQE128        = 1 << 10 // 128-byte SQEs
	IORING_SETUP_CQE32         = 1 << 11 // 32-byte CQEs
	IORING_SETUP_SINGLE_ISSUER = 1 << 12 // Single thread submits (kernel 6.0+)
	IORING_SETUP_DEFER_TASKRUN = 1 << 13 // Defer task work (kernel 6.1+)
	IORING_SETUP_NO_MMAP       = 1 << 14 // User provides ring memory
	IORING_SETUP_REGISTERED_FD = 1 << 15 // Ring fd is registered
	IORING_SETUP_NO_SQARRAY    = 1 << 16 // No SQ array (kernel 6.6+)
)

// IORING_ENTER flags for io_uring_enter().
const (
	IORING_ENTER_GETEVENTS       = 1 << 0 // Wait for completions
	IORING_ENTER_SQ_WAKEUP       = 1 << 1 // Wake up SQ thread
	IORING_ENTER_SQ_WAIT         = 1 << 2 // Wait for SQ space
	IORING_ENTER_EXT_ARG         = 1 << 3 // Extended argument
	IORING_ENTER_REGISTERED_RING = 1 << 4 // fd is registered ring index
)

// SQE flags.
const (
	IOSQE_FIXED_FILE       = 1 << 0 // Use fixed file descriptor
	IOSQE_IO_DRAIN         = 1 << 1 // Drain IO before this one
	IOSQE_IO_LINK          = 1 << 2 // Link to next SQE
	IOSQE_IO_HARDLINK      = 1 << 3 // Hard link to next SQE
	IOSQE_ASYNC            = 1 << 4 // Always async
	IOSQE_BUFFER_SELECT    = 1 << 5 // Select buffer from pool
	IOSQE_CQE_SKIP_SUCCESS = 1 << 6 // Skip CQE on success
)

// CQE flags.
const (
	IORING_CQE_F_BUFFER        = 1 << 0 // Buffer ID is valid
	IORING_CQE_F_MORE          = 1 << 1 // More CQEs to come
	IORING_CQE_F_SOCK_NONEMPTY = 1 << 2 // Socket has more data
	IORING_CQE_F_NOTIF         = 1 << 3 // Notification CQE
)

// IORING_OFF constants for mmap offsets.
const (
	IORING_OFF_SQ_RING = 0
	IORING_OFF_CQ_RING = 0x8000000
	IORING_OFF_SQES    = 0x10000000
)
