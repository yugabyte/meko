import { useEffect, useState } from 'react';
import type { AppState, DatapackInvite, FailedStart } from '../../shared/types';
import { notConnectedText, onlyMekoBlocks } from '../../shared/harnesses';
import { MEKO_INSTALL_COMMAND } from '../../shared/mekourls';
import { api, errorMessage } from '../api';
import { copyText } from '../clipboard';
import type { Conn } from '../hooks/useEvents';
import { useInstall } from '../installapp';
import { setUpWithMeko } from '../mekoagents';
import { installing, mekoUpdates, refreshMekoUpdate, takeRow, updateFailed, useMekoUpdate } from '../mekoupdate';
import { claudeUpdateText, noAgent } from '../util';
import { Busy } from './common';

const DISMISS_KEY = 'ak-dismissed-updated-from';
/** The failed update whose banner was dismissed: its job's start. */
const FAILED_KEY = 'ak-dismissed-update-failed';

/** How long the "Updated to" banner shows, counting only while the page is in view, before it dismisses itself. */
export const UPDATED_BANNER_MS = 2 * 60_000;

/**
 * Calls `hide` once the page has been in view for `ms` in all: the clock runs only while it's shown,
 * so a banner in a background tab still gets seen. Returns the cleanup.
 */
export function hideWhenSeen(
  page: Pick<Document, 'visibilityState' | 'addEventListener' | 'removeEventListener'>,
  ms: number,
  hide: () => void,
): () => void {
  let left = ms;
  let since = 0;
  let timer: ReturnType<typeof setTimeout> | undefined;
  const tick = () => {
    if (page.visibilityState === 'visible') {
      if (timer !== undefined) return;
      since = Date.now();
      timer = setTimeout(hide, left);
    } else if (timer !== undefined) {
      clearTimeout(timer);
      timer = undefined;
      left -= Date.now() - since;
    }
  };
  page.addEventListener('visibilitychange', tick);
  tick();
  return () => {
    page.removeEventListener('visibilitychange', tick);
    clearTimeout(timer);
  };
}

function readDismissed(key = DISMISS_KEY): string | null {
  try {
    return localStorage.getItem(key);
  } catch {
    return null;
  }
}

function saveDismissed(key: string, value: string): void {
  try {
    localStorage.setItem(key, value);
  } catch {
    /* ignore */
  }
}

/** The Claude Code version whose too-old banner was dismissed, and the update outcome dismissed (its time). */
const CLAUDE_OLD_KEY = 'ak-dismissed-claude-old';
const CLAUDE_UPDATE_KEY = 'ak-dismissed-claude-update';

/**
 * The Claude Code sessions launch with, when it's too old for Kode's mod: update it here (its own
 * `claude update`), or dismiss the banner for that version. Then the update's outcome.
 */
function ClaudeCodeBanners({ state, onError }: { state: AppState; onError: (m: string) => void }) {
  const c = state.claudeCode;
  const [oldDismissed, setOldDismissed] = useState(() => readDismissed(CLAUDE_OLD_KEY));
  const [updateDismissed, setUpdateDismissed] = useState(() => readDismissed(CLAUDE_UPDATE_KEY));
  const [starting, setStarting] = useState(false);
  if (!c?.modEnabled) return null;
  const last = claudeUpdateText(c, state.serverName);
  const lastKey = c.lastUpdate ? String(c.lastUpdate.at) : null;
  const old = c.supportsMod === false && !!c.version && oldDismissed !== c.version;
  const update = async () => {
    setStarting(true);
    try {
      await api.updateClaude();
    } catch (e) {
      onError(errorMessage(e));
    } finally {
      setStarting(false);
    }
  };
  return (
    <>
      {c.updating && (
        <div className="banner info">
          <span>
            <Busy label="Updating Claude Code" /> Running sessions aren’t affected.
          </span>
        </div>
      )}
      {!c.updating && last && lastKey !== updateDismissed && (
        <div className={last.ok ? 'banner info' : 'banner warn'} role={last.ok ? undefined : 'alert'}>
          <span className="banner-text">{last.text}</span>
          <button
            className="link-btn"
            onClick={() => {
              saveDismissed(CLAUDE_UPDATE_KEY, lastKey!);
              setUpdateDismissed(lastKey);
            }}
          >
            Dismiss
          </button>
        </div>
      )}
      {!c.updating && old && (
        <div className="banner warn">
          <span className="banner-text">
            Claude Code {c.version} on {state.serverName} is older than {c.minModVersion}, the first version Kode’s mod
            runs in, so Kode types its prompts into the terminal.
          </span>
          <span className="banner-btns">
            <button className="btn primary small" disabled={starting} onClick={update}>
              {starting ? <Busy label="Updating" /> : 'Update Claude Code'}
            </button>
            <button
              className="btn small"
              onClick={() => {
                saveDismissed(CLAUDE_OLD_KEY, c.version!);
                setOldDismissed(c.version);
              }}
            >
              Dismiss
            </button>
          </span>
        </div>
      )}
    </>
  );
}

