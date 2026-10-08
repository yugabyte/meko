# Diagrams

The docs one level up embed the PNGs in `img/`. Edit a source here, then regenerate.

| Source | Tool | Image |
|---|---|---|
| `archify/harness-architecture.architecture.json` | Archify | `img/harness-architecture.png` |
| `archify/benchmark-data.dataflow.json` | Archify | `img/benchmark-data.png` |
| `archify/memory-search.sequence.json` | Archify | `img/memory-search.png` |
| `archify/collection-rollout.lifecycle.json` | Archify | `img/collection-rollout.png` |
| `archify/kb-import.sequence.json` | Archify | `img/kb-import.png` |
| `archify/datapack-listing.workflow.json` | Archify | `img/datapack-listing.png` |
| `excalidraw/gen.py` | Excalidraw | `img/partition-pruning.png`, `img/index-vs-exact.png`, `img/latency-vs-scale.png`, `img/keyset-paging.png` |

Archify draws the structured charts: components, calls in order, states,
lanes, stages. Excalidraw draws the explainers that need free placement: a
partition grid, a vector neighbourhood, a log-log chart drawn to scale.

```bash
./check.sh sequence archify/memory-search.sequence.json   # showcase validation, diagnostics only
python3 excalidraw/gen.py                                  # rewrite the .excalidraw scenes
uv run --with playwright python render.py [name ...]       # all images, or the named ones
```

`render.py` needs Node and the Archify skill (`ARCHIFY` overrides its path),
plus network access: the Excalidraw exporter and its fonts load from unpkg and
excalidraw.com. It also writes `archify/<name>.html`, an interactive version of
each chart, and `img/<name>.svg` for each Excalidraw scene; the SVG loads its fonts
from the web, which is why the docs embed the PNG.

The PNGs show the chart only. The notes Archify draws beside a chart are in each
spec's `cards` and are repeated in the doc text.

The chart numbers in `excalidraw/gen.py` are copied from the measurements in the
docs. Change them in both places.
