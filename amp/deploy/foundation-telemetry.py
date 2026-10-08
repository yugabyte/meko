#!/usr/bin/env python3
"""Write a copy of a foundation spec with YugabyteDB metrics export turned on.

The CI counterpart of the step an environment's install hint prints for an
operator (meko#1405). Both add the same two things: a TelemetryProvider naming
the BYOC door, and a telemetry.metrics block on the universe saying what to
collect and who to send it to. YBA renders the collector config from those.

A copy rather than an edit in place, so a failed run leaves the checkout clean
and the generated file can be diffed when something looks wrong.

Line-based, not a YAML round-trip: the spec is heavily commented and PyYAML
would drop every comment, turning a readable diff into a rewrite of the file.
The tradeoff is that the two anchors below have to hold, so both are checked and
the script fails loudly rather than writing a spec that silently lacks telemetry.

Usage:
    YCP_ENV_ID=... YCP_BYOC_METRICS_URL=... \\
      foundation-telemetry.py <spec-in> <spec-out> [account-id]
"""

import os
import sys

PROVIDER_NAME = "byoc-metrics"
SECRET_NAME = "byoc-metrics-auth"
USERNAME = "byoc"


def provider_block(metrics_url):
    """The export destination, declared above the universe: keys are applied top
    to bottom, and the universe's telemetry is not reconciled until this one is
    Ready. The password is a Secret reference, so no credential is written here.
    """
    base = metrics_url[: -len("/metrics")] if metrics_url.endswith("/metrics") else metrics_url
    return f"""# Added by amp/deploy/foundation-telemetry.py — see meko#1405.
TelemetryProvider:
  name: {PROVIDER_NAME}
  spec:
    provider: OTLP
    otlp:
      endpoint: {base}
      protocol: HTTP
      metricsEndpoint: {metrics_url}
      # Explicit, though gzip is the CRD's default: the door returns 415 to an
      # uncompressed payload, so a changed default would fail every send.
      compression: gzip
      authType: BasicAuth
      basicAuth:
        username: {USERNAME}
        passwordSecret: {{name: {SECRET_NAME}, key: password}}

"""


def telemetry_block(env_id, account_id, indent="    "):
    """What to collect, and the identity on every record.

    MINIMAL, not TABLE_OFF. TABLE_OFF is the level whose name says what we want,
    but YBA's metrics export sends only priority_regex, metrics and show_help, and
    TABLE_OFF's params file defines neither of the first two — so it arrives as no
    filter at all and emits every per-table series. MINIMAL defines both.

    No CQL_EXPORT — amp's computes speak YSQL.
    """
    lines = [
        "# Added by amp/deploy/foundation-telemetry.py — see meko#1405.",
        "telemetry:",
        "  metrics:",
        "    collectionLevel: MINIMAL",
        "    scrapeConfigTargets: [MASTER_EXPORT, TSERVER_EXPORT, YSQL_EXPORT]",
        "    exporters:",
        f"      - telemetryProvider: {PROVIDER_NAME}",
        "        additionalTags:",
        "          source: amp",
        f"          environment_id: {env_id}",
    ]
    # Omitted rather than empty when there is no account: a blank tag on every
    # series is worse than no tag, and YCP's standalone account is a sentinel.
    if account_id:
        lines.append(f"          account_id: {account_id}")
    return "".join(indent + line + "\n" for line in lines) + "\n"


def main():
    if len(sys.argv) < 3:
        sys.exit(f"usage: {sys.argv[0]} <spec-in> <spec-out> [account-id]")
    spec_in, spec_out = sys.argv[1], sys.argv[2]
    account_id = sys.argv[3] if len(sys.argv) > 3 else ""

    env_id = os.environ.get("YCP_ENV_ID", "").strip()
    metrics_url = os.environ.get("YCP_BYOC_METRICS_URL", "").strip()
    if not env_id:
        sys.exit("YCP_ENV_ID is required: it is the environment_id every series is tagged with")
    if not metrics_url:
        sys.exit("YCP_BYOC_METRICS_URL is required: it is where the metrics go")

    with open(spec_in, encoding="utf-8") as f:
        lines = f.readlines()

    out, added_provider, added_telemetry = [], False, False
    for line in lines:
        # The universe entry is a top-level key, so it starts at column 0.
        if line.startswith("YBUniverse:") and not added_provider:
            out.append(provider_block(metrics_url))
            added_provider = True
            out.append(line)
            continue
        # Its spec: body is indented two, and its fields four. Inserting right
        # after the opening keeps the block away from any nested mapping.
        if added_provider and not added_telemetry and line.rstrip() == "  spec:":
            out.append(line)
            out.append(telemetry_block(env_id, account_id))
            added_telemetry = True
            continue
        out.append(line)

    if not added_provider:
        sys.exit(f"{spec_in}: no top-level 'YBUniverse:' key; cannot place the TelemetryProvider")
    if not added_telemetry:
        sys.exit(
            f"{spec_in}: the YBUniverse entry has no '  spec:' line; "
            "cannot place the telemetry block"
        )

    with open(spec_out, "w", encoding="utf-8") as f:
        f.writelines(out)


if __name__ == "__main__":
    main()
