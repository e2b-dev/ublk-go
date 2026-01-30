//go:build integration

// Integration tests require:
// - Root privileges (sudo)
// - ublk kernel module loaded (modprobe ublk_drv)
//
// Run with: sudo go test -tags=integration -v ./ublk -run=Integration

package ublk

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"sync"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

// integrationBackend is a thread-safe in-memory backend for integration tests.
type integrationBackend struct {
	mu   sync.RWMutex
	data []byte
	size int64
}

func newIntegrationBackend(size int64) *integrationBackend {
	return &integrationBackend{
		data: make([]byte, size),
		size: size,
	}
}

func (b *integrationBackend) ReadAt(p []byte, off int64) (n int, err error) {
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

func (b *integrationBackend) WriteAt(p []byte, off int64) (n int, err error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if off >= b.size {
		return 0, fmt.Errorf("offset %d beyond size %d", off, b.size)
	}
	end := min(off+int64(len(p)), b.size)
	n = copy(b.data[off:end], p)
	return n, nil
}

func (b *integrationBackend) Flush() error {
	return nil
}

func (b *integrationBackend) WriteZeroes(off, length int64) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	if off >= b.size {
		return fmt.Errorf("offset %d beyond size %d", off, b.size)
	}
	end := min(off+length, b.size)
	clear(b.data[off:end])
	return nil
}

