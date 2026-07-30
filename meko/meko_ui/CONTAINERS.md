# Container Deployment Notes

This stack serves Meko and Langfuse under one domain/path:

- `/` -> Meko UI (`meko-ui`)
- `/observe/` -> Langfuse Web (`langfuse-web`)

## Prerequisites

1. Langfuse compose stack is up and healthy.
2. Langfuse network exists (default from Langfuse compose): `langfuse_default`.

## Start Meko + Proxy

```bash
docker compose -f docker-compose.containers.yml up -d --build
```

App entrypoint:

- `http://localhost:5000/`
- `http://localhost:5000/observe/`

## If Langfuse uses a different network name

```bash
LANGFUSE_DOCKER_NETWORK=<your_network_name> docker compose -f docker-compose.containers.yml up -d --build
```

## Verify routing

```bash
curl -I http://localhost:5000/
curl -I http://localhost:5000/observe/
```

## Stop

```bash
docker compose -f docker-compose.containers.yml down
```
