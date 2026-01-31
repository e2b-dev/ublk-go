//go:build integration

// Integration tests require:
// - Root privileges (sudo)
// - ublk kernel module loaded (modprobe ublk_drv)
//
// Run with: sudo go test -tags=integration -v ./ublk -run=Integration
// Or use:   sudo make test-integration

package ublk

import (
	"bytes"
	"crypto/rand"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

func init() {
	// Check prerequisites at test initialization
	if os.Getuid() != 0 {
		fmt.Fprintln(os.Stderr, "WARNING: Integration tests require root privileges")
	}
}

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

// TestIntegrationRandomIO tests random read/write patterns.
func TestIntegrationRandomIO(t *testing.T) {
	if os.Getuid() != 0 {
		t.Skip("Integration tests require root")
	}

	const deviceSize = 32 * 1024 * 1024 // 32MB
	const blockSize = 4096
	const numOps = 200

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

	fd, err := unix.Open(dev.BlockDevicePath(), unix.O_RDWR|unix.O_SYNC, 0)
	if err != nil {
		t.Fatalf("Failed to open block device: %v", err)
	}
	defer unix.Close(fd)

	// Random I/O test
	buf := make([]byte, blockSize)
	readBuf := make([]byte, blockSize)

	for i := 0; i < numOps; i++ {
		// Random offset (block-aligned)
		var offsetBytes [8]byte
		rand.Read(offsetBytes[:])
		offset := int64(offsetBytes[0]|offsetBytes[1]<<8) * blockSize
		offset = offset % (deviceSize - blockSize)
		offset = (offset / blockSize) * blockSize // Ensure alignment

		// Random data
		rand.Read(buf)

		// Write
		n, err := unix.Pwrite(fd, buf, offset)
		if err != nil {
			t.Fatalf("Write %d failed at offset %d: %v", i, offset, err)
		}
		if n != blockSize {
			t.Fatalf("Short write: %d", n)
		}

		// Read back
		n, err = unix.Pread(fd, readBuf, offset)
		if err != nil {
			t.Fatalf("Read %d failed at offset %d: %v", i, offset, err)
		}
		if n != blockSize {
			t.Fatalf("Short read: %d", n)
		}

		// Verify
		if !bytes.Equal(buf, readBuf) {
			t.Fatalf("Data mismatch at operation %d, offset %d", i, offset)
		}
	}

	t.Logf("Random I/O test passed: %d operations", numOps)
}

// TestIntegrationMultipleDevices tests creating multiple devices.
func TestIntegrationMultipleDevices(t *testing.T) {
	if os.Getuid() != 0 {
		t.Skip("Integration tests require root")
	}

	const deviceSize = 8 * 1024 * 1024 // 8MB each
	const numDevices = 3

	devices := make([]*Device, numDevices)
	backends := make([]*integrationBackend, numDevices)

	// Create multiple devices
	for i := 0; i < numDevices; i++ {
		backends[i] = newIntegrationBackend(deviceSize)

		config := DefaultConfig()
		config.Size = deviceSize
		config.BlockSize = 4096
		config.NrHWQueues = 1
		config.QueueDepth = 32

		dev, err := CreateDevice(backends[i], config)
		if err != nil {
			// Clean up already created devices
			for j := 0; j < i; j++ {
				devices[j].Delete()
			}
			t.Fatalf("CreateDevice %d failed: %v", i, err)
		}
		devices[i] = dev
		t.Logf("Created device %d: %s", i, dev.BlockDevicePath())
	}

	// Clean up all devices
	defer func() {
		for i, dev := range devices {
			if dev != nil {
				if err := dev.Delete(); err != nil {
					t.Logf("Warning: failed to delete device %d: %v", i, err)
				}
			}
		}
	}()

	time.Sleep(200 * time.Millisecond)

	// Write unique data to each device
	for i, dev := range devices {
		fd, err := unix.Open(dev.BlockDevicePath(), unix.O_RDWR|unix.O_SYNC, 0)
		if err != nil {
			t.Fatalf("Failed to open device %d: %v", i, err)
		}

		pattern := bytes.Repeat([]byte{byte(i + 1)}, 4096)
		n, err := unix.Pwrite(fd, pattern, 0)
		unix.Close(fd)

		if err != nil || n != len(pattern) {
			t.Fatalf("Write to device %d failed: %v", i, err)
		}
	}

	// Verify each device has correct data
	for i, dev := range devices {
		fd, err := unix.Open(dev.BlockDevicePath(), unix.O_RDONLY, 0)
		if err != nil {
			t.Fatalf("Failed to open device %d for read: %v", i, err)
		}

		buf := make([]byte, 4096)
		n, err := unix.Pread(fd, buf, 0)
		unix.Close(fd)

		if err != nil || n != len(buf) {
			t.Fatalf("Read from device %d failed: %v", i, err)
		}

		expected := bytes.Repeat([]byte{byte(i + 1)}, 4096)
		if !bytes.Equal(buf, expected) {
			t.Fatalf("Device %d data mismatch", i)
		}
	}

	t.Logf("Multiple devices test passed: %d devices", numDevices)
}

// TestIntegrationFlush tests the flush operation.
func TestIntegrationFlush(t *testing.T) {
	if os.Getuid() != 0 {
		t.Skip("Integration tests require root")
	}

	const deviceSize = 16 * 1024 * 1024

	flushCount := 0
	backend := &flushTestBackend{
		integrationBackend: newIntegrationBackend(deviceSize),
		flushCount:         &flushCount,
	}

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

	time.Sleep(100 * time.Millisecond)

	// Open with O_SYNC to trigger flushes
	fd, err := unix.Open(dev.BlockDevicePath(), unix.O_RDWR|unix.O_SYNC, 0)
	if err != nil {
		t.Fatalf("Failed to open: %v", err)
	}
	defer unix.Close(fd)

	// Write data
	buf := make([]byte, 4096)
	rand.Read(buf)
	_, err = unix.Pwrite(fd, buf, 0)
	if err != nil {
		t.Fatalf("Write failed: %v", err)
	}

	// Sync should trigger flush
	err = unix.Fsync(fd)
	if err != nil {
		t.Fatalf("Fsync failed: %v", err)
	}

	// Give time for flush to be processed
	time.Sleep(100 * time.Millisecond)

	t.Logf("Flush test passed, flush count: %d", flushCount)
}

type flushTestBackend struct {
	*integrationBackend
	flushCount *int
}

func (b *flushTestBackend) Flush() error {
	*b.flushCount++
	return nil
}

// TestIntegrationStress runs a stress test with many operations.
func TestIntegrationStress(t *testing.T) {
	if os.Getuid() != 0 {
		t.Skip("Integration tests require root")
	}

	if testing.Short() {
		t.Skip("Skipping stress test in short mode")
	}

	const deviceSize = 64 * 1024 * 1024 // 64MB
	const blockSize = 4096
	const duration = 5 * time.Second
	const numWorkers = 4

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
	stopCh := make(chan struct{})
	var totalOps int64
	var totalErrors int64
	var mu sync.Mutex

	// Start workers
	for w := 0; w < numWorkers; w++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()

			fd, err := unix.Open(dev.BlockDevicePath(), unix.O_RDWR, 0)
			if err != nil {
				mu.Lock()
				totalErrors++
				mu.Unlock()
				return
			}
			defer unix.Close(fd)

			buf := make([]byte, blockSize)
			ops := int64(0)
			errors := int64(0)

			for {
				select {
				case <-stopCh:
					mu.Lock()
					totalOps += ops
					totalErrors += errors
					mu.Unlock()
					return
				default:
				}

				// Random offset
				var offsetBytes [2]byte
				rand.Read(offsetBytes[:])
				offset := int64(offsetBytes[0]|offsetBytes[1]<<8) * blockSize
				offset = offset % (deviceSize - blockSize)

				// Random operation
				var opByte [1]byte
				rand.Read(opByte[:])

				if opByte[0]%2 == 0 {
					// Write
					rand.Read(buf)
					_, err := unix.Pwrite(fd, buf, offset)
					if err != nil {
						errors++
					}
				} else {
					// Read
					_, err := unix.Pread(fd, buf, offset)
					if err != nil {
						errors++
					}
				}
				ops++
			}
		}(w)
	}

	// Run for duration
	time.Sleep(duration)
	close(stopCh)
	wg.Wait()

	stats := dev.Stats().Snapshot()
	opsPerSec := float64(totalOps) / duration.Seconds()

	t.Logf("Stress test completed:")
	t.Logf("  Duration: %v", duration)
	t.Logf("  Workers: %d", numWorkers)
	t.Logf("  Total ops: %d (%.0f ops/sec)", totalOps, opsPerSec)
	t.Logf("  Errors: %d", totalErrors)
	t.Logf("  Stats: reads=%d writes=%d", stats.Reads, stats.Writes)

	if totalErrors > 0 {
		t.Errorf("Stress test had %d errors", totalErrors)
	}
}

