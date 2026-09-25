#!/usr/bin/env bash
#
# Flow tests for the build-tools base image plumbing. No docker daemon and no
# real image build -- fast enough to run on every PR. Two flows:
#
#   1. Publish flow (build.sh): a stub `docker` on PATH records the commands
#      build.sh would run, and we assert it builds/pushes the right ref from the
#      right context, and rejects bad args.
#   2. Lockstep-pin invariant: the base's tag pin is a single constant, so the
#      three consumer Dockerfiles, build.sh's defaults, and the publish workflow
#      must all name the SAME registry/image/tag. This catches a half-done bump
#      (e.g. root Dockerfile moved to v2 but the endpoint image left on v1).
#
# Run: bash deploy/build-tools/test_build.sh
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
amp_root="$(cd "$here/../.." && pwd)"
repo_root="$(cd "$amp_root/.." && pwd)"
build_sh="$here/build.sh"
workflow="$repo_root/.github/workflows/amp-build-tools.yml"

# Expected pin -- the single source of truth this whole suite checks everything against.
WANT_REGISTRY="quay.io/yugabyte"
WANT_IMAGE="amp-build-tools"
WANT_TAG="v1"

rc=0
pass() { echo "  ✓ $1"; }
fail() { echo "  ✗ $1"; rc=1; }

# --- stub docker -------------------------------------------------------------
# A fake `docker` that appends its argv (one space-joined line per call) to
# $DOCKER_LOG and exits 0, so build.sh runs to completion without a daemon.
stub_dir="$(mktemp -d)"
trap 'rm -rf "$stub_dir"' EXIT
cat > "$stub_dir/docker" <<'STUB'
#!/usr/bin/env bash
printf '%s\n' "$*" >> "$DOCKER_LOG"
exit 0
STUB
chmod +x "$stub_dir/docker"

# Run build.sh with the stub docker; capture exit code + the recorded commands.
# Sets globals: RUN_RC (exit code) and RUN_LOG (path to the docker command log).
run_build() {
  RUN_LOG="$stub_dir/log.$RANDOM"
  : > "$RUN_LOG"
  set +e
  DOCKER_LOG="$RUN_LOG" PATH="$stub_dir:$PATH" bash "$build_sh" "$@" >/dev/null 2>&1
  RUN_RC=$?
  set -e
}

echo "build-tools publish flow (build.sh):"

# Default invocation: build the pinned ref from the amp/ context, no push. The
# context is the directory holding build-tools/ (a backreference), which
# Dockerfile.dockerignore narrows to the checksum manifest and its checker.
run_build
if [ "$RUN_RC" -eq 0 ]; then pass "default: exit 0"; else fail "default: exit $RUN_RC"; fi
if grep -Eq "^build -t ${WANT_REGISTRY}/${WANT_IMAGE}:${WANT_TAG} -f (.*)/build-tools/Dockerfile \1$" "$RUN_LOG"; then
  pass "default: builds ${WANT_REGISTRY}/${WANT_IMAGE}:${WANT_TAG} from the amp/ context"
else
  fail "default: build command wrong -> $(cat "$RUN_LOG")"
fi
if grep -q '^push ' "$RUN_LOG"; then fail "default: pushed without --push"; else pass "default: no push"; fi

# --push: same build, plus a push of the same ref.
run_build --push
if grep -Fxq "push ${WANT_REGISTRY}/${WANT_IMAGE}:${WANT_TAG}" "$RUN_LOG"; then
  pass "--push: pushes ${WANT_REGISTRY}/${WANT_IMAGE}:${WANT_TAG}"
else
  fail "--push: did not push the pinned ref -> $(cat "$RUN_LOG")"
fi

# Overrides flow through to both build and push refs.
run_build --push --registry r.example/ns --image custom --tag v9
if grep -Eq "^build -t r.example/ns/custom:v9 -f " "$RUN_LOG" && grep -Fxq "push r.example/ns/custom:v9" "$RUN_LOG"; then
  pass "--registry/--image/--tag: override the ref for build and push"
else
  fail "overrides not honored -> $(cat "$RUN_LOG")"
