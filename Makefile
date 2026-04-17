.PHONY: test test-unit test-integration cover cover-html chain flushbench flushbench-race stress fault sigkill build lint lint-fmt lint-tidy lint-vet fmt hooks

test: test-unit test-integration

test-unit:
	go test -v -count=1 -race ./ublk/uring/ ./ublk/

test-integration:
	go test -c -race -tags=integration -o /tmp/ublk.test ./ublk/
	sudo /tmp/ublk.test -test.v -test.timeout=300s

# Produce coverage profiles (unit + integration) under ./coverage/.
cover:
	mkdir -p coverage
	go test -count=1 -race -coverprofile=coverage/unit.out -coverpkg=./ublk/... ./ublk/uring/ ./ublk/
	go test -c -race -tags=integration -coverpkg=./ublk/... -o /tmp/ublk.test ./ublk/
	sudo /tmp/ublk.test -test.v -test.timeout=120s -test.coverprofile=coverage/integration.out
	sudo chmod 644 coverage/integration.out
	@echo
	@echo "=== unit        coverage ==="
	@go tool cover -func=coverage/unit.out | tail -1
	@echo "=== integration coverage ==="
	@go tool cover -func=coverage/integration.out | tail -1

# Render the integration coverage profile as HTML in your browser.
cover-html: cover
	go tool cover -html=coverage/integration.out

# Chain two ublk devices in the same process (proxy -> storage) and
# verify byte-exact data flow through both stacks.
chain:
	go build -race -o /tmp/ublk-chain ./example/chain
	sudo /tmp/ublk-chain

# Diagnose where time is spent during filesystem flushes.
# Prints per-backend-call trace with microsecond timestamps so you can
# see whether our stack is slow or the kernel is waiting on its own
# timers. Built without -race so latency numbers reflect production;
# use 'make flushbench-race' for the race-detector version.
flushbench:
	go build -o /tmp/ublk-flushbench ./example/flushbench
	sudo /tmp/ublk-flushbench

flushbench-race:
	go build -race -o /tmp/ublk-flushbench-race ./example/flushbench
	sudo /tmp/ublk-flushbench-race

# Race-detector stress run: exercises rapid Create/Close, I/O-during-
# shutdown, concurrent Close, and many parallel devices. Passes if the
# race detector stays silent for the configured duration.
stress:
	go build -race -o /tmp/ublk-stress ./example/stress
	sudo /tmp/ublk-stress -duration 30s

# Fault injection: Backend returns EIO on a configurable fraction of
# operations. Verifies errors propagate to userspace and Close still
# completes when the device is in an unhappy state.
fault:
	go build -race -o /tmp/ublk-fault ./example/fault
	sudo /tmp/ublk-fault

# SIGKILL recovery: spawn a child, kill -9 it mid-I/O, verify the
# kernel cleans up and the parent can create a fresh device afterwards.
sigkill:
	go build -race -o /tmp/ublk-sigkill ./example/sigkill
	sudo /tmp/ublk-sigkill

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
