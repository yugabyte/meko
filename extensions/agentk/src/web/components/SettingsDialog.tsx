import { useCallback, useEffect, useRef, useState, type KeyboardEvent, type MutableRefObject, type ReactNode } from 'react';
import { WORK_SYNC_MINUTES_MAX, type HarnessStatus, type MekoKeySource, type MekoStatus, type Settings, type SettingsUpdate, type UpdateStatus } from '../../shared/types';
import { HARNESS_CAPS, HARNESS_IDS, type HarnessId } from '../../shared/harnesses';
import { MEKO_ACCOUNT_PATH, MEKO_CLOUD_PATH } from '../../shared/mekourls';
import { api, errorMessage } from '../api';
import { AUTO_SAVE_IDLE, autoSave, settle, type AutoSaveStatus } from '../autosave';
import { saveRailSessions, useRailSessions } from '../rail';
import { installing, mekoUpdates, refreshMekoUpdate, takeRow, updateFailed, useMekoUpdate } from '../mekoupdate';
import type { MekoAuth } from '../signin';
import { harnessLogosToggle } from '../toggle';
import { claudeUpdateText, cx, defaultAgentChoice, httpHost, relativeTime } from '../util';
import { AgentLabel, AgentPicker, Busy, Dialog, ErrorText, Field } from './common';

export type SettingsTab = 'general' | 'harness' | 'work-queue' | 'updates';
type Tab = SettingsTab;

const TABS: Tab[] = ['general', 'harness', 'work-queue', 'updates'];
const TAB_LABEL: Record<Tab, string> = { general: 'General', harness: 'Harness', 'work-queue': 'Work queue', updates: 'Updates' };

export function SettingsDialog({
  mekoAuth,
  update,
  meko,
  harnesses,
  twoAgents,
  serverName,
  initialTab = 'general',
  initialAgent,
  onClose,
  onWhatsNew,
}: {
  /** What Meko, which signs people in to AgentK, says about this browser (null without Meko). */
  mekoAuth: MekoAuth | null;
  update: UpdateStatus;
  meko: MekoStatus;
  /** The coding agents sessions can run on (`AppState.harnesses`). */
  harnesses: HarnessStatus[];
  /** Open sessions run on more than one agent (`harnessLogos()`), so the logos would show. */
  twoAgents: boolean;
  /** `AppState.serverName`, for what to do when Claude Code's update fails. */
  serverName: string;
  /** The tab it opens on (the Claude Code Meko banner's Details opens Harness). */
  initialTab?: SettingsTab;
  /** The agent Harness shows first (Details: Claude Code); else the default agent. */
  initialAgent?: HarnessId;
  onClose: () => void;
  /** Opens What's new from one version to another, in place of Settings: once Settings' saves are in, as a close. */
  onWhatsNew: (from: string, to: string) => void;
}) {
  const [tab, setTab] = useState<Tab>(initialTab);
  // One saver for General and Harness, so a save from a tab that has since closed still shows how it went.
  const auto = useAutoSave();
  // What the General tab holds typed and not saved yet, saved before Settings closes.
  const typed = useRef<(() => void) | null>(null);
  // A close waited for the saves out and one failed: closing again leaves it unsaved.
  const [held, setHeld] = useState(false);
  const closing = useRef(false);
  useEffect(() => {
    if (auto.status.state === 'saving') setHeld(false);
  }, [auto.status.state]);
  // Leaving Settings, closed or for What's new, waits for its saves, so none is left untracked.
  const leave = (then: () => void) => {
    if (held) {
      typed.current = null;
      return then();
    }
    if (closing.current) return;
    closing.current = true;
    typed.current?.();
    void auto.flush().then((ok) => {
      closing.current = false;
      if (ok) then();
      else setHeld(true);
    });
  };
  const close = () => leave(onClose);
  return (
    <Dialog
      title="Settings"
      wide
      onClose={close}
      aside={
        <div className="seg" role="tablist">
          {TABS.map((t) => (
            <button key={t} role="tab" aria-selected={tab === t} className={cx(tab === t && 'on')} onClick={() => setTab(t)}>
              {TAB_LABEL[t]}
            </button>
          ))}
        </div>
      }
    >
      {tab === 'general' && (
        <div className="stack">
          <RailSessionsSetting />
          <GeneralTab auto={auto} typed={typed} />
          <MekoLinks meko={meko} auth={mekoAuth} />
          <UsageStatsCheck />
        </div>
      )}
      {tab === 'harness' && (
        <HarnessTab auto={auto} harnesses={harnesses} twoAgents={twoAgents} serverName={serverName} meko={meko} initialAgent={initialAgent} />
      )}
      {tab === 'work-queue' && <WorkQueueTab auto={auto} />}
      {tab === 'updates' && <UpdatesTab initial={update} onWhatsNew={(from, to) => leave(() => onWhatsNew(from, to))} />}
      <SaveStatus status={auto.status} held={held} />
    </Dialog>
  );
}

