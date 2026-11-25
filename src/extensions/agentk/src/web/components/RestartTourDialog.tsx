import { useState } from 'react';
import { AGENTK_TOUR } from '../../shared/types';
import { api, errorMessage } from '../api';
import { Busy, Dialog, ErrorText } from './common';

/**
 * Start the Kode tour over (the command palette, while a tour is open): asks first, since the server
 * closes the tour's project and deletes its demo folder, with any changes in it, and its datapacks,
 * then starts a new tour (`POST /api/workshops/:id/start-over`).
 */
export function RestartTourDialog({ onClose, onStarted }: { onClose: () => void; onStarted: (projectId: string) => void }) {
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const startOver = async () => {
    setBusy(true);
    setError(null);
    try {
      const r = await api.startOverWorkshop(AGENTK_TOUR);
      onStarted(r.projectId);
    } catch (e) {
      setError(errorMessage(e));
      setBusy(false);
    }
  };
  const close = () => {
    if (!busy) onClose();
  };

  return (
    <Dialog
      title="Start the Kode tour over?"
      onClose={close}
      footer={
        <>
          <span className="sp" />
          <button className="btn" disabled={busy} onClick={close}>
            Cancel
          </button>
          <button className="btn danger" disabled={busy} onClick={() => void startOver()}>
            {busy ? <Busy label="Starting over" /> : 'Start over'}
          </button>
        </>
      }
    >
      <p className="muted">
        Kode closes the tour's project and archives its sessions, then deletes its demo folder, with any changes made in it, and its two Meko
        datapacks. A new tour starts from the beginning.
      </p>
      <ErrorText error={error} />
    </Dialog>
  );
}