/** Past this many, invite banners collapse into one line with Show. */
const MAX_INVITE_BANNERS = 3;

/** Project datapacks shared with us: open as a project, decline, or put off. */
function InviteBanners({
  invites,
  onOpen,
  onError,
}: {
  invites: DatapackInvite[];
  onOpen: (i: DatapackInvite) => void;
  onError: (m: string) => void;
}) {
  const [expanded, setExpanded] = useState(false);
  const [busy, setBusy] = useState<string | null>(null);
  if (!invites.length) return null;
  if (invites.length > MAX_INVITE_BANNERS && !expanded) {
    return (
      <div className="banner info">
        <span className="banner-text">{invites.length} project datapacks were shared with you.</span>
        <button className="btn small" onClick={() => setExpanded(true)}>
          Show
        </button>
      </div>
    );
  }
  const act = async (i: DatapackInvite, fn: (id: string) => Promise<void>) => {
    setBusy(i.datapackId);
    try {
      await fn(i.datapackId);
    } catch (e) {
      onError(errorMessage(e));
    } finally {
      setBusy(null);
    }
  };
  return (
    <>
      {invites.map((i) => {
        const who = i.ownerName ?? i.ownerEmail ?? 'Someone';
        const as = i.grant ? ` as ${i.grant}` : '';
        const viewer = i.grant === 'viewer';
        const invited = !!i.inviteId;
        const disabled = busy === i.datapackId;
        return (
          <div className="banner info invite-banner" key={i.datapackId}>
            <span className="banner-text">
              <b>{who}</b> {invited ? 'invited you to ' : 'shared '}
              <span className="mono">{i.name}</span>
              {invited ? as : ` with you${as}`}.
              {viewer &&
                (invited
                  ? ' It’s read only, so it can’t be a project’s own datapack.'
                  : ' It’s read only, so attach it from a project’s home.')}
            </span>
            <span className="banner-btns">
              {!viewer && (
                <button className="btn primary small" disabled={disabled} onClick={() => onOpen(i)}>
                  Open as project
                </button>
              )}
              {viewer && invited && (
                <button className="btn primary small" disabled={disabled} onClick={() => void act(i, api.acceptInvite)}>
                  Accept
                </button>
              )}
              {invited && (
                <button className="btn small" disabled={disabled} onClick={() => void act(i, api.declineInvite)}>
                  Decline
                </button>
              )}
              <button className="btn small" disabled={disabled} onClick={() => void act(i, api.laterInvite)}>
                {viewer && !invited ? 'OK' : 'Later'}
              </button>
            </span>
          </div>
        );
      })}
    </>
  );
}

/**
 * The offer to install Kode as an app (`installapp.ts`), until it's installed or turned down in this
 * browser. On iOS only Not now hides it: Safari can't tell that Kode is on the Home Screen already.
 */
function InstallBanner() {
  const { offer, install, dismiss } = useInstall();
  if (!offer) return null;
  return (
    <div className="banner info">
      <span className="banner-text">
        {offer === 'prompt'
          ? 'Install Kode as an app: it opens in its own window, with its own icon in your dock or on your home screen.'
          : 'Add Kode to your Home Screen: tap Share, then Add to Home Screen. It opens full screen there, and can show notifications.'}
      </span>
      <span className="banner-btns">
        {offer === 'prompt' && (
          <button className="btn primary small" onClick={() => void install()}>
            Install
          </button>
        )}
        <button className="btn small" onClick={dismiss}>
          Not now
        </button>
      </span>
    </div>
  );
}

