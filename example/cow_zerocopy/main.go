// cow_zerocopy demonstrates an efficient copy-on-write overlay pattern
// with bitmap-based routing and sparse file diff extraction.
//
// Architecture:
//
//	┌─────────────────────────────────────────────────────────────┐
//	│                      ublk Device                            │
//	├─────────────────────────────────────────────────────────────┤
//	│  COWBackend                                                 │
//	│  ┌─────────────────────────────────────────────────────┐   │
//	│  │  Bitmap: O(1) per-block dirty tracking              │   │
//	│  │  [0][1][1][0][0][1][1][1][0][0]...                  │   │
//	│  └─────────────────────────────────────────────────────┘   │
//	│                         │                                   │
//	│         ┌───────────────┴───────────────┐                  │
//	│         ▼                               ▼                  │
//	│  ┌─────────────┐                ┌─────────────────┐        │
//	│  │  Base File  │                │  Sparse Overlay │        │
//	│  │ (read-only) │                │   (grows on     │        │
//	│  │             │                │    write)       │        │
//	│  └─────────────┘                └─────────────────┘        │
//	└─────────────────────────────────────────────────────────────┘
//
// Features:
//   - O(1) bitmap check for routing decisions
//   - Sparse overlay file (only dirty blocks consume disk space)
//   - SEEK_HOLE/SEEK_DATA for O(extents) diff extraction
//   - Proper handling of multi-block requests spanning dirty/clean regions
//   - Efficient diff export for uploading changes
//
// Run with: sudo go run ./example/cow_zerocopy/
package main

import (
	"encoding/binary"
	"fmt"
	"io"
	"log"
	"math/bits"
	"os"
	"os/signal"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/e2b-dev/ublk-go/ublk"
)

// COWBackend implements copy-on-write over a read-only base with a sparse overlay.
// It maintains a bitmap for O(1) dirty block tracking and uses SEEK_HOLE/SEEK_DATA
// for efficient diff extraction.
type COWBackend struct {
	mu sync.RWMutex

	base      *os.File // Read-only base image
	overlay   *os.File // Sparse overlay file
	blockSize int64
	size      int64

	// Bitmap: 1 bit per block, O(1) dirty check
	dirty []uint64

	// Statistics
	readsFromBase    atomic.Uint64
	readsFromOverlay atomic.Uint64
	readsMixed       atomic.Uint64
	writes           atomic.Uint64
	dirtyBlocks      atomic.Int64
}

// NewCOWBackend creates a new copy-on-write backend.
// base: read-only source file
// overlayPath: path for sparse overlay file (will be created/truncated)
// size: device size in bytes
// blockSize: block size for tracking (typically 4096).
func NewCOWBackend(base *os.File, overlayPath string, size, blockSize int64) (*COWBackend, error) {
	// Create sparse overlay file
	overlay, err := os.OpenFile(overlayPath, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return nil, fmt.Errorf("failed to create overlay: %w", err)
	}

	// Set size without allocating (sparse file)
	if err := overlay.Truncate(size); err != nil {
		overlay.Close()
		return nil, fmt.Errorf("failed to set overlay size: %w", err)
	}

	// Calculate bitmap size (1 bit per block, rounded up to uint64)
	numBlocks := (size + blockSize - 1) / blockSize
	bitmapWords := (numBlocks + 63) / 64

	return &COWBackend{
		base:      base,
		overlay:   overlay,
		blockSize: blockSize,
		size:      size,
		dirty:     make([]uint64, bitmapWords),
	}, nil
}

// isBlockDirty checks if a block has been written (O(1)).
func (b *COWBackend) isBlockDirty(blockNum int64) bool {
	idx := blockNum / 64
	bit := uint64(1) << (blockNum % 64)
	return b.dirty[idx]&bit != 0
}

// markBlockDirty marks a block as written.
func (b *COWBackend) markBlockDirty(blockNum int64) bool {
	idx := blockNum / 64
	bit := uint64(1) << (blockNum % 64)
	wasClean := b.dirty[idx]&bit == 0
	b.dirty[idx] |= bit
	return wasClean
}

