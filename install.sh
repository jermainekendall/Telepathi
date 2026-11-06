#!/bin/bash
set -e

echo "================================================"
echo "  Telepathi Protocol - Build & Install Script"
echo "================================================"
echo ""

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$SCRIPT_DIR"

# ---- Install system dependencies ----
echo "Checking system dependencies..."

NEED_INSTALL=""
dpkg -s libvulkan-dev &>/dev/null || NEED_INSTALL="$NEED_INSTALL libvulkan-dev"
dpkg -s vulkan-tools &>/dev/null || NEED_INSTALL="$NEED_INSTALL vulkan-tools"
dpkg -s glslang-tools &>/dev/null || NEED_INSTALL="$NEED_INSTALL glslang-tools"

if [ -n "$NEED_INSTALL" ]; then
    echo "Installing:$NEED_INSTALL"
    sudo apt-get update -qq
    sudo apt-get install -y -qq $NEED_INSTALL
else
    echo "All system dependencies present."
fi
echo ""

# ---- Check for Go ----
if ! command -v go &> /dev/null; then
    echo "Go is not installed. Installing Go 1.22..."
    ARCH=$(uname -m)
    case $ARCH in
        x86_64)  GOARCH="amd64" ;;
        aarch64) GOARCH="arm64" ;;
        *)       echo "Unsupported architecture: $ARCH"; exit 1 ;;
    esac

    GO_VERSION="1.22.4"
    GO_TAR="go${GO_VERSION}.linux-${GOARCH}.tar.gz"
    wget -q "https://go.dev/dl/${GO_TAR}" -O "/tmp/${GO_TAR}"
    sudo rm -rf /usr/local/go
    sudo tar -C /usr/local -xzf "/tmp/${GO_TAR}"
    rm "/tmp/${GO_TAR}"
    export PATH=$PATH:/usr/local/go/bin
    if ! grep -q '/usr/local/go/bin' ~/.bashrc 2>/dev/null; then
        echo 'export PATH=$PATH:/usr/local/go/bin' >> ~/.bashrc
    fi
fi

echo "Go version: $(go version)"
echo ""

# ---- Compile SPIR-V shaders ----
echo "Compiling Vulkan compute shaders..."
mkdir -p shaders

if command -v glslangValidator &> /dev/null; then
    GLSLC="glslangValidator"
    glslangValidator -V shaders/sha256_blocks.comp -o shaders/sha256_blocks.spv
    glslangValidator -V shaders/entropy_classify.comp -o shaders/entropy_classify.spv
    echo "Shaders compiled successfully."
elif command -v glslc &> /dev/null; then
    glslc shaders/sha256_blocks.comp -o shaders/sha256_blocks.spv
    glslc shaders/entropy_classify.comp -o shaders/entropy_classify.spv
    echo "Shaders compiled successfully."
else
    echo "WARNING: No GLSL compiler found. GPU acceleration will not be available."
    echo "         Install with: sudo apt install glslang-tools"
fi
echo ""

# ---- Build Go binary ----
echo "Building Telepathi Protocol..."
CGO_ENABLED=1 go build -o telepathi -ldflags="-s -w" ./cmd/telepathi/

echo "Build successful!"
echo ""

# ---- Install ----
echo "Installing to /usr/local/bin/telepathi..."
sudo cp telepathi /usr/local/bin/telepathi
sudo chmod +x /usr/local/bin/telepathi

# Copy shaders alongside binary
sudo mkdir -p /usr/local/share/telepathi/shaders
if [ -f shaders/sha256_blocks.spv ]; then
    sudo cp shaders/*.spv /usr/local/share/telepathi/shaders/
fi

echo ""
echo "================================================"
echo "  Installation complete!"
echo "================================================"
echo ""
echo "Usage:"
echo ""
echo "  Receiver:  telepathi listen -port 7331 -dir ./received"
echo "  Sender:    telepathi send -host <IP> -port 7331 file.dat"
echo "  Sender:    telepathi send -host <IP> -workers 8 file.dat"
echo ""
echo "GPU Status:"
if [ -f shaders/sha256_blocks.spv ]; then
    echo "  Shaders compiled. GPU will be used if a hardware GPU is detected."
else
    echo "  No shaders compiled. CPU-only mode."
fi
echo ""
