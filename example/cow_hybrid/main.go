// cow_hybrid demonstrates a realistic COW setup where:
// - Base: In-memory data (simulates compressed/chunked storage, decompressed on demand)
// - Overlay: File (can use zero-copy for writes and dirty block reads)
//
// This represents a common pattern where the base image is:
// - Stored compressed and decompressed on-demand
// - Retrieved from network/object storage
// - Memory-mapped but needs transformation
//
// Key insight:
// - Base reads MUST be user-copy (data is in memory, not a file fd)
// - Overlay reads/writes CAN be zero-copy (it's a file)
//
// For the base interface, ReadAt(p, off) is optimal because it allows
// decompressing directly into the destination buffer (single copy).
// A SliceAt() that returns internal data would require an extra copy.
//
// Run with: sudo go run ./example/cow_hybrid/
package main

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"errors"
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

// =============================================================================
// Base Image Interface
// =============================================================================

// BaseImage provides read-only access to the base data.
// Implementations can decompress, fetch from network, etc.
// ReadAt is the optimal interface: decompress directly into the caller's buffer.
type BaseImage interface {
	// ReadAt reads data into p at offset off.
	// For compressed sources, this should decompress directly into p.
	// This minimizes copies: decompress directly into ublk buffer.
	ReadAt(p []byte, off int64) (int, error)

	// Size returns the total size of the uncompressed image.
	Size() int64

	// Close releases resources.
	Close() error
}

// =============================================================================
// Compressed Base Implementation (simulates real-world scenario)
// =============================================================================

// CompressedChunk represents a compressed region of the base image.
type CompressedChunk struct {
	offset         int64  // Uncompressed offset
	uncompressedSz int64  // Uncompressed size
	data           []byte // Compressed data
}

// CompressedBase simulates a compressed base image that decompresses on demand.
// In reality, this could be:
// - Chunks from object storage (S3, GCS)
// - A local compressed file
// - Network-fetched blocks.
type CompressedBase struct {
	mu        sync.RWMutex
	chunks    []CompressedChunk
	chunkSize int64
	totalSize int64

	// Cache for recently decompressed chunks (LRU would be better in production).
	cache      map[int64][]byte
	cacheHits  atomic.Uint64
	cacheMiss  atomic.Uint64
	decompress atomic.Uint64
}

// NewCompressedBase creates a compressed base from raw data.
// It compresses the data in chunks to simulate real storage.
func NewCompressedBase(data []byte, chunkSize int64) (*CompressedBase, error) {
	numChunks := (int64(len(data)) + chunkSize - 1) / chunkSize
	chunks := make([]CompressedChunk, 0, numChunks)

	for off := int64(0); off < int64(len(data)); off += chunkSize {
		end := min(off+chunkSize, int64(len(data)))
		raw := data[off:end]

		// Compress this chunk
		var buf bytes.Buffer
		w := zlib.NewWriter(&buf)
		if _, err := w.Write(raw); err != nil {
			return nil, err
		}
		if err := w.Close(); err != nil {
			return nil, err
		}

		chunks = append(chunks, CompressedChunk{
			offset:         off,
			uncompressedSz: int64(len(raw)),
			data:           buf.Bytes(),
		})
	}

	log.Printf("Compressed %d bytes into %d chunks (%.1f%% ratio)",
		len(data), len(chunks), float64(totalCompressedSize(chunks))*100/float64(len(data)))

	return &CompressedBase{
		chunks:    chunks,
		chunkSize: chunkSize,
		totalSize: int64(len(data)),
		cache:     make(map[int64][]byte),
	}, nil
}

func totalCompressedSize(chunks []CompressedChunk) int64 {
	var total int64
	for _, c := range chunks {
		total += int64(len(c.data))
	}
	return total
}

