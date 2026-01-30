package ublk

import (
	"testing"
)

// TestRingWrapAroundLogic verifies that GetSQE correctly reloads head from memory
// when the local counter indicates the ring is full.
func TestRingWrapAroundLogic(t *testing.T) {
	// 1. Manually construct a Ring with mocked memory
	entries := uint32(4)
	mask := uint32(3)

	// Allocate memory for SQ ring (array of uint32 pointers/values)
	// We need enough space for head, tail, mask, entries, flags, dropped, array
	// For simplicity, just use separate variables
	var head, tail, ringMask, ringEntries, flags, dropped uint32
	var array [4]uint32

	ringMask = mask
	ringEntries = entries

	r := &Ring{
		sq: &submissionQueue{
			head:        &head,
			tail:        &tail,
			ringMask:    &ringMask,
			ringEntries: &ringEntries,
			flags:       &flags,
			dropped:     &dropped,
			array:       &array[0],
			sqeHead:     0,
			sqeTail:     0,
		},
		sqes:    make([]UringSQE, entries),
		sqeSize: SizeOfUringSQE(),
	}

	// 2. Fill the ring
	for i := range int(entries) {
		_, err := r.GetSQE()
		if err != nil {
			t.Fatalf("Failed to get SQE #%d: %v", i, err)
		}
	}

	// 3. Next GetSQE should fail (ring is full, head=0, tail=4)
	_, err := r.GetSQE()
	if err == nil {
		t.Fatal("Expected error when ring is full, got nil")
	}
	if err.Error() != "submission queue full" {
		t.Errorf("Unexpected error msg: %v", err)
	}

	// 4. Simulate kernel consuming 2 entries (advance head)
	// We update the memory location that r.sq.head points to
	head = 2
	// Note: We do NOT manually update r.sq.sqeHead. GetSQE should do that.

	// 5. Next GetSQE should succeed now
	_, err2 := r.GetSQE()
	if err2 != nil {
		t.Errorf("GetSQE failed after kernel advanced head: %v", err2)
	}

	// Verify local cache was updated
	if r.sq.sqeHead != 2 {
		t.Errorf("r.sq.sqeHead not updated. Got %d, want 2", r.sq.sqeHead)
	}
}

// TestRingSQE128WrapAroundLogic verifies the same for SQE128.
func TestRingSQE128WrapAroundLogic(t *testing.T) {
	entries := uint32(4)
	mask := uint32(3)
	sqeSize := uintptr(128)

	var head, tail, ringMask, ringEntries uint32
	var array [4]uint32
	ringMask = mask
	ringEntries = entries

	// Fake mmapSQEs buffer
	mmapSQEs := make([]byte, int(entries)*int(sqeSize))

	r := &Ring{
		sq: &submissionQueue{
			head:        &head,
			tail:        &tail,
			ringMask:    &ringMask,
			ringEntries: &ringEntries,
			array:       &array[0],
			sqeHead:     0,
			sqeTail:     0,
		},
		mmapSQEs: mmapSQEs,
		sqeSize:  sqeSize,
	}

	// 1. Fill ring
	for i := range int(entries) {
		_, err := r.GetSQE128()
		if err != nil {
			t.Fatalf("Failed to get SQE128 #%d: %v", i, err)
		}
	}

	// 2. Should fail
	_, err := r.GetSQE128()
	if err == nil {
		t.Fatal("Expected error when ring is full")
	}

	// 3. Advance head
	head = 1

	// 4. Should succeed
	_, err = r.GetSQE128()
	if err != nil {
		t.Errorf("GetSQE128 failed after head advance: %v", err)
	}

	if r.sq.sqeHead != 1 {
		t.Errorf("r.sq.sqeHead not updated. Got %d, want 1", r.sq.sqeHead)
	}
}
