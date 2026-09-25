import { useCallback, useEffect, useMemo, useState, type FormEvent } from 'react';
import type {
  MekoDatapack,
  ProjectHome,
  ProjectSettings,
  ProjectWithSessions,
  SearchResult,
} from '../../shared/types';
import { api, errorMessage } from '../api';
import { navigate } from '../router';
import { cx, shortTime } from '../util';
import { ConfirmButton, ErrorText, Field } from './common';

const SOURCE_LABEL: Record<SearchResult['source'], string> = {
  memory: 'memory',
  knowledge_base: 'knowledge',
  conversation: 'conversation',
};

export function ProjectHomeView({
  project,
  onBack,
  onError,
}: {
  project: ProjectWithSessions;
  onBack: () => void;
  onError: (m: string) => void;
}) {
  const [home, setHome] = useState<ProjectHome | null>(null);
  const [loadError, setLoadError] = useState<string | null>(null);

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
  const dpKey = project.datapacks.map((d) => d.id).join(',');
  useEffect(load, [load, sessionsKey, dpKey]);

  return (
    <div className="home-view">
      <header className="home-hd">
        <button className="link-btn" onClick={onBack}>
          ← Sessions
        </button>
        <h1>{project.name}</h1>
        <span className="mono muted small">
          {project.repoPath} · {project.defaultBranch}
        </span>
      </header>
      <div className="home-scroll">
        <SearchBox projectId={project.id} />
        {loadError && <p className="error-text">{loadError}</p>}
        <div className="home-grid">
          <section className="box">
            <span className="label">Datapacks</span>
            {(home?.datapacks ?? project.datapacks.map((d) => ({ ...d, memoryCount: null, knowledgeCount: null }))).map(
              (d) => (
                <div className="dp" key={d.id}>
                  <span className="mono dp-name" title={d.id}>
                    {d.name}
                  </span>
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
                  ) : (
                    <span />
                  )}
                </div>
              ),
            )}
            <AttachDatapack
              attached={new Set(project.datapacks.map((d) => d.id))}
              onAttach={async (d) => {
                try {
                  await api.attachDatapack(project.id, { id: d.id, name: d.name });
                  load();
                } catch (e) {
                  onError(errorMessage(e));
                }
              }}
            />
          </section>
          <section className="box">
            <span className="label">Recent activity</span>
            {!home && !loadError && <p className="muted">Loading…</p>}
            {home && home.activity.length === 0 && <p className="muted">Nothing yet. Start a session to get going.</p>}
            {home?.activity.slice(0, 40).map((a, i) => (
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
          </section>
        </div>
        <ProjectSettingsForm project={project} onError={onError} />
      </div>
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
          {busy ? 'Searching…' : 'Search'}
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
          autoFocus
        />
        <button className="btn ghost" onClick={() => setOpen(false)}>
          Done
        </button>
      </div>
      <div className="dp-list">
        {error && <p className="error-text pad">{error}</p>}
        {!all && !error && <p className="muted pad">Loading datapacks…</p>}
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

function ProjectSettingsForm({
  project,
  onError,
}: {
  project: ProjectWithSessions;
  onError: (m: string) => void;
}) {
  const [name, setName] = useState(project.name);
  const [s, setS] = useState<ProjectSettings>(project.settings);
  const [saved, setSaved] = useState(false);
  const [busy, setBusy] = useState(false);
  useEffect(() => {
    setName(project.name);
    setS(project.settings);
  }, [project.id, project.name, project.settings]);

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
        <Field label="Branch prefix">
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
            onChange={(e) => set('reviewer', e.target.value as ProjectSettings['reviewer'])}
          >
            <option value="agentk">AgentK</option>
            <option value="people">People only</option>
          </select>
        </Field>
        <Field label="AgentK login">
          <input
            id="ps-login"
            className="input mono"
            value={s.reviewerLogin}
            disabled={s.reviewer !== 'agentk'}
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
        </div>
        <div className="settings-actions">
          <button className="btn primary" disabled={!dirty || busy || !name.trim()}>
            {busy ? 'Saving…' : 'Save'}
          </button>
          {saved && <span className="muted small">Saved</span>}
          <span className="sp" />
          <ConfirmButton
            label="Delete project"
            confirmLabel="Delete project"
            prompt="Removes its sessions and worktrees. Your repo and datapacks stay."
            danger
            onConfirm={async () => {
              try {
                await api.deleteProject(project.id);
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
