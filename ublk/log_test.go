package ublk

import (
	"bytes"
	"log"
	"strings"
	"testing"
)

func TestLogf(t *testing.T) {
	// Save original logger
	origLogger := DefaultLogger
	defer func() { DefaultLogger = origLogger }()

	// Test with custom logger
	var buf bytes.Buffer
	DefaultLogger = log.New(&buf, "", 0)

	logf("test message %d", 42)

	output := buf.String()
	if !strings.Contains(output, "test message 42") {
		t.Errorf("Expected output to contain 'test message 42', got: %s", output)
	}
}

func TestLogfNilLogger(t *testing.T) {
	// Save original logger
	origLogger := DefaultLogger
	defer func() { DefaultLogger = origLogger }()

	// Test with nil logger (should not panic)
	DefaultLogger = nil
	logf("this should not panic")
}

func TestDefaultLoggerNotNil(t *testing.T) {
	if DefaultLogger == nil {
		t.Error("DefaultLogger should not be nil by default")
	}
}
