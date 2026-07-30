# Development Guide

## Prerequisites

### Go Linter

```bash
go install github.com/golangci/golangci-lint/cmd/golangci-lint@latest
```

### Python Linters

```bash
pip install mypy pycodestyle
```

## Linter Configuration

| Tool | Config File | Scope |
|------|-------------|-------|
| golangci-lint | `.golangci.yml` | Go code (`cli_app/`) |
| mypy | `mypy.ini` | Python type checking (`system_agent/`) |
| pycodestyle | `tox.ini` | Python style checking (`system_agent/`) |
| EditorConfig | `.editorconfig` | Editor formatting (all files) |

### Key Style Rules

- **Max line length**: 100 characters (default), 120 for Go files
- **Indentation**: Tabs for Go, 4 spaces for Python, 2 spaces for YAML/shell
- **Line endings**: LF (Unix-style)
- **Trailing whitespace**: Trimmed (except Markdown)

## Running Linters

### Go

```bash
go install github.com/golangci/golangci-lint/cmd/golangci-lint@latest
echo 'export PATH="$PATH:$HOME/go/bin"' >> ~/.bashrc
source ~/.bashrc

cd cli_app && golangci-lint run ./...
```

### Python

```bash
cd system_agent && mypy .
cd system_agent && pycodestyle .
```

### Run All

```bash
(cd cli_app && golangci-lint run ./...) && \
(cd system_agent && mypy . && pycodestyle .)
```

## Git Pre-Push Hook

To automatically run all linters before every `git push`, create the hook:

```bash
cat > .git/hooks/pre-push << 'EOF'
#!/usr/bin/env bash
set -e

echo "Running Go linter..."
(cd cli_app && golangci-lint run ./...)

echo "Running Python type checker..."
(cd system_agent && mypy .)

echo "Running Python style checker..."
(cd system_agent && pycodestyle .)

echo "All linters passed."
EOF
chmod +x .git/hooks/pre-push
```

Once installed, any `git push` will be blocked if linters report errors.

## Build

```bash
$ cd cli_app
$ go build -o meko .
```

## Meko CLI cms

```
./meko start
./meko datapack create --name foo --embed_model text-embedding-3-small --inference_model LLama-3.1-8B-Instruct --seed_knowledge_base s3://proto-automated-embedding/playgentic_documents/sample/
```


### Testing

```
./bin/ysqlsh -U yugabyte -d yugabyte -c "TRUNCATE meko_system.agent_knowledge_base, meko_system.agent_registry, meko_system.datapack_knowledge_base_registry, meko_system.datapack_registry, meko_system.account_registry CASCADE;"
```

```
-- list all knoweldge bases that belongs to a datapack
SELECT kb.id, kb.knowledge_base_url, kb.created_at
FROM meko_system.datapack_knowledge_base_registry kb
JOIN meko_system.datapack_registry dp ON dp.id = kb.datapack_id
WHERE dp.datapack_name = 'foo';

-- list all agents that belong to a datapack
SELECT a.id, a.agent_name, a.agent_metadata, a.created_at
FROM meko_system.agent_registry a
JOIN meko_system.datapack_registry dp ON dp.id = a.datapack_id
WHERE dp.datapack_name = 'foo';

-- list all knowledge bases that belong to a agent
SELECT kb.id, kb.knowledge_base_url, kb.created_at
FROM meko_system.datapack_knowledge_base_registry kb
JOIN meko_system.agent_knowledge_base akb ON akb.knowledge_base_id = kb.id
JOIN meko_system.agent_registry a ON a.id = akb.agent_id
WHERE a.agent_name = 'meko_user_agent';
```