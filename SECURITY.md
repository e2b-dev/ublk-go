# Security Policy

## Supported Versions

| Version | Supported          |
| ------- | ------------------ |
| latest  | :white_check_mark: |

## Reporting a Vulnerability

If you discover a security vulnerability in this project, please report it responsibly:

1. **Do not** open a public GitHub issue for security vulnerabilities
2. Email the maintainers directly with details of the vulnerability
3. Include steps to reproduce the issue if possible
4. Allow reasonable time for the issue to be addressed before public disclosure

## Security Considerations

This library interacts with the Linux kernel's ublk driver and uses:

- **io_uring syscalls** - Direct kernel interface
- **Memory-mapped I/O** - Shared memory with kernel
- **Unsafe pointer operations** - Required for io_uring structures

### Running Requirements

- **Root privileges (CAP_SYS_ADMIN)** are required to create ublk devices
- The `ublk_drv` kernel module must be loaded

### Best Practices

- Run ublk devices in isolated environments when possible
- Validate all backend implementations for proper bounds checking
- Monitor device statistics for anomalies
