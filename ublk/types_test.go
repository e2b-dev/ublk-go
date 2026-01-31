package ublk

import (
	"testing"
)

func TestIOOperationConstants(t *testing.T) {
	t.Parallel()
	ops := []uint8{
		UBLK_IO_OP_READ,
		UBLK_IO_OP_WRITE,
		UBLK_IO_OP_FLUSH,
		UBLK_IO_OP_DISCARD,
		UBLK_IO_OP_WRITE_ZEROES,
	}

	seen := make(map[uint8]bool)
	for _, op := range ops {
		if seen[op] {
			t.Errorf("Duplicate IO operation constant: %d", op)
		}
		seen[op] = true
	}
}

func TestIOFlagConstants(t *testing.T) {
	t.Parallel()
	if UBLK_IO_F_FUA == 0 {
		t.Error("UBLK_IO_F_FUA should not be zero")
	}
}

func TestCommandConstants(t *testing.T) {
	t.Parallel()
	// Test ioctl-encoded commands
	encodedCmds := []uintptr{
		UBLK_U_CMD_ADD_DEV,
		UBLK_U_CMD_DEL_DEV,
		UBLK_U_CMD_START_DEV,
		UBLK_U_CMD_STOP_DEV,
		UBLK_U_CMD_SET_PARAMS,
		UBLK_U_CMD_GET_PARAMS,
		UBLK_U_CMD_GET_DEV_INFO,
	}

	for _, cmd := range encodedCmds {
		if cmd == 0 {
			t.Error("Encoded control command should not be zero")
		}
		// Ioctl-encoded commands have the type 'u' (0x75) at bits 8-15
		if (cmd>>8)&0xFF != 'u' {
			t.Errorf("Encoded command 0x%x should have type 'u'", cmd)
		}
	}
}

func TestDeviceFlagConstants(t *testing.T) {
	t.Parallel()
	// Test that flags are powers of 2 and match expected values
	flagTests := []struct {
		name     string
		flag     uint64
		expected uint64
	}{
		{"UBLK_F_SUPPORT_ZERO_COPY", UBLK_F_SUPPORT_ZERO_COPY, 1 << 0},
		{"UBLK_F_CMD_IOCTL_ENCODE", UBLK_F_CMD_IOCTL_ENCODE, 1 << 6},
		{"UBLK_F_USER_COPY", UBLK_F_USER_COPY, 1 << 7},
		{"UBLK_F_AUTO_BUF_REG", UBLK_F_AUTO_BUF_REG, 1 << 11},
	}

	for _, tt := range flagTests {
		if tt.flag != tt.expected {
			t.Errorf("%s = 0x%x, expected 0x%x", tt.name, tt.flag, tt.expected)
		}
		if tt.flag&(tt.flag-1) != 0 {
			t.Errorf("%s (0x%x) is not a power of 2", tt.name, tt.flag)
		}
	}
}

func TestUblkParamsStructure(t *testing.T) {
	t.Parallel()
	params := UblkParams{}

	params.Len = 128
	params.Types = UBLK_PARAM_TYPE_BASIC
	params.Basic.LogicalBSShift = 9
	params.Basic.PhysicalBSShift = 12
	params.Basic.MaxSectors = 256
	params.Basic.DevSectors = 1024 * 1024

	if params.Basic.LogicalBSShift != 9 {
		t.Error("Failed to set LogicalBSShift")
	}

	params.Discard.MaxDiscardSectors = 1024
	params.Discard.MaxDiscardSegments = 2

	if params.Discard.MaxDiscardSegments != 2 {
		t.Error("Failed to set MaxDiscardSegments")
	}
}

func TestUblksrvIODescStructure(t *testing.T) {
	t.Parallel()
	desc := UblksrvIODesc{
		Addr:        0x1234567890ABCDEF,
		NrSectors:   128,
		StartSector: 100,
		OpFlags:     UBLK_IO_F_FUA,
	}

	if desc.Addr != 0x1234567890ABCDEF {
		t.Error("Failed to set Addr")
	}
	if desc.NrSectors != 128 {
		t.Error("Failed to set NrSectors")
	}
	if desc.OpFlags != UBLK_IO_F_FUA {
		t.Error("Failed to set OpFlags")
	}
	if desc.StartSector != 100 {
		t.Error("Failed to set StartSector")
	}
}
