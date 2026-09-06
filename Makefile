VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS  = -s -w -X main.version=$(VERSION)

.PHONY: build install-local dist test clean

build:
	go build -ldflags "$(LDFLAGS)" -o hpscan ./cmd/hpscan

# Copy the binary to /usr/local/bin (macOS / Linux).
install-local: build
	rm -f /usr/local/bin/hpscan && install -m 755 hpscan /usr/local/bin/hpscan  # rm first: overwriting a running binary in place breaks macOS code signing

# Cross-compile for Mac (Apple Silicon + Intel) and Synology (x86_64 + ARM64).
dist:
	mkdir -p dist
	GOOS=darwin GOARCH=arm64 go build -ldflags "$(LDFLAGS)" -o dist/hpscan-darwin-arm64 ./cmd/hpscan
	GOOS=darwin GOARCH=amd64 go build -ldflags "$(LDFLAGS)" -o dist/hpscan-darwin-amd64 ./cmd/hpscan
	GOOS=linux  GOARCH=amd64 CGO_ENABLED=0 go build -ldflags "$(LDFLAGS)" -o dist/hpscan-linux-amd64 ./cmd/hpscan
	GOOS=linux  GOARCH=arm64 CGO_ENABLED=0 go build -ldflags "$(LDFLAGS)" -o dist/hpscan-linux-arm64 ./cmd/hpscan

test:
	go test ./...

clean:
	rm -rf dist hpscan
