#!/usr/bin/env python3
"""Migrates existing datapacks.artifacts rows (and their S3 objects) from the
old key shape "<user>/<datapack>/<hash>/<filename>" to the new
"artifacts/<user>/<datapack>/<hash>/<filename>" prefix S3Service.
UploadArtifactObject now writes for every new artifact -- this script is
only for the backlog of rows written before that method existed. See the
comment on UploadArtifactObject for why the prefix exists.

Exactly one flag, every run -- never more than one, no default. Not even
--dry-run may combine with a mode: running this against the wrong
database/bucket by forgetting a flag should fail loudly, and a step this
hard to undo should never happen as a side effect of a different one.
  --dry-run      print the inventory (every row, split into in-S3/inline/
                 empty) and exit. Changes nothing.
  --count        same as --dry-run.
  --copy-only    copy each old-key object to its new key and verify it
                 landed. Never touches the DB.
  --update-only  update the DB row for whatever object is already sitting
                 at the new key (copied out of band, or by an earlier
                 --copy-only run). A row whose new key has no object yet is
                 left alone rather than pointed at nothing.
  --delete-only  removes old-key copies for rows already migrated in the DB
                 (s3_key like 'artifacts/%') -- the other two modes only
                 ever look at rows still on the old key, so this is the only
                 one that ever revisits one that's done and cleans up what
                 it left behind.
Run them in that order -- dry-run, copy, dry-run, update, dry-run, delete --
each its own separate invocation.

Safe to re-run: the main query skips any row whose s3_key already starts
with "artifacts/".

Requires: psql, and either mc (MinIO/local dev) or the aws CLI (real S3),
selected via --backend / BACKEND. --psql-opts / PSQL_OPTS carries the DB
connection -- nothing is defaulted, so this never silently connects to the
wrong database via psql's local-socket fallback. Since the flags value
starts with "-h", pass it as --psql-opts=... (with an "=") or via the
PSQL_OPTS env var -- a space-separated "--psql-opts value" makes argparse
mistake it for another option.

Usage (local dev, per LOCAL_DEV_SETUP.md's yugabyted connection):
    PGPASSWORD=yugabyte ./migrate.py \\
        --bucket meko-kb-uploads \\
        --psql-opts="-h 127.0.0.1 -p 5433 -U yugabyte -d yugabyte" \\
        --dry-run

Usage (real S3, using the aws CLI's normal credential chain -- env vars,
instance profile, IRSA, whatever's already configured; nothing here reads
AWS keys directly):
    ./migrate.py --backend aws --bucket meko-kb-uploads \\
        --psql-opts="-h prod-db-host -p 5432 -U meko -d meko" \\
        --dry-run
"""
import argparse
import os
import shlex
import subprocess
import sys


def copy_object_mc(alias, bucket, old_key, new_key):
    return subprocess.run(
        ["mc", "cp", "--quiet", f"{alias}/{bucket}/{old_key}", f"{alias}/{bucket}/{new_key}"]
    ).returncode == 0


def object_exists_mc(alias, bucket, key):
    return subprocess.run(
        ["mc", "stat", "--quiet", f"{alias}/{bucket}/{key}"],
        stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL,
    ).returncode == 0


def delete_object_mc(alias, bucket, key):
    subprocess.run(["mc", "rm", "--quiet", f"{alias}/{bucket}/{key}"], check=True)


def copy_object_aws(bucket, old_key, new_key):
    # aws s3 cp between two s3:// URIs is a server-side copy -- no bytes
    # round-trip through this machine, same as mc cp within one bucket.
    return subprocess.run(
        ["aws", "s3", "cp", "--only-show-errors", f"s3://{bucket}/{old_key}", f"s3://{bucket}/{new_key}"]
    ).returncode == 0


def object_exists_aws(bucket, key):
    return subprocess.run(
        ["aws", "s3api", "head-object", "--bucket", bucket, "--key", key],
        stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL,
    ).returncode == 0


def delete_object_aws(bucket, key):
    subprocess.run(["aws", "s3", "rm", "--only-show-errors", f"s3://{bucket}/{key}"], check=True)


def psql(psql_opts, sql, variables=None):
    # sql goes over stdin, not -c: -c hands the string straight to the server
    # and skips psql's own preprocessing, so a `:'var'` reference in it comes
    # back as a syntax error instead of getting substituted.
    cmd = ["psql"] + shlex.split(psql_opts)
    for name, value in (variables or {}).items():
        cmd += ["-v", f"{name}={value}"]
    cmd += ["-tA", "-F", "|"]
    return subprocess.run(cmd, input=sql, capture_output=True, text=True, check=True).stdout


def rows_needing_migration(args):
    output = psql(args.psql_opts, """
        select id, s3_key from datapacks.artifacts
        where s3_key is not null and s3_key <> '' and s3_key not like 'artifacts/%'
    """)
    return [line.split("|", 1) for line in output.splitlines() if line.strip()]


def update_s3_key(args, row_id, new_key):
    psql(args.psql_opts,
         "update datapacks.artifacts set s3_key = :'new_key' where id = :'id'::uuid",
         {"id": row_id, "new_key": new_key})


def copy_only(args, copy_object, object_exists):
    rows = rows_needing_migration(args)
    if not rows:
        print("nothing to copy")
        return

    total = copied = 0
    for row_id, old_key in rows:
        total += 1
        new_key = f"artifacts/{old_key}"
        print(f"[{row_id}] {old_key} -> {new_key}")

        if not copy_object(old_key, new_key):
            print("  copy failed", file=sys.stderr)
            continue
        if not object_exists(new_key):
            print("  copy did not verify at new key", file=sys.stderr)
            continue
        copied += 1

    print(f"done: {copied}/{total} copied")


