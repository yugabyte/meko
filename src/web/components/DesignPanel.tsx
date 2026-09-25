import { useEffect, useMemo, useState } from 'react';
import { marked } from 'marked';
import DOMPurify from 'dompurify';
import type { DesignDoc, Session } from '../../shared/types';
import { api, errorMessage } from '../api';
import { routePath } from '../router';
import { relativeTime } from '../util';
import { SidePanel } from './SidePanel';

/**
 * The session's design doc: a resizable side panel next to the terminal, or the whole
 * page when opened in its own tab (`page`).
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

  // Refetch when the design appears, the stage moves, or new activity may have revised it.
  useEffect(() => {
    let cancelled = false;
    api
      .design(session.id)
      .then((d) => !cancelled && (setDoc(d), setError(null)))
      .catch((e) => !cancelled && setError(errorMessage(e)));
    return () => {
      cancelled = true;
    };
  }, [session.id, session.hasDesign, session.stage, session.stage === 'design' ? session.lastEventAt : null]);

  useEffect(() => {
    if (page) document.title = `${session.name} · Design · AgentK Local`;
  }, [page, session.name]);

  const html = useMemo(() => {
    if (!doc?.markdown) return '';
    return DOMPurify.sanitize(marked.parse(doc.markdown, { async: false }) as string);
  }, [doc?.markdown]);

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
        {!page && (
          <a
            className="icon-btn"
            href={routePath({ view: 'design', projectId: session.projectId, sessionId: session.id })}
            target="_blank"
            rel="noopener"
            title="Open in a new tab"
            aria-label="Open the design in a new tab"
          >
            ↗
          </a>
        )}
        {onClose && (
          <button className="icon-btn" onClick={onClose} aria-label="Close design">
            ×
          </button>
        )}
      </div>
      <div className="design-body">
        {error ? (
          <p className="error-text">{error}</p>
        ) : !doc ? (
          <p className="muted">Loading…</p>
        ) : !doc.markdown ? (
          <p className="muted">
            No design yet. Claude is in plan mode; when it proposes a plan, it appears here and is saved to the
            project’s datapack.
          </p>
        ) : (
          <div className="prose" dangerouslySetInnerHTML={{ __html: html }} />
        )}
      </div>
    </>
  );

  if (page) return <main className="design-page">{content}</main>;
  return (
    <SidePanel name="design" label="Design doc" className="design-panel">
      {content}
    </SidePanel>
  );
}
