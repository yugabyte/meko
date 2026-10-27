import { UsagePanel } from './UsagePanel';
import { useCallback, useEffect, useMemo, useState, type FormEvent } from 'react';
import {
  REVIEWER_LABELS,
  type DatapackRef,
  type LocalMekoState,
  type MekoDatapack,
  type ProjectHome,
  type ProjectSettings,
  type ProjectWithSessions,
  type ReviewerMode,
  type SearchResult,
} from '../../shared/types';
import { LOGIN_PLACEHOLDER, expandBranchPrefix } from '../../shared/branches';
import { api, errorMessage } from '../api';
import { useIsMobile, useTouch } from '../hooks/useMedia';
import { navigate } from '../router';
import { cx, shortTime } from '../util';
import { Busy, ConfirmButton, DatapackName, ErrorText, Field } from './common';
import { AddDatapackDialog } from './AddDatapackDialog';
import { MekoLinkDialog } from './MekoLinkDialog';
import { ShareDialog } from './ShareDialog';
import { TopBar } from './TopBar';

const SOURCE_LABEL: Record<SearchResult['source'], string> = {
  memory: 'memory',
  knowledge_base: 'knowledge',
  conversation: 'conversation',
};

export function ProjectHomeView({
  project,
  order,
  onMove,
  mekoWebUrl,
  mekoConfigured,
  mekoLocal,
  ghLogin,
  onMenu,
  onBack,
  onLearn,
  onError,
}: {
  project: ProjectWithSessions;
  /** Where the project sits in the projects list. */
  order: { index: number; count: number };
  /** Moves the project to this index in the list (the tap equivalent of dragging it in the rail). */
  onMove: (index: number) => void;
  mekoWebUrl: string | null;
  /** There's a Meko to use (Meko's Meko Cloud link, or the Meko on this computer). */
  mekoConfigured: boolean;
  /** The Meko on this computer (`MekoStatus.local`), for what to do while there's no Meko. */
  mekoLocal: LocalMekoState;
  /** The login gh is signed in as, for `{login}` in the branch prefix. */
  ghLogin: string | null;
  /** Opens the projects drawer (phone layout). */
  onMenu: () => void;
  onBack: () => void;
  /** Opens the New session dialog on Learn the project, offered while the dedicated datapack is empty. */
  onLearn: () => void;
  onError: (m: string) => void;
}) {
  const mobile = useIsMobile();
  const [home, setHome] = useState<ProjectHome | null>(null);
  const [loadError, setLoadError] = useState<string | null>(null);
  /**
   * The open dialog: sharing, setting up Meko, adding a dedicated datapack (with why it's asked
   * for), or switching it (from the one shown when Switch was tapped, so a switch made meanwhile in
   * another tab is refused rather than undone).
   */
  const [dialog, setDialog] = useState<{ kind: 'share' | 'key' | 'datapack' | 'switch'; reason?: string; replacing?: DatapackRef } | null>(null);
  const datapacks = home?.datapacks ?? project.datapacks.map((d) => ({ ...d, memoryCount: null, knowledgeCount: null, grant: null }));
  // Which datapack is dedicated comes from the live project (another tab may have just added one);
  // its counts, grant and owner from the home page's read.
  const dedicatedRef = project.datapacks.find((d) => d.role === 'dedicated') ?? null;
  const dedicated = dedicatedRef
    ? (datapacks.find((d) => d.id === dedicatedRef.id) ?? { ...dedicatedRef, memoryCount: null, knowledgeCount: null, grant: null, ownerName: null, ownerEmail: null })
    : null;
  // A contributor or viewer can't share it: Share says who can instead.
  const cantShare = !!dedicated?.grant && dedicated.grant !== 'owner' && dedicated.grant !== 'maintainer';
  /** Who can share it, when a contributor or viewer asks (null: the notice isn't shown). */
  const [sharers, setSharers] = useState<string[] | null>(null);
  const share = () => {
    if (!mekoConfigured) setDialog({ kind: 'key', reason: 'A project is shared through its Meko datapack, so sharing needs Meko.' });
    else if (!dedicated) setDialog({ kind: 'datapack', reason: `A project is shared through its datapack, and ${project.name} has none yet.` });
    else if (cantShare) {
      const owner = dedicated.ownerName ?? dedicated.ownerEmail;
      const person = (name: string | null, email: string | null) => (name && email ? `${name} (${email})` : (name ?? email ?? ''));
      setSharers(owner ? [`its owner, ${person(dedicated.ownerName, dedicated.ownerEmail)}`] : []);
      // Meko lists the maintainers.
      api
        .datapackPeople(dedicated.id)
        .then((p) => {
          const maintainers = p.members.filter((m) => m.grant === 'maintainer').map((m) => person(m.name || null, m.email || null));
          if (maintainers.length) setSharers((cur) => [...(cur ?? []), `a maintainer: ${maintainers.join(', ')}`]);
        })
        .catch(() => undefined);
    } else setDialog({ kind: 'share' });
  };

  const load = useCallback(() => {
    api
      .home(project.id)
      .then((h) => {
        setHome(h);
        setLoadError(null);
      })
      .catch((e) => setLoadError(errorMessage(e)));
  }, [project.id]);

  // Reload when the project changes shape (datapacks, sessions, activity).
  const sessionsKey = project.sessions.map((s) => `${s.id}:${s.status}:${s.stage}`).join(',');
  const dpKey = project.datapacks.map((d) => `${d.id}:${d.role}`).join(',');
  useEffect(load, [load, sessionsKey, dpKey]);

  return (
    <div className="home-view">
      {mobile && (
        <TopBar onMenu={onMenu} sub={project.name} title="Project home">
          <button className="btn small" onClick={onBack}>
            Sessions
          </button>
        </TopBar>
      )}
      <header className="home-hd">
        {!mobile && (
          <button className="link-btn" onClick={onBack}>
            ← Sessions
          </button>
        )}
        <h1>{project.name}</h1>
        <span className="mono muted small home-repo">
          {project.repoPath} · {project.defaultBranch}
        </span>
        <button className="btn small home-share" onClick={share}>
          Share
        </button>
      </header>
      {sharers && dedicated && (
        <div className="banner info home-share-banner" role="status">
          <span className="banner-text">
            You’re a {dedicated.grant} on <span className="mono">{dedicated.name}</span>, so only its owner or a maintainer can
            share the project.{' '}
            {sharers.length ? `Ask ${sharers.join(', or ')}.` : 'Ask its owner or a maintainer to share it.'}
          </span>
          <button className="btn small" onClick={() => setSharers(null)}>
            OK
          </button>
        </div>
      )}
      <div className="home-scroll">
        {mekoConfigured && project.datapacks.length > 0 && <SearchBox projectId={project.id} />}
        {loadError && <p className="error-text">{loadError}</p>}
        <div className="home-grid">
          <section className="box">
            <span className="label">Datapacks</span>
            {datapacks.map(
              (d) => (
                <div className="dp" key={d.id}>
                  <DatapackName className="mono dp-name" id={d.id} name={d.name} mekoWebUrl={mekoWebUrl} />
                  <span className={cx('tag', d.role === 'shared' && 'shared')}>{d.role}</span>
                  <span className="num">
                    {d.memoryCount != null ? `${fmt(d.memoryCount)} mem` : ''}
                    {d.knowledgeCount != null ? ` · ${fmt(d.knowledgeCount)} knowledge` : ''}
                  </span>
                  {d.role === 'shared' ? (
                    <ConfirmButton
                      className="link-btn"
                      label="Detach"
                      confirmLabel="Detach"
                      danger
                      onConfirm={async () => {
                        try {
                          await api.detachDatapack(project.id, d.id);
                          load();
                        } catch (e) {
                          onError(errorMessage(e));
                        }
                      }}
                    />
                  ) : mekoConfigured ? (
                    <button className="link-btn" onClick={() => setDialog({ kind: 'switch', replacing: { id: d.id, name: d.name, role: d.role } })}>
                      Switch
                    </button>
                  ) : (
                    <span />
                  )}
                </div>
              ),
            )}
            {!dedicated && (
              <div className="dp-none">
                <span className="muted">No dedicated datapack.</span>
                {mekoConfigured ? (
                  <button className="btn small" onClick={() => setDialog({ kind: 'datapack' })}>
                    Add a dedicated datapack
                  </button>
                ) : (
                  <button className="btn small" onClick={() => setDialog({ kind: 'key' })}>
                    Set up Meko
                  </button>
                )}
              </div>
            )}
            {mekoConfigured && dedicated && home && !home.hasLearnSession && dedicated.memoryCount === 0 && dedicated.knowledgeCount === 0 && (
              <div className="dp-none">
                <span className="muted">This datapack is empty.</span>
                <button className="btn small" id="home-learn" onClick={onLearn}>
                  Learn the project
                </button>
              </div>
            )}
            {mekoConfigured && <AttachDatapack
              attached={new Set(project.datapacks.map((d) => d.id))}
              onAttach={async (d) => {
                try {
                  await api.attachDatapack(project.id, { id: d.id, name: d.name });
                  load();
                } catch (e) {
                  onError(errorMessage(e));
                }
              }}
            />}
          </section>
          <section className="box">
            <span className="label">Recent activity</span>
            {!home && !loadError && <p className="muted"><Busy label="Loading" /></p>}
            {home && home.activity.length === 0 && <p className="muted">Nothing yet. Start a session to get going.</p>}
            {home && home.activity.length > 0 && (
              <div className="act-list" tabIndex={0} aria-label="Recent activity">
                {home.activity.map((a, i) => (
                  <div className="act" key={i}>
                    <time>{shortTime(a.at)}</time>
                    <span>
                      {a.sessionId && a.sessionName ? (
                        <button
                          className="link-btn inline"
                          onClick={() => navigate({ view: 'project', projectId: project.id, sessionId: a.sessionId })}
                        >
                          {a.sessionName}
                        </button>
                      ) : null}
                      {a.sessionName ? ': ' : ''}
                      {a.text}
                    </span>
                  </div>
                ))}
              </div>
            )}
          </section>
        </div>
        {home && <UsagePanel projectId={project.id} usage={home.usage} mekoOn={mekoConfigured && project.datapacks.length > 0} />}
        <ProjectSettingsForm project={project} order={order} onMove={onMove} ghLogin={ghLogin} mekoConfigured={mekoConfigured} onError={onError} />
      </div>
      {dialog?.kind === 'share' && dedicated && (
        <ShareDialog projectId={project.id} projectName={project.name} datapack={dedicated} onClose={() => setDialog(null)} />
      )}
      {dialog?.kind === 'key' && <MekoLinkDialog reason={dialog.reason} local={mekoLocal} onClose={() => setDialog(null)} />}
      {dialog?.kind === 'datapack' && (
        <AddDatapackDialog project={project} reason={dialog.reason} onClose={() => setDialog(null)} onAdded={() => load()} />
      )}
      {dialog?.kind === 'switch' && dialog.replacing && (
        <AddDatapackDialog
          project={project}
          replacing={dialog.replacing}
          sessions={project.sessions}
          onClose={() => setDialog(null)}
          onAdded={() => load()}
        />
      )}
    </div>
  );
}

