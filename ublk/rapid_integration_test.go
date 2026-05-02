//go:build integration

package ublk

import (
	"bytes"
	"crypto/rand"
	"fmt"
	"testing"
	"time"

	"golang.org/x/sys/unix"
	"pgregory.net/rapid"
)

// TestRapidStateMachine is a property-based / model-based state-machine
// test for the device lifecycle and data plane.
//
// rapid generates pseudo-random sequences of (Write, Read, Fsync, Close,
// Create) actions against up to two live ublk devices and an in-process
// shadow model. After every action a probe Read is verified against the
// shadow. rapid will automatically shrink any failing sequence to a
// minimal reproducer — that is the primary value over the long-running
// fixed-shape TestTortureRandomIO soak.
//
// The four invariants checked here are:
//
//  1. A Read returns bytes from the most recent Write at that range
//     (per device).
//  2. Bytes written to device A never appear at the same offset on
//     device B (cross-device isolation).
//  3. Close terminates within a bounded time (5 s — enforced via a
//     timer; a hang in del_gendisk would otherwise block forever).
//  4. Close is idempotent — calling it again after a successful close
//     must not panic or hang.
//
// fd-close-before-Close discipline (see AGENTS.md): every action that
// opens /dev/ublkbN closes its fd before Device.Close runs. The state
// machine's closeDevice action therefore does unix.Close(fd) first, or
// del_gendisk would block waiting for the open ref to drop.
//
// Tunable via standard rapid flags / env vars (see `go test -args -h`):
// e.g. `RAPID_CHECKS=200 sudo /tmp/ublk.test -test.run=TestRapid`.
func TestRapidStateMachine(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		sm := newRapidSM(t)
		defer sm.cleanup()

		t.Repeat(map[string]func(*rapid.T){
			"create": sm.createDevice,
			"write":  sm.writeDevice,
			"read":   sm.readDevice,
			"fsync":  sm.fsyncDevice,
			"close":  sm.closeDevice,

			// Empty key: invariant check, runs after every action.
			"": sm.checkInvariants,
		})
	})
}

// rapidSM holds the live devices and the shadow model. rapid.T.Repeat
// invokes its methods in a randomly-generated order; bookkeeping must
// therefore tolerate any ordering of (create, write, read, fsync,
// close).
type rapidSM struct {
	t *rapid.T

	// All devices ever created in this Run; capped to keep each Run
	// cheap (each ublk.New takes ~10 ms on a warm host).
	totalCreated int

	// Live devices indexed by stable id. Entries removed on close.
	live map[int]*rapidDev

	// Per-device shadow of what the block device contains.
	model map[int][]byte

	// Monotonically increasing id assigned to every newly created device.
	nextID int
}

type rapidDev struct {
	id  int
	dev *Device
	fd  int
}

const (
	rapidBlk          = 4096
	rapidDevSize      = 2 * 1024 * 1024 // 2 MiB → 512 blocks; fast.
	rapidMaxLive      = 2               // up to 2 devices alive at a time
	rapidMaxCreates   = 16              // cap total ublk.New per Run
	rapidCloseTimeout = 5 * time.Second
)

// rapidWriteLengths constrains generated write/read sizes to a small
// set of block-aligned values (see PR description / TODO Testing).
var rapidWriteLengths = []int{rapidBlk / 8, rapidBlk, 2 * rapidBlk}

func init() {
	// Block size 512 + lengths above must all be ≥ 512 and aligned.
	// rapidBlk/8 = 512 satisfies this.
	if rapidBlk/8 != 512 {
		panic("rapidBlk/8 must equal 512 (kernel block size)")
	}
}

func newRapidSM(t *rapid.T) *rapidSM {
	return &rapidSM{
		t:     t,
		live:  make(map[int]*rapidDev),
		model: make(map[int][]byte),
	}
}

// cleanup tears down any devices left alive at the end of a Run. Always
// closes the user fd before Device.Close to avoid del_gendisk blocking.
func (s *rapidSM) cleanup() {
	for id, d := range s.live {
		_ = unix.Close(d.fd)
		_ = d.dev.Close()
		delete(s.live, id)
	}
}

func (s *rapidSM) createDevice(t *rapid.T) {
	if len(s.live) >= rapidMaxLive {
		t.Skip("max live devices reached")
	}
	if s.totalCreated >= rapidMaxCreates {
		t.Skip("max creates per Run reached")
	}

	be := newMemBackend(rapidDevSize)
	dev, err := New(be, rapidDevSize)
	if err != nil {
		t.Fatalf("ublk.New: %v", err)
	}
	fd, err := unix.Open(dev.Path(), unix.O_RDWR|unix.O_DIRECT, 0)
	if err != nil {
		_ = dev.Close()
		t.Fatalf("open %s: %v", dev.Path(), err)
	}

	id := s.nextID
	s.nextID++
	s.totalCreated++
	s.live[id] = &rapidDev{id: id, dev: dev, fd: fd}
	s.model[id] = make([]byte, rapidDevSize)
}

// pickLiveID draws one of the currently live device ids. Skips the
// action when no device exists (not fatal — rapid will simply try a
// different action).
func (s *rapidSM) pickLiveID(t *rapid.T, label string) int {
	if len(s.live) == 0 {
		t.Skip("no live devices")
	}
	ids := make([]int, 0, len(s.live))
	for id := range s.live {
		ids = append(ids, id)
	}
	// Stable order so rapid's shrinker can reason about the choice.
	for i := 1; i < len(ids); i++ {
		for j := i; j > 0 && ids[j-1] > ids[j]; j-- {
			ids[j-1], ids[j] = ids[j], ids[j-1]
		}
	}
	idx := rapid.IntRange(0, len(ids)-1).Draw(t, label)
	return ids[idx]
}

