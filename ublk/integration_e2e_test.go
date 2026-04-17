//go:build integration

package ublk

import (
	"bytes"
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

// TestEndToEnd is the former `example/probe` harness, now a table-driven
// Go test. Each step that was a probe.step() becomes a t.Run subtest so
// failures name the exact step and output is captured in test logs. The
// whole test aborts on first subtest failure because later steps depend
// on earlier ones (you can't mount a device that failed to mkfs).
func TestEndToEnd(t *testing.T) {
	const (
		devSize = 128 * 1024 * 1024
		blkSize = 4096
	)

	be := newMemBackend(devSize)
	dev, err := New(be, devSize)
	if err != nil {
		t.Fatalf("ublk.New: %v", err)
	}
	devPath := dev.Path()
	t.Cleanup(func() { _ = dev.Close() })

	var mountpoint string
	t.Cleanup(func() {
		if mountpoint != "" {
			_ = exec.Command("umount", mountpoint).Run()
			_ = os.Remove(mountpoint)
		}
	})

	t.Run("device node is a block device", func(t *testing.T) {
		fi, err := os.Stat(devPath)
		if err != nil {
			t.Fatalf("stat %s: %v", devPath, err)
		}
		if fi.Mode()&os.ModeDevice == 0 {
			t.Fatalf("%s is not a block device (mode=%v)", devPath, fi.Mode())
		}
	})

	t.Run("BLKGETSIZE64 matches ublk.New size", func(t *testing.T) {
		fd, err := unix.Open(devPath, unix.O_RDONLY, 0)
		if err != nil {
			t.Fatal(err)
		}
		defer unix.Close(fd)

		var size uint64
		const BLKGETSIZE64 = 0x80081272
		if _, _, errno := unix.Syscall(
			unix.SYS_IOCTL, uintptr(fd), BLKGETSIZE64,
			uintptr(unsafe.Pointer(&size)),
		); errno != 0 {
			t.Fatalf("BLKGETSIZE64: %v", errno)
		}
		if size != devSize {
			t.Fatalf("kernel size = %d, want %d", size, devSize)
		}
	})

	t.Run("O_DIRECT zero-read of fresh device reaches backend", func(t *testing.T) {
		before := be.reads.Load()
		buf, err := directRead(devPath, 0, blkSize)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(buf, make([]byte, blkSize)) {
			t.Fatal("first block of fresh device is not zero")
		}
		if be.reads.Load() == before {
			t.Fatal("Backend.ReadAt was not called during O_DIRECT read")
		}
	})

	t.Run("O_DIRECT roundtrip: backend holds bytes at same offset", func(t *testing.T) {
		pattern := make([]byte, blkSize)
		if _, err := rand.Read(pattern); err != nil {
			t.Fatal(err)
		}
		const off int64 = 8 * blkSize

		if err := directWrite(devPath, off, pattern); err != nil {
			t.Fatalf("direct write: %v", err)
		}
		got, err := directRead(devPath, off, blkSize)
		if err != nil {
			t.Fatalf("direct read: %v", err)
		}
		if !bytes.Equal(got, pattern) {
			t.Fatal("direct read did not match what we wrote")
		}
		stored := be.snapshot()[off : off+int64(blkSize)]
		if !bytes.Equal(stored, pattern) {
			t.Fatal("backend storage does not match what the kernel wrote at the same offset")
		}
	})

	t.Run("mkfs.ext4", func(t *testing.T) {
		if err := runShell(t, "mkfs.ext4", "-q", "-F", devPath); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("mount", func(t *testing.T) {
		mp, err := os.MkdirTemp("", "ublk-e2e-*")
		if err != nil {
			t.Fatal(err)
		}
		mountpoint = mp
		if err := runShell(t, "mount", devPath, mountpoint); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("write+syncfs visible at backend", func(t *testing.T) {
		before := be.writes.Load()
		content := bytes.Repeat([]byte("probe write: ublk-go e2e test\n"), 256)
		f := mountpoint + "/probe.txt"
		if err := os.WriteFile(f, content, 0o644); err != nil {
			t.Fatalf("write %s: %v", f, err)
		}
		if err := runShell(t, "sync", "-f", mountpoint); err != nil {
			t.Fatalf("syncfs: %v", err)
		}
		if be.writes.Load() == before {
			t.Fatal("syncfs did not cause any Backend.WriteAt calls")
		}
	})

	t.Run("fsync alone visible at backend", func(t *testing.T) {
		before := be.writes.Load()
		f, err := os.Create(mountpoint + "/fsync.bin")
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		data := bytes.Repeat([]byte("F"), 32*1024)
		if _, err := f.Write(data); err != nil {
			t.Fatal(err)
		}
		if err := f.Sync(); err != nil {
			t.Fatalf("fsync: %v", err)
		}
		if be.writes.Load() == before {
			t.Fatal("fsync did not cause any Backend.WriteAt calls")
		}
	})

	t.Run("flush fs before drop_caches", func(t *testing.T) {
		if err := runShell(t, "sync", "-f", mountpoint); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("drop_caches fast once fs is clean", func(t *testing.T) {
		before := be.writes.Load()
		if err := os.WriteFile("/proc/sys/vm/drop_caches", []byte("3"), 0); err != nil {
			t.Fatalf("drop_caches: %v", err)
		}
		if dw := be.writes.Load() - before; dw > 16 {
			t.Fatalf("drop_caches triggered %d backend writes; fs was not clean going in", dw)
		}
	})

	t.Run("readback after drop reaches backend", func(t *testing.T) {
		before := be.reads.Load()
		want, err := os.ReadFile(mountpoint + "/probe.txt")
		if err != nil {
			t.Fatalf("read probe.txt: %v", err)
		}
		if !bytes.HasPrefix(want, []byte("probe write:")) {
			t.Fatal("probe.txt did not round-trip correctly")
		}
		if be.reads.Load() == before {
			t.Fatal("readback served entirely from cache; Backend.ReadAt not invoked")
		}
	})

	t.Run("filesystem bytes visible in raw backend storage", func(t *testing.T) {
		snap := be.snapshot()
		pattern := []byte("probe write: ublk-go e2e test\n")
		for off := 0; off+len(pattern) <= len(snap); off += blkSize {
			if bytes.Contains(snap[off:off+blkSize+len(pattern)], pattern) {
				t.Logf("found probe.txt contents at backend offset %d", off)
				return
			}
		}
		t.Fatal("probe.txt pattern not found anywhere in backend storage")
	})

	t.Run("concurrent writers all reach backend", func(t *testing.T) {
		const (
			workers = 8
			files   = 16
			size    = 64 * 1024
		)
		before := be.writes.Load()
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
					name := fmt.Sprintf("%s/w%d-f%d.bin", mountpoint, id, i)
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
				t.Fatal(err)
			}
		}
		if err := runShell(t, "sync", "-f", mountpoint); err != nil {
			t.Fatal(err)
		}
		if be.writes.Load() == before {
			t.Fatal("concurrent writers produced no Backend.WriteAt calls")
		}
	})

	t.Run("remount preserves probe.txt (journal replay)", func(t *testing.T) {
		if err := runShell(t, "umount", mountpoint); err != nil {
			t.Fatal(err)
		}
		if err := runShell(t, "mount", devPath, mountpoint); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(mountpoint + "/probe.txt"); err != nil {
			t.Fatalf("probe.txt missing after remount: %v", err)
		}
	})

	t.Run("umount", func(t *testing.T) {
		if err := runShell(t, "umount", mountpoint); err != nil {
			t.Fatal(err)
		}
		mountpoint = ""
	})

	t.Run("Close", func(t *testing.T) {
		if err := dev.Close(); err != nil {
			t.Fatalf("dev.Close: %v", err)
		}
	})

	t.Run("both device nodes are gone after Close", func(t *testing.T) {
		charPath := "/dev/ublkc" + strings.TrimPrefix(devPath, "/dev/ublkb")
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			_, blkErr := os.Stat(devPath)
			_, chrErr := os.Stat(charPath)
			if errors.Is(blkErr, os.ErrNotExist) && errors.Is(chrErr, os.ErrNotExist) {
				return
			}
			time.Sleep(50 * time.Millisecond)
		}
		t.Fatalf("device nodes still present 2s after Close: %s, %s", devPath, charPath)
	})
}

// --- helpers used by integration_e2e_test.go ---

func alignedBuf(n int) []byte {
	const align = 4096
	raw := make([]byte, n+align)
	addr := uintptr(unsafe.Pointer(&raw[0]))
	off := int(uintptr(align) - addr%uintptr(align))
	if off == align {
		off = 0
	}
	return raw[off : off+n]
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

// runShell is a test-aware shell invoker that attributes output to the
// calling test via t.Log when -v is passed.
func runShell(t *testing.T, name string, args ...string) error {
	t.Helper()
	t.Logf(">>> %s %v", name, args)
	cmd := exec.Command(name, args...)
	out, err := cmd.CombinedOutput()
	if len(out) > 0 {
		t.Logf("    %s", bytes.TrimRight(out, "\n"))
	}
	return err
}
