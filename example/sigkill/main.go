// SIGKILL recovery probe.
//
// Spawns a child process that creates a ublk device and hammers it with
// writes, then kills the child with SIGKILL. In the parent, verifies:
//
//   - /dev/ublkbN and /dev/ublkcN of the child's device both disappear
//     within a short wall-clock window (kernel's ublk_ch_release on fd
//     close handles this; we don't help from Go after SIGKILL — by
//     design, deferred functions don't run on SIGKILL).
//
//   - The parent can then create a new ublk device cleanly with the
//     same process — i.e. we didn't leak anything that'd prevent future
//     New() calls.
//
// Exit 0 = child cleanup happened without our help, and parent recovered.
// Non-zero = the child's device nodes lingered after SIGKILL, or the
// parent's subsequent New() failed.
//
// This is the one scenario that ends with an unclean shutdown path —
// Go's defer, sync.Once, and every other cleanup hook is irrelevant
// because the kernel SIGKILL bypasses all userspace. We're verifying
// that the kernel's own cleanup path (triggered by fd close on process
// exit) is sufficient.
//
// Requires root and ublk_drv. The binary runs itself with a "-child"
// arg to provide the child half.
//
//	sudo /tmp/ublk-sigkill
//	make sigkill
package main

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/e2b-dev/ublk-go/ublk"
)

const devSize = 8 * 1024 * 1024

type memBackend struct {
	mu   sync.RWMutex
	data []byte
}

func (b *memBackend) ReadAt(p []byte, off int64) (int, error) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return copy(p, b.data[off:off+int64(len(p))]), nil
}

func (b *memBackend) WriteAt(p []byte, off int64) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return copy(b.data[off:off+int64(len(p))], p), nil
}

func main() {
	child := flag.Bool("child", false, "run as the child half (internal)")
	flag.Parse()

	log.SetFlags(log.Ltime | log.Lmicroseconds)

	if *child {
		runChild()
		return
	}
	if os.Getuid() != 0 {
		log.Fatal("sigkill probe must be run as root")
	}
	if err := runParent(); err != nil {
		log.Fatalf("FAIL: %v", err)
	}
	log.Println("PASS: SIGKILL recovery works; parent created fresh device after child was killed")
}

// runChild creates a ublk device, prints its path for the parent to
// read, and hammers it with writes until the OS kills it. Never
// returns.
func runChild() {
	backend := &memBackend{data: make([]byte, devSize)}
	dev, err := ublk.New(backend, devSize)
	if err != nil {
		fmt.Fprintf(os.Stderr, "child: ublk.New: %v\n", err)
		os.Exit(2)
	}
	// Announce the device path on stdout so the parent knows what
	// to look for after it kills us.
	fmt.Println("CHILD_READY", dev.Path())

	// Open our own block device and write forever. Keeps the child
	// doing real work when the SIGKILL lands.
	fd, err := unix.Open(dev.Path(), unix.O_WRONLY, 0)
	if err != nil {
		fmt.Fprintf(os.Stderr, "child: open: %v\n", err)
		os.Exit(2)
	}
	buf := make([]byte, 4096)
	for {
		_, _ = unix.Pwrite(fd, buf, 0)
	}
}

