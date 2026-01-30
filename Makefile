.PHONY: build test lint fmt clean bench

# Build all packages
build:
	go build ./...

# Run all tests
test:
	go test ./...

# Run tests with race detector
test-race:
	go test ./... -race

# Run tests with coverage
test-cover:
	go test ./ublk -cover -coverprofile=coverage.out
	go tool cover -html=coverage.out -o coverage.html

# Run benchmarks
bench:
	go test ./ublk -bench=. -benchmem -run=^$$

# Run linters
lint:
	golangci-lint run ./...

# Format code
fmt:
	golangci-lint fmt ./...
	gofmt -w .

# Verify everything
check: fmt lint test

# Clean build artifacts
clean:
	rm -f coverage.out coverage.html
	go clean ./...

# Run example (requires root)
run-example:
	sudo go run ./example/main.go