/** Saved in this browser as it's ticked, apart from the server settings below. */
function RailSessionsSetting() {
  const on = useRailSessions();
  return (
    <div className="field">
      <label className="check">
        <input id="set-rail-sessions" type="checkbox" checked={on} onChange={(e) => saveRailSessions(e.target.checked)} />
        <span>List sessions under their project in the projects pane</span>
      </label>
      <span className="hint">In place of the tabs above the terminal. Kept in this browser.</span>
    </div>
  );
}

/**
 * Saved in this browser as it's ticked, apart from Settings › Harness's server settings. Offered while a second agent is
 * turned on, or while open sessions run on two agents (the logos' own condition): turning an agent
 * off leaves its open sessions, and their logos, in place.
 */
function HarnessLogosSetting() {
  const on = harnessLogosToggle.use();
  return (
    <div className="field">
      <label className="check">
        <input id="set-harness-logos" type="checkbox" checked={on} onChange={(e) => harnessLogosToggle.save(e.target.checked)} />
        <span>Show each session's agent logo beside its name</span>
      </label>
      <span className="hint">While your open sessions run on more than one agent. The tooltip names the agent either way. Kept in this browser.</span>
    </div>
  );
}

/** A tab's server fields, as `PUT /api/settings` takes them. */
type Patch = Omit<SettingsUpdate, 'meko'>;

/**
 * General's and Harness's server settings, saved as they change (`src/web/autosave.ts`): `change`
 * shows a patch at once and saves it, and a patch that fails to save goes back to the server's last
 * answer. `load` waits for the saves still out, so a tab opened again shows them.
 */
function useAutoSave() {
  const [s, setS] = useState<Settings | null>(null);
  const [status, setStatus] = useState<AutoSaveStatus>(AUTO_SAVE_IDLE);
  // The server's last answer.
  const confirmed = useRef<Settings | null>(null);
  const [saver] = useState(() =>
    autoSave((patch: Patch) => api.saveSettings(patch), {
      onSaved: (next, patch, queued) => {
        confirmed.current = next;
        setS((cur) => cur && settle(cur, next, patch, queued));
      },
      onFailed: (_, patch, queued) => setS((cur) => cur && confirmed.current && settle(cur, confirmed.current, patch, queued)),
      onStatus: setStatus,
    }),
  );
  const load = useCallback(async () => {
    setS(null);
    await saver.flush();
    const x = await api.settings();
    confirmed.current = x;
    setS(x);
    return x;
  }, [saver]);
  const change = (patch: Patch) => {
    setS((cur) => cur && { ...cur, ...patch });
    void saver.save(patch);
  };
  return { s, load, change, status, flush: saver.flush };
}
type AutoSettings = ReturnType<typeof useAutoSave>;

/**
 * Where Save was: Saving while a change is out, then Saved, or why one didn't save (after a close
 * that waited for it, with what closing again does).
 */
function SaveStatus({ status, held }: { status: AutoSaveStatus; held: boolean }) {
  return (
    <>
      <ErrorText error={status.error === null ? null : `${errorMessage(status.error)}${held ? ' Close again to leave it unsaved.' : ''}`} />
      <div className="settings-actions muted small" role="status">
        {status.state === 'saving' ? <Busy label="Saving" /> : status.state === 'saved' ? 'Saved' : null}
      </div>
    </>
  );
}

const clampDepth = (typed: string) => Math.max(1, Math.min(4, Math.round(Number(typed)) || 1));
// A blank or unreadable entry keeps the saved value: a stray 0 would remove every idle worktree.
const clampOr = (typed: string, max: number, saved: number) => {
  const n = Math.round(Number(typed));
  return typed.trim() && Number.isFinite(n) ? Math.max(0, Math.min(max, n)) : saved;
};

/**
 * The work queue's sync: which sources it reads (both off turns sync off, the Sync button too) and how
 * often it runs by itself. The minutes save when the field loses focus or takes Enter.
 */
