.PHONY: build test bench vet shadow clean

BINARY := bin/soma
BRIDGE_BINARY := bin/soma-bridge-discord

build:
	CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o $(BINARY) ./cmd/soma
	CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o $(BRIDGE_BINARY) ./cmd/soma-bridge-discord

test:
	go test -race -v ./...

bench:
	go test -race -v ./benchmarks/...

vet:
	go vet ./...

shadow: build
	TMP=$$(mktemp -d) && trap 'rm -rf "$$TMP"' EXIT && ./$(BINARY) shadow run --dir "$$TMP" --out-md docs/benchmarks/BENCHMARK_REPORT.md

clean:
	rm -rf bin/ coverage.out
