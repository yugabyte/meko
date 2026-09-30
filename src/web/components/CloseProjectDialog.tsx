import { useState } from 'react';
import type { ProjectWithSessions } from '../../shared/types';
import { api, errorMessage } from '../api';
import { Dialog, ErrorText } from './common';

/**
 * Offered when the project on screen loses its last open session: delete it, start another
 * session, or keep it. The project has no sessions left to stop, so it only says what stays.
 */
export function CloseProjectDialog({
  project,
  onClose,
  onNewSession,
  onDeleted,
}: {
  project: ProjectWithSessions;
  onClose: () => void;
  onNewSession: () => void;
  onDeleted: () => void;
}) {
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const remove = async () => {
    setBusy(true);
    setError(null);
    try {
      await api.deleteProject(project.id);
      onDeleted();
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
          <button className="btn danger" disabled={busy} onClick={remove}>
            {busy ? 'Deleting…' : 'Delete project'}
          </button>
          <span className="sp" />
          <button className="btn" disabled={busy} onClick={onClose}>
            Keep project
          </button>
          <button className="btn primary" disabled={busy} onClick={onNewSession}>
            New session
          </button>
        </>
      }
    >
      <p className="muted">
        That was the project's last session. Delete the project if you're done with it. Your repo and datapacks stay.
      </p>
      <ErrorText error={error} />
    </Dialog>
  );
}
