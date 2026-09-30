import { useState } from 'react';
import type { ProjectWithSessions } from '../../shared/types';
import { api, errorMessage } from '../api';
import { Dialog, ErrorText } from './common';

/**
 * Offered when the project on screen loses its last open session: close it, start another
 * session, or keep it. The project has no sessions left to stop, so it only says what stays.
 */
export function CloseProjectDialog({
  project,
  onClose,
  onNewSession,
  onClosed,
}: {
  project: ProjectWithSessions;
  onClose: () => void;
  onNewSession: () => void;
  onClosed: () => void;
}) {
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const close = async () => {
    setBusy(true);
    setError(null);
    try {
      await api.closeProject(project.id);
      onClosed();
    } catch (e) {
      setError(errorMessage(e));
      setBusy(false);
    }
  };

  return (
    <Dialog
      title={`No open sessions in ${project.name}`}
      onClose={onClose}
      footer={
        <>
          <button className="btn danger" disabled={busy} onClick={close}>
            {busy ? 'Closing…' : 'Close project'}
          </button>
          <span className="sp" />
          <button className="btn" disabled={busy} onClick={onClose}>
            Keep it open
          </button>
          <button className="btn primary" disabled={busy} onClick={onNewSession}>
            New session
          </button>
        </>
      }
    >
      <p className="muted">
        That was the project's last session. Close the project if you're done with it.{' '}
        {project.datapacks.some((d) => d.role === 'dedicated')
          ? 'Your repo and datapacks stay, and opening its datapack again brings the project back.'
          : 'Your repo stays.'}
      </p>
      <ErrorText error={error} />
    </Dialog>
  );
}
