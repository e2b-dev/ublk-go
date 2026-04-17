//go:build integration

package ublk

import (
	"bytes"
	"crypto/rand"
	"testing"
)

func TestMaxIOBoundary(t *testing.T) {
	t.Parallel()

	const (
		size   = 2 * 1024 * 1024
		maxIO  = 256 * 512
		offset = size - maxIO
	)

	dev, backend := makeDevice(t, size)
	path := dev.Path()

	pattern := make([]byte, maxIO)
	if _, err := rand.Read(pattern); err != nil {
		t.Fatalf("rand.Read: %v", err)
	}

	if err := directWrite(path, offset, pattern); err != nil {
		t.Fatalf("directWrite: %v", err)
	}

	got, err := directRead(path, offset, maxIO)
	if err != nil {
		t.Fatalf("directRead: %v", err)
	}
	if !bytes.Equal(got, pattern) {
		t.Fatalf("direct read mismatch at 128 KiB boundary (first diff at byte %d)", firstDiff(got, pattern))
	}

	stored := backend.snapshot()[offset : offset+maxIO]
	if !bytes.Equal(stored, pattern) {
		t.Fatalf("backend mismatch at 128 KiB boundary (first diff at byte %d)", firstDiff(stored, pattern))
	}
}
