#!/usr/bin/env bash
#
# Tests for the ci-gate composite action. Run from anywhere:
#   bash .github/actions/ci-gate/action.test.sh
#
# The gate decides whether a PR can merge, so its two behaviours are worth
# pinning: skipped must pass (or every paths-filtered PR blocks forever) and
# failure/cancelled must fail (or the gate is decoration). Both `run:` blocks are
# extracted from action.yml itself, so these test the shipped code rather than a
# copy of it.
#
# The audit cases also assert every real workflow's gate covers all of its jobs —
# the same check CI runs, available before pushing.

cd "$(dirname "${BASH_SOURCE[0]}")/../../.." || exit 1
ACTION=.github/actions/ci-gate/action.yml
TMP=$(mktemp -d) || exit 1
trap 'rm -rf "$TMP"' EXIT
pass=0 fail=0

python3 - "$ACTION" "$TMP" <<'PY' || {
import sys, yaml
action, tmp = sys.argv[1], sys.argv[2]
steps = yaml.safe_load(open(action))["runs"]["steps"]
runs = [s["run"] for s in steps if "run" in s]
assert len(runs) == 2, f"expected 2 run steps, found {len(runs)}"
open(f"{tmp}/verdict.sh", "w").write(runs[0])
open(f"{tmp}/audit.sh", "w").write(runs[1])
PY
  echo "could not extract steps from $ACTION"
  exit 1
}

report() { # report <expected: pass|fail> <actual rc> <description>
  local want=$1 rc=$2 desc=$3 got
  [ "$rc" -eq 0 ] && got=pass || got=fail
  if [ "$got" = "$want" ]; then
    pass=$((pass + 1))
  else
    fail=$((fail + 1))
    printf 'FAIL  want=%-4s got=%-4s  %s\n' "$want" "$got" "$desc"
  fi
}

verdict() { # verdict <expected> <needs-json> <description>
  NEEDS=$2 bash "$TMP/verdict.sh" >/dev/null 2>&1
  report "$1" $? "$3"
}

audit() { # audit <expected> <workflow-path> <gate-job-id> <exclude> <description>
  WORKFLOW_REF="o/r/$2@refs/heads/main" REPO=o/r GATE_JOB=$3 EXCLUDE=$4 \
    bash "$TMP/audit.sh" >/dev/null 2>&1
  report "$1" $? "$5"
}

# --- the manifest must not reference contexts it cannot see -------------------
# Regression: `${{ toJSON(needs) }}` written into an input *description* as an example
# is still template-validated on load, and `needs` is not a valid named-value in an
# action manifest — the runner refused to load the action at all, failing every gate
# before a step ran. Prose only; the caller supplies the value.
# `inputs.needs` is the legitimate reference — it is an input of this action, not the
# workflow context — so it is excluded before looking for a bare `needs`.
if grep -oE '\$\{\{[^}]*\}\}' "$ACTION" | grep -vF 'inputs.needs' | grep -qE '\bneeds\b'; then bad=1; else bad=0; fi
report pass "$bad" 'manifest avoids the needs context in an expression'

# --- the verdict: skipped is the only allowed non-pass -----------------------
verdict pass '{"a":{"result":"success"},"b":{"result":"success"}}' 'all success'
verdict pass '{"a":{"result":"success"},"b":{"result":"skipped"}}' 'skipped alongside success'
verdict pass '{"a":{"result":"skipped"},"b":{"result":"skipped"}}' 'everything skipped (docs-only PR)'
verdict fail '{"a":{"result":"success"},"b":{"result":"failure"}}' 'one failure'
verdict fail '{"a":{"result":"skipped"},"b":{"result":"cancelled"}}' 'one cancelled'
verdict fail '{}' 'empty needs gates nothing'
verdict fail '' 'absent needs input'

# --- the audit: every job in a gated workflow must be in the gate's needs ----
audit pass .github/workflows/meko-pr-build.yml meko_ci_gate '' 'meko-pr-build covered'
audit pass .github/workflows/amp-pr-build.yml amp_ci_gate '' 'amp-pr-build covered'
audit pass .github/workflows/amp-on-demand-checks.yml amp_on_demand_gate '' 'amp-on-demand covered'
audit pass .github/workflows/amp-mcp-skills.yml amp_mcp_skills_gate '' 'amp-mcp-skills covered'

# A job added to a gated workflow but not to its gate: the drift this catches.
# NB: absolute path, deliberately not `cd`-into-a-subshell — a subshell would
# increment its own copy of the counters, so a failure here would print and still
# exit 0.
printf 'jobs:\n  a: {}\n  b: {}\n  ungated: {}\n  g:\n    needs: [a, b]\n' >"$TMP/drift.yml"
audit fail "$TMP/drift.yml" g '' 'job missing from needs'
audit pass "$TMP/drift.yml" g 'ungated' 'missing job explicitly excluded'
audit fail .github/workflows/does-not-exist.yml g '' 'workflow file absent (no checkout)'
audit fail .github/workflows/meko-pr-build.yml no_such_gate '' 'gate job id not in the file'

printf '\n%d passed, %d failed\n' "$pass" "$fail"
[ "$fail" -eq 0 ]
