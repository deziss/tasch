VERSION := 0.1.0
BINARY_DIR := bin
DIST_DIR := dist
MODULE := github.com/deziss/tasch

.PHONY: build clean proto test run-test package deb rpm

build:
	@echo "Building tasch..."
	go build -o $(BINARY_DIR)/tasch ./cmd/tasch
	@echo "Done. Binary: $(BINARY_DIR)/tasch"

clean:
	rm -rf $(BINARY_DIR) $(DIST_DIR)

package: deb rpm

deb: build
	@echo "Packaging DEB..."
	mkdir -p $(DIST_DIR)
	nfpm package --config nfpm.yaml --target $(DIST_DIR)/ --packager deb

rpm: build
	@echo "Packaging RPM..."
	mkdir -p $(DIST_DIR)
	nfpm package --config nfpm.yaml --target $(DIST_DIR)/ --packager rpm
