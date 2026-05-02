.PHONY: test test-unit test-integration fuzz-uring fuzz-uring-submit fuzz-uring-cancel cover cover-html chain flushbench flushbench-race stress fault sigkill build lint lint-fmt lint-tidy lint-vet fmt hooks

test: test-unit test-integration

test-unit:
	go test -v -count=1 -race ./ublk/uring/ ./ublk/
	@$(MAKE) fuzz-uring FUZZTIME=10s
	@$(MAKE) fuzz-uring FUZZTIME=10s

test-integration:
	go test -c -race -tags=integration -o /tmp/ublk.test ./ublk/
	sudo /tmp/ublk.test -test.v -test.timeout=300s

# Run the io_uring fuzz targets back-to-back. Default budget is 30s
# per target, override with FUZZTIME=2m make fuzz-uring (or longer).
# These targets need no kernel module and no root.
FUZZTIME ?= 30s
fuzz-uring: fuzz-uring-submit fuzz-uring-cancel

fuzz-uring-submit:
	go test -run=^$$ -fuzz=FuzzRingSubmit -fuzztime=$(FUZZTIME) ./ublk/uring/

fuzz-uring-cancel:
	go test -run=^$$ -fuzz=FuzzRingCancel -fuzztime=$(FUZZTIME) ./ublk/uring/

# Produce coverage profiles (unit + integration + combined) under ./coverage/.
cover:
	mkdir -p coverage
	go test -count=1 -race -coverprofile=coverage/unit.out -coverpkg=./ublk/... ./ublk/uring/ ./ublk/
	go test -c -race -tags=integration -coverpkg=./ublk/... -o /tmp/ublk.test ./ublk/
	sudo /tmp/ublk.test -test.v -test.timeout=300s -test.coverprofile=coverage/integration.out
	sudo chmod 644 coverage/integration.out
	go install github.com/wadey/gocovmerge@latest
	"$$(go env GOPATH)/bin/gocovmerge" coverage/unit.out coverage/integration.out > coverage/combined.out
	@echo
	@echo "=== unit ==="
	@go tool cover -func=coverage/unit.out | tail -1
	@echo "=== integration ==="
	@go tool cover -func=coverage/integration.out | tail -1
	@echo "=== combined (unit + integration) ==="
	@go tool cover -func=coverage/combined.out | tail -1

# Render the combined coverage profile as HTML in your browser.
cover-html: cover
	go tool cover -html=coverage/combined.out

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
