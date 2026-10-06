import { useEffect, useMemo, useState } from 'react';
import { marked } from 'marked';
import DOMPurify from 'dompurify';
import type { DesignDoc, Session } from '../../shared/types';
import { api, errorMessage } from '../api';
import { designFrameDoc } from '../design-frame';
import { useIsMobile } from '../hooks/useMedia';
import { routePath } from '../router';
import { approveLabel, cx, designApprovalBlocker, relativeTime } from '../util';
import { DesignQuestions } from './DesignQuestions';
import { SidePanel } from './SidePanel';
import { Busy } from './common';

// Markdown designs render in the app's own page, so they get no forms or styles (an invisible
// form button could post to the API with the user's cookie, and a <style> could restyle the
// app), and their links open in a new tab instead of replacing the workbench.
const PURIFY = {
  FORBID_TAGS: ['form', 'button', 'select', 'textarea', 'style', 'link', 'meta', 'base'],
  FORBID_ATTR: ['style', 'action', 'formaction'],
};
DOMPurify.addHook('afterSanitizeAttributes', (node) => {
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
 * The session's design: a published Artifact in a sandboxed frame, or markdown, and its open
 * questions to answer. Shown as a resizable side panel next to the terminal (the questions on
 * their own tab), or the whole page in its own tab (`page`, the questions beside the design on a
 * wide screen).
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
  const [tab, setTab] = useState<'design' | 'questions'>('design');
  const mobile = useIsMobile();

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
    return DOMPurify.sanitize(marked.parse(doc.markdown, { async: false }) as string, PURIFY);
  }, [doc?.markdown]);
  const frame = useMemo(() => (doc?.html ? designFrameDoc(doc.html, theme) : null), [doc?.html, theme]);

  // Questions show while designing (other feedback goes there too), and afterwards if there were any.
  const hasQuestions = !!doc && !error && (session.stage === 'design' || doc.questions.length > 0);
  const split = hasQuestions && page && !mobile;
  const tabs = hasQuestions && !split;
  const showDoc = !tabs || tab === 'design';
  const open = session.stage === 'design' ? session.designQuestions.open : 0;
  const unsent = session.stage === 'design' ? session.designQuestions.unsent : 0;

  const docView = (
    <>
      {frame && !error ? (
        // No allow-same-origin: the page can't reach AgentK Local's cookies or API.
        <iframe
          hidden={!showDoc}
          className="design-frame"
          title="Design artifact"
          sandbox="allow-scripts allow-popups allow-popups-to-escape-sandbox"
          referrerPolicy="no-referrer"
          srcDoc={frame}
        />
      ) : (
        <div className="design-body" hidden={!showDoc}>
          {error ? (
            <p className="error-text">{error}</p>
          ) : !doc ? (
            <p className="muted"><Busy label="Loading" /></p>
          ) : doc.markdown ? (
            <div className="prose" dangerouslySetInnerHTML={{ __html: html }} />
          ) : doc.artifactUrl ? (
            <p className="muted">
              The design is an Artifact on claude.ai. Its page couldn’t be read here, so open it with{' '}
              <b>Open on claude.ai</b>.
            </p>
          ) : (
            <p className="muted">
              No design yet. AgentK is reading the code; the design shows up here as soon as it’s written, and is
              saved to the project’s datapack.
            </p>
          )}
        </div>
      )}
    </>
  );

  const content = (
    <>
      <div className={cx('design-hd', tabs && 'has-tabs')}>
        <b>{page ? `Design · ${session.name}` : 'Design'}</b>
        {doc?.updatedAt && <span className="muted small design-updated">updated {relativeTime(doc.updatedAt)}</span>}
        {tabs && (
          <div className="seg design-tabs" role="tablist" aria-label="Design and its questions">
            <button role="tab" aria-selected={tab === 'design'} className={cx(tab === 'design' && 'on')} onClick={() => setTab('design')}>
              Design
            </button>
            <button role="tab" aria-selected={tab === 'questions'} className={cx(tab === 'questions' && 'on')} onClick={() => setTab('questions')}>
              Questions
              {open > 0 && <span className="count-badge" aria-label={`${open} open`}>{open}</span>}
            </button>
          </div>
        )}
        <span className="sp" />
        {session.stage === 'design' && session.hasDesign && (
          <button
            className="btn small primary"
            disabled={session.busy || !!designApprovalBlocker(session)}
            title={designApprovalBlocker(session) ?? undefined}
            onClick={onApprove}
          >
            {approveLabel(unsent)}
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
          <button className="icon-btn panel-close" onClick={onClose} aria-label="Close design">
            <span className="desktop-only">×</span>
            <span className="mobile-only">← Back</span>
          </button>
        )}
      </div>
      {split ? (
        <div className="design-split">
          <div className="design-split-doc">{docView}</div>
          <aside className="design-qcol" aria-label="Open questions">
            <div className="design-qcol-hd">
              Questions{open > 0 && <span className="count-badge">{open}</span>}
            </div>
            <DesignQuestions session={session} doc={doc!} />
          </aside>
        </div>
      ) : (
        docView
      )}
      {tabs && tab === 'questions' && (
        <div className="dq-view" role="tabpanel">
          <DesignQuestions session={session} doc={doc!} />
        </div>
      )}
      {tabs && tab === 'design' && open > 0 && (
        <button className="dq-bar" onClick={() => setTab('questions')}>
          <span>{open === 1 ? '1 open question' : `${open} open questions`}</span>
          <span className="sp" />
          <span>Answer ›</span>
        </button>
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
