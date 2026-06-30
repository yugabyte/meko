#!/usr/bin/env python3
# Tabulate best-of-N timings from microbench.sql runs.
#
# Usage:
#   parse_results.py run1.txt run2.txt [run3.txt ...]
#
# Each input file should contain `=== bench_XX ===` headers and
# `Time: NNN ms` lines extracted from psql output. `run_microbench.sh`
# produces this format directly.

import re
import sys


def parse(path):
    out = {}
    cur = None
    extra = None
    with open(path) as f:
        for line in f:
            line = line.strip()
            m = re.match(r"=== (.+) ===", line)
            if m:
                cur = m.group(1)
                extra = []
                continue
            m = re.match(r"Time: ([\d.]+) ms", line)
            if m and cur:
                t = float(m.group(1))
                # ANALYZE step issues three statements; sum them.
                if cur == "bench_03b_analyze":
                    extra.append(t)
                    if len(extra) == 3:
                        out[cur] = sum(extra)
                        cur = None
                else:
                    out[cur] = t
                    cur = None
    return out


def main(paths):
    runs = [parse(p) for p in paths]
    keys = sorted(set().union(*runs))
    headers = [f"r{i+1}" for i in range(len(runs))]
    w = max(9, max(len(h) for h in headers))
    print(f"{'metric':40s} " + " ".join(f"{h:>{w}s}" for h in headers) + f" {'best':>{w}s}")
    for k in keys:
        vals = [r.get(k, float("nan")) for r in runs]
        good = [v for v in vals if v == v]
        bestv = min(good) if good else float("nan")
        cells = " ".join(f"{v:{w}.1f}" for v in vals)
        print(f"{k:40s} {cells} {bestv:{w}.1f}")


if __name__ == "__main__":
    if len(sys.argv) < 2:
        sys.exit("usage: parse_results.py run1.txt [run2.txt ...]")
    main(sys.argv[1:])