func (b *integrationBackend) getData(off, length int64) []byte {
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

// TestIntegrationDeviceLifecycle tests full device lifecycle.
func TestIntegrationDeviceLifecycle(t *testing.T) {
	if os.Getuid() != 0 {
		t.Skip("Integration tests require root")
	}

	const deviceSize = 16 * 1024 * 1024 // 16MB

	backend := newIntegrationBackend(deviceSize)

	config := DefaultConfig()
	config.Size = deviceSize
	config.BlockSize = 4096
	config.NrHWQueues = 1
	config.QueueDepth = 64

	dev, err := CreateDevice(backend, config)
	if err != nil {
		t.Fatalf("CreateDevice failed: %v", err)
	}
	defer dev.Delete()

	t.Logf("Created device: %s (ID: %d)", dev.BlockDevicePath(), dev.DeviceID())

	// Verify device exists
	if _, err := os.Stat(dev.BlockDevicePath()); err != nil {
		t.Fatalf("Block device does not exist: %v", err)
	}

	// Wait for device to be ready
	time.Sleep(100 * time.Millisecond)

	t.Log("Device lifecycle test passed")
}

// TestIntegrationMmapReadWrite tests mmapping the block device.
func TestIntegrationMmapReadWrite(t *testing.T) {
	if os.Getuid() != 0 {
		t.Skip("Integration tests require root")
	}

	const deviceSize = 16 * 1024 * 1024 // 16MB
	const blockSize = 4096

	backend := newIntegrationBackend(deviceSize)

	config := DefaultConfig()
	config.Size = deviceSize
	config.BlockSize = blockSize
	config.NrHWQueues = 1
	config.QueueDepth = 64

	dev, err := CreateDevice(backend, config)
	if err != nil {
		t.Fatalf("CreateDevice failed: %v", err)
	}
	defer dev.Delete()

	blockDevPath := dev.BlockDevicePath()
	t.Logf("Created device: %s", blockDevPath)

	time.Sleep(100 * time.Millisecond)

	// Open block device
	fd, err := unix.Open(blockDevPath, unix.O_RDWR|unix.O_SYNC, 0)
	if err != nil {
		t.Fatalf("Failed to open block device: %v", err)
	}
	defer unix.Close(fd)

	// Mmap the device
	data, err := unix.Mmap(fd, 0, deviceSize, unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED)
	if err != nil {
		t.Fatalf("Failed to mmap: %v", err)
	}
	defer unix.Munmap(data)

	t.Run("WriteAndSync", func(t *testing.T) {
		testPattern := bytes.Repeat([]byte("UBLK"), 1024)
		copy(data[0:], testPattern)

		if err := unix.Msync(data[:len(testPattern)], unix.MS_SYNC); err != nil {
			t.Fatalf("Msync failed: %v", err)
		}

		backendData := backend.getData(0, int64(len(testPattern)))
		if !bytes.Equal(backendData, testPattern) {
			t.Fatalf("Backend data mismatch")
		}
		t.Log("Write and sync verified")
	})

	t.Run("WriteAtOffset", func(t *testing.T) {
		offset := 1024 * 1024 // 1MB
		testData := bytes.Repeat([]byte{0xDE, 0xAD, 0xBE, 0xEF}, 1024)
		copy(data[offset:], testData)

		if err := unix.Msync(data[offset:offset+len(testData)], unix.MS_SYNC); err != nil {
			t.Fatalf("Msync failed: %v", err)
		}

		backendData := backend.getData(int64(offset), int64(len(testData)))
		if !bytes.Equal(backendData, testData) {
			t.Fatalf("Backend data mismatch at offset")
		}
		t.Log("Write at offset verified")
	})

	t.Run("ReadBack", func(t *testing.T) {
		offset := 1024 * 1024
		testData := bytes.Repeat([]byte{0xDE, 0xAD, 0xBE, 0xEF}, 1024)

		readBack := make([]byte, len(testData))
		copy(readBack, data[offset:offset+len(testData)])

		if !bytes.Equal(readBack, testData) {
			t.Fatalf("Read back mismatch")
		}
		t.Log("Read back verified")
	})

	t.Run("LargeWrite", func(t *testing.T) {
		largeSize := 4 * 1024 * 1024
		largeOffset := 4 * 1024 * 1024
		largeData := bytes.Repeat([]byte{0x55, 0xAA}, largeSize/2)

		start := time.Now()
		copy(data[largeOffset:largeOffset+largeSize], largeData)
		if err := unix.Msync(data[largeOffset:largeOffset+largeSize], unix.MS_SYNC); err != nil {
			t.Fatalf("Msync failed: %v", err)
		}
		elapsed := time.Since(start)

		backendData := backend.getData(int64(largeOffset), int64(largeSize))
		if !bytes.Equal(backendData, largeData) {
			t.Fatalf("Large write verification failed")
		}

		throughput := float64(largeSize) / elapsed.Seconds() / (1024 * 1024)
		t.Logf("Large write: %d bytes in %v (%.2f MB/s)", largeSize, elapsed, throughput)
	})

	// Print stats
	stats := dev.Stats().Snapshot()
	t.Logf("Stats: reads=%d writes=%d errors=%d",
		stats.Reads, stats.Writes, stats.ReadErrors+stats.WriteErrors+stats.OtherErrors)
}

// TestIntegrationDirectIO tests direct I/O (bypassing page cache).
func TestIntegrationDirectIO(t *testing.T) {
	if os.Getuid() != 0 {
		t.Skip("Integration tests require root")
	}

	const deviceSize = 16 * 1024 * 1024
	const blockSize = 4096

	backend := newIntegrationBackend(deviceSize)

	config := DefaultConfig()
	config.Size = deviceSize
	config.BlockSize = blockSize
	config.NrHWQueues = 1
	config.QueueDepth = 64

	dev, err := CreateDevice(backend, config)
	if err != nil {
		t.Fatalf("CreateDevice failed: %v", err)
	}
	defer dev.Delete()

	time.Sleep(100 * time.Millisecond)

	// Open with O_DIRECT
	fd, err := unix.Open(dev.BlockDevicePath(), unix.O_RDWR|unix.O_DIRECT|unix.O_SYNC, 0)
	if err != nil {
		t.Fatalf("Failed to open with O_DIRECT: %v", err)
	}
	defer unix.Close(fd)

	// Allocate aligned buffer for O_DIRECT
	buf := make([]byte, blockSize+blockSize)
	aligned := buf[:blockSize]
	// Ensure alignment (this is a simplification; real code should use mmap or posix_memalign)
	offset := uintptr(unsafe.Pointer(&buf[0])) % uintptr(blockSize)
	if offset != 0 {
		aligned = buf[blockSize-int(offset) : 2*blockSize-int(offset)]
	}

	// Write pattern
	for i := range aligned {
		aligned[i] = byte(i % 256)
	}

	n, err := unix.Pwrite(fd, aligned, 0)
	if err != nil {
		t.Fatalf("Pwrite failed: %v", err)
	}
	if n != blockSize {
		t.Fatalf("Short write: %d", n)
	}

	// Verify in backend
	backendData := backend.getData(0, blockSize)
	if !bytes.Equal(backendData, aligned) {
		t.Fatalf("Backend data mismatch after direct write")
	}

	// Clear and read back
	clear(aligned)
	n, err = unix.Pread(fd, aligned, 0)
	if err != nil {
		t.Fatalf("Pread failed: %v", err)
	}
	if n != blockSize {
		t.Fatalf("Short read: %d", n)
	}

	// Verify read data
	for i := range aligned {
		if aligned[i] != byte(i%256) {
			t.Fatalf("Read data mismatch at byte %d", i)
		}
	}

	t.Log("Direct I/O test passed")
}

// TestIntegrationConcurrentIO tests concurrent I/O operations.
func TestIntegrationConcurrentIO(t *testing.T) {
	if os.Getuid() != 0 {
		t.Skip("Integration tests require root")
	}

	const deviceSize = 64 * 1024 * 1024 // 64MB
	const blockSize = 4096
	const numGoroutines = 8
	const opsPerGoroutine = 100

	backend := newIntegrationBackend(deviceSize)

	config := DefaultConfig()
	config.Size = deviceSize
	config.BlockSize = blockSize
	config.NrHWQueues = 1
	config.QueueDepth = 128

	dev, err := CreateDevice(backend, config)
	if err != nil {
		t.Fatalf("CreateDevice failed: %v", err)
	}
	defer dev.Delete()

	time.Sleep(100 * time.Millisecond)

	var wg sync.WaitGroup
	errors := make(chan error, numGoroutines*opsPerGoroutine)

	for g := range numGoroutines {
		wg.Add(1)
		go func(goroutineID int) {
			defer wg.Done()

			fd, err := unix.Open(dev.BlockDevicePath(), unix.O_RDWR|unix.O_SYNC, 0)
			if err != nil {
				errors <- fmt.Errorf("goroutine %d: open failed: %w", goroutineID, err)
				return
			}
			defer unix.Close(fd)

			buf := make([]byte, blockSize)

			for i := range opsPerGoroutine {
				// Each goroutine writes to its own region
				offset := int64(goroutineID*opsPerGoroutine+i) * blockSize
				if offset+blockSize > deviceSize {
					continue
				}

				// Write unique pattern
				pattern := byte((goroutineID * opsPerGoroutine) + i)
				for j := range buf {
					buf[j] = pattern
				}

				n, err := unix.Pwrite(fd, buf, offset)
				if err != nil {
					errors <- fmt.Errorf("goroutine %d op %d: write failed: %w", goroutineID, i, err)
					continue
				}
				if n != blockSize {
					errors <- fmt.Errorf("goroutine %d op %d: short write: %d", goroutineID, i, n)
				}

				// Read back and verify
				clear(buf)
				n, err = unix.Pread(fd, buf, offset)
				if err != nil {
					errors <- fmt.Errorf("goroutine %d op %d: read failed: %w", goroutineID, i, err)
					continue
				}

				for j := range buf[:n] {
					if buf[j] != pattern {
						errors <- fmt.Errorf("goroutine %d op %d: data mismatch at byte %d", goroutineID, i, j)
						break
					}
				}
			}
		}(g)
	}

	wg.Wait()
	close(errors)

	var errCount int
	for err := range errors {
		t.Error(err)
		errCount++
		if errCount > 10 {
			t.Fatal("Too many errors, stopping")
		}
	}

	stats := dev.Stats().Snapshot()
	t.Logf("Concurrent I/O stats: reads=%d writes=%d errors=%d",
		stats.Reads, stats.Writes, stats.ReadErrors+stats.WriteErrors+stats.OtherErrors)

	if errCount == 0 {
		t.Log("Concurrent I/O test passed")
	}
}
