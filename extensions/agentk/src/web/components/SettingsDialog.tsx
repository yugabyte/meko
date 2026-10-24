import { useCallback, useEffect, useRef, useState, type FormEvent, type KeyboardEvent, type MutableRefObject, type ReactNode } from 'react';
import type {
  AuthStatus,
  HarnessStatus,
  MekoKeySource,
  MekoSettingsUpdate,
  MekoStatus,
  Settings,
  SettingsUpdate,
  TotpEnrollResponse,
  UpdateStatus,
} from '../../shared/types';
import { HARNESS_CAPS, HARNESS_IDS, type HarnessId } from '../../shared/harnesses';
import { api, errorMessage } from '../api';
import { AUTO_SAVE_IDLE, autoSave, settle, type AutoSaveStatus } from '../autosave';
import { saveRailSessions, useRailSessions } from '../rail';
import { harnessLogosToggle } from '../toggle';
import { claudeUpdateText, cx, defaultAgentChoice, httpHost, mekoKeyLinkBase, relativeTime } from '../util';
import { AgentLabel, AgentPicker, Busy, ConfirmButton, Dialog, ErrorText, Field } from './common';
import { MekoGetKey } from './MekoGetKey';

export type SettingsTab = 'general' | 'harness' | 'meko' | 'account' | 'updates';
type Tab = SettingsTab;

const TABS: Tab[] = ['general', 'harness', 'meko', 'account', 'updates'];
const TAB_LABEL: Record<Tab, string> = { general: 'General', harness: 'Harness', meko: 'Meko', account: 'Account', updates: 'Updates' };

export function SettingsDialog({
  auth,
  update,
  meko,
  harnesses,
  twoAgents,
  serverName,
  initialTab = 'general',
  onClose,
  onWhatsNew,
}: {
  auth: AuthStatus;
  update: UpdateStatus;
  meko: MekoStatus;
  /** The coding agents sessions can run on (`AppState.harnesses`). */
  harnesses: HarnessStatus[];
  /** Open sessions run on more than one agent (`harnessLogos()`), so the logos would show. */
  twoAgents: boolean;
  /** `AppState.serverName`, for what to do when Claude Code's update fails. */
  serverName: string;
  /** The tab it opens on (the Meko banner's Details opens the Meko tab). */
  initialTab?: SettingsTab;
  onClose: () => void;
  /** Opens What's new from one version to another, in place of Settings. */
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
  const close = () => {
    if (held) {
      typed.current = null;
      return onClose();
    }
    if (closing.current) return;
    closing.current = true;
    typed.current?.();
    void auto.flush().then((ok) => {
      closing.current = false;
      if (ok) onClose();
      else setHeld(true);
    });
  };
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
        </div>
      )}
      {tab === 'harness' && <HarnessTab auto={auto} harnesses={harnesses} twoAgents={twoAgents} serverName={serverName} />}
      {tab === 'meko' && (
        <MekoTab
          status={meko}
          claude={harnesses.find((h) => h.id === 'claude')?.available ?? false}
          kiro={harnesses.find((h) => h.id === 'kiro')?.available ?? false}
        />
      )}
      {tab === 'account' && (
        <div className="stack">
          {auth.localAccess ? <LocalAccessNote /> : <AccountTab auth={auth} />}
          <UsageStatsCheck />
        </div>
      )}
      {tab === 'updates' && <UpdatesTab initial={update} onWhatsNew={onWhatsNew} />}
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
          change, as for its other tools. Takes effect from each session’s next start.
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
      <div className="fields">
        <Field label="Listen address">
          <input id="set-bind" className="input mono" value={s.bind} readOnly disabled />
        </Field>
        <Field label="Port">
          <input id="set-port" className="input mono" value={s.port} readOnly disabled />
        </Field>
      </div>
      <p className="hint">Change the address and port in config.toml, then restart the server.</p>
    </div>
  );
}

