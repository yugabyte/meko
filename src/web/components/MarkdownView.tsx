import { useCallback, useEffect, useLayoutEffect, useMemo, useRef, useState, type CSSProperties, type ReactNode, type RefObject } from 'react';
import { createPortal } from 'react-dom';
import DOMPurify from 'dompurify';
import { renderMarkdown } from '../markdown';
import { DIAGRAM_FAILED, diagramAlt, drawDiagram, svgDataUrl, zoomStep, type Diagram } from '../mermaid';
import { useDarkTheme } from '../theme';
import { cx } from '../util';
import { Busy, Dialog } from './common';

// Markdown designs render in the app's own page, so they get no forms or styles (an invisible
// form button could post to the API with the user's cookie, and a <style> could restyle the
// app), and their links open in a new tab instead of replacing the workbench.
// Its own DOMPurify instance: Mermaid adds hooks of its own to the shared one (keeping an <a>'s
// target, which would undo the new-tab rule below), and those must never reach this.
const purify = DOMPurify(window);
const PURIFY = {
  FORBID_TAGS: ['form', 'button', 'select', 'textarea', 'style', 'link', 'meta', 'base'],
  FORBID_ATTR: ['style', 'action', 'formaction'],
};
purify.addHook('afterSanitizeAttributes', (node) => {
  // Task-list checkboxes (`- [x] done`) are the one kind of input kept: any input becomes a
  // read-only tick, and with no forms or buttons it can't send anything.
  if (node.tagName === 'INPUT') {
    node.setAttribute('type', 'checkbox');
    node.setAttribute('disabled', '');
  }
  if (node.tagName === 'A' && node.hasAttribute('href')) {
    node.setAttribute('target', '_blank');
    node.setAttribute('rel', 'noopener noreferrer');
  }
});

/** Sanitized markdown, with its ```mermaid blocks drawn as diagrams. */
export function MarkdownView({ markdown }: { markdown: string }) {
  const dark = useDarkTheme();
  const { html, diagrams } = useMemo(() => {
    const r = renderMarkdown(markdown);
    return { html: purify.sanitize(r.html, PURIFY), diagrams: r.diagrams };
  }, [markdown]);
  // Keyed by the HTML: new HTML replaces the slots, so the diagrams mount again into the new ones.
  return <Prose key={html} html={html} diagrams={diagrams} dark={dark} />;
}

function Prose({ html, diagrams, dark }: { html: string; diagrams: string[]; dark: boolean }) {
  const [slots, setSlots] = useState<HTMLElement[]>([]);
  // Called once, when the HTML is in: each slot gets its diagram through a portal.
  const found = useCallback((el: HTMLDivElement | null) => {
    if (el) setSlots(Array.from(el.querySelectorAll<HTMLElement>('div.mermaid-slot[data-mermaid]')));
  }, []);
  return (
    <>
      <div ref={found} className="prose" dangerouslySetInnerHTML={{ __html: html }} />
      {slots.map((slot, i) => {
        const source = diagrams[Number(slot.dataset.mermaid)];
        return source === undefined ? null : createPortal(<MermaidDiagram source={source} dark={dark} />, slot, i);
      })}
    </>
  );
}

function MermaidDiagram({ source, dark }: { source: string; dark: boolean }) {
  // The last one drawn stays up while the next is drawn (after a theme change), so nothing flickers.
  const [diagram, setDiagram] = useState<Diagram | null>(null);
  useEffect(() => {
    let live = true;
    void drawDiagram(source, dark).then((d) => live && setDiagram(d));
    return () => {
      live = false;
    };
  }, [source, dark]);
  const alt = useMemo(() => diagramAlt(source), [source]);
  const src = useMemo(() => (diagram && 'svg' in diagram ? svgDataUrl(diagram.svg) : null), [diagram]);

  const code = (
    <pre>
      <code>{source}</code>
    </pre>
  );
  if (!diagram) {
    return (
      <p className="muted mermaid-busy">
        <Busy label="Drawing diagram" />
      </p>
    );
  }
  if ('error' in diagram) {
    return (
      <div className="mermaid-error">
        <p className="error-text">
          {'retry' in diagram && diagram.retry
            ? 'Couldn’t draw this diagram. Its source is below.'
            : DIAGRAM_FAILED}
        </p>
        <pre className="mermaid-error-msg">{diagram.error}</pre>
        {code}
      </div>
    );
  }
  return <DiagramFigure src={src!} alt={alt} width={diagram.width} height={diagram.height} code={code} />;
}

type Shown = { src: string; alt: string; width: number | null; height: number | null };

/** A drawn diagram fitted to the column, its source under it; a click opens it full size. */
function DiagramFigure({ code, ...shown }: Shown & { code: ReactNode }) {
  const { src, alt, width, height } = shown;
  const [full, setFull] = useState(false);
  const zoom = useZoom(width, null);
  return (
    <figure className="mermaid-fig">
      <div className={cx('mermaid-view', zoom.scale !== null && 'zoomed')}>
        <div className="diagram-zoom-pin">{zoom.controls}</div>
        <div ref={zoom.box} className="diagram-scroll">
          <button type="button" className="mermaid-open" onClick={() => setFull(true)} title="Open full size" aria-label={`${alt}: open full size`}>
            <img ref={zoom.img} src={src} alt={alt} width={width ?? undefined} height={height ?? undefined} style={zoom.style} />
          </button>
        </div>
      </div>
      <details className="mermaid-src">
        <summary>Source</summary>
        {code}
      </details>
      {full && createPortal(<DiagramDialog {...shown} onClose={() => setFull(false)} />, document.body)}
    </figure>
  );
}