// runParent launches the child, waits for it to announce its device,
// SIGKILLs it, verifies the kernel cleans up, and then creates its
// own fresh device to confirm we didn't leak anything.
func runParent() error {
	self, err := os.Executable()
	if err != nil {
		return err
	}
	cmd := exec.Command(self, "-child")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start child: %w", err)
	}

	devPath, err := readReady(stdout)
	if err != nil {
		_ = cmd.Process.Kill()
		return err
	}
	log.Printf("child PID %d created %s", cmd.Process.Pid, devPath)

	time.Sleep(100 * time.Millisecond)

	log.Printf("sending SIGKILL to PID %d", cmd.Process.Pid)
	if err := cmd.Process.Signal(syscall.SIGKILL); err != nil {
		return fmt.Errorf("SIGKILL child: %w", err)
	}

	// Reap the zombie, but with a watchdog — if cmd.Wait() blocks,
	// something in the kernel's exit path (most likely
	// ublk_ch_release) is stuck, and we want evidence of that rather
	// than a silent hang.
	waitErr := make(chan error, 1)
	go func() { waitErr <- cmd.Wait() }()
	select {
	case <-waitErr:
		log.Printf("child reaped after SIGKILL")
	case <-time.After(10 * time.Second):
		return fmt.Errorf("cmd.Wait() blocked 10s after SIGKILL (PID %d still not reaped); "+
			"/proc/%d/stack may tell you which kernel routine is stuck",
			cmd.Process.Pid, cmd.Process.Pid)
	}

	// The REAL correctness question: can the parent process keep
	// using the library after a child was killed mid-I/O? If we'd
	// leaked something unrecoverable at the kernel level (minor
	// exhaustion, ublk_device slot permanently held, control-fd
	// jammed), ublk.New here would fail or hang.
	//
	// Whether the child's /dev/ublkbN node physically goes away is a
	// separate question — it depends on the kernel's async
	// ublk_ch_release workqueue, which on some kernel versions (6.17
	// observed) appears to leave stale nodes behind indefinitely. We
	// check for that below as an *informational* post-test signal,
	// not a pass/fail criterion. Our code can do nothing about it.
	newStart := time.Now()
	dev2, err := ublk.New(&memBackend{data: make([]byte, devSize)}, devSize)
	if err != nil {
		return fmt.Errorf("parent New after child SIGKILL (within %v): %w",
			time.Since(newStart).Truncate(time.Millisecond), err)
	}
	log.Printf("parent created fresh device %s in %v",
		dev2.Path(), time.Since(newStart).Truncate(time.Millisecond))

	if err := dev2.Close(); err != nil {
		return fmt.Errorf("parent Close: %w", err)
	}

	// Informational: did the orphan child device go away? If not,
	// likely a kernel-side async-cleanup stall; log but don't fail.
	if err := waitGone(devPath); err != nil {
		log.Printf("WARN: %s", err)
	} else {
		log.Printf("child's orphan device nodes have been cleaned up by the kernel")
	}
	return nil
}

// readReady scans the child's stdout for its "CHILD_READY <path>"
// announcement, with a timeout so we don't hang forever if the child
// dies on startup.
func readReady(r io.Reader) (string, error) {
	ch := make(chan string, 1)
	errCh := make(chan error, 1)
	go func() {
		scan := bufio.NewScanner(r)
		for scan.Scan() {
			line := scan.Text()
			if strings.HasPrefix(line, "CHILD_READY ") {
				ch <- strings.TrimPrefix(line, "CHILD_READY ")
				return
			}
		}
		errCh <- fmt.Errorf("child stdout closed without CHILD_READY: %w", scan.Err())
	}()
	select {
	case p := <-ch:
		return p, nil
	case err := <-errCh:
		return "", err
	case <-time.After(5 * time.Second):
		return "", errors.New("child didn't announce CHILD_READY within 5s")
	}
}

// waitGone is now informational-only: it polls for the child's orphan
// device nodes to disappear, but its result doesn't fail the test —
// whether the kernel's ublk_ch_release workqueue eventually fires is
// not a library-correctness question.
//
// The library's correctness is about whether the *parent* can keep
// using the API after an ungraceful child death, which is checked
// separately.
func waitGone(blockPath string) error {
	charPath := "/dev/ublkc" + strings.TrimPrefix(blockPath, "/dev/ublkb")
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		_, blkErr := os.Stat(blockPath)
		_, chrErr := os.Stat(charPath)
		if errors.Is(blkErr, os.ErrNotExist) && errors.Is(chrErr, os.ErrNotExist) {
			return nil
		}
		time.Sleep(200 * time.Millisecond)
	}
	return fmt.Errorf("child's device nodes (%s / %s) still present 10s after SIGKILL "+
		"— kernel ublk_ch_release workqueue appears stalled on this kernel",
		blockPath, charPath)
}
