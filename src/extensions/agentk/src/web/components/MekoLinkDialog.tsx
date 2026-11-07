import type { LocalMekoState } from '../../shared/types';
import { MEKO_HOME_PATH } from '../../shared/mekourls';
import { Dialog } from './common';

/** Why the Meko on this computer can't be used, as a sentence. */
const LOCAL_STATE_TEXT: Record<LocalMekoState, string> = {
  stopped: 'Meko is installed on this computer but not running: start it with meko start.',
  failed: 'Meko on this computer didn’t answer meko token --json (see what it says when you run it).',
  none: 'No Meko was found on this computer: install and start Meko.',
  running: '',
};

/** There's no Meko to use (none running on this computer): what to do. */
export function MekoLinkDialog({ reason, local, onClose }: { reason?: string; local: LocalMekoState; onClose: () => void }) {
  return (
    <Dialog
      title="Set up Meko"
      onClose={onClose}
      footer={
        <a className="btn primary" id="meko-link-open" href={MEKO_HOME_PATH}>
          Open Meko
        </a>
      }
    >
      <div className="stack">
        <p className="muted">
          {reason ?? 'Meko gives projects a datapack: shared memory for their sessions, and what you share a project with.'}
        </p>
        <p className="muted small">{LOCAL_STATE_TEXT[local]} Kode uses the Meko on this computer once it runs.</p>
      </div>
    </Dialog>
  );
}
