package ublk

import (
	"testing"
)

func FuzzUblkIOCommandFromBytes(f *testing.F) {
	// Seed corpus
	f.Add([]byte{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0}) // 24 zeros
	f.Add([]byte("short"))                                                                // Short

	f.Fuzz(func(t *testing.T, data []byte) {
		cmd := UblkIOCommandFromBytes(data)
		if len(data) < 24 {
			if cmd != nil {
				t.Errorf("Expected nil for length %d, got %v", len(data), cmd)
			}
		} else {
			if cmd == nil {
				t.Errorf("Expected non-nil for length %d", len(data))
			}
			// Access fields to ensure no panic/segfault (though it's just a cast)
			_ = cmd.QID
			_ = cmd.Tag
			_ = cmd.Addr
			_ = cmd.Result
		}
	})
}
