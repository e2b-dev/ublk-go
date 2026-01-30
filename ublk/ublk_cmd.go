package ublk

import (
	"unsafe"
)

// UblkIOCommand represents a ublk IO command structure.
// Matches struct ublksrv_io_cmd in kernel.
type UblkIOCommand struct {
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
// Returns command and the Op code.
func NewFetchReqCommand(qid, tag uint16) (*UblkIOCommand, uint32) {
	return &UblkIOCommand{
		QID: qid,
		Tag: tag,
	}, UBLK_IO_FETCH_REQ
}

// NewCommitAndFetchReqCommand creates a UBLK_IO_COMMIT_AND_FETCH_REQ command.
// Returns command and the Op code.
func NewCommitAndFetchReqCommand(qid, tag uint16, result uint64) (*UblkIOCommand, uint32) {
	return &UblkIOCommand{
		QID:    qid,
		Tag:    tag,
		Result: result,
	}, UBLK_IO_COMMIT_AND_FETCH_REQ
}
