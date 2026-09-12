#!/usr/bin/env bash
# Build the meko component images on ephemeral AWS instances in parallel and push
# them to quay. Called by pipeline_meko_build.groovy; parameters are documented there.
# Nothing is ever built on the Jenkins agent — it only resolves refs and orchestrates.
set -euo pipefail

: "${QUAY_USER:?QUAY_USER is required}"
: "${QUAY_TOKEN:?QUAY_TOKEN is required}"

VERSION="${VERSION:?VERSION is required}"
QUAY_NS="${QUAY_NS:-quay.io/yugabyte}"
ALL_IMAGES="api-server,ui,inference-gateway,provisioner,mcp-server,rag-agent,langfuse-web,langfuse-worker"
IMAGES="${IMAGES:-$ALL_IMAGES}"
PUSH="${PUSH:-1}"
# Outside the workspace: this runs as root in the agent container, and a
# root-owned directory left behind breaks the next checkout's git clean.
WORK="${WORK:-$(mktemp -d)}"
BUILD_LABEL="${BUILD_LABEL:-local-$$}"
HEARTBEAT_SECONDS="${HEARTBEAT_SECONDS:-30}"

INSTANCE_TYPE="${INSTANCE_TYPE:-c7i.2xlarge}"
BUILDER_AMI="${BUILDER_AMI:-ami-044fea1e64cb7e8ee}"
REGION="${REGION:-us-west-2}"
SUBNET="${SUBNET:-subnet-11798e59}"
SGS="${SGS:-sg-0427247c274837cdc sg-0ab0af5e93728f73a sg-05b6d45d3ab93c852 sg-0271332bdd9fa1313}"
SSH_USER="${SSH_USER:-ec2-user}"
VOLUME_SIZE="${VOLUME_SIZE:-100}"

MEKO_REPO="${MEKO_REPO:-git@github.com:yugabyte/meko.git}"
MEKO_REF="${MEKO_REF:-main}"
MCP_REPO="${MCP_REPO:-git@github.com:yugabyte/meko-mcp-server.git}"
MCP_REF="${MCP_REF:-main}"
LANGFUSE_REPO="${LANGFUSE_REPO:-git@github.com:yugabyte/meko-langfuse.git}"
LANGFUSE_REF="${LANGFUSE_REF:-main}"
YBDB_REPO="${YBDB_REPO:-git@github.com:yugabyte/yugabyte-db.git}"
YBDB_REF="${YBDB_REF:-master}"

VITE_OBSERVE_EMBED_PATH="${VITE_OBSERVE_EMBED_PATH:-/}"
VITE_INFERENCE_GATEWAY_URL="${VITE_INFERENCE_GATEWAY_URL:-/inference}"

MANIFEST="${MANIFEST:-$WORK/build-manifest.txt}"

SSH_OPTS=(-A -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o ConnectTimeout=10 -o LogLevel=ERROR)
[ -n "${SSH_KEY:-}" ] && SSH_OPTS+=(-i "$SSH_KEY")

log() { echo "[$(date -u +%H:%M:%S)] $*"; }

source_of() {
    case "$1" in
        api-server|ui|inference-gateway|provisioner) echo meko ;;
        mcp-server)                                  echo mcp ;;
        rag-agent)                                   echo ybdb ;;
        langfuse-web|langfuse-worker)                echo langfuse ;;
        *) echo "unknown component: $1" >&2; return 1 ;;
    esac
}

image_of() {
    case "$1" in
        api-server)        echo "$QUAY_NS/meko-api-server" ;;
        ui)                echo "$QUAY_NS/meko-ui" ;;
        inference-gateway) echo "$QUAY_NS/meko-inference-gateway" ;;
        provisioner)       echo "$QUAY_NS/meko-provisioner" ;;
        mcp-server)        echo "$QUAY_NS/meko-mcp-server" ;;
        rag-agent)         echo "$QUAY_NS/meko-rag-agent" ;;
        langfuse-web)      echo "$QUAY_NS/meko-langfuse-web" ;;
        langfuse-worker)   echo "$QUAY_NS/meko-langfuse-worker" ;;
    esac
}

repo_of() {
    case "$1" in
        meko)     echo "$MEKO_REPO" ;;
        mcp)      echo "$MCP_REPO" ;;
        langfuse) echo "$LANGFUSE_REPO" ;;
        ybdb)     echo "$YBDB_REPO" ;;
    esac
}

