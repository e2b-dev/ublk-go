package ublk

import (
	"testing"
	"unsafe"

	"github.com/stretchr/testify/assert"
)

// TestKernelStructSizes verifies struct layouts match kernel ABI.
// These sizes are critical for correct kernel communication.
func TestKernelStructSizes(t *testing.T) {
	t.Parallel()
	assert.Equal(t, SizeOfUringSQE128, uintptr(128), "SQE128 must be 128 bytes for IORING_SETUP_SQE128")
	assert.Equal(t, SizeOfUringParams, uintptr(120), "UringParams must match kernel io_uring_params")
	assert.Equal(t, unsafe.Sizeof(UblksrvCtrlCmd{}), uintptr(32), "CtrlCmd must be 32 bytes for ioctl encoding")
}
