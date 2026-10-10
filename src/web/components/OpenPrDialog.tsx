import { useState } from 'react';
import type { Session } from '../../shared/types';
import { api, errorMessage } from '../api';
import { instructionsBlocker, openPrGate } from '../util';
import { Busy, Dialog, ErrorText, InstructionsField } from './common';

/**
 * Open PR with instructions for AgentK, sent after the PR prompt (`OpenPrRequest`). Mid-turn it
 * queues, as the gate does, and the instructions wait with it; pressed while one is queued, they
 * replace its instructions.
 */
export function OpenPrDialog({ session, onClose }: { session: Session; onClose: () => void }) {
  const [instructions, setInstructions] = useState('');
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);

  const pr = openPrGate(session);
  const queues = pr.label !== 'Open PR';
  const blocker =
    (session.stage !== 'code' ? 'Open PR is only available in the Code stage.' : null) ??
    (session.busy ? 'Wait for AgentK to finish.' : null) ??
    pr.reason ??
    instructionsBlocker(instructions);

  const open = async () => {
    if (blocker || busy) return;
    setError(null);
    setBusy(true);
    try {
      await api.openPr(session.id, instructions.trim() ? { instructions: instructions.trim() } : {});
      onClose();
    } catch (e) {
      setError(errorMessage(e));
    } finally {
      setBusy(false);
    }
  };

  return (
    <Dialog
      title="Open PR with instructions"
      onClose={onClose}
      footer={
        <>
          <button className="btn" onClick={onClose}>
            Cancel
          </button>
          <button className="btn primary" disabled={!!blocker || busy} title={blocker ?? pr.title} onClick={() => void open()}>
            {busy ? <Busy label="Opening" /> : pr.label}
          </button>
        </>
      }
    >
      <form
        className="stack"
        onSubmit={(e) => {
          e.preventDefault();
          void open();
        }}
      >
        <InstructionsField
          id="pr-instructions"
          value={instructions}
          onChange={setInstructions}
          autoFocus
          hint={
            session.queued === 'open-pr'
              ? 'An Open PR is already queued: these replace any instructions it has.'
              : queues
                ? 'AgentK is mid-turn: the PR prompt and these go when the turn ends.'
                : 'Sent after the PR prompt, which commits, pushes and opens (or updates) the pull request.'
          }
          placeholder="e.g. Open it as a draft, and target release-2.4."
        />
        <ErrorText error={error} />
      </form>
    </Dialog>
  );
}
