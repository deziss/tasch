BINARY_DIR := bin
MODULE := github.com/deziss/tasch

.PHONY: build clean proto test run-test

build:
	@echo "Building tasch..."
	go build -o $(BINARY_DIR)/tasch ./cmd/tasch
	@echo "Done. Binary: $(BINARY_DIR)/tasch"

clean:
	rm -rf $(BINARY_DIR)

proto:
	PATH="$$HOME/go/bin:$$PATH" protoc \
		--go_out=. --go_opt=paths=source_relative \
		--go-grpc_out=. --go-grpc_opt=paths=source_relative \
		api/v1/scheduler.proto

test:
	go test ./... -v

run-test: build
	bash test.sh
