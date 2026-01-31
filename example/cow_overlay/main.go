// cow_overlay demonstrates a copy-on-write overlay pattern.
//
// This example shows how to create multiple ublk devices that share a
// read-only base layer, with each having its own writable overlay.
// Writes go to the overlay, reads check overlay first then fall back to base.
//
// Architecture:
//
//	┌─────────────┐  ┌─────────────┐  ┌─────────────┐
//	│  Device A   │  │  Device B   │  │  Device C   │
//	│  (overlay)  │  │  (overlay)  │  │  (overlay)  │
//	└──────┬──────┘  └──────┬──────┘  └──────┬──────┘
//	       │                │                │
//	       └────────────────┼────────────────┘
//	                        │
//	               ┌────────┴────────┐
//	               │   Base Image    │
//	               │   (read-only)   │
//	               └─────────────────┘
//
// Note: This uses user-copy mode because true zero-copy for COW requires
// per-block routing decisions that can't be done with io_uring fixed files.
// The trade-off is a memory copy, but we avoid disk I/O for cached blocks.
//
// Run with: sudo go run ./example/cow_overlay/
package main

import (
	"fmt"
	"log"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/e2b-dev/ublk-go/ublk"
)

// COWBackend implements copy-on-write over a read-only base.
// Multiple COWBackends can share the same base for efficient cloning.
type COWBackend struct {
	mu        sync.RWMutex
	base      *os.File // Read-only base image (can be shared)
	overlay   *os.File // Per-instance writable overlay
	dirty     []uint64 // Bitmap: 1 bit per block
	blockSize int64
	size      int64
}

// NewCOWBackend creates a new copy-on-write backend over a base file.
// The base can be shared among multiple backends for efficient cloning.
func NewCOWBackend(base *os.File, size, blockSize int64) (*COWBackend, error) {
	// Create overlay using memfd
	overlayFD, err := unix.MemfdCreate("ublk-cow-overlay", unix.MFD_CLOEXEC)
	if err != nil {
		return nil, fmt.Errorf("memfd_create for overlay failed: %w", err)
	}
	overlay := os.NewFile(uintptr(overlayFD), "memfd:cow-overlay")

	if err := overlay.Truncate(size); err != nil {
		overlay.Close()
		return nil, fmt.Errorf("overlay truncate failed: %w", err)
	}

	// Calculate bitmap size (1 bit per block, rounded up to uint64)
	numBlocks := (size + blockSize - 1) / blockSize
	bitmapSize := (numBlocks + 63) / 64

	return &COWBackend{
		base:      base,
		overlay:   overlay,
		dirty:     make([]uint64, bitmapSize),
		blockSize: blockSize,
		size:      size,
	}, nil
}

// isBlockDirty checks if a block has been written to the overlay.
func (b *COWBackend) isBlockDirty(blockNum int64) bool {
	idx := blockNum / 64
	bit := uint64(1) << (blockNum % 64)
	return b.dirty[idx]&bit != 0
}

// markBlockDirty marks a block as written to the overlay.
func (b *COWBackend) markBlockDirty(blockNum int64) {
	idx := blockNum / 64
	bit := uint64(1) << (blockNum % 64)
	b.dirty[idx] |= bit
}

// ReadAt reads from overlay if dirty, otherwise from base.
func (b *COWBackend) ReadAt(p []byte, off int64) (int, error) {
	b.mu.RLock()
	defer b.mu.RUnlock()

	// Handle block-aligned reads efficiently
	startBlock := off / b.blockSize
	endOff := off + int64(len(p))
	endBlock := (endOff + b.blockSize - 1) / b.blockSize

	// Fast path: all blocks from same source
	allDirty := true
	allClean := true
	for blk := startBlock; blk < endBlock; blk++ {
		if b.isBlockDirty(blk) {
			allClean = false
		} else {
			allDirty = false
		}
	}

	if allDirty {
		return b.overlay.ReadAt(p, off)
	}
	if allClean {
		return b.base.ReadAt(p, off)
	}

	// Slow path: mixed blocks, read per-block
	totalRead := 0
	remaining := p
	currentOff := off

	for len(remaining) > 0 && currentOff < b.size {
		blockNum := currentOff / b.blockSize
		blockStart := blockNum * b.blockSize
		blockEnd := min(blockStart+b.blockSize, b.size)

		// Calculate how much to read from this block
		readEnd := min(blockEnd, off+int64(len(p)))
		toRead := int(readEnd - currentOff)

		var src *os.File
		if b.isBlockDirty(blockNum) {
			src = b.overlay
		} else {
			src = b.base
		}

		n, err := src.ReadAt(remaining[:toRead], currentOff)
		totalRead += n
		if err != nil {
			return totalRead, err
		}

		remaining = remaining[n:]
		currentOff += int64(n)
	}

	return totalRead, nil
}