// ReadAt implements BaseImage.ReadAt.
// Decompresses directly into the provided buffer (optimal: single copy).
func (b *CompressedBase) ReadAt(p []byte, off int64) (int, error) {
	b.mu.RLock()
	defer b.mu.RUnlock()

	if off >= b.totalSize {
		return 0, io.EOF
	}

	totalRead := 0
	remaining := p
	currentOff := off

	for len(remaining) > 0 && currentOff < b.totalSize {
		chunkIdx := int(currentOff / b.chunkSize)
		if chunkIdx >= len(b.chunks) {
			break
		}

		chunk := &b.chunks[chunkIdx]
		chunkStart := chunk.offset
		chunkEnd := chunkStart + chunk.uncompressedSz

		// Get decompressed chunk data (from cache or decompress).
		decompressed, err := b.getChunkData(chunkIdx)
		if err != nil {
			return totalRead, err
		}

		// Calculate offsets within chunk
		readStart := currentOff - chunkStart
		readEnd := min(chunkEnd, off+int64(len(p))) - chunkStart
		toRead := int(readEnd - readStart)

		// Copy to destination buffer (this is the unavoidable copy)
		copy(remaining[:toRead], decompressed[readStart:readEnd])

		totalRead += toRead
		remaining = remaining[toRead:]
		currentOff += int64(toRead)
	}

	return totalRead, nil
}

// getChunkData returns decompressed chunk data, using cache.
func (b *CompressedBase) getChunkData(chunkIdx int) ([]byte, error) {
	chunk := &b.chunks[chunkIdx]

	// Check cache
	if cached, ok := b.cache[chunk.offset]; ok {
		b.cacheHits.Add(1)
		return cached, nil
	}
	b.cacheMiss.Add(1)

	// Decompress
	b.decompress.Add(1)
	r, err := zlib.NewReader(bytes.NewReader(chunk.data))
	if err != nil {
		return nil, fmt.Errorf("zlib reader: %w", err)
	}
	defer r.Close()

	decompressed := make([]byte, chunk.uncompressedSz)
	if _, err := io.ReadFull(r, decompressed); err != nil {
		return nil, fmt.Errorf("decompress: %w", err)
	}

	// Cache (simple: just store, no eviction).
	b.cache[chunk.offset] = decompressed

	return decompressed, nil
}

func (b *CompressedBase) Size() int64 {
	return b.totalSize
}

func (b *CompressedBase) Close() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.cache = nil
	return nil
}

func (b *CompressedBase) Stats() map[string]uint64 {
	return map[string]uint64{
		"cache_hits":     b.cacheHits.Load(),
		"cache_misses":   b.cacheMiss.Load(),
		"decompressions": b.decompress.Load(),
	}
}

// =============================================================================
// Hybrid COW Backend
// =============================================================================

// HybridCOWBackend combines:
// - BaseImage (in-memory, potentially compressed) for clean blocks
// - File overlay for dirty blocks (can use zero-copy).
type HybridCOWBackend struct {
	mu sync.RWMutex

	base       BaseImage
	overlay    *os.File
	blockSize  int64
	size       int64
	dirty      []uint64
	dirtyCount atomic.Int64

	// Stats
	readsBase    atomic.Uint64
	readsOverlay atomic.Uint64
	readsMixed   atomic.Uint64
	writes       atomic.Uint64
}

// NewHybridCOWBackend creates a hybrid COW backend.
func NewHybridCOWBackend(base BaseImage, overlayPath string, blockSize int64) (*HybridCOWBackend, error) {
	size := base.Size()

	// Create sparse overlay
	overlay, err := os.OpenFile(overlayPath, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return nil, fmt.Errorf("create overlay: %w", err)
	}
	if err := overlay.Truncate(size); err != nil {
		overlay.Close()
		return nil, fmt.Errorf("truncate overlay: %w", err)
	}

	numBlocks := (size + blockSize - 1) / blockSize
	bitmapWords := (numBlocks + 63) / 64

	return &HybridCOWBackend{
		base:      base,
		overlay:   overlay,
		blockSize: blockSize,
		size:      size,
		dirty:     make([]uint64, bitmapWords),
	}, nil
}

