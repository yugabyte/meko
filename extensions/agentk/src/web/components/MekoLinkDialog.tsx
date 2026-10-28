import type { LocalMekoState, MekoKeySource } from '../../shared/types';
import { MEKO_CLOUD_PATH, MEKO_CLOUD_SIGNUP_URL } from '../../shared/mekourls';
import { Dialog } from './common';

/** Why the Meko on this computer can't be used, as a sentence that the way to Meko Cloud follows. */
const LOCAL_STATE_TEXT: Record<LocalMekoState, string> = {
  stopped: 'Meko is installed on this computer but not running: start it with meko start.',
  failed: 'Meko on this computer didn’t answer meko token --json (see what it says when you run it).',
  none: 'No Meko was found on this computer: install and start Meko.',
  running: '',
};

/**
 * There's no Meko to use (no Meko Cloud link, and no Meko running on this computer): what to do.
 * The Meko Cloud link is Meko's, set on its own page, so this only points there.
 *
 * With `sharing` (the Meko AgentK uses, `MekoStatus.source`), Share was asked for while AgentK can't
 * share (`MekoStatus.sharing` false). On the Meko on this computer, which has no share routes, it
 * points to Meko Cloud, as Meko's own Invite does: Sign up there, or link it. Any other Meko can
 * share once Meko knows its web app URL, so it points to Meko's settings to set it.
 */
export function MekoLinkDialog({
  reason,
  local,
  sharing,
  onClose,
}: {
  reason?: string;
  local: LocalMekoState;
  sharing?: MekoKeySource | null;
  onClose: () => void;
}) {
  if (sharing === 'local') {
    return (
      <Dialog
        title="Share through Meko Cloud"
        onClose={onClose}
        footer={
          <>
            <a className="btn" id="meko-link-open" href={MEKO_CLOUD_PATH}>
              Link Meko Cloud
            </a>
            <a className="btn primary" id="meko-signup" href={MEKO_CLOUD_SIGNUP_URL} target="_blank" rel="noopener noreferrer">
              Sign up for Meko Cloud ↗
            </a>
          </>
        }
      >
        <div className="stack">
          <p className="muted">
            Sharing lives in Meko Cloud. AgentK uses the Meko on this computer, which keeps datapacks here, and only Meko
            Cloud shares them with your team.
          </p>
          <p className="muted small">
            Sign up, make an API key in Meko Cloud’s Settings, then paste it under Link Meko Cloud. AgentK then uses Meko
            Cloud, where you give the project a datapack to share.
          </p>
        </div>
      </Dialog>
    );
  }
  if (sharing) {
    return (
      <Dialog
        title="Set Meko’s web app URL"
        onClose={onClose}
        footer={
          <a className="btn primary" id="meko-link-open" href={MEKO_CLOUD_PATH}>
            Open Meko settings
          </a>
        }
      >
        <p className="muted">
          AgentK shares a project through Meko’s API, and the web app URL of the Meko it’s linked to isn’t known. Set it in
          Meko’s settings (Settings › Meko Cloud).
        </p>
      </Dialog>
    );
  }
  return (
    <Dialog
      title="Set up Meko"
      onClose={onClose}
      footer={
        <a className="btn primary" id="meko-link-open" href={MEKO_CLOUD_PATH}>
          Link Meko Cloud ↗
        </a>
      }
    >
      <div className="stack">
        <p className="muted">
          {reason ?? 'Meko gives projects a datapack: shared memory for their sessions, and what you share a project with.'}
        </p>
        <p className="muted small">
          {LOCAL_STATE_TEXT[local]} AgentK uses the Meko on this computer while it runs, or Meko Cloud once it’s linked in
          Meko’s settings.
        </p>
      </div>
    </Dialog>
  );
}
