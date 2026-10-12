#!/usr/bin/env python3
"""Adapt amp-monitoring's dashboards for a Grafana fed by the BYOC telemetry door.

  python3 adapt_amp_dashboards.py OUT_DIR

The chart's dashboards (amp/deploy/helm/amp-monitoring/dashboards) assume one amp
environment in a VictoriaMetrics datasource. Behind the door, one Prometheus holds
every BYOC environment, and every amp series carries account_id and environment_id.
So each dashboard gets an Account and an Environment variable, every metric selector
gets environment_id=~"$environment", and the datasource becomes `prometheus`.
endpoint-logs.json needs VictoriaLogs and is skipped.
"""

import json
import os
import re
import sys

SRC = os.path.join(
    os.path.dirname(os.path.abspath(__file__)),
    "../../../../amp/deploy/helm/amp-monitoring/dashboards",
)
SKIP = {"endpoint-logs.json"}
DS_UID = "prometheus"
LABEL = 'environment_id=~"$environment"'
# Metric families amp ships; label names never start with these.
METRIC = re.compile(r"^(amp_|pg_|lfc_|pageserver_|safekeeper_|compute_|container_|yb_client_)")
IDENT = re.compile(r"[A-Za-z_:][A-Za-z0-9_:]*")


GROUPING = {"by", "without", "on", "ignoring", "group_left", "group_right"}


def inject(expr):
    """Add LABEL to every metric selector in a PromQL expression, skipping string literals
    and label lists such as `by (pageserver_connection_kind)`."""
    out, i, n = [], 0, len(expr)
    parens, last_word = [], ""  # per open paren: does it hold label names?
    while i < n:
        c = expr[i]
        if c == "(":
            parens.append(last_word in GROUPING)
        elif c == ")" and parens:
            parens.pop()
        if c in "\"'`":  # copy a string literal verbatim
            j = i + 1
            while j < n and expr[j] != c:
                j += 2 if expr[j] == "\\" else 1
            out.append(expr[i : j + 1])
            i = j + 1
            continue
        m = IDENT.match(expr, i)
        if m and (i == 0 or not (expr[i - 1].isalnum() or expr[i - 1] in "_:")):
            name, j = m.group(0), m.end()
            out.append(name)
            last_word = name
            k = j
            while k < n and expr[k] == " ":
                k += 1
            labels_only = bool(parens) and parens[-1]
            if METRIC.match(name) and not labels_only and not (k < n and expr[k] == "("):
                if k < n and expr[k] == "{":
                    end = _close(expr, k + 1)
                    out.append(expr[j : k + 1] + _open(expr, k + 1) + expr[k + 1 : end])
                    i = end
                else:
                    out.append("{" + LABEL + "}")
                    i = j
            else:
                i = j
            continue
        if c == "{":  # a selector with no metric name, e.g. {__name__=~"..."}
            end = _close(expr, i + 1)
            out.append("{" + _open(expr, i + 1) + expr[i + 1 : end])
            i = end
            continue
        if not c.isspace():
            last_word = ""
        out.append(c)
        i += 1
    return "".join(out)


# rate() over several metrics picked by a __name__ regex drops the name, and Prometheus
# (unlike VictoriaMetrics) rejects the colliding series; keep the name as a label first.
MULTI_NAME_RATE = re.compile(r"rate\((\{[^{}]*__name__=~[^{}]*\})\[([^\]]+)\]\)")


def prometheus_compat(expr):
    return MULTI_NAME_RATE.sub(
        r'rate(label_replace(\1, "metric", "$1", "__name__", "(.+)")[\2:])', expr
    )


def _close(expr, k):
    """Index of the '}' ending the matchers that start at k. They are copied verbatim:
    a label name such as container_name must not be read as a metric."""
    n = len(expr)
    while k < n and expr[k] != "}":
        if expr[k] in "\"'`":
            q, k = expr[k], k + 1
            while k < n and expr[k] != q:
                k += 2 if expr[k] == "\\" else 1
        k += 1
    return k


def _open(expr, k):
    """What to put right after a '{': the label, plus a comma unless the braces are empty."""
    while k < len(expr) and expr[k] == " ":
        k += 1
    return LABEL if k < len(expr) and expr[k] == "}" else LABEL + ", "


def adapt(d):
    s = json.dumps(d).replace('"uid": "victoriametrics"', f'"uid": "{DS_UID}"')
    d = json.loads(s)

    def walk(o):
        if isinstance(o, dict):
            for k, v in o.items():
                if k == "expr" and isinstance(v, str):
                    o[k] = prometheus_compat(inject(v))
                elif k == "query" and isinstance(v, str) and "label_values(" in v:
                    o[k] = inject(v)
                elif k == "query" and isinstance(v, dict) and isinstance(v.get("query"), str):
                    v["query"] = inject(v["query"])
                else:
                    walk(v)
        elif isinstance(o, list):
            for x in o:
                walk(x)

    walk(d.get("panels", []))
    walk(d.get("templating", {}))
    ds = {"type": "prometheus", "uid": DS_UID}
    env_vars = [
        {
            "name": "account",
            "label": "Account",
            "type": "query",
            "datasource": ds,
            "refresh": 1,
            "query": {
                "query": 'label_values(amp_controller_build_info{source="amp"}, account_id)',
                "refId": "acct",
            },
        },
        {
            "name": "environment",
            "label": "Environment",
            "type": "query",
            "datasource": ds,
            "refresh": 1,
            "query": {
                "query": 'label_values(amp_controller_build_info{source="amp", account_id=~"$account"}, '
                "environment_id)",
                "refId": "env",
            },
        },
    ]
    d.setdefault("templating", {}).setdefault("list", [])
    d["templating"]["list"] = env_vars + d["templating"]["list"]
    d["tags"] = sorted(set(d.get("tags", [])) | {"amp", "byoc"})
    d.pop("id", None)
    return d


if __name__ == "__main__":
    out = sys.argv[1]
    os.makedirs(out, exist_ok=True)
    for f in sorted(os.listdir(SRC)):
        if f.endswith(".json") and f not in SKIP:
            d = adapt(json.load(open(os.path.join(SRC, f))))
            json.dump(d, open(os.path.join(out, f), "w"), indent=1)
            print(f"{f}: {d['title']}")