function WorkQueueTab({ auto }: { auto: AutoSettings }) {
  const { s, load, change } = auto;
  const [minutes, setMinutes] = useState('');
  const [error, setError] = useState<string | null>(null);
  useEffect(() => {
    load().then((x) => setMinutes(String(x.workQueueSyncMinutes)), (e) => setError(errorMessage(e)));
  }, [load]);
  if (!s) return error ? <ErrorText error={error} /> : <p className="muted"><Busy label="Loading" /></p>;
  const off = !s.workQueueGithub && !s.workQueueSlack;
  const saveMinutes = () => {
    const m = clampOr(minutes, WORK_SYNC_MINUTES_MAX, s.workQueueSyncMinutes);
    setMinutes(String(m));
    if (m !== s.workQueueSyncMinutes) change({ workQueueSyncMinutes: m });
  };
  return (
    <div className="stack">
      <p className="muted small">What Sync adds to the work queue. Turn both off to turn sync off.</p>
      <div className="field">
        <label className="check">
          <input id="set-wq-github" type="checkbox" checked={s.workQueueGithub} onChange={(e) => change({ workQueueGithub: e.target.checked })} />
          <span>GitHub review requests</span>
        </label>
        <span className="hint">Open PRs that ask for your review, through your gh login.</span>
      </div>
      <div className="field">
        <label className="check">
          <input id="set-wq-slack" type="checkbox" checked={s.workQueueSlack} onChange={(e) => change({ workQueueSlack: e.target.checked })} />
          <span>Slack Later list</span>
        </label>
        <span className="hint">
          The messages you saved for later, read by Claude Code through your Slack connector. Each read is a model call; on the
          timer it runs only while the last read worked.
        </span>
      </div>
      <Field label="Sync every (minutes)" hint={off ? 'Sync is off.' : '0 syncs only when you press Sync.'}>
        <input
          id="set-wq-minutes"
          className="input"
          type="number"
          min={0}
          max={WORK_SYNC_MINUTES_MAX}
          value={minutes}
          disabled={off}
          onChange={(e) => setMinutes(e.target.value)}
          onBlur={saveMinutes}
          onKeyDown={(e) => {
            if (e.key === 'Enter') {
              e.preventDefault();
              saveMinutes();
            }
          }}
        />
      </Field>
    </div>
  );
}

/** The fields General saves when they lose focus. */
type TypedField = 'reposRoots' | 'reposDepth' | 'idleWorktreesPerRepo' | 'idleWorktreeDays' | 'cloneInto' | 'serverName';

