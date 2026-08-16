BIN     := bin/mcp-diet
PKG     := ./cmd/mcp-diet
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: all build test race cover bench vet fmt lint analyze clean install

all: vet test build

build:
	@mkdir -p bin
	go build -trimpath -ldflags '$(LDFLAGS)' -o $(BIN) $(PKG)

install:
	go install -trimpath -ldflags '$(LDFLAGS)' $(PKG)

test:
	go test ./... -race -count=1

# Latency budgets are skipped under -race; this target enforces them.
bench:
	go test ./... -count=1
	go test ./prune ./session ./graph -run=XXX -bench=. -benchmem

cover:
	go test ./... -coverprofile=coverage.out -covermode=atomic
	go tool cover -func=coverage.out | tail -1
	go tool cover -html=coverage.out -o coverage.html

vet:
	go vet ./...

fmt:
	gofmt -l -w .

analyze: build
	$(BIN) analyze testdata/tools.json

clean:
	rm -rf bin dist coverage.out coverage.html
