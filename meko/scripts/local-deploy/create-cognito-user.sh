#!/usr/bin/env bash
# Create a Cognito user for local Meko (the signin page hides self-service signup).
# Wraps the two AWS CLI calls: admin-create-user + admin-set-user-password.
#
# Prereq: you must be authenticated to the mekodata-dev AWS account first, e.g.
#   aws sso login --profile <profile>     (then pass --profile, or export AWS_PROFILE)
#   — or export AWS_ACCESS_KEY_ID / AWS_SECRET_ACCESS_KEY env creds.
#
# Pool/region default to COGNITO_POOL_ID / COGNITO_REGION from ./.env; override
# with flags. Email/password are prompted if not passed.
#
# Flags:
#   --email <email>       user's email (also the username); prompted if omitted.
#   --password <pw>       permanent password; prompted (hidden) if omitted.
#   --pool-id <id>        Cognito User Pool ID (default: COGNITO_POOL_ID from .env).
#   --region <region>     AWS region (default: COGNITO_REGION from .env).
#   --profile <name>      AWS named profile to use for every aws call.
#   -h, --help            show this help.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ENV_FILE="${SCRIPT_DIR}/.env"

GREEN=$'\033[0;32m'; RED=$'\033[0;31m'; YEL=$'\033[0;33m'; CYAN=$'\033[0;36m'; NC=$'\033[0m'
info()  { printf "${CYAN}▸${NC} %s\n" "$*"; }
ok()    { printf "${GREEN}✓${NC} %s\n" "$*"; }
warn()  { printf "${YEL}⚠${NC} %s\n" "$*"; }
die()   { printf "${RED}✗${NC} %s\n" "$*" >&2; exit 1; }

# Pull pool/region defaults from .env if present (don't clobber the environment).
POOL_ID="${COGNITO_POOL_ID:-}"
REGION="${COGNITO_REGION:-}"
if [ -f "${ENV_FILE}" ]; then
  POOL_ID="${POOL_ID:-$(sed -n 's/^COGNITO_POOL_ID=//p' "${ENV_FILE}" | head -1)}"
  REGION="${REGION:-$(sed -n 's/^COGNITO_REGION=//p' "${ENV_FILE}" | head -1)}"
fi

EMAIL=""
PASSWORD=""
PROFILE=""
while [ $# -gt 0 ]; do
  case "$1" in
    --email)    EMAIL="$2"; shift 2 ;;
    --password) PASSWORD="$2"; shift 2 ;;
    --pool-id)  POOL_ID="$2"; shift 2 ;;
    --region)   REGION="$2"; shift 2 ;;
    --profile)  PROFILE="$2"; shift 2 ;;
    -h|--help)  sed -n '2,18p' "$0"; exit 0 ;;
    *) die "unknown flag: $1 (see --help)" ;;
  esac
done

command -v aws >/dev/null 2>&1 || die "aws CLI not found on PATH (install + configure it for the mekodata-dev account)"
[ -n "${POOL_ID}" ] || die "no Cognito pool id (set COGNITO_POOL_ID in .env or pass --pool-id)"
[ -n "${REGION}" ]  || die "no AWS region (set COGNITO_REGION in .env or pass --region)"

# Every aws call carries --profile when one was given.
AWS_ARGS=()
[ -n "${PROFILE}" ] && AWS_ARGS+=(--profile "${PROFILE}")

# Preflight: confirm we're authenticated to AWS before touching Cognito.
info "checking AWS credentials"
aws "${AWS_ARGS[@]}" sts get-caller-identity >/dev/null 2>&1 \
  || die "not authenticated to AWS. Log in to the mekodata-dev account first (e.g. 'aws sso login --profile <profile>' then pass --profile, or export env creds), then re-run."

[ -n "${EMAIL}" ] || { printf "${CYAN}▸${NC} email: "; read -r EMAIL; }
[ -n "${EMAIL}" ] || die "email is required"
if [ -z "${PASSWORD}" ]; then
  printf "${CYAN}▸${NC} permanent password: "; read -rs PASSWORD; printf "\n"
fi
[ -n "${PASSWORD}" ] || die "password is required"

# 1. Create the user. SUPPRESS skips the invitation email; email_verified=true
#    skips the confirmation-code step.
info "creating Cognito user ${EMAIL} in pool ${POOL_ID} (${REGION})"
aws "${AWS_ARGS[@]}" cognito-idp admin-create-user \
  --user-pool-id "${POOL_ID}" \
  --username "${EMAIL}" \
  --user-attributes Name=email,Value="${EMAIL}" Name=email_verified,Value=true \
  --message-action SUPPRESS \
  --region "${REGION}"

# 2. Set a permanent password (otherwise the first login forces a
#    temporary-password change flow).
info "setting a permanent password"
aws "${AWS_ARGS[@]}" cognito-idp admin-set-user-password \
  --user-pool-id "${POOL_ID}" \
  --username "${EMAIL}" \
  --password "${PASSWORD}" \
  --permanent \
  --region "${REGION}"

ok "user ${EMAIL} ready — sign in at http://localhost:${UI_HOST_PORT:-5000}/signin"