function fmt(n: number): string {
  return n >= 1000 ? `${(n / 1000).toFixed(1).replace(/\.0$/, '')}k` : String(n);
}

function SearchBox({ projectId }: { projectId: string }) {
  const [q, setQ] = useState('');
  const [results, setResults] = useState<SearchResult[] | null>(null);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const run = async (e: FormEvent) => {
    e.preventDefault();
    if (!q.trim()) return;
    setBusy(true);
    setError(null);
    try {
      setResults(await api.search(projectId, q.trim()));
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      setBusy(false);
    }
  };

  return (
    <div className="search-block">
      <form className="filter-row" onSubmit={run}>
        <input
          id="home-search"
          className="input"
          placeholder="Search this project’s memory and knowledge…"
          value={q}
          onChange={(e) => setQ(e.target.value)}
        />
        <button className="btn" disabled={busy || !q.trim()}>
          {busy ? <Busy label="Searching" /> : 'Search'}
        </button>
      </form>
      <ErrorText error={error} />
      {results && (
        <div className="results">
          {results.length === 0 && <p className="muted">No matches in this project’s datapacks.</p>}
          {results.map((r, i) => (
            <div className="result" key={i}>
              <div className="result-meta">
                <span className="tag shared">{SOURCE_LABEL[r.source]}</span>
                {r.datapackName && <span className="mono muted small">{r.datapackName}</span>}
              </div>
              <p>{r.text}</p>
            </div>
          ))}
        </div>
      )}
    </div>
  );
}