func (b *HybridCOWBackend) isBlockDirty(blockNum int64) bool {
	idx := blockNum / 64
	bit := uint64(1) << (blockNum % 64)
	return b.dirty[idx]&bit != 0
}

func (b *HybridCOWBackend) markBlockDirty(blockNum int64) bool {
	idx := blockNum / 64
	bit := uint64(1) << (blockNum % 64)
	wasClean := b.dirty[idx]&bit == 0
	b.dirty[idx] |= bit
	return wasClean
}

// ReadAt implements io.ReaderAt with proper routing.
func (b *HybridCOWBackend) ReadAt(p []byte, off int64) (int, error) {
	b.mu.RLock()
	defer b.mu.RUnlock()

	startBlock := off / b.blockSize
	endBlock := (off + int64(len(p)) + b.blockSize - 1) / b.blockSize

	// Check dirty state
	allDirty := true
	allClean := true
	for blk := startBlock; blk < endBlock; blk++ {
		if b.isBlockDirty(blk) {
			allClean = false
		} else {
			allDirty = false
		}
	}

	// Fast paths
	if allClean {
		b.readsBase.Add(1)
		// User-copy from base (unavoidable - base is in memory)
		return b.base.ReadAt(p, off)
	}
	if allDirty {
		b.readsOverlay.Add(1)
		// Could be zero-copy if we had per-request routing in io_worker
		return b.overlay.ReadAt(p, off)
	}

	// Mixed: read block by block
	b.readsMixed.Add(1)
	return b.readMixed(p, off)
}

func (b *HybridCOWBackend) readMixed(p []byte, off int64) (int, error) {
	totalRead := 0
	remaining := p
	currentOff := off

	for len(remaining) > 0 && currentOff < b.size {
		blockNum := currentOff / b.blockSize
		blockEnd := min((blockNum+1)*b.blockSize, b.size)
		toRead := int(min(blockEnd-currentOff, int64(len(remaining))))

		var n int
		var err error
		if b.isBlockDirty(blockNum) {
			n, err = b.overlay.ReadAt(remaining[:toRead], currentOff)
		} else {
			n, err = b.base.ReadAt(remaining[:toRead], currentOff)
		}
		totalRead += n
		if err != nil && !errors.Is(err, io.EOF) {
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

// WriteAt always writes to overlay.
func (b *HybridCOWBackend) WriteAt(p []byte, off int64) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	n, err := b.overlay.WriteAt(p, off)
	if err != nil {
		return n, err
	}

	// Mark blocks dirty
	startBlock := off / b.blockSize
	endBlock := (off + int64(n) + b.blockSize - 1) / b.blockSize
	newDirty := int64(0)
	for blk := startBlock; blk < endBlock; blk++ {
		if b.markBlockDirty(blk) {
			newDirty++
		}
	}
	b.dirtyCount.Add(newDirty)
	b.writes.Add(1)

	return n, nil
}

// Flush syncs overlay.
func (b *HybridCOWBackend) Flush() error {
	return b.overlay.Sync()
}

// Overlay implements ublk.COWBackend.
// Returns the overlay file for zero-copy I/O.
func (b *HybridCOWBackend) Overlay() (*os.File, error) {
	return b.overlay, nil
}

// ClassifyRange implements ublk.COWBackend.
// Returns whether the range is all dirty, all clean, or mixed.
func (b *HybridCOWBackend) ClassifyRange(offset, length int64) (allDirty, allClean bool) {
	b.mu.RLock()
	defer b.mu.RUnlock()

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
		if !allDirty && !allClean {
			return false, false
		}
	}

	return allDirty, allClean
}

// ReadBaseAt implements ublk.COWBackend.
// Reads from the base (clean blocks).
func (b *HybridCOWBackend) ReadBaseAt(p []byte, off int64) (int, error) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	b.readsBase.Add(1)
	return b.base.ReadAt(p, off)
}

