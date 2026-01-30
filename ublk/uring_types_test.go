package ublk

import (
	"testing"
	"unsafe"
)

func TestUringSQESize(t *testing.T) {
	t.Parallel()
	size := SizeOfUringSQE()
	if size == 0 {
		t.Error("UringSQE size should not be zero")
	}
	t.Logf("UringSQE size: %d bytes", size)
}

func TestUringCQESize(t *testing.T) {
	t.Parallel()
	size := SizeOfUringCQE()
	if size == 0 {
		t.Error("UringCQE size should not be zero")
	}
	t.Logf("UringCQE size: %d bytes", size)
}

func TestUringParamsSize(t *testing.T) {
	t.Parallel()
	if size := SizeOfUringParams(); size != 120 {
		t.Errorf("expected SizeOfUringParams to be 120, got %d", size)
	}
}

func TestSizeOfUringSQE128(t *testing.T) {
	if size := SizeOfUringSQE128(); size != 128 {
		t.Errorf("expected SizeOfUringSQE128 to be 128, got %d", size)
	}
}

func TestUblksrvIODescSize(t *testing.T) {
	t.Parallel()
	size := unsafe.Sizeof(UblksrvIODesc{})
	if size == 0 {
		t.Error("UblksrvIODesc size should not be zero")
	}
	t.Logf("UblksrvIODesc size: %d bytes", size)
}

func TestUblkParamsSize(t *testing.T) {
	t.Parallel()
	size := unsafe.Sizeof(UblkParams{})
	if size == 0 {
		t.Error("UblkParams size should not be zero")
	}
	t.Logf("UblkParams size: %d bytes", size)
}

func TestUblkCtrlDevInfoSize(t *testing.T) {
	t.Parallel()
	size := unsafe.Sizeof(UblksrvCtrlDevInfo{})
	if size == 0 {
		t.Error("UblksrvCtrlDevInfo size should not be zero")
	}
	t.Logf("UblksrvCtrlDevInfo size: %d bytes", size)
}

func TestUblkQueueAffinitySize(t *testing.T) {
	t.Parallel()
	size := unsafe.Sizeof(UblkQueueAffinity{})
	if size == 0 {
		t.Error("UblkQueueAffinity size should not be zero")
	}
	t.Logf("UblkQueueAffinity size: %d bytes", size)
}

func TestUblksrvCtrlCmdSize(t *testing.T) {
	t.Parallel()
	size := unsafe.Sizeof(UblksrvCtrlCmd{})
	if size != 32 {
		t.Errorf("UblksrvCtrlCmd size should be 32, got %d", size)
	}
}
