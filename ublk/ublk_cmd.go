package ublk

import (
	"unsafe"
)

// UblkIOCommand represents a ublk IO command structure.
// This is embedded in the uring_cmd structure.
type UblkIOCommand struct {
	Op     uint32
	DevID  uint32
	QID    uint16
	Tag    uint16
	Result uint64
	Addr   uint64
}

// Size returns the size of UblkIOCommand.
func (c *UblkIOCommand) Size() uintptr {
	return unsafe.Sizeof(*c)
}

// ToBytes converts the command to bytes for embedding in uring_cmd.
func (c *UblkIOCommand) ToBytes() []byte {
	return (*[unsafe.Sizeof(*c)]byte)(unsafe.Pointer(c))[:]
}

// UblkIOCommandFromBytes creates a UblkIOCommand from bytes.
func UblkIOCommandFromBytes(data []byte) *UblkIOCommand {
	if len(data) < int(unsafe.Sizeof(UblkIOCommand{})) {
		return nil
	}
	return (*UblkIOCommand)(unsafe.Pointer(&data[0]))
}

// NewFetchReqCommand creates a UBLK_IO_FETCH_REQ command.
func NewFetchReqCommand(devID int, qid, tag uint16) *UblkIOCommand {
	return &UblkIOCommand{
		Op:    UBLK_IO_FETCH_REQ,
		DevID: uint32(devID),
		QID:   qid,
		Tag:   tag,
	}
}

// NewCommitAndFetchReqCommand creates a UBLK_IO_COMMIT_AND_FETCH_REQ command.
func NewCommitAndFetchReqCommand(devID int, qid, tag uint16, result uint64) *UblkIOCommand {
	return &UblkIOCommand{
		Op:     UBLK_IO_COMMIT_AND_FETCH_REQ,
		DevID:  uint32(devID),
		QID:    qid,
		Tag:    tag,
		Result: result,
	}
}
