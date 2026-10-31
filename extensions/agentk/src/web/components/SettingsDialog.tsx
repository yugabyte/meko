import { useCallback, useEffect, useRef, useState, type KeyboardEvent, type MutableRefObject, type ReactNode } from 'react';
import {
  WORK_SYNC_MINUTES_MAX, type HarnessMekoState, type HarnessStatus, type MekoKeySource, type MekoStatus, type Settings, type SettingsUpdate, type UpdateStatus,
} from '../../shared/types';
import { HARNESS_CAPS, HARNESS_IDS, onlyMekoBlocks, type HarnessId } from '../../shared/harnesses';
import { MEKO_ACCOUNT_PATH, MEKO_CLOUD_PATH } from '../../shared/mekourls';
import { api, errorMessage } from '../api';
import { AUTO_SAVE_IDLE, autoSave, settle, type AutoSaveStatus } from '../autosave';
import { saveRailSessions, useRailSessions } from '../rail';
import { UPDATE_MEKO_AGENTS, connectToMeko, connectable, setUpWithMeko } from '../mekoagents';
import { installing, mekoUpdates, refreshMekoUpdate, takeRow, updateFailed, useMekoUpdate } from '../mekoupdate';
import type { MekoAuth } from '../signin';
import { setTermThemePref, termThemePref, TERM_THEME_LABEL, type TermThemePref } from '../termtheme';
import { harnessLogosToggle } from '../toggle';
import { claudeUpdateText, cx, defaultAgentChoice, httpHost, relativeTime } from '../util';
import { AgentLabel, AgentPicker, Busy, Dialog, ErrorText, Field, InfoButton, InfoCheck, InfoPanel, ReviewFields } from './common';
import { setReviewDefaults } from '../reviewdefaults';

export type SettingsTab = 'general' | 'harness' | 'code-review' | 'work-queue' | 'updates';
type Tab = SettingsTab;

const TABS: Tab[] = ['general', 'harness', 'code-review', 'work-queue', 'updates'];
const TAB_LABEL: Record<Tab, string> = {
  general: 'General', harness: 'Harness', 'code-review': 'Code review', 'work-queue': 'Work queue', updates: 'Updates',
};

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
  /** What Meko, which decides who reaches AgentK, says about this browser (null without Meko). */
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
  // One saver for General, Harness, Code review and Work queue, so a save from a tab that has since closed
  // still shows how it went. General's usage-stats box saves on its own.
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
      {tab === 'code-review' && <CodeReviewTab auto={auto} />}
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
    <InfoCheck id="set-rail-sessions" label="List sessions in the projects pane" checked={on} onChange={saveRailSessions}>
      Under each project, in place of the tabs above the terminal. Kept in this browser.
    </InfoCheck>
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
    <InfoCheck id="set-harness-logos" label="Show each session's agent logo beside its name" checked={on} onChange={harnessLogosToggle.save}>
      When your open sessions run on more than one agent. The tooltip names the agent either way. Kept in this browser.
    </InfoCheck>
  );
}

/** A tab's server fields, as `PUT /api/settings` takes them. */
type Patch = Omit<SettingsUpdate, 'meko'>;

/**
 * General's, Harness's, Code review's and Work queue's server settings, saved as they change
 * (`src/web/autosave.ts`): `change` shows a patch at once and saves it, and a patch that fails to
 * save goes back to the server's last answer. `load` waits for the saves still out, so a tab opened again shows them.
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
    setReviewDefaults(x);
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
      <p className="muted small">What Sync adds to the queue; both off turns it off.</p>
      <InfoCheck id="set-wq-github" label="GitHub review requests" checked={s.workQueueGithub} onChange={(on) => change({ workQueueGithub: on })}>
        Open PRs that ask for your review, through your gh login.
      </InfoCheck>
      <InfoCheck id="set-wq-slack" label="Slack To-dos" checked={s.workQueueSlack} onChange={(on) => change({ workQueueSlack: on })}>
        The messages on your To-dos, read by Claude Code through your Slack connector. Each read is a model call; on the
        timer it runs only while the last read worked.
      </InfoCheck>
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

/** Code review, once its settings are loaded. */
function CodeReviewTab({ auto }: { auto: AutoSettings }) {
  const { s, load, change } = auto;
  const [error, setError] = useState<string | null>(null);
  useEffect(() => {
    load().catch((e) => setError(errorMessage(e)));
  }, [load]);
  if (!s) return error ? <ErrorText error={error} /> : <p className="muted"><Busy label="Loading" /></p>;
  return <CodeReviewSettings s={s} change={change} />;
}

/**
 * The local review before a PR opens (docs/design/code-review.md): the global strategy, re-review
 * rounds and depth, which projects and sessions can override. Each saves as it's picked.
 */
