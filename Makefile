.PHONY: test build clean

test:
	go test ./... -count=1

build:
	go build -race ./...

clean:
	golangci-lint run ./...
	gofmt -w .
	go mod tidy
