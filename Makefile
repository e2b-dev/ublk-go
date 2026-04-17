.PHONY: test test-integration build lint

test:
	go test -v -count=1 ./ublk/uring/ ./ublk/

test-integration:
	go test -c -o /tmp/ublk.test ./ublk/
	sudo /tmp/ublk.test -test.v -test.timeout=120s

build:
	go build ./...

lint:
	gofmt -w .
	golangci-lint run ./...
	go mod tidy
