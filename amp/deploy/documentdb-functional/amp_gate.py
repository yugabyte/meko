"""Apply amp's exclusions to upstream documentdb's functional-test allowlist.

Upstream gates its own PRs on an allowlist of functional tests that pass
reliably against documentdb-local (documentdb-local/functional-tests/config/
allowlist.yml in the documentdb source tree). amp runs the same gate against
its own gateway. exclusions.yml names the allowlisted tests that fail on amp for
a stated reason; this writes the allowlist minus those, and upstream's
functional_gate.py then does everything else unchanged.

An exclusion must still name an allowlisted test for the engine. One that no
longer does (upstream dropped or renamed it) is an error, so the list cannot
outlive what it excludes.

Usage: python3 amp_gate.py <allowlist.yml> <exclusions.yml> <engine> <out.yml>
"""

import sys

import yaml


def entry_id(entry):
    return entry if isinstance(entry, str) else entry["id"]


def applies(entry, engine):
    engines = None if isinstance(entry, str) else entry.get("engines")
    return engines is None or engine in engines


def main(allowlist_path, exclusions_path, engine, out_path):
    with open(allowlist_path) as f:
        allowlist = yaml.safe_load(f)
    with open(exclusions_path) as f:
        exclusions = (yaml.safe_load(f) or {}).get("exclusions") or []

    reasons = {}
    for e in exclusions:
        if not isinstance(e, dict) or not e.get("id") or not str(e.get("reason", "")).strip():
            sys.exit(f"exclusion needs an id and a reason: {e!r}")
        if e["id"] in reasons:
            sys.exit(f"exclusion listed twice: {e['id']}")
        reasons[e["id"]] = e["reason"]

    tests = allowlist.get("tests") or []
    allowlisted = {entry_id(t) for t in tests if applies(t, engine)}
    stale = sorted(set(reasons) - allowlisted)
    if stale:
        sys.exit(
            "exclusions that name no allowlisted test (upstream dropped or renamed them):\n  "
            + "\n  ".join(stale)
        )

    allowlist["tests"] = [t for t in tests if entry_id(t) not in reasons]
    with open(out_path, "w") as f:
        yaml.safe_dump(allowlist, f, sort_keys=False)
    print(
        f"amp_gate: {len(allowlisted)} allowlisted, {len(reasons)} excluded on amp, "
        f"{len(allowlisted) - len(reasons)} gated"
    )


if __name__ == "__main__":
    if len(sys.argv) != 5:
        sys.exit(__doc__)
    main(*sys.argv[1:])