function GeneralTab({ auto, typed }: { auto: AutoSettings; typed: MutableRefObject<(() => void) | null> }) {
  const { s, load, change } = auto;
  // The typed fields as typed: each saves when it loses focus or takes Enter, never mid-typing (a
  // lower idle worktree count removes worktrees as soon as it's saved), numbers clamped first.
  const [roots, setRoots] = useState<string[]>([]);
  const [depth, setDepth] = useState('');
  const [idle, setIdle] = useState('');
  const [idleDays, setIdleDays] = useState('');
  const [cloneInto, setCloneInto] = useState('');
  const [serverName, setServerName] = useState('');
  // The fields above hold what was loaded: until then there's nothing typed to save.
  const [ready, setReady] = useState(false);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    load()
      .then((x) => {
        setReady(true);
        setRoots(x.reposRoots);
        setDepth(String(x.reposDepth));
        setIdle(String(x.idleWorktreesPerRepo));
        setIdleDays(String(x.idleWorktreeDays));
        setCloneInto(x.cloneInto);
        setServerName(x.serverName);
      })
      .catch((e) => setError(errorMessage(e)));
  }, [load]);

  // What the typed fields hold that differs from the settings (an empty repo folder list is
  // included, for the server to say why it can't be; a blank clone folder isn't).
  const typedPatch = (rootList: string[]): Patch => {
    if (!s || !ready) return {};
    const patch: Patch = {};
    const r = rootList.map((x) => x.trim()).filter(Boolean);
    if (r.join('\n') !== s.reposRoots.join('\n')) patch.reposRoots = r;
    const d = clampDepth(depth);
    if (d !== s.reposDepth) patch.reposDepth = d;
    const cap = clampOr(idle, 20, s.idleWorktreesPerRepo);
    if (cap !== s.idleWorktreesPerRepo) patch.idleWorktreesPerRepo = cap;
    const days = clampOr(idleDays, 365, s.idleWorktreeDays);
    if (days !== s.idleWorktreeDays) patch.idleWorktreeDays = days;
    if (cloneInto.trim() && cloneInto.trim() !== s.cloneInto) patch.cloneInto = cloneInto.trim();
    if (serverName.trim() !== s.serverName) patch.serverName = serverName.trim();
    return patch;
  };
  // Saves one field, or every field that differs, each on its own: the server refuses a whole
  // request for one bad field, so a field it refuses doesn't take the others down with it.
  const commit = (only?: TypedField, rootList = roots) => {
    for (const [k, v] of Object.entries(typedPatch(rootList))) {
      if (!only || k === only) change({ [k]: v } as Patch);
    }
  };
  // Switching tabs or closing Settings saves what's still typed: no blur comes when a focused
  // field goes. Settings' close calls it first, to wait for its answer, and takes it away when
  // closing again leaves a refused field unsaved.
  const latest = useRef(commit);
  useEffect(() => {
    latest.current = commit;
  });
  useEffect(() => {
    typed.current = () => latest.current();
    return () => {
      typed.current?.();
      typed.current = null;
    };
  }, [typed]);

  if (!s || !ready) return error ? <ErrorText error={error} /> : <p className="muted"><Busy label="Loading" /></p>;

  // A typed field's handlers: `tidy` shows the value as it's saved (clamped, or the saved one back).
  const saveOn = (field: TypedField, tidy?: () => void) => ({
    onBlur: () => {
      tidy?.();
      commit(field);
    },
    onKeyDown: (e: KeyboardEvent) => {
      if (e.key !== 'Enter') return;
      tidy?.();
      commit(field);
    },
  });

  return (
    <div className="stack">
      <div className="field">
        <span className="label">Repo folders</span>
        <span className="hint">The Open project dialog lists the git repos in these folders.</span>
        {roots.map((r, i) => (
          <div className="filter-row" key={i}>
            <input
              id={`set-root-${i}`}
              className="input mono"
              value={r}
              onChange={(e) => setRoots(roots.map((x, j) => (j === i ? e.target.value : x)))}
              {...saveOn('reposRoots')}
            />
            <button
              type="button"
              className="btn ghost"
              disabled={roots.length <= 1}
              onClick={() => {
                const next = roots.filter((_, j) => j !== i);
                setRoots(next);
                commit('reposRoots', next);
              }}
            >
              Remove
            </button>
          </div>
        ))}
        <button type="button" className="link-btn" onClick={() => setRoots([...roots, ''])}>
          + Add folder
        </button>
      </div>
      <div className="fields">
        <Field label="Scan depth" hint="1 lists only the folders directly inside each repo folder.">
          <input
            id="set-depth"
            className="input"
            type="number"
            min={1}
            max={4}
            value={depth}
            onChange={(e) => setDepth(e.target.value)}
            {...saveOn('reposDepth', () => setDepth(String(clampDepth(depth))))}
          />
        </Field>
        <Field label="Clone into" hint="Where “Clone from URL” puts new repos.">
          <input
            id="set-clone"
            className="input mono"
            value={cloneInto}
            onChange={(e) => setCloneInto(e.target.value)}
            {...saveOn('cloneInto', () => !cloneInto.trim() && setCloneInto(s.cloneInto))}
          />
        </Field>
      </div>
      <div className="field">
        <span className="hint">
          When a session ends, its worktree waits for the next session on the same repo, from any project, with its build
          outputs intact. Each idle worktree keeps those outputs on disk.
        </span>
        <div className="fields">
          <Field label="Idle worktrees per repo" hint="0 removes worktrees when sessions end.">
            <input
              id="set-idle-worktrees"
              className="input"
              type="number"
              min={0}
              max={20}
              value={idle}
              onChange={(e) => setIdle(e.target.value)}
              {...saveOn('idleWorktreesPerRepo', () => setIdle(String(clampOr(idle, 20, s.idleWorktreesPerRepo))))}
            />
          </Field>
          <Field label="Remove unused after (days)" hint="An idle worktree no session has used for this long is removed. 0 keeps it.">
            <input
              id="set-idle-worktree-days"
              className="input"
              type="number"
              min={0}
              max={365}
              value={idleDays}
              onChange={(e) => setIdleDays(e.target.value)}
              {...saveOn('idleWorktreeDays', () => setIdleDays(String(clampOr(idleDays, 365, s.idleWorktreeDays))))}
            />
          </Field>
        </div>
      </div>
      <div className="field">
        <label className="check">
          <input
            id="set-resolve-conflicts"
            type="checkbox"
            checked={s.resolveConflicts}
            onChange={(e) => change({ resolveConflicts: e.target.checked })}
          />
          <span>Resolve merge conflicts on PRs</span>
        </label>
        <span className="hint">
          When a PR in PR &amp; Review conflicts with its base, AgentK merges the base into the branch, resolves the
          conflicts and pushes. Off leaves them to you.
        </span>
      </div>
      <div className="field">
        <label className="check">
          <input
            id="set-tools-enabled"
            type="checkbox"
            checked={s.toolsEnabled}
            onChange={(e) => change({ toolsEnabled: e.target.checked })}
          />
          <span>Let sessions start and move sessions</span>
        </label>
        <span className="hint">
          Sessions get AgentK’s session tools, so when you ask, the agent can open projects, start sessions (from GitHub
          issues and PRs too), move sessions to another stage and open another session’s PR. The agent asks before each
          change, as for its other tools, unless Kiro may run any tool without asking (Harness). Turning this off stops them at once;
          turning it on reaches each session at its next start.
        </span>
      </div>
      <Field label="Server name" hint={`Shown in the rail next to your user name. Leave it empty to use this machine’s name (${s.hostName}).`}>
        <input
          id="set-server-name"
          className="input"
          value={serverName}
          placeholder={s.hostName}
          maxLength={64}
          onChange={(e) => setServerName(e.target.value)}
          {...saveOn('serverName')}
        />
      </Field>
      <p className="hint">
        Meko serves AgentK. Who can reach it, and on which port, are set in Meko’s config.toml, in its{' '}
        <code>[web]</code> table.
      </p>
    </div>
  );
}

