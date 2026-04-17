package uring

import (
	"errors"
	"testing"
	"time"
	"unsafe"
)

func TestStructSizes(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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

func TestNewSQE128(t *testing.T) {
	t.Parallel()
	r, err := NewSQE128(8)
	if err != nil {
		t.Fatalf("NewSQE128: %v", err)
	}
	defer r.Close()

	sqe := r.GetSQE128()
	if sqe == nil {
		t.Fatal("GetSQE128 returned nil from a fresh ring")
	}

	// All 8 slots must be available, then the 9th should return nil.
	// We've already taken one, so 7 more succeed, then the 8th fails.
	for i := range 7 {
		if r.GetSQE128() == nil {
			t.Fatalf("GetSQE128 returned nil at i=%d; ring should have 7 more slots", i)
		}
	}
	if r.GetSQE128() != nil {
		t.Fatal("GetSQE128 returned non-nil after filling all 8 slots")
	}
}

func TestCancelUnblocksWaitCQE(t *testing.T) {
	t.Parallel()
	r, err := New(4)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer r.Close()

	done := make(chan error, 1)
	go func() {
		_, err := r.WaitCQE()
		done <- err
	}()

	// Give WaitCQE time to enter epoll_wait.
	time.Sleep(10 * time.Millisecond)
	r.Cancel()

	select {
	case err := <-done:
		if !errors.Is(err, ErrCancelled) {
			t.Fatalf("WaitCQE after Cancel: got %v, want ErrCancelled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("WaitCQE did not unblock within 1s after Cancel")
	}
}

func TestPeekCQE(t *testing.T) {
	t.Parallel()
	r, err := New(4)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer r.Close()

	// Nothing submitted yet — PeekCQE must return nil.
	if c := r.PeekCQE(); c != nil {
		t.Fatalf("PeekCQE on empty ring returned %+v, want nil", c)
	}

	// Submit one NOP and SubmitAndWait — covers both SubmitAndWait
	// and the populated PeekCQE branch.
	sqe := r.GetSQE64()
	if sqe == nil {
		t.Fatal("GetSQE64 nil on fresh ring")
	}
	sqe.Opcode = 0
	sqe.UserData = 0xDEADBEEF
	if _, err := r.SubmitAndWait(); err != nil {
		t.Fatalf("SubmitAndWait: %v", err)
	}

	c := r.PeekCQE()
	if c == nil {
		t.Fatal("PeekCQE after SubmitAndWait returned nil, want a CQE")
	}
	if c.UserData != 0xDEADBEEF {
		t.Fatalf("PeekCQE CQE.UserData = %#x, want 0xDEADBEEF", c.UserData)
	}
	r.SeenCQE()

	// After draining the CQE, PeekCQE must again return nil.
	if c := r.PeekCQE(); c != nil {
		t.Fatalf("PeekCQE after drain returned %+v, want nil", c)
	}
}

func TestManyCycles(t *testing.T) {
	t.Parallel()
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