// WriteAt writes to the overlay and marks blocks as dirty.
func (b *COWBackend) WriteAt(p []byte, off int64) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	// Write to overlay
	n, err := b.overlay.WriteAt(p, off)
	if err != nil {
		return n, err
	}

	// Mark affected blocks as dirty
	startBlock := off / b.blockSize
	endOff := off + int64(n)
	endBlock := (endOff + b.blockSize - 1) / b.blockSize

	for blk := startBlock; blk < endBlock; blk++ {
		// If block wasn't dirty, we need to copy unwritten portions from base
		if !b.isBlockDirty(blk) {
			blockStart := blk * b.blockSize
			blockEnd := min(blockStart+b.blockSize, b.size)

			// Copy portions of the block not covered by this write
			if off > blockStart {
				// Copy from base: [blockStart, off)
				buf := make([]byte, off-blockStart)
				if _, err := b.base.ReadAt(buf, blockStart); err == nil {
					b.overlay.WriteAt(buf, blockStart)
				}
			}
			if endOff < blockEnd {
				// Copy from base: [endOff, blockEnd)
				buf := make([]byte, blockEnd-endOff)
				if _, err := b.base.ReadAt(buf, endOff); err == nil {
					b.overlay.WriteAt(buf, endOff)
				}
			}

			b.markBlockDirty(blk)
		}
	}

	return n, nil
}

// Flush syncs the overlay to stable storage.
func (b *COWBackend) Flush() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.overlay.Sync()
}

// DirtyBlockCount returns the number of blocks written to the overlay.
func (b *COWBackend) DirtyBlockCount() int64 {
	b.mu.RLock()
	defer b.mu.RUnlock()

	var count int64
	for _, word := range b.dirty {
		// Count set bits
		for word != 0 {
			count++
			word &= word - 1
		}
	}
	return count
}

// Close releases overlay resources (does not close shared base).
func (b *COWBackend) Close() error {
	return b.overlay.Close()
}

func main() {
	log.SetFlags(log.Ltime | log.Lmicroseconds)

	// Create a shared base image using memfd
	size := int64(64 * 1024 * 1024) // 64MB
	blockSize := int64(4096)        // 4KB blocks

	baseFD, err := unix.MemfdCreate("ublk-cow-base", unix.MFD_CLOEXEC)
	if err != nil {
		log.Fatalf("Failed to create base memfd: %v", err)
	}
	base := os.NewFile(uintptr(baseFD), "memfd:cow-base")
	defer base.Close()

	if err := base.Truncate(size); err != nil {
		base.Close()
		log.Fatalf("Failed to truncate base: %v", err)
	}

	// Write some initial data to base (simulating a disk image)
	pattern := []byte("BASE_DATA_")
	for off := int64(0); off < size; off += int64(len(pattern)) {
		base.WriteAt(pattern, off)
	}
	log.Printf("Created base image: %d bytes", size)

	// Create two COW backends sharing the same base
	cow1, err := NewCOWBackend(base, size, blockSize)
	if err != nil {
		log.Fatalf("Failed to create COW backend 1: %v", err)
	}
	defer cow1.Close()

	cow2, err := NewCOWBackend(base, size, blockSize)
	if err != nil {
		log.Fatalf("Failed to create COW backend 2: %v", err)
	}
	defer cow2.Close()

	// Create devices with user-copy mode (required for COW routing logic)
	config := ublk.DefaultConfig()
	config.Size = uint64(size)
	config.UserCopy = true // Use user-copy for COW decision making

	dev1, err := ublk.New(cow1, config)
	if err != nil {
		log.Fatalf("Failed to create device 1: %v", err)
	}
	defer dev1.Delete()

	dev2, err := ublk.New(cow2, config)
	if err != nil {
		log.Fatalf("Failed to create device 2: %v", err)
	}
	defer dev2.Delete()

	log.Printf("Created COW device 1: %s", dev1.BlockDevicePath())
	log.Printf("Created COW device 2: %s", dev2.BlockDevicePath())
	log.Println()
	log.Println("Both devices share the same read-only base image.")
	log.Println("Writes to one device don't affect the other.")
	log.Println()
	log.Println("Test with:")
	log.Printf("  echo 'DEVICE1' | sudo dd of=%s bs=1 seek=0", dev1.BlockDevicePath())
	log.Printf("  echo 'DEVICE2' | sudo dd of=%s bs=1 seek=0", dev2.BlockDevicePath())
	log.Printf("  sudo xxd %s | head -1", dev1.BlockDevicePath())
	log.Printf("  sudo xxd %s | head -1", dev2.BlockDevicePath())

	// Wait for signal
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	log.Println()
	log.Println("Press Ctrl+C to stop.")

	for {
		select {
		case <-sigCh:
			log.Println("Shutting down...")
			return
		case <-ticker.C:
			log.Printf("Device 1: %d dirty blocks, Device 2: %d dirty blocks",
				cow1.DirtyBlockCount(), cow2.DirtyBlockCount())
		}
	}
}