/** A diagram full size: at its natural size to start, scrolling when it's bigger than the dialog. */
function DiagramDialog({ src, alt, width, height, onClose }: Shown & { onClose: () => void }) {
  const zoom = useZoom(width, 1);
  return (
    <Dialog title={alt} wide className="diagram-dialog" aside={zoom.controls} onClose={onClose}>
      <div ref={zoom.box} className={cx('diagram-view', zoom.scale === null && 'fit')}>
        <img ref={zoom.img} src={src} alt={alt} width={width ?? undefined} height={height ?? undefined} style={zoom.style} />
      </div>
    </Dialog>
  );
}

/**
 * Zooming a diagram's image with − and + buttons, and its size between them, which fits it back to
 * the width it has. `scale` is a share of the natural `width`, or null while fitted. For mice: the
 * buttons are hidden on touch screens, where pinching zooms. Nothing to zoom without a width.
 */
function useZoom(
  width: number | null,
  start: number | null,
): { scale: number | null; box: RefObject<HTMLDivElement | null>; img: RefObject<HTMLImageElement | null>; style?: CSSProperties; controls: ReactNode } {
  const [scale, setScale] = useState(start);
  // The box that scrolls when the diagram outgrows it.
  const box = useRef<HTMLDivElement>(null);
  const img = useRef<HTMLImageElement>(null);
  // Where the middle of the view was in the diagram before a zoom, to keep it there after: across
  // the box's own scroll, and down whatever scrolls up and down (the box in the dialog; in the
  // column the box grows with the image, so the design panel around it).
  const anchor = useRef<{ x: number; y: number; at: number } | null>(null);
  useLayoutEffect(() => {
    const el = box.current;
    const a = anchor.current;
    anchor.current = null;
    if (!el || !img.current || !a) return;
    el.scrollLeft = a.x * el.scrollWidth - el.clientWidth / 2;
    const r = img.current.getBoundingClientRect();
    const scroller = verticalScroller(el);
    if (scroller) scroller.scrollTop += r.top + a.y * r.height - a.at;
  }, [scale]);

  const zoomTo = (next: number | null) => {
    const el = box.current;
    if (next === scale || !el || !img.current) return;
    const r = img.current.getBoundingClientRect();
    const at = visibleMiddle(img.current);
    anchor.current = { x: (el.scrollLeft + el.clientWidth / 2) / el.scrollWidth, y: r.height ? (at - r.top) / r.height : 0, at };
    setScale(next);
  };
  const step = (dir: 1 | -1) => {
    // A fitted diagram steps from the size it's shown at.
    const now = scale ?? (img.current && width ? img.current.getBoundingClientRect().width / width : 1);
    const next = zoomStep(now, dir);
    if (next !== null) zoomTo(next);
  };
  // A fitted diagram's size is only known once measured, so both buttons stay on while it's fitted.
  // A button that can't act is aria-disabled, not disabled: a disabled button loses focus, and the
  // one pressed to reach the end would drop a keyboard user's place.
  const can = (dir: 1 | -1) => scale === null || zoomStep(scale, dir) !== null;

  const controls = width ? (
    <div className="diagram-zoom" role="group" aria-label="Zoom">
      <button type="button" className="icon-btn" onClick={() => step(-1)} aria-disabled={!can(-1)} title="Zoom out" aria-label="Zoom out">
        −
      </button>
      <button type="button" className="icon-btn diagram-zoom-level" onClick={() => zoomTo(null)} aria-disabled={scale === null} title="Fit to width">
        {scale === null ? 'Fit' : `${Math.round(scale * 100)}%`}
      </button>
      <button type="button" className="icon-btn" onClick={() => step(1)} aria-disabled={!can(1)} title="Zoom in" aria-label="Zoom in">
        +
      </button>
    </div>
  ) : null;
  const style = scale !== null && width ? { width: Math.round(width * scale), maxWidth: 'none' } : undefined;
  return { scale, box, img, style, controls };
}

/** The screen height of the middle of what's visible of `el`, inside the boxes that clip it and the window. */
function visibleMiddle(el: HTMLElement): number {
  const r = el.getBoundingClientRect();
  let top = Math.max(r.top, 0);
  let bottom = Math.min(r.bottom, window.innerHeight);
  for (let p = el.parentElement; p; p = p.parentElement) {
    if (getComputedStyle(p).overflowY === 'visible') continue;
    const pr = p.getBoundingClientRect();
    top = Math.max(top, pr.top);
    bottom = Math.min(bottom, pr.bottom);
  }
  return bottom > top ? (top + bottom) / 2 : r.top + r.height / 2;
}

/** `el`, or the nearest box around it, that scrolls up and down; null when none does. */
function verticalScroller(el: HTMLElement): HTMLElement | null {
  for (let p: HTMLElement | null = el; p && p !== document.documentElement; p = p.parentElement) {
    const o = getComputedStyle(p).overflowY;
    if ((o === 'auto' || o === 'scroll') && p.scrollHeight > p.clientHeight) return p;
  }
  return null;
}
