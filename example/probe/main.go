// Autonomous end-to-end probe for ublk-go.
//
// Creates a ublk device and exercises both sides of the stack:
//
//   - device-level direct I/O (O_DIRECT through /dev/ublkbN) to confirm
//     1:1 mapping between kernel offsets and what Backend.ReadAt/WriteAt
//     actually see.
//   - filesystem-level I/O through ext4 (mkfs, mount, write, fsync,
//     drop caches, readback, concurrent writers, remount/journal replay,
//     umount, close).
//
// Every step runs with its own timeout; a hang triggers a panic that
// dumps all goroutine stacks. Intended for unattended use by CI,
// scripts, or AI agents. Requires root and a loaded ublk_drv module:
//
//	sudo modprobe ublk_drv
//	sudo go run ./example/probe
//
// Exit 0 = all steps passed. Non-zero = first failure (cleanup is
// best-effort).
package main

import (
	"bytes"
	"crypto/rand"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"

	"github.com/e2b-dev/ublk-go/ublk"
)

const (
	devSize = 128 * 1024 * 1024 // 128 MiB
	blkSize = 4096
)

// loggingBackend is an in-memory Backend that counts I/O and lets the
// probe read its state to verify "did this byte actually reach me"
// independently of any kernel page cache.
type loggingBackend struct {
	mu         sync.RWMutex
	data       []byte
	reads      atomic.Int64
	writes     atomic.Int64
	readBytes  atomic.Int64
	writeBytes atomic.Int64
}

func (b *loggingBackend) ReadAt(p []byte, off int64) (int, error) {
	b.reads.Add(1)
	b.readBytes.Add(int64(len(p)))
	b.mu.RLock()
	defer b.mu.RUnlock()
	return copy(p, b.data[off:off+int64(len(p))]), nil
}

func (b *loggingBackend) WriteAt(p []byte, off int64) (int, error) {
	b.writes.Add(1)
	b.writeBytes.Add(int64(len(p)))
	b.mu.Lock()
	defer b.mu.Unlock()
	return copy(b.data[off:off+int64(len(p))], p), nil
}

// slice returns a copy of a range of the backend's storage. Safe to
// call from the probe while the worker may be writing — takes the
// backend's RLock.
func (b *loggingBackend) slice(off int64, n int) []byte {
	b.mu.RLock()
	defer b.mu.RUnlock()
	out := make([]byte, n)
	copy(out, b.data[off:off+int64(n)])
	return out
}

type probe struct {
	backend    *loggingBackend
	dev        *ublk.Device
	mountpoint string
	devPath    string
	mounted    bool
	timeout    time.Duration
}

func main() {
	timeout := flag.Duration("step-timeout", 30*time.Second, "per-step timeout")
	flag.Parse()

	log.SetFlags(log.Ltime | log.Lmicroseconds)

	if os.Getuid() != 0 {
		log.Fatal("probe must be run as root")
	}

	p := &probe{
		backend: &loggingBackend{data: make([]byte, devSize)},
		timeout: *timeout,
	}

	err := p.run()
	p.cleanup()
	if err != nil {
		log.Printf("FAIL: %v", err)
		os.Exit(1)
	}
	log.Printf("PASS: all steps succeeded")
}

func (p *probe) run() error {
	steps := []struct {
		name string
		fn   func() error
	}{
		{"create device", p.createDevice},
		{"blockdev size matches Backend size", p.checkBlockdevSize},
		{"direct I/O zero-read (pre-mkfs)", p.directZeroRead},
		{"direct I/O write-read roundtrip matches backend", p.directRoundtrip},
		{"mkfs.ext4", p.mkfs},
		{"mount", p.mountFS},
		{"single write + syncfs triggers backend writes", p.singleWrite},
		{"fsync propagates writes to backend", p.fsyncVisible},
		{"drop caches + readback triggers backend reads", p.dropAndVerify},
		{"readback value matches backend storage directly", p.backendEquivalence},
		{"concurrent writers", p.concurrent},
		{"remount (journal replay)", p.remount},
		{"unmount", p.umount},
		{"close device", p.closeDevice},
		{"verify device path gone", p.verifyGone},
	}
	for _, s := range steps {
		if err := p.step(s.name, s.fn); err != nil {
			return err
		}
	}
	return nil
}

