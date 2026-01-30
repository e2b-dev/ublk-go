//go:build !cgo
// +build !cgo

package ublk

// io_uring constants - stable kernel ABI values
// These values are from the Linux kernel io_uring interface and do not change.

// io_uring opcodes
const (
	IORING_OP_URING_CMD = 46 // Added in kernel 5.19
)

// IORING_SETUP flags for io_uring_setup()
const (
	IORING_SETUP_IOPOLL     = 1 << 0 // Use polling for IO completion
	IORING_SETUP_SQPOLL     = 1 << 1 // Use kernel thread for SQ polling
	IORING_SETUP_SQ_AFF     = 1 << 2 // SQ thread CPU affinity
	IORING_SETUP_CQSIZE     = 1 << 3 // Custom CQ size
	IORING_SETUP_CLAMP      = 1 << 4 // Clamp entries to limits
	IORING_SETUP_ATTACH_WQ  = 1 << 5 // Attach to existing workqueue
	IORING_SETUP_R_DISABLED = 1 << 6 // Start with ring disabled
)

// IORING_ENTER flags for io_uring_enter()
const (
	IORING_ENTER_GETEVENTS = 1 << 0 // Wait for completions
	IORING_ENTER_SQ_WAKEUP = 1 << 1 // Wake up SQ thread
	IORING_ENTER_SQ_WAIT   = 1 << 2 // Wait for SQ space
)

// SQE flags
const (
	IOSQE_FIXED_FILE    = 1 << 0 // Use fixed file descriptor
	IOSQE_IO_DRAIN      = 1 << 1 // Drain IO before this one
	IOSQE_IO_LINK       = 1 << 2 // Link to next SQE
	IOSQE_IO_HARDLINK   = 1 << 3 // Hard link to next SQE
	IOSQE_ASYNC         = 1 << 4 // Always async
	IOSQE_BUFFER_SELECT = 1 << 5 // Select buffer from pool
)

// CQE flags
const (
	IORING_CQE_F_BUFFER        = 1 << 0 // Buffer ID is valid
	IORING_CQE_F_MORE          = 1 << 1 // More CQEs to come
	IORING_CQE_F_SOCK_NONEMPTY = 1 << 2 // Socket has more data
)

// IORING_OFF constants for mmap offsets
const (
	IORING_OFF_SQ_RING = 0
	IORING_OFF_CQ_RING = 0x8000000
	IORING_OFF_SQES    = 0x10000000
)
