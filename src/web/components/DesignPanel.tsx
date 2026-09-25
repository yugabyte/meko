import { useEffect, useMemo, useState } from 'react';
import { marked } from 'marked';
import DOMPurify from 'dompurify';
import type { DesignDoc, Session } from '../../shared/types';
import { api, errorMessage } from '../api';
import { relativeTime } from '../util';

export function DesignPanel({
  session,
  onClose,
  onApprove,
}: {
  session: Session;
  onClose: () => void;
  onApprove: () => void;
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

  const html = useMemo(() => {
    if (!doc?.markdown) return '';
    return DOMPurify.sanitize(marked.parse(doc.markdown, { async: false }) as string);
  }, [doc?.markdown]);

  return (
    <aside className="design-panel" aria-label="Design doc">
      <div className="design-hd">
        <b>Design</b>
        {doc?.updatedAt && <span className="muted small">updated {relativeTime(doc.updatedAt)}</span>}
        <span className="sp" />
        {session.stage === 'design' && session.hasDesign && (
          <button className="btn small primary" disabled={session.busy} onClick={onApprove}>
            Approve design
          </button>
        )}
        <button className="icon-btn" onClick={onClose} aria-label="Close design">
          ×
        </button>
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
    </aside>
  );
}