// TestIntegrationFilesystem tests filesystem creation and mounting.
func TestIntegrationFilesystem(t *testing.T) {
	if os.Getuid() != 0 {
		t.Skip("Integration tests require root")
	}

	// Check for mkfs.ext4
	if _, err := exec.LookPath("mkfs.ext4"); err != nil {
		t.Skip("mkfs.ext4 not found")
	}

	const deviceSize = 64 * 1024 * 1024 // 64MB (min for some FS)
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

	blockDevPath := dev.BlockDevicePath()
	t.Logf("Created device for FS test: %s", blockDevPath)

	// Create ext4 filesystem
	cmd := exec.Command("mkfs.ext4", "-F", blockDevPath)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("mkfs.ext4 failed: %v\nOutput: %s", err, out)
	}
	t.Log("Filesystem created")

	// Create mount point
	mountPoint, err := os.MkdirTemp("", "ublk-mount-test-*")
	if err != nil {
		t.Fatalf("Failed to create mount point: %v", err)
	}
	defer os.RemoveAll(mountPoint)

	// Mount
	cmd = exec.Command("mount", blockDevPath, mountPoint)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("mount failed: %v\nOutput: %s", err, out)
	}
	// Defer unmount (using umount -l to be safe against busy loops if test fails)
	defer exec.Command("umount", mountPoint).Run()

	t.Logf("Mounted at %s", mountPoint)

	// File operations
	testFile := filepath.Join(mountPoint, "hello.txt")
	content := []byte("Hello, ublk filesystem!")

	// Write
	if err := os.WriteFile(testFile, content, 0o644); err != nil {
		t.Fatalf("Failed to write to file: %v", err)
	}

	// Sync to ensure data hits the device (via backend)
	unix.Sync()

	// Read
	readBack, err := os.ReadFile(testFile)
	if err != nil {
		t.Fatalf("Failed to read from file: %v", err)
	}

	if !bytes.Equal(readBack, content) {
		t.Errorf("File content mismatch: got %q, want %q", readBack, content)
	}

	t.Log("File operations passed")

	// Unmount explicitly to checking exit code
	if err := exec.Command("umount", mountPoint).Run(); err != nil {
		t.Fatalf("Unmount failed: %v", err)
	}
	t.Log("Unmounted")
}

