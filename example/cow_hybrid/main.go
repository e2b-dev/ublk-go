// cow_hybrid demonstrates hybrid COW with compressed base + file overlay.
//
// Run with: sudo go run ./example/cow_hybrid/
package main

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"errors"
	"io"
	"log"
	"math/bits"
	"os"
	"os/signal"
	"sync"
	"sync/atomic"
	"syscall"

	"golang.org/x/sys/unix"

	"github.com/e2b-dev/ublk-go/ublk"
)

// CompressedBase simulates a compressed base image.
type CompressedBase struct {
	mu        sync.RWMutex
	chunks    []compressedChunk
	chunkSize int64
	totalSize int64
	cache     map[int64][]byte
}

type compressedChunk struct {
	offset         int64
	uncompressedSz int64
	data           []byte
}

func NewCompressedBase(data []byte, chunkSize int64) (*CompressedBase, error) {
	var chunks []compressedChunk
	for off := int64(0); off < int64(len(data)); off += chunkSize {
		end := min(off+chunkSize, int64(len(data)))
		raw := data[off:end]
		var buf bytes.Buffer
		w := zlib.NewWriter(&buf)
		w.Write(raw)
		w.Close()
		chunks = append(chunks, compressedChunk{offset: off, uncompressedSz: int64(len(raw)), data: buf.Bytes()})
	}
	return &CompressedBase{chunks: chunks, chunkSize: chunkSize, totalSize: int64(len(data)), cache: make(map[int64][]byte)}, nil
}

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
		data, err := b.getChunkData(chunkIdx)
		if err != nil {
			return totalRead, err
		}
		readStart := currentOff - chunk.offset
		readEnd := min(chunk.offset+chunk.uncompressedSz, off+int64(len(p))) - chunk.offset
		toRead := int(readEnd - readStart)
		copy(remaining[:toRead], data[readStart:readEnd])
		totalRead += toRead
		remaining = remaining[toRead:]
		currentOff += int64(toRead)
	}
	return totalRead, nil
}

func (b *CompressedBase) getChunkData(idx int) ([]byte, error) {
	chunk := &b.chunks[idx]
	if cached, ok := b.cache[chunk.offset]; ok {
		return cached, nil
	}
	r, _ := zlib.NewReader(bytes.NewReader(chunk.data))
	defer r.Close()
	data := make([]byte, chunk.uncompressedSz)
	io.ReadFull(r, data)
	b.cache[chunk.offset] = data
	return data, nil
}

func (b *CompressedBase) Size() int64  { return b.totalSize }
func (b *CompressedBase) Close() error { return nil }

// HybridCOWBackend combines compressed base + file overlay.
type HybridCOWBackend struct {
	base      *CompressedBase
	overlay   *os.File
	blockSize int64
	size      int64
	dirty     []uint64
}

func NewHybridCOWBackend(base *CompressedBase, overlayPath string, blockSize int64) (*HybridCOWBackend, error) {
	size := base.Size()
	overlay, err := os.OpenFile(overlayPath, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return nil, err
	}
	overlay.Truncate(size)
	numBlocks := (size + blockSize - 1) / blockSize
	return &HybridCOWBackend{base: base, overlay: overlay, blockSize: blockSize, size: size, dirty: make([]uint64, (numBlocks+63)/64)}, nil
}

func (b *HybridCOWBackend) isBlockDirty(blockNum int64) bool {
	return atomic.LoadUint64(&b.dirty[blockNum/64])&(1<<(blockNum%64)) != 0
}

func (b *HybridCOWBackend) markBlockDirty(blockNum int64) {
	idx := blockNum / 64
	bit := uint64(1) << (blockNum % 64)
	for {
		old := atomic.LoadUint64(&b.dirty[idx])
		if old&bit != 0 || atomic.CompareAndSwapUint64(&b.dirty[idx], old, old|bit) {
			return
		}
	}
}

