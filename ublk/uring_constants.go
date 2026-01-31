//go:build cgo

package ublk

/*
#cgo pkg-config: liburing

#include <liburing.h>
*/
import "C"

// Constants from liburing/kernel headers.
const (
	IORING_OP_URING_CMD   = C.IORING_OP_URING_CMD
	IORING_OP_READ_FIXED  = C.IORING_OP_READ_FIXED
	IORING_OP_WRITE_FIXED = C.IORING_OP_WRITE_FIXED
)

// IORING_SETUP flags for io_uring_setup().
// Some newer flags are hardcoded as they may not be in older liburing headers.
const (
	IORING_SETUP_IOPOLL        = C.IORING_SETUP_IOPOLL
	IORING_SETUP_SQPOLL        = C.IORING_SETUP_SQPOLL
	IORING_SETUP_SQ_AFF        = C.IORING_SETUP_SQ_AFF
	IORING_SETUP_CQSIZE        = C.IORING_SETUP_CQSIZE
	IORING_SETUP_CLAMP         = C.IORING_SETUP_CLAMP
	IORING_SETUP_ATTACH_WQ     = C.IORING_SETUP_ATTACH_WQ
	IORING_SETUP_R_DISABLED    = C.IORING_SETUP_R_DISABLED
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
	IORING_ENTER_GETEVENTS       = C.IORING_ENTER_GETEVENTS
	IORING_ENTER_SQ_WAKEUP       = C.IORING_ENTER_SQ_WAKEUP
	IORING_ENTER_SQ_WAIT         = C.IORING_ENTER_SQ_WAIT
	IORING_ENTER_EXT_ARG         = 1 << 3 // Extended argument
	IORING_ENTER_REGISTERED_RING = 1 << 4 // fd is registered ring index
)

// SQE flags for submission queue entries.
const (
	IOSQE_FIXED_FILE       = C.IOSQE_FIXED_FILE
	IOSQE_IO_DRAIN         = C.IOSQE_IO_DRAIN
	IOSQE_IO_LINK          = C.IOSQE_IO_LINK
	IOSQE_IO_HARDLINK      = C.IOSQE_IO_HARDLINK
	IOSQE_ASYNC            = C.IOSQE_ASYNC
	IOSQE_BUFFER_SELECT    = C.IOSQE_BUFFER_SELECT
	IOSQE_CQE_SKIP_SUCCESS = 1 << 6 // Skip CQE on success
)

// CQE flags for completion queue entries.
const (
	IORING_CQE_F_BUFFER        = C.IORING_CQE_F_BUFFER
	IORING_CQE_F_MORE          = C.IORING_CQE_F_MORE
	IORING_CQE_F_SOCK_NONEMPTY = C.IORING_CQE_F_SOCK_NONEMPTY
	IORING_CQE_F_NOTIF         = 1 << 3 // Notification CQE
)

// IORING_OFF constants for mmap offsets.
const (
	IORING_OFF_SQ_RING = 0
	IORING_OFF_CQ_RING = 0x8000000
	IORING_OFF_SQES    = 0x10000000
)
