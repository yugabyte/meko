# Runbook: repair a Langfuse-less datapack

Use this when a user reports every tool call against one datapack failing with:

```
No Langfuse project found for datapack <id>
```

Tracked as [MEKO-621](https://yugabyte.atlassian.net/browse/MEKO-621).
Sibling issue on the repair path: [MEKO-591](https://yugabyte.atlassian.net/browse/MEKO-591) /
[meko-mcp-server#332](https://github.com/yugabyte/meko-mcp-server/issues/332).

## What's wrong

`datapack_create` writes the datapack row, then calls
`LangfuseService.ProvisionForDatapack`. If Langfuse returns an error or the
20-second request times out, the service logs the error and returns `nil`.
The handler does not check that result, so `datapack_create` still returns
success. Later MCP calls fail because no row exists in
`meko_system.langfuse_project_mapping`.

The UI retries provisioning for default-named datapacks every five minutes and
for the first owned datapack when its owner opens Observe. An owner can also
call `POST /api/v1/datapacks/<id>/langfuse/project-keys` with write
authentication. Use this one-shot only when those paths do not repair the pack,
such as when its owner cannot open the UI.

## When to run this

Run it when all three are true:

- The user's error string is exactly `No Langfuse project found for datapack <id>`.
- `meko_system.datapacks` has a row for that datapack id.
- `meko_system.langfuse_project_mapping` has **no** row for that datapack id.

If a mapping row already exists, this is a different failure. Don't run the tool
until you know why.

## Prereqs

- Prod AWS access, `kubectl` context `meko-prod`.
- `psql` on your path (`/opt/homebrew/opt/libpq/bin/psql` on macOS).
- Go toolchain, and this repo checked out.

## Steps

### 1. Read the prod api_server config

```bash
POD=$(kubectl --context meko-prod -n meko-system get pods -l app=meko-api-server -o name | head -1)
kubectl --context meko-prod -n meko-system exec "$POD" -- printenv \
  | grep -E "^(SYSTEM_DB|LANGFUSE_HOST)"
kubectl --context meko-prod -n meko-system get deploy meko-api-server -o yaml \
  | grep -- '--langfuse-admin-api-key='
```

The `printenv` output has the DB settings. Keep its `SYSTEM_DB_NAME`; production
currently uses the `meko_system` database. The Deployment yaml has the Langfuse
admin key on a container arg. Copy these values into your shell.

### 2. Port-forward the system DB

```bash
kubectl --context meko-prod -n yb-prod-meko-prod-system-data \
  port-forward pod/ybmeko-prod-s-us-east-2a-oimt-yb-tserver-0 15433:5433 &
```

The tserver Service is headless, so forward the pod, not the Service.

### 3. Preflight

Confirm the pack is stuck and identify the owner:

```bash
export PATH="/opt/homebrew/opt/libpq/bin:$PATH"
export SYSTEM_DB_NAME='<SYSTEM_DB_NAME from step 1>'
export SYSTEM_DB_USER_NAME='<SYSTEM_DB_USER_NAME from step 1>'
export SYSTEM_DB_PASSWORD='<SYSTEM_DB_PASSWORD from step 1>'
export PGPASSWORD="$SYSTEM_DB_PASSWORD"
psql -h 127.0.0.1 -p 15433 -U "$SYSTEM_DB_USER_NAME" -d "$SYSTEM_DB_NAME" -c "
  SELECT dp.datapack_id, dp.datapack_name, dp.user_id, u.user_email
  FROM meko_system.datapacks dp JOIN meko_system.users u ON u.user_id = dp.user_id
  WHERE dp.datapack_id = '<DATAPACK_ID>';
  SELECT count(*) AS mapping_rows FROM meko_system.langfuse_project_mapping
  WHERE datapack_id = '<DATAPACK_ID>';
"
```

Expect one datapack row and `mapping_rows = 0`.

### 4. Run the repair

Port-forward the in-cluster Langfuse Service. This keeps the admin key off the
public network:

```bash
kubectl --context meko-prod -n meko-langfuse \
  port-forward svc/langfuse-web 13000:3000 &

cd meko/api_server
export SYSTEM_DB_HOST=127.0.0.1 SYSTEM_DB_PORT=15433
export SYSTEM_DB_SSLMODE=disable
export LANGFUSE_HOST='http://127.0.0.1:13000'
export LANGFUSE_ADMIN_API_KEY='<from step 1>'
go run ./cmd/repair-langfuse-mapping <DATAPACK_ID>
```

Success looks like:

```
INFO msg=preflight        datapack_id=... datapack_name=... user_id=... email=...
INFO msg="langfuse project api keys created" project_id=cmXXX...
INFO msg=provisioned      langfuse_public_key_prefix=pk-lf-XX
INFO msg="repair ok"      datapack_id=...
```

The tool is idempotent. Rerunning on an already-repaired pack is a no-op.

### 5. Verify

```bash
psql -h 127.0.0.1 -p 15433 -U "$SYSTEM_DB_USER_NAME" -d "$SYSTEM_DB_NAME" -c "
  SELECT datapack_id, langfuse_project_id, langfuse_project_name,
         substr(langfuse_public_key,1,12) AS pk_prefix, created_at
  FROM meko_system.langfuse_project_mapping
  WHERE datapack_id = '<DATAPACK_ID>';
"
```

Expect one row with a fresh `created_at`. Then ask the reporter to retry their
original call, or try `datapack_describe` yourself. The `No Langfuse project
found` error is gone.

### 6. Tell the reporter

Reply on the original thread with the fix and the tickets:

- MEKO-621 tracks the atomic-create defect.
- MEKO-591 / meko-mcp-server#332 tracks the update-repair path.

Keep the message factual: "Your datapack is repaired, root cause is MEKO-621."

## Rollback

Deleting only the mapping row permanently orphans the remote Langfuse project.
If the tool inserted a bad mapping, first record the project id:

```sql
SELECT langfuse_project_id
FROM meko_system.langfuse_project_mapping
WHERE datapack_id = '<DATAPACK_ID>';
```

Delete that project in the Langfuse admin UI. Then remove the local mapping:

```sql
DELETE FROM meko_system.langfuse_project_mapping
WHERE datapack_id = '<DATAPACK_ID>';
```

Do not rerun the one-shot until both records are gone. A rerun after deleting
only the mapping can create a second, suffixed project.

The UI caches project-key provisioning for five minutes in
`meko:langfuse-project-keys-cache:v1`. Before testing the rollback in a browser,
wait for that cache to expire or remove the session storage entry and reload.

## Gotchas

- **Do not infer the upstream cause from the datapack name.** Both `Hallway
  Events` and `adlc/proj/agentk/agent_core` needed repair. A name collision gets
  a random suffix, but other name rejections can still fail provisioning.
- **Check the api_server logs for the original error.** The confirmed defect is
  that the service swallows provisioning errors and the handler returns
  success. The original Langfuse failure can have more than one cause.
- **`kubectl exec` on prod pods** is gated by the local-dev classifier as
  `[Production Reads]`. If your session denies it, run the `printenv` and
  Deployment lookup steps yourself and paste the values in.
