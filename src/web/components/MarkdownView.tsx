import { useCallback, useEffect, useMemo, useState } from 'react';
import { createPortal } from 'react-dom';
import DOMPurify from 'dompurify';
import { renderMarkdown } from '../markdown';
import { diagramAlt, drawDiagram, svgDataUrl, type Diagram } from '../mermaid';
import { useDarkTheme } from '../theme';
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
  const [full, setFull] = useState(false);
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
            : 'Couldn’t draw this diagram. Ask AgentK to fix it; its source is below.'}
        </p>
        <pre className="mermaid-error-msg">{diagram.error}</pre>
        {code}
      </div>
    );
  }
  const img = <img src={src!} alt={alt} width={diagram.width ?? undefined} height={diagram.height ?? undefined} />;
  return (
    <figure className="mermaid-fig">
      <button type="button" className="mermaid-open" onClick={() => setFull(true)} title="Open full size" aria-label={`${alt}: open full size`}>
        {img}
      </button>
      <details className="mermaid-src">
        <summary>Source</summary>
        {code}
      </details>
      {full &&
        createPortal(
          <Dialog title={alt} wide className="diagram-dialog" onClose={() => setFull(false)}>
            <div className="diagram-view">{img}</div>
          </Dialog>,
          document.body,
        )}
    </figure>
  );
}
