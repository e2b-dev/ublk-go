// Autonomous end-to-end probe for ublk-go.
//
// Creates a ublk device, formats it with ext4, mounts it, exercises
// several I/O paths (single write, syncfs, drop-caches readback,
// concurrent writers), then unmounts and closes. Each step runs with
// its own timeout so hangs surface as failures instead of blocking
// forever.
//
// Intended for unattended use by CI, scripts, or AI agents. Requires
// root and a loaded ublk_drv module:
//
//	sudo modprobe ublk_drv
//	sudo go run ./example/probe
//
// Exit code 0 = everything passed. Non-zero = first step that failed
// (the whole run bails on first failure and cleans up best-effort).
package main

import (
	"bytes"
	"crypto/rand"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"sync"
	"sync/atomic"
	"time"

	"github.com/e2b-dev/ublk-go/ublk"
)

const devSize = 128 * 1024 * 1024 // 128 MiB

type loggingBackend struct {
	mu         sync.RWMutex
	data       []byte
	reads      atomic.Int64
	writes     atomic.Int64
	readBytes  atomic.Int64
	writeBytes atomic.Int64
}

func (b *loggingBackend) ReadAt(p []byte, off int64) (int, error) {
	b.reads.Add(1)
	b.readBytes.Add(int64(len(p)))
	b.mu.RLock()
	defer b.mu.RUnlock()
	return copy(p, b.data[off:off+int64(len(p))]), nil
}

func (b *loggingBackend) WriteAt(p []byte, off int64) (int, error) {
	b.writes.Add(1)
	b.writeBytes.Add(int64(len(p)))
	b.mu.Lock()
	defer b.mu.Unlock()
	return copy(b.data[off:off+int64(len(p))], p), nil
}

type probe struct {
	backend    *loggingBackend
	dev        *ublk.Device
	mountpoint string
	devPath    string
	mounted    bool
	timeout    time.Duration
}

func main() {
	timeout := flag.Duration("step-timeout", 30*time.Second, "per-step timeout")
	flag.Parse()

	log.SetFlags(log.Ltime | log.Lmicroseconds)

	if os.Getuid() != 0 {
		log.Fatal("probe must be run as root")
	}

	p := &probe{
		backend: &loggingBackend{data: make([]byte, devSize)},
		timeout: *timeout,
	}

	err := p.run()
	p.cleanup()
	if err != nil {
		log.Printf("FAIL: %v", err)
		os.Exit(1)
	}
	log.Printf("PASS: all steps succeeded")
}

func (p *probe) run() error {
	steps := []struct {
		name string
		fn   func() error
	}{
		{"create device", p.createDevice},
		{"mkfs.ext4", p.mkfs},
		{"mount", p.mountFS},
		{"single write + syncfs", p.singleWrite},
		{"drop caches + readback", p.dropAndVerify},
		{"concurrent writers", p.concurrent},
		{"remount (journal replay)", p.remount},
		{"unmount", p.umount},
		{"close device", p.closeDevice},
		{"verify device path gone", p.verifyGone},
	}
	for _, s := range steps {
		if err := p.step(s.name, s.fn); err != nil {
			return err
		}
	}
	return nil
}

func (p *probe) step(name string, fn func() error) error {
	start := time.Now()
	log.Printf("=== %s", name)

	done := make(chan error, 1)
	go func() { done <- fn() }()

	select {
	case err := <-done:
		if err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		p.stats(time.Since(start))
		return nil
	case <-time.After(p.timeout):
		// Dump goroutines so the user (or agent) can diagnose.
		panic(fmt.Sprintf("step %q hung for %v — dumping goroutines", name, p.timeout))
	}
}

func (p *probe) stats(elapsed time.Duration) {
	b := p.backend
	log.Printf("    ok in %-10v   reads=%-5d (%5d KiB)  writes=%-5d (%5d KiB)",
		elapsed.Truncate(time.Microsecond),
		b.reads.Load(), b.readBytes.Load()/1024,
		b.writes.Load(), b.writeBytes.Load()/1024)
}

