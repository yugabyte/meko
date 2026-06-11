---
status: pending
severity: medium
area: yblc
opened: 2026-06-11
---

# First-time login: bootstrap the first admin user + empty tenant pool

## Goal

After init ([task 36](36_yblc-init.md), §9.3) a fresh placement group has a schema
but no users. Implement the "first-time login" flow that creates the **first admin
user** (email as username + password) plus the initial setup — at minimum one
**empty tenant pool** — so an admin lands in a usable placement group. Per §10.1,
users are either admin (placement-group-wide) or pool-granted; this task creates the
bootstrap admin.

## Scope

### In scope

- **First-admin bootstrap.** A one-time, guarded flow (runs only when no admin
  exists) that creates the first **admin** user with email-as-username + password.
  Maps to the `user` entity (§10.1) with admin status — all pools, no
  `user_pool_grant` rows.
- **Initial setup.** Create one empty **tenant pool** (§4.1 / §10.1) — empty is a
  valid resting state (§4) — so the admin has somewhere to create tenants next.
- **Credential storage.** Store the admin credential safely (password hashing). The
  user-facing API *authentication mechanism* is deferred (§12); this task creates
  the `user` + credential record per the §10.1 model and enough of a login to verify
  it round-trips — not the general auth scheme.
- **Idempotency / re-entrancy** (§8): never create a second admin if one already
  exists; re-running is a no-op.

### Out of scope

- **Full RBAC / multi-user management** and per-pool grant UX — the model exists
  (§10.1 / §10.2); management flows come later.
- **The general API-auth scheme** — §12, explicitly deferred.
- **Tenants / clusters / endpoints inside the pool** — [task 38](38_yblc-create-tenant-flow.md).

## Plan / sketch

1. Schema: `user`, `user_pool_grant`, `tenant_pool` per §10.1 / §10.2 (admin users
   skip grants; pool names unique within the placement group; deletes restricted).
   Likely lands with [task 36](36_yblc-init.md)'s migrations.
2. Bootstrap gate: detect "no admin exists" so the flow is strictly first-time.
3. Create the admin (email username + hashed password) and one empty tenant pool.
4. Minimal login path to confirm the credential authenticates.

## Notes / links

- Design: [yblight-controller](../yblight_controller/yblight-controller.md) §10.1
  (user / admin / tenant_pool entities), §10.2 (schema-enforced invariants), §4.1
  (logical constructs), §12 (API auth deferred).
- "Decided" per §10.1: the pool is the finest grain of user access; there are no
  tenant-scoped users.
- Depends on [task 36](36_yblc-init.md) (schema + init). Precedes
  [task 38](38_yblc-create-tenant-flow.md) — creating a tenant needs a pool + an
  admin to own the action.