function AttachDatapack({
  attached,
  onAttach,
}: {
  attached: Set<string>;
  onAttach: (d: MekoDatapack) => Promise<void>;
}) {
  // On touch, the keyboard comes up only when a field is tapped.
  const touch = useTouch();
  const [open, setOpen] = useState(false);
  const [all, setAll] = useState<MekoDatapack[] | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [filter, setFilter] = useState('');

  useEffect(() => {
    if (!open || all) return;
    api
      .datapacks()
      .then(setAll)
      .catch((e) => setError(errorMessage(e)));
  }, [open, all]);

  const shown = useMemo(() => {
    const q = filter.trim().toLowerCase();
    return (all ?? []).filter((d) => !attached.has(d.id) && (!q || d.name.toLowerCase().includes(q))).slice(0, 100);
  }, [all, attached, filter]);

  if (!open) {
    return (
      <button className="link-btn" onClick={() => setOpen(true)}>
        + Attach datapack
      </button>
    );
  }
  return (
    <div className="dp-picker">
      <div className="filter-row">
        <input
          id="attach-filter"
          className="input"
          placeholder="Search your datapacks…"
          value={filter}
          onChange={(e) => setFilter(e.target.value)}
          autoFocus={!touch}
        />
        <button className="btn ghost" onClick={() => setOpen(false)}>
          Done
        </button>
      </div>
      <div className="dp-list">
        {error && <p className="error-text pad">{error}</p>}
        {!all && !error && <p className="muted pad">
                <Busy label="Loading datapacks" />
              </p>}
        {all && shown.length === 0 && <p className="muted pad">No other datapacks match.</p>}
        {shown.map((d) => (
          <button key={d.id} className="dp-row" onClick={() => onAttach(d)}>
            <span className="mono">{d.name}</span>
            <span className="link-btn">Attach</span>
          </button>
        ))}
      </div>
    </div>
  );
}