/** What Settings says about a harness's CLI: its version, or why it isn't offered. */
function harnessNote(id: HarnessId, h: HarnessStatus | undefined): ReactNode {
  if (!h?.enabled) return 'Off.';
  if (h.version === null) {
    if (!h.checked) return <Busy label="Looking for it" />;
    return `⚠️ ${HARNESS_CAPS[id].label} didn't answer --version here. Install it and run ${HARNESS_CAPS[id].login}, or set [${id}] command in config.toml, then check again.`;
  }
  if (!h.available) {
    const upgrade = HARNESS_CAPS[id].upgrade;
    return `Version ${h.version}; AgentK needs ${h.minVersion} or later. ${upgrade ? `Run ${upgrade}` : 'Update it'}, then check again.`;
  }
  return `Version ${h.version}.`;
}

/** Which `Settings` field turns each agent on: its Offer … for new sessions box. */
const ENABLED_KEY: Record<HarnessId, 'claudeEnabled' | 'opencodeEnabled' | 'kiroEnabled'> = {
  claude: 'claudeEnabled', opencode: 'opencodeEnabled', kiro: 'kiroEnabled',
};

/**
 * The coding agents: the default agent and the logos, then a bar that picks one agent, with that
 * agent's settings under it. Each saves as it's changed.
 */
function HarnessTab({
  auto,
  harnesses,
  twoAgents,
  serverName,
  meko,
  initialAgent,
}: {
  auto: AutoSettings;
  harnesses: HarnessStatus[];
  twoAgents: boolean;
  serverName: string;
  meko: MekoStatus;
  initialAgent?: HarnessId;
}) {
  const { s, load, change } = auto;
  // The agent the bar shows: the one asked for, else the default agent when the tab opens, so a new
  // default doesn't move it.
  const [shown, setShown] = useState<HarnessId>(initialAgent ?? 'claude');
  const [checking, setChecking] = useState(false);
  // The agent whose update this page started, until its request answers (each agent updates on its own).
  const [updating, setUpdating] = useState<HarnessId | null>(null);
  // What Claude Code's update came to, while `[claude] mod` is off and so its banners don't show.
  const [claudeOutcome, setClaudeOutcome] = useState<{ ok: boolean; text: string } | null>(null);
  // What Check again answered, until the next state push replaces it.
  const [rechecked, setRechecked] = useState<HarnessStatus[] | null>(null);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => setRechecked(null), [harnesses]);

  useEffect(() => {
    load()
      .then((x) => {
        if (!initialAgent) setShown(x.defaultHarness);
      })
      .catch((e) => setError(errorMessage(e)));
  }, [load, initialAgent]);

  if (!s) return error ? <ErrorText error={error} /> : <p className="muted"><Busy label="Loading" /></p>;

  const agents = rechecked ?? harnesses;
  const defaultAgent = defaultAgentChoice(agents, s.defaultHarness);
  // Turning off the default agent moves the default to another available one, if there is one.
  const enable = (id: HarnessId, on: boolean) => {
    const patch: Patch = { [ENABLED_KEY[id]]: on };
    const next = on || s.defaultHarness !== id ? s.defaultHarness : (agents.find((h) => h.id !== id && h.available)?.id ?? s.defaultHarness);
    if (next !== s.defaultHarness) patch.defaultHarness = next;
    change(patch);
  };
  // Reads every agent's version again (nothing polls them).
  const checkAgain = () => {
    setChecking(true);
    setError(null);
    api
      .refreshHarnesses()
      .then(setRechecked)
      .catch((err) => setError(errorMessage(err)))
      .finally(() => setChecking(false));
  };
  // A failed `claude update` is an outcome, not an error: it comes back in `lastUpdate`.
  const update = (id: HarnessId) => {
    setUpdating(id);
    setError(null);
    if (id === 'claude') setClaudeOutcome(null);
    const run =
      id === 'claude'
        ? api.updateClaude().then((c) => setClaudeOutcome(c.modEnabled ? null : claudeUpdateText(c, serverName)))
        : api.updateHarness(id);
    run.catch((err) => setError(errorMessage(err))).finally(() => setUpdating(null));
  };

  return (
    <div className="stack">
      {defaultAgent && (
        <div className="field">
          <span className="label" id="set-agent-label">
            Default agent
          </span>
          <AgentPicker
            idPrefix="set-agent"
            agents={defaultAgent.agents}
            value={defaultAgent.value}
            onChange={(id) => change({ defaultHarness: id })}
            labelledBy="set-agent-label"
          />
          <span className="hint">
            New sessions and Ask AgentK run on it; the New session dialog can pick another. A session keeps the agent it
            started on.
          </span>
        </div>
      )}
      {(twoAgents || harnesses.some((h) => h.id !== 'claude' && h.enabled)) && <HarnessLogosSetting />}
      <div className="harness-pick">
        <HarnessBar value={shown} onChange={setShown} />
        <AgentSettings
          id={shown}
          s={s}
          status={agents.find((h) => h.id === shown)}
          checking={checking}
          updating={updating === shown}
          outcome={shown === 'claude' ? claudeOutcome : null}
          meko={meko}
          onEnable={(on) => enable(shown, on)}
          onChange={change}
          onCheck={checkAgain}
          onUpdate={() => update(shown)}
        />
      </div>
      <ErrorText error={error} />
    </div>
  );
}

