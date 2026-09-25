import { useEffect, useMemo, useState } from 'react';
import type { DatapackChoice, MekoDatapack, Project, RepoInfo } from '../../shared/types';
import { api, errorMessage } from '../api';
import { cx, relativeTime, slugify } from '../util';
import { Dialog, ErrorText, Field } from './common';

type Source = 'list' | 'clone' | 'folder';

function nameFromUrl(url: string): string {
  const m = url.trim().replace(/\/+$/, '').match(/([^/:]+?)(\.git)?$/);
  return m ? m[1] : '';
}

function nameFromPath(p: string): string {
  return p.trim().replace(/\/+$/, '').split('/').pop() ?? '';
}

export function NewProjectDialog({
  mekoOk,
  onClose,
  onCreated,
}: {
  mekoOk: boolean;
  onClose: () => void;
  onCreated: (p: Project) => void;
}) {
  const [repos, setRepos] = useState<RepoInfo[] | null>(null);
  const [roots, setRoots] = useState<string[]>([]);
  const [reposError, setReposError] = useState<string | null>(null);
  const [filter, setFilter] = useState('');
  const [sort, setSort] = useState<'recent' | 'az'>('recent');
  const [source, setSource] = useState<Source>('list');
  const [selected, setSelected] = useState<RepoInfo | null>(null);
  const [cloneUrl, setCloneUrl] = useState('');
  const [folder, setFolder] = useState('');
  const [name, setName] = useState('');
  const [nameTouched, setNameTouched] = useState(false);

  const [datapacks, setDatapacks] = useState<MekoDatapack[] | null>(null);
  const [dpError, setDpError] = useState<string | null>(null);
  const [dpMode, setDpMode] = useState<'create' | 'existing'>('create');
  const [dpName, setDpName] = useState('');
  const [dpNameTouched, setDpNameTouched] = useState(false);
  const [dpExisting, setDpExisting] = useState<MekoDatapack | null>(null);
  const [dpFilter, setDpFilter] = useState('');

  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);

  useEffect(() => {
    api
      .repos()
      .then((r) => {
        setRepos(r.repos);
        setRoots(r.roots);
      })
      .catch((e) => setReposError(errorMessage(e)));
    api
      .datapacks()
      .then(setDatapacks)
      .catch((e) => setDpError(errorMessage(e)));
  }, []);

  // Keep the project name and default datapack name in step with the chosen repo until edited.
  const suggested =
    source === 'list' ? (selected?.name ?? '') : source === 'clone' ? nameFromUrl(cloneUrl) : nameFromPath(folder);
  useEffect(() => {
    if (!nameTouched) setName(suggested);
  }, [suggested, nameTouched]);
  // Datapack naming convention: adlc/proj/<repo>/<project_name>.
  const defaultDp = `adlc/proj/${slugify(suggested) || 'repo'}/${slugify(name) || 'project'}`;
  useEffect(() => {
    if (!dpNameTouched) setDpName(defaultDp);
  }, [defaultDp, dpNameTouched]);

  const shown = useMemo(() => {
    if (!repos) return [];
    const q = filter.trim().toLowerCase();
    const list = repos.filter(
      (r) => !q || r.name.toLowerCase().includes(q) || (r.remote ?? '').toLowerCase().includes(q),
    );
    return list.sort((a, b) => (sort === 'az' ? a.name.localeCompare(b.name) : b.lastTouchedAt - a.lastTouchedAt));
  }, [repos, filter, sort]);

  const dpShown = useMemo(() => {
    const q = dpFilter.trim().toLowerCase();
    return (datapacks ?? []).filter((d) => !q || d.name.toLowerCase().includes(q)).slice(0, 200);
  }, [datapacks, dpFilter]);

  const repoChosen =
    source === 'list' ? !!selected : source === 'clone' ? cloneUrl.trim().length > 0 : folder.trim().length > 0;
  const dpChosen = dpMode === 'create' ? dpName.trim().length > 0 : !!dpExisting;
  const canCreate = repoChosen && dpChosen && name.trim().length > 0 && mekoOk && !busy;

  const create = async () => {
    setError(null);
    setBusy(true);
    const datapack: DatapackChoice =
      dpMode === 'create'
        ? { mode: 'create', name: dpName.trim() }
        : { mode: 'existing', id: dpExisting!.id, name: dpExisting!.name };
    try {
      const p = await api.createProject({
        repoPath: source === 'list' ? selected!.path : source === 'folder' ? folder.trim() : undefined,
        cloneUrl: source === 'clone' ? cloneUrl.trim() : undefined,
        name: name.trim(),
        datapack,
      });
      onCreated(p);
    } catch (e) {
      setError(errorMessage(e));
    } finally {
      setBusy(false);
    }
  };

  return (
    <Dialog
      title="New project"
      wide
      onClose={onClose}
      aside={
        <span className="mono muted small dialog-aside">
          {roots.join(', ')}
          {repos ? ` · ${repos.length} repos` : ''}
        </span>
      }
      footer={
        <>
          <div className="alt-links">
            {source !== 'list' && (
              <button className="link-btn" onClick={() => setSource('list')}>
                Pick from list
              </button>
            )}
            {source !== 'clone' && (
              <button className="link-btn" onClick={() => setSource('clone')}>
                Clone from URL…
              </button>
            )}
            {source !== 'folder' && (
              <button className="link-btn" onClick={() => setSource('folder')}>
                Other folder…
              </button>
            )}
          </div>
          <button className="btn primary" disabled={!canCreate} onClick={create}>
            {busy ? (source === 'clone' ? 'Cloning…' : 'Creating…') : 'Create project'}
          </button>
        </>
      }
    >
      {!mekoOk && (
        <div className="banner warn inline">Meko is unreachable, so new projects can’t be created right now.</div>
      )}
      {source === 'list' && (
        <>
          <div className="filter-row">
            <input
              id="np-filter"
              className="input"
              placeholder="Filter repos…"
              value={filter}
              onChange={(e) => setFilter(e.target.value)}
              autoFocus
            />
            <div className="seg" role="group" aria-label="Sort">
              <button className={cx(sort === 'recent' && 'on')} onClick={() => setSort('recent')}>
                Recent
              </button>
              <button className={cx(sort === 'az' && 'on')} onClick={() => setSort('az')}>
                A–Z
              </button>
            </div>
          </div>
          <div className="repos" role="listbox" aria-label="Repositories">
            {reposError && <p className="error-text pad">{reposError}</p>}
            {!repos && !reposError && <p className="muted pad">Scanning {roots.join(', ') || 'your repos folder'}…</p>}
            {repos && shown.length === 0 && <p className="muted pad">No repos match.</p>}
            {shown.map((r) => {
              const used = !!r.projectId;
              return (
                <button
                  key={r.path}
                  role="option"
                  aria-selected={selected?.path === r.path}
                  className={cx('repo', selected?.path === r.path && 'sel', used && 'used')}
                  disabled={used}
                  onClick={() => setSelected(r)}
                  title={r.path}
                >
                  <span className="radio" />
                  <span className="nm">{r.name}</span>
                  <span className="rm">{r.remote ?? 'no remote'}</span>
                  <span className="br">{r.branch ?? '—'}</span>
                  <span className="when">{used ? `In project “${r.projectName}”` : relativeTime(r.lastTouchedAt)}</span>
                </button>
              );
            })}
          </div>
        </>
      )}
      {source === 'clone' && (
        <Field label="Git URL" hint={`Cloned into your clone folder with your existing git credentials.`}>
          <input
            id="np-clone"
            className="input mono"
            placeholder="git@github.com:org/repo.git"
            value={cloneUrl}
            onChange={(e) => setCloneUrl(e.target.value)}
            autoFocus
          />
        </Field>
      )}
      {source === 'folder' && (
        <Field label="Repo folder" hint="Any git repo on this machine. ~ is expanded.">
          <input
            id="np-folder"
            className="input mono"
            placeholder="~/work/some-repo"
            value={folder}
            onChange={(e) => setFolder(e.target.value)}
            autoFocus
          />
        </Field>
      )}

      <div className="fields">
        <Field label="Project name">
          <input
            id="np-name"
            className="input"
            value={name}
            onChange={(e) => {
              setName(e.target.value);
              setNameTouched(true);
            }}
          />
        </Field>
        <Field label="Dedicated datapack">
          <select
            id="np-dp-mode"
            className="input"
            value={dpMode}
            onChange={(e) => setDpMode(e.target.value as 'create' | 'existing')}
          >
            <option value="create">Create a new datapack</option>
            <option value="existing">Use an existing datapack</option>
          </select>
        </Field>
      </div>
      {dpMode === 'create' ? (
        <Field label="New datapack name" hint="Datapack names are unique across Meko.">
          <input
            id="np-dp-name"
            className="input mono"
            value={dpName}
            onChange={(e) => {
              setDpName(e.target.value);
              setDpNameTouched(true);
            }}
          />
        </Field>
      ) : (
        <div className="dp-picker">
          <input
            id="np-dp-filter"
            className="input"
            placeholder="Search your datapacks…"
            value={dpFilter}
            onChange={(e) => setDpFilter(e.target.value)}
          />
          <div className="dp-list" role="listbox">
            {dpError && <p className="error-text pad">{dpError}</p>}
            {!datapacks && !dpError && <p className="muted pad">Loading datapacks…</p>}
            {dpShown.map((d) => (
              <button
                key={d.id}
                role="option"
                aria-selected={dpExisting?.id === d.id}
                className={cx('dp-row', dpExisting?.id === d.id && 'sel')}
                onClick={() => setDpExisting(d)}
              >
                <span className="radio" />
                <span className="mono">{d.name}</span>
                {d.grant && <span className="tag shared">{d.grant}</span>}
              </button>
            ))}
          </div>
        </div>
      )}
      <ErrorText error={error} />
    </Dialog>
  );
}
