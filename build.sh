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
    
    # Compile with flags to strip symbols and debug information (-s -w) to reduce binary size
    CGO_ENABLED=0 GOOS="$GOOS" GOARCH="$GOARCH" go build \
        -ldflags="-s -w" \
        -o "$output_path" \
        ./cmd/tasch
        
    echo "  -> Generated: $output_path"
done

echo ""
echo "=== Build Complete ==="
ls -lh "$OUTPUT_DIR"
