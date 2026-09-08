#!/usr/bin/env bash

# Exit immediately if a command exits with a non-zero status
set -e

# Target platforms to build
PLATFORMS=(
    "linux/amd64"
    "linux/arm64"
    "windows/amd64"
    "windows/arm64"
    "darwin/amd64"
    "darwin/arm64"
)

OUTPUT_DIR="dist/bin"
mkdir -p "$OUTPUT_DIR"

MODULE="github.com/deziss/tasch"
VERSION="$(cat VERSION)"
COMMIT="$(git rev-parse --short HEAD 2>/dev/null || echo unknown)"
BUILD_DATE="$(date -u +%Y-%m-%dT%H:%M:%SZ)"

echo "=== Starting Tasch Cross-Compilation ==="
echo "Output directory: $OUTPUT_DIR"
echo ""

# Loop through each target platform
for platform in "${PLATFORMS[@]}"; do
    # Split the platform into OS and ARCH
    IFS="/" read -r -a parts <<< "$platform"
    GOOS="${parts[0]}"
    GOARCH="${parts[1]}"
    
    # Define binary name
    binary_name="tasch-${GOOS}-${GOARCH}"
    if [ "$GOOS" = "windows" ]; then
        binary_name="${binary_name}.exe"
    fi
    
    output_path="${OUTPUT_DIR}/${binary_name}"
    
    echo "Building for OS=${GOOS} ARCH=${GOARCH}..."
    
    # Same flags as `make build`, so the cross-compiled artifacts and the packaged binary are
    # the same build. They used to differ: build.sh stripped and disabled cgo while the Makefile
    # did neither, and it was the Makefile's output that shipped in the .deb and .rpm.
    CGO_ENABLED=0 GOOS="$GOOS" GOARCH="$GOARCH" go build \
        -trimpath \
        -ldflags="-s -w -X '${MODULE}/internal/version.Version=${VERSION}' -X '${MODULE}/internal/version.Commit=${COMMIT}' -X '${MODULE}/internal/version.BuildDate=${BUILD_DATE}'" \
        -o "$output_path" \
        ./cmd/tasch
        
    echo "  -> Generated: $output_path"
done

echo ""
echo "=== Build Complete ==="
ls -lh "$OUTPUT_DIR"

# Publish these alongside the binaries. Without them a downloader has no way to check that the
# binary they are about to run on every node is the one that was built here.
( cd "$OUTPUT_DIR" && sha256sum ./* > SHA256SUMS )
echo ""
echo "Checksums: $OUTPUT_DIR/SHA256SUMS"
