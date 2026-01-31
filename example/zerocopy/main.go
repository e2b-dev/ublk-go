// zerocopy demonstrates a zero-copy in-memory backend using memfd.
//
// Zero-copy mode avoids copying data between kernel and userspace buffers
// by registering the backend's file descriptor with io_uring.
//
// Run with: sudo go run ./example/zerocopy/
package main

import (
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"golang.org/x/sys/unix"

	"github.com/e2b-dev/ublk-go/ublk"
)

// MemfdBackend is a zero-copy backend using memfd.
type MemfdBackend struct {
	file *os.File
	size int64
}

// NewMemfdBackend creates a new memfd-backed storage.
func NewMemfdBackend(size int64) (*MemfdBackend, error) {
	fd, err := unix.MemfdCreate("ublk-zerocopy", unix.MFD_CLOEXEC)
	if err != nil {
		return nil, fmt.Errorf("memfd_create failed: %w", err)
	}

	file := os.NewFile(uintptr(fd), "memfd:ublk-zerocopy")
	if err := file.Truncate(size); err != nil {
		file.Close()
		return nil, fmt.Errorf("truncate failed: %w", err)
	}

	return &MemfdBackend{file: file, size: size}, nil
}

func (b *MemfdBackend) ReadAt(p []byte, off int64) (int, error)  { return b.file.ReadAt(p, off) }
func (b *MemfdBackend) WriteAt(p []byte, off int64) (int, error) { return b.file.WriteAt(p, off) }
func (b *MemfdBackend) FixedFile() (*os.File, error)             { return b.file, nil }
func (b *MemfdBackend) Flush() error                             { return nil }
func (b *MemfdBackend) Close() error                             { return b.file.Close() }

func main() {
	size := int64(64 * 1024 * 1024)
	backend, err := NewMemfdBackend(size)
	if err != nil {
		log.Fatalf("Failed to create backend: %v", err)
	}
	defer backend.Close()

	config := ublk.DefaultConfig()
	config.Size = uint64(size)
	config.ZeroCopy = true

	dev, err := ublk.New(backend, config)
	if err != nil {
		log.Fatalf("Failed to create device: %v", err)
	}
	defer dev.Delete()

	log.Printf("Created zero-copy ublk device: %s", dev.BlockDevicePath())
	log.Println("Test with: sudo dd if=/dev/zero of=" + dev.BlockDevicePath() + " bs=4k count=1000")
	log.Println("Press Ctrl+C to stop.")

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh

	log.Println("Shutting down...")
}
