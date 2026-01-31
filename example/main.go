package main

import (
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/e2b-dev/ublk-go/ublk"
)

// MemoryBackend is a thread-safe in-memory storage backend.
// It implements all ublk backend interfaces: Backend, Flusher, WriteZeroer, Discarder.
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
	clear(b.data[off:end])

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

	dev, err := ublk.CreateDevice(backend, config)
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

	// Print stats every 10 seconds
	done := make(chan struct{})
	go func() {
		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				printStats(dev)
			case <-done:
				return
			}
		}
	}()

	<-sigCh
	close(done)
	fmt.Println("\nStopping device...")

	printStats(dev)

	if err := dev.Delete(); err != nil {
		log.Fatalf("Failed to delete device: %v", err)
	}

	fmt.Println("Device stopped successfully")
}

func printStats(dev *ublk.Device) {
	stats := dev.Stats().Snapshot()
	fmt.Println("\n=== Device Stats ===")
	fmt.Printf("Reads:        %d (%s)\n", stats.Reads, formatBytes(stats.BytesRead))
	fmt.Printf("Writes:       %d (%s)\n", stats.Writes, formatBytes(stats.BytesWritten))
	fmt.Printf("Flushes:      %d\n", stats.Flushes)
	fmt.Printf("Discards:     %d\n", stats.Discards)
	fmt.Printf("Write Zeroes: %d\n", stats.WriteZeroes)
	if stats.ReadErrors > 0 || stats.WriteErrors > 0 || stats.OtherErrors > 0 {
		fmt.Printf("Errors: read=%d write=%d other=%d\n",
			stats.ReadErrors, stats.WriteErrors, stats.OtherErrors)
	}
	fmt.Println("====================")
}

func formatBytes(bytes uint64) string {
	const (
		KB = 1024
		MB = KB * 1024
		GB = MB * 1024
	)
	switch {
	case bytes >= GB:
		return fmt.Sprintf("%.2f GB", float64(bytes)/GB)
	case bytes >= MB:
		return fmt.Sprintf("%.2f MB", float64(bytes)/MB)
	case bytes >= KB:
		return fmt.Sprintf("%.2f KB", float64(bytes)/KB)
	default:
		return fmt.Sprintf("%d B", bytes)
	}
}