export function CodeReviewSettings({ s, change }: { s: Settings; change: (patch: Patch) => void }) {
  return (
    <div className="stack">
      <p className="muted small">The defaults. Projects and sessions can set their own.</p>
      <div className="review-settings">
        <ReviewFields
          id="set-review"
          scoped={false}
          inherited={null}
          value={{ strategy: s.codeReviewStrategy, rounds: s.codeReviewRounds, depth: s.codeReviewDepth }}
          onChange={(v) => {
            const patch = {
              codeReviewStrategy: v.strategy ?? s.codeReviewStrategy,
              codeReviewRounds: v.rounds ?? s.codeReviewRounds,
              codeReviewDepth: v.depth ?? s.codeReviewDepth,
            };
            setReviewDefaults(patch);
            change(patch);
          }}
        />
      </div>
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
      <TerminalColors />
      <div className="field">
        <span className="label">Repo folders</span>
        <span className="hint">Where Open project looks for git repos.</span>
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
        <Field label="Scan depth" info={{ for: 'set-depth', text: 'How many folders deep to look for repos. 1 looks only directly inside each repo folder.' }}>
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
      <div className="fields">
        <Field
          label="Idle worktrees per repo"
          info={{
            for: 'set-idle-worktrees',
            text:
              'When a session ends, its worktree waits, build outputs intact, for the next session on the same repo, from any project. ' +
              'Each one keeps its outputs on disk. 0 removes worktrees when sessions end.',
          }}
        >
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
        <Field
          label="Remove unused after (days)"
          info={{ for: 'set-idle-worktree-days', text: 'An idle worktree no session has used for this long is removed. 0 keeps them.' }}
        >
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
      <InfoCheck
        id="set-resolve-conflicts"
        label="Resolve merge conflicts on PRs"
        checked={s.resolveConflicts}
        onChange={(on) => change({ resolveConflicts: on })}
      >
        When a PR conflicts with its base, AgentK merges the base in, resolves the conflicts and pushes. Off leaves them to you.
      </InfoCheck>
      <InfoCheck
        id="set-tools-enabled"
        label="Let sessions start and move sessions"
        checked={s.toolsEnabled}
        onChange={(on) => change({ toolsEnabled: on })}
      >
        Gives agents AgentK’s session tools. When you ask, they can open projects, start sessions (from GitHub issues and PRs
        too), move sessions between stages and open another session’s PR. The agent asks before each change, unless Kiro may
        run any tool without asking (Harness). Off applies at once. On applies at each session’s next start.
      </InfoCheck>
      <Field label="Server name" info={{ for: 'set-server-name', text: 'Shown in the projects pane next to your user name. Empty uses this machine’s name.' }}>
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
    </div>
  );
}

/** The terminals' colors: kept in this browser as soon as one is picked, not on the server. */
function TerminalColors() {
  const [pref, setPref] = useState(termThemePref);
  return (
    <Field
      label="Terminal colors"
      hint="Kept in this browser."
      info={{
        for: 'set-term-theme',
        text: 'Applies at once to every terminal in this browser. Claude Code has its own colors too: run /theme in a session to match.',
      }}
    >
      <select
        id="set-term-theme"
        className="input"
        value={pref}
        onChange={(e) => {
          const v = e.target.value as TermThemePref;
          setPref(v);
          setTermThemePref(v);
        }}
      >
        {(Object.keys(TERM_THEME_LABEL) as TermThemePref[]).map((k) => (
          <option key={k} value={k}>
            {TERM_THEME_LABEL[k]}
          </option>
        ))}
      </select>
    </Field>
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
    if (onlyMekoBlocks(h)) return `Version ${h.version}; not offered until it’s connected to Meko.`;
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
          <div className="info-row">
            <span className="label" id="set-agent-label">
              Default agent
            </span>
            <InfoButton id="set-agent-info" label="Default agent" />
          </div>
          <AgentPicker
            idPrefix="set-agent"
            agents={defaultAgent.agents}
            value={defaultAgent.value}
            onChange={(id) => change({ defaultHarness: id })}
            labelledBy="set-agent-label"
          />
          <InfoPanel id="set-agent-info">
            New sessions and Ask AgentK run on it. The New session dialog can pick another, and a session keeps the agent it
            started on.
          </InfoPanel>
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
          onHarnesses={setRechecked}
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
  onHarnesses = () => undefined,
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
  /** Every harness's status as Connect to Meko's refresh answered, until the next state push. */
  onHarnesses?: (harnesses: HarnessStatus[]) => void;
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
      {meko && enabled && status && (status.available || onlyMekoBlocks(status)) && (
        <AgentMeko id={id} harness={status} meko={meko} onHarnesses={onHarnesses} />
      )}
      {id === 'opencode' && enabled && (
        <InfoCheck
          id="set-opencode-tracking"
          label="Turn off OpenCode's tracking"
          checked={s.opencodeDisableTracking}
          onChange={(on) => onChange({ opencodeDisableTracking: on })}
        >
          On, OpenCode sessions and Ask AgentK send no telemetry, skip OpenCode's update check and hide OpenCode Zen's free
          models, whose providers may train on your prompts, and commands the agent runs export no OpenTelemetry. Your prompts
          and code still go to the model you pick; for code that must stay on this computer, use a local model (Ollama, LM
          Studio). Applies when a session next starts or resumes.
        </InfoCheck>
      )}
      {id === 'kiro' && enabled && (
        <InfoCheck id="set-kiro-trust" label="Let Kiro run any tool without asking" checked={s.kiroTrustTools} onChange={(on) => onChange({ kiroTrustTools: on })}>
          Kiro tells AgentK nothing while it waits for a permission answer, so the session looks busy until you answer in its
          terminal. On, Kiro runs every tool, shell commands included, without asking, in AgentK's sessions only. Applies when a
          session next starts or resumes.
        </InfoCheck>
      )}
    </div>
  );
}

