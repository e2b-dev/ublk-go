package ublk

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRoundUpPow2(t *testing.T) {
	t.Parallel()
	tests := []struct{ input, want uint }{
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
		{1024, 1024},
	}
	for _, tt := range tests {
		assert.Equal(t, tt.want, roundUpPow2(tt.input), "roundUpPow2(%d)", tt.input)
	}
}

func TestNewRingInvalidEntries(t *testing.T) {
	t.Parallel()
	_, err := NewRing(5000, 0)
	assert.Error(t, err, "entries > 4096 should fail")
}

func TestNewRingValidEntries(t *testing.T) {
	ring, err := NewRing(64, 0)
	if err != nil {
		t.Skipf("io_uring not available: %v", err)
	}
	defer ring.Close()
	assert.GreaterOrEqual(t, ring.fd, 0)
}

func TestRingOptions(t *testing.T) {
	t.Parallel()
	t.Run("DeferTaskrun implies SingleIssuer", func(t *testing.T) {
		t.Parallel()
		cfg := &ringConfig{}
		WithDeferTaskrun()(cfg)
		assert.True(t, cfg.deferTaskrun)
		assert.True(t, cfg.singleIssuer)
	})
}

func TestRingWithOptions(t *testing.T) {
	ring, err := NewRingWithOptions(64, 0, WithSingleIssuer())
	if err != nil {
		t.Skipf("io_uring not available: %v", err)
	}
	defer ring.Close()
	require.NotZero(t, ring.flags&IORING_SETUP_SINGLE_ISSUER)
}
