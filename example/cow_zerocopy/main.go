// cow_zerocopy demonstrates copy-on-write with bitmap-based routing.
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
	"sync/atomic"
	"syscall"

	"golang.org/x/sys/unix"

	"github.com/e2b-dev/ublk-go/ublk"
)

// COWBackend implements copy-on-write over a read-only base with a sparse overlay.
type COWBackend struct {
	base      *os.File
	overlay   *os.File
	blockSize int64
	size      int64
	dirty     []uint64 // Bitmap: 1 bit per block
}

// NewCOWBackend creates a new copy-on-write backend.
func NewCOWBackend(base *os.File, overlayPath string, size, blockSize int64) (*COWBackend, error) {
	overlay, err := os.OpenFile(overlayPath, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return nil, fmt.Errorf("failed to create overlay: %w", err)
	}
	if err := overlay.Truncate(size); err != nil {
		overlay.Close()
		return nil, fmt.Errorf("failed to set overlay size: %w", err)
	}

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

func (b *COWBackend) isBlockDirty(blockNum int64) bool {
	idx := blockNum / 64
	bit := uint64(1) << (blockNum % 64)
	return atomic.LoadUint64(&b.dirty[idx])&bit != 0
}

func (b *COWBackend) markBlockDirty(blockNum int64) {
	idx := blockNum / 64
	bit := uint64(1) << (blockNum % 64)
	for {
		old := atomic.LoadUint64(&b.dirty[idx])
		if old&bit != 0 || atomic.CompareAndSwapUint64(&b.dirty[idx], old, old|bit) {
			return
		}
	}
}

func (b *COWBackend) classifyRange(offset, length int64) (allDirty, allClean bool) {
	startBlock := offset / b.blockSize
	endBlock := (offset + length + b.blockSize - 1) / b.blockSize
	allDirty, allClean = true, true

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
	return
}

func (b *COWBackend) ReadAt(p []byte, off int64) (int, error) {
	allDirty, allClean := b.classifyRange(off, int64(len(p)))
	if allClean {
		return b.base.ReadAt(p, off)
	}
	if allDirty {
		return b.overlay.ReadAt(p, off)
	}
	return b.readMixed(p, off)
}

func (b *COWBackend) readMixed(p []byte, off int64) (int, error) {
	totalRead := 0
	remaining := p
	currentOff := off

	for len(remaining) > 0 && currentOff < b.size {
		blockNum := currentOff / b.blockSize
		blockEnd := min((blockNum+1)*b.blockSize, b.size)
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

func (b *COWBackend) WriteAt(p []byte, off int64) (int, error) {
	n, err := b.overlay.WriteAt(p, off)
	if err != nil {
		return n, err
	}
	startBlock := off / b.blockSize
	endBlock := (off + int64(n) + b.blockSize - 1) / b.blockSize
	for blk := startBlock; blk < endBlock; blk++ {
		b.markBlockDirty(blk)
	}
	return n, nil
}

func (b *COWBackend) Flush() error { return b.overlay.Sync() }

// DirtyExtent represents a contiguous dirty region.
type DirtyExtent struct {
	Offset int64
	Length int64
}

// DirtyExtents returns dirty regions using SEEK_HOLE/SEEK_DATA.
func (b *COWBackend) DirtyExtents() ([]DirtyExtent, error) {
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
		extents = append(extents, DirtyExtent{Offset: dataStart, Length: holeStart - dataStart})
		offset = holeStart
	}
	return extents, nil //nolint:nilerr // ENXIO from SEEK_DATA is expected (no more data)
}

// DirtyBlockCount returns dirty block count using popcount.
func (b *COWBackend) DirtyBlockCount() int64 {
	var count int64
	for i := range b.dirty {
		count += int64(bits.OnesCount64(atomic.LoadUint64(&b.dirty[i])))
	}
	return count
}

// DirtyBytes returns total dirty data size.
func (b *COWBackend) DirtyBytes() int64 {
	return b.DirtyBlockCount() * b.blockSize
}

// ExportDiff writes dirty data to writer. Format: [offset:8][length:8][data]...
func (b *COWBackend) ExportDiff(w io.Writer) error {
	extents, err := b.DirtyExtents()
	if err != nil {
		return err
	}
	buf := make([]byte, b.blockSize*256)
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

func (b *COWBackend) Close() error { return b.overlay.Close() }

func main() {
	size := int64(128 * 1024 * 1024)
	blockSize := int64(4096)
	basePath := "/tmp/ublk-cow-base"
	overlayPath := "/tmp/ublk-cow-overlay"

	// Create base image with pattern
	base, err := os.Create(basePath)
	if err != nil {
		log.Fatalf("Failed to create base: %v", err)
	}
	defer os.Remove(basePath)
	defer base.Close()

	if err := base.Truncate(size); err != nil {
		log.Fatalf("Failed to truncate base: %v", err)
	}
	pattern := []byte("BASE_DATA_PATTERN_")
	for off := int64(0); off < size; off += int64(len(pattern)) {
		base.WriteAt(pattern, off)
	}
	base.Sync()
	base.Close()

	base, err = os.Open(basePath)
	if err != nil {
		log.Fatalf("Failed to open base read-only: %v", err)
	}

	backend, err := NewCOWBackend(base, overlayPath, size, blockSize)
	if err != nil {
		log.Fatalf("Failed to create COW backend: %v", err)
	}
	defer os.Remove(overlayPath)
	defer backend.Close()

	config := ublk.DefaultConfig()
	config.Size = uint64(size)
	config.BlockSize = uint64(blockSize)
	config.UserCopy = true

	dev, err := ublk.New(backend, config)
	if err != nil {
		log.Fatalf("Failed to create device: %v", err)
	}
	defer dev.Delete()

	log.Printf("Device: %s", dev.BlockDevicePath())
	log.Printf("Write: sudo dd if=/dev/urandom of=%s bs=4k count=100", dev.BlockDevicePath())
	log.Printf("Read:  sudo xxd %s | head", dev.BlockDevicePath())
	log.Println("Press Ctrl+C to see dirty extents and stop.")

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh

	log.Printf("Dirty: %d blocks, %d KB", backend.DirtyBlockCount(), backend.DirtyBytes()/1024)
	extents, _ := backend.DirtyExtents()
	for i, ext := range extents {
		log.Printf("  Extent %d: offset=%d length=%d", i, ext.Offset, ext.Length)
	}
}
