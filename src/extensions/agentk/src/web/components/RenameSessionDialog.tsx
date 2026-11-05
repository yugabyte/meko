import { useState } from 'react';
import { SESSION_NAME_MAX, type Session } from '../../shared/types';
import { api, errorMessage } from '../api';
import { Busy, Dialog, ErrorText, Field } from './common';

/** Renames a session. Only the name changes: the branch and worktree keep the names they started with. */
export function RenameSessionDialog({ session, onClose }: { session: Session; onClose: () => void }) {
  const [name, setName] = useState(session.name);
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  const trimmed = name.replace(/\s+/g, ' ').trim();
  const blocker = !trimmed ? 'Give the session a name.' : null;

  const save = async () => {
    if (blocker || busy) return;
    if (trimmed === session.name) {
      onClose();
      return;
    }
    setError(null);
    setBusy(true);
    try {
      await api.updateSession(session.id, { name: trimmed });
      onClose();
    } catch (e) {
      setError(errorMessage(e));
    } finally {
      setBusy(false);
    }
  };

  return (
    <Dialog
      title="Rename session"
      onClose={onClose}
      footer={
        <>
          <button className="btn" onClick={onClose}>
            Cancel
          </button>
          <button className="btn primary" disabled={!!blocker || busy} title={blocker ?? undefined} onClick={() => void save()}>
            {busy ? <Busy label="Renaming" /> : 'Rename'}
          </button>
        </>
      }
    >
      <form
        className="stack"
        onSubmit={(e) => {
          e.preventDefault();
          void save();
        }}
      >
        <Field label="Name" hint={`Branch ${session.branch} and the session's worktree keep their names.`}>
          <input
            id="rename-session-name"
            className="input"
            value={name}
            maxLength={SESSION_NAME_MAX}
            autoComplete="off"
            onChange={(e) => setName(e.target.value)}
            onFocus={(e) => e.target.select()}
          />
        </Field>
        <ErrorText error={error} />
      </form>
    </Dialog>
  );
}