// classifyRange determines if a byte range is all dirty, all clean, or mixed.
func (b *COWBackend) classifyRange(offset, length int64) (allDirty, allClean bool) {
	startBlock := offset / b.blockSize
	endBlock := (offset + length + b.blockSize - 1) / b.blockSize

	allDirty = true
	allClean = true

	for blk := startBlock; blk < endBlock; blk++ {
		if b.isBlockDirty(blk) {
			allClean = false
		} else {
			allDirty = false
		}
		// Early exit if mixed
		if !allDirty && !allClean {
			return false, false
		}
	}

	return allDirty, allClean
}

// ReadAt implements io.ReaderAt with COW routing.
func (b *COWBackend) ReadAt(p []byte, off int64) (int, error) {
	b.mu.RLock()
	defer b.mu.RUnlock()

	allDirty, allClean := b.classifyRange(off, int64(len(p)))

	// Fast paths: entire range from single source
	if allClean {
		b.readsFromBase.Add(1)
		return b.base.ReadAt(p, off)
	}
	if allDirty {
		b.readsFromOverlay.Add(1)
		return b.overlay.ReadAt(p, off)
	}

	// Mixed path: read block by block
	b.readsMixed.Add(1)
	return b.readMixed(p, off)
}

// readMixed handles reads that span both dirty and clean blocks.
func (b *COWBackend) readMixed(p []byte, off int64) (int, error) {
	totalRead := 0
	remaining := p
	currentOff := off

	for len(remaining) > 0 && currentOff < b.size {
		blockNum := currentOff / b.blockSize
		blockStart := blockNum * b.blockSize
		blockEnd := min(blockStart+b.blockSize, b.size)

		// Calculate how much to read from this block
		readStart := currentOff
		readEnd := min(blockEnd, off+int64(len(p)))
		toRead := int(readEnd - readStart)

		// Choose source based on dirty state
		var src *os.File
		if b.isBlockDirty(blockNum) {
			src = b.overlay
		} else {
			src = b.base
		}

		n, err := src.ReadAt(remaining[:toRead], currentOff)
		totalRead += n
		if err != nil && err != io.EOF {
			return totalRead, err
		}

		remaining = remaining[n:]
		currentOff += int64(n)

		if n < toRead {
			break
		}
	}

	return totalRead, nil
}

// WriteAt implements io.WriterAt, always writing to overlay.
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
	endBlock := (off + int64(n) + b.blockSize - 1) / b.blockSize

	newDirty := int64(0)
	for blk := startBlock; blk < endBlock; blk++ {
		if b.markBlockDirty(blk) {
			newDirty++
		}
	}
	b.dirtyBlocks.Add(newDirty)
	b.writes.Add(1)

	return n, nil
}

// Flush syncs the overlay to disk.
func (b *COWBackend) Flush() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.overlay.Sync()
}

// DirtyExtent represents a contiguous region of dirty data.
type DirtyExtent struct {
	Offset int64
	Length int64
}

// DirtyExtents returns all dirty regions using SEEK_HOLE/SEEK_DATA.
// This is O(number of extents), not O(total blocks).
func (b *COWBackend) DirtyExtents() ([]DirtyExtent, error) {
	b.mu.RLock()
	defer b.mu.RUnlock()

	var extents []DirtyExtent
	offset := int64(0)
	fd := int(b.overlay.Fd())

	for offset < b.size {
		// Find next data region
		dataStart, err := unix.Seek(fd, offset, unix.SEEK_DATA)
		if err != nil {
			// ENXIO means no more data
			break
		}

		// Find end of data region (next hole)
		holeStart, err := unix.Seek(fd, dataStart, unix.SEEK_HOLE)
		if err != nil {
			holeStart = b.size
		}

		extents = append(extents, DirtyExtent{
			Offset: dataStart,
			Length: holeStart - dataStart,
		})
		offset = holeStart
	}

	return extents, nil
}

