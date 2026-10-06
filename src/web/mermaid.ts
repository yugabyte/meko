// Drawing a markdown design's Mermaid diagrams.
//
// Mermaid is imported only when a design has a diagram, so it stays out of the main bundle. Its
// SVG is shown as an <img> (see `svgDataUrl`), never inline: an image can't run scripts, load
// anything or restyle the app, so a bug in Mermaid can't reach AgentK Local's page.

import type { MermaidConfig } from 'mermaid';

/**
 * Diagrams use a system font, not the app's web font: Mermaid measures labels in the page, but
 * an SVG drawn as an image can't load web fonts, so a web font would leave labels misfit.
 */
export const DIAGRAM_FONT = "system-ui, -apple-system, 'Segoe UI', Roboto, sans-serif";

/** Most drawn diagrams kept, so a revision that leaves a diagram alone doesn't draw it again. */
const MAX_CACHED = 50;
/** Most characters of Mermaid's error message shown. */
const ERROR_CHARS = 600;

export type Diagram = { svg: string; width: number | null; height: number | null } | { error: string };

export function mermaidConfig(dark: boolean): MermaidConfig {
  return {
    startOnLoad: false,
    // Labels are escaped and click callbacks are off.
    securityLevel: 'strict',
    // SVG text instead of <foreignObject> HTML, which images draw differently across browsers.
    htmlLabels: false,
    // SVG labels wrap to fit the default 200 px by splitting words ("renderMarkdo wn()").
    flowchart: { wrappingWidth: 300 },
    theme: dark ? 'dark' : 'default',
    fontFamily: DIAGRAM_FONT,
    themeVariables: { fontFamily: DIAGRAM_FONT },
    suppressErrorRendering: true,
  };
}

/** The diagram's size from its viewBox, so the image has a natural size; null parts when it has none. */
export function diagramSize(svg: string): { width: number | null; height: number | null } {
  const box = /<svg\b[^>]*?\sviewBox\s*=\s*["']([^"']*)["']/i.exec(svg)?.[1];
  const [, , w, h] = (box ?? '').trim().split(/[\s,]+/).map(Number);
  const ok = (n: number | undefined) => (n !== undefined && Number.isFinite(n) && n > 0 ? Math.ceil(n) : null);
  return ok(w) && ok(h) ? { width: ok(w), height: ok(h) } : { width: null, height: null };
}

/** The SVG as an image URL. Never opened as a page: an SVG document could run its scripts. */
export function svgDataUrl(svg: string): string {
  return `data:image/svg+xml;charset=utf-8,${encodeURIComponent(svg)}`;
}

/** The lines that say what a diagram is, without front matter, directives and comments. */
function bodyLines(source: string): string[] {
  const text = source.replace(/^\s*---\r?\n[\s\S]*?\r?\n---[^\n]*(\n|$)/, '');
  return text
    .split(/\r?\n/)
    .map((l) => l.trim())
    .filter((l) => l && !l.startsWith('%%'));
}

/** Kinds whose name doesn't follow from their keyword. */
const KIND_NAMES: Record<string, string> = {
  graph: 'flowchart',
  pie: 'pie chart',
  gantt: 'Gantt chart',
  'er diagram': 'entity relationship diagram',
};

/** The image's alt text: the diagram's accTitle, else the kind of diagram it is. */
export function diagramAlt(source: string): string {
  const lines = bodyLines(source);
  for (const l of lines) {
    const title = /^accTitle\s*:\s*(.+)$/.exec(l)?.[1].trim();
    if (title) return title;
  }
  const kind = /^[A-Za-z][\w-]*/.exec(lines[0] ?? '')?.[0];
  if (!kind) return 'Diagram';
  const name = kind
    .replace(/-(beta|v\d+)$/i, '')
    .replace(/([a-z])([A-Z])/g, '$1 $2')
    .toLowerCase();
  let text = KIND_NAMES[name] ?? name;
  if (!/(diagram|chart|graph)$/.test(text)) text += ' diagram';
  return text[0].toUpperCase() + text.slice(1);
}

const cache = new Map<string, Promise<Diagram>>();
let queue: Promise<unknown> = Promise.resolve();
let seq = 0;

/**
 * Draws a diagram, or says why it couldn't. Mermaid's settings are global and its renders can't
 * overlap, so they take turns; results are cached by theme and source.
 */
export function drawDiagram(source: string, dark: boolean): Promise<Diagram> {
  const key = `${dark ? 'dark' : 'light'}\n${source}`;
  const hit = cache.get(key);
  if (hit) {
    // Most recently used last, so the oldest goes first.
    cache.delete(key);
    cache.set(key, hit);
    return hit;
  }
  let retry = false;
  const drawn = queue.then(async (): Promise<Diagram> => {
    let mermaid;
    try {
      mermaid = (await import('mermaid')).default;
    } catch (e) {
      // A failed download (offline, a new version deployed) is tried again next time.
      retry = true;
      return { error: `Couldn't load the diagram library: ${messageOf(e)}` };
    }
    try {
      mermaid.initialize(mermaidConfig(dark));
      // styles.css keys on this id (Mermaid's measuring container is "d" + it).
      const { svg } = await mermaid.render(`ak-mermaid-${++seq}`, source);
      return { svg, ...diagramSize(svg) };
    } catch (e) {
      return { error: messageOf(e) };
    }
  });
  queue = drawn;
  cache.set(key, drawn);
  while (cache.size > MAX_CACHED) cache.delete(cache.keys().next().value!);
  void drawn.then(() => retry && cache.get(key) === drawn && cache.delete(key));
  return drawn;
}

function messageOf(e: unknown): string {
  const text = (e instanceof Error ? e.message : String(e)).trim() || 'unknown error';
  return text.length > ERROR_CHARS ? `${text.slice(0, ERROR_CHARS)}…` : text;
}