ref_of() {
    case "$1" in
        meko)     echo "$MEKO_REF" ;;
        mcp)      echo "$MCP_REF" ;;
        langfuse) echo "$LANGFUSE_REF" ;;
        ybdb)     echo "$YBDB_REF" ;;
    esac
}

# One builder per image, except where images share a layer cache worth reusing:
# the langfuse pair builds from one context and the worker mounts most of the
# web image's layers.
group_of() {
    case "$1" in
        langfuse-web|langfuse-worker) echo langfuse ;;
        *) echo "$1" ;;
    esac
}

# rag-agent is one directory of a repo orders of magnitude larger than the rest.
sparse_of() { [ "$1" = ybdb ] && echo python/ai/rag_agent || true; }

# No clone on the agent: a ref resolves over the wire, and a sha passes through.
resolve_sha() {
    local src=$1 ref sha
    ref=$(ref_of "$src")
    if [[ "$ref" =~ ^[0-9a-f]{7,40}$ ]]; then
        echo "$ref"
        return 0
    fi
    sha=$(git ls-remote "$(repo_of "$src")" "$ref" | head -1 | cut -f1)
    [ -n "$sha" ] || { echo "cannot resolve $ref in $(repo_of "$src")" >&2; return 1; }
    echo "$sha"
}

quay_token() {
    curl -sf --max-time 30 -u "$QUAY_USER:$QUAY_TOKEN" \
        "https://quay.io/v2/auth?service=quay.io&scope=repository:${1#*/}:pull" \
        | sed -n 's/.*"token"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p'
}

quay_has_tag() {
    local tag=$2 repo=${1#*/} token
    token=$(quay_token "$1")
    [ -n "$token" ] || return 1
    curl -sf --max-time 30 -o /dev/null -H "Authorization: Bearer $token" \
        -H 'Accept: application/vnd.docker.distribution.manifest.v2+json,application/vnd.docker.distribution.manifest.list.v2+json,application/vnd.oci.image.index.v1+json,application/vnd.oci.image.manifest.v1+json' \
        "https://quay.io/v2/${repo}/manifests/${tag}"
}

# The single quotes below are deliberate: these lines expand on the instance.
# shellcheck disable=SC2016
push_line() { [ "$PUSH" = 1 ] && echo 'sudo docker push "$IMG"' || true; }

# Emitted to run ON THE INSTANCE, from the repo root, with IMG and TAG in the env.
# shellcheck disable=SC2016
component_cmds() {
    case "$1" in
        api-server)
            # The script runs "$DOCKER" as one word, so DOCKER="sudo docker" is a
            # command named "sudo docker". Run the whole script under sudo instead.
            if [ "$PUSH" = 1 ]; then
                echo 'sudo env IMAGE="${IMG%:*}" QUAY_USER="$QUAY_USER" QUAY_TOKEN="$QUAY_TOKEN" meko/api_server/build-image.sh --tag "$TAG" --no-latest'
            else
                echo 'sudo env IMAGE="${IMG%:*}" meko/api_server/build-image.sh --tag "$TAG" --no-latest --no-push'
            fi
            ;;
        provisioner)
            echo 'command -v make >/dev/null 2>&1 || sudo dnf install -y -q make'
            echo 'make -C meko/meko-provisioner docker-build IMG="$IMG" CONTAINER_TOOL="sudo docker"'
            push_line
            ;;
        ui)
            echo 'sudo docker build --build-arg VITE_OBSERVE_EMBED_PATH="$VITE_OBSERVE_EMBED_PATH" --build-arg VITE_INFERENCE_GATEWAY_URL="$VITE_INFERENCE_GATEWAY_URL" -t "$IMG" meko/meko_ui'
            push_line
            ;;
        inference-gateway) echo 'sudo docker build -t "$IMG" meko/inference_gateway'; push_line ;;
        mcp-server)        echo 'sudo docker build -t "$IMG" .'; push_line ;;
        rag-agent)         echo 'sudo docker build -t "$IMG" python/ai/rag_agent'; push_line ;;
        langfuse-web)      echo 'sudo docker build -f web/Dockerfile -t "$IMG" .'; push_line ;;
        langfuse-worker)   echo 'sudo docker build -f worker/Dockerfile -t "$IMG" .'; push_line ;;
    esac
}