// DirtyExtentsFromBitmap returns dirty regions by scanning the bitmap.
// This is O(bitmap words) + O(dirty blocks).
func (b *COWBackend) DirtyExtentsFromBitmap() []DirtyExtent {
	b.mu.RLock()
	defer b.mu.RUnlock()

	var extents []DirtyExtent
	inExtent := false
	var extentStart int64

	numBlocks := b.size / b.blockSize

	for wordIdx, word := range b.dirty {
		if word == 0 {
			// Skip 64 clean blocks at once
			if inExtent {
				// Close current extent
				extentEnd := int64(wordIdx) * 64 * b.blockSize
				extents = append(extents, DirtyExtent{
					Offset: extentStart,
					Length: extentEnd - extentStart,
				})
				inExtent = false
			}
			continue
		}

		// Process each bit in this word
		for bit := range 64 {
			blockNum := int64(wordIdx)*64 + int64(bit)
			if blockNum >= numBlocks {
				break
			}

			isDirty := word&(1<<bit) != 0

			if isDirty && !inExtent {
				extentStart = blockNum * b.blockSize
				inExtent = true
			} else if !isDirty && inExtent {
				extentEnd := blockNum * b.blockSize
				extents = append(extents, DirtyExtent{
					Offset: extentStart,
					Length: extentEnd - extentStart,
				})
				inExtent = false
			}
		}
	}

	// Close final extent if needed
	if inExtent {
		extents = append(extents, DirtyExtent{
			Offset: extentStart,
			Length: b.size - extentStart,
		})
	}

	return extents
}

// DirtyBlockCount returns the number of dirty blocks using popcount.
func (b *COWBackend) DirtyBlockCount() int64 {
	b.mu.RLock()
	defer b.mu.RUnlock()

	var count int64
	for _, word := range b.dirty {
		count += int64(bits.OnesCount64(word))
	}
	return count
}

// DirtyBytes returns total dirty data size.
func (b *COWBackend) DirtyBytes() int64 {
	return b.DirtyBlockCount() * b.blockSize
}

// ExportDiff writes all dirty data to the given writer in a simple format.
// Format: [offset:8][length:8][data:length]...
func (b *COWBackend) ExportDiff(w io.Writer) error {
	extents, err := b.DirtyExtents()
	if err != nil {
		return err
	}

	b.mu.RLock()
	defer b.mu.RUnlock()

	buf := make([]byte, b.blockSize*256) // 1MB buffer

	for _, ext := range extents {
		// Write header
		header := make([]byte, 16)
		binary.LittleEndian.PutUint64(header[0:8], uint64(ext.Offset))
		binary.LittleEndian.PutUint64(header[8:16], uint64(ext.Length))
		if _, err := w.Write(header); err != nil {
			return err
		}

		// Write data in chunks
		remaining := ext.Length
		offset := ext.Offset
		for remaining > 0 {
			toRead := min(remaining, int64(len(buf)))
			n, err := b.overlay.ReadAt(buf[:toRead], offset)
			if err != nil && err != io.EOF {
				return err
			}
			if _, err := w.Write(buf[:n]); err != nil {
				return err
			}
			remaining -= int64(n)
			offset += int64(n)
		}
	}

	return nil
}

// Stats returns current statistics.
func (b *COWBackend) Stats() map[string]uint64 {
	return map[string]uint64{
		"reads_from_base":    b.readsFromBase.Load(),
		"reads_from_overlay": b.readsFromOverlay.Load(),
		"reads_mixed":        b.readsMixed.Load(),
		"writes":             b.writes.Load(),
		"dirty_blocks":       uint64(b.dirtyBlocks.Load()),
	}
}

// Close releases resources.
func (b *COWBackend) Close() error {
	return b.overlay.Close()
}

