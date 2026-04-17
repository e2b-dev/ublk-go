package uring

import (
	"testing"
	"unsafe"
)

func TestStructSizes(t *testing.T) {
	if unsafe.Sizeof(SQE128{}) != 128 {
		t.Fatalf("SQE128 is %d bytes, want 128", unsafe.Sizeof(SQE128{}))
	}
	if unsafe.Sizeof(SQE64{}) != 64 {
		t.Fatalf("SQE64 is %d bytes, want 64", unsafe.Sizeof(SQE64{}))
	}
	if unsafe.Sizeof(CQE{}) != 16 {
		t.Fatalf("CQE is %d bytes, want 16", unsafe.Sizeof(CQE{}))
	}

	var s SQE128
	off := uintptr(unsafe.Pointer(&s.Cmd[0])) - uintptr(unsafe.Pointer(&s))
	if off != 48 {
		t.Fatalf("SQE128.Cmd at offset %d, want 48", off)
	}
}

func TestNOPRoundTrip(t *testing.T) {
	r, err := New(16)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer r.Close()

	for i := range 16 {
		sqe := r.GetSQE64()
		if sqe == nil {
			t.Fatalf("GetSQE64 nil at %d", i)
		}
		sqe.Opcode = 0 // NOP
		sqe.UserData = uint64(i) + 1
	}

	n, err := r.Submit()
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if n != 16 {
		t.Fatalf("submitted %d, want 16", n)
	}

	seen := make(map[uint64]bool)
	for range 16 {
		c, err := r.WaitCQE()
		if err != nil {
			t.Fatalf("WaitCQE: %v", err)
		}
		if c.Res != 0 {
			t.Errorf("NOP res=%d", c.Res)
		}
		seen[c.UserData] = true
		r.SeenCQE()
	}

	for i := uint64(1); i <= 16; i++ {
		if !seen[i] {
			t.Errorf("missing CQE for UserData=%d", i)
		}
	}
}

func TestManyCycles(t *testing.T) {
	r, err := New(4)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer r.Close()

	for cycle := range 200 {
		for i := range int(r.SQEntries()) {
			sqe := r.GetSQE64()
			if sqe == nil {
				t.Fatalf("cycle %d: GetSQE64 nil at %d", cycle, i)
			}
			sqe.Opcode = 0
			sqe.UserData = uint64(cycle*1000 + i)
		}
		if _, err := r.Submit(); err != nil {
			t.Fatalf("cycle %d: Submit: %v", cycle, err)
		}
		for range r.SQEntries() {
			c, err := r.WaitCQE()
			if err != nil {
				t.Fatalf("cycle %d: WaitCQE: %v", cycle, err)
			}
			if c.Res != 0 {
				t.Fatalf("cycle %d: NOP res=%d", cycle, c.Res)
			}
			r.SeenCQE()
		}
	}
}
