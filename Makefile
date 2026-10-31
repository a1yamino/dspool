BINARY := dspool
VERSION ?= 0.1.0

.PHONY: build test race run fmt vet clean

build:
	go build -trimpath -ldflags "-s -w" -o $(BINARY) ./cmd/dspool

test:
	go test ./...

race:
	go test -race ./...

run:
	go run ./cmd/dspool -config config.yaml

fmt:
	gofmt -w ./cmd ./internal

vet:
	go vet ./...

clean:
	rm -f $(BINARY)