func main() {
	log.SetFlags(log.Ltime | log.Lmicroseconds)

	// Configuration
	size := int64(128 * 1024 * 1024) // 128MB device
	blockSize := int64(4096)         // 4KB blocks
	basePath := "/tmp/ublk-cow-base"
	overlayPath := "/tmp/ublk-cow-overlay"

	// Create base image with pattern
	log.Println("Creating base image...")
	base, err := os.Create(basePath)
	if err != nil {
		log.Fatalf("Failed to create base: %v", err)
	}
	defer os.Remove(basePath)
	defer base.Close()

	if err := base.Truncate(size); err != nil {
		log.Fatalf("Failed to truncate base: %v", err)
	}

	// Write recognizable pattern to base
	pattern := []byte("BASE_DATA_PATTERN_")
	for off := int64(0); off < size; off += int64(len(pattern)) {
		base.WriteAt(pattern, off)
	}
	base.Sync()

	// Reopen base as read-only
	base.Close()
	base, err = os.Open(basePath)
	if err != nil {
		log.Fatalf("Failed to open base read-only: %v", err)
	}

	log.Printf("Base image: %s (%d MB)", basePath, size/1024/1024)

	// Create COW backend
	backend, err := NewCOWBackend(base, overlayPath, size, blockSize)
	if err != nil {
		log.Fatalf("Failed to create COW backend: %v", err)
	}
	defer os.Remove(overlayPath)
	defer backend.Close()

	log.Printf("Overlay: %s (sparse)", overlayPath)
	log.Printf("Block size: %d, Total blocks: %d", blockSize, size/blockSize)

	// Create ublk device with user-copy mode
	config := ublk.DefaultConfig()
	config.Size = uint64(size)
	config.BlockSize = uint64(blockSize)
	config.UserCopy = true // Required for COW routing in backend

	dev, err := ublk.CreateDevice(backend, config)
	if err != nil {
		log.Fatalf("Failed to create device: %v", err)
	}
	defer dev.Delete()

	log.Printf("Device: %s", dev.BlockDevicePath())
	log.Println()
	log.Println("Test commands:")
	log.Println("  # Write some data (creates dirty blocks)")
	log.Printf("  echo 'Hello COW!' | sudo dd of=%s bs=1 seek=0 conv=notrunc", dev.BlockDevicePath())
	log.Printf("  sudo dd if=/dev/urandom of=%s bs=4k count=100 seek=50 conv=notrunc", dev.BlockDevicePath())
	log.Println()
	log.Println("  # Read data (routes to base or overlay based on dirty state)")
	log.Printf("  sudo xxd %s | head -5", dev.BlockDevicePath())
	log.Println()
	log.Println("Press Ctrl+C to see dirty extents and stop.")

	// Signal handling
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	// Stats ticker
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-sigCh:
			log.Println()
			log.Println("=== Final Statistics ===")

			stats := backend.Stats()
			log.Printf("Reads from base:    %d", stats["reads_from_base"])
			log.Printf("Reads from overlay: %d", stats["reads_from_overlay"])
			log.Printf("Reads mixed:        %d", stats["reads_mixed"])
			log.Printf("Writes:             %d", stats["writes"])
			log.Printf("Dirty blocks:       %d", stats["dirty_blocks"])
			log.Printf("Dirty data:         %d KB", backend.DirtyBytes()/1024)

			log.Println()
			log.Println("=== Dirty Extents (from SEEK_HOLE/SEEK_DATA) ===")
			extents, _ := backend.DirtyExtents()
			if len(extents) == 0 {
				log.Println("No dirty extents (no writes)")
			} else {
				for i, ext := range extents {
					log.Printf("  [%d] offset=%d length=%d (%d KB)",
						i, ext.Offset, ext.Length, ext.Length/1024)
				}
			}

			log.Println()
			log.Println("=== Dirty Extents (from bitmap) ===")
			bitmapExtents := backend.DirtyExtentsFromBitmap()
			if len(bitmapExtents) == 0 {
				log.Println("No dirty extents (no writes)")
			} else {
				for i, ext := range bitmapExtents {
					log.Printf("  [%d] offset=%d length=%d (%d KB)",
						i, ext.Offset, ext.Length, ext.Length/1024)
				}
			}

			log.Println()
			log.Println("Shutting down...")
			return

		case <-ticker.C:
			stats := backend.Stats()
			log.Printf("Stats: base_reads=%d overlay_reads=%d mixed=%d writes=%d dirty_blocks=%d",
				stats["reads_from_base"],
				stats["reads_from_overlay"],
				stats["reads_mixed"],
				stats["writes"],
				stats["dirty_blocks"])
		}
	}
}
