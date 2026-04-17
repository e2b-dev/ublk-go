// End-to-end demo: create a ublk device, format it with ext4, mount it,
// write and read a file, then enter an interactive phase where every
// backend I/O is logged so you can poke at the mount from another
// terminal and watch the page cache flush down to Backend.ReadAt /
// Backend.WriteAt.
//
// Requires root and the ublk_drv module:
//
//	sudo modprobe ublk_drv
//	sudo go run ./example/fsdemo
//
// Then from another terminal:
//
//	echo hi | sudo tee /tmp/ublk-fsdemo-*/poke.txt
//	sync
//
// (The `sync` is what actually forces the write through to the backend;
// without it the Linux page cache buffers the write for up to ~30s.)
package main

import (
	"bytes"
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"sync"
	"sync/atomic"
	"syscall"

	"github.com/e2b-dev/ublk-go/ublk"
)

const devSize = 64 * 1024 * 1024 // 64 MiB

// loggingBackend is an in-memory Backend that counts I/O and, when
// verbose is true, prints every call.
type loggingBackend struct {
	mu         sync.RWMutex
	data       []byte
	reads      atomic.Int64
	writes     atomic.Int64
	readBytes  atomic.Int64
	writeBytes atomic.Int64
	verbose    atomic.Bool
}

func (b *loggingBackend) ReadAt(p []byte, off int64) (int, error) {
	b.reads.Add(1)
	b.readBytes.Add(int64(len(p)))
	if b.verbose.Load() {
		log.Printf("  R off=%-10d len=%d", off, len(p))
	}
	b.mu.RLock()
	defer b.mu.RUnlock()
	return copy(p, b.data[off:off+int64(len(p))]), nil
}

func (b *loggingBackend) WriteAt(p []byte, off int64) (int, error) {
	b.writes.Add(1)
	b.writeBytes.Add(int64(len(p)))
	if b.verbose.Load() {
		log.Printf("  W off=%-10d len=%d", off, len(p))
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return copy(b.data[off:off+int64(len(p))], p), nil
}

func main() {
	log.SetFlags(log.Ltime | log.Lmicroseconds)
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	backend := &loggingBackend{data: make([]byte, devSize)}

	dev, err := ublk.New(backend, devSize)
	if err != nil {
		return fmt.Errorf("ublk.New: %w", err)
	}
	defer func() {
		log.Printf("closing ublk device")
		if err := dev.Close(); err != nil {
			log.Printf("close: %v", err)
		}
	}()

	path := dev.BlockDevicePath()
	log.Printf("created %s (%d MiB)", path, devSize/1024/1024)
	stats(backend, "idle")

	// Heavy, noisy operations — keep verbose off so mkfs doesn't print
	// thousands of lines.
	if err := shell("mkfs.ext4", "-q", "-F", path); err != nil {
		return fmt.Errorf("mkfs.ext4: %w", err)
	}
	stats(backend, "after mkfs.ext4")

	mountpoint, err := os.MkdirTemp("", "ublk-fsdemo-*")
	if err != nil {
		return err
	}
	defer os.Remove(mountpoint)

	if err := shell("mount", path, mountpoint); err != nil {
		return fmt.Errorf("mount: %w", err)
	}
	defer func() {
		if err := shell("umount", mountpoint); err != nil {
			log.Printf("umount: %v", err)
		}
	}()
	log.Printf("mounted at %s", mountpoint)
	stats(backend, "after mount")

	// Scripted write + readback so the user sees expected behavior
	// before entering the interactive phase.
	content := bytes.Repeat([]byte("hello from ublk-backed ext4\n"), 100)
	fpath := mountpoint + "/greeting.txt"
	if err := os.WriteFile(fpath, content, 0o644); err != nil {
		return fmt.Errorf("write: %w", err)
	}
	log.Printf("wrote %s (%d bytes)", fpath, len(content))

	if err := shell("sync", "-f", mountpoint); err != nil {
		return fmt.Errorf("sync: %w", err)
	}
	stats(backend, "after write + sync")

	if err := os.WriteFile("/proc/sys/vm/drop_caches", []byte("3"), 0); err != nil {
		log.Printf("drop_caches: %v (read stats may be misleading)", err)
	}
	got, err := os.ReadFile(fpath)
	if err != nil {
		return fmt.Errorf("read: %w", err)
	}
	if !bytes.Equal(got, content) {
		return fmt.Errorf("read-back mismatch: want %d bytes, got %d", len(content), len(got))
	}
	log.Printf("read %d bytes back; content matches", len(got))
	stats(backend, "after drop_caches + read")

	// Interactive phase: turn on per-call tracing and wait for Ctrl+C
	// so the user can drive I/O from another terminal.
	backend.verbose.Store(true)
	fmt.Println()
	log.Printf("interactive: every Backend.ReadAt / WriteAt is now logged")
	log.Printf("try (from another terminal, as root):")
	log.Printf("    echo hi | sudo tee %s/poke.txt && sync -f %s", mountpoint, mountpoint)
	log.Printf("Ctrl+C to unmount and exit")

	sigc := make(chan os.Signal, 1)
	signal.Notify(sigc, syscall.SIGINT, syscall.SIGTERM)
	<-sigc
	fmt.Println()
	backend.verbose.Store(false)
	stats(backend, "final")
	return nil
}

// shell runs a command, echoing stdout/stderr to the terminal.
func shell(name string, args ...string) error {
	fmt.Printf(">>> %s %v\n", name, args)
	cmd := exec.Command(name, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func stats(b *loggingBackend, phase string) {
	log.Printf("[%-22s] reads=%-4d (%4d KiB)  writes=%-4d (%4d KiB)",
		phase,
		b.reads.Load(), b.readBytes.Load()/1024,
		b.writes.Load(), b.writeBytes.Load()/1024)
}