# A group's components build in order on its one instance, each re-exporting its
# own IMG/TAG.
group_cmds() {
    local component
    for component in ${GROUP_COMPONENTS[$1]}; do
        printf 'export IMG=%q TAG=%q\n' "$(image_of "$component"):${VERSION}" "$VERSION"
        component_cmds "$component"
    done
}

declare -A INSTANCE_ID SHA GROUP_COMPONENTS
IDS=()

# shellcheck disable=SC2329  # invoked via the EXIT trap
cleanup() {
    if [ "${#IDS[@]}" -gt 0 ]; then
        log "terminating instances: ${IDS[*]}"
        aws ec2 terminate-instances --region "$REGION" --instance-ids "${IDS[@]}" >/dev/null || true
    fi
}
trap cleanup EXIT

# shellcheck disable=SC2086  # SGS is a deliberate space-separated multi-arg
launch() {
    local component=$1 id
    id=$(aws ec2 run-instances --region "$REGION" \
        --image-id "$BUILDER_AMI" --instance-type "$INSTANCE_TYPE" \
        --key-name yb-dev-aws-2 --subnet-id "$SUBNET" \
        --security-group-ids $SGS \
        --associate-public-ip-address \
        --block-device-mappings "[{\"DeviceName\":\"/dev/sda1\",\"Ebs\":{\"VolumeSize\":${VOLUME_SIZE},\"VolumeType\":\"gp3\"}}]" \
        --tag-specifications "ResourceType=instance,Tags=[{Key=Name,Value=meko-build-${BUILD_LABEL}-${component}},{Key=yb_task,Value=stress-dev},{Key=yb_owner,Value=spilshchikov},{Key=yb_dept,Value=qa},{Key=autoclean,Value=true},{Key=meko-build,Value=meko-build-${BUILD_LABEL}}]" \
        --count 1 --query 'Instances[0].InstanceId' --output text)
    INSTANCE_ID[$component]="$id"
    IDS+=("$id")
    log "launched $component builder: $id"
}

# describe-instances is eventually consistent: an id run-instances just returned
# can answer InvalidInstanceID.NotFound, and the address arrives later still.
instance_ip() {
    local id=$1 ip
    aws ec2 wait instance-exists --region "$REGION" --instance-ids "$id" 2>/dev/null || true
    for _ in $(seq 1 30); do
        ip=$(aws ec2 describe-instances --region "$REGION" --instance-ids "$id" \
            --query 'Reservations[0].Instances[0].PrivateIpAddress' --output text 2>/dev/null || true)
        case "$ip" in ''|None|null) ;; *) printf '%s' "$ip"; return 0 ;; esac
        sleep 5
    done
    return 1
}

wait_ssh() {
    local ip=$1
    for _ in $(seq 1 30); do
        ssh "${SSH_OPTS[@]}" "$SSH_USER@$ip" 'echo ok' >/dev/null 2>&1 && return 0
        sleep 10
    done
    return 1
}

# shellcheck disable=SC2029  # the tag/ref/creds are meant to expand here, not there
remote_build() {
    local group=$1 ip=$2 src
    src=$(source_of "${GROUP_COMPONENTS[$group]%% *}")
    ssh "${SSH_OPTS[@]}" "$SSH_USER@$ip" \
        "REPO_URL='$(repo_of "$src")' REF='${SHA[$src]}' SPARSE='$(sparse_of "$src")' \
         VITE_OBSERVE_EMBED_PATH='$VITE_OBSERVE_EMBED_PATH' \
         VITE_INFERENCE_GATEWAY_URL='$VITE_INFERENCE_GATEWAY_URL' \
         QUAY_USER='$QUAY_USER' QUAY_TOKEN='$QUAY_TOKEN' \
         BUILD_SCRIPT='$(group_cmds "$group" | base64)' bash -s" <<'EOREMOTE'
set -euo pipefail
export GIT_SSH_COMMAND="ssh -o StrictHostKeyChecking=accept-new"
rm -rf ~/src && git init -q ~/src && cd ~/src
git remote add origin "$REPO_URL"
if [ -n "$SPARSE" ]; then
    git config extensions.partialClone origin
    git config remote.origin.promisor true
    git config remote.origin.partialclonefilter blob:none
    git fetch -q --depth 1 --filter=blob:none origin "$REF"
    git sparse-checkout set "$SPARSE"
else
    git fetch -q --depth 1 origin "$REF"
fi
git checkout -q --detach FETCH_HEAD
echo "$QUAY_TOKEN" | sudo docker login quay.io --username "$QUAY_USER" --password-stdin
echo "$BUILD_SCRIPT" | base64 -d > ~/build.sh
bash -e ~/build.sh
sudo docker logout quay.io
EOREMOTE
}

