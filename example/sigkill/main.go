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
	fmt.Println("CHILD_READY", dev.BlockDevicePath())

	// Open our own block device and write forever. Keeps the child
	// doing real work when the SIGKILL lands.
	fd, err := unix.Open(dev.BlockDevicePath(), unix.O_WRONLY, 0)
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

	if err := waitGone(devPath); err != nil {
		return err
	}
	log.Printf("device nodes removed cleanly by the kernel")

	// Now the real assertion: can the parent create a new device?
	// If we'd leaked anything at the kernel level (minor numbers,
	// char device inodes, workqueue items, ublk_device slots), this
	// would fail or hang.
	dev2, err := ublk.New(&memBackend{data: make([]byte, devSize)}, devSize)
	if err != nil {
		return fmt.Errorf("parent New after child SIGKILL: %w", err)
	}
	log.Printf("parent created fresh device %s", dev2.BlockDevicePath())
	if err := dev2.Close(); err != nil {
		return fmt.Errorf("parent Close: %w", err)
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

// waitGone polls for both the block and char device nodes to disappear.
//
// The kernel's ublk_ch_release handler runs in a workqueue (async);
// actual device-node removal happens some time after process exit.
// On kernel 6.17 this can take ten-plus seconds under load. We give
// it up to 30s before declaring a failure.
func waitGone(blockPath string) error {
	charPath := "/dev/ublkc" + strings.TrimPrefix(blockPath, "/dev/ublkb")
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		_, blkErr := os.Stat(blockPath)
		_, chrErr := os.Stat(charPath)
		if errors.Is(blkErr, os.ErrNotExist) && errors.Is(chrErr, os.ErrNotExist) {
			return nil
		}
		time.Sleep(200 * time.Millisecond)
	}
	return fmt.Errorf("device nodes still present 30s after SIGKILL: %s / /dev/ublkc%s "+
		"(kernel ublk_ch_release workqueue stalled — not a ublk-go bug, but worth "+
		"checking `dmesg` and `cat /proc/sys/kernel/workqueue/default_cpumask`)",
		blockPath, strings.TrimPrefix(blockPath, "/dev/ublkb"))
}
