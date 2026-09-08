# Horizon Autoscaler — Makefile
#
# Targets:
#   make build      Build the Go binary locally
#   make test       Run all tests
#   make test-race  Run tests with the race detector (requires CGO / Linux/macOS)
#   make vet        Run go vet
#   make run        Run the service locally on :8080
#   make clean      Remove local build artifacts

APP_DIR   := ./app
BINARY    := $(APP_DIR)/horizon-app
PORT      ?= 8080
WORK_MS   ?= 100

.PHONY: build test test-race vet run clean

## build: Compile the Go binary into app/horizon-app
build:
	cd $(APP_DIR) && go build -o horizon-app .

## test: Run all unit and integration tests
test:
	cd $(APP_DIR) && go test -v ./...

## test-race: Run tests with the Go race detector (needs CGO + 64-bit GCC)
test-race:
	cd $(APP_DIR) && go test -v -race ./...

## vet: Run go vet on all packages
vet:
	cd $(APP_DIR) && go vet ./...

## run: Build and run the service locally
run: build
	$(BINARY) -port $(PORT)

## clean: Remove compiled binaries
clean:
	rm -f $(APP_DIR)/horizon-app $(APP_DIR)/horizon-app.exe
