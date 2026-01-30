package ublk

import (
	"os"
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
	cmd, op := NewFetchReqCommand(0, 0)
	if op != UBLK_IO_FETCH_REQ {
		t.Errorf("Expected op %d, got %d", UBLK_IO_FETCH_REQ, op)
	}
	if cmd.QID != 0 || cmd.Tag != 0 {
		t.Error("Command fields not set correctly")
	}

	cmd2, op2 := NewCommitAndFetchReqCommand(2, 3, 0)
	if op2 != UBLK_IO_COMMIT_AND_FETCH_REQ {
		t.Errorf("Expected op %d, got %d", UBLK_IO_COMMIT_AND_FETCH_REQ, op2)
	}
	if cmd2.QID != 2 || cmd2.Tag != 3 {
		t.Error("Command fields not set correctly")
	}
}

func TestUblkIOCommandBytes(t *testing.T) {
	t.Parallel()
	cmd, _ := NewFetchReqCommand(7, 99)

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
	if parsed.QID != cmd.QID || parsed.Tag != cmd.Tag {
		t.Error("Round-trip failed: fields don't match")
	}

	// FromBytes with short buffer should return nil
	if UblkIOCommandFromBytes([]byte{1, 2, 3}) != nil {
		t.Error("Expected nil for short buffer")
	}
}

func TestUblkIOCommandSize(t *testing.T) {
	t.Parallel()
	cmd, _ := NewFetchReqCommand(0, 0)
	size := cmd.Size()

	// ublksrv_io_cmd is 24 bytes (2+2+4(pad)+8+8)
	const expectedSize = 24

	if size != expectedSize {
		t.Errorf("Size should be %d, got %d", expectedSize, size)
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

func TestStartWorkersError(t *testing.T) {
	t.Parallel()
	d := &Device{
		info: UblksrvCtrlDevInfo{
			NrHWQueues: 1,
			QueueDepth: 10000, // Too large, NewRing will fail
		},
		charDevFD: os.NewFile(0, "mock"), // Need a dummy FD so Init doesn't crash on Fd() call
	}

	err := d.startWorkers()
	if err == nil {
		t.Error("startWorkers() should fail with invalid queue depth")
	}
}

func TestNewDevice(t *testing.T) {
	// Setup mock control device
	tmp, err := os.CreateTemp(t.TempDir(), "ublk-control")
	if err != nil {
		t.Fatal(err)
	}
	defer tmp.Close()

	// Override path
	origPath := controlDevicePath
	controlDevicePath = tmp.Name()
	defer func() { controlDevicePath = origPath }()

	d, err := NewDevice(
		func(p []byte, off int64) (int, error) { return 0, nil },
		func(p []byte, off int64) (int, error) { return 0, nil },
	)
	if err != nil {
		t.Fatalf("NewDevice failed: %v", err)
	}
	if d.controlFD == nil {
		t.Error("controlFD not set")
	}
	d.controlFD.Close() // cleanup
}

func TestNewDeviceWithBackend(t *testing.T) {
	// Setup mock control device
	tmp, err := os.CreateTemp(t.TempDir(), "ublk-control")
	if err != nil {
		t.Fatal(err)
	}
	defer tmp.Close()

	// Override path
	origPath := controlDevicePath
	controlDevicePath = tmp.Name()
	defer func() { controlDevicePath = origPath }()

	backend := &MockBackend{}
	d, err := NewDeviceWithBackend(backend, WithZeroCopy())
	if err != nil {
		t.Fatalf("NewDeviceWithBackend failed: %v", err)
	}
	if d.controlFD == nil {
		t.Error("controlFD not set")
	}
	if !d.HasZeroCopy() {
		t.Error("WithZeroCopy option not applied")
	}
	d.controlFD.Close() // cleanup
}

func TestNewDeviceError(t *testing.T) {
	// Override path to non-existent file
	origPath := controlDevicePath
	controlDevicePath = "/non/existent/path"
	defer func() { controlDevicePath = origPath }()

	_, err := NewDevice(nil, nil)
	if err == nil {
		t.Error("Expected error for non-existent control device")
	}

	_, err = NewDeviceWithBackend(&MockBackend{})
	if err == nil {
		t.Error("Expected error for non-existent control device")
	}
}