func (p *probe) step(name string, fn func() error) error {
	start := time.Now()
	log.Printf("=== %s", name)

	done := make(chan error, 1)
	go func() { done <- fn() }()

	select {
	case err := <-done:
		if err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		p.stats(time.Since(start))
		return nil
	case <-time.After(p.timeout):
		// Dump goroutines so the user (or agent) can diagnose.
		panic(fmt.Sprintf("step %q hung for %v — dumping goroutines", name, p.timeout))
	}
}

func (p *probe) stats(elapsed time.Duration) {
	b := p.backend
	log.Printf("    ok in %-10v   reads=%-5d (%5d KiB)  writes=%-5d (%5d KiB)",
		elapsed.Truncate(time.Microsecond),
		b.reads.Load(), b.readBytes.Load()/1024,
		b.writes.Load(), b.writeBytes.Load()/1024)
}

func (p *probe) cleanup() {
	if p.mounted {
		_ = exec.Command("umount", p.mountpoint).Run()
		p.mounted = false
	}
	if p.mountpoint != "" {
		_ = os.Remove(p.mountpoint)
	}
	if p.dev != nil {
		_ = p.dev.Close()
		p.dev = nil
	}
}

// --- steps ---

func (p *probe) createDevice() error {
	dev, err := ublk.New(p.backend, devSize)
	if err != nil {
		return err
	}
	p.dev = dev
	p.devPath = dev.BlockDevicePath()
	log.Printf("    created %s", p.devPath)

	fi, err := os.Stat(p.devPath)
	if err != nil {
		return fmt.Errorf("stat %s: %w", p.devPath, err)
	}
	if fi.Mode()&os.ModeDevice == 0 {
		return fmt.Errorf("%s is not a block device (mode=%v)", p.devPath, fi.Mode())
	}
	return nil
}

// checkBlockdevSize uses BLKGETSIZE64 to ask the kernel how big the
// device is and verifies it equals what we told ublk.New.
func (p *probe) checkBlockdevSize() error {
	fd, err := unix.Open(p.devPath, unix.O_RDONLY, 0)
	if err != nil {
		return err
	}
	defer unix.Close(fd)

	var size uint64
	const BLKGETSIZE64 = 0x80081272
	if _, _, errno := unix.Syscall(
		unix.SYS_IOCTL, uintptr(fd), BLKGETSIZE64,
		uintptr(unsafe.Pointer(&size)),
	); errno != 0 {
		return fmt.Errorf("BLKGETSIZE64: %w", errno)
	}
	if size != devSize {
		return fmt.Errorf("kernel says device is %d bytes, want %d", size, devSize)
	}
	return nil
}

// directZeroRead opens the raw block device with O_DIRECT (bypasses the
// page cache), reads a block, and verifies it's all zeros — which it
// must be because our backend starts zeroed and there have been no
// writes yet. Also confirms Backend.ReadAt is actually reachable.
func (p *probe) directZeroRead() error {
	before := p.backend.reads.Load()

	buf, err := directRead(p.devPath, 0, blkSize)
	if err != nil {
		return err
	}
	if !bytes.Equal(buf, make([]byte, blkSize)) {
		return errors.New("first 4K of fresh device is not zero")
	}
	if p.backend.reads.Load() == before {
		return errors.New("Backend.ReadAt was not called during O_DIRECT read")
	}
	return nil
}

