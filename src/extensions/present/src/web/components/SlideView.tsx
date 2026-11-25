// A slide as the room sees it, drawn from its blocks with the built-in template. The same component
// draws the editor's preview, the console's current and next slides, and the audience window.

import DOMPurify from 'dompurify';
import { marked } from 'marked';
import { useEffect, useId, useState } from 'react';
import { blocksOf, type Block, type Deck, type Slide } from '../../shared/deck.js';
import { assetUrl } from '../api.js';
import { cx } from './ui.js';

/**
 * What deck text may hold beyond Markdown's own HTML. It can come from datapacks or the web, so no
 * element or attribute that could restyle or cover the page (style, class, id) or take input.
 */
const MARKDOWN_HTML = {
  USE_PROFILES: { html: true },
  FORBID_TAGS: ['style', 'form', 'input', 'button', 'textarea', 'select', 'option', 'label', 'dialog'],
  FORBID_ATTR: ['style', 'class', 'id'],
};

function Markdown({ text }: { text: string }) {
  const html = DOMPurify.sanitize(marked.parse(text, { async: false }) as string, MARKDOWN_HTML);
  return <div className="md" dangerouslySetInnerHTML={{ __html: html }} />;
}

/** A Mermaid diagram, drawn once Mermaid's chunk has loaded, in strict mode. */
function Diagram({ text }: { text: string }) {
  const id = `m${useId().replace(/[^a-zA-Z0-9]/g, '')}`;
  const [svg, setSvg] = useState<string | null>(null);
  const [failed, setFailed] = useState(false);
  useEffect(() => {
    let live = true;
    void import('mermaid').then(async ({ default: mermaid }) => {
      // SVG text labels: HTML labels sit in <foreignObject>, which the sanitizer below removes.
      mermaid.initialize({ startOnLoad: false, securityLevel: 'strict', theme: 'dark', flowchart: { htmlLabels: false }, htmlLabels: false });
      try {
        const r = await mermaid.render(id, text);
        if (!live) return;
        setSvg(DOMPurify.sanitize(r.svg, { USE_PROFILES: { svg: true, svgFilters: true } }));
        setFailed(false);
      } catch {
        if (live) setFailed(true);
      }
    });
    return () => {
      live = false;
    };
  }, [id, text]);
  if (failed) return <pre className="whitespace-pre-wrap text-[0.6em] text-slide-muted">{text}</pre>;
  return svg ? <div className="[&_svg]:mx-auto [&_svg]:max-h-[40cqw] [&_svg]:max-w-full" dangerouslySetInnerHTML={{ __html: svg }} /> : <div className="text-slide-muted">Drawing…</div>;
}

function BlockView({ b, deckId, shown }: { b: Block; deckId: string; shown: number }) {
  switch (b.type) {
    case 'heading': return <h2>{b.text}</h2>;
    case 'text': return <div className="text-slide-fg/90"><Markdown text={b.markdown} /></div>;
    case 'bullets':
      return (
        <ul className="list-disc space-y-[0.35em] pl-[1.2em] text-slide-fg/90">
          {b.items.map((it, i) => (
            <li key={i} className={cx('transition-opacity duration-300', b.build && i >= shown && 'opacity-0')}>{it}</li>
          ))}
        </ul>
      );
    case 'metric':
      return (
        <div>
          <div className="text-[3.2em] font-bold leading-none text-slide-accent">{b.value}</div>
          <div className="text-slide-muted">{b.label}</div>
        </div>
      );
    case 'quote':
      return (
        <blockquote className="border-l-[0.2em] border-slide-accent pl-[0.8em] text-[1.3em] italic">
          “{b.text}”{b.by && <footer className="mt-[0.4em] text-[0.7em] not-italic text-slide-muted">{b.by}</footer>}
        </blockquote>
      );
    case 'table':
      return (
        <table className="w-full border-collapse text-[0.8em]">
          <thead><tr>{b.header.map((h, i) => <th key={i} className="border-b border-slide-muted/40 p-[0.3em] text-left text-slide-muted">{h}</th>)}</tr></thead>
          <tbody>{b.rows.map((r, i) => <tr key={i}>{r.map((c, j) => <td key={j} className="border-b border-slide-muted/20 p-[0.3em]">{c}</td>)}</tr>)}</tbody>
        </table>
      );
    case 'image': return <img src={assetUrl(deckId, b.asset)} alt={b.alt} className="mx-auto max-h-[38cqw] max-w-full object-contain" />;
    case 'video':
      return (
        <figure>
          <video src={assetUrl(deckId, b.asset)} controls className="mx-auto max-h-[38cqw] max-w-full" />
          {b.caption && <figcaption className="mt-[0.3em] text-center text-[0.7em] text-slide-muted">{b.caption}</figcaption>}
        </figure>
      );
    case 'diagram': return <Diagram text={b.mermaid} />;
    case 'animation':
      // The animation player comes with animation datapacks; until then the slide names what plays here.
      return (
        <div className="rounded-[0.4em] border border-dashed border-slide-muted/50 p-[0.8em] text-center text-slide-muted">
          Animation <span className="font-mono text-slide-fg">{b.ref}</span>
        </div>
      );
    case 'code': return <pre className="overflow-hidden rounded-[0.3em] bg-black/30 p-[0.6em] text-[0.7em] text-slide-fg"><code>{b.code}</code></pre>;
  }
}

/** Splits blocks into the layout's columns: visual blocks go right in two-column and diagram-right. */
function columns(slide: Slide, blocks: Block[]): [Block[], Block[]] {
  if (slide.layout !== 'two-column' && slide.layout !== 'diagram-right') return [blocks, []];
  const visual = (b: Block) => ['image', 'video', 'diagram', 'animation', 'code', 'table'].includes(b.type);
  const right = blocks.filter((b, i) => (slide.layout === 'two-column' ? i >= Math.ceil(blocks.length / 2) && b.type !== 'heading' : visual(b)));
  return [blocks.filter((b) => !right.includes(b)), right];
}

export function SlideView({ deck, deckId, slide, variant, step = Infinity, className, label }: {
  deck: Deck; deckId: string; slide: Slide | undefined; variant: string; step?: number; className?: string; label?: string;
}) {
  if (!slide) return <div className={cx('slide-frame aspect-video rounded bg-slide', className)} />;
  const blocks = blocksOf(deck, slide, variant);
  // A build step shows one bullet of the slide's build lists, in order across them.
  let left = step;
  const shownFor = blocks.map((b) => {
    if (b.type !== 'bullets' || !b.build) return Infinity;
    const n = Math.max(0, Math.min(left, b.items.length));
    left -= n;
    return n;
  });
  const [main, side] = columns(slide, blocks);
  const render = (list: Block[]) => list.map((b) => {
    const i = blocks.indexOf(b);
    return <BlockView key={i} b={b} deckId={deckId} shown={shownFor[i]!} />;
  });
  const centered = slide.layout === 'title' || slide.layout === 'section' || slide.layout === 'big-number' || slide.layout === 'quote';
  return (
    <div className={cx('slide-frame aspect-video w-full overflow-hidden rounded bg-slide text-slide-fg', className)} aria-label={label}>
      <div className={cx('slide flex h-full gap-[4cqw] p-[5cqw]', centered ? 'flex-col items-center justify-center text-center' : 'items-stretch')}>
        <div className={cx('flex min-w-0 flex-1 flex-col gap-[1.6cqw]', !centered && 'justify-center')}>{render(main)}</div>
        {side.length > 0 && <div className="flex min-w-0 flex-1 flex-col justify-center gap-[1.6cqw]">{render(side)}</div>}
      </div>
    </div>
  );
}
