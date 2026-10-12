#!/usr/bin/env bash
# Give the seeded accounts what MCP needs to serve them:
#   - one Meko PAT per account (mcp_personal_access_tokens stores the hash; the
#     mcp-load-pats Secret holds "<account> <token>" lines for the load generator);
#   - a Langfuse mapping for every seeded datapack, all pointing at the one project
#     Langfuse created at startup (traces per account is fine; MCP fails every tool
#     call for a datapack without a mapping row).
#
# It mints a PAT for every seeded account (acct-*@scale.test) and maps datapack
# indexes below SEEDED (default 1,000,000) plus the 50,000-datapack account.
#
#   SEEDED=3000000 ./seed_stack.sh
set -euo pipefail

CTX=${CTX:-amiram.mizne@amp-bench.us-east-2.eksctl.io}
NS=${NS:-meko-onemil}
K=(kubectl --context "$CTX" -n "$NS")
sql() { "${K[@]}" exec -i yb-tserver-0 -c yb-tserver -- ysqlsh -h yb-tserver-0.yb-tservers -v ON_ERROR_STOP=1 -q "$@"; }
secret() { "${K[@]}" get secret stack-secrets -o jsonpath="{.data.$1}" | base64 -d; }

# Same shape as api_server auth.GeneratePAT: mko_tkn_ + 24 random bytes, lowercase
# unpadded base32. Anything else fails the format check before any lookup. Only
# accounts the seeder has created (they own a datapack) get one.
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
sql -At -c "SELECT user_id FROM meko_system.users WHERE user_email LIKE 'acct-%@scale.test'" >"$work/users"
python3 - "$work" <<'PY'
import base64, secrets, sys, os
work = sys.argv[1]
users = [u.strip() for u in open(os.path.join(work, "users")) if u.strip()]
with open(os.path.join(work, "pats.sql"), "w") as sqlf, open(os.path.join(work, "pats"), "w") as out:
    sqlf.write("DELETE FROM meko_system.mcp_personal_access_tokens WHERE name = 'onemil-load';\n")
    for u in users:
        p = "mko_tkn_" + base64.b32encode(secrets.token_bytes(24)).decode().rstrip("=").lower()
        sqlf.write("INSERT INTO meko_system.mcp_personal_access_tokens (user_id, name, token_prefix, "
                   f"token_hash, expires_at) VALUES ('{u}', 'onemil-load', '{p[:16]}', "
                   f"encode(sha256(convert_to('{p}', 'UTF8')), 'hex'), NOW() + INTERVAL '30 days');\n")
        out.write(f"{u} {p}\n")
PY
sql <"$work/pats.sql"
"${K[@]}" create secret generic mcp-load-pats --from-file=pats="$work/pats" --dry-run=client -o yaml |
  "${K[@]}" apply --server-side --force-conflicts -f - >/dev/null
echo "seeded $(wc -l <"$work/pats") PATs, one per account"

[ "${SKIP_LANGFUSE:-}" ] && exit 0 # PATs only
pub=$(secret langfuse-public-key) sec=$(secret langfuse-secret-key)
# One statement per 100k datapack names keeps each transaction bounded.
# The main range, then the 50,000-datapack account's own range (workload.BIG_ACCOUNT_START).
# -f %.0f: macOS seq prints 1000000 and up as 1e+06, which printf %07d rejects.
for lo in $(seq -f %.0f 0 100000 $((${SEEDED:-1000000} - 1))) 5000000; do
  hi=$((lo + 99999))
  sql -c "INSERT INTO meko_system.langfuse_project_mapping
            (datapack_id, langfuse_project_id, langfuse_project_name, langfuse_public_key, langfuse_secret_key)
          SELECT datapack_id, 'onemil-' || datapack_id::text, 'onemil-account', '$pub', '$sec'
            FROM meko_system.datapacks
           WHERE datapack_name BETWEEN 'onemil-$(printf %07d "$lo")' AND 'onemil-$(printf %07d "$hi")'
          ON CONFLICT (datapack_id) DO UPDATE
             SET langfuse_public_key = EXCLUDED.langfuse_public_key,
                 langfuse_secret_key = EXCLUDED.langfuse_secret_key, updated_at = NOW();"
  echo "mapped datapacks $lo-$hi"
done
sql -c "SELECT count(*) AS mapped FROM meko_system.langfuse_project_mapping;"
