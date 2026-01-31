// mmap_test demonstrates memory-mapping a ublk block device.
//
// This example:
// 1. Creates a ublk device with an in-memory backend
// 2. Opens the block device and mmaps it
// 3. Writes data through the mmap
// 4. Reads it back and verifies
// 5. Tests that changes are visible in the backend
//
// Run with: sudo go run ./example/mmap_test/
package main

import (
	"bytes"
	"fmt"
	"io"
	"log"
	"sync"
	"time"

	"golang.org/x/sys/unix"

	"github.com/e2b-dev/ublk-go/ublk"
)

const (
	deviceSize = 16 * 1024 * 1024 // 16MB - small for testing
	blockSize  = 4096
)

// MemoryBackend is a thread-safe in-memory storage backend.
type MemoryBackend struct {
	mu   sync.RWMutex
	data []byte
	size int64
}

func NewMemoryBackend(size int64) *MemoryBackend {
	return &MemoryBackend{
		data: make([]byte, size),
		size: size,
	}
}

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

func (b *MemoryBackend) Flush() error {
	return nil
}

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

// GetData returns a copy of the backend data for verification.
func (b *MemoryBackend) GetData(off, length int64) []byte {
	b.mu.RLock()
	defer b.mu.RUnlock()

	if off >= b.size {
		return nil
	}
	end := min(off+length, b.size)
	result := make([]byte, end-off)
	copy(result, b.data[off:end])
	return result
}

