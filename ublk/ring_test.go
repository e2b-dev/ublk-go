package ublk

import (
	"testing"
)

// TestRingRoundUpPow2 tests the power of 2 rounding function.
func TestRingRoundUpPow2(t *testing.T) {
	tests := []struct {
		input  uint
		output uint
	}{
		{0, 1},
		{1, 1},
		{2, 2},
		{3, 4},
		{5, 8},
		{15, 16},
		{17, 32},
		{100, 128},
		{255, 256},
		{256, 256},
		{257, 512},
		{1023, 1024},
		{1024, 1024},
		{4096, 4096},
	}

	for _, tt := range tests {
		result := roundUpPow2(tt.input)
		if result != tt.output {
			t.Errorf("roundUpPow2(%d) = %d, want %d", tt.input, result, tt.output)
		}
	}
}

// TestNewRingInvalidEntries tests Ring creation with invalid entries.
func TestNewRingInvalidEntries(t *testing.T) {
	// Entries > 4096 should fail (after rounding up)
	_, err := NewRing(5000, 0)
	if err == nil {
		t.Error("Expected error for entries > 4096")
	}

	// Note: 0 entries gets rounded up to 1 by roundUpPow2, which is valid
}

// TestNewRingValidEntries tests Ring creation - will fail without kernel but validates API.
func TestNewRingValidEntries(t *testing.T) {
	// This will fail without io_uring support, but we're testing the API
	ring, err := NewRing(64, 0)
	if err != nil {
		// Expected without io_uring support
		t.Logf("NewRing error (expected without io_uring): %v", err)
		return
	}
	defer ring.Close()

	// If we got here, the ring was created successfully
	if ring.fd < 0 {
		t.Error("Ring fd should be non-negative")
	}
}
