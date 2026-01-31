package ublk

import (
	"testing"
)

func FuzzUblkIOCommandFromBytes(f *testing.F) {
	// Seed corpus
	f.Add([]byte{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0}) // 16 zeros
	f.Add([]byte("short"))                                        // Short

	f.Fuzz(func(t *testing.T, data []byte) {
		cmd := UblkIOCommandFromBytes(data)
		if len(data) < 16 {
			if cmd != nil {
				t.Errorf("Expected nil for length %d, got %v", len(data), cmd)
			}
			return
		}
		if cmd == nil {
			t.Errorf("Expected non-nil for length %d", len(data))
			return
		}
		// Access fields to ensure no panic/segfault (though it's just a cast)
		_ = cmd.QID
		_ = cmd.Tag
		_ = cmd.Addr
		_ = cmd.Result
	})
}
