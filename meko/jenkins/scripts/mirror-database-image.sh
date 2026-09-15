#!/usr/bin/env bash
#
# Mirror the Meko YugabyteDB build from the Meko ECR to Quay, so a BYOC data plane
# needs one pull credential instead of two.
#
# Every other data-plane image publishes to quay.io/yugabyte/meko-* from
# build-images-aws.sh. The database cannot: it is not built from source here. It is a
# YugabyteDB release artifact carrying MAGE and pg_dist_rag, and a data plane on a
# stock image fails its first migration, so it has to be copied rather than rebuilt.
# See https://github.com/yugabyte/meko/issues/1319.
#
# Runs where AWS credentials already are — the Jenkins agent, or a laptop — NOT on a
# build-images-aws.sh builder: those launch with no instance profile and so cannot
# read ECR at all.
#
# Usage:
#   TAG=2026.1.0.0-b1-meko-2 meko/jenkins/scripts/mirror-database-image.sh
#   TAG=... PUSH=0 meko/jenkins/scripts/mirror-database-image.sh   # resolve only
#
# Env:
#   TAG           REQUIRED — the ECR tag to mirror; reused verbatim on Quay so the
#                 two registries never disagree about what a tag means.
#   QUAY_USER / QUAY_TOKEN   REQUIRED to push. The robot needs write on DST_REPO.
#   ECR_REGISTRY  default 161908593886.dkr.ecr.us-east-2.amazonaws.com
#   ECR_REGION    default us-east-2
#   SRC_REPO      default yugabyte-db/ybdb-meko-release
#   QUAY_NS       default quay.io/yugabyte
#   DST_REPO      default meko-yugabytedb
#   PUSH          default 1
set -euo pipefail

TAG="${TAG:?TAG is required (the ECR tag to mirror, e.g. 2026.1.0.0-b1-meko-2)}"
ECR_REGISTRY="${ECR_REGISTRY:-161908593886.dkr.ecr.us-east-2.amazonaws.com}"
ECR_REGION="${ECR_REGION:-us-east-2}"
SRC_REPO="${SRC_REPO:-yugabyte-db/ybdb-meko-release}"
QUAY_NS="${QUAY_NS:-quay.io/yugabyte}"
DST_REPO="${DST_REPO:-meko-yugabytedb}"
PUSH="${PUSH:-1}"
DOCKER="${DOCKER:-docker}"

SRC="${ECR_REGISTRY}/${SRC_REPO}:${TAG}"
DST="${QUAY_NS}/${DST_REPO}:${TAG}"

log() { printf '==> %s\n' "$*"; }

# Same shape as build-images-aws.sh's check: a tag already on Quay is left alone, so a
# re-run after a partial failure copies only what is missing and a repeat run is a
# no-op rather than a re-push.
quay_has_tag() {
    local token
    token=$(curl -sf --max-time 30 -u "$QUAY_USER:$QUAY_TOKEN" \
        "https://quay.io/v2/auth?service=quay.io&scope=repository:${QUAY_NS#quay.io/}/${DST_REPO}:pull" \
        | sed -n 's/.*"token"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p')
    [ -n "$token" ] || return 1
    curl -sf --max-time 30 -o /dev/null -H "Authorization: Bearer $token" \
        -H 'Accept: application/vnd.docker.distribution.manifest.v2+json,application/vnd.docker.distribution.manifest.list.v2+json,application/vnd.oci.image.index.v1+json,application/vnd.oci.image.manifest.v1+json' \
        "https://quay.io/v2/${QUAY_NS#quay.io/}/${DST_REPO}/manifests/${TAG}"
}

log "source: $SRC"
log "target: $DST"

if [ "$PUSH" != "1" ]; then
    log "PUSH=0 — resolved only, nothing copied"
    exit 0
fi

: "${QUAY_USER:?QUAY_USER is required to push}"
: "${QUAY_TOKEN:?QUAY_TOKEN is required to push}"

if quay_has_tag; then
    log "$DST already exists — nothing to do"
    exit 0
fi

log "logging in to ECR ($ECR_REGION)"
aws ecr get-login-password --region "$ECR_REGION" \
    | "$DOCKER" login "$ECR_REGISTRY" -u AWS --password-stdin

log "logging in to quay.io as $QUAY_USER"
printf '%s' "$QUAY_TOKEN" | "$DOCKER" login quay.io -u "$QUAY_USER" --password-stdin

# imagetools copies the manifest registry-to-registry rather than pulling and
# retagging. That preserves a multi-arch index — a pull/tag/push collapses it to
# whichever architecture the runner happens to be — and moves no layers through here.
log "copying $SRC -> $DST"
"$DOCKER" buildx imagetools create --tag "$DST" "$SRC"

log "mirrored $DST"