/** New sessions whose worktree couldn't be made after the dialog closed: try again (the dialog filled in) or dismiss. */
function FailedStartBanners({
  state,
  onRetry,
  onError,
}: {
  state: AppState;
  onRetry: (f: FailedStart) => void;
  onError: (m: string) => void;
}) {
  const projects = new Map(state.projects.map((p) => [p.id, p.name]));
  const failed = (state.failedStarts ?? []).filter((f) => projects.has(f.projectId));
  return (
    <>
      {failed.map((f) => (
        <div className="banner warn" key={f.id} role="alert">
          <span className="banner-text">
            <b>{f.name}</b> in {projects.get(f.projectId)} didn’t start. {f.error}
          </span>
          <span className="banner-btns">
            <button className="btn primary small" onClick={() => onRetry(f)}>
              Try again
            </button>
            <button className="btn small" onClick={() => api.dismissFailedStart(f.id).catch((e) => onError(errorMessage(e)))}>
              Dismiss
            </button>
          </span>
        </div>
      ))}
    </>
  );
}

/**
 * Claude Code's sessions don't reach Kode's Meko (its meko MCP server isn't connected, as Meko
 * reports it, or its settings name another server or a token that doesn't work there). Kode never
 * fixes it on its own: the user sets Claude Code up from here or Settings › Harness › Claude Code,
 * which has Meko connect it first (`setUpWithMeko`).
 */
function MekoClaudeBanner({ state, onDetails, onError }: { state: AppState; onDetails: () => void; onError: (m: string) => void }) {
  const [busy, setBusy] = useState(false);
  const gaps = state.meko.claude?.gaps ?? [];
  if (!state.meko.configured || !gaps.length) return null;
  const setUp = async () => {
    setBusy(true);
    try {
      await setUpWithMeko('claude', state.harnesses.find((h) => h.id === 'claude')?.meko);
    } catch (e) {
      onError(errorMessage(e));
    } finally {
      setBusy(false);
    }
  };
  return (
    <div className="banner warn" role="alert">
      <span className="banner-text">
        Claude Code sessions may not reach Kode’s Meko datapacks. {gaps.join(' ')} Setting it up changes Claude Code’s own
        settings; new sessions pick it up, and running ones keep what they started with.
      </span>
      <span className="banner-btns">
        <button className="btn primary small" disabled={busy} onClick={() => void setUp()}>
          {busy ? <Busy label="Setting up" /> : 'Set up Claude Code with this Meko'}
        </button>
        <button className="btn small" disabled={busy} onClick={onDetails}>
          Details
        </button>
      </span>
    </div>
  );
}

/**
 * No Meko on the server's computer (`MekoStatus.local` is `none`): how to install it. It can't be
 * dismissed, and goes once the server finds `meko` (it looks every minute while there's none).
 */
export function MekoInstallBanner({ state }: { state: AppState }) {
  const [copied, setCopied] = useState(false);
  if (state.meko.local !== 'none') return null;
  return (
    <div className="banner warn" role="alert">
      <span className="banner-text">
        Meko isn’t installed on {state.serverName}. Kode keeps each project’s memory in Meko. Install it there:{' '}
        <code className="mono">{MEKO_INSTALL_COMMAND}</code>. This goes away once Kode finds it.
      </span>
      <button className="btn small" onClick={() => void copyText(MEKO_INSTALL_COMMAND).then(setCopied)}>
        {copied ? 'Copied' : 'Copy'}
      </button>
    </div>
  );
}

