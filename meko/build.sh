#!/usr/bin/env bash
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "$0")" && pwd)"
BUILD_DIR="${REPO_ROOT}/build/latest"
GO_MIN_VERSION="1.24"

# ── Colours (disabled when piped) ────────────────────────────────────────────
if [ -t 1 ]; then
    GREEN='\033[0;32m' RED='\033[0;31m' YELLOW='\033[0;33m' CYAN='\033[0;36m' NC='\033[0m'
else
    GREEN='' RED='' YELLOW='' CYAN='' NC=''
fi

info()  { printf "${CYAN}▸${NC} %s\n" "$*"; }
ok()    { printf "${GREEN}✓${NC} %s\n" "$*"; }
warn()  { printf "${YELLOW}⚠${NC} %s\n" "$*"; }
die()   { printf "${RED}✗${NC} %s\n" "$*" >&2; exit 1; }

# ── Pre-flight checks ───────────────────────────────────────────────────────
command -v go >/dev/null 2>&1 || die "Go is not installed. Please install Go >= ${GO_MIN_VERSION}."

GO_VERSION="$(go version | sed -E 's/.*go([0-9]+\.[0-9]+).*/\1/')"
if [ "$(printf '%s\n' "${GO_MIN_VERSION}" "${GO_VERSION}" | sort -V | head -n1)" != "${GO_MIN_VERSION}" ]; then
    die "Go ${GO_MIN_VERSION}+ required (found ${GO_VERSION})"
fi

# ── Parse flags ──────────────────────────────────────────────────────────────
TARGETS=()
RUN_LINT=false
VERBOSE=false

usage() {
    cat <<EOF
Usage: $(basename "$0") [OPTIONS] [TARGETS...]

Build meko binaries into build/latest/.

Targets (default: all):
  cli            Build the CLI app (meko)
  api            Build the API server (meko-api-server)
  provisioner    Build the provisioner operator (meko-provisioner)

Options:
  -l, --lint     Run golangci-lint after building
  -v, --verbose  Verbose go build output
  -c, --clean    Remove build/latest/ and exit
  -h, --help     Show this help
EOF
    exit 0
}

while [[ $# -gt 0 ]]; do
    case "$1" in
        -l|--lint)    RUN_LINT=true; shift ;;
        -v|--verbose) VERBOSE=true; shift ;;
        -c|--clean)   rm -rf "${BUILD_DIR}"; ok "Cleaned ${BUILD_DIR}"; exit 0 ;;
        -h|--help)    usage ;;
        cli|api|provisioner) TARGETS+=("$1"); shift ;;
        *)            die "Unknown argument: $1 (see --help)" ;;
    esac
done

if [ ${#TARGETS[@]} -eq 0 ]; then
    TARGETS=(cli api provisioner)
fi

# ── Build metadata ───────────────────────────────────────────────────────────
GIT_COMMIT="$(git -C "${REPO_ROOT}" rev-parse --short HEAD 2>/dev/null || echo "unknown")"
GIT_BRANCH="$(git -C "${REPO_ROOT}" rev-parse --abbrev-ref HEAD 2>/dev/null || echo "unknown")"
BUILD_TIME="$(date -u '+%Y-%m-%dT%H:%M:%SZ')"

LDFLAGS="-s -w"
LDFLAGS="${LDFLAGS} -X main.gitCommit=${GIT_COMMIT}"
LDFLAGS="${LDFLAGS} -X main.gitBranch=${GIT_BRANCH}"
LDFLAGS="${LDFLAGS} -X main.buildTime=${BUILD_TIME}"

GO_BUILD_FLAGS=(-trimpath -ldflags "${LDFLAGS}")
if [ "${VERBOSE}" = true ]; then
    GO_BUILD_FLAGS+=(-v)
fi

mkdir -p "${BUILD_DIR}"

# ── Build functions ──────────────────────────────────────────────────────────
build_cli() {
    info "Building cli_app → meko"
    (cd "${REPO_ROOT}/cli_app" && go build "${GO_BUILD_FLAGS[@]}" -o "${BUILD_DIR}/meko" .)
    ok "meko $(file_size "${BUILD_DIR}/meko")"
}

build_api() {
    info "Building api_server → meko-api-server"
    (cd "${REPO_ROOT}/api_server" && go build "${GO_BUILD_FLAGS[@]}" -o "${BUILD_DIR}/meko-api-server" .)
    ok "meko-api-server $(file_size "${BUILD_DIR}/meko-api-server")"
}

build_provisioner() {
    info "Building meko-provisioner → meko-provisioner"
    (cd "${REPO_ROOT}/meko-provisioner" && go build "${GO_BUILD_FLAGS[@]}" -o "${BUILD_DIR}/meko-provisioner" ./cmd)
    ok "meko-provisioner $(file_size "${BUILD_DIR}/meko-provisioner")"
}

file_size() {
    local size
    size="$(stat --printf='%s' "$1" 2>/dev/null || stat -f '%z' "$1" 2>/dev/null)"
    if [ "${size}" -ge 1048576 ]; then
        printf '(%.1f MB)' "$(echo "scale=1; ${size}/1048576" | bc)"
    else
        printf '(%.0f KB)' "$(echo "scale=0; ${size}/1024" | bc)"
    fi
}

# ── Run builds ───────────────────────────────────────────────────────────────
printf "\n${CYAN}meko build${NC}  commit=%s  branch=%s\n\n" "${GIT_COMMIT}" "${GIT_BRANCH}"

for target in "${TARGETS[@]}"; do
    case "${target}" in
        cli)         build_cli ;;
        api)         build_api ;;
        provisioner) build_provisioner ;;
    esac
done

# ── Optional lint ────────────────────────────────────────────────────────────
if [ "${RUN_LINT}" = true ]; then
    if command -v golangci-lint >/dev/null 2>&1; then
        printf "\n"
        for target in "${TARGETS[@]}"; do
            case "${target}" in
                cli)         info "Linting cli_app"; (cd "${REPO_ROOT}/cli_app" && golangci-lint run ./...) && ok "cli_app lint passed" ;;
                api)         info "Linting api_server"; (cd "${REPO_ROOT}/api_server" && golangci-lint run ./...) && ok "api_server lint passed" ;;
                provisioner) info "Linting meko-provisioner"; (cd "${REPO_ROOT}/meko-provisioner" && GOTOOLCHAIN=go1.25.8 make lint) && ok "meko-provisioner lint passed" ;;
            esac
        done
    else
        warn "golangci-lint not found, skipping lint"
    fi
fi

# ── Summary ──────────────────────────────────────────────────────────────────
printf "\n${GREEN}Build complete.${NC} Binaries in: ${BUILD_DIR}/\n"
ls -lh "${BUILD_DIR}"/