// TestIntegrationMmapCoherency tests consistency between mmap and standard I/O.
func TestIntegrationMmapCoherency(t *testing.T) {
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

	fd, err := unix.Open(dev.BlockDevicePath(), unix.O_RDWR|unix.O_SYNC, 0)
	if err != nil {
		t.Fatalf("Failed to open: %v", err)
	}
	defer unix.Close(fd)

	// Mmap the device
	data, err := unix.Mmap(fd, 0, deviceSize, unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED)
	if err != nil {
		t.Fatalf("Failed to mmap: %v", err)
	}
	defer unix.Munmap(data)

	t.Run("MmapWrite_PreadRead", func(t *testing.T) {
		pattern := []byte("Written via Mmap")
		offset := 0
		// Write to mmap
		copy(data[offset:], pattern)
		if err := unix.Msync(data[offset:offset+4096], unix.MS_SYNC); err != nil {
			t.Fatalf("Msync failed: %v", err)
		}

		// Read via Pread
		buf := make([]byte, len(pattern))
		if _, err := unix.Pread(fd, buf, int64(offset)); err != nil {
			t.Fatalf("Pread failed: %v", err)
		}

		if !bytes.Equal(buf, pattern) {
			t.Errorf("Mismatch: Mmap wrote %q, Pread read %q", pattern, buf)
		}
	})

	t.Run("Pwrite_MmapRead", func(t *testing.T) {
		pattern := []byte("Written via Pwrite")
		offset := 4096
		// Write via Pwrite
		if _, err := unix.Pwrite(fd, pattern, int64(offset)); err != nil {
			t.Fatalf("Pwrite failed: %v", err)
		}

		// Read from mmap (should be coherent)
		got := data[offset : offset+len(pattern)]
		if !bytes.Equal(got, pattern) {
			// Try msync to invalidate? Actually MAP_SHARED + Page Cache should make it visible immediately
			// if the kernel updates the page.
			t.Errorf("Mismatch: Pwrite wrote %q, Mmap saw %q", pattern, got)
		}
	})

	t.Log("Mmap coherency verified")
}

// TestIntegrationMsync tests that msync triggers flush.
func TestIntegrationMsync(t *testing.T) {
	if os.Getuid() != 0 {
		t.Skip("Integration tests require root")
	}

	const deviceSize = 16 * 1024 * 1024
	const blockSize = 4096

	flushCount := 0
	backend := &flushTestBackend{
		integrationBackend: newIntegrationBackend(deviceSize),
		flushCount:         &flushCount,
	}

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

	fd, err := unix.Open(dev.BlockDevicePath(), unix.O_RDWR, 0)
	if err != nil {
		t.Fatalf("Failed to open: %v", err)
	}
	defer unix.Close(fd)

	// Mmap
	data, err := unix.Mmap(fd, 0, deviceSize, unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED)
	if err != nil {
		t.Fatalf("Failed to mmap: %v", err)
	}
	defer unix.Munmap(data)

	// Write to mmap
	copy(data[0:], []byte("Sync Me"))

	// Verify no flush yet (approximate, since OS might flush background)
	initialFlushes := *backend.flushCount

	// Msync with MS_SYNC should trigger flush
	if err := unix.Msync(data[:4096], unix.MS_SYNC); err != nil {
		t.Fatalf("Msync failed: %v", err)
	}

	// Give it a moment to process
	time.Sleep(100 * time.Millisecond)

	if *backend.flushCount <= initialFlushes {
		t.Errorf("Msync did not trigger backend flush. Count: %d -> %d", initialFlushes, *backend.flushCount)
	} else {
		t.Logf("Msync triggered flush (count: %d)", *backend.flushCount)
	}
}

