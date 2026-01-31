package main

import (
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"sync"
	"syscall"

	"github.com/e2b-dev/ublk-go/ublk"
)

// MemoryBackend is a thread-safe in-memory storage backend.
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

// ReadAt implements io.ReaderAt.
func (b *MemoryBackend) ReadAt(p []byte, off int64) (n int, err error) {
	b.mu.RLock()
	defer b.mu.RUnlock()

	if off >= b.size {
		return 0, io.EOF
	}

	n = copy(p, b.data[off:min(off+int64(len(p)), b.size)])
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

// WriteAt implements io.WriterAt.
func (b *MemoryBackend) WriteAt(p []byte, off int64) (n int, err error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if off >= b.size {
		return 0, fmt.Errorf("offset %d beyond size %d", off, b.size)
	}

	n = copy(b.data[off:min(off+int64(len(p)), b.size)], p)
	return n, nil
}

// Flush implements ublk.Flusher.
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

	clear(b.data[off:min(off+length, b.size)])
	return nil
}

// Discard implements ublk.Discarder.
func (b *MemoryBackend) Discard(off, length int64) error {
	return b.WriteZeroes(off, length)
}

func main() {
	const deviceSize = 1024 * 1024 * 1024
	backend := NewMemoryBackend(deviceSize)

	config := ublk.DefaultConfig()
	config.Size = deviceSize
	config.BlockSize = 512
	config.NrHWQueues = 1
	config.QueueDepth = 128

	dev, err := ublk.New(backend, config)
	if err != nil {
		log.Fatalf("Failed to create device: %v", err)
	}

	fmt.Printf("Created ublk device: %s (ID: %d)\n", dev.BlockDevicePath(), dev.DeviceID())
	fmt.Println("You can now use this device like:")
	fmt.Printf("  sudo mkfs.ext4 %s\n", dev.BlockDevicePath())
	fmt.Printf("  sudo mount %s /mnt\n", dev.BlockDevicePath())
	fmt.Println("\nPress Ctrl+C to stop...")

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	<-sigCh

	fmt.Println("\nStopping device...")
	if err := dev.Delete(); err != nil {
		log.Fatalf("Failed to delete device: %v", err)
	}
	fmt.Println("Device stopped successfully")
}
