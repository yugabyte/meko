# Meko Provisioner — Usage Guide

Kubernetes operator that provisions per-tenant namespaces with MCP server, RAG worker, and Langfuse workloads when a `MekoUserAccount` custom resource is created. Cleans up everything on deletion via a finalizer.

## Prerequisites

- Go >= 1.24
- Access to a Kubernetes cluster (for deployment/e2e) or none needed for unit tests (envtest bundles its own API server + etcd)
- `kubectl` configured to your target cluster (for deployment)
- Docker (for building container images)

---

## Deploying the Operator

### Generate a standalone install YAML

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