/** What Settings says about a harness's CLI: its version, or why it isn't offered. */
function harnessNote(id: HarnessId, h: HarnessStatus | undefined): ReactNode {
  if (!h?.enabled) return 'Off.';
  if (h.version === null) {
    if (!h.checked) return <Busy label="Looking for it" />;
    return `${HARNESS_CAPS[id].label} didn't answer --version here. Install it and run ${HARNESS_CAPS[id].login}, or set [${id}] command in config.toml, then check again.`;
  }
  if (!h.available) return `Version ${h.version}; AgentK needs ${h.minVersion} or later. Update it, then check again.`;
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
}: {
  auto: AutoSettings;
  harnesses: HarnessStatus[];
  twoAgents: boolean;
  serverName: string;
}) {
  const { s, load, change } = auto;
  // The agent the bar shows: the default agent when the tab opens, so a new default doesn't move it.
  const [shown, setShown] = useState<HarnessId>('claude');
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
      .then((x) => setShown(x.defaultHarness))
      .catch((e) => setError(errorMessage(e)));
  }, [load]);

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

const KEY_SOURCE: Record<MekoKeySource, string> = {
  config: 'from config.toml',
  env: 'from the MEKO_API_KEY environment variable',
  claude: 'from Claude Code’s settings',
};

/** The Meko server and key AgentK uses, its links, and whether Claude Code's (and OpenCode's and Kiro's) sessions use the same. */
function MekoTab({ status, claude: claudeOn, kiro }: { status: MekoStatus; claude: boolean; kiro: boolean }) {
  const [s, setS] = useState<Settings | null>(null);
  const [url, setUrl] = useState('');
  const [key, setKey] = useState('');
  const [webUrl, setWebUrl] = useState('');
  const [apiUrl, setApiUrl] = useState('');
  const [confirmHost, setConfirmHost] = useState<string | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [saved, setSaved] = useState(false);
  const [busy, setBusy] = useState<'save' | 'claude' | 'kiro' | null>(null);
  const [claudeDone, setClaudeDone] = useState(false);
  const [kiroDone, setKiroDone] = useState(false);

  const show = (x: Settings) => {
    setS(x);
    setUrl(x.meko.url);
    setWebUrl(x.meko.webUrl);
    setApiUrl(x.meko.apiUrl);
    setKey('');
  };

  useEffect(() => {
    api
      .settings()
      .then(show)
      .catch((e) => setError(errorMessage(e)));
  }, []);

  if (!s) return error ? <ErrorText error={error} /> : <p className="muted"><Busy label="Loading" /></p>;
  const m = s.meko;

  const save = async (e: FormEvent) => {
    e.preventDefault();
    const next: MekoSettingsUpdate = {};
    if (url.trim() !== m.url) next.url = url.trim();
    if (key.trim()) next.apiKey = key.trim();
    if (webUrl.trim() !== m.webUrl) next.webUrl = webUrl.trim();
    if (apiUrl.trim() !== m.apiUrl) next.apiUrl = apiUrl.trim();
    if (!Object.keys(next).length) return;
    // Datapacks live on one Meko: moving to another server leaves existing projects' behind.
    const newHost = httpHost(url.trim() || m.defaultUrl);
    if (next.url !== undefined && newHost && newHost !== httpHost(m.effectiveUrl) && confirmHost !== newHost) {
      setConfirmHost(newHost);
      return;
    }
    setBusy('save');
    setError(null);
    try {
      show(await api.saveSettings({ meko: next }));
      setConfirmHost(null);
      setSaved(true);
      setTimeout(() => setSaved(false), 2500);
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      setBusy(null);
    }
  };

  const setUpClaude = async () => {
    setBusy('claude');
    setError(null);
    setClaudeDone(false);
    try {
      show(await api.mekoClaudeSetup());
      setClaudeDone(true);
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      setBusy(null);
    }
  };

  const setUpKiro = async () => {
    setBusy('kiro');
    setError(null);
    setKiroDone(false);
    try {
      show(await api.mekoKiroSetup());
      setKiroDone(true);
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      setBusy(null);
    }
  };
  // Worked out by the server (MekoStatus.kiro): Kiro's Meko MCP server is this one and sends a key.
  const kiroGaps = status.kiro?.gaps ?? [];

  // Worked out by the server (MekoStatus.claude): the same server in both of Claude Code's places, tokens that work there.
  const claude = status.claude;
  const claudeProblems = [...claude.gaps, ...claude.unchecked];
  const claudeTag = claude.gaps.length ? 'differs' : claude.checking ? 'checking' : claude.unchecked.length ? 'not checked' : 'matches';

  /** Turns Meko off (the key is kept) or back on (checked with Meko first). */
  const setEnabled = async (enabled: boolean) => {
    setBusy('save');
    setError(null);
    try {
      show(await api.saveSettings({ meko: { enabled } }));
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      setBusy(null);
    }
  };

  return (
    <form className="stack" onSubmit={save}>
      <div className="meko-onoff">
        {m.enabled ? (
          <>
            <span className="muted">Meko gives projects datapacks: shared memory, and what a project is shared through.</span>
            <ConfirmButton
              className="btn small"
              label="Turn off Meko"
              confirmLabel="Turn off"
              danger
              prompt="AgentK stops using Meko: new sessions get no datapack, and designs and summaries aren't saved there. Projects keep their datapacks for when you turn it back on. Claude Code's own Meko setup isn't changed."
              onConfirm={() => setEnabled(false)}
            />
          </>
        ) : (
          <>
            <span>
              <b>Meko is off.</b> <span className="muted">Projects work without datapacks.</span>
            </span>
            <button type="button" className="btn small primary" disabled={busy !== null || m.keyHint === null} onClick={() => void setEnabled(true)}>
              Turn on Meko
            </button>
          </>
        )}
      </div>
      <Field label="MCP server URL" hint={`Empty means Meko Cloud (${m.defaultUrl}), unless the environment or Claude Code’s settings name one. Now using ${m.effectiveUrl}.`}>
        <input
          id="set-meko-url"
          className="input mono"
          placeholder={m.defaultUrl}
          value={url}
          onChange={(e) => {
            setUrl(e.target.value);
            setConfirmHost(null);
          }}
        />
      </Field>
      {m.keyHint === null && (
        <MekoGetKey
          title="No Meko API key yet."
          webUrl={mekoKeyLinkBase({ url, webUrl }, m)}
          fallback={`Fill in the web app URL below to get links to sign up and make a key on ${httpHost(url.trim() || m.fallbackUrl) ?? 'this Meko'}.`}
        />
      )}
      <Field
        label="API key"
        hint={
          m.keyHint === null
            ? 'Paste the key you generated in Meko.'
            : `A key is set${m.keyHint ? `, ending in ${m.keyHint}` : ''}${m.keySource ? `, ${KEY_SOURCE[m.keySource]}` : ''}. Type a new key to replace it. The key is never shown in full.`
        }
      >
        <input
          id="set-meko-key"
          className="input mono"
          type="password"
          autoComplete="off"
          placeholder={m.keyHint ? `••••••••${m.keyHint}` : 'mko_tkn_…'}
          value={key}
          onChange={(e) => setKey(e.target.value)}
        />
      </Field>
      <div className="fields">
        <Field label="Web app URL" hint="For links to datapacks, and sharing: the API URL defaults to it plus /api/v1.">
          <input
            id="set-meko-web"
            className="input mono"
            placeholder={m.effectiveWebUrl ?? 'https://meko.example.com'}
            value={webUrl}
            onChange={(e) => setWebUrl(e.target.value)}
          />
        </Field>
        <Field label="API URL" hint="Meko’s REST API, for sharing.">
          <input
            id="set-meko-api"
            className="input mono"
            placeholder={m.effectiveApiUrl ?? 'https://meko.example.com/api/v1'}
            value={apiUrl}
            onChange={(e) => setApiUrl(e.target.value)}
          />
        </Field>
      </div>
      <p className="hint">
        Leave the web app and API URLs empty for Meko Cloud: they’re worked out from the MCP URL. For your own Meko, fill in
        the web app URL; the API URL defaults to it plus /api/v1.
      </p>
      {status.configured && !m.effectiveWebUrl && (
        <p className="confirm-warn" role="status">
          AgentK can’t work out the web app for {httpHost(m.effectiveUrl) ?? 'this Meko'}, so datapack links
          {m.effectiveApiUrl ? ' are' : ' and sharing are'} off. Set the web app URL to turn them on.
        </p>
      )}

      <div className="meko-status">
        <div className="meko-status-row">
          <span>Meko</span>
          {status.configured ? <span className="tag">on</span> : <span className="tag shared">off</span>}
          {status.configured && <span className="hint">A new URL or key is checked with Meko before it's saved.</span>}
        </div>
        {status.configured && claudeOn && (
          <div className="meko-status-row">
            <span>Claude Code</span>
            <span className={cx('tag', claudeTag !== 'matches' && 'shared')}>{claudeTag}</span>
            <span className="hint">
              {claudeProblems.length
                ? `${claudeProblems.join(' ')} New sessions pick up a fix; running ones keep what they started with.`
                : claude.checking
                  ? 'Checking Claude Code’s tokens on this Meko.'
                  : `Sessions use ${m.effectiveUrl}, with tokens that work there.`}
            </span>
            {claudeProblems.length > 0 && (
              <button type="button" className="btn small" disabled={busy !== null} onClick={() => void setUpClaude()}>
                {busy === 'claude' ? <Busy label="Setting up" /> : 'Set up Claude Code with this Meko'}
              </button>
            )}
            {claudeDone && !claudeProblems.length && <span className="ok-text">Claude Code now uses this Meko.</span>}
          </div>
        )}
        {status.configured && status.opencode && (
          <div className="meko-status-row">
            <span>OpenCode</span>
            <span className="tag">{status.opencode.own ? 'its own' : 'matches'}</span>
            <span className="hint">
              {status.opencode.own
                ? 'OpenCode sessions use the meko server in OpenCode’s own config, which AgentK leaves as it is.'
                : `OpenCode sessions use ${m.effectiveUrl}: AgentK adds it to their config.`}
            </span>
          </div>
        )}
        {status.configured && kiro && status.kiro && (
          <div className="meko-status-row">
            <span>Kiro</span>
            <span className={cx('tag', kiroGaps.length > 0 && 'shared')}>{kiroGaps.length ? 'differs' : 'matches'}</span>
            <span className="hint">
              {kiroGaps.length
                ? `${kiroGaps.join(' ')} Meko's setup for Kiro adds its MCP server and its agent, which Kiro sessions are built from.`
                : `Kiro sessions use ${m.effectiveUrl}.`}
            </span>
            {kiroGaps.length > 0 && (
              <button type="button" className="btn small" disabled={busy !== null} onClick={() => void setUpKiro()}>
                {busy === 'kiro' ? <Busy label="Setting up" /> : 'Set up Kiro with this Meko'}
              </button>
            )}
            {kiroDone && !kiroGaps.length && <span className="ok-text">Kiro now uses this Meko.</span>}
          </div>
        )}
      </div>

      {confirmHost && (
        <p className="confirm-warn" role="alert">
          Existing projects’ datapacks live on {httpHost(m.effectiveUrl) ?? 'the current Meko'}, not on {confirmHost}, so they
          won’t work after the switch. Press Switch Meko to switch anyway.
        </p>
      )}
      <ErrorText error={error} />
      <div className="settings-actions">
        <button className="btn primary" disabled={busy !== null}>
          {busy === 'save' ? <Busy label="Checking" /> : confirmHost ? 'Switch Meko' : 'Check and save'}
        </button>
        {saved && <span className="muted small">Saved</span>}
      </div>
    </form>
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

function LocalAccessNote() {
  return (
    <div className="stack">
      <h3>No sign-in needed</h3>
      <p className="muted">
        AgentK only listens on this computer, so it opens without a username and password. To reach it from other
        devices, rerun <code>agentk install</code> and choose remote access: that turns on HTTPS and a login.
      </p>
    </div>
  );
}

function AccountTab({ auth }: { auth: AuthStatus }) {
  const [current, setCurrent] = useState('');
  const [next, setNext] = useState('');
  const [confirm, setConfirm] = useState('');
  const [pwMsg, setPwMsg] = useState<string | null>(null);
  const [pwErr, setPwErr] = useState<string | null>(null);
  const [pwBusy, setPwBusy] = useState(false);

  // The login-time auth status goes stale once two-step sign-in is turned on or off, so ask again.
  const [totpOn, setTotpOn] = useState<boolean | null>(null);
  const [enroll, setEnroll] = useState<TotpEnrollResponse | null>(null);
  const [code, setCode] = useState('');
  const [totpErr, setTotpErr] = useState<string | null>(null);
  const [totpMsg, setTotpMsg] = useState<string | null>(null);
  const [totpBusy, setTotpBusy] = useState(false);

  useEffect(() => {
    let live = true;
    api
      .authStatus()
      .then((a) => live && setTotpOn(a.totpEnabled))
      .catch(() => live && setTotpOn(auth.totpEnabled));
    return () => {
      live = false;
    };
  }, [auth.totpEnabled]);

  const changePw = async (e: FormEvent) => {
    e.preventDefault();
    if (pwBusy) return;
    setPwErr(null);
    setPwMsg(null);
    if (next.length < 12) return setPwErr('Use at least 12 characters.');
    if (next !== confirm) return setPwErr('The two new passwords don’t match.');
    setPwBusy(true);
    try {
      await api.changePassword(current, next);
      setCurrent('');
      setNext('');
      setConfirm('');
      setPwMsg('Password changed.');
    } catch (err) {
      setPwErr(errorMessage(err));
    } finally {
      setPwBusy(false);
    }
  };

  const startEnroll = async () => {
    if (totpBusy) return;
    setTotpErr(null);
    setTotpBusy(true);
    try {
      setEnroll(await api.totpEnroll());
    } catch (err) {
      setTotpErr(errorMessage(err));
    } finally {
      setTotpBusy(false);
    }
  };

  const confirmTotp = async (e: FormEvent) => {
    e.preventDefault();
    if (totpBusy) return;
    setTotpErr(null);
    setTotpBusy(true);
    try {
      if (totpOn) {
        await api.totpDisable(code);
        setTotpOn(false);
        setTotpMsg('Two-step sign-in is off.');
      } else {
        await api.totpConfirm(code);
        setTotpOn(true);
        setEnroll(null);
        setTotpMsg('Two-step sign-in is on.');
      }
      setCode('');
    } catch (err) {
      setTotpErr(errorMessage(err));
    } finally {
      setTotpBusy(false);
    }
  };

  return (
    <div className="stack">
      <form className="stack" onSubmit={changePw}>
        <h3>Change password</h3>
        <Field label="Current password">
          <input
            id="acc-current"
            className="input"
            type="password"
            autoComplete="current-password"
            value={current}
            onChange={(e) => setCurrent(e.target.value)}
          />
        </Field>
        <div className="fields">
          <Field label="New password">
            <input
              id="acc-next"
              className="input"
              type="password"
              autoComplete="new-password"
              value={next}
              onChange={(e) => setNext(e.target.value)}
            />
          </Field>
          <Field label="Confirm new password">
            <input
              id="acc-confirm"
              className="input"
              type="password"
              autoComplete="new-password"
              value={confirm}
              onChange={(e) => setConfirm(e.target.value)}
            />
          </Field>
        </div>
        <ErrorText error={pwErr} />
        {pwMsg && <p className="ok-text">{pwMsg}</p>}
        <div>
          <button className="btn primary" disabled={!current || !next || pwBusy}>
            {pwBusy ? <Busy label="Changing" /> : 'Change password'}
          </button>
        </div>
      </form>

      <div className="stack">
        <h3>Two-step sign-in</h3>
        <p className="muted">
          {totpOn === null
            ? <Busy label="Checking" />
            : totpOn
              ? 'On. Sign-in asks for a code from your authenticator app.'
              : 'Off. Add a code from an authenticator app to every sign-in.'}
        </p>
        {totpOn === false && !enroll && (
          <div>
            <button className="btn" disabled={totpBusy} onClick={startEnroll}>
              Set up authenticator
            </button>
          </div>
        )}
        {enroll && !totpOn && (
          <div className="totp-enroll">
            <div className="qr" dangerouslySetInnerHTML={{ __html: enroll.qrSvg }} />
            <div className="stack">
              <p>Scan the code with your authenticator app, or enter this key:</p>
              <code className="secret">{enroll.secret}</code>
            </div>
          </div>
        )}
        {(enroll || totpOn) && (
          <form className="filter-row" onSubmit={confirmTotp}>
            <input
              id="acc-totp"
              className="input mono"
              inputMode="numeric"
              autoComplete="one-time-code"
              placeholder="6-digit code"
              maxLength={6}
              value={code}
              onChange={(e) => setCode(e.target.value.replace(/\D/g, ''))}
            />
            <button className={cx('btn', totpOn ? 'danger' : 'primary')} disabled={code.length !== 6 || totpBusy}>
              {totpOn ? 'Turn off' : 'Confirm'}
            </button>
          </form>
        )}
        <ErrorText error={totpErr} />
        {totpMsg && <p className="ok-text">{totpMsg}</p>}
      </div>
    </div>
  );
}

function UpdatesTab({ initial, onWhatsNew }: { initial: UpdateStatus; onWhatsNew: (from: string, to: string) => void }) {
  const [u, setU] = useState(initial);
  // What the update on offer brings, else what the last update brought (none on an install never updated).
  const whatsNew =
    u.available && u.latest
      ? { from: u.current, to: u.latest, label: `What’s in v${u.latest}` }
      : u.lastUpdatedFrom
        ? { from: u.lastUpdatedFrom, to: u.current, label: `What’s new in v${u.current}` }
        : null;
  const [busy, setBusy] = useState<'check' | 'apply' | 'channel' | null>(null);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => setU(initial), [initial]);

  const setChannel = async (channel: Settings['updateChannel']) => {
    setBusy('channel');
    setError(null);
    try {
      const next = await api.saveSettings({ updateChannel: channel });
      setU((cur) => ({ ...cur, channel: next.updateChannel }));
    } catch (e) {
      setError(errorMessage(e));
    } finally {
      setBusy(null);
    }
  };

  const run = async (kind: 'check' | 'apply') => {
    setBusy(kind);
    setError(null);
    try {
      setU(kind === 'check' ? await api.checkUpdate() : await api.applyUpdate());
    } catch (e) {
      setError(errorMessage(e));
    } finally {
      setBusy(null);
    }
  };

  return (
    <div className="stack">
      <Field label="Update channel" hint="Saved as soon as you pick one.">
        <select
          id="set-channel"
          className="input"
          value={u.channel}
          disabled={busy === 'channel'}
          onChange={(e) => void setChannel(e.target.value as Settings['updateChannel'])}
        >
          <option value="stable">Stable releases</option>
          <option value="prerelease">Include pre-releases</option>
          <option value="off">Off (don’t check)</option>
        </select>
      </Field>
      <dl className="kv">
        <dt>Installed</dt>
        <dd className="mono">v{u.current}</dd>
        <dt>Latest</dt>
        <dd className="mono">{u.latest ? `v${u.latest}` : '—'}</dd>
        <dt>Last checked</dt>
        <dd>{relativeTime(u.lastCheckedAt)}</dd>
      </dl>
      {u.lastError && <p className="error-text">Last check failed: {u.lastError}</p>}
      <p className="muted">
        Updates restart only the web server. Your Claude sessions keep running, and the page reconnects on its own.
      </p>
      <ErrorText error={error} />
      <div className="settings-actions">
        <button className="btn" disabled={!!busy || u.channel === 'off'} onClick={() => run('check')}>
          {busy === 'check' ? <Busy label="Checking" /> : 'Check now'}
        </button>
        {u.available && (
          <button className="btn primary" disabled={!!busy || u.applying} onClick={() => run('apply')}>
            {busy === 'apply' || u.applying ? <Busy label="Updating" /> : 'Update to the latest version'}
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
