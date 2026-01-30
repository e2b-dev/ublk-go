package ublk

import (
	"log"
	"os"
)

// Logger defines the interface for logging within ublk.
// Users can replace DefaultLogger with their own implementation.
type Logger interface {
	Printf(format string, v ...interface{})
}

// DefaultLogger is the default logger used by ublk.
// Set to nil to disable logging.
var DefaultLogger Logger = log.New(os.Stderr, "[ublk] ", log.LstdFlags)

func logf(format string, v ...interface{}) {
	if DefaultLogger != nil {
		DefaultLogger.Printf(format, v...)
	}
}
