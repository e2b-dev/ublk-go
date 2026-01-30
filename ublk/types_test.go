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
	cmds := []uint32{
		UBLK_CMD_ADD_DEV,
		UBLK_CMD_DEL_DEV,
		UBLK_CMD_START_DEV,
		UBLK_CMD_STOP_DEV,
		UBLK_CMD_SET_PARAMS,
		UBLK_CMD_GET_PARAMS,
		UBLK_CMD_GET_QUEUE_AFFINITY,
		UBLK_CMD_GET_DEV_INFO,
		UBLK_CMD_GET_DEV_INFO2,
	}

	for _, cmd := range cmds {
		if cmd == 0 {
			t.Error("Control command should not be zero")
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
	flags := []uint32{
		UBLK_F_SUPPORT_ZERO_COPY,
		UBLK_F_NEED_GET_DATA,
		UBLK_F_UNPRIVILEGED_DEV,
		UBLK_F_PER_IO_DAEMON,
		UBLK_F_AUTO_BUF_REG,
	}

	for i, f := range flags {
		if f == 0 {
			t.Errorf("Device flag %d should not be zero", i)
		}
		if f&(f-1) != 0 {
			t.Errorf("Device flag %d (0x%x) is not a power of 2", i, f)
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
		Addr:    0x1234567890ABCDEF,
		Length:  4096,
		OpFlags: UBLK_IO_F_FETCHED,
		EndIO:   0,
		Tag:     42,
	}

	if desc.Addr != 0x1234567890ABCDEF {
		t.Error("Failed to set Addr")
	}
	if desc.Length != 4096 {
		t.Error("Failed to set Length")
	}
	if desc.OpFlags != UBLK_IO_F_FETCHED {
		t.Error("Failed to set OpFlags")
	}
	if desc.Tag != 42 {
		t.Error("Failed to set Tag")
	}
}
