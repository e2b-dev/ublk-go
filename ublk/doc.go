//go:generate go run ../internal/gen/generate.go

// Package ublk provides a pure Go implementation of the Linux ublk
// (userspace block device) driver interface.
//
// # Overview
//
// The ublk driver allows implementing block devices in userspace. This package
// provides a high-level API for creating and managing ublk devices, handling
// I/O requests via io_uring, and implementing custom storage backends.
//
// # Quick Start
//
// Implement the Backend interface (io.ReaderAt + io.WriterAt) and create a device:
//
//	backend := &MyBackend{data: make([]byte, 1<<30)} // 1GB
//
//	config := ublk.DefaultConfig()
//	config.Size = 1 << 30
//
//	dev, err := ublk.CreateDevice(backend, config)
//	if err != nil {
//	    log.Fatal(err)
//	}
//	defer dev.Delete()
//
//	fmt.Println("Device:", dev.BlockDevicePath())
//
// # Requirements
//
//   - Linux kernel 6.0+ with ublk driver (modprobe ublk_drv)
//   - Root privileges (CAP_SYS_ADMIN)
//
// # Architecture
//
// The package uses io_uring for both control plane (device management) and
// data plane (I/O handling) operations. Each hardware queue runs in its own
// goroutine, processing requests asynchronously.
//
// See the project's ARCHITECTURE.md for detailed design documentation.
package ublk
