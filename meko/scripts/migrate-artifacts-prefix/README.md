# migrate.py

Moves `datapacks.artifacts` off the old S3 key shape onto the `artifacts/` prefix.
Exactly one flag per run, always -- never two together, not even `--dry-run` with a
mode. Check with `--dry-run`, then run the mode for real, as two separate commands.

## Setup

```bash
# local dev (yugabyted + MinIO, per LOCAL_DEV_SETUP.md) -- --backend mc is the
# default but needs the mc binary installed; this environment doesn't have it,
# so use --backend aws pointed at MinIO instead:
export PGPASSWORD=yugabyte
export AWS_ACCESS_KEY_ID=meko AWS_SECRET_ACCESS_KEY=mekosecret AWS_REGION=us-east-2
export AWS_ENDPOINT_URL_S3=http://localhost:19094
PSQL_OPTS="-h 127.0.0.1 -p 5433 -U yugabyte -d yugabyte"
BUCKET=meko-kb-uploads
BACKEND="--backend aws"

# real S3
export AWS_ACCESS_KEY_ID=... AWS_SECRET_ACCESS_KEY=... AWS_REGION=...
PSQL_OPTS="-h prod-db-host -p 5432 -U meko -d meko"
BUCKET=meko-kb-uploads
BACKEND="--backend aws"
```

If `mc` is actually installed (`mc alias set local http://localhost:9000 meko mekosecret`
first), drop `BACKEND` and the `AWS_*`/`AWS_ENDPOINT_URL_S3` exports and use the default.

## Check what's there

```bash
python3 migrate.py --bucket "$BUCKET" --psql-opts="$PSQL_OPTS" $BACKEND --dry-run
```

## 1. Copy

```bash
python3 migrate.py --bucket "$BUCKET" --psql-opts="$PSQL_OPTS" $BACKEND --copy-only
```

## 2. Update the DB

```bash
python3 migrate.py --bucket "$BUCKET" --psql-opts="$PSQL_OPTS" $BACKEND --update-only
```

## 3. Delete the old copies

Only after step 2 has been live long enough to be confident nothing needs the old keys.

```bash
python3 migrate.py --bucket "$BUCKET" --psql-opts="$PSQL_OPTS" $BACKEND --delete-only
```

Safe to re-run any step: each one only acts on rows it hasn't already finished. Run
`--dry-run` in its own command whenever you want to check the inventory again.
