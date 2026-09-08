# Single source of truth for the version. Makefile, nfpm.yaml, and the binary all read it, so
# they cannot drift the way they did when the project shipped v0.8.0 while every machine-readable
# field still said 0.1.0.
VERSION := $(shell cat VERSION)
COMMIT := $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
BUILD_DATE := $(shell date -u +%Y-%m-%dT%H:%M:%SZ)

BINARY_DIR := bin
DIST_DIR := dist
MODULE := github.com/deziss/tasch

# -trimpath keeps absolute build paths out of the binary; -s -w strips symbols. Both the
# Makefile and build.sh use these, so `make build` and a release build produce the same artifact
# — previously the packaged binary was the unstripped, untrimmed Makefile output.
LDFLAGS := -s -w \
	-X '$(MODULE)/internal/version.Version=$(VERSION)' \
	-X '$(MODULE)/internal/version.Commit=$(COMMIT)' \
	-X '$(MODULE)/internal/version.BuildDate=$(BUILD_DATE)'
GOFLAGS := -trimpath

.PHONY: all build clean proto test test-race lint vet fmt fmt-check vulncheck verify run-test package deb rpm checksums version

all: verify build

build:
	@echo "Building tasch $(VERSION) ($(COMMIT))..."
	CGO_ENABLED=0 go build $(GOFLAGS) -ldflags="$(LDFLAGS)" -o $(BINARY_DIR)/tasch ./cmd/tasch
	@echo "Done. Binary: $(BINARY_DIR)/tasch"

version:
	@echo $(VERSION)

clean:
	rm -rf $(BINARY_DIR) $(DIST_DIR)

fmt:
	gofmt -w .

fmt-check:
	@unformatted=$$(gofmt -l .); \
	if [ -n "$$unformatted" ]; then \
		echo "These files are not gofmt-formatted:"; echo "$$unformatted"; exit 1; \
	fi

vet:
	go vet ./...

lint:
	golangci-lint run ./...

test:
	go test ./...

test-race:
	go test ./... -race -cover

vulncheck:
	govulncheck ./...

# Everything CI enforces, runnable in one command before pushing.
verify: fmt-check vet test-race

# End-to-end integration suite. Runs real master and worker processes.
run-test:
	./test.sh

proto:
	protoc --proto_path=api/v1 \
		--go_out=api/v1 --go_opt=paths=source_relative \
		--go-grpc_out=api/v1 --go-grpc_opt=paths=source_relative \
		api/v1/scheduler.proto

package: deb rpm checksums

deb: build
	@echo "Packaging DEB..."
	mkdir -p $(DIST_DIR)
	VERSION=$(VERSION) nfpm package --config nfpm.yaml --target $(DIST_DIR)/ --packager deb

rpm: build
	@echo "Packaging RPM..."
	mkdir -p $(DIST_DIR)
	VERSION=$(VERSION) nfpm package --config nfpm.yaml --target $(DIST_DIR)/ --packager rpm

# Publish these alongside the artifacts. Without them a downloader has no way to tell whether
# the binary they are about to run on every node is the one that was built here.
checksums:
	@cd $(DIST_DIR) && sha256sum * > SHA256SUMS 2>/dev/null || true
	@echo "Wrote $(DIST_DIR)/SHA256SUMS"
