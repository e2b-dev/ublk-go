.PHONY: test bench build clean

test:
	go test ./... -count=1

bench:
	go test ./ublk -bench=. -benchmem -run=^$$

build:
	go build -race ./...

clean:
	golangci-lint run ./...
	gofmt -w .
	go mod tidy
