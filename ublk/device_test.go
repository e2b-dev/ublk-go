package ublk

import (
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestUblkIOCommandRoundTrip(t *testing.T) {
	t.Parallel()
	cmd, _ := NewFetchReqCommand(7, 99, 0x12345678)
	data := cmd.ToBytes()

	parsed := UblkIOCommandFromBytes(data)
	require.NotNil(t, parsed)
	assert.Equal(t, cmd.QID, parsed.QID)
	assert.Equal(t, cmd.Tag, parsed.Tag)
	assert.Equal(t, cmd.Addr, parsed.Addr)

	assert.Nil(t, UblkIOCommandFromBytes([]byte{1, 2, 3}), "short buffer")
}

func TestStartWorkersError(t *testing.T) {
	t.Parallel()
	d := &Device{
		info:      UblksrvCtrlDevInfo{NrHWQueues: 1, QueueDepth: 10000},
		charDevFD: os.NewFile(0, "mock"),
	}
	assert.Error(t, d.startWorkers(), "invalid queue depth should fail")
}

func TestNewDeviceWithBackendError(t *testing.T) {
	origPath := controlDevicePath
	controlDevicePath = "/non/existent/path"
	defer func() { controlDevicePath = origPath }()

	_, err := NewDeviceWithBackend(&MockBackend{})
	assert.Error(t, err)
}

func TestDeviceSyncNotStarted(t *testing.T) {
	t.Parallel()
	d := &Device{started: false}
	assert.ErrorIs(t, d.Sync(), ErrDeviceNotStarted)
}

func TestDeviceFlushBuffersNotStarted(t *testing.T) {
	t.Parallel()
	d := &Device{started: false}
	assert.ErrorIs(t, d.FlushBuffers(), ErrDeviceNotStarted)
}
