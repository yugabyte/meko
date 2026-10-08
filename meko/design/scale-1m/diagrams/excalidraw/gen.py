"""Generate the Excalidraw scenes in this directory. Numbers come from the MEKO-784
and MEKO-785 measurements (meko/test/scale); edit them here, not in the .excalidraw
files, then run ../render.py.

  python3 gen.py
"""

import json
import math
import random
from pathlib import Path

HERE = Path(__file__).resolve().parent

INK = "#1e1e1e"
MUTED = "#868e96"
RED = "#e03131"
GREEN = "#2f9e44"
BLUE = "#1971c2"
AMBER = "#f08c00"
FILL_RED = "#ffc9c9"
FILL_GREEN = "#b2f2bb"
FILL_BLUE = "#a5d8ff"
FILL_GREY = "#e9ecef"
FILL_AMBER = "#ffec99"


class Scene:
    def __init__(self, seed: int):
        self.rng = random.Random(seed)
        self.elements = []

    def _base(self, kind, x, y, w, h, stroke, fill="transparent", **kw):
        e = {
            "id": f"{kind[:3]}{len(self.elements)}",
            "type": kind,
            "x": x,
            "y": y,
            "width": w,
            "height": h,
            "angle": 0,
            "strokeColor": stroke,
            "backgroundColor": fill,
            "fillStyle": "solid",
            "strokeWidth": kw.pop("width", 2),
            "strokeStyle": kw.pop("style", "solid"),
            "roughness": kw.pop("rough", 1),
            "opacity": 100,
            "groupIds": [],
            "frameId": None,
            "roundness": kw.pop("round", None),
            "seed": self.rng.randint(1, 2**31),
            "version": 1,
            "versionNonce": self.rng.randint(1, 2**31),
            "isDeleted": False,
            "boundElements": None,
            "updated": 1,
            "link": None,
            "locked": False,
        }
        e.update(kw)
        self.elements.append(e)
        return e

    def rect(self, x, y, w, h, stroke=INK, fill="transparent", **kw):
        return self._base("rectangle", x, y, w, h, stroke, fill, round={"type": 3}, **kw)

    def text(self, x, y, s, size=20, color=INK, align="left", family=1):
        lines = s.split("\n")
        w = max(len(line) for line in lines) * size * 0.6
        h = len(lines) * size * 1.25
        if align == "center":
            x -= w / 2
        elif align == "right":
            x -= w
        return self._base(
            "text",
            x,
            y,
            w,
            h,
            color,
            text=s,
            originalText=s,
            fontSize=size,
            fontFamily=family,
            textAlign=align,
            verticalAlign="top",
            baseline=int(size * 0.9),
            containerId=None,
            lineHeight=1.25,
            autoResize=True,
        )

    def _path(self, kind, pts, color, arrow, **kw):
        x0, y0 = pts[0]
        rel = [[x - x0, y - y0] for x, y in pts]
        xs, ys = [p[0] for p in rel], [p[1] for p in rel]
        return self._base(
            kind,
            x0,
            y0,
            max(xs) - min(xs),
            max(ys) - min(ys),
            color,
            points=rel,
            lastCommittedPoint=None,
            startBinding=None,
            endBinding=None,
            startArrowhead=None,
            endArrowhead="arrow" if arrow else None,
            # @excalidraw/utils 0.1.2 curves a multi-point path unless told otherwise.
            strokeSharpness="sharp",
            **kw,
        )

    def arrow(self, pts, color=INK, **kw):
        return self._path("arrow", pts, color, True, **kw)

    def line(self, pts, color=INK, **kw):
        return self._path("line", pts, color, False, **kw)

    def dot(self, x, y, r, color, fill):
        return self._base("ellipse", x - r, y - r, 2 * r, 2 * r, color, fill, width=1)

    def save(self, name):
        doc = {
            "type": "excalidraw",
            "version": 2,
            "source": "meko/design/scale-1m/diagrams/excalidraw/gen.py",
            "elements": self.elements,
            "appState": {"viewBackgroundColor": "#ffffff", "gridSize": None},
            "files": {},
        }
        (HERE / f"{name}.excalidraw").write_text(json.dumps(doc, indent=1) + "\n")
        print(f"wrote {name}.excalidraw")