func TestIntegrationFirecrackerCompat(t *testing.T) {
	// Verifies compatibility with Firecracker's live migration pattern:
	// mmap(MAP_SHARED|MAP_NORESERVE, PROT_READ|PROT_WRITE) + msync(MS_SYNC)

	requireRoot(t)
	dev, err := createTestDevice(t, 1024*1024, 1, 64)
	if err != nil {
		t.Fatal(err)
	}
	defer dev.Delete()

	// Wait for device to be ready
	time.Sleep(100 * time.Millisecond)

	fd, err := unix.Open(dev.BlockDevicePath(), unix.O_RDWR, 0)
	if err != nil {
		t.Fatalf("Failed to open device: %v", err)
	}
	defer unix.Close(fd)

	// Firecracker config: PROT_READ|PROT_WRITE, MAP_SHARED|MAP_NORESERVE
	// Note: MAP_NORESERVE is not strictly defined in x/sys/unix for all archs universally stable,
	// but it is standard Linux. We use unix.MAP_NORESERVE.
	data, err := unix.Mmap(fd, 0, 1024*1024, unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED|unix.MAP_NORESERVE)
	if err != nil {
		t.Fatalf("Mmap failed: %v", err)
	}
	defer unix.Munmap(data)

	// Write data to mmap
	copy(data[:5], []byte("TEST1"))

	// Flush using MS_SYNC (Firecracker pattern)
	if err := unix.Msync(data, unix.MS_SYNC); err != nil {
		t.Fatalf("Msync failed: %v", err)
	}

	// Verify data persistence by reading back via syscall (bypassing mmap cache if possible, or just consistency)
	buf := make([]byte, 5)
	if _, err := unix.Pread(fd, buf, 0); err != nil {
		t.Fatalf("Pread failed: %v", err)
	}
	if string(buf) != "TEST1" {
		t.Errorf("Expected 'TEST1', got '%s'", string(buf))
	}
}

func TestIntegrationFallocate(t *testing.T) {
	requireRoot(t)

	const deviceSize = 1024 * 1024 // 1MB
	backend := newIntegrationBackend(deviceSize)

	config := DefaultConfig()
	config.Size = deviceSize
	config.BlockSize = 512
	config.NrHWQueues = 1
	config.QueueDepth = 64
	// Enable DISCARD support explicitly
	config.MaxDiscardSectors = 256
	config.MaxDiscardSegments = 1

	dev, err := CreateDevice(backend, config)
	if err != nil {
		t.Fatal(err)
	}
	defer dev.Delete()

	// Wait for device to be ready
	time.Sleep(100 * time.Millisecond)

	fd, err := unix.Open(dev.BlockDevicePath(), unix.O_RDWR, 0)
	if err != nil {
		t.Fatalf("Failed to open device: %v", err)
	}
	defer unix.Close(fd)

	// Test pure fallocate (preallocate) - usually translates to valid if backend allows writes,
	// or might return EOPNOTSUPP if block device doesn't support specific fallocate op.
	// For block devices, fallocate(0) is often emulation or requires specific support.
	err = unix.Fallocate(fd, 0, 0, 4096)
	if err != nil {
		t.Logf("Fallocate(0) result: %v", err)
	} else {
		t.Log("Fallocate(0) succeeded")
	}

	// Test ZERO_RANGE - translates to WRITE_ZEROES
	err = unix.Fallocate(fd, unix.FALLOC_FL_ZERO_RANGE, 4096, 4096)
	if err != nil {
		t.Logf("Fallocate(ZERO_RANGE) result: %v", err)
	} else {
		t.Log("Fallocate(ZERO_RANGE) succeeded")
	}

	// Test PUNCH_HOLE - translates to DISCARD
	err = unix.Fallocate(fd, unix.FALLOC_FL_PUNCH_HOLE|unix.FALLOC_FL_KEEP_SIZE, 8192, 4096)
	if err != nil {
		t.Logf("Fallocate(PUNCH_HOLE) result: %v", err)
	} else {
		t.Log("Fallocate(PUNCH_HOLE) succeeded")
	}
}