/** Settings › Harness's bar: every agent, on or off, so one that's off can be turned on. */
export function HarnessBar({ value, onChange }: { value: HarnessId; onChange: (id: HarnessId) => void }) {
  return (
    <div className="seg agents" role="tablist" aria-label="Agent">
      {HARNESS_IDS.map((id) => (
        <button
          key={id}
          id={`set-harness-tab-${id}`}
          type="button"
          role="tab"
          aria-selected={value === id}
          aria-controls="set-harness-panel"
          className={cx(value === id && 'on')}
          onClick={() => onChange(id)}
        >
          <AgentLabel harness={id} />
        </button>
      ))}
    </div>
  );
}

/** One agent's settings, under the bar: whether it's offered, its version, and its own extras. */
export function AgentSettings({
  id,
  s,
  status,
  checking,
  updating,
  outcome = null,
  meko,
  onEnable,
  onChange,
  onCheck,
  onUpdate,
}: {
  id: HarnessId;
  s: Settings;
  status: HarnessStatus | undefined;
  /** Check again is reading the versions. */
  checking: boolean;
  /** This page started this agent's update, and it hasn't answered yet. */
  updating: boolean;
  /** What this page's update of it came to, when nothing else says so (Claude Code with the mod off). */
  outcome?: { ok: boolean; text: string } | null;
  /** Whether this agent's sessions reach AgentK's Meko (`AppState.meko`); left out, no Meko row. */
  meko?: MekoStatus;
  onEnable: (on: boolean) => void;
  onChange: (patch: Omit<SettingsUpdate, 'meko'>) => void;
  onCheck: () => void;
  onUpdate: () => void;
}) {
  const label = HARNESS_CAPS[id].label;
  const enabled = s[ENABLED_KEY[id]];
  return (
    <div className="stack harness-panel" id="set-harness-panel" role="tabpanel" aria-labelledby={`set-harness-tab-${id}`}>
      <div className="field">
        <label className="check">
          <input id={`set-${id}`} type="checkbox" checked={enabled} onChange={(e) => onEnable(e.target.checked)} />
          <span>Offer {label} for new sessions</span>
        </label>
        <span className="hint">{harnessNote(id, status)}</span>
        {status?.enabled && (
          <div className="settings-actions">
            <button type="button" id={`set-${id}-check`} className="btn small" disabled={checking} onClick={onCheck}>
              {checking ? <Busy label="Checking" /> : 'Check again'}
            </button>
            {enabled && status.available && (
              <button type="button" id={`set-${id}-update`} className="btn small" disabled={updating || status.updating} onClick={onUpdate}>
                {updating || status.updating ? <Busy label={`Updating ${label}`} /> : `Update ${label}`}
              </button>
            )}
          </div>
        )}
        {outcome && (
          <span id={`set-${id}-outcome`} className={outcome.ok ? 'ok-text' : 'error-text'} role="status">
            {outcome.text}
          </span>
        )}
      </div>
      {meko && enabled && status?.available && <AgentMeko id={id} meko={meko} />}
      {id === 'opencode' && enabled && (
        <div className="field">
          <label className="check">
            <input
              id="set-opencode-tracking"
              type="checkbox"
              checked={s.opencodeDisableTracking}
              onChange={(e) => onChange({ opencodeDisableTracking: e.target.checked })}
            />
            <span>Turn off OpenCode's tracking</span>
          </label>
          <span className="hint">
            {s.opencodeDisableTracking ? 'OpenCode sessions and Ask AgentK' : 'When on, OpenCode sessions and Ask AgentK'} send no telemetry, skip
            OpenCode's update check and hide OpenCode Zen's free models, whose providers may train on your prompts. Commands the agent runs in a
            session don't export OpenTelemetry either.{' '}
            {s.opencodeDisableTracking &&
              'Your prompts and code still go to the model you pick: for code that must not leave this computer, use a local model such as Ollama or LM Studio. '}
            Takes effect when a session next starts or resumes.
          </span>
        </div>
      )}
      {id === 'kiro' && enabled && (
        <div className="field">
          <label className="check">
            <input id="set-kiro-trust" type="checkbox" checked={s.kiroTrustTools} onChange={(e) => onChange({ kiroTrustTools: e.target.checked })} />
            <span>Let Kiro run any tool without asking</span>
          </label>
          <span className="hint">
            Kiro says nothing to AgentK while it waits for a permission answer, so the session shows as working until you answer in its terminal. On,
            Kiro runs every tool, shell commands included, without asking, in AgentK's sessions only. Takes effect when a session next starts or
            resumes.
          </span>
        </div>
      )}
    </div>
  );
}

