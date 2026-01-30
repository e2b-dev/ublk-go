package main

import (
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/ublk-go/ublk/ublk"
)

// SimpleMemoryBackend is a simple in-memory backend
type SimpleMemoryBackend struct {
	data []byte
	size int64
}

func NewSimpleMemoryBackend(size int64) *SimpleMemoryBackend {
	return &SimpleMemoryBackend{
		data: make([]byte, size),
		size: size,
	}
}

func (b *SimpleMemoryBackend) ReadAt(p []byte, off int64) (n int, err error) {
	if off >= b.size {
		return 0, fmt.Errorf("offset %d beyond size %d", off, b.size)
	}

	end := off + int64(len(p))
	if end > b.size {
		end = b.size
	}

	n = copy(p, b.data[off:end])
	if n < len(p) {
		return n, fmt.Errorf("short read: got %d, wanted %d", n, len(p))
	}

	return n, nil
}

func (b *SimpleMemoryBackend) WriteAt(p []byte, off int64) (n int, err error) {
	if off >= b.size {
		return 0, fmt.Errorf("offset %d beyond size %d", off, b.size)
	}

	end := off + int64(len(p))
	if end > b.size {
		// Extend if needed (simplified)
		if end > int64(cap(b.data)) {
			newData := make([]byte, end)
			copy(newData, b.data)
			b.data = newData
		}
		b.size = end
	}

	n = copy(b.data[off:], p)
	return n, nil
}

func main() {
	// Create a simple in-memory backend (1GB)
	backend := NewSimpleMemoryBackend(1024 * 1024 * 1024)

	// Configure the device
	config := ublk.DefaultConfig()
	config.Size = 1024 * 1024 * 1024 // 1GB
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
	err = dev.Delete()
	if err != nil {
		log.Fatalf("Failed to delete device: %v", err)
	}

	fmt.Println("Device stopped successfully")
}
