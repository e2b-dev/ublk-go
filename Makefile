.PHONY: test test-integration build lint fmt

test:
	go test -v -count=1 -race ./ublk/uring/ ./ublk/

test-integration:
	go test -c -race -o /tmp/ublk.test ./ublk/
	sudo /tmp/ublk.test -test.v -test.timeout=120s

build:
	go build ./...

lint:
	test -z "$$(gofmt -l .)"
	golangci-lint run ./...
	go mod verify

fmt:
	gofmt -w .
	go mod tidy
