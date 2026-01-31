# Contributing to ublk-go

Thank you for your interest in contributing to ublk-go!

## Development Setup

1. **Requirements**
   - Go 1.25.6 or later
   - Linux with kernel 6.0+ (ublk driver support)
   - Root privileges for integration tests

2. **Clone and build**

   ```bash
   git clone https://github.com/e2b-dev/ublk-go.git
   cd ublk-go
   make build
   ```

3. **Run tests**

   ```bash
   # Unit tests (no root required)
   make test

   # Integration tests (requires root)
   sudo make test-integration
   ```

## Making Changes

1. Fork the repository
2. Create a feature branch (`git checkout -b feature/my-feature`)
3. Make your changes
4. Run tests and linters:

   ```bash
   make check
   ```

5. Commit your changes with a clear message
6. Push to your fork and create a Pull Request

## Code Style

- Run `make fmt` to format code
- Run `make lint` to check for issues
- Follow standard Go conventions
- Add tests for new functionality

## Commit Messages

Use clear, descriptive commit messages:

- `fix: correct buffer overflow check`
- `feat: add support for discard operations`
- `docs: update API documentation`
- `test: add integration tests for mmap`
- `refactor: simplify io_worker event loop`

## Testing

- **Unit tests**: Test individual functions without kernel interaction
- **Integration tests**: Test full device lifecycle (requires root + ublk module)
- Use `t.Parallel()` for tests that can run concurrently
- Add benchmarks for performance-critical code

## Pull Request Guidelines

- Keep PRs focused on a single change
- Include tests for new functionality
- Update documentation as needed
- Ensure CI passes before requesting review
