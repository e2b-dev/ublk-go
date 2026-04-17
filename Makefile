.PHONY: test test-unit test-integration probe chain flushbench stress torture build lint lint-fmt lint-tidy lint-vet fmt hooks

test: test-unit test-integration

test-unit:
	go test -v -count=1 -race ./ublk/uring/ ./ublk/

test-integration:
	go test -c -race -tags=integration -o /tmp/ublk.test ./ublk/
	sudo /tmp/ublk.test -test.v -test.timeout=120s

# End-to-end autonomous smoke test: create device, mkfs/mount/IO/umount/close.
# Each step has a timeout so hangs surface as failures.
probe:
	go build -race -o /tmp/ublk-probe ./example/probe
	sudo /tmp/ublk-probe

# Chain two ublk devices in the same process (proxy -> storage) and
# verify byte-exact data flow through both stacks.
chain:
	go build -race -o /tmp/ublk-chain ./example/chain
	sudo /tmp/ublk-chain

# Diagnose where time is spent during filesystem flushes.
# Prints per-backend-call trace with microsecond timestamps so you can
# see whether our stack is slow or the kernel is waiting on its own
# timers.
flushbench:
	go build -race -o /tmp/ublk-flushbench ./example/flushbench
	sudo /tmp/ublk-flushbench

# Race-detector stress run: exercises rapid Create/Close, I/O-during-
# shutdown, concurrent Close, and many parallel devices. Passes if the
# race detector stays silent for the configured duration.
stress:
	go build -race -o /tmp/ublk-stress ./example/stress
	sudo /tmp/ublk-stress -duration 30s

# Randomised I/O torture: picks random (offset, length, direction)
# tuples, maintains a shadow of expected device contents, fails on
# first divergence. Catches data-integrity / offset / ordering bugs.
torture:
	go build -race -o /tmp/ublk-torture ./example/torture
	sudo /tmp/ublk-torture -duration 30s -parallel 4

build:
	go build ./...

lint: lint-fmt lint-tidy lint-vet

lint-fmt:
	test -z "$$(gofmt -l .)"

lint-tidy:
	go mod tidy -diff

lint-vet:
	golangci-lint run ./...
	go mod verify

fmt:
	gofmt -w .
	go mod tidy

hooks:
	git config core.hooksPath .githooks
