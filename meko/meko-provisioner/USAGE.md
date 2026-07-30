# Meko Provisioner — Usage Guide

Kubernetes operator that provisions per-tenant namespaces with MCP server, RAG worker, and Langfuse workloads when a `MekoUserAccount` custom resource is created. Cleans up everything on deletion via a finalizer.

## Prerequisites

- Go >= 1.24
- Access to a Kubernetes cluster (for deployment/e2e) or none needed for unit tests (envtest bundles its own API server + etcd)
- `kubectl` configured to your target cluster (for deployment)
- Docker (for building container images)

---

## Building

### From the repo root (via build.sh)

```bash
# Build only the provisioner binary
./build.sh provisioner

# Build all targets (cli + api + provisioner)
./build.sh

# Build with lint
./build.sh provisioner -l
```

The binary lands at `build/latest/meko-provisioner`.

### From the meko-provisioner directory (via Make)

```bash
cd meko-provisioner

# Build the manager binary (includes manifest generation, fmt, vet)
make build

# Binary output: bin/manager
```

---

## Running Tests

### Unit + envtest tests

```bash
cd meko-provisioner

# Full test pipeline (generate manifests, fmt, vet, download envtest binaries, run tests)
make test
```

This runs all tests except e2e, including:

- **Type tests** (`api/v1/`) — tier constants, deep copy correctness
- **Controller envtest tests** (`internal/controller/`) — namespace creation, deployment/service provisioning, status updates, per-tier variation, idempotency

To run tests directly without the Make wrapper:

```bash
cd meko-provisioner

# Set up envtest binaries (first time only)
make setup-envtest

# Run specific packages
KUBEBUILDER_ASSETS="$(pwd)/bin/k8s/1.35.0-linux-amd64" go test ./api/v1/ -v
KUBEBUILDER_ASSETS="$(pwd)/bin/k8s/1.35.0-linux-amd64" go test ./internal/controller/ -v
```

### E2E tests (requires Kind)

```bash
cd meko-provisioner

# Creates a Kind cluster, runs e2e, tears down
make test-e2e
```

---

## Linting

```bash
cd meko-provisioner

# Run golangci-lint (downloads v2.8.0 locally on first run)
GOTOOLCHAIN=go1.25.8 make lint

# Auto-fix lint issues
GOTOOLCHAIN=go1.25.8 make lint-fix
```

> **Note:** The `GOTOOLCHAIN=go1.25.8` is needed because the module targets Go 1.25 (required by controller-runtime), while the system Go may be 1.24.

---

## Deploying the Operator

### Option A: Run locally (dev mode, uses your kubeconfig)

```bash
cd meko-provisioner

# Install CRDs into the cluster
make install

# Run the controller on your machine (not in-cluster)
make run
```

### Option B: Generate a standalone install YAML

```bash
cd meko-provisioner

export IMG=<your-registry>/meko-provisioner:<tag>
make build-installer IMG=$IMG

# Output: dist/install.yaml — apply to any cluster with:
kubectl apply -f dist/install.yaml
```

---

## Using the MekoUserAccount CRD

### 1. Create the tenants namespace

All `MekoUserAccount` CRs live in the `meko-tenants` namespace:

```bash
kubectl apply -f config/samples/namespace_meko-tenants.yaml
```

Or manually:

```yaml
apiVersion: v1
kind: Namespace
metadata:
  name: meko-tenants
```

### 2. Create a MekoUserAccount

```yaml
apiVersion: saas.mekodata.ai/v1
kind: MekoUserAccount
metadata:
  name: mekouseraccount-abc123
  namespace: meko-tenants
spec:
  accountId: "abc123"
  accountTier: "free"              # free | pro | enterprise
  mcpImage: "your-registry/mcp-server:latest"
  ragImage: "your-registry/rag-worker:latest"
  langfuseImage: "langfuse/langfuse:latest"
  mcpReplicas: 1                   # optional, defaults to 1
  ragReplicas: 1                   # optional, defaults to 1
```

Apply it:

```bash
kubectl apply -f config/samples/saas_v1_mekouseraccount.yaml
```

### 3. What the operator creates

When you apply a `MekoUserAccount`, the controller:

| Resource | Name | Namespace | Details |
|----------|------|-----------|---------|
| Namespace | `tenant-{accountId}` | (cluster-scoped) | Isolated tenant namespace |
| Deployment | `mcp-server` | `tenant-{accountId}` | Image from `spec.mcpImage`, replicas from `spec.mcpReplicas` |
| Deployment | `rag-worker` | `tenant-{accountId}` | Image from `spec.ragImage`, replicas from `spec.ragReplicas` |
| Deployment | `langfuse` | `tenant-{accountId}` | Image from `spec.langfuseImage`, 1 replica |
| Service | `mcp-server` | `tenant-{accountId}` | ClusterIP, port 8080 |
| Service | `langfuse` | `tenant-{accountId}` | ClusterIP, port 3000 |

All deployments have:
- `automountServiceAccountToken: false`
- Environment variables `MEKO_ACCOUNT_ID` and `MEKO_ACCOUNT_TIER`
- Labels `app=<name>` and `account-id=<accountId>`

### 4. Check status

```bash
kubectl get mekouseraccounts -n meko-tenants

kubectl describe mekouseraccount mekouseraccount-abc123 -n meko-tenants
```

Status fields:
- `phase` — `Active` when all resources are provisioned, `Deleting` during teardown
- `namespaceName` — the created tenant namespace (e.g. `tenant-abc123`)

### 5. Verify tenant resources

```bash
kubectl get all -n tenant-abc123
```

### 6. Delete a tenant

```bash
kubectl delete mekouseraccount mekouseraccount-abc123 -n meko-tenants
```

The finalizer (`saas.mekodata.ai/cleanup`) ensures the entire `tenant-{accountId}` namespace is deleted (cascading all deployments and services inside) before the CR is removed.

### Spec reference

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `accountId` | string | yes | — | Unique account identifier (min 1 char) |
| `accountTier` | string | yes | — | One of `free`, `pro`, `enterprise` |
| `mcpImage` | string | yes | — | Container image for MCP server |
| `ragImage` | string | yes | — | Container image for RAG worker |
| `langfuseImage` | string | yes | — | Container image for Langfuse |
| `mcpReplicas` | int32 | no | 1 | MCP server replica count |
| `ragReplicas` | int32 | no | 1 | RAG worker replica count |

---

## Regenerating Manifests

After editing `api/v1/mekouseraccount_types.go` or RBAC markers:

```bash
cd meko-provisioner

make generate    # Regenerate DeepCopy methods
make manifests   # Regenerate CRD YAML + RBAC ClusterRole
```