def update_only(args, object_exists):
    rows = rows_needing_migration(args)
    if not rows:
        print("nothing to update")
        return

    total = updated = 0
    for row_id, old_key in rows:
        total += 1
        new_key = f"artifacts/{old_key}"
        print(f"[{row_id}] {old_key} -> {new_key}")

        if not object_exists(new_key):
            print("  no object at the new key yet, skipping (nothing to point the DB row at)", file=sys.stderr)
            continue
        update_s3_key(args, row_id, new_key)
        updated += 1

    print(f"done: {updated}/{total} updated")


def delete_only(args, object_exists, delete_object):
    output = psql(args.psql_opts, "select id, s3_key from datapacks.artifacts where s3_key like 'artifacts/%'")
    rows = [line.split("|", 1) for line in output.splitlines() if line.strip()]
    if not rows:
        print("nothing already migrated")
        return

    prefix_len = len("artifacts/")
    total = removed = 0
    for row_id, new_key in rows:
        old_key = new_key[prefix_len:]
        if not object_exists(old_key):
            continue
        total += 1
        print(f"[{row_id}] {old_key} (old copy of {new_key})")
        delete_object(old_key)
        removed += 1

    print(f"done: {removed}/{total} old copies removed")


def report(args):
    # DB-only: size_bytes and s3_key already say everything needed, so this
    # doesn't touch S3.
    output = psql(args.psql_opts, """
        select id, filename, size_bytes, s3_key
        from datapacks.artifacts
        order by created_at
    """)
    rows = [line.split("|", 3) for line in output.splitlines() if line.strip()]

    in_s3 = [(row_id, filename, s3_key) for row_id, filename, _, s3_key in rows if s3_key]
    inline = [(row_id, filename) for row_id, filename, _, s3_key in rows if not s3_key]
    empty = [(row_id, filename, s3_key) for row_id, filename, size_bytes, s3_key in rows if int(size_bytes) == 0]

    print(f"=== all files ({len(rows)}) ===")
    for row_id, filename, size_bytes, s3_key in rows:
        print(f"[{row_id}] {filename}  ({size_bytes} bytes, {s3_key or 'inline'})")

    print(f"\n=== in S3 ({len(in_s3)}) ===")
    for row_id, filename, s3_key in in_s3:
        print(f"[{row_id}] {filename}  {s3_key}")

    print(f"\n=== inline in DB ({len(inline)}) ===")
    for row_id, filename in inline:
        print(f"[{row_id}] {filename}")

    print(f"\n=== empty, 0 bytes ({len(empty)}) ===")
    for row_id, filename, s3_key in empty:
        print(f"[{row_id}] {filename}  ({s3_key or 'inline'})")

    print(f"\ntotal: {len(rows)} files -- {len(in_s3)} in S3, {len(inline)} inline, {len(empty)} empty")


def main():
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument("--backend", choices=["mc", "aws"], default=os.environ.get("BACKEND", "mc"))
    parser.add_argument("--mc-alias", default=os.environ.get("MC_ALIAS", "local"))
    parser.add_argument("--bucket", default=os.environ.get("BUCKET"),
                         help="artifacts bucket name (or set BUCKET)")
    parser.add_argument("--psql-opts", default=os.environ.get("PSQL_OPTS"),
                         help="psql connection flags, e.g. '-h 127.0.0.1 -p 5433 -U yugabyte -d yugabyte' (or set PSQL_OPTS)")
    parser.add_argument("--dry-run", action="store_true", help="print the inventory and exit; changes nothing")
    parser.add_argument("--count", action="store_true", help="same as --dry-run")
    parser.add_argument("--copy-only", action="store_true", help="just copy old-key objects to their new key")
    parser.add_argument("--update-only", action="store_true",
                         help="just update the DB row for whatever object is already at the new key")
    parser.add_argument("--delete-only", action="store_true",
                         help="just remove lingering old-key copies for rows already migrated in the DB")
    args = parser.parse_args()

    flags = [args.dry_run, args.count, args.copy_only, args.update_only, args.delete_only]
    if sum(flags) != 1:
        parser.error("pass exactly one of --dry-run, --count, --copy-only, --update-only, --delete-only "
                      "-- never more than one, and never none")
    if not args.bucket:
        parser.error("--bucket (or BUCKET) is required")
    if not args.psql_opts:
        parser.error("--psql-opts (or PSQL_OPTS) is required")

    if args.dry_run or args.count:
        report(args)
        return

    if args.backend == "mc":
        copy_object = lambda old, new: copy_object_mc(args.mc_alias, args.bucket, old, new)
        object_exists = lambda key: object_exists_mc(args.mc_alias, args.bucket, key)
        delete_object = lambda key: delete_object_mc(args.mc_alias, args.bucket, key)
    else:
        copy_object = lambda old, new: copy_object_aws(args.bucket, old, new)
        object_exists = lambda key: object_exists_aws(args.bucket, key)
        delete_object = lambda key: delete_object_aws(args.bucket, key)

    if args.copy_only:
        copy_only(args, copy_object, object_exists)
    elif args.update_only:
        update_only(args, object_exists)
    elif args.delete_only:
        delete_only(args, object_exists, delete_object)


if __name__ == "__main__":
    main()
