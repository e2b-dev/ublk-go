// zerocopy demonstrates a zero-copy in-memory backend using memfd.
//
// Zero-copy mode avoids copying data between kernel and userspace buffers
// by registering the backend's file descriptor with io_uring. The kernel
// reads/writes directly to/from the backend file.
//
// Run with: sudo go run ./example/zerocopy/
package main

import (
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/e2b-dev/ublk-go/ublk"
)

// MemfdBackend is a zero-copy backend using memfd (anonymous memory-backed file).
// It implements FixedFileBackend for zero-copy io_uring operations.
//
// No locking needed: file I/O (pread/pwrite) is atomic at kernel level.
type MemfdBackend struct {
	file *os.File
	size int64
}

// NewMemfdBackend creates a new memfd-backed storage.
func NewMemfdBackend(size int64) (*MemfdBackend, error) {
	// Create anonymous memory-backed file
	fd, err := unix.MemfdCreate("ublk-zerocopy", unix.MFD_CLOEXEC)
	if err != nil {
		return nil, fmt.Errorf("memfd_create failed: %w", err)
	}

	file := os.NewFile(uintptr(fd), "memfd:ublk-zerocopy")

	// Set the size
	if err := file.Truncate(size); err != nil {
		file.Close()
		return nil, fmt.Errorf("truncate failed: %w", err)
	}

	return &MemfdBackend{
		file: file,
		size: size,
	}, nil
}

// ReadAt implements io.ReaderAt (required by Backend).
func (b *MemfdBackend) ReadAt(p []byte, off int64) (int, error) {
	return b.file.ReadAt(p, off)
}

// WriteAt implements io.WriterAt (required by Backend).
func (b *MemfdBackend) WriteAt(p []byte, off int64) (int, error) {
	return b.file.WriteAt(p, off)
}

// FixedFile returns the file for zero-copy io_uring registration.
// This is called by ublk to register the fd with io_uring.
func (b *MemfdBackend) FixedFile() (*os.File, error) {
	return b.file, nil
}

// Flush ensures data is persisted (memfd is always in memory, so this is a no-op).
func (b *MemfdBackend) Flush() error {
	return nil
}

// Close releases resources.
func (b *MemfdBackend) Close() error {
	return b.file.Close()
}

func main() {
	log.SetFlags(log.Ltime | log.Lmicroseconds)

	// Create a 64MB memfd-backed storage
	size := int64(64 * 1024 * 1024)
	backend, err := NewMemfdBackend(size)
	if err != nil {
		log.Fatalf("Failed to create backend: %v", err)
	}
	defer backend.Close()
	log.Printf("Created memfd backend: %d bytes", size)

	// Configure with zero-copy enabled
	config := ublk.DefaultConfig()
	config.Size = uint64(size)
	config.ZeroCopy = true // Enable zero-copy mode

	// Create the device
	dev, err := ublk.New(backend, config)
	if err != nil {
		backend.Close()
		log.Fatalf("Failed to create device: %v", err)
	}
	defer dev.Delete()

	log.Printf("Created zero-copy ublk device: %s", dev.BlockDevicePath())
	log.Printf("Zero-copy enabled: %v", dev.HasZeroCopy())

	// Wait for signal
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	// Print stats periodically
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	log.Println("Device ready. Press Ctrl+C to stop.")
	log.Println("Test with: sudo dd if=/dev/zero of=" + dev.BlockDevicePath() + " bs=4k count=1000")

	for {
		select {
		case <-sigCh:
			log.Println("Shutting down...")
			return
		case <-ticker.C:
			stats := dev.Stats().Snapshot()
			log.Printf("Stats: reads=%d writes=%d flushes=%d",
				stats.Reads, stats.Writes, stats.Flushes)
		}
	}
}