// DirtyExtents returns dirty regions using SEEK_HOLE/SEEK_DATA.
func (b *HybridCOWBackend) DirtyExtents() ([]DirtyExtent, error) {
	b.mu.RLock()
	defer b.mu.RUnlock()

	var extents []DirtyExtent
	offset := int64(0)
	fd := int(b.overlay.Fd())

	for offset < b.size {
		dataStart, err := unix.Seek(fd, offset, unix.SEEK_DATA)
		if err != nil {
			break
		}
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

// DirtyExtentsFromBitmap returns dirty regions from bitmap.
func (b *HybridCOWBackend) DirtyExtentsFromBitmap() []DirtyExtent {
	b.mu.RLock()
	defer b.mu.RUnlock()

	var extents []DirtyExtent
	inExtent := false
	var extentStart int64
	numBlocks := b.size / b.blockSize

	for wordIdx, word := range b.dirty {
		if word == 0 {
			if inExtent {
				extentEnd := int64(wordIdx) * 64 * b.blockSize
				extents = append(extents, DirtyExtent{
					Offset: extentStart,
					Length: extentEnd - extentStart,
				})
				inExtent = false
			}
			continue
		}

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

	if inExtent {
		extents = append(extents, DirtyExtent{
			Offset: extentStart,
			Length: b.size - extentStart,
		})
	}
	return extents
}

func (b *HybridCOWBackend) DirtyBlockCount() int64 {
	b.mu.RLock()
	defer b.mu.RUnlock()
	var count int64
	for _, word := range b.dirty {
		count += int64(bits.OnesCount64(word))
	}
	return count
}

func (b *HybridCOWBackend) DirtyBytes() int64 {
	return b.DirtyBlockCount() * b.blockSize
}

// ExportDiff writes dirty data in a simple format.
func (b *HybridCOWBackend) ExportDiff(w io.Writer) error {
	extents, err := b.DirtyExtents()
	if err != nil {
		return err
	}

	b.mu.RLock()
	defer b.mu.RUnlock()

	buf := make([]byte, 256*1024)
	for _, ext := range extents {
		header := make([]byte, 16)
		binary.LittleEndian.PutUint64(header[0:8], uint64(ext.Offset))
		binary.LittleEndian.PutUint64(header[8:16], uint64(ext.Length))
		if _, err := w.Write(header); err != nil {
			return err
		}

		remaining := ext.Length
		offset := ext.Offset
		for remaining > 0 {
			toRead := min(remaining, int64(len(buf)))
			n, err := b.overlay.ReadAt(buf[:toRead], offset)
			if err != nil && !errors.Is(err, io.EOF) {
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

func (b *HybridCOWBackend) Stats() map[string]uint64 {
	return map[string]uint64{
		"reads_base":    b.readsBase.Load(),
		"reads_overlay": b.readsOverlay.Load(),
		"reads_mixed":   b.readsMixed.Load(),
		"writes":        b.writes.Load(),
		"dirty_blocks":  uint64(b.dirtyCount.Load()),
	}
}

func (b *HybridCOWBackend) Close() error {
	return b.overlay.Close()
}

// DirtyExtent represents a dirty region.
type DirtyExtent struct {
	Offset int64
	Length int64
}

// =============================================================================
// Main
// =============================================================================

func main() {
	log.SetFlags(log.Ltime | log.Lmicroseconds)

	// Configuration
	size := int64(64 * 1024 * 1024) // 64MB device
	blockSize := int64(4096)        // 4KB blocks
	chunkSize := int64(64 * 1024)   // 64KB compression chunks
	overlayPath := "/tmp/ublk-hybrid-overlay"

	// Create base image with pattern (simulates real data).
	log.Println("Creating base image data...")
	baseData := make([]byte, size)
	pattern := []byte("BASE_IMAGE_DATA_")
	for i := range baseData {
		baseData[i] = pattern[i%len(pattern)]
	}

	// Create compressed base (simulates on-demand decompression).
	log.Println("Compressing base image...")
	compressedBase, err := NewCompressedBase(baseData, chunkSize)
	if err != nil {
		log.Fatalf("Failed to create compressed base: %v", err)
	}
	defer compressedBase.Close()

	// Create hybrid COW backend.
	log.Println("Creating hybrid COW backend...")
	backend, err := NewHybridCOWBackend(compressedBase, overlayPath, blockSize)
	if err != nil {
		log.Fatalf("Failed to create backend: %v", err)
	}
	defer os.Remove(overlayPath)
	defer backend.Close()

	log.Printf("Base: %d MB compressed, %d chunks", size/1024/1024, size/chunkSize)
	log.Printf("Overlay: %s (sparse)", overlayPath)
	log.Printf("Block size: %d, Total blocks: %d", blockSize, size/blockSize)

	// Create ublk device with COW mode for optimal performance:
	// - Clean block reads: user-copy from base (unavoidable - in memory)
	// - Dirty block reads: zero-copy from overlay file
	// - Writes: zero-copy to overlay file
	config := ublk.DefaultConfig()
	config.Size = uint64(size)
	config.BlockSize = uint64(blockSize)
	config.COW = true

	dev, err := ublk.New(backend, config)
	if err != nil {
		log.Fatalf("Failed to create device: %v", err)
	}
	defer dev.Delete()

	log.Printf("Device: %s", dev.BlockDevicePath())
	log.Println()
	log.Println("Architecture:")
	log.Println("  Base: In-memory, compressed (decompressed on-demand)")
	log.Println("  Overlay: Sparse file (dirty blocks)")
	log.Println()
	log.Println("COW mode enabled:")
	log.Println("  - Base reads: user-copy (data in memory, not file fd)")
	log.Println("  - Overlay reads: ZERO-COPY via io_uring")
	log.Println("  - Writes: ZERO-COPY via io_uring")
	log.Println()
	log.Println("Test commands:")
	log.Printf("  sudo dd if=%s bs=4k count=10 | xxd | head -20  # Read base", dev.BlockDevicePath())
	log.Printf("  echo 'DIRTY!' | sudo dd of=%s bs=1 seek=0 conv=notrunc", dev.BlockDevicePath())
	log.Printf("  sudo dd if=%s bs=4k count=10 | xxd | head -5   # Now reads overlay", dev.BlockDevicePath())
	log.Println()
	log.Println("Press Ctrl+C to see stats and stop.")

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

			cowStats := backend.Stats()
			baseStats := compressedBase.Stats()

			log.Println("COW Backend:")
			log.Printf("  Reads from base:    %d", cowStats["reads_base"])
			log.Printf("  Reads from overlay: %d", cowStats["reads_overlay"])
			log.Printf("  Reads mixed:        %d", cowStats["reads_mixed"])
			log.Printf("  Writes:             %d", cowStats["writes"])
			log.Printf("  Dirty blocks:       %d", cowStats["dirty_blocks"])
			log.Printf("  Dirty data:         %d KB", backend.DirtyBytes()/1024)

			log.Println("Compressed Base:")
			log.Printf("  Cache hits:    %d", baseStats["cache_hits"])
			log.Printf("  Cache misses:  %d", baseStats["cache_misses"])
			log.Printf("  Decompressions: %d", baseStats["decompressions"])

			log.Println()
			log.Println("=== Dirty Extents ===")
			extents, _ := backend.DirtyExtents()
			if len(extents) == 0 {
				log.Println("No dirty extents")
			} else {
				for i, ext := range extents {
					log.Printf("  [%d] offset=%d length=%d", i, ext.Offset, ext.Length)
				}
			}

			log.Println()
			log.Println("Shutting down...")
			return

		case <-ticker.C:
			cowStats := backend.Stats()
			baseStats := compressedBase.Stats()
			log.Printf("Stats: base_reads=%d overlay_reads=%d writes=%d decompressions=%d",
				cowStats["reads_base"], cowStats["reads_overlay"],
				cowStats["writes"], baseStats["decompressions"])
		}
	}
}