/** Where AgentK's Meko key comes from, after "AgentK uses". */
const SOURCE_TEXT: Record<MekoKeySource, string> = {
  meko: 'Meko Cloud',
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
 * its other extensions) and, on a Meko from before it dropped its login, the account that signs
 * people in to both. A newer Meko has no account page: its status has no username.
 */
export function MekoLinks({ meko, auth }: { meko: MekoStatus; auth: MekoAuth | null }) {
  // Meko Cloud reached through the Meko on this computer is named by its web app, not that Meko's URL.
  const host = httpHost((meko.source === 'meko' && meko.webUrl) || meko.url || '');
  return (
    <div className="field" id="set-meko-links">
      <div className="info-row">
        <span className="label">Meko</span>
        <InfoButton id="set-meko-info" label="Meko" />
      </div>
      <span className="hint">
        {meko.source
          ? `AgentK uses ${SOURCE_TEXT[meko.source]}${host && meko.source !== 'local' ? ` (${host})` : ''}.${
              meko.source !== 'meko' && meko.source !== 'local' ? ` ${OWN_KEY_FIX}` : ''
            }`
          : 'Meko isn’t set up: start Meko on this computer (meko start), or link Meko Cloud.'}
      </span>
      <InfoPanel id="set-meko-info">
        {auth?.access === 'remote' && auth.username ? 'Meko signs you in to AgentK.' : auth?.access === 'local' ? 'Meko opens AgentK on this computer without a sign-in.' : 'Meko opens AgentK without a sign-in.'} Who can
        reach AgentK, and on which port, are set in Meko’s config.toml, in its <code>[web]</code> table.
      </InfoPanel>
      <div className="settings-actions">
        <a className="btn small" id="set-meko-cloud" href={MEKO_CLOUD_PATH}>
          Meko Cloud ↗
        </a>
        {auth?.username && (
          <a className="btn small" id="set-meko-account" href={MEKO_ACCOUNT_PATH}>
            Account ↗
          </a>
        )}
      </div>
    </div>
  );
}

/** The Meko row's tag for an agent's `meko` MCP server, as Meko reports it (`HarnessStatus.meko`). */
const MEKO_TAG: Record<HarnessMekoState, string> = {
  connected: 'connected',
  stale: 'needs reconnecting',
  elsewhere: 'connected elsewhere',
  none: 'not connected',
  missing: 'not found',
};

/**
 * Settings › Harness › an agent: whether it's connected to Meko (Meko's list, `HarnessStatus.meko`),
 * with **Connect to Meko** when it isn't, and whether its sessions reach AgentK's Meko, worked out by
 * the server (`MekoStatus.claude`, `.opencode`, `.kiro`), with Meko's setup for what Meko's capture
 * needs. Nothing when there's nothing to say for that agent.
 */
