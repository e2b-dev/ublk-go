.PHONY: build test test-unit test-integration lint fmt clean bench

GO ?= go

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
	golangci-lint run ./...

# Format code
fmt:
	golangci-lint fmt ./... || true
	gofmt -w .

# Verify everything (unit tests only)
check: fmt lint test-unit

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
