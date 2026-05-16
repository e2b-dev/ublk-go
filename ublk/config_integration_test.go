//go:build integration

package ublk

import (
	"bytes"
	"crypto/rand"
	"os"
	"testing"
	"unsafe"

	"golang.org/x/sys/unix"
)

// TestNewWithOptions verifies a non-default block size reaches the kernel
// (BLKBSZGET) and survives a full mkfs.ext4 + mount + write + sync + umount
// + remount + read round-trip. A wrong LogicalBSShift / PhysicalBSShift /
// IOOptShift / IOMinShift surfaces as mkfs hanging, mount rejecting writes,
// or readback mismatch. QueueDepth and MaxIOSize are also set to non-default
// values to exercise the rest of the option plumbing under the same workload.
func TestNewWithOptions(t *testing.T) {
	const size = 64 * 1024 * 1024
	backend := newMemBackend(size)
	dev, err := New(backend, size, WithBlockSize(4096), WithQueueDepth(32), WithMaxIOSize(64*1024))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = dev.Close() })
	path := dev.Path()

	fd := openBlkDev(t, path, unix.O_RDWR)
	var bs uint32
	if _, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), 0x80081270 /* BLKBSZGET */, uintptr(unsafe.Pointer(&bs))); errno != 0 {
		t.Fatalf("BLKBSZGET: %v", errno)
	}
	_ = unix.Close(fd)
	if bs != 4096 {
		t.Fatalf("BLKBSZGET = %d, want 4096", bs)
	}

	mountpoint, err := os.MkdirTemp("", "ublk-4k-*")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() {
		_ = runShell(t, "umount", mountpoint)
		_ = os.Remove(mountpoint)
	})

	if err := runShell(t, "mkfs.ext4", "-q", "-F", "-b", "4096", path); err != nil {
		t.Fatalf("mkfs.ext4 -b 4096: %v", err)
	}
	if err := runShell(t, "mount", path, mountpoint); err != nil {
		t.Fatalf("mount: %v", err)
	}

	file := mountpoint + "/pattern.bin"
	pattern := make([]byte, 1<<20)
	if _, err := rand.Read(pattern); err != nil {
		t.Fatalf("rand.Read: %v", err)
	}
	if err := os.WriteFile(file, pattern, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := runShell(t, "sync", "-f", file); err != nil {
		t.Fatalf("sync -f: %v", err)
	}
	if err := runShell(t, "umount", mountpoint); err != nil {
		t.Fatalf("umount: %v", err)
	}
	if err := runShell(t, "mount", path, mountpoint); err != nil {
		t.Fatalf("remount: %v", err)
	}
	got, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !bytes.Equal(got, pattern) {
		t.Fatalf("roundtrip mismatch at byte %d", firstDiff(got, pattern))
	}
}
