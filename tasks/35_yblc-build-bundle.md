---
status: pending
severity: high
area: yblc
opened: 2026-06-11
---

# Build & package process for yblight-controller + its global services

## Goal

yblight-controller runs alongside two managed **global services** —
`endpoint_storage` and the storage controller (§5.1, §11) — one of each per
placement group. This task builds the **yblc bundle**: a build process that
produces yblight-controller together with its co-located global services as a
single deployable unit, with two outputs:

1. a **VM-ready bundle** — the *primary* output for yblc: release binaries + the
   launch scaffolding needed to run on a bare VM (mirroring today's
   [`scripts/yb-light-ship.sh`](../scripts/yb-light-ship.sh) yb-pageserver
   bundle), and
2. a **container image** (Dockerfile) wrapping the same artifact for Kubernetes.

Principle 2: yblc must run equally well on a VM or on Kubernetes; its only hard
dependency is the YBDB universe over YSQL. The build must not bake in
runtime-specific assumptions.

## Scope

### In scope

- **Build process** that compiles yblight-controller (new crate, TBD) +
  `endpoint_storage` + `storage_controller` in release mode and stages them with
  the launch scaffolding the global tier needs (configs, an otel collector if the
  global tier runs one).
- **VM-ready output (primary).** Extend or mirror `scripts/yb-light-ship.sh` to
  produce a versioned bundle dir launchable on a bare VM: drop it on a host, point
  it at a YBDB connection string + signing key (§9), and start. Carry the
  `libssl`/`libcrypto`/`libicu` co-location + `LD_LIBRARY_PATH` gaps already noted
  in AGENTS.md's bundle sharp-edges.
- **Container image (Dockerfile).** A target packaging the same global-tier bundle
  into an image for k8s — a yblc-global target on the top-level multi-stage
  `Dockerfile`, or a dedicated Dockerfile.

### Out of scope

- **The endpoint image / Helm chart** — that is [task 34](34_k8s-endpoint-deploy.md),
  a different artifact (the per-endpoint blob).
- **yblc's init logic** — [task 36](36_yblc-init.md). This task ships the binaries;
  init wires them up at first start.
- **The yblight-controller crate's feature implementation** — this task is the
  build/packaging skeleton and may begin by bundling the two existing services
  plus a placeholder controller binary.

## Plan / sketch

1. Decide the yblc crate layout (likely a new `yblight_controller/` crate; the dir
   holds only the design doc today). The build needs a controller binary even if it
   starts as a stub.
2. Bundle script:
   `cargo build --release -p yblight_controller -p endpoint_storage -p storage_controller`,
   stage into a versioned dir following the `yb-light-ship.sh` pattern (the AWS
   ship names the dest per-commit).
3. Add a Dockerfile target for the global tier.
4. Mind the build constraints: `protoc ≥ 3.15` for proto3 `optional` (AGENTS.md
   "Build notes"), and the shared-lib co-location gap.

## Notes / links

- Design: [yblight-controller](../yblight_controller/yblight-controller.md) §5.1
  (global services), §11 (managed services), §9 (deployment config), §2 (standalone,
  ybops for VMs).
- Mirrors [`scripts/yb-light-ship.sh`](../scripts/yb-light-ship.sh) (existing
  yb-pageserver bundle/ship) and the top-level `Dockerfile` (Neon-style multi-stage
  build of all binaries).
- Foundational: [task 36](36_yblc-init.md) (init) and [task 34](34_k8s-endpoint-deploy.md)
  (endpoint image) consume this build's outputs.
- AGENTS.md "Build notes" (protoc) and "Outstanding work / sharp edges" (bundle lib
  co-location, `LD_LIBRARY_PATH` stripping) apply directly.
