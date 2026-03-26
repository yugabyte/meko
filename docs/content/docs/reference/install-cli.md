---
title: Install the CLI
linkTitle: Install the CLI
description: Download or build the Meko CLI
weight: 20
---

## Download a pre-built binary

Pre-built binaries are published as GitHub Releases for every tagged version. Visit [Releases](https://github.com/yugabyte/meko/releases/latest) to download the archive for your platform.

| Platform | Archive |
|----------|---------|
| macOS (Apple Silicon) | `meko_<VERSION>_darwin_arm64.tar.gz` |
| macOS (Intel) | `meko_<VERSION>_darwin_amd64.tar.gz` |
| Linux x86_64 | `meko_<VERSION>_linux_amd64.tar.gz` |
| Linux ARM64 | `meko_<VERSION>_linux_arm64.tar.gz` |

### Example install (macOS apple silicon)

```bash
curl -Lo meko.tar.gz \
  https://github.com/yugabyte/meko/releases/latest/download/meko_0.1.0_darwin_arm64.tar.gz
tar xzf meko.tar.gz
chmod +x meko
sudo mv meko /usr/local/bin/
```

### Verify

```bash
meko --version
```

## Build from Source

Requires Go 1.25+.

```bash
cd cli_app
go mod download
go build -o meko .
```
