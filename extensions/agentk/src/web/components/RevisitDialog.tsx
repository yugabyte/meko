import { useState } from 'react';
import { REVISIT_MAX_MINUTES, type Session } from '../../shared/types';
import { api, errorMessage } from '../api';
import { REVISIT_PRESETS, revisitWhen, revisitLeft } from '../util';
import { Busy, Dialog, ErrorText, Field } from './common';

/**
 * Revisit later: sets a session aside until a time, or changes or clears that time. Until it comes,
 * the session's dot shows the revisit mark instead of its status, so it doesn't draw the eye.
 */
export function RevisitDialog({ session, onClose }: { session: Session; onClose: () => void }) {
  const [amount, setAmount] = useState('');
  const [unit, setUnit] = useState<'min' | 'h'>('min');
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState<number | 'clear' | 'custom' | null>(null);
  const n = Number(amount);
  const custom = unit === 'h' ? Math.round(n * 60) : n;
  const blocker = !amount.trim()
    ? 'Type how long.'
    : !Number.isFinite(n) || !Number.isInteger(custom) || custom < 1
      ? 'Type a whole number of minutes, or of hours (1.5 works).'
      : custom > REVISIT_MAX_MINUTES
        ? 'A session can be set aside for a week at most.'
        : null;

  const set = async (minutes: number | null, which: number | 'clear' | 'custom') => {
    if (busy !== null) return;
    setError(null);
    setBusy(which);
    try {
      await api.updateSession(session.id, { revisitMinutes: minutes });
      onClose();
    } catch (e) {
      setError(errorMessage(e));
    } finally {
      setBusy(null);
    }
  };

  return (
    <Dialog
      title={`Revisit ${session.name} later`}
      onClose={onClose}
      footer={
        <>
          {session.revisitAt !== null && (
            <button className="btn" disabled={busy !== null} onClick={() => void set(null, 'clear')}>
              {busy === 'clear' ? <Busy label="Clearing" /> : 'Revisit now'}
            </button>
          )}
          <button className="btn" onClick={onClose}>
            Cancel
          </button>
        </>
      }
    >
      <div className="stack">
        <p className="muted">
          {session.revisitAt !== null
            ? `Set aside until ${revisitWhen(session.revisitAt)}, in ${revisitLeft(session.revisitAt)}. Pick another time to change it, or Revisit now to bring it back.`
            : `While you wait on something, the session shows a clock in place of its status until the time you pick. Talking to it keeps it set aside.`}
        </p>
        <div className="revisit-presets">
          {REVISIT_PRESETS.map((p) => (
            <button key={p.minutes} className="btn" disabled={busy !== null} onClick={() => void set(p.minutes, p.minutes)}>
              {busy === p.minutes ? <Busy label={p.label} /> : p.label}
            </button>
          ))}
        </div>
        <form
          className="revisit-custom"
          onSubmit={(e) => {
            e.preventDefault();
            if (!blocker) void set(custom, 'custom');
          }}
        >
          <Field label="Or in">
            <span className="revisit-custom-row">
              <input
                id="revisit-amount"
                className="input"
                inputMode="decimal"
                autoComplete="off"
                value={amount}
                onChange={(e) => setAmount(e.target.value)}
              />
              <select className="input" value={unit} onChange={(e) => setUnit(e.target.value as 'min' | 'h')} aria-label="Unit">
                <option value="min">minutes</option>
                <option value="h">hours</option>
              </select>
              <button type="submit" className="btn primary" disabled={!!blocker || busy !== null} title={blocker ?? undefined}>
                {busy === 'custom' ? <Busy label="Setting" /> : 'Set'}
              </button>
            </span>
          </Field>
        </form>
        <ErrorText error={error} />
      </div>
    </Dialog>
  );
}
