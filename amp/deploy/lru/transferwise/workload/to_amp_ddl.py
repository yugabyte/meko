"""Rewrites the vendored schema from YugabyteDB DDL to plain Postgres.

The schema in this directory is a byte-for-byte copy of the customer's, dumped
from YugabyteDB, so it carries YB-only syntax that amp's Postgres rejects:
`SPLIT INTO n TABLETS`, `PRIMARY KEY((a) HASH, b ASC)`, `USING lsm`,
`CREATE INDEX NONCONCURRENTLY`, and a leading `yb_hash_code(id) % n` index column
whose only job on YB is to spread the index over tablets. Keeping the copy
verbatim and converting here means a refresh from the source repo is a plain
`cp`, and every deviation from the customer's schema is visible in one file.

    python3 to_amp_ddl.py bomber_schema_cutdown.sql > schema.pg.sql
"""

import re
import sys

# Order matters: the hash-prefix column has to go before ASC/DESC keywords are
# touched, and SPLIT INTO before the statement terminator is normalised.
SUBS = [
    # The one deviation that changes behaviour rather than syntax. The driver
    # ANALYZEs every benchmark table a minute into each 15m cycle under a single
    # 5-minute client deadline, and returns on the first failure, so every table
    # after the one that blows the budget is skipped. At 221M and 265M rows that
    # produced 93 timeouts a day and left evt_audit 28h stale. Analyze is
    # autovacuum's job here, and it has no deadline. FINDINGS 2026-09-19.
    (
        re.compile(r"^\tgo runOneShotAnalyze\(.*\)$", re.M),
        "\t// runOneShotAnalyze disabled for amp by to_amp_ddl.py; autovacuum owns ANALYZE",
    ),
    # `SPLIT INTO 6 TABLETS;` closes a CREATE TABLE on YB. Drop it, keep the `;`.
    (re.compile(r"\nSPLIT INTO \d+ TABLETS;", re.I), ";"),
    (re.compile(r"\s+SPLIT INTO \d+ TABLETS", re.I), ""),
    # `PRIMARY KEY((id) HASH, regionid ASC)` -> `PRIMARY KEY(id, regionid)`.
    (re.compile(r"\(\s*([\w.\"]+)\s*\)\s+HASH", re.I), r"\1"),
    (re.compile(r"\bHASH\b", re.I), ""),
    (re.compile(r"USING lsm\b", re.I), "USING btree"),
    (re.compile(r"CREATE (UNIQUE )?INDEX NONCONCURRENTLY\b", re.I), r"CREATE \1INDEX"),
    # Tablet-spreading prefix column: no tablets on amp, and yb_hash_code does
    # not exist there. Only ever appears first in an index column list.
    (re.compile(r"\(\s*\(\(yb_hash_code\([^)]*\)\s*%\s*\d+\)\)\s*(?:ASC|DESC)?\s*,\s*", re.I), "("),
    (
        re.compile(r"\(\s*\(\(yb_hash_code\([^)]*\)\s*%\s*\d+\)\)\s*(?:ASC|DESC)?\s*\)", re.I),
        "(id)",
    ),
    # Placement-aware tablespaces are a YB feature; ../up.sh does not create them.
    (re.compile(r"^SET default_tablespace = .*$", re.M), "SET default_tablespace = '';"),
]

# Postgres takes no ASC/DESC inside a PRIMARY KEY or UNIQUE constraint, and a
# constraint may not repeat a column: dropping HASH leaves
# `PRIMARY KEY(id, regionid ASC, id ASC)` on the tables whose hash column is
# also a range column. In a CREATE INDEX the ASC/DESC stays, where it is legal.
KEY = re.compile(r"((?:PRIMARY KEY|UNIQUE)\s*\()([^()]*)(\))", re.I)


def normalise_keys(sql):
    def fix(m):
        seen, cols = set(), []
        for col in (c.strip() for c in m.group(2).split(",")):
            name = re.sub(r"\s+(ASC|DESC)$", "", col, flags=re.I).strip()
            if name and name.lower() not in seen:
                seen.add(name.lower())
                cols.append(name)
        return m.group(1) + ", ".join(cols) + m.group(3)

    return KEY.sub(fix, sql)


def convert(sql):
    for pat, repl in SUBS:
        sql = pat.sub(repl, sql)
    sql = normalise_keys(sql)
    sql = re.sub(r"[ \t]+\)", ")", sql)
    sql = re.sub(r"\(\s+", "(", sql)
    return sql


if __name__ == "__main__":
    src = open(sys.argv[1]).read() if len(sys.argv) > 1 else sys.stdin.read()
    out = convert(src)
    if "go runOneShotAnalyze(" in out:
        sys.exit("to_amp_ddl: the driver ANALYZE call survived the rewrite")
    for token in ("yb_hash_code", "SPLIT INTO", "USING lsm", "NONCONCURRENTLY"):
        if re.search(token, out, re.I):
            sys.exit(f"to_amp_ddl: {token} survived the rewrite")
    sys.stdout.write(out)