/** Where AgentK's Meko key comes from, after "AgentK uses". */
const SOURCE_TEXT: Record<MekoKeySource, string> = {
  meko: 'Meko Cloud, linked in Meko',
  local: 'the Meko on this computer',
  config: 'its own Meko key from config.toml',
  env: 'its own Meko key from MEKO_API_KEY',
  claude: 'its own Meko key from Claude Code’s settings',
};

/**
 * What to do while AgentK uses a key of its own: Meko refused it, or keeps no link yet (too old, or
 * the key is for a server on this computer).
 */
const OWN_KEY_FIX = 'Link Meko Cloud on Meko’s Meko Cloud page to move it there (update Meko first if it has no such page).';

/**
 * Settings › General's ways to Meko's own pages: the Meko Cloud link (Meko keeps it for AgentK and
 * its other extensions) and the account that signs people in to both.
 */
export function MekoLinks({ meko, auth }: { meko: MekoStatus; auth: MekoAuth | null }) {
  const host = httpHost(meko.url ?? '');
  return (
    <div className="field" id="set-meko-links">
      <span className="label">Meko</span>
      <span className="hint">
        {meko.source
          ? `AgentK uses ${SOURCE_TEXT[meko.source]}${host && meko.source !== 'local' ? ` (${host})` : ''}.${
              meko.source !== 'meko' && meko.source !== 'local' ? ` ${OWN_KEY_FIX}` : ''
            }`
          : 'Meko isn’t set up: start Meko on this computer (meko start), or link Meko Cloud.'}{' '}
        {auth?.access === 'local' ? 'Meko opens AgentK on this computer without a sign-in.' : 'Meko signs you in to AgentK.'}
      </span>
      <div className="settings-actions">
        <a className="btn small" id="set-meko-cloud" href={MEKO_CLOUD_PATH}>
          Meko Cloud ↗
        </a>
        <a className="btn small" id="set-meko-account" href={MEKO_ACCOUNT_PATH}>
          Account ↗
        </a>
      </div>
    </div>
  );
}

/**
 * Settings › Harness › an agent: whether its sessions reach AgentK's Meko, worked out by the server
 * (`MekoStatus.claude`, `.opencode`, `.kiro`), with Meko's setup for it when they don't. Nothing
 * while there's no Meko, or nothing to say for that agent.
 */
function AgentMeko({ id, meko }: { id: HarnessId; meko: MekoStatus }) {
  const [busy, setBusy] = useState(false);
  const [done, setDone] = useState(false);
  const [error, setError] = useState<string | null>(null);
  if (!meko.configured) return null;
  const url = meko.url ?? '';
  const label = HARNESS_CAPS[id].label;
  let tag: string;
  let bad = false;
  let hint: string;
  let setUp: (() => Promise<unknown>) | null = null;
  if (id === 'claude') {
    const c = meko.claude;
    const problems = [...c.gaps, ...c.unchecked];
    bad = problems.length > 0;
    tag = c.gaps.length ? 'differs' : c.checking ? 'checking' : c.unchecked.length ? 'not checked' : 'matches';
    hint = problems.length
      ? `${problems.join(' ')} New sessions pick up a fix; running ones keep what they started with.`
      : c.checking
        ? 'Checking Claude Code’s tokens on this Meko.'
        : `Sessions use ${url}, with tokens that work there.`;
    if (problems.length) setUp = api.mekoClaudeSetup;
  } else if (id === 'opencode') {
    if (!meko.opencode) return null;
    tag = meko.opencode.own ? 'its own' : 'matches';
    hint = meko.opencode.own
      ? 'OpenCode sessions use the meko server in OpenCode’s own config, which AgentK leaves as it is.'
      : `OpenCode sessions use ${url}: AgentK adds it to their config.`;
  } else {
    if (!meko.kiro) return null;
    const gaps = meko.kiro.gaps;
    bad = gaps.length > 0;
    tag = bad ? 'differs' : 'matches';
    hint = bad
      ? `${gaps.join(' ')} Meko's setup for Kiro adds its MCP server and its agent, which Kiro sessions are built from.`
      : `Kiro sessions use ${url}.`;
    if (bad) setUp = api.mekoKiroSetup;
  }
  const run = (fn: () => Promise<unknown>) => {
    setBusy(true);
    setError(null);
    setDone(false);
    fn()
      .then(() => setDone(true))
      .catch((e) => setError(errorMessage(e)))
      .finally(() => setBusy(false));
  };
  return (
    <div className="field">
      <span className="label">
        Meko{' '}
        <span id={`set-${id}-meko`} className={cx('tag', bad && 'shared')}>
          {tag}
        </span>
      </span>
      <span className="hint">{hint}</span>
      {setUp && (
        <div className="settings-actions">
          <button type="button" id={`set-${id}-meko-setup`} className="btn small" disabled={busy} onClick={() => run(setUp)}>
            {busy ? <Busy label="Setting up" /> : `Set up ${label} with this Meko`}
          </button>
        </div>
      )}
      {done && !setUp && <span className="ok-text">{label} now uses this Meko.</span>}
      <ErrorText error={error} />
    </div>
  );
}

