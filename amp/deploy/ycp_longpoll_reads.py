#!/usr/bin/env python3
"""Print "<wait> <latency>" in seconds for each agent desired-state long-poll read in
the api-server access log on stdin. ycp-reconcile-smoke.sh's long-poll checks read it.

wait is whatever the reconcile-agent put on the query string: whole seconds ("9")
since AMP-103, a Go duration ("9.873s") before. latency is the logger's Go duration.
"""

import re
import sys

READ = re.compile(r'agent/desired-state\?since=\d+&wait=([0-9.a-zµ]+)"? .*?latency=([0-9.a-zµ]+)')
UNIT = {"h": 3600, "m": 60, "s": 1, "ms": 1e-3, "us": 1e-6, "µs": 1e-6, "ns": 1e-9}


def secs(d):
    """A Go duration, or a bare number of seconds, in seconds."""
    if re.fullmatch(r"[0-9.]+", d):
        return float(d)
    return sum(float(n) * UNIT[u] for n, u in re.findall(r"([0-9.]+)(h|ms|m|s|us|µs|ns)", d))


def reads(text):
    return [(secs(w), secs(l)) for w, l in READ.findall(text)]


if __name__ == "__main__":
    for w, l in reads(sys.stdin.read()):
        print(w, l)
