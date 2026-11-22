.PHONY: build test bench vet clean

BINARY := bin/soma

build:
	CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o $(BINARY) ./cmd/soma

test:
	go test -race -v ./...

bench:
	go test -race -v ./benchmarks/...

vet:
	go vet ./...

clean:
	rm -rf bin/ coverage.out