/** `[call_home] enabled`, saved as soon as it's changed; off and disabled while YB_DISABLE_CALLHOME turns it off. */
function UsageStatsCheck() {
  const [on, setOn] = useState<boolean | null>(null);
  const [offBy, setOffBy] = useState<string | undefined>();
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    api
      .settings()
      .then((x) => {
        setOn(x.callHomeEnabled);
        setOffBy(x.callHomeOffBy);
      })
      .catch((e) => setError(errorMessage(e)));
  }, []);

  const change = async (next: boolean) => {
    setBusy(true);
    setError(null);
    try {
      setOn((await api.saveSettings({ callHomeEnabled: next })).callHomeEnabled);
    } catch (e) {
      setError(errorMessage(e));
    } finally {
      setBusy(false);
    }
  };

  return (
    <div className="stack">
      {on !== null && (
        <label className="check">
          <input
            id="acc-usage-stats"
            type="checkbox"
            checked={on && !offBy}
            disabled={busy || !!offBy}
            onChange={(e) => void change(e.target.checked)}
          />
          <span>Send anonymous usage stats to YugabyteDB</span>
        </label>
      )}
      {offBy && <p className="muted small">Turned off by {offBy} in AgentK's environment.</p>}
      <ErrorText error={error} />
    </div>
  );
}

function UpdatesTab({ initial, onWhatsNew }: { initial: UpdateStatus; onWhatsNew: (from: string, to: string) => void }) {
  // Meko checks for AgentK's updates, installs them and keeps Update automatically (mekoupdate.ts).
  const { row, error: mekoError, loaded } = useMekoUpdate();
  const can = !!row?.updatable;
  const available = can && !!row.update_available && !!row.latest;
  // What the update on offer brings, else what the last update brought (none on an install never updated).
  const whatsNew =
    available && row.latest
      ? { from: initial.current, to: row.latest, label: `What’s in v${row.latest}` }
      : initial.lastUpdatedFrom
        ? { from: initial.lastUpdatedFrom, to: initial.current, label: `What’s new in v${initial.current}` }
        : null;
  const [busy, setBusy] = useState<'check' | 'apply' | 'auto' | null>(null);
  const [error, setError] = useState<string | null>(null);
  const failed = updateFailed(row);

  const run = async (kind: 'check' | 'apply' | 'auto', f: () => Promise<Parameters<typeof takeRow>[0]>) => {
    setBusy(kind);
    setError(null);
    try {
      takeRow(await f());
    } catch (e) {
      setError(errorMessage(e));
      void refreshMekoUpdate();
    } finally {
      setBusy(null);
    }
  };

  return (
    <div className="stack">
      <dl className="kv">
        <dt>Installed</dt>
        <dd className="mono">v{initial.current}</dd>
        <dt>Latest</dt>
        <dd className="mono">{row?.latest ? `v${row.latest}` : '—'}</dd>
        <dt>Last checked</dt>
        <dd>{relativeTime(row?.checked_at ? Date.parse(row.checked_at) : null)}</dd>
      </dl>
      {!loaded ? (
        <p className="muted">
          <Busy label="Asking Meko" />
        </p>
      ) : mekoError ? (
        <p className="muted">{mekoError}</p>
      ) : (
        <label className="check">
          <input
            id="set-auto-update"
            type="checkbox"
            checked={row?.auto_update ?? true}
            disabled={!can || busy === 'auto'}
            onChange={(e) => {
              const on = e.target.checked;
              void run('auto', () => mekoUpdates.setAuto(on));
            }}
          />
          <span>Update automatically (Meko installs new versions when it finds them)</span>
        </label>
      )}
      {row?.waiting && <p className="muted">An automatic update is waiting: {row.waiting}</p>}
      {row?.check_error && <p className="error-text">Last check failed: {row.check_error}</p>}
      {failed && <p className="error-text">The last update failed: {failed}</p>}
      <p className="muted">
        Meko installs AgentK’s updates. They restart only the web server: your Claude sessions keep running, and the page reconnects on
        its own.
      </p>
      <ErrorText error={error} />
      <div className="settings-actions">
        <button className="btn" disabled={!can || !!busy || installing(row)} onClick={() => void run('check', mekoUpdates.check)}>
          {busy === 'check' ? <Busy label="Checking" /> : 'Check now'}
        </button>
        {(available || installing(row)) && (
          <button className="btn primary" disabled={!!busy || installing(row)} onClick={() => void run('apply', mekoUpdates.update)}>
            {busy === 'apply' || installing(row) ? <Busy label="Updating" /> : 'Update to the latest version'}
          </button>
        )}
        {whatsNew && (
          <button className="link-btn" onClick={() => onWhatsNew(whatsNew.from, whatsNew.to)}>
            {whatsNew.label}
          </button>
        )}
      </div>
    </div>
  );
}
