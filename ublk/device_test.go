package ublk

import (
	"testing"
)

// TestDeviceErrors tests error definitions
func TestDeviceErrors(t *testing.T) {
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

// TestUblkCommand tests command creation
func TestUblkCommand(t *testing.T) {
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

// TestUblkIOCommandBytes tests command serialization
func TestUblkIOCommandBytes(t *testing.T) {
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

// TestUblkIOCommandSize tests command size
func TestUblkIOCommandSize(t *testing.T) {
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

// TestErrInvalidRequest tests the ErrInvalidRequest error
func TestErrInvalidRequest(t *testing.T) {
	if ErrInvalidRequest == nil {
		t.Error("ErrInvalidRequest should not be nil")
	}
	if ErrInvalidRequest.Error() != "invalid request" {
		t.Errorf("Expected 'invalid request', got '%s'", ErrInvalidRequest.Error())
	}
}
