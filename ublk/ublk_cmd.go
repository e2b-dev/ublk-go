package ublk

import (
	"unsafe"
)

// UblkIOCommand represents a ublk IO command structure.
// Matches struct ublksrv_io_cmd in kernel.
type UblkIOCommand struct {
	QID    uint16
	Tag    uint16
	Result int32
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

// NewFetchReqCommand creates a UBLK_U_IO_FETCH_REQ command.
// Returns command and the Op code.
func NewFetchReqCommand(qid, tag uint16, addr uint64) (*UblkIOCommand, uint32) {
	return &UblkIOCommand{
		QID:  qid,
		Tag:  tag,
		Addr: addr,
	}, uint32(UBLK_U_IO_FETCH_REQ)
}

// NewCommitAndFetchReqCommand creates a UBLK_U_IO_COMMIT_AND_FETCH_REQ command.
// Returns command and the Op code.
func NewCommitAndFetchReqCommand(qid, tag uint16, result int32, addr uint64) (*UblkIOCommand, uint32) {
	return &UblkIOCommand{
		QID:    qid,
		Tag:    tag,
		Result: result,
		Addr:   addr,
	}, uint32(UBLK_U_IO_COMMIT_AND_FETCH_REQ)
}

// NewRegisterIOBufCommand creates a UBLK_U_IO_REGISTER_IO_BUF command.
// addr is the io_uring fixed buffer index to register.
func NewRegisterIOBufCommand(qid, tag uint16, addr uint64) (*UblkIOCommand, uint32) {
	return &UblkIOCommand{
		QID:  qid,
		Tag:  tag,
		Addr: addr,
	}, uint32(UBLK_U_IO_REGISTER_IO_BUF)
}

// NewUnregisterIOBufCommand creates a UBLK_U_IO_UNREGISTER_IO_BUF command.
// addr is the io_uring fixed buffer index to unregister.
func NewUnregisterIOBufCommand(qid, tag uint16, addr uint64) (*UblkIOCommand, uint32) {
	return &UblkIOCommand{
		QID:  qid,
		Tag:  tag,
		Addr: addr,
	}, uint32(UBLK_U_IO_UNREGISTER_IO_BUF)
}
