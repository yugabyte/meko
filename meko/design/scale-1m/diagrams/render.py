"""Render every diagram source in this directory to the images the docs embed.

  archify/<name>.<type>.json  -> archify/<name>.html (interactive) + img/<name>.png
  excalidraw/<name>.excalidraw -> img/<name>.svg + img/<name>.png

Archify specs go through `archify deliver --quality showcase`, which fails on any
layout or composition error, then headless Chromium screenshots the diagram's SVG.
Excalidraw scenes are exported with @excalidraw/utils in the same browser, so the
hand-drawn fonts load; the PNG is what the docs embed because a Markdown viewer
will not fetch the SVG's web fonts.

  uv run --with playwright python render.py [name ...]
  ARCHIFY=/path/to/archify/bin/archify.mjs   # default: ~/.claude/skills/archify

Excalidraw sources are generated: edit excalidraw/gen.py and run it first.
"""

import asyncio
import json
import os
import subprocess
import sys
from pathlib import Path

from playwright.async_api import async_playwright

HERE = Path(__file__).resolve().parent
IMG = HERE / "img"
ARCHIFY = os.environ.get(
    "ARCHIFY", str(Path.home() / ".claude/skills/archify/bin/archify.mjs")
)
EXCALIDRAW_UTILS = (
    "https://unpkg.com/@excalidraw/utils@0.1.2/dist/excalidraw-utils.min.js"
)


def deliver(spec: Path) -> Path:
    name, kind = spec.name.removesuffix(".json").rsplit(".", 1)
    out = spec.with_name(f"{name}.html")
    r = subprocess.run(
        [
            "node",
            ARCHIFY,
            "deliver",
            kind,
            str(spec),
            str(out),
            "--quality",
            "showcase",
            "--json",
        ],
        capture_output=True,
        check=False,
        text=True,
    )
    if r.returncode != 0:
        sys.exit(
            f"{spec.name}: archify deliver failed\n{r.stdout[-2000:]}{r.stderr[-2000:]}"
        )
    return out


async def main(only: set) -> None:
    IMG.mkdir(exist_ok=True)
    specs = [
        s
        for s in sorted((HERE / "archify").glob("*.*.json"))
        if not only or s.name.split(".")[0] in only
    ]
    scenes = [
        s
        for s in sorted((HERE / "excalidraw").glob("*.excalidraw"))
        if not only or s.stem in only
    ]
    async with async_playwright() as p:
        browser = await p.chromium.launch()
        page = await browser.new_page(
            viewport={"width": 1600, "height": 1000},
            device_scale_factor=2,
            color_scheme="light",
        )
        for spec in specs:
            html = deliver(spec)
            await page.goto(html.as_uri())
            await page.wait_for_timeout(800)
            # The viewer's controls float over the diagram; hide everything but the SVG.
            await page.add_style_tag(
                content="body * { visibility: hidden !important; }"
                " svg[data-quality-profile], svg[data-quality-profile] *"
                " { visibility: visible !important; }"
            )
            png = IMG / f"{spec.name.split('.')[0]}.png"
            await page.locator("svg[data-quality-profile]").first.screenshot(
                path=str(png)
            )
            print(f"archify    {spec.name} -> {png.relative_to(HERE)}")
        if scenes:
            await page.set_content(
                f'<html><body style="margin:0"><script src="{EXCALIDRAW_UTILS}">'
                "</script></body></html>"
            )
            await page.wait_for_function(
                "window.ExcalidrawUtils !== undefined", timeout=60000
            )
        for scene in scenes:
            data = json.loads(scene.read_text())
            svg = await page.evaluate(
                "async (s) => (await ExcalidrawUtils.exportToSvg({elements: s.elements,"
                " appState: {...s.appState, exportBackground: true}, files: s.files || {}})).outerHTML",
                data,
            )
            (IMG / f"{scene.stem}.svg").write_text(svg)
            shot = await browser.new_page(device_scale_factor=2)
            await shot.set_content(
                f'<html><body style="margin:0;background:#fff">{svg}</body></html>'
            )
            await shot.evaluate("document.fonts.ready")
            await shot.locator("svg").first.screenshot(
                path=str(IMG / f"{scene.stem}.png")
            )
            await shot.close()
            print(f"excalidraw {scene.name} -> img/{scene.stem}.png")
        await browser.close()


if __name__ == "__main__":
    asyncio.run(main(set(sys.argv[1:])))