function AgentMeko({
  id, harness, meko, onHarnesses,
}: { id: HarnessId; harness: HarnessStatus; meko: MekoStatus; onHarnesses: (harnesses: HarnessStatus[]) => void }) {
  const [busy, setBusy] = useState(false);
  const [done, setDone] = useState<string | null>(null);
  const [error, setError] = useState<string | null>(null);
  const label = HARNESS_CAPS[id].label;
  const url = meko.url ?? '';
  const m = harness.meko;
  // The connection first, then what AgentK checks itself.
  let tag: string | null = m ? MEKO_TAG[m.state] : null;
  const lines: string[] = [];
  if (m?.state === 'elsewhere') lines.push(`Connected elsewhere: ${m.url ?? 'another server'}.`);
  else if (m?.state === 'missing') lines.push(`Meko didn’t find ${label} on this computer.`);
  else if (!m && meko.agentList === 'old') lines.push(UPDATE_MEKO_AGENTS);
  // Why, or what a fix does, behind the ⓘ.
  let info: string | null = null;
  let capture: string[] = [];
  if (meko.configured && id === 'claude') {
    const c = meko.claude;
    capture = c.capture;
    lines.push(...c.capture, ...c.unchecked);
    tag ??= c.capture.length ? 'differs' : c.checking ? 'checking' : c.unchecked.length ? 'not checked' : 'matches';
    if (c.checking && !lines.length) lines.push('Checking Claude Code’s token on this Meko.');
  } else if (meko.configured && id === 'kiro' && meko.kiro) {
    capture = meko.kiro.capture;
    lines.push(...capture);
    tag ??= capture.length ? 'differs' : 'matches';
  } else if (meko.configured && id === 'opencode' && meko.opencode) {
    tag ??= meko.opencode.own ? 'its own' : 'matches';
    info = meko.opencode.own ? 'AgentK leaves OpenCode’s config as it is.' : 'AgentK adds it to their config.';
    if (!lines.length) lines.push(meko.opencode.own ? 'Sessions use OpenCode’s own meko server.' : `Sessions use ${url}.`);
  }
  if (!tag && !lines.length) return null;
  if (!lines.length && meko.configured && (!m || m.state === 'connected')) lines.push(`Sessions use ${url}.`);
  const setUp = capture.length > 0 && (id === 'claude' || id === 'kiro');
  const connect = !setUp && connectable(m);
  if (setUp) info = 'Meko’s setup changes the agent’s own config. New sessions pick it up; running ones keep what they started with.';
  else if (connect) info = 'Meko adds its meko MCP server to the agent’s own config, for all your projects. New sessions pick it up.';
  const bad = !!tag && !['connected', 'matches', 'its own', 'checking'].includes(tag);
  const run = (fn: () => Promise<string>) => {
    setBusy(true);
    setError(null);
    setDone(null);
    fn()
      .then(setDone)
      .catch((e) => setError(errorMessage(e)))
      .finally(() => setBusy(false));
  };
  const connectNow = () =>
    run(async () => {
      const r = await connectToMeko(id);
      onHarnesses(r.harnesses);
      return [`Connected ${label} to Meko.`, ...r.notes].join(' ');
    });
  const setUpNow = () =>
    run(async () => {
      await setUpWithMeko(id as 'claude' | 'kiro', m);
      return `${label} now uses this Meko.`;
    });
  return (
    <div className="field">
      <div className="info-row">
        <span className="label">
          Meko{' '}
          {tag && (
            <span id={`set-${id}-meko`} className={cx('tag', bad && 'shared')}>
              {tag}
            </span>
          )}
        </span>
        {info && <InfoButton id={`set-${id}-meko-info`} label="Meko" />}
      </div>
      {lines.length > 0 && <span className="hint">{lines.join(' ')}</span>}
      {info && <InfoPanel id={`set-${id}-meko-info`}>{info}</InfoPanel>}
      {(setUp || connect) && (
        <div className="settings-actions">
          {setUp ? (
            <button type="button" id={`set-${id}-meko-setup`} className="btn small" disabled={busy} onClick={setUpNow}>
              {busy ? <Busy label="Setting up" /> : `Set up ${label} with this Meko`}
            </button>
          ) : (
            <button type="button" id={`set-${id}-meko-connect`} className="btn small" disabled={busy} onClick={connectNow}>
              {busy ? <Busy label="Connecting" /> : 'Connect to Meko'}
            </button>
          )}
        </div>
      )}
      {done && !setUp && !connect && <span className="ok-text">{done}</span>}
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

/** Who installs AgentK's updates and what one restarts, behind Settings › Updates' ⓘ. */
const UPDATES_INFO =
  'Meko checks for and installs AgentK’s updates. An update restarts only the web server: sessions keep running and the page reconnects on its own.';

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
        // Why updates can't be had here, with who installs them behind the same ⓘ as Update automatically.
        <div className="info-check">
          <div className="info-row">
            <p className="muted">{mekoError}</p>
            <InfoButton id="set-auto-update-info" label="Updates" />
          </div>
          <InfoPanel id="set-auto-update-info">{UPDATES_INFO}</InfoPanel>
        </div>
      ) : (
        <InfoCheck
          id="set-auto-update"
          label="Update automatically"
          checked={row?.auto_update ?? true}
          disabled={!can || busy === 'auto'}
          onChange={(on) => void run('auto', () => mekoUpdates.setAuto(on))}
        >
          {UPDATES_INFO}
        </InfoCheck>
      )}
      {row?.waiting && <p className="muted">An automatic update is waiting: {row.waiting}</p>}
      {row?.check_error && <p className="error-text">Last check failed: {row.check_error}</p>}
      {failed && <p className="error-text">The last update failed: {failed}</p>}
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