export function Banners({
  state,
  conn,
  stale,
  onError,
  onOpenInvite,
  onRetryStart,
  onMekoSettings,
  onSettings,
  onWhatsNew,
}: {
  state: AppState;
  conn: Conn;
  /** The server's version while this page still runs an older one (see useReload), else null. */
  stale: string | null;
  onError: (m: string) => void;
  onOpenInvite: (i: DatapackInvite) => void;
  onRetryStart: (f: FailedStart) => void;
  /** Opens Settings › Harness on Claude Code, where its Meko row is. */
  onMekoSettings: () => void;
  /** Opens Settings (its General tab, where the agents are turned on). */
  onSettings: () => void;
  /** Opens What's new from one version to another. */
  onWhatsNew: (from: string, to: string) => void;
}) {
  const [dismissed, setDismissed] = useState<string | null>(readDismissed);
  const [applying, setApplying] = useState(false);
  const u = state.update;
  // Meko installs Kode's updates: what's available, and its job, are Meko's (mekoupdate.ts).
  const meko = useMekoUpdate().row;
  const offer = meko?.updatable && meko.update_available && meko.latest ? meko.latest : null;
  const failed = updateFailed(meko);
  const failedKey = failed && meko?.job ? meko.job.started_at : null;
  const [dismissedFailure, setDismissedFailure] = useState<string | null>(() => readDismissed(FAILED_KEY));
  const updatedKey = u.justUpdatedFrom ? `${u.justUpdatedFrom}->${u.current}` : null;
  const shownUpdate = !stale && updatedKey && dismissed !== updatedKey ? updatedKey : null;
  // The "Updated to" banner dismisses itself once it has been seen for a while, as Dismiss does.
  useEffect(() => {
    if (!shownUpdate) return;
    return hideWhenSeen(document, UPDATED_BANNER_MS, () => {
      saveDismissed(DISMISS_KEY, shownUpdate);
      setDismissed(shownUpdate);
    });
  }, [shownUpdate]);

  return (
    <div className="banners">
      {state.sessionHost?.outdated && (
        <div className="banner warn" role="alert">
          <span className="banner-text">
            The session host on {state.serverName} runs version {state.sessionHost.version}, too old for this version of
            Kode, so sessions can’t start. Run meko kode host restart there: running sessions restart and resume.
          </span>
        </div>
      )}
      {noAgent(state.harnesses ?? []) && (
        <div className="banner warn" role="alert">
          <span className="banner-text">
            No coding agent is available on {state.serverName}, so no session can start. Install Claude Code or Kiro CLI there, or turn
            one on in Settings.
            {(state.harnesses ?? []).filter(onlyMekoBlocks).map((h) => ` ${notConnectedText(h.id)}.`)}
          </span>
          <button className="btn primary small" onClick={onSettings}>
            Settings
          </button>
        </div>
      )}
      {conn === 'closed' && (
        <div className="banner warn">
          <span>
            Lost the connection to the server. <Busy label="Reconnecting" /> Your sessions keep running.
          </span>
        </div>
      )}
      {stale && (
        <div className="banner info">
          <span className="banner-text">Kode was updated to v{stale}. Reload to use it.</span>
          <button className="btn primary small" onClick={() => location.reload()}>
            Reload
          </button>
        </div>
      )}
      {/* After an update this shows once the page has reloaded, not beside the Reload banner. */}
      {shownUpdate && (
        <div className="banner info">
          Updated to v{u.current} (from v{u.justUpdatedFrom}).
          <button className="link-btn" onClick={() => u.justUpdatedFrom && onWhatsNew(u.justUpdatedFrom, u.current)}>
            What’s new
          </button>
          <button
            className="link-btn"
            onClick={() => {
              saveDismissed(DISMISS_KEY, shownUpdate);
              setDismissed(shownUpdate);
            }}
          >
            Dismiss
          </button>
        </div>
      )}
      {offer && !installing(meko) && (
        <div className="banner info">
          Version {offer} is available. Updating restarts the web server only; your sessions keep running.
          <button
            className="btn primary small"
            disabled={applying}
            onClick={async () => {
              setApplying(true);
              try {
                takeRow(await mekoUpdates.update());
              } catch (e) {
                onError(errorMessage(e));
                void refreshMekoUpdate();
              } finally {
                setApplying(false);
              }
            }}
          >
            {applying ? <Busy label="Updating" /> : 'Update to the latest version'}
          </button>
          <button className="btn small" onClick={() => onWhatsNew(u.current, offer)}>
            What’s new
          </button>
        </div>
      )}
      {installing(meko) && (
        <div className="banner info">
          <span>
            <Busy label="Installing the update" /> The page reloads on its own when it’s done.
          </span>
        </div>
      )}
      {failed && failedKey && dismissedFailure !== failedKey && (
        <div className="banner warn">
          <span className="banner-text">Updating Kode failed: {failed}</span>
          <button
            className="link-btn"
            onClick={() => {
              saveDismissed(FAILED_KEY, failedKey);
              setDismissedFailure(failedKey);
            }}
          >
            Dismiss
          </button>
        </div>
      )}
      <MekoInstallBanner state={state} />
      <ClaudeCodeBanners state={state} onError={onError} />
      <MekoClaudeBanner state={state} onDetails={onMekoSettings} onError={onError} />
      <FailedStartBanners state={state} onRetry={onRetryStart} onError={onError} />
      <InviteBanners invites={state.invites ?? []} onOpen={onOpenInvite} onError={onError} />
      <InstallBanner />
    </div>
  );
}
