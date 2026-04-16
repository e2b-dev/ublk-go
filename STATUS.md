# Current Status

## What works

- **Struct layouts**: All Go structs verified to match kernel UAPI exactly (ctrlCmd=32B, devInfo=64B, ioCmd=16B, ioDesc=24B, sqe128=128B, cqe=16B).
- **io_uring ring**: Ring creation, SQE/CQE management, NOP round-trips, multi-cycle wrap-around — all tested and passing.
- **Control plane ADD_DEV**: Device creation via `UBLK_U_CMD_ADD_DEV` works on kernel 6.17 after fixing the `devInfo.DevID` field (must match `ctrlCmd.DevID`).
- **SET_PARAMS**: Device parameter setup works.
- **Unit tests**: `TestKernelABI`, `TestIoUringNOPRoundTrip`, `TestIoUringManyCycles` all pass.

## What doesn't work yet

- **START_DEV hangs**: After submitting all FETCH_REQ commands, `START_DEV` blocks indefinitely in the kernel's `wait_for_completion_interruptible(&ub->completion)`. The FETCH_REQ submissions don't return errors, but the kernel never fires the completion — meaning `ublk_mark_io_ready()` is not being called for all tags.

## Root cause investigation

The hang occurs in `io_uring_enter` for the control ring while the kernel processes `START_DEV` inline. The kernel's `ublk_ctrl_start_dev()` calls `wait_for_completion_interruptible(&ub->completion)`, which blocks until all queues have submitted their FETCH_REQ and `ublk_mark_io_ready()` has been called `queue_depth` times per queue.

Possible causes being investigated:
1. **FETCH_REQ not processed inline**: The FETCH_REQ SQEs may be deferred to io-wq instead of being processed during `io_uring_enter`. If so, the completion fires asynchronously but after `START_DEV` is already blocking.
2. **Ordering issue**: The FETCH_REQ are submitted on the IO ring and `START_DEV` on the control ring. If the kernel processes `START_DEV` before the FETCH completions propagate, it deadlocks.
3. **Kernel 6.17 behavioral change**: The September 2025 ublk refactoring ([longhorn/longhorn#11977](https://github.com/longhorn/longhorn/issues/11977)) changed internal ready-counting. The new `nr_io_ready` / `nr_queues_ready` logic may have additional requirements not yet met.

## Bugs fixed so far

1. **Control ring cmd placement** (original code): Command data was in `sqe.Addr` instead of `sqe.Cmd[]`.
2. **START_DEV ordering** (original code): START_DEV was sent before FETCH_REQ.
3. **Missing PID in START_DEV** (original code): `cmd.Data[0]` was never set.
4. **CQE struct size** (original code): 32 bytes instead of 16.
5. **IO descriptor mmap offset** (original code): Always 0 instead of per-queue offset.
6. **DevSectors calculation** (original code): `size/blockSize` instead of `size/512`.
7. **devInfo.DevID mismatch** (kernel 6.17): `info.DevID` must equal `cmd.DevID` — new validation added in kernel 6.17.

## Next steps

- Debug why FETCH_REQ submissions don't trigger `ublk_mark_io_ready` in the kernel. Options:
  - Try submitting FETCH_REQ from a dedicated OS thread (`runtime.LockOSThread`) before START_DEV.
  - Try calling `io_uring_enter` with `IORING_ENTER_GETEVENTS` after FETCH submission to force task work processing.
  - Read `dmesg` output after a test run (the kernel's `pr_warn` calls may reveal the rejection reason).
  - Test with an older kernel (pre-6.14) where the ublk UAPI was stable.
  - Cross-reference with the [ublksrv](https://github.com/ublk-org/ublksrv) C reference implementation to see how it handles the init sequence on 6.17.