func (p *probe) cleanup() {
	if p.mounted {
		_ = exec.Command("umount", p.mountpoint).Run()
		p.mounted = false
	}
	if p.mountpoint != "" {
		_ = os.Remove(p.mountpoint)
	}
	if p.dev != nil {
		_ = p.dev.Close()
		p.dev = nil
	}
}

// --- steps ---

func (p *probe) createDevice() error {
	dev, err := ublk.New(p.backend, devSize)
	if err != nil {
		return err
	}
	p.dev = dev
	p.devPath = dev.BlockDevicePath()
	log.Printf("    created %s", p.devPath)

	fi, err := os.Stat(p.devPath)
	if err != nil {
		return fmt.Errorf("stat %s: %w", p.devPath, err)
	}
	if fi.Mode()&os.ModeDevice == 0 {
		return fmt.Errorf("%s is not a block device (mode=%v)", p.devPath, fi.Mode())
	}
	return nil
}

func (p *probe) mkfs() error {
	return runCmd("mkfs.ext4", "-q", "-F", p.devPath)
}

func (p *probe) mountFS() error {
	mp, err := os.MkdirTemp("", "ublk-probe-*")
	if err != nil {
		return err
	}
	p.mountpoint = mp
	if err := runCmd("mount", p.devPath, mp); err != nil {
		return err
	}
	p.mounted = true
	log.Printf("    mounted at %s", mp)
	return nil
}

func (p *probe) singleWrite() error {
	content := bytes.Repeat([]byte("probe write: ublk-go smoke test\n"), 256)
	f := p.mountpoint + "/probe.txt"
	if err := os.WriteFile(f, content, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", f, err)
	}
	// syncfs scoped to our mount: flush dirty pages through to the
	// backend without dragging in other host filesystems.
	if err := runCmd("sync", "-f", p.mountpoint); err != nil {
		return fmt.Errorf("syncfs: %w", err)
	}
	return nil
}

func (p *probe) dropAndVerify() error {
	if err := os.WriteFile("/proc/sys/vm/drop_caches", []byte("3"), 0); err != nil {
		return fmt.Errorf("drop_caches: %w", err)
	}
	want, err := os.ReadFile(p.mountpoint + "/probe.txt")
	if err != nil {
		return fmt.Errorf("read probe.txt: %w", err)
	}
	if !bytes.HasPrefix(want, []byte("probe write:")) {
		return errors.New("probe.txt did not round-trip correctly")
	}
	return nil
}

func (p *probe) concurrent() error {
	const (
		workers = 8
		files   = 16
		size    = 64 * 1024
	)
	var wg sync.WaitGroup
	errCh := make(chan error, workers)
	for w := range workers {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			buf := make([]byte, size)
			if _, err := rand.Read(buf); err != nil {
				errCh <- err
				return
			}
			for i := range files {
				name := fmt.Sprintf("%s/w%d-f%d.bin", p.mountpoint, id, i)
				if err := os.WriteFile(name, buf, 0o644); err != nil {
					errCh <- fmt.Errorf("write %s: %w", name, err)
					return
				}
			}
		}(w)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		if err != nil {
			return err
		}
	}
	return runCmd("sync", "-f", p.mountpoint)
}

func (p *probe) remount() error {
	if err := runCmd("umount", p.mountpoint); err != nil {
		return err
	}
	p.mounted = false
	if err := runCmd("mount", p.devPath, p.mountpoint); err != nil {
		return err
	}
	p.mounted = true

	// File written before remount must still be there.
	if _, err := os.Stat(p.mountpoint + "/probe.txt"); err != nil {
		return fmt.Errorf("probe.txt missing after remount: %w", err)
	}
	return nil
}

func (p *probe) umount() error {
	if err := runCmd("umount", p.mountpoint); err != nil {
		return err
	}
	p.mounted = false
	return nil
}

func (p *probe) closeDevice() error {
	err := p.dev.Close()
	p.dev = nil
	return err
}

func (p *probe) verifyGone() error {
	// Give the kernel a beat for del_gendisk to publish the removal.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(p.devPath); errors.Is(err, os.ErrNotExist) {
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	return fmt.Errorf("%s still present after Close()", p.devPath)
}

func runCmd(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}
