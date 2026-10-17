import { useEffect, useState } from 'react';
import type { ReleaseNotes } from '../../shared/types';
import { api, errorMessage } from '../api';
import { Busy, Dialog, ErrorText } from './common';

/**
 * What's new from one version to another: every feat, fix, perf and deps change between their
 * tags, by type, across all the releases in between (GET /api/update/notes).
 */
export function WhatsNewDialog({ from, to, onClose }: { from: string; to: string; onClose: () => void }) {
  const [notes, setNotes] = useState<ReleaseNotes | null>(null);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    let live = true;
    setNotes(null);
    setError(null);
    api.updateNotes(from, to).then(
      (n) => live && setNotes(n),
      (e) => live && setError(errorMessage(e)),
    );
    return () => {
      live = false;
    };
  }, [from, to]);

  const changes = notes?.sections.filter((s) => s.title !== 'Breaking changes').reduce((n, s) => n + s.items.length, 0) ?? 0;
  const compareUrl = notes?.compareUrl ?? null;
  // GitHub gives at most 250 commits for one comparison: the rest may hold changes.
  const cut = !!notes && notes.commits < notes.totalCommits;

  return (
    <Dialog
      title={`What’s new in v${to}`}
      className="whats-new"
      onClose={onClose}
      footer={
        compareUrl && (
          <a href={compareUrl} target="_blank" rel="noreferrer" className="link-btn">
            See every commit on GitHub
          </a>
        )
      }
    >
      {!notes && !error && <Busy label="Loading" />}
      <ErrorText error={error} />
      {notes && (
        <>
          <p className="muted">
            Since v{from}: {changes} {changes === 1 ? 'change' : 'changes'} in {notes.totalCommits} {notes.totalCommits === 1 ? 'commit' : 'commits'}.
            {cut && ` Showing the changes in the first ${notes.commits}; see the rest on GitHub.`}
          </p>
          {notes.sections.length === 0 && (
            <p>
              {cut
                ? `Nothing user-facing changed in the first ${notes.commits} commits. See the rest on GitHub.`
                : `Nothing user-facing changed between v${from} and v${to}.`}
            </p>
          )}
          {notes.sections.map((s) => (
            <section key={s.title}>
              <h3>{s.title}</h3>
              <ul>
                {s.items.map((i) => (
                  <li key={`${i.sha}:${i.summary}`}>
                    {i.component && <b>{i.component}: </b>}
                    {i.summary}{' '}
                    <a href={i.url} target="_blank" rel="noreferrer" className="mono">
                      {i.pr ? `#${i.pr}` : i.sha}
                    </a>
                  </li>
                ))}
              </ul>
            </section>
          ))}
        </>
      )}
    </Dialog>
  );
}
