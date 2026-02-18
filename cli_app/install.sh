#!/bin/bash

# Yuga CLI Installation Script

set -e

echo "🚀 Installing Yuga CLI..."
echo ""

# Check Go installation
if ! command -v go &> /dev/null; then
    echo "❌ Go is not installed. Please install Go 1.21 or higher."
    echo "   Visit: https://golang.org/dl/"
    exit 1
fi

# Check Go version
GO_VERSION=$(go version | awk '{print $3}' | sed 's/go//')
REQUIRED_VERSION="1.21"

if [ "$(printf '%s\n' "$REQUIRED_VERSION" "$GO_VERSION" | sort -V | head -n1)" != "$REQUIRED_VERSION" ]; then
    echo "❌ Go version $REQUIRED_VERSION or higher is required. Found: $GO_VERSION"
    exit 1
fi

echo "✓ Go version: $GO_VERSION"
echo ""

# Download dependencies
echo "📦 Downloading dependencies..."
go mod download

# Build the binary
echo "🔨 Building binary..."
go build -o yuga-cli .

# Make it executable
chmod +x yuga-cli

echo ""
echo "✅ Installation complete!"
echo ""
echo "To run the CLI:"
echo "  ./yuga-cli interactive    # Launch interactive TUI"
echo "  ./yuga-cli status         # Check system status"
echo "  ./yuga-cli --help         # View all commands"
echo ""
echo "To install globally, run:"
echo "  sudo mv yuga-cli /usr/local/bin/"
echo ""
