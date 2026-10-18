import { useState } from 'react';
import { MEKO_TEACH_MAX, type MekoGap } from '../../shared/types';
import { api, errorMessage } from '../api';
import { GAP_QUALITY_LABEL, askedText } from '../util';
import { Busy, Dialog, ErrorText, Field } from './common';

/**
 * Teach Meko: what Meko should know about a question it couldn't answer, saved as a memory in the
 * project's dedicated datapack (`datapack`). A failed save keeps the text and says why.
 */
export function TeachMekoDialog({
  projectId,
  gap,
  datapack,
  onClose,
  onTaught,
}: {
  projectId: string;
  gap: MekoGap;
  datapack: string;
  onClose: () => void;
  onTaught: (gap: MekoGap) => void;
}) {
  const [text, setText] = useState('');
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  const trimmed = text.trim();
  const blocker = !trimmed ? 'Write what Meko should know first.' : trimmed.length > MEKO_TEACH_MAX ? `Keep it to ${MEKO_TEACH_MAX.toLocaleString()} characters.` : null;

  const save = async () => {
    if (blocker || busy) return;
    setError(null);
    setBusy(true);
    try {
      onTaught(await api.teachMeko(projectId, { key: gap.key, text: trimmed }));
    } catch (e) {
      setError(errorMessage(e));
      setBusy(false);
    }
  };

  return (
    <Dialog
      title="Teach Meko"
      onClose={onClose}
      footer={
        <>
          <button className="btn" onClick={onClose}>
            Cancel
          </button>
          <button className="btn primary" disabled={!!blocker || busy} title={blocker ?? undefined} onClick={() => void save()}>
            {busy ? <Busy label="Saving" /> : 'Save to Meko'}
          </button>
        </>
      }
    >
      <div className="stack">
        <div className="tm-question">
          <span className="muted small">
            {askedText(gap.asks)}, {GAP_QUALITY_LABEL[gap.quality].toLowerCase()}:
          </span>
          <blockquote className="tm-query">{gap.query}</blockquote>
        </div>
        <Field
          label="What should Meko know?"
          hint={`Saved as a memory in ${datapack}, which the project's team shares. Write it so it makes sense on its own.`}
        >
          <textarea
            id="teach-meko-text"
            className="input tm-text"
            rows={6}
            value={text}
            maxLength={MEKO_TEACH_MAX}
            onChange={(e) => setText(e.target.value)}
          />
        </Field>
        <span className="muted small tm-count">
          {trimmed.length.toLocaleString()} / {MEKO_TEACH_MAX.toLocaleString()}
        </span>
        <ErrorText error={error} />
      </div>
    </Dialog>
  );
}