/** Under the branch prefix: what {login} stands for, and what the prefix comes to. */
function branchPrefixHint(prefix: string, login: string | null): string | null {
  if (!prefix.includes(LOGIN_PLACEHOLDER)) return null;
  const now = expandBranchPrefix(prefix, login);
  return login
    ? `${LOGIN_PLACEHOLDER} is your GitHub login: new branches start with ${now}`
    : `${LOGIN_PLACEHOLDER} is your GitHub login, once gh is signed in. Until then, new branches start with ${now}`;
}

function ProjectSettingsForm({
  project,
  order,
  onMove,
  ghLogin,
  mekoConfigured,
  onError,
}: {
  project: ProjectWithSessions;
  order: { index: number; count: number };
  onMove: (index: number) => void;
  ghLogin: string | null;
  /** Meko is on: Log Meko calls is offered (and kept on show while it's on, so it can be turned off). */
  mekoConfigured: boolean;
  onError: (m: string) => void;
}) {
  const [name, setName] = useState(project.name);
  const [s, setS] = useState<ProjectSettings>(project.settings);
  const [saved, setSaved] = useState(false);
  const [busy, setBusy] = useState(false);
  // Every state push brings a new settings object; reset the form only when the saved values change.
  const savedKey = JSON.stringify([project.name, project.settings]);
  useEffect(() => {
    const [n, ps] = JSON.parse(savedKey) as [string, ProjectSettings];
    setName(n);
    setS(ps);
  }, [project.id, savedKey]);

  const dirty = name !== project.name || JSON.stringify(s) !== JSON.stringify(project.settings);
  const set = <K extends keyof ProjectSettings>(k: K, v: ProjectSettings[K]) => setS((x) => ({ ...x, [k]: v }));

  const save = async (e: FormEvent) => {
    e.preventDefault();
    setBusy(true);
    try {
      await api.updateProject(project.id, { name: name.trim(), settings: s });
      setSaved(true);
      setTimeout(() => setSaved(false), 2500);
    } catch (err) {
      onError(errorMessage(err));
    } finally {
      setBusy(false);
    }
  };

  return (
    <section className="box settings-box">
      <span className="label">Project settings</span>
      <form className="settings-grid" onSubmit={save}>
        <Field label="Name">
          <input id="ps-name" className="input" value={name} onChange={(e) => setName(e.target.value)} />
        </Field>
        <Field label="Branch prefix" hint={branchPrefixHint(s.branchPrefix, ghLogin)}>
          <input
            id="ps-prefix"
            className="input mono"
            value={s.branchPrefix}
            onChange={(e) => set('branchPrefix', e.target.value)}
          />
        </Field>
        <Field label="Reviewer">
          <select
            id="ps-reviewer"
            className="input"
            value={s.reviewer}
            onChange={(e) => set('reviewer', e.target.value as ReviewerMode)}
            title="Whose reviews go straight to AgentK. Every other review is held for you to send or dismiss, and your own always go. Merge waits for the AgentK reviewer once it's active on the PR."
          >
            {(Object.keys(REVIEWER_LABELS) as ReviewerMode[]).map((m) => (
              <option key={m} value={m}>{REVIEWER_LABELS[m]}</option>
            ))}
          </select>
        </Field>
        <Field label="AgentK login">
          <input
            id="ps-login"
            className="input mono"
            value={s.reviewerLogin}
            disabled={s.reviewer === 'people'}
            onChange={(e) => set('reviewerLogin', e.target.value)}
          />
        </Field>
        <Field label="Merge method">
          <select
            id="ps-merge"
            className="input"
            value={s.mergeMethod}
            onChange={(e) => set('mergeMethod', e.target.value as ProjectSettings['mergeMethod'])}
          >
            <option value="repo-default">Repo default</option>
            <option value="squash">Squash</option>
            <option value="merge">Merge commit</option>
            <option value="rebase">Rebase</option>
          </select>
        </Field>
        <div className="checks">
          <label className="check">
            <input
              id="ps-auto"
              type="checkbox"
              checked={s.autoContinue}
              onChange={(e) => set('autoContinue', e.target.checked)}
            />
            <span>Continue interrupted turns after a restart</span>
          </label>
          <label className="check">
            <input id="ps-notify" type="checkbox" checked={s.notify} onChange={(e) => set('notify', e.target.checked)} />
            <span>Browser notifications</span>
          </label>
          {(mekoConfigured || project.settings.mekoLog) && (
            <div className="check-row">
              <label className="check">
                <input id="ps-mekolog" type="checkbox" checked={s.mekoLog} onChange={(e) => set('mekoLog', e.target.checked)} />
                <span>
                  Log Meko calls
                  <span className="muted small check-hint">
                    What AgentK and Claude send to Meko for each session, and what comes back. Deleted with the session.
                  </span>
                </span>
              </label>
              <button type="button" className="link-btn" onClick={() => navigate({ view: 'meko', projectId: project.id, sessionId: null })}>
                View calls
              </button>
            </div>
          )}
        </div>
        {order.count > 1 && (
          <div className="settings-actions">
            <span className="muted small">Place in the projects list</span>
            <button type="button" className="btn small" disabled={order.index <= 0} onClick={() => onMove(order.index - 1)}>
              Move up
            </button>
            <button
              type="button"
              className="btn small"
              disabled={order.index < 0 || order.index >= order.count - 1}
              onClick={() => onMove(order.index + 1)}
            >
              Move down
            </button>
          </div>
        )}
        <div className="settings-actions">
          <button className="btn primary" disabled={!dirty || busy || !name.trim()}>
            {busy ? <Busy label="Saving" /> : 'Save'}
          </button>
          {saved && <span className="muted small">Saved</span>}
          <span className="sp" />
          <ConfirmButton
            label="Close project"
            confirmLabel="Close project"
            prompt={`Stops its sessions and removes their worktrees, except ones with uncommitted changes. ${
              project.datapacks.some((d) => d.role === 'dedicated')
                ? 'Your repo, branches and datapacks stay, and opening its datapack again brings the project back.'
                : 'Your repo and branches stay.'
            }`}
            danger
            onConfirm={async () => {
              try {
                await api.closeProject(project.id);
                navigate({ view: 'root' });
              } catch (err) {
                onError(errorMessage(err));
              }
            }}
          />
        </div>
      </form>
    </section>
  );
}
