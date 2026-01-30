# Building ublk-go

## Requirements

- Go 1.25 or later
- Linux kernel 6.0+ with ublk driver enabled

## Building

The implementation is **pure Go** - no CGO or C dependencies required:

```bash
go build ./...
```

### Optional: Build with CGO

If you prefer to validate constants against system headers:

```bash
# Install liburing-dev (Ubuntu/Debian) or liburing-devel (Fedora)
sudo apt-get install liburing-dev gcc

CGO_ENABLED=1 go build ./...
```

## Testing

### Unit tests (no root required)

```bash
go test ./...
```

### Verbose and coverage

```bash
# Verbose output
go test ./ublk -v

# With coverage report
go test ./ublk -cover

# Race detector
go test ./ublk -race
```

### Run specific test groups

```bash
# Buffer manager tests
go test ./ublk -run BufferManager

# Request parsing tests
go test ./ublk -run Request

# IO worker tests
go test ./ublk -run IOWorker

# Type and constant tests
go test ./ublk -run 'Test.*Size|Test.*Constants'
```

### Benchmarks

```bash
go test ./ublk -bench=. -benchmem
```

## Linting

The project uses [golangci-lint](https://golangci-lint.run/) for code quality:

```bash
# Run all linters
golangci-lint run ./...

# Auto-fix formatting issues
golangci-lint fmt ./...
```

The configuration (`.golangci.yml`) enables:

- **gofumpt**: Stricter formatting than gofmt
- **gci**: Import ordering (stdlib, external, local)
- **revive**: Comprehensive linting
- **staticcheck**: Advanced static analysis
- **errorlint**: Error wrapping checks
- **gocritic**: Opinionated code review suggestions

### Integration tests (requires root)

Full device lifecycle and mmap tests need root privileges and a kernel with ublk enabled:

```bash
# Load the ublk driver
sudo modprobe ublk_drv

# Verify the control device exists
ls -l /dev/ublk-control

# Run all integration tests
sudo -E go test -tags=integration -v ./ublk -run Integration

# Run specific integration tests
sudo -E go test -tags=integration -v ./ublk -run IntegrationMmap
sudo -E go test -tags=integration -v ./ublk -run IntegrationDirectIO
sudo -E go test -tags=integration -v ./ublk -run IntegrationConcurrent
```

### Mmap example (requires root)

The mmap_test example demonstrates memory-mapping a ublk device:

```bash
# Run the mmap test example
sudo go run ./example/mmap_test/

# Run the basic example
sudo go run ./example/main.go
```

## Troubleshooting

### ublk driver not loaded

```bash
# Check if module is available
modprobe -n ublk_drv

# Load the driver
sudo modprobe ublk_drv

# Verify control device exists
ls -l /dev/ublk-control
```

### Permission denied

ublk requires root privileges to create block devices:

```bash
sudo go run ./example/main.go
```

### CGO build issues (optional CGO mode only)

If building with `CGO_ENABLED=1` and encountering issues:

1. Install liburing-dev: `sudo apt-get install liburing-dev gcc`
2. Check pkg-config: `pkg-config --modversion liburing`
3. Use pure Go mode instead: `CGO_ENABLED=0 go build ./...`
