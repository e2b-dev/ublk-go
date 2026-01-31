.PHONY: all build test test-unit test-integration lint fmt clean bench security generate help

GO ?= go
GOLANGCI_LINT ?= golangci-lint

# Default target
all: check

# Build all packages
build:
	$(GO) build ./...

# Run unit tests only (no root required)
test-unit:
	$(GO) test ./...

# Alias for test-unit
test: test-unit

# Run tests with race detector
test-race:
	$(GO) test ./... -race

# Run tests with coverage
test-cover:
	$(GO) test ./ublk -cover -coverprofile=coverage.out
	$(GO) tool cover -html=coverage.out -o coverage.html
	@echo "Coverage report: coverage.html"

# Run integration tests (requires root and ublk module)
# Usage: sudo make test-integration
test-integration:
	@if [ "$$(id -u)" -ne 0 ]; then \
		echo "ERROR: Integration tests require root. Run: sudo make test-integration"; \
		exit 1; \
	fi
	@if ! lsmod | grep -q ublk_drv; then \
		echo "Loading ublk_drv module..."; \
		modprobe ublk_drv || { echo "ERROR: Failed to load ublk_drv module"; exit 1; }; \
	fi
	$(GO) test -tags=integration -v ./ublk -run=Integration -timeout=120s

# Run all tests including integration (requires root)
test-all: test-unit
	@echo "Running integration tests (requires root)..."
	@if [ "$$(id -u)" -eq 0 ]; then \
		$(MAKE) test-integration; \
	else \
		echo "Skipping integration tests (not root). Run: sudo make test-integration"; \
	fi

# Run benchmarks
bench:
	$(GO) test ./ublk -bench=. -benchmem -run=^$$

# Run linters
lint:
	$(GOLANGCI_LINT) run ./...

# Format code
fmt:
	$(GOLANGCI_LINT) fmt ./... || true
	gofmt -w .

# Generate code (requires liburing-dev for CGO)
# This regenerates ublk/iouring_linux.go from kernel headers
generate:
	@echo "Regenerating io_uring constants (requires liburing-dev)..."
	$(GO) run ./internal/gen/generate.go

# Run security checks (govulncheck)
security:
	@command -v govulncheck >/dev/null 2>&1 || { \
		echo "Installing govulncheck..."; \
		$(GO) install golang.org/x/vuln/cmd/govulncheck@latest; \
	}
	govulncheck ./...

# Verify everything (unit tests only)
check: fmt lint test-unit

# Full CI pipeline
ci: check security

# Clean build artifacts
clean:
	rm -f coverage.out coverage.html mmap_test
	$(GO) clean ./...

# Build mmap test binary
build-mmap-test:
	$(GO) build -o mmap_test ./example/mmap_test/

# Run mmap test (requires root)
# Usage: sudo make run-mmap-test
run-mmap-test: build-mmap-test
	@if [ "$$(id -u)" -ne 0 ]; then \
		echo "ERROR: Requires root. Run: sudo make run-mmap-test"; \
		exit 1; \
	fi
	./mmap_test

# Quick check that ublk is working (requires root)
verify:
	@if [ "$$(id -u)" -ne 0 ]; then \
		echo "ERROR: Requires root. Run: sudo make verify"; \
		exit 1; \
	fi
	@echo "Checking ublk module..."
	@lsmod | grep -q ublk_drv || modprobe ublk_drv
	@echo "Running quick integration test..."
	$(GO) test -tags=integration -v ./ublk -run=TestIntegrationDeviceLifecycle -timeout=30s
	@echo "SUCCESS: ublk-go is working correctly!"

# Show help
help:
	@echo "ublk-go Makefile targets:"
	@echo ""
	@echo "  build            Build all packages"
	@echo "  test             Run unit tests"
	@echo "  test-race        Run tests with race detector"
	@echo "  test-cover       Run tests with coverage report"
	@echo "  test-integration Run integration tests (requires root)"
	@echo "  test-all         Run all tests"
	@echo "  bench            Run benchmarks"
	@echo "  lint             Run linters"
	@echo "  fmt              Format code"
	@echo "  security         Run security vulnerability check"
	@echo "  generate         Regenerate io_uring constants (requires liburing-dev)"
	@echo "  check            Run fmt, lint, and test"
	@echo "  ci               Full CI pipeline (check + security)"
	@echo "  clean            Clean build artifacts"
	@echo "  verify           Quick integration test (requires root)"
	@echo "  help             Show this help"