// directRoundtrip writes a random 4K block directly to the device,
// reads it back via O_DIRECT, and verifies (a) the readback matches,
// (b) the backend's raw storage actually holds the pattern at the same
// offset. This is the strongest guarantee we can give that kernel
// offsets translate 1:1 to Backend.WriteAt offsets.
func (p *probe) directRoundtrip() error {
	pattern := make([]byte, blkSize)
	if _, err := rand.Read(pattern); err != nil {
		return err
	}
	const off int64 = 8 * blkSize

	if err := directWrite(p.devPath, off, pattern); err != nil {
		return fmt.Errorf("direct write: %w", err)
	}

	got, err := directRead(p.devPath, off, blkSize)
	if err != nil {
		return fmt.Errorf("direct read: %w", err)
	}
	if !bytes.Equal(got, pattern) {
		return errors.New("direct read did not match what we wrote")
	}

	// And the backend itself must hold the same bytes at the same
	// offset — anything else means kernel/userspace disagree on the
	// data layout.
	backend := p.backend.slice(off, blkSize)
	if !bytes.Equal(backend, pattern) {
		return errors.New("backend storage does not match what the kernel wrote at the same offset")
	}
	return nil
}

func (p *probe) mkfs() error {
	return runCmd("mkfs.ext4", "-q", "-F", p.devPath)
}

func (p *probe) mountFS() error {
	mp, err := os.MkdirTemp("", "ublk-probe-*")
	if err != nil {
		return err
	}
	p.mountpoint = mp
	if err := runCmd("mount", p.devPath, mp); err != nil {
		return err
	}
	p.mounted = true
	log.Printf("    mounted at %s", mp)
	return nil
}

func (p *probe) singleWrite() error {
	before := p.backend.writes.Load()

	content := bytes.Repeat([]byte("probe write: ublk-go smoke test\n"), 256)
	f := p.mountpoint + "/probe.txt"
	if err := os.WriteFile(f, content, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", f, err)
	}
	if err := runCmd("sync", "-f", p.mountpoint); err != nil {
		return fmt.Errorf("syncfs: %w", err)
	}
	if p.backend.writes.Load() == before {
		return errors.New("syncfs did not cause any Backend.WriteAt calls")
	}
	return nil
}

// fsyncVisible opens a file, writes, fsyncs, and asserts that fsync
// alone (without any other sync command) flushes dirty pages down to
// Backend.WriteAt.
func (p *probe) fsyncVisible() error {
	before := p.backend.writes.Load()

	f, err := os.Create(p.mountpoint + "/fsync.bin")
	if err != nil {
		return err
	}
	defer f.Close()

	data := bytes.Repeat([]byte("F"), 32*1024)
	if _, err := f.Write(data); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return fmt.Errorf("fsync: %w", err)
	}
	if p.backend.writes.Load() == before {
		return errors.New("fsync did not cause any Backend.WriteAt calls")
	}
	return nil
}

func (p *probe) dropAndVerify() error {
	before := p.backend.reads.Load()

	if err := os.WriteFile("/proc/sys/vm/drop_caches", []byte("3"), 0); err != nil {
		return fmt.Errorf("drop_caches: %w", err)
	}
	want, err := os.ReadFile(p.mountpoint + "/probe.txt")
	if err != nil {
		return fmt.Errorf("read probe.txt: %w", err)
	}
	if !bytes.HasPrefix(want, []byte("probe write:")) {
		return errors.New("probe.txt did not round-trip correctly")
	}
	if p.backend.reads.Load() == before {
		return errors.New("post-drop_caches read was served entirely from cache; Backend.ReadAt not invoked")
	}
	return nil
}

// backendEquivalence proves filesystem-level reads ultimately come from
// the backend by searching for our magic pattern in the raw backend
// storage (across the whole device, since we don't know where ext4
// placed probe.txt's extent).
func (p *probe) backendEquivalence() error {
	pattern := []byte("probe write: ublk-go smoke test\n")
	// Scan in 4K chunks to keep memory usage tiny.
	for off := int64(0); off+int64(len(pattern)) <= devSize; off += blkSize {
		chunk := p.backend.slice(off, blkSize)
		if bytes.Contains(chunk, pattern) {
			log.Printf("    found probe.txt contents at backend offset %d", off)
			return nil
		}
	}
	return errors.New("probe.txt pattern not found anywhere in backend storage")
}