fi

# Bad args must fail fast (exit 2) and never touch docker.
run_build --bogus
if [ "$RUN_RC" -eq 2 ] && ! [ -s "$RUN_LOG" ]; then pass "unknown flag: exit 2, docker not called"; else fail "unknown flag: rc=$RUN_RC log=$(cat "$RUN_LOG")"; fi
run_build stray-arg
if [ "$RUN_RC" -eq 2 ] && ! [ -s "$RUN_LOG" ]; then pass "unexpected positional: exit 2, docker not called"; else fail "unexpected positional: rc=$RUN_RC log=$(cat "$RUN_LOG")"; fi

# --help is a clean exit 0 (never invokes docker).
run_build --help
if [ "$RUN_RC" -eq 0 ] && ! [ -s "$RUN_LOG" ]; then pass "--help: exit 0, docker not called"; else fail "--help: rc=$RUN_RC"; fi

echo
echo "lockstep-pin invariant (base <-> 3 consumers <-> build.sh <-> workflow):"

# First `ARG NAME=` value in a Dockerfile (the default before any FROM).
arg_default() { grep -m1 "^ARG $2=" "$1" | sed "s/^ARG $2=//"; }

check_consumer() {
  local label="$1" file="$2" r i t
  if [ ! -f "$file" ]; then fail "$label: missing $file"; return; fi
  r="$(arg_default "$file" REPOSITORY)"; i="$(arg_default "$file" IMAGE)"; t="$(arg_default "$file" TAG)"
  if [ "$r" = "$WANT_REGISTRY" ] && [ "$i" = "$WANT_IMAGE" ] && [ "$t" = "$WANT_TAG" ]; then
    pass "$label: $r/$i:$t"
  else
    fail "$label: got $r/$i:$t, want ${WANT_REGISTRY}/${WANT_IMAGE}:${WANT_TAG}"
  fi
}

check_consumer "root Dockerfile"      "$amp_root/Dockerfile"
check_consumer "endpoint Dockerfile"  "$amp_root/deploy/endpoint/Dockerfile"
check_consumer "pg18-build Dockerfile" "$amp_root/deploy/extensions/pg18-build.Dockerfile"

# build.sh's own defaults (registry="..."/image="..."/tag="...").
sh_default() { grep -m1 "^$2=" "$1" | sed -E "s/^$2=\"?([^\"]*)\"?.*/\1/"; }
sr="$(sh_default "$build_sh" registry)"; si="$(sh_default "$build_sh" image)"; st="$(sh_default "$build_sh" tag)"
if [ "$sr" = "$WANT_REGISTRY" ] && [ "$si" = "$WANT_IMAGE" ] && [ "$st" = "$WANT_TAG" ]; then
  pass "build.sh defaults: $sr/$si:$st"
else
  fail "build.sh defaults: got $sr/$si:$st"
fi

# Publish workflow: REGISTRY/IMAGE env + the tag input default.
if [ -f "$workflow" ]; then
  wr="$(grep -m1 '^  REGISTRY:' "$workflow" | awk '{print $2}')"
  wi="$(grep -m1 '^  IMAGE:' "$workflow" | awk '{print $2}')"
  wt="$(awk '/^      tag:/{f=1} f&&/default:/{print $2; exit}' "$workflow")"
  if [ "$wr" = "$WANT_REGISTRY" ] && [ "$wi" = "$WANT_IMAGE" ] && [ "$wt" = "$WANT_TAG" ]; then
    pass "workflow: REGISTRY=$wr IMAGE=$wi tag-default=$wt"
  else
    fail "workflow: got REGISTRY=$wr IMAGE=$wi tag-default=$wt"
  fi
else
  fail "workflow: missing $workflow"
fi

echo
if [ "$rc" -ne 0 ]; then
  echo "FAIL: build-tools flow tests failed. If you bumped the base tag, bump it in"
  echo "ALL of: the three consumer Dockerfiles, build.sh, and amp-build-tools.yml."
  exit 1
fi
echo "PASS: build-tools publish flow + lockstep pin are consistent."
