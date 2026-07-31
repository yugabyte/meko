# Shared ECR credential resolution + pull-secret (re)mint, sourced by
# deploy/up.sh and deploy/controller/redeploy.sh so both behave identically.
#
# The ECR login token that backs the k8s pull secret is only valid ~12h, so the
# secret is re-minted on every run. Both use the same AWS credential source,
# resolved in priority order (see resolve_ecr_creds). Not executable on its own.

# resolve_ecr_creds: make AWS credentials available for `aws ecr get-login-password`.
# Priority:
#   1. AWS_ACCESS_KEY_ID + AWS_SECRET_ACCESS_KEY already in the environment -> use them.
#   2. else AWS_BOT_JSON points to a readable JSON file (e.g. the `aws iam
#      create-access-key` output for the dshubin_bot bot account) -> parse the access
#      key + secret out of it and export them.
#   3. else neither -> log and fall through to the ambient AWS config (i.e. an
#      `aws sso login` session); `get-login-password` then uses whatever's configured.
resolve_ecr_creds() {
  if [ -n "${AWS_ACCESS_KEY_ID:-}" ] && [ -n "${AWS_SECRET_ACCESS_KEY:-}" ]; then
    echo ">> ECR creds: using AWS_ACCESS_KEY_ID / AWS_SECRET_ACCESS_KEY from the environment"
    return 0
  fi
  if [ -n "${AWS_BOT_JSON:-}" ] && [ -f "${AWS_BOT_JSON}" ]; then
    command -v jq >/dev/null 2>&1 || {
      echo "ERROR: AWS_BOT_JSON=${AWS_BOT_JSON} is set but 'jq' is not on PATH (needed to parse it)" >&2
      return 1
    }
    echo ">> ECR creds: reading the dshubin_bot keys from AWS_BOT_JSON=${AWS_BOT_JSON}"
    # Tolerate the common shapes: `aws iam create-access-key` ({.AccessKey.*}), a
    # flat {AccessKeyId,SecretAccessKey}, or {aws_access_key_id,aws_secret_access_key}.
    AWS_ACCESS_KEY_ID="$(jq -re '.AccessKey.AccessKeyId // .AccessKeyId // .aws_access_key_id' "${AWS_BOT_JSON}")" || {
      echo "ERROR: could not read an access key id from ${AWS_BOT_JSON}" >&2; return 1; }
    AWS_SECRET_ACCESS_KEY="$(jq -re '.AccessKey.SecretAccessKey // .SecretAccessKey // .aws_secret_access_key' "${AWS_BOT_JSON}")" || {
      echo "ERROR: could not read a secret access key from ${AWS_BOT_JSON}" >&2; return 1; }
    export AWS_ACCESS_KEY_ID AWS_SECRET_ACCESS_KEY
    return 0
  fi
  echo ">> ECR creds: no AWS_ACCESS_KEY_ID/SECRET and no AWS_BOT_JSON — using the ambient AWS config (assuming an active 'aws sso login' session)"
}

# ensure_pull_secret <ns> <ecr> <secret-name> <region>: resolve creds, mint a fresh
# ECR login token, and (re)create the namespace docker-registry pull secret from it
# (idempotent apply — refreshes the ~12h token). Best-effort local `docker login`
# too, so `docker pull` works on this machine when docker is present.
ensure_pull_secret() {
  local ns="$1" ecr="$2" secret="$3" region="$4"
  resolve_ecr_creds || return 1
  local token
  token="$(aws ecr get-login-password --region "${region}")" || {
    echo "ERROR: 'aws ecr get-login-password' failed — check the ECR credentials (see deploy/README.md, 'ECR access & the pull secret')" >&2
    return 1
  }
  if command -v docker >/dev/null 2>&1; then
    docker login --username AWS --password-stdin "${ecr}" <<<"${token}" >/dev/null 2>&1 || true
  fi
  kubectl -n "${ns}" create secret docker-registry "${secret}" \
    --docker-server="${ecr}" \
    --docker-username=AWS \
    --docker-password="${token}" \
    --dry-run=client -o yaml | kubectl apply -f -
}
