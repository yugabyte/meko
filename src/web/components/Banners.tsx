import { useState } from 'react';
import type { AppState, DatapackInvite, FailedStart } from '../../shared/types';
import { api, errorMessage } from '../api';
import type { Conn } from '../hooks/useEvents';
import { Busy } from './common';

const DISMISS_KEY = 'ak-dismissed-updated-from';

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
 * The Claude Code sessions launch with, when it's too old for AgentK's mod: update it here (its own
 * `claude update`), or dismiss the banner for that version. Then the update's outcome.
 */
function ClaudeCodeBanners({ state, onError }: { state: AppState; onError: (m: string) => void }) {
  const c = state.claudeCode;
  const [oldDismissed, setOldDismissed] = useState(() => readDismissed(CLAUDE_OLD_KEY));
  const [updateDismissed, setUpdateDismissed] = useState(() => readDismissed(CLAUDE_UPDATE_KEY));
  const [starting, setStarting] = useState(false);
  if (!c?.modEnabled) return null;
  const last = c.lastUpdate;
  const lastKey = last ? String(last.at) : null;
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
          <span className="banner-text">
            {!last.ok
              ? `Couldn’t update Claude Code${last.message ? `: ${last.message}` : ''}. Run claude update on ${state.serverName}, or update it the way it was installed.`
              : last.from && last.to && last.from !== last.to
                ? `Claude Code was updated to ${last.to} (from ${last.from}). Running sessions keep their version until Claude restarts in them.`
                : c.supportsMod === false
                  ? `claude update found nothing newer than ${last.to ?? 'this version'}. Its release channel, or your organization’s settings, may be holding it back.`
                  : `Claude Code is up to date${last.to ? ` (${last.to})` : ''}.`}
          </span>
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
            Claude Code {c.version} on {state.serverName} is older than {c.minModVersion}, the first version AgentK’s mod
            runs in, so AgentK types its prompts into the terminal.
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
 * Claude Code's sessions don't reach AgentK's Meko (another server, or a token that doesn't work
 * there). AgentK never fixes it on its own: the user sets Claude Code up from here or the Meko tab.
 */
function MekoClaudeBanner({ state, onDetails, onError }: { state: AppState; onDetails: () => void; onError: (m: string) => void }) {
  const [busy, setBusy] = useState(false);
  const gaps = state.meko.claude?.gaps ?? [];
  if (!state.meko.configured || !gaps.length) return null;
  const setUp = async () => {
    setBusy(true);
    try {
      await api.mekoClaudeSetup();
    } catch (e) {
      onError(errorMessage(e));
    } finally {
      setBusy(false);
    }
  };
  return (
    <div className="banner warn" role="alert">
      <span className="banner-text">
        Claude Code sessions may not reach AgentK’s Meko datapacks. {gaps.join(' ')} Setting it up changes Claude Code’s own
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

export function Banners({
  state,
  conn,
  stale,
  onError,
  onOpenInvite,
  onRetryStart,
  onMekoSettings,
}: {
  state: AppState;
  conn: Conn;
  /** The server's version while this page still runs an older one (see useReload), else null. */
  stale: string | null;
  onError: (m: string) => void;
  onOpenInvite: (i: DatapackInvite) => void;
  onRetryStart: (f: FailedStart) => void;
  /** Opens the Meko tab in Settings. */
  onMekoSettings: () => void;
}) {
  const [dismissed, setDismissed] = useState<string | null>(readDismissed);
  const [applying, setApplying] = useState(false);
  const u = state.update;
  const updatedKey = u.justUpdatedFrom ? `${u.justUpdatedFrom}->${u.current}` : null;

  return (
    <div className="banners">
      {state.sessionHost?.outdated && (
        <div className="banner warn" role="alert">
          <span className="banner-text">
            The session host on {state.serverName} runs version {state.sessionHost.version}, too old for this version of AgentK
            Local, so sessions can’t start. Run agent-k-local host restart there: running sessions restart and resume.
          </span>
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
          <span className="banner-text">AgentK Local was updated to v{stale}. Reload to use it.</span>
          <button className="btn primary small" onClick={() => location.reload()}>
            Reload
          </button>
        </div>
      )}
      {/* After an update this shows once the page has reloaded, not beside the Reload banner. */}
      {!stale && updatedKey && dismissed !== updatedKey && (
        <div className="banner info">
          Updated to v{u.current} (from v{u.justUpdatedFrom}).
          {u.releaseUrl && (
            <>
              {' '}
              <a href={u.releaseUrl} target="_blank" rel="noreferrer">
                What’s new
              </a>
            </>
          )}
          <button
            className="link-btn"
            onClick={() => {
              saveDismissed(DISMISS_KEY, updatedKey);
              setDismissed(updatedKey);
            }}
          >
            Dismiss
          </button>
        </div>
      )}
      {u.available && u.latest && !u.applying && (
        <div className="banner info">
          Version {u.latest} is available. Updating restarts the web server only; your sessions keep running.
          <button
            className="btn primary small"
            disabled={applying}
            onClick={async () => {
              setApplying(true);
              try {
                await api.applyUpdate();
              } catch (e) {
                onError(errorMessage(e));
              } finally {
                setApplying(false);
              }
            }}
          >
            {applying ? <Busy label="Updating" /> : `Update to v${u.latest}`}
          </button>
        </div>
      )}
      {u.applying && (
        <div className="banner info">
          <span>
            <Busy label="Installing the update" /> The page reloads on its own when it’s done.
          </span>
        </div>
      )}
      <ClaudeCodeBanners state={state} onError={onError} />
      <MekoClaudeBanner state={state} onDetails={onMekoSettings} onError={onError} />
      <FailedStartBanners state={state} onRetry={onRetryStart} onError={onError} />
      <InviteBanners invites={state.invites ?? []} onOpen={onOpenInvite} onError={onError} />
    </div>
  );
}