build_group() {
    local group=$1 ip attempt component src
    ip=$(instance_ip "${INSTANCE_ID[$group]}") \
        || { log "[$group] builder never reported an address"; return 1; }
    log "[$group] waiting for ssh on $ip"
    wait_ssh "$ip" || { log "[$group] ssh never came up"; return 1; }
    for attempt in 1 2; do
        log "[$group] building ${GROUP_COMPONENTS[$group]} at $VERSION (attempt $attempt)"
        if remote_build "$group" "$ip"; then
            if [ "$PUSH" = 1 ]; then
                for component in ${GROUP_COMPONENTS[$group]}; do
                    src=$(source_of "$component")
                    printf '%s\t%s:%s\t%s@%s\n' "$component" "$(image_of "$component")" \
                        "$VERSION" "$src" "${SHA[$src]:0:7}" >> "$MANIFEST"
                done
            fi
            return 0
        fi
        [ "$attempt" -lt 2 ] && { log "[$group] attempt $attempt failed — retrying on the same instance"; sleep 15; }
    done
    return 1
}

IFS=',' read -ra TARGETS <<< "$IMAGES"
rm -rf "$WORK/log" "$MANIFEST"
mkdir -p "$WORK/log"

for component in "${TARGETS[@]}"; do
    src=$(source_of "$component")
    [ -n "${SHA[$src]:-}" ] || SHA[$src]=$(resolve_sha "$src")
done

log "version=$VERSION images=$IMAGES"

PENDING_GROUPS=()
for component in "${TARGETS[@]}"; do
    if quay_has_tag "$(image_of "$component")" "$VERSION"; then
        log "[$component] $(image_of "$component"):${VERSION} already on quay — skipped"
        continue
    fi
    group=$(group_of "$component")
    [ -n "${GROUP_COMPONENTS[$group]:-}" ] || PENDING_GROUPS+=("$group")
    GROUP_COMPONENTS[$group]="${GROUP_COMPONENTS[$group]:-}${GROUP_COMPONENTS[$group]:+ }$component"
done

if [ "${#PENDING_GROUPS[@]}" -eq 0 ]; then
    log "nothing to build"
    exit 0
fi
log "launching ${#PENDING_GROUPS[@]} builder(s): ${PENDING_GROUPS[*]}"

# In the parent, before backgrounding: a child's IDS append would be invisible to
# the EXIT trap, and bash resets the trap in an async subshell, so the instances
# would leak.
for group in "${PENDING_GROUPS[@]}"; do
    launch "$group"
done

declare -A PIDS
for group in "${PENDING_GROUPS[@]}"; do
    build_group "$group" > "$WORK/log/${group}.log" 2>&1 &
    PIDS["$group"]=$!
done

FAILED=()
DONE=()
while [ "${#DONE[@]}" -lt "${#PENDING_GROUPS[@]}" ]; do
    for group in "${PENDING_GROUPS[@]}"; do
        [ -n "${PIDS[$group]:-}" ] || continue
        kill -0 "${PIDS[$group]}" 2>/dev/null && continue
        wait "${PIDS[$group]}" || FAILED+=("$group")
        unset "PIDS[$group]"
        DONE+=("$group")
        echo "===== $group ====="
        cat "$WORK/log/${group}.log"
    done
    if [ "${#DONE[@]}" -lt "${#PENDING_GROUPS[@]}" ]; then
        log "still building: ${!PIDS[*]}"
        sleep "$HEARTBEAT_SECONDS"
    fi
done

if [ "${#FAILED[@]}" -gt 0 ]; then
    log "failed: ${FAILED[*]}"
    exit 1
fi

if [ -f "$MANIFEST" ]; then
    log "published:"
    sort -o "$MANIFEST" "$MANIFEST"
    cat "$MANIFEST"
fi
