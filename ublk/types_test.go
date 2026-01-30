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
	if UBLK_IO_F_FETCHED == 0 {
		t.Error("UBLK_IO_F_FETCHED should not be zero")
	}
	if UBLK_IO_F_NEED_GET_DATA == 0 {
		t.Error("UBLK_IO_F_NEED_GET_DATA should not be zero")
	}
	if UBLK_IO_F_FETCHED == UBLK_IO_F_NEED_GET_DATA {
		t.Error("IO flags should be distinct")
	}
}

func TestCommandConstants(t *testing.T) {
	t.Parallel()
	// Test raw command numbers
	cmds := []uint32{
		UBLK_CMD_ADD_DEV,
		UBLK_CMD_DEL_DEV,
		UBLK_CMD_START_DEV,
		UBLK_CMD_STOP_DEV,
		UBLK_CMD_SET_PARAMS,
		UBLK_CMD_GET_PARAMS,
	}

	for _, cmd := range cmds {
		if cmd == 0 {
			t.Error("Control command should not be zero")
		}
	}

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

func TestIOCommandConstants(t *testing.T) {
	t.Parallel()
	ioCmds := []uint32{
		UBLK_IO_FETCH_REQ,
		UBLK_IO_COMMIT_AND_FETCH_REQ,
		UBLK_IO_NEED_GET_DATA,
	}

	seen := make(map[uint32]bool)
	for _, cmd := range ioCmds {
		if cmd == 0 {
			t.Error("IO command should not be zero")
		}
		if seen[cmd] {
			t.Errorf("Duplicate IO command: %d", cmd)
		}
		seen[cmd] = true
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
		{"UBLK_F_URING_CMD_COMP_IN_TASK", UBLK_F_URING_CMD_COMP_IN_TASK, 1 << 1},
		{"UBLK_F_NEED_GET_DATA", UBLK_F_NEED_GET_DATA, 1 << 2},
		{"UBLK_F_USER_RECOVERY", UBLK_F_USER_RECOVERY, 1 << 3},
		{"UBLK_F_UNPRIVILEGED_DEV", UBLK_F_UNPRIVILEGED_DEV, 1 << 5},
		{"UBLK_F_CMD_IOCTL_ENCODE", UBLK_F_CMD_IOCTL_ENCODE, 1 << 6},
		{"UBLK_F_USER_COPY", UBLK_F_USER_COPY, 1 << 7},
		{"UBLK_F_AUTO_BUF_REG", UBLK_F_AUTO_BUF_REG, 1 << 11},
		{"UBLK_F_PER_IO_DAEMON", UBLK_F_PER_IO_DAEMON, 1 << 13},
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

	params.Basic.LogicalBSize = 512
	params.Basic.PhysicalBSize = 4096
	params.Basic.MaxSectors = 256
	params.Basic.DevSectors = 1024 * 1024

	if params.Basic.LogicalBSize != 512 {
		t.Error("Failed to set LogicalBSize")
	}

	params.IO.QueueDepth = 128
	params.IO.NrHWQueues = 2

	if params.IO.QueueDepth != 128 {
		t.Error("Failed to set QueueDepth")
	}
}

func TestUblksrvIODescStructure(t *testing.T) {
	t.Parallel()
	desc := UblksrvIODesc{
		Addr:        0x1234567890ABCDEF,
		NrSectors:   128,
		StartSector: 100,
		OpFlags:     UBLK_IO_F_FETCHED,
	}

	if desc.Addr != 0x1234567890ABCDEF {
		t.Error("Failed to set Addr")
	}
	if desc.NrSectors != 128 {
		t.Error("Failed to set NrSectors")
	}
	if desc.OpFlags != UBLK_IO_F_FETCHED {
		t.Error("Failed to set OpFlags")
	}
	if desc.StartSector != 100 {
		t.Error("Failed to set StartSector")
	}
}
