# Simple Makefile for fleetctl
BINARY := fleetctl
CMD := ./cmd/fleetctl

.PHONY: all tidy build run install test clean help build-all build-windows build-linux-amd64 build-linux-arm64 build-mac-intel

all: build

tidy:
	go mod tidy

build: tidy
	mkdir -p bin
	go build -o bin/$(BINARY) $(CMD)

# Cross-compilation targets
# build-windows: tidy
# 	mkdir -p bin
# 	GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -o bin/$(BINARY)-windows-amd64.exe $(CMD)

build-linux-amd64: tidy
	mkdir -p bin
	GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o bin/$(BINARY)-linux-amd64 $(CMD)

build-linux-arm64: tidy
	mkdir -p bin
	GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -o bin/$(BINARY)-linux-arm64 $(CMD)

build-mac-intel: tidy
	mkdir -p bin
	GOOS=darwin GOARCH=amd64 CGO_ENABLED=0 go build -o bin/$(BINARY)-darwin-amd64 $(CMD)

build-all:  build-linux-amd64 build-linux-arm64 build-mac-intel

run:
	# Pass args like: make run ARGS="--config fleet.yaml"
	go run $(CMD) $(ARGS)

install: tidy
	go install $(CMD)

test:
	go test ./...

clean:
	rm -rf bin

help:
	@echo "Targets:"
	@echo "  tidy                - Run go mod tidy"
	@echo "  build               - Build binary to bin/$(BINARY)"
	@echo "  build-windows       - Build Windows (amd64) to bin/$(BINARY)-windows-amd64.exe"
	@echo "  build-linux-amd64   - Build Linux (amd64) to bin/$(BINARY)-linux-amd64"
	@echo "  build-linux-arm64   - Build Linux (arm64) to bin/$(BINARY)-linux-arm64"
	@echo "  build-mac-intel     - Build macOS (Intel/amd64) to bin/$(BINARY)-darwin-amd64"
	@echo "  build-all           - Build all target platforms above"
	@echo "  run                 - Run the CLI (use ARGS to pass flags)"
	@echo "  install             - Install binary to GOPATH/bin"
	@echo "  test                - Run all tests"
	@echo "  clean               - Remove build artifacts"
