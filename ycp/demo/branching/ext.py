"""The extensions presentation: `ycp_demo.py extensions`.

Try an extension on a copy of production, without touching production:
  1. main can't   -- CREATE EXTENSION vector fails on main
  2. Fork         -- a copy of main, with its own compute
  3. Enable       -- one YCP write adds pgvector to the copy's branch; amp redeploys only
                     the copy's compute, with the extension image mounted
  4. Use it       -- "similar products" for Aman, with an HNSW index
  5. Drop         -- the copy goes; main still has no pgvector

A branch declares extensions only once it exists (amp/controller/architecture/ac-ycp.md,
"Branch extensions"), so the enable is a second write after the fork.
"""

import time

import show as S
import ycp_demo as d

from show import bold, cyan, dim, green, red

# The catalog (and the console) call it pgvector; inside Postgres it is `vector`.
EXT, SQL_NAME = "pgvector", "vector"
BRANCH = "try-pgvector"

# Each product's vector, from its attributes (the seed derives them from the id):
# category (6), product type within the category (4), product line (10, weighted
# lower), and price. A stand-in for model embeddings, which would be stored the same way.
EMBED = """
UPDATE products SET embedding = (
  '[' || array_to_string(
       ARRAY(SELECT CASE WHEN i = id % 6 THEN 1.0 ELSE 0 END FROM generate_series(0, 5) i)
    || ARRAY(SELECT CASE WHEN i = (id / 6) % 4 THEN 1.0 ELSE 0 END FROM generate_series(0, 3) i)
    || ARRAY(SELECT CASE WHEN i = id % 10 THEN 0.5 ELSE 0 END FROM generate_series(0, 9) i)
    || ARRAY[round(price / 450.0, 3)], ',') || ']')::vector
"""

ANCHOR = f"""
SELECT p.id, p.name, p.price FROM orders o
JOIN order_items i ON i.order_id = o.id JOIN products p ON p.id = i.product_id
WHERE o.customer_id = {d.CUSTOMER_ID} ORDER BY o.placed_at DESC, i.line LIMIT 1
"""


def similar(addr, anchor_id):
    anchor = f"(SELECT embedding FROM products WHERE id = {anchor_id})"
    return d.psql(
        addr,
        [
            # The index returns at most ef_search rows, so match the inner LIMIT.
            "SET hnsw.ef_search = 200",
            # One row per product, not per numbered variant of it ("Summit Rain Jacket 137, 162, ...").
            # The anchor is a scalar subquery, not a join: only a Var-free operand lets the
            # planner serve ORDER BY ... <-> from the HNSW index.
            f"""SELECT name || '|' || price || '|' || round(dist::numeric, 3) FROM (
              SELECT DISTINCT ON (base) name, price, dist FROM (
                SELECT name, price, embedding <-> {anchor} AS dist, regexp_replace(name, ' [0-9]+$', '') AS base
                FROM products
                WHERE regexp_replace(name, ' [0-9]+$', '') <>
                      (SELECT regexp_replace(name, ' [0-9]+$', '') FROM products WHERE id = {anchor_id})
                ORDER BY embedding <-> {anchor} LIMIT 200) near
              ORDER BY base, dist) best
            ORDER BY dist LIMIT 5""",
        ],
    ).splitlines()[-5:]


