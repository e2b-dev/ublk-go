.PHONY: test test-integration build lint

test:
	go test -v -count=1 ./ublk/

test-integration:
	@echo "Loading ublk_drv..."
	@modprobe ublk_drv 2>/dev/null || true
	go test -v -count=1 -timeout=120s ./ublk/

build:
	go build -race ./...

lint:
	golangci-lint run ./...
	gofmt -w .
	go mod tidy
