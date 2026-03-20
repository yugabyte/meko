# Meko CLI Usage

## Installation

### Download a pre-built binary

Pre-built binaries are published as GitHub Releases for every tagged version.
Visit [Releases](https://github.com/yugabyte/meko/releases/latest) to download
the archive for your platform.

| Platform | Archive |
|----------|---------|
| macOS (Apple Silicon) | `meko_<VERSION>_darwin_arm64.tar.gz` |
| macOS (Intel) | `meko_<VERSION>_darwin_amd64.tar.gz` |
| Linux x86_64 | `meko_<VERSION>_linux_amd64.tar.gz` |
| Linux ARM64 | `meko_<VERSION>_linux_arm64.tar.gz` |

Example install (macOS Apple Silicon):

```bash
curl -Lo meko.tar.gz \
  https://github.com/yugabyte/meko/releases/latest/download/meko_0.1.0_darwin_arm64.tar.gz
tar xzf meko.tar.gz
chmod +x meko
sudo mv meko /usr/local/bin/
```

Verify:

```bash
meko --version
```

### Build from source

Requires Go 1.25+.

```bash
cd cli_app
go mod download
go build -o meko .
```

---

## Authentication

All datapack commands require a valid login session. Credentials are stored
locally at `~/var/meko/credentials.json`.

### Login

```bash
meko login
```

You will be prompted for your email and password. Alternatively, pass them as
flags:

```bash
meko login --email user@example.com --password secret
```

To authenticate against a different API server:

```bash
meko login --api-server https://my-server.example.com
```

The default API server is `https://mekodev.com`.

### Logout

```bash
meko logout
```

Removes locally stored tokens.

---

## Datapack Commands

All `meko datapack` subcommands require a valid login session. If you are not
logged in, they will fail with:

```
Error: not logged in - run `meko login` first
```

### Global flags

| Flag | Description |
|------|-------------|
| `--json` | Output raw JSON instead of formatted text |
| `--verbose`, `-v` | Enable verbose logging |

### Create a datapack

```bash
meko datapack create --name my-datapack
```

This provisions a new datapack with a dedicated database, MCP server endpoint,
and database access credentials. A spinner is shown while the server processes
the request (typically up to 10 seconds).

For machine-readable output:

```bash
meko datapack create --name my-datapack --json
```

Example JSON output:

```json
{
  "datapack_id": "e02a7107-c79e-451d-b360-9cd1e13f175d",
  "agent_id": "18311fce-36c5-40fe-bd45-67f5913b0b52",
  "name": "my-datapack",
  "mcp_server": {
    "url": "https://mekodev.com/<account_id>/mcp",
    "api_key": "318ce1c589d08edb11e24b0a781fe675"
  },
  "db_access": {
    "user_name": "db_my-datapack",
    "password": "d28a0d1498c1ab9692184dd9c9cd1a79",
    "conn_str": "postgresql://db_my-datapack:d28a0d14...@host:5433/account_id_my-datapack"
  }
}
```

### List datapacks

```bash
meko datapack list
```

Displays a table of all datapacks with name, ID, and creation date.

```bash
meko datapack list --json
```

### Delete a datapack

```bash
meko datapack delete --name my-datapack
```

You will be prompted to re-type the datapack name as confirmation, since this
permanently destroys the datapack and its underlying database.

To skip the confirmation prompt (for scripts):

```bash
meko datapack delete --name my-datapack --force
```

### Show datapack status

```bash
meko datapack status --name my-datapack
```

Displays summary info: name, ID, agent count, knowledge base count, and
creation date.

```bash
meko datapack status --name my-datapack --json
```

---

## Logging

The CLI writes structured logs to `~/var/meko/logs/`. Use `--verbose` to enable
debug-level output. Log files are useful for diagnosing authentication or
network errors.

---

## Releasing a New Version

The CLI release pipeline is fully automated via
[GoReleaser](https://goreleaser.com/) and GitHub Actions.

### How it works

1. A developer pushes a version tag.
2. The GitHub Actions workflow (`.github/workflows/release.yml`) triggers.
3. GoReleaser cross-compiles the CLI for macOS and Linux (amd64 + arm64).
4. Archives (`.tar.gz`) and a SHA256 checksum file are uploaded to a new
   GitHub Release.

### Cutting a release

```bash
# Tag the commit
git tag v0.2.0

# Push the tag to trigger the pipeline
git push origin v0.2.0
```

The release will appear at
`https://github.com/yugabyte/meko/releases/tag/v0.2.0` once the workflow
completes.

### Version injection

The binary's version string is injected at build time via Go ldflags. The
variable `cli_app/cmd.version` is set to the git tag value by GoReleaser.
During local development, `meko --version` shows `dev`.

### Configuration files

| File | Purpose |
|------|---------|
| `.goreleaser.yml` | GoReleaser config: build targets, archive format, release settings |
| `.github/workflows/release.yml` | GitHub Actions workflow triggered on `v*` tags |

### Testing the release locally

You can dry-run the build without publishing:

```bash
goreleaser build --snapshot --clean
```

This produces binaries under `dist/` without creating a GitHub release.

To validate the config:

```bash
goreleaser check
```

### Release artifacts

Each release includes:

| File | Contents |
|------|----------|
| `meko_<VERSION>_darwin_arm64.tar.gz` | macOS Apple Silicon binary |
| `meko_<VERSION>_darwin_amd64.tar.gz` | macOS Intel binary |
| `meko_<VERSION>_linux_amd64.tar.gz` | Linux x86_64 binary |
| `meko_<VERSION>_linux_arm64.tar.gz` | Linux ARM64 binary |
| `checksums.txt` | SHA256 checksums for all archives |
