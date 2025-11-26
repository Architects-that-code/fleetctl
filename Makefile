# Simple Makefile for fleetctl
BINARY := fleetctl
CMD := ./cmd/fleetctl

.PHONY: all tidy build run install test clean help

all: build

tidy:
	go mod tidy

build: tidy
	mkdir -p bin
	go build -o bin/$(BINARY) $(CMD)

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
	@echo "  tidy     - Run go mod tidy"
	@echo "  build    - Build binary to bin/$(BINARY)"
	@echo "  run      - Run the CLI (use ARGS to pass flags)"
	@echo "  install  - Install binary to GOPATH/bin"
	@echo "  test     - Run all tests"
	@echo "  clean    - Remove build artifacts"
