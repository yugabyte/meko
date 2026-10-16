import { mekoAsk, mekoPlan } from '../knowledge';
import type { SessionTerminal } from '../terminals';
import { keepFocus } from '../util';
import { Busy } from './common';

/**
 * Upload to Meko, on a terminal's strip of held files: shown while at least one of them is a type
 * the knowledge base takes. It asks first, in the strip (what goes, to which datapack, who can find
 * it, and what it skips), then sends them; each one sent leaves the strip. The state lives on the
 * terminal (`meko`, `uploadToMeko()`), which outlasts this pane.
 */
export function MekoUpload({ term, datapack }: { term: SessionTerminal; datapack: string }) {
  const plan = mekoPlan(term.held.list);
  const { asking, sending } = term.meko;
  // While held paths are being pasted to Claude, the files are theirs: Upload waits.
  const busy = term.sending;
  if (sending.size) {
    return (
      <span className="held-meko" role="status">
        <Busy label={`Uploading ${sending.size === 1 ? '1 file' : `${sending.size} files`} to Meko`} />
      </span>
    );
  }
  if (!plan.upload.length) return null;
  if (!asking) {
    return (
      <button type="button" className="btn small held-meko" disabled={busy} onPointerDown={keepFocus} onMouseDown={keepFocus} onClick={() => term.askMeko(true)}>
        Upload to Meko
      </button>
    );
  }
  return (
    <div className="held-meko-ask" role="group" aria-label="Upload to Meko">
      <span>{mekoAsk(plan, datapack)}</span>
      <span className="held-meko-buttons">
        <button type="button" className="btn small" onPointerDown={keepFocus} onMouseDown={keepFocus} onClick={() => term.askMeko(false)}>
          Cancel
        </button>
        <button type="button" className="btn small primary" disabled={busy} onPointerDown={keepFocus} onMouseDown={keepFocus} onClick={() => void term.uploadToMeko()}>
          Upload
        </button>
      </span>
    </div>
  );
}
