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
	size := SizeOfUringParams()
	if size == 0 {
		t.Error("UringParams size should not be zero")
	}
	t.Logf("UringParams size: %d bytes", size)
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

func TestUblkRequestSize(t *testing.T) {
	t.Parallel()
	size := unsafe.Sizeof(UblkRequest{})
	if size == 0 {
		t.Error("UblkRequest size should not be zero")
	}
	t.Logf("UblkRequest size: %d bytes", size)
}

func TestUblkSegmentSize(t *testing.T) {
	t.Parallel()
	size := unsafe.Sizeof(UblkSegment{})
	if size == 0 {
		t.Error("UblkSegment size should not be zero")
	}
	t.Logf("UblkSegment size: %d bytes", size)
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