func (s *rapidSM) writeDevice(t *rapid.T) {
	id := s.pickLiveID(t, "writeDev")
	d := s.live[id]

	length := rapid.SampledFrom(rapidWriteLengths).Draw(t, "len")
	maxStartBlocks := (rapidDevSize - length) / rapidBlk
	startBlk := rapid.IntRange(0, maxStartBlocks).Draw(t, "startBlk")
	off := int64(startBlk * rapidBlk)

	buf := alignedBuf(length)
	if _, err := rand.Read(buf); err != nil {
		t.Fatalf("rand.Read: %v", err)
	}

	n, err := unix.Pwrite(d.fd, buf, off)
	if err != nil || n != length {
		t.Fatalf("dev=%d pwrite off=%d len=%d: n=%d err=%v",
			id, off, length, n, err)
	}
	copy(s.model[id][off:], buf)
}

func (s *rapidSM) readDevice(t *rapid.T) {
	id := s.pickLiveID(t, "readDev")
	d := s.live[id]

	length := rapid.SampledFrom(rapidWriteLengths).Draw(t, "len")
	maxStartBlocks := (rapidDevSize - length) / rapidBlk
	startBlk := rapid.IntRange(0, maxStartBlocks).Draw(t, "startBlk")
	off := int64(startBlk * rapidBlk)

	buf := alignedBuf(length)
	n, err := unix.Pread(d.fd, buf, off)
	if err != nil || n != length {
		t.Fatalf("dev=%d pread off=%d len=%d: n=%d err=%v",
			id, off, length, n, err)
	}

	want := s.model[id][off : off+int64(length)]
	if diff := firstDiff(buf, want); diff >= 0 {
		t.Fatalf("dev=%d READ mismatch at off=%d byte=%d shadow=0x%02x got=0x%02x",
			id, off, diff, want[diff], buf[diff])
	}
}

func (s *rapidSM) fsyncDevice(t *rapid.T) {
	id := s.pickLiveID(t, "fsyncDev")
	d := s.live[id]
	if err := unix.Fsync(d.fd); err != nil {
		t.Fatalf("dev=%d fsync: %v", id, err)
	}
}

// closeDevice does the full Close → idempotent recheck cycle:
//   - close the user fd first (AGENTS.md fd-close-before-Close
//     discipline; otherwise del_gendisk blocks),
//   - call dev.Close() under a 5 s timer (invariant 3),
//   - call dev.Close() a second time and assert it does not hang or
//     return an error (invariant 4 — mirrors TestCloseIdempotent).
func (s *rapidSM) closeDevice(t *rapid.T) {
	if len(s.live) == 0 {
		t.Skip("no live devices")
	}
	id := s.pickLiveID(t, "closeDev")
	d := s.live[id]

	if err := unix.Close(d.fd); err != nil {
		t.Fatalf("dev=%d close fd: %v", id, err)
	}

	if err := closeWithDeadline(d.dev, rapidCloseTimeout); err != nil {
		t.Fatalf("dev=%d Close (1st): %v", id, err)
	}
	if err := closeWithDeadline(d.dev, rapidCloseTimeout); err != nil {
		t.Fatalf("dev=%d Close (2nd, idempotent): %v", id, err)
	}

	delete(s.live, id)
	delete(s.model, id)
}

// checkInvariants runs after every Repeat action. It picks one block on
// each live device and asserts the device returns the model bytes
// (invariant 1) and that the same offset on every other live device
// returns *its* model bytes — i.e. no cross-device bleed (invariant 2,
// satisfied as long as invariant 1 holds for every device since each
// device has an independent shadow).
func (s *rapidSM) checkInvariants(t *rapid.T) {
	if len(s.live) == 0 {
		return
	}

	maxBlk := (rapidDevSize - rapidBlk) / rapidBlk
	startBlk := rapid.IntRange(0, maxBlk).Draw(t, "probeBlk")
	off := int64(startBlk * rapidBlk)

	for id, d := range s.live {
		buf := alignedBuf(rapidBlk)
		n, err := unix.Pread(d.fd, buf, off)
		if err != nil || n != rapidBlk {
			t.Fatalf("invariant probe dev=%d off=%d: n=%d err=%v",
				id, off, n, err)
		}
		want := s.model[id][off : off+rapidBlk]
		if !bytes.Equal(buf, want) {
			t.Fatalf("invariant violation: dev=%d off=%d byte=%d shadow=0x%02x got=0x%02x",
				id, off, firstDiff(buf, want), want[firstDiff(buf, want)], buf[firstDiff(buf, want)])
		}
	}
}

// closeWithDeadline runs dev.Close in a goroutine and fails if it has
// not returned within d. Invariant 3 — without the timer, a hang in
// del_gendisk would deadlock the test rather than report it as a
// failure rapid can shrink.
func closeWithDeadline(dev *Device, d time.Duration) error {
	done := make(chan error, 1)
	go func() { done <- dev.Close() }()
	select {
	case err := <-done:
		return err
	case <-time.After(d):
		return fmt.Errorf("Close did not return within %v", d)
	}
}
