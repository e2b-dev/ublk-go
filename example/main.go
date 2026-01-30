package main

import (
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"sync"
	"syscall"

	"github.com/ublk-go/ublk/ublk"
)

// MemoryBackend is a thread-safe in-memory storage backend.
// It implements ublk.Backend, ublk.Flusher, and ublk.WriteZeroer.
type MemoryBackend struct {
	mu   sync.RWMutex
	data []byte
	size int64
}

// NewMemoryBackend creates a new in-memory backend with the given size.
func NewMemoryBackend(size int64) *MemoryBackend {
	return &MemoryBackend{
		data: make([]byte, size),
		size: size,
	}
}

// ReadAt implements io.ReaderAt (thread-safe).
func (b *MemoryBackend) ReadAt(p []byte, off int64) (n int, err error) {
	b.mu.RLock()
	defer b.mu.RUnlock()

	if off >= b.size {
		return 0, io.EOF
	}

	end := min(off+int64(len(p)), b.size)

	n = copy(p, b.data[off:end])
	if n < len(p) {
		return n, io.EOF
	}

	return n, nil
}

// WriteAt implements io.WriterAt (thread-safe).
func (b *MemoryBackend) WriteAt(p []byte, off int64) (n int, err error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if off >= b.size {
		return 0, fmt.Errorf("offset %d beyond size %d", off, b.size)
	}

	end := min(off+int64(len(p)), b.size)

	n = copy(b.data[off:end], p)
	return n, nil
}

// Flush implements ublk.Flusher.
// For in-memory storage, this is a no-op.
func (b *MemoryBackend) Flush() error {
	return nil
}

// WriteZeroes implements ublk.WriteZeroer.
func (b *MemoryBackend) WriteZeroes(off, length int64) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	if off >= b.size {
		return fmt.Errorf("offset %d beyond size %d", off, b.size)
	}

	end := min(off+length, b.size)

	// Zero the region
	clear(b.data[off:end])

	return nil
}

func main() {
	// Create a 1GB in-memory backend
	const deviceSize = 1024 * 1024 * 1024
	backend := NewMemoryBackend(deviceSize)

	// Configure the device
	config := ublk.DefaultConfig()
	config.Size = deviceSize
	config.BlockSize = 512
	config.NrHWQueues = 1
	config.QueueDepth = 128

	// Create and start the device
	dev, err := ublk.CreateDevice(backend, config)
	if err != nil {
		log.Fatalf("Failed to create device: %v", err)
	}

	fmt.Printf("Created ublk device: %s (ID: %d)\n", dev.BlockDevicePath(), dev.DeviceID())
	fmt.Println("You can now use this device like:")
	fmt.Printf("  sudo mkfs.ext4 %s\n", dev.BlockDevicePath())
	fmt.Printf("  sudo mount %s /mnt\n", dev.BlockDevicePath())
	fmt.Println("\nPress Ctrl+C to stop...")

	// Handle signals
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)

	<-sigCh
	fmt.Println("\nStopping device...")

	// Clean up
	if err := dev.Delete(); err != nil {
		log.Fatalf("Failed to delete device: %v", err)
	}

	fmt.Println("Device stopped successfully")
}
