import { useState } from 'react';
import type { ProjectWithSessions } from '../../shared/types';
import { api, errorMessage } from '../api';
import { liveWithReviews } from '../util';
import { Busy } from './common';

/**
 * The project screen with no session on it: start one, or close the project. Close project shows
 * only while the project has no open sessions (code reviews included), so it has nothing to stop
 * and acts at once, as `CloseProjectDialog`'s does. The route can name no session for a render
 * while the project has some (App's effect then opens one), and this mustn't stop them unasked.
 * Its own module so a test can render it: `ProjectView.tsx` doesn't load outside a browser.
 */
export function EmptyProject({
  project,
  onNewSession,
  onClosed,
  onError,
}: {
  project: Pick<ProjectWithSessions, 'id' | 'name' | 'sessions'>;
  onNewSession: () => void;
  onClosed: () => void;
  onError: (msg: string) => void;
}) {
  const [closing, setClosing] = useState(false);
  const closable = liveWithReviews(project).length === 0;

  const close = async () => {
    setClosing(true);
    try {
      await api.closeProject(project.id);
      onClosed();
    } catch (e) {
      onError(errorMessage(e));
      setClosing(false);
    }
  };

  return (
    <div className="empty-main">
      <h2>No sessions in {project.name}</h2>
      <p className="muted">
        Start a session for a task. It gets its own branch and worktree. Pick what it's for: exploring, planning
        then implementing, or coding straight away.
      </p>
      <div className="empty-actions">
        {closable && (
          <button className="btn danger" disabled={closing} onClick={() => void close()}>
            {closing ? <Busy label="Closing" /> : 'Close project'}
          </button>
        )}
        <button className="btn primary" disabled={closing} onClick={onNewSession}>
          New session
        </button>
      </div>
    </div>
  );
}