def partition_grid(s, x, y, hit, fill_hit, color_hit, cols=16, rows=8, cell=22):
    for i in range(cols * rows):
        cx, cy = x + (i % cols) * (cell + 4), y + (i // cols) * (cell + 4)
        on = hit(i)
        s.rect(
            cx,
            cy,
            cell,
            cell,
            color_hit if on else MUTED,
            fill_hit if on else FILL_GREY,
            width=1,
            rough=0,
        )


def partition_pruning():
    s = Scene(784)
    s.text(40, 20, "Why memory_search read 128 partitions", 30)
    s.text(
        40,
        64,
        "memory_search filters on (datapack, user). It leaves agent_id out so agents share memories.",
        18,
        MUTED,
    )

    s.text(40, 120, "Before: meko_mem0_collection", 24, RED)
    s.text(40, 154, "PARTITION BY HASH (datapack, user, agent)", 18, family=3)
    partition_grid(s, 40, 196, lambda i: True, FILL_RED, RED)
    s.text(
        40,
        470,
        "No agent in the filter, so no single hash:\n"
        "one HNSW scan per partition, 128 in all,\nthen a merge.",
        18,
    )
    s.text(40, 570, "43 s at 100k datapacks · over 35 min at 1M", 20, RED)

    s.text(560, 120, "After: meko_mem0_memories (000006)", 24, GREEN)
    s.text(560, 154, "PARTITION BY HASH (datapack)", 18, family=3)
    partition_grid(s, 560, 196, lambda i: i == 37, FILL_GREEN, GREEN)
    s.text(
        560,
        470,
        "The datapack alone picks the partition,\n"
        "with or without an agent:\none partition is read.",
        18,
    )
    s.text(560, 570, "2.5 ms exact · 29.5 ms through the index, at 1M", 20, GREEN)

    s.text(
        40,
        630,
        "The 128 partitions stay: each bounds its HNSW index. "
        "The same data unpartitioned took 29-103 s per index search at 100k.",
        16,
        MUTED,
    )
    s.save("partition-pruning")


def index_vs_exact():
    s = Scene(7841)
    s.text(40, 20, "Filtered HNSW walk vs exact scan of one datapack", 30)
    s.text(
        40,
        64,
        "One partition's index holds the vectors of every datapack that hashes to it.",
        18,
        MUTED,
    )

    # Left: the index walk. Other tenants' points sit close to the query.
    s.text(40, 120, "HNSW, filtered afterwards", 24, AMBER)
    s.rect(40, 160, 440, 330, MUTED, "transparent", style="dashed")
    qx, qy = 260, 325
    rng = random.Random(3)
    for _ in range(70):
        a, r = rng.uniform(0, 2 * math.pi), rng.uniform(20, 150)
        x, y = qx + r * math.cos(a) * 1.3, qy + r * math.sin(a)
        mine = rng.random() < 0.12
        s.dot(x, y, 6, GREEN if mine else MUTED, FILL_GREEN if mine else FILL_GREY)
    s.dot(qx, qy, 9, RED, FILL_RED)
    s.text(qx + 14, qy - 30, "query", 18, RED)
    s.arrow(
        [[qx, qy], [qx + 60, qy + 40], [qx + 120, qy + 10], [qx + 170, qy + 70]],
        AMBER,
        style="dashed",
    )
    s.text(
        40,
        540,
        "The walk visits the nearest vectors of every tenant\n"
        "and drops the ones that aren't this datapack's.\n"
        "ef_search bounds the walk: recall falls, or latency grows.",
        17,
    )
    s.text(40, 640, "small datapacks at 100k: recall 0.92, 131 ms (KB)", 18, AMBER)

    # Right: the exact path.
    s.text(560, 120, "Exact, by primary-key prefix", 24, GREEN)
    s.rect(560, 160, 420, 330, MUTED, "transparent", style="dashed")
    for i in range(8):
        y = 184 + i * 36
        mine = 3 <= i <= 5
        s.rect(
            580,
            y,
            380,
            26,
            GREEN if mine else MUTED,
            FILL_GREEN if mine else FILL_GREY,
            width=1,
            rough=0,
        )
        s.text(
            592,
            y + 3,
            f"({'dp_17' if mine else 'dp_' + str(i * 911 % 997)}, user, agent, created, id)",
            15,
            INK if mine else MUTED,
            family=3,
        )
    s.arrow([[990, 341], [968, 341]], GREEN)
    s.text(996, 329, "range\nscan", 16, GREEN)
    s.text(
        560,
        540,
        "MATERIALIZED CTE reads only this datapack's rows,\n"
        "then ranks them all by distance.\n"
        "Chosen when a bounded count finds ≤ 2,000 rows.",
        17,
    )
    s.text(560, 640, "2.2 ms, recall 1.00 at 100k (memories)", 18, GREEN)
    s.save("index-vs-exact")


def latency_vs_scale():
    s = Scene(785)
    x0, y0, w, h = 140, 110, 760, 420
    lo, hi = math.log10(0.001), math.log10(10_000)  # 1 ms .. 10,000 s
    xlo, xhi = math.log10(3_000), math.log10(2_000_000)

    def px(n):
        return x0 + (math.log10(n) - xlo) / (xhi - xlo) * w

    def py(sec):
        return y0 + h - (math.log10(sec) - lo) / (hi - lo) * h

    s.text(40, 20, "memory_search p50 vs datapacks in the deployment", 30)
    s.text(40, 64, "3-node RF3 universe, r5dn.8xlarge. Log scale on both axes.", 18, MUTED)
    s.line([[x0, y0], [x0, y0 + h], [x0 + w, y0 + h]], INK, rough=0)
    for sec, label in (
        (0.001, "1 ms"),
        (0.01, "10 ms"),
        (0.1, "100 ms"),
        (1, "1 s"),
        (10, "10 s"),
        (100, "100 s"),
        (1000, "1,000 s"),
        (10_000, "10,000 s"),
    ):
        s.line([[x0, py(sec)], [x0 + w, py(sec)]], "#dee2e6", width=1, rough=0)
        s.text(x0 - 12, py(sec) - 10, label, 15, MUTED, align="right")
    for n, label in (
        (6_000, "6k"),
        (100_000, "100k"),
        (300_000, "300k"),
        (1_000_000, "1M"),
    ):
        s.line([[px(n), y0 + h], [px(n), y0 + h + 8]], INK, rough=0)
        s.text(px(n), y0 + h + 14, label, 16, align="center")

    before = [(6_000, 1.66), (100_000, 43.3), (300_000, 153), (1_000_000, 2_100)]
    s.line([[px(n), py(t)] for n, t in before], RED, width=3)
    for n, t in before:
        s.dot(px(n), py(t), 6, RED, FILL_RED)
    s.text(px(1_000_000) + 14, py(2_100) - 12, "> 35 min\n(stopped)", 16, RED)
    s.text(px(6_000) - 20, py(1.66) - 110, "before: meko_mem0_collection", 18, RED)

    exact = [(100_000, 0.0022), (1_000_000, 0.0025)]
    s.line([[px(n), py(t)] for n, t in exact], GREEN, width=3)
    for n, t in exact:
        s.dot(px(n), py(t), 6, GREEN, FILL_GREEN)
    s.text(
        px(100_000) - 60,
        py(0.0022) - 34,
        "after, small datapack (exact): 2.2 ms → 2.5 ms",
        17,
        GREEN,
    )
    s.dot(px(1_000_000), py(0.0295), 6, BLUE, FILL_BLUE)
    s.text(
        px(1_000_000) - 420,
        py(0.0295) - 30,
        "after, 5,000-row datapack (index): 29.5 ms",
        17,
        BLUE,
    )

    s.text(
        40,
        y0 + h + 60,
        "Single client, one query at a time. Through MCP at 1M across 9,170 accounts, "
        "a memory_search call took 238-274 ms end to end.",
        16,
        MUTED,
    )
    s.save("latency-vs-scale")


def keyset_paging():
    s = Scene(781)
    s.text(40, 20, "Paging GET /datapacks: offset vs keyset", 30)
    s.text(
        40,
        64,
        "idx_datapacks_user_created (user_id HASH, created_at DESC, datapack_id DESC)",
        18,
        MUTED,
        family=3,
    )

    def rows(x, y, highlight, skipped):
        for i in range(12):
            ry = y + i * 30
            if i in highlight:
                stroke, fill = GREEN, FILL_GREEN
            elif i in skipped:
                stroke, fill = RED, FILL_RED
            else:
                stroke, fill = MUTED, FILL_GREY
            s.rect(x, ry, 300, 24, stroke, fill, width=1, rough=0)
            s.text(
                x + 10,
                ry + 2,
                f"row {i + 1}",
                15,
                INK if (i in highlight or i in skipped) else MUTED,
                family=3,
            )

    s.text(40, 120, "OFFSET 8 LIMIT 4", 24, RED)
    rows(40, 164, highlight=range(8, 12), skipped=range(8))
    s.text(
        40,
        580,
        "Reads and throws away every earlier row.\n"
        "Page n costs n pages of work, and a row inserted\n"
        "meanwhile shifts every later page by one.",
        17,
    )

    s.text(520, 120, "WHERE (created, id) < cursor LIMIT 4", 24, GREEN)
    rows(520, 164, highlight=range(8, 12), skipped=())
    s.arrow([[870, 406], [830, 406]], GREEN)
    s.text(878, 392, "cursor from\nthe last page", 16, GREEN)
    s.text(
        520,
        580,
        "Seeks straight to the cursor in the index.\n"
        "Every page costs the same: at 50,000 datapacks a\n"
        "100-row page took 104-194 ms (with ~85 ms port-forward).",
        17,
    )
    s.text(
        40,
        690,
        "A cursor is bound to its sort and filters; a mismatched one is a 400, never a silent restart at page one.",
        16,
        MUTED,
    )
    s.save("keyset-paging")


if __name__ == "__main__":
    partition_pruning()
    index_vs_exact()
    latency_vs_scale()
    keyset_paging()