def extensions(args):
    S.PAUSE = "--pause" in args
    addr, main_id = d.main_endpoint()
    C = d.CLUSTER
    b_name, e_name = f"{C}/{BRANCH}", f"{C}/{BRANCH}/rw"
    t_show = time.time()
    print("\n  " + bold(S.magenta("Try an extension on a copy of production.")))
    print("  " + dim(f"{d.STORE} wants “similar products” for {d.CUSTOMER}. That needs {EXT}."))

    # ---- act 1
    S.act(1, "Production can't", f"{EXT} isn't available on main, and main is the live store.")
    S.typeout("  " + cyan(f"CREATE EXTENSION {SQL_NAME};") + dim(f"   -- {EXT}, on main"))
    try:
        d.psql(addr, f"CREATE EXTENSION {SQL_NAME}")
        print(
            f"  {red('it worked on main: drop it with DROP EXTENSION, the demo assumes main lacks it')}"
        )
        return
    except RuntimeError as e:
        print(f"  {red('✖')} {S.pg_error(str(e))}")
    S.beat()

    # ---- act 2
    S.act(2, "Fork", "A full copy of main, with its own compute.")
    t = time.time()
    d.add(
        [
            {"kind": "branch", "name": b_name, "spec": {"parent_branch_id": main_id}},
            {"kind": "endpoint", "name": e_name, "spec": d.endpoint("cluster_ip")},
        ]
    )
    try:
        S.spinner_call("forking main", lambda: d.wait([f"branch/{b_name}"]))
        fork_s = time.time() - t
        inv, up_s = S.spinner_call(
            "starting the copy's compute", lambda: d.wait([f"endpoint/{e_name}"])
        )
        print(
            f"  {green('✔')} {bold(BRANCH)} forked in {fork_s:.1f}s, compute up in {fork_s + up_s:.0f}s"
        )
        S.beat()

        # ---- act 3
        S.act(
            3, f"Enable {EXT} on the copy", "One YCP write. amp redeploys only the copy's compute."
        )
        S.typeout("  " + cyan(f'branch {BRANCH}: extensions = ["{EXT}"]'))
        t = time.time()
        d.update(b_name, extensions=[EXT])

        def mounted():
            while time.time() - t < 300:
                row = d.inventory().get(f"endpoint/{e_name}")
                try:
                    a = d.endpoint_addr(row)
                    if (
                        d.psql(
                            a,
                            f"SELECT count(*) FROM pg_available_extensions WHERE name = '{SQL_NAME}'",
                            timeout=20,
                        )
                        == "1"
                    ):
                        return a
                # SystemExit too: endpoint_addr die()s while the new pod has no IP yet.
                except (Exception, SystemExit):  # noqa: BLE001 -- the old pod is going away
                    pass
                time.sleep(1)
            raise RuntimeError("the extension never appeared on the copy")

        b_addr, en_s = S.spinner_call(f"redeploying the copy with the {EXT} image", mounted)
        print(
            f"  {green('✔')} {EXT} available on {bold(BRANCH)} {en_s:.0f}s after the write  "
            f"{dim('· main: untouched')}"
        )
        S.beat()

        # ---- act 4
        S.act(4, "Use it", f"Products similar to the one {d.CUSTOMER.split()[0]} bought last.")
        steps = [
            (f"CREATE EXTENSION {SQL_NAME}", f"CREATE EXTENSION {SQL_NAME}"),
            (
                "ALTER TABLE products ADD COLUMN embedding vector(21)",
                "ALTER TABLE products ADD COLUMN embedding vector(21)",
            ),
            ("UPDATE products SET embedding = …  -- 2,000 products", EMBED),
            (
                "CREATE INDEX ON products USING hnsw (embedding vector_l2_ops)",
                "CREATE INDEX ON products USING hnsw (embedding vector_l2_ops)",
            ),
        ]
        for shown, sql in steps:
            S.typeout("  " + cyan(shown), delay=0.004)
            _, secs = S.spinner_call("", lambda sql=sql: d.psql(b_addr, sql))
            print(f"    {dim(f'{secs:.1f}s')}")
        version = d.psql(
            b_addr, f"SELECT extversion FROM pg_extension WHERE extname = '{SQL_NAME}'"
        )
        print(f"  {green('✔')} {bold(f'{EXT} {version}')} installed on {BRANCH}")
        anchor = d.psql(b_addr, ANCHOR).split("|")
        print(f"\n  {d.CUSTOMER.split()[0]} bought  {bold(anchor[1])}  {dim('$' + anchor[2])}")
        print(f"  {dim('similar, by vector distance:')}")
        t = time.time()
        rows = similar(b_addr, anchor[0])
        q_ms = (time.time() - t) * 1000
        for r in rows:
            name, price, dist = r.split("|")
            print(
                f"    {S.pad(bold(name), 34)} {f'${float(price):,.2f}':>9}   {dim('distance ' + dist)}"
            )
        print(f"  {dim(f'{q_ms:.0f} ms round trip, HNSW index · vectors from product attributes')}")
        S.beat()
    finally:
        # ---- act 5 (always: never leave the copy behind)
        S.act(5, "Drop the copy", f"The experiment goes. Main never had {EXT}.")
        t = time.time()
        d.remove(b_name)
        drop_s = time.time() - t  # YCP accepted it: the console removes the copy now
        print(f"  {green('✔')} dropped in {drop_s:.1f}s  {dim('·  releasing its compute')}")
        S.spinner_call(
            "releasing the copy's compute",
            lambda: d.wait([f"branch/{b_name}"], gone=True, timeout=300),
        )
        release_s = time.time() - t
        on_main = d.psql(addr, f"SELECT count(*) FROM pg_extension WHERE extname = '{SQL_NAME}'")
        print(
            f"  {green('✔')} compute released in {release_s:.0f}s  {dim('·')}  {EXT} on main: "
            f"{bold(green('none') if on_main == '0' else red('installed?!'))}"
        )

    card = [
        f"{EXT} {version} tried on a full copy of {d.STORE} in {time.time() - t_show:.0f}s",
        f"one YCP write to enable it · {en_s:.0f}s to redeploy the copy · main untouched",
    ]
    width = max(len(c) for c in card) + 4
    print("\n  " + green("╭" + "─" * width + "╮"))
    for c in card:
        print("  " + green("│") + "  " + S.pad(bold(c), width - 2) + green("│"))
    print("  " + green("╰" + "─" * width + "╯") + "\n")


def main(args):
    try:
        extensions(args)
    except KeyboardInterrupt:
        raise SystemExit("\ninterrupted; the copy was dropped")
