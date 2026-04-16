// A minimal ublk block device backed by in-memory storage.
//
// Usage (requires root and ublk_drv module):
//
//	modprobe ublk_drv
//	go run ./example
package main

import (
	"fmt"
	"os"
	"os/signal"
	"sync"
	"syscall"

	"github.com/e2b-dev/ublk-go/ublk"
)

const deviceSize = 256 * 1024 * 1024 // 256 MiB

type memBackend struct {
	mu   sync.RWMutex
	data []byte
}

func (m *memBackend) ReadAt(p []byte, off int64) (int, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return copy(p, m.data[off:off+int64(len(p))]), nil
}

func (m *memBackend) WriteAt(p []byte, off int64) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return copy(m.data[off:off+int64(len(p))], p), nil
}

func (m *memBackend) Flush() error { return nil }

func main() {
	backend := &memBackend{data: make([]byte, deviceSize)}

	dev, err := ublk.New(backend, ublk.Config{
		Size: deviceSize,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "ublk.New: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("Block device ready: %s\n", dev.BlockDevicePath())
	fmt.Println("Press Ctrl+C to stop.")

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig

	fmt.Println("\nStopping...")
	if err := dev.Close(); err != nil {
		fmt.Fprintf(os.Stderr, "Close: %v\n", err)
		os.Exit(1)
	}
	fmt.Println("Done.")
}
