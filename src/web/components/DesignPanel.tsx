import { useEffect, useMemo, useState } from 'react';
import { marked } from 'marked';
import DOMPurify from 'dompurify';
import type { DesignDoc, Session } from '../../shared/types';
import { api, errorMessage } from '../api';
import { designFrameDoc } from '../design-frame';
import { routePath } from '../router';
import { relativeTime } from '../util';
import { SidePanel } from './SidePanel';

/** The app's explicit theme (`data-theme` on <html>), or null when it follows the system. */
function useDataTheme(): string | null {
  const read = () => document.documentElement.getAttribute('data-theme');
  const [theme, setTheme] = useState(read);
  useEffect(() => {
    const obs = new MutationObserver(() => setTheme(read()));
    obs.observe(document.documentElement, { attributes: true, attributeFilter: ['data-theme'] });
    return () => obs.disconnect();
  }, []);
  return theme;
}

/**
 * The session's design: a published Artifact in a sandboxed frame, or markdown. Shown as a
 * resizable side panel next to the terminal, or the whole page in its own tab (`page`).
 */
export function DesignPanel({
  session,
  onClose,
  onApprove,
  page = false,
}: {
  session: Session;
  onClose?: () => void;
  onApprove: () => void;
  page?: boolean;
}) {
  const [doc, setDoc] = useState<DesignDoc | null>(null);
  const [error, setError] = useState<string | null>(null);

  const theme = useDataTheme();

  // Refetch whenever the design is written or republished.
  useEffect(() => {
    let cancelled = false;
    api
      .design(session.id)
      .then((d) => !cancelled && (setDoc(d), setError(null)))
      .catch((e) => !cancelled && setError(errorMessage(e)));
    return () => {
      cancelled = true;
    };
  }, [session.id, session.designUpdatedAt]);

  useEffect(() => {
    if (page) document.title = `${session.name} · Design · AgentK Local`;
  }, [page, session.name]);

  const html = useMemo(() => {
    if (!doc?.markdown) return '';
    return DOMPurify.sanitize(marked.parse(doc.markdown, { async: false }) as string);
  }, [doc?.markdown]);
  const frame = useMemo(() => (doc?.html ? designFrameDoc(doc.html, theme) : null), [doc?.html, theme]);

  const content = (
    <>
      <div className="design-hd">
        <b>{page ? `Design · ${session.name}` : 'Design'}</b>
        {doc?.updatedAt && <span className="muted small">updated {relativeTime(doc.updatedAt)}</span>}
        <span className="sp" />
        {session.stage === 'design' && session.hasDesign && (
          <button className="btn small primary" disabled={session.busy} onClick={onApprove}>
            Approve design
          </button>
        )}
        {doc?.artifactUrl && (
          <a className="btn small" href={doc.artifactUrl} target="_blank" rel="noopener noreferrer">
            Open on claude.ai ↗
          </a>
        )}
        {!page && (
          <a
            className="icon-btn"
            href={routePath({ view: 'design', projectId: session.projectId, sessionId: session.id })}
            target="_blank"
            rel="noopener"
            title="Open full page in a new tab"
            aria-label="Open the design full page in a new tab"
          >
            <svg width="15" height="15" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">
              <path d="M15 3h6v6 M9 21H3v-6 M21 3l-7 7 M3 21l7-7" />
            </svg>
          </a>
        )}
        {onClose && (
          <button className="icon-btn" onClick={onClose} aria-label="Close design">
            ×
          </button>
        )}
      </div>
      {frame && !error ? (
        // No allow-same-origin: the page can't reach AgentK Local's cookies or API.
        <iframe
          className="design-frame"
          title="Design artifact"
          sandbox="allow-scripts allow-popups allow-popups-to-escape-sandbox"
          referrerPolicy="no-referrer"
          srcDoc={frame}
        />
      ) : (
        <div className="design-body">
          {error ? (
            <p className="error-text">{error}</p>
          ) : !doc ? (
            <p className="muted">Loading…</p>
          ) : doc.markdown ? (
            <div className="prose" dangerouslySetInnerHTML={{ __html: html }} />
          ) : doc.artifactUrl ? (
            <p className="muted">
              The design is an Artifact on claude.ai. Its page couldn’t be read here, so open it with{' '}
              <b>Open on claude.ai</b>.
            </p>
          ) : (
            <p className="muted">
              No design yet. Claude is reading the code; the design shows up here as soon as it’s written, and is
              saved to the project’s datapack.
            </p>
          )}
        </div>
      )}
    </>
  );

  if (page) return <main className="design-page">{content}</main>;
  return (
    <SidePanel name="design" label="Design doc" className="design-panel">
      {content}
    </SidePanel>
  );
}