func (p *probe) concurrent() error {
	const (
		workers = 8
		files   = 16
		size    = 64 * 1024
	)
	before := p.backend.writes.Load()

	var wg sync.WaitGroup
	errCh := make(chan error, workers)
	for w := range workers {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			buf := make([]byte, size)
			if _, err := rand.Read(buf); err != nil {
				errCh <- err
				return
			}
			for i := range files {
				name := fmt.Sprintf("%s/w%d-f%d.bin", p.mountpoint, id, i)
				if err := os.WriteFile(name, buf, 0o644); err != nil {
					errCh <- fmt.Errorf("write %s: %w", name, err)
					return
				}
			}
		}(w)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		if err != nil {
			return err
		}
	}
	if err := runCmd("sync", "-f", p.mountpoint); err != nil {
		return err
	}
	if p.backend.writes.Load() == before {
		return errors.New("concurrent writers produced no Backend.WriteAt calls")
	}
	return nil
}

func (p *probe) remount() error {
	if err := runCmd("umount", p.mountpoint); err != nil {
		return err
	}
	p.mounted = false
	if err := runCmd("mount", p.devPath, p.mountpoint); err != nil {
		return err
	}
	p.mounted = true
	if _, err := os.Stat(p.mountpoint + "/probe.txt"); err != nil {
		return fmt.Errorf("probe.txt missing after remount: %w", err)
	}
	return nil
}

func (p *probe) umount() error {
	if err := runCmd("umount", p.mountpoint); err != nil {
		return err
	}
	p.mounted = false
	return nil
}

func (p *probe) closeDevice() error {
	err := p.dev.Close()
	p.dev = nil
	return err
}

func (p *probe) verifyGone() error {
	// Derive /dev/ublkcN from /dev/ublkbN; they share the minor number
	// from ublk's POV.
	charPath := "/dev/ublkc" + p.devPath[len("/dev/ublkb"):]

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		_, blkErr := os.Stat(p.devPath)
		_, chrErr := os.Stat(charPath)
		if errors.Is(blkErr, os.ErrNotExist) && errors.Is(chrErr, os.ErrNotExist) {
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	blkState := describeStat(p.devPath)
	chrState := describeStat(charPath)
	return fmt.Errorf("leftover device nodes after Close(): %s (%s), %s (%s)",
		p.devPath, blkState, charPath, chrState)
}

func describeStat(path string) string {
	_, err := os.Stat(path)
	if err == nil {
		return "still exists"
	}
	return err.Error()
}

// --- helpers ---

// alignedBuf returns a 4096-aligned byte slice of the requested size,
// required for O_DIRECT on most filesystems.
func alignedBuf(size int) []byte {
	const align = 4096
	raw := make([]byte, size+align)
	addr := uintptr(unsafe.Pointer(&raw[0]))
	off := int(uintptr(align) - addr%uintptr(align))
	if off == align {
		off = 0
	}
	return raw[off : off+size]
}

func directRead(path string, off int64, n int) ([]byte, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_DIRECT, 0)
	if err != nil {
		return nil, err
	}
	defer unix.Close(fd)

	buf := alignedBuf(n)
	got, err := unix.Pread(fd, buf, off)
	if err != nil {
		return nil, err
	}
	if got != n {
		return nil, fmt.Errorf("short read: %d/%d", got, n)
	}
	return buf, nil
}

func directWrite(path string, off int64, data []byte) error {
	fd, err := unix.Open(path, unix.O_WRONLY|unix.O_DIRECT, 0)
	if err != nil {
		return err
	}
	defer unix.Close(fd)

	buf := alignedBuf(len(data))
	copy(buf, data)

	n, err := unix.Pwrite(fd, buf, off)
	if err != nil {
		return err
	}
	if n != len(data) {
		return fmt.Errorf("short write: %d/%d", n, len(data))
	}
	return nil
}

func runCmd(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}
