package ublk

import (
	"testing"
)

func TestDeviceErrors(t *testing.T) {
	t.Parallel()
	if ErrDeviceNotStarted == nil {
		t.Error("ErrDeviceNotStarted should be defined")
	}
	if ErrDeviceAlreadyStarted == nil {
		t.Error("ErrDeviceAlreadyStarted should be defined")
	}
	if ErrInvalidParameters == nil {
		t.Error("ErrInvalidParameters should be defined")
	}
	if ErrCharDevNotOpen == nil {
		t.Error("ErrCharDevNotOpen should be defined")
	}
}

func TestUblkCommand(t *testing.T) {
	t.Parallel()
	cmd := NewFetchReqCommand(0, 0, 0)
	if cmd.Op != UBLK_IO_FETCH_REQ {
		t.Errorf("Expected op %d, got %d", UBLK_IO_FETCH_REQ, cmd.Op)
	}

	cmd2 := NewCommitAndFetchReqCommand(1, 2, 3, 0)
	if cmd2.Op != UBLK_IO_COMMIT_AND_FETCH_REQ {
		t.Errorf("Expected op %d, got %d", UBLK_IO_COMMIT_AND_FETCH_REQ, cmd2.Op)
	}
	if cmd2.DevID != 1 || cmd2.QID != 2 || cmd2.Tag != 3 {
		t.Error("Command fields not set correctly")
	}
}

func TestUblkIOCommandBytes(t *testing.T) {
	t.Parallel()
	cmd := NewFetchReqCommand(42, 7, 99)

	// ToBytes should return a valid slice
	data := cmd.ToBytes()
	if len(data) == 0 {
		t.Fatal("ToBytes returned empty slice")
	}

	// FromBytes should round-trip
	parsed := UblkIOCommandFromBytes(data)
	if parsed == nil {
		t.Fatal("UblkIOCommandFromBytes returned nil")
	}
	if parsed.Op != cmd.Op || parsed.DevID != cmd.DevID || parsed.QID != cmd.QID || parsed.Tag != cmd.Tag {
		t.Error("Round-trip failed: fields don't match")
	}

	// FromBytes with short buffer should return nil
	if UblkIOCommandFromBytes([]byte{1, 2, 3}) != nil {
		t.Error("Expected nil for short buffer")
	}
}

func TestUblkIOCommandSize(t *testing.T) {
	t.Parallel()
	cmd := NewFetchReqCommand(0, 0, 0)
	size := cmd.Size()
	if size == 0 {
		t.Error("Size should not be zero")
	}
	// Size should match ToBytes length
	if int(size) != len(cmd.ToBytes()) {
		t.Errorf("Size() = %d, but ToBytes() len = %d", size, len(cmd.ToBytes()))
	}
}

func TestErrInvalidRequest(t *testing.T) {
	t.Parallel()
	if ErrInvalidRequest == nil {
		t.Error("ErrInvalidRequest should not be nil")
	}
	if ErrInvalidRequest.Error() != "invalid request" {
		t.Errorf("Expected 'invalid request', got '%s'", ErrInvalidRequest.Error())
	}
}

func TestDeviceOptions(t *testing.T) {
	t.Parallel()
	t.Run("WithZeroCopy", func(t *testing.T) {
		t.Parallel()
		d := &Device{}
		WithZeroCopy()(d)
		if d.flags&UBLK_F_SUPPORT_ZERO_COPY == 0 {
			t.Error("WithZeroCopy should set UBLK_F_SUPPORT_ZERO_COPY")
		}
	})

	t.Run("WithAutoBufReg", func(t *testing.T) {
		t.Parallel()
		d := &Device{}
		WithAutoBufReg()(d)
		if d.flags&UBLK_F_AUTO_BUF_REG == 0 {
			t.Error("WithAutoBufReg should set UBLK_F_AUTO_BUF_REG")
		}
		if d.flags&UBLK_F_SUPPORT_ZERO_COPY == 0 {
			t.Error("WithAutoBufReg should also set UBLK_F_SUPPORT_ZERO_COPY")
		}
	})

	t.Run("WithUserRecovery", func(t *testing.T) {
		t.Parallel()
		d := &Device{}
		WithUserRecovery()(d)
		if d.flags&UBLK_F_USER_RECOVERY == 0 {
			t.Error("WithUserRecovery should set UBLK_F_USER_RECOVERY")
		}
	})

	t.Run("WithUnprivileged", func(t *testing.T) {
		t.Parallel()
		d := &Device{}
		WithUnprivileged()(d)
		if d.flags&UBLK_F_UNPRIVILEGED_DEV == 0 {
			t.Error("WithUnprivileged should set UBLK_F_UNPRIVILEGED_DEV")
		}
	})

	t.Run("WithUserCopy", func(t *testing.T) {
		t.Parallel()
		d := &Device{}
		WithUserCopy()(d)
		if d.flags&UBLK_F_USER_COPY == 0 {
			t.Error("WithUserCopy should set UBLK_F_USER_COPY")
		}
	})
}

func TestDeviceFeatureFlags(t *testing.T) {
	t.Parallel()
	// Test that flags match kernel values from linux/ublk_cmd.h
	tests := []struct {
		name  string
		value uint64
		want  uint64
	}{
		{"UBLK_F_SUPPORT_ZERO_COPY", UBLK_F_SUPPORT_ZERO_COPY, 1 << 0},
		{"UBLK_F_URING_CMD_COMP_IN_TASK", UBLK_F_URING_CMD_COMP_IN_TASK, 1 << 1},
		{"UBLK_F_NEED_GET_DATA", UBLK_F_NEED_GET_DATA, 1 << 2},
		{"UBLK_F_USER_RECOVERY", UBLK_F_USER_RECOVERY, 1 << 3},
		{"UBLK_F_USER_RECOVERY_REISSUE", UBLK_F_USER_RECOVERY_REISSUE, 1 << 4},
		{"UBLK_F_UNPRIVILEGED_DEV", UBLK_F_UNPRIVILEGED_DEV, 1 << 5},
		{"UBLK_F_CMD_IOCTL_ENCODE", UBLK_F_CMD_IOCTL_ENCODE, 1 << 6},
		{"UBLK_F_USER_COPY", UBLK_F_USER_COPY, 1 << 7},
		{"UBLK_F_ZONED", UBLK_F_ZONED, 1 << 8},
		{"UBLK_F_USER_RECOVERY_FAIL_IO", UBLK_F_USER_RECOVERY_FAIL_IO, 1 << 9},
		{"UBLK_F_AUTO_BUF_REG", UBLK_F_AUTO_BUF_REG, 1 << 11},
		{"UBLK_F_PER_IO_DAEMON", UBLK_F_PER_IO_DAEMON, 1 << 13},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if tt.value != tt.want {
				t.Errorf("%s = %d, want %d", tt.name, tt.value, tt.want)
			}
		})
	}
}

func TestDeviceHelpers(t *testing.T) {
	t.Parallel()
	d := &Device{flags: UBLK_F_SUPPORT_ZERO_COPY | UBLK_F_AUTO_BUF_REG | UBLK_F_USER_COPY}

	if !d.HasZeroCopy() {
		t.Error("HasZeroCopy() should return true")
	}

	if !d.HasAutoBufReg() {
		t.Error("HasAutoBufReg() should return true")
	}

	if !d.HasUserCopy() {
		t.Error("HasUserCopy() should return true")
	}

	d2 := &Device{}
	if d2.HasZeroCopy() {
		t.Error("HasZeroCopy() should return false for default device")
	}
	if d2.HasUserCopy() {
		t.Error("HasUserCopy() should return false for default device")
	}
}
