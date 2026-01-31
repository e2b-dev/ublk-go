package ublk

import "unsafe"

type UblkIOCommand struct {
	QID    uint16
	Tag    uint16
	Result int32
	Addr   uint64
}

const SizeOfUblkIOCommand = unsafe.Sizeof(UblkIOCommand{})

func (c *UblkIOCommand) ToBytes() []byte {
	return (*[SizeOfUblkIOCommand]byte)(unsafe.Pointer(c))[:]
}

func UblkIOCommandFromBytes(data []byte) *UblkIOCommand {
	if len(data) < int(SizeOfUblkIOCommand) {
		return nil
	}
	return (*UblkIOCommand)(unsafe.Pointer(&data[0]))
}

func NewFetchReqCommand(qid, tag uint16, addr uint64) (*UblkIOCommand, uint32) {
	return &UblkIOCommand{QID: qid, Tag: tag, Addr: addr}, uint32(UBLK_U_IO_FETCH_REQ)
}

func NewCommitAndFetchReqCommand(qid, tag uint16, result int32, addr uint64) (*UblkIOCommand, uint32) {
	return &UblkIOCommand{QID: qid, Tag: tag, Result: result, Addr: addr}, uint32(UBLK_U_IO_COMMIT_AND_FETCH_REQ)
}

func NewRegisterIOBufCommand(qid, tag uint16, addr uint64) (*UblkIOCommand, uint32) {
	return &UblkIOCommand{QID: qid, Tag: tag, Addr: addr}, uint32(UBLK_U_IO_REGISTER_IO_BUF)
}

func NewUnregisterIOBufCommand(qid, tag uint16, addr uint64) (*UblkIOCommand, uint32) {
	return &UblkIOCommand{QID: qid, Tag: tag, Addr: addr}, uint32(UBLK_U_IO_UNREGISTER_IO_BUF)
}
