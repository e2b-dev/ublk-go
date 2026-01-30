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

// TestNewRingWithOptions tests ring creation with performance options.
func TestNewRingWithOptions(t *testing.T) {
	// Test that options apply flags correctly
	t.Run("SingleIssuer", func(t *testing.T) {
		cfg := &ringConfig{}
		WithSingleIssuer()(cfg)
		if !cfg.singleIssuer {
			t.Error("WithSingleIssuer should set singleIssuer")
		}
	})

	t.Run("DeferTaskrun", func(t *testing.T) {
		cfg := &ringConfig{}
		WithDeferTaskrun()(cfg)
		if !cfg.deferTaskrun {
			t.Error("WithDeferTaskrun should set deferTaskrun")
		}
		if !cfg.singleIssuer {
			t.Error("WithDeferTaskrun should also set singleIssuer (required)")
		}
	})

	t.Run("CoopTaskrun", func(t *testing.T) {
		cfg := &ringConfig{}
		WithCoopTaskrun()(cfg)
		if !cfg.coopTaskrun {
			t.Error("WithCoopTaskrun should set coopTaskrun")
		}
	})

	// Test actual ring creation with options (may fail without kernel support)
	t.Run("CreateWithOptions", func(t *testing.T) {
		ring, err := NewRingWithOptions(64, 0, WithSingleIssuer())
		if err != nil {
			t.Logf("NewRingWithOptions error (expected without kernel 6.0+): %v", err)
			return
		}
		defer ring.Close()

		if ring.flags&IORING_SETUP_SINGLE_ISSUER == 0 {
			t.Error("Ring should have SINGLE_ISSUER flag set")
		}
	})
}

// TestRingSetupFlags tests that io_uring setup flags have correct values.
func TestRingSetupFlags(t *testing.T) {
	// Verify the flag values match kernel ABI
	tests := []struct {
		name  string
		value uint
		want  uint
	}{
		{"IORING_SETUP_IOPOLL", IORING_SETUP_IOPOLL, 1 << 0},
		{"IORING_SETUP_SQPOLL", IORING_SETUP_SQPOLL, 1 << 1},
		{"IORING_SETUP_SQ_AFF", IORING_SETUP_SQ_AFF, 1 << 2},
		{"IORING_SETUP_COOP_TASKRUN", IORING_SETUP_COOP_TASKRUN, 1 << 8},
		{"IORING_SETUP_SINGLE_ISSUER", IORING_SETUP_SINGLE_ISSUER, 1 << 12},
		{"IORING_SETUP_DEFER_TASKRUN", IORING_SETUP_DEFER_TASKRUN, 1 << 13},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.value != tt.want {
				t.Errorf("%s = %d, want %d", tt.name, tt.value, tt.want)
			}
		})
	}
}