func (b *HybridCOWBackend) ReadAt(p []byte, off int64) (int, error) {
	startBlock := off / b.blockSize
	endBlock := (off + int64(len(p)) + b.blockSize - 1) / b.blockSize
	allDirty, allClean := true, true
	for blk := startBlock; blk < endBlock; blk++ {
		if b.isBlockDirty(blk) {
			allClean = false
		} else {
			allDirty = false
		}
	}
	if allClean {
		return b.base.ReadAt(p, off)
	}
	if allDirty {
		return b.overlay.ReadAt(p, off)
	}
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

func (b *HybridCOWBackend) WriteAt(p []byte, off int64) (int, error) {
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

func (b *HybridCOWBackend) Flush() error                                { return b.overlay.Sync() }
func (b *HybridCOWBackend) Overlay() (*os.File, error)                  { return b.overlay, nil }
func (b *HybridCOWBackend) ReadBaseAt(p []byte, off int64) (int, error) { return b.base.ReadAt(p, off) }
func (b *HybridCOWBackend) Close() error                                { return b.overlay.Close() }

func (b *HybridCOWBackend) ClassifyRange(offset, length int64) (allDirty, allClean bool) {
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

func (b *HybridCOWBackend) DirtyBlockCount() int64 {
	var count int64
	for i := range b.dirty {
		count += int64(bits.OnesCount64(atomic.LoadUint64(&b.dirty[i])))
	}
	return count
}

func (b *HybridCOWBackend) DirtyBytes() int64 { return b.DirtyBlockCount() * b.blockSize }

func (b *HybridCOWBackend) DirtyExtents() []DirtyExtent {
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
	return extents
}

func (b *HybridCOWBackend) ExportDiff(w io.Writer) error {
	buf := make([]byte, 256*1024)
	for _, ext := range b.DirtyExtents() {
		header := make([]byte, 16)
		binary.LittleEndian.PutUint64(header[0:8], uint64(ext.Offset))
		binary.LittleEndian.PutUint64(header[8:16], uint64(ext.Length))
		w.Write(header)
		remaining := ext.Length
		offset := ext.Offset
		for remaining > 0 {
			toRead := min(remaining, int64(len(buf)))
			n, _ := b.overlay.ReadAt(buf[:toRead], offset)
			w.Write(buf[:n])
			remaining -= int64(n)
			offset += int64(n)
		}
	}
	return nil
}

type DirtyExtent struct {
	Offset int64
	Length int64
}

func main() {
	size := int64(64 * 1024 * 1024)
	blockSize := int64(4096)
	chunkSize := int64(64 * 1024)
	overlayPath := "/tmp/ublk-hybrid-overlay"

	baseData := make([]byte, size)
	pattern := []byte("BASE_IMAGE_DATA_")
	for i := range baseData {
		baseData[i] = pattern[i%len(pattern)]
	}

	compressedBase, _ := NewCompressedBase(baseData, chunkSize)
	defer compressedBase.Close()

	backend, err := NewHybridCOWBackend(compressedBase, overlayPath, blockSize)
	if err != nil {
		log.Fatalf("Failed to create backend: %v", err)
	}
	defer os.Remove(overlayPath)
	defer backend.Close()

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
	log.Printf("Write: echo 'DIRTY!' | sudo dd of=%s bs=1 seek=0 conv=notrunc", dev.BlockDevicePath())
	log.Printf("Read:  sudo dd if=%s bs=4k count=10 | xxd | head", dev.BlockDevicePath())
	log.Println("Press Ctrl+C to see dirty extents and stop.")

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh

	log.Printf("Dirty: %d blocks, %d KB", backend.DirtyBlockCount(), backend.DirtyBytes()/1024)
	for i, ext := range backend.DirtyExtents() {
		log.Printf("  Extent %d: offset=%d length=%d", i, ext.Offset, ext.Length)
	}
}