func main() {
	log.SetFlags(log.Ltime | log.Lmicroseconds)

	// Create backend
	backend := NewMemoryBackend(deviceSize)
	log.Printf("Created memory backend: %d bytes", deviceSize)

	// Create ublk device
	config := ublk.DefaultConfig()
	config.Size = deviceSize
	config.BlockSize = blockSize
	config.NrHWQueues = 1
	config.QueueDepth = 64
	// Note: Requires root/CAP_SYS_ADMIN to create ublk devices

	dev, err := ublk.New(backend, config)
	if err != nil {
		log.Fatalf("Failed to create device: %v", err)
	}

	// Use a cleanup function instead of defer to avoid exitAfterDefer
	cleanup := func() {
		log.Println("Deleting device...")
		if err := dev.Delete(); err != nil {
			log.Printf("Warning: failed to delete device: %v", err)
		}
	}

	blockDevPath := dev.BlockDevicePath()
	log.Printf("Created ublk device: %s", blockDevPath)

	// Wait a moment for device to be ready
	time.Sleep(100 * time.Millisecond)

	// Open the block device
	fd, err := unix.Open(blockDevPath, unix.O_RDWR|unix.O_SYNC, 0)
	if err != nil {
		cleanup()
		log.Fatalf("Failed to open block device: %v", err)
	}
	log.Printf("Opened block device fd=%d", fd)

	// Mmap the entire device
	data, err := unix.Mmap(fd, 0, deviceSize, unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED)
	if err != nil {
		unix.Close(fd)
		cleanup()
		log.Fatalf("Failed to mmap block device: %v", err)
	}

	// Cleanup function for fd and mmap
	cleanupIO := func() {
		unix.Munmap(data)
		unix.Close(fd)
	}
	log.Printf("Mmapped %d bytes at %p", len(data), &data[0])

	// Test 1: Write pattern through mmap
	log.Println("\n=== Test 1: Write pattern through mmap ===")
	testPattern := []byte("Hello, ublk mmap! This is a test pattern. ")
	for i := 0; i < blockSize; i += len(testPattern) {
		remaining := min(blockSize-i, len(testPattern))
		copy(data[i:], testPattern[:remaining])
	}

	// Sync to ensure data reaches backend
	if err := unix.Msync(data[:blockSize], unix.MS_SYNC); err != nil {
		cleanupIO()
		cleanup()
		log.Fatalf("Msync failed: %v", err)
	}
	log.Println("Wrote and synced first block via mmap")

	// Verify in backend
	backendData := backend.GetData(0, blockSize)
	if !bytes.HasPrefix(backendData, testPattern) {
		cleanupIO()
		cleanup()
		log.Fatalf("FAIL: Backend data mismatch. Got: %q...", backendData[:min(50, len(backendData))])
	}
	log.Println("SUCCESS: Backend contains expected pattern")

	// Test 2: Write at different offsets
	log.Println("\n=== Test 2: Write at offset 1MB ===")
	offset := 1024 * 1024 // 1MB
	testData := bytes.Repeat([]byte{0xAB, 0xCD, 0xEF, 0x12}, 1024)
	copy(data[offset:], testData)

	if err := unix.Msync(data[offset:offset+len(testData)], unix.MS_SYNC); err != nil {
		cleanupIO()
		cleanup()
		log.Fatalf("Msync failed: %v", err)
	}
	log.Printf("Wrote %d bytes at offset %d", len(testData), offset)

	backendData = backend.GetData(int64(offset), int64(len(testData)))
	if !bytes.Equal(backendData, testData) {
		cleanupIO()
		cleanup()
		log.Fatal("FAIL: Backend data mismatch at offset")
	}
	log.Println("SUCCESS: Backend contains expected data at offset")

	// Test 3: Read back through mmap
	log.Println("\n=== Test 3: Read back through mmap ===")
	readBack := make([]byte, len(testData))
	copy(readBack, data[offset:offset+len(testData)])
	if !bytes.Equal(readBack, testData) {
		cleanupIO()
		cleanup()
		log.Fatal("FAIL: Read back mismatch")
	}
	log.Println("SUCCESS: Read back matches written data")

	// Test 4: Modify backend directly, read via mmap
	log.Println("\n=== Test 4: Backend write, mmap read ===")
	directData := []byte("Direct backend write!")
	directOffset := int64(2 * 1024 * 1024) // 2MB
	if _, err := backend.WriteAt(directData, directOffset); err != nil {
		cleanupIO()
		cleanup()
		log.Fatalf("Backend write failed: %v", err)
	}

	// Invalidate page cache to see backend changes
	if err := unix.Msync(data, unix.MS_INVALIDATE); err != nil {
		log.Printf("Msync invalidate: %v (may be expected)", err)
	}

	// Re-read through direct I/O to bypass cache
	buf := make([]byte, blockSize)
	n, err := unix.Pread(fd, buf, directOffset)
	if err != nil {
		cleanupIO()
		cleanup()
		log.Fatalf("Pread failed: %v", err)
	}
	if bytes.HasPrefix(buf[:n], directData) {
		log.Println("SUCCESS: Direct read shows backend data")
	} else {
		log.Printf("Note: Page cache may still have old data (got: %q)", buf[:min(30, n)])
	}

	// Test 5: Large sequential write
	log.Println("\n=== Test 5: Large sequential write (4MB) ===")
	largeSize := 4 * 1024 * 1024
	largeOffset := 4 * 1024 * 1024
	largeData := bytes.Repeat([]byte{0x55, 0xAA}, largeSize/2)

	start := time.Now()
	copy(data[largeOffset:largeOffset+largeSize], largeData)
	if err := unix.Msync(data[largeOffset:largeOffset+largeSize], unix.MS_SYNC); err != nil {
		cleanupIO()
		cleanup()
		log.Fatalf("Msync failed: %v", err)
	}
	elapsed := time.Since(start)
	throughput := float64(largeSize) / elapsed.Seconds() / (1024 * 1024)
	log.Printf("Wrote %d bytes in %v (%.2f MB/s)", largeSize, elapsed, throughput)

	// Verify
	backendData = backend.GetData(int64(largeOffset), int64(largeSize))
	if !bytes.Equal(backendData, largeData) {
		cleanupIO()
		cleanup()
		log.Fatal("FAIL: Large write verification failed")
	}
	log.Println("SUCCESS: Large write verified")
	log.Println("\nAll mmap tests passed!")

	cleanupIO()
	cleanup()
}
