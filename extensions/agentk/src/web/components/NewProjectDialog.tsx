import { Fragment, type KeyboardEvent, useCallback, useEffect, useMemo, useRef, useState } from 'react';
import type { ClosedProject, DatapackChoice, DatapackInvite, MekoDatapack, OpenedProject, Project, RepoInfo } from '../../shared/types';
import { newRepoNameProblem } from '../../shared/types';
import { canonicalRepoName, matchRepo, parseProjectDatapack, remoteRepoName } from '../../shared/datapacks';
import { api, errorCode, errorMessage } from '../api';
import {
  type DetectedDatapack, type DpOption, GROUP_LABEL, datapackOptions, detectDatapack, projectNameOf, readOnly, searchDatapacks,
  suggestProjects, tagFor,
} from '../datapackpick';
import { useTouch } from '../hooks/useMedia';
import { cx, relativeTime } from '../util';
import { Busy, Dialog, ErrorText, Field } from './common';

type Source = 'list' | 'clone' | 'folder' | 'new';

function nameFromPath(p: string): string {
  return p.trim().replace(/\/+$/, '').split('/').pop() ?? '';
}

/** The last two folders above a repo, e.g. "…/work/client": tells same-named repos apart on a phone. */
function parentFolder(p: string): string {
  const parts = p.split('/').filter(Boolean).slice(0, -1);
  return (parts.length > 2 ? '…/' : '/') + parts.slice(-2).join('/');
}

function projectList(r: RepoInfo): string {
  return r.projects.map((p) => `“${p.name}”`).join(', ');
}

/** What the Datapack section says: which datapack the project gets, or why it gets none. */
function DatapackLine({ detected }: { detected: DetectedDatapack }) {
  switch (detected.kind) {
    case 'no-meko':
      return (
        <>
          Meko isn’t set up, so the project opens without a datapack. Start Meko on this computer (meko start), or link Meko Cloud
          in Meko’s web UI (Settings › Meko Cloud), to give projects one.
        </>
      );
    case 'none':
      return <><b>No datapack.</b> The project opens without one. You can add one later from its home.</>;
    case 'loading':
      return <Busy label="Checking Meko’s datapacks" />;
    case 'no-name':
      return <>Type a project name, or pick one of your projects. The datapack follows from the name and the repo.</>;
    case 'no-repo':
      return <><b>New datapack</b> <span className="mono">{detected.name}</span> once you pick a repo below.</>;
    case 'new':
      return <><b>New datapack</b> <span className="mono">{detected.name}</span></>;
    default: {
      const o = detected.option;
      const tag = tagFor(o);
      return (
        <>
          <b>{o.inviteId ? 'Accepts the invite to' : 'Uses'}</b> <span className="mono">{o.name}</span>
          {tag && <span className="tag shared">{tag}</span>}
        </>
      );
    }
  }
}

export function NewProjectDialog({
  mekoConfigured,
  projects,
  initialInvite,
  onClose,
  onCreated,
}: {
  /** Meko is on (a key is set). Off, a project is opened without a datapack. */
  mekoConfigured: boolean;
  projects: Project[];
  /** A share to start with chosen (Open as project on its banner). */
  initialInvite?: DatapackInvite | null;
  onClose: () => void;
  /** `datapackEmpty`: the project's datapack is empty, so a learn session is offered first. */
  onCreated: (p: OpenedProject) => void;
}) {
  // On touch, the keyboard comes up only when a field is tapped.
  const touch = useTouch();
  const [repos, setRepos] = useState<RepoInfo[] | null>(null);
  const [roots, setRoots] = useState<string[]>([]);
  const [cloneInto, setCloneInto] = useState('');
  const [reposError, setReposError] = useState<string | null>(null);
  const [filter, setFilter] = useState('');
  const [sort, setSort] = useState<'recent' | 'az'>('recent');
  const [source, setSource] = useState<Source>('list');
  const [selected, setSelected] = useState<RepoInfo | null>(null);
  const [cloneUrl, setCloneUrl] = useState('');
  const [folder, setFolder] = useState('');
  const [name, setName] = useState('');
  // A new repo's folder name, always typed by the user (never filled in from the project name).
  const [newName, setNewName] = useState('');

  const [datapacks, setDatapacks] = useState<MekoDatapack[] | null>(null);
  const [invites, setInvites] = useState<DatapackInvite[]>(initialInvite ? [initialInvite] : []);
  const [closedProjects, setClosedProjects] = useState<ClosedProject[]>([]);
  const [dpError, setDpError] = useState<string | null>(null);
  const [dpLoading, setDpLoading] = useState(false);
  // With no Meko a datapack can't be had: the project starts without one.
  const mekoUsable = mekoConfigured;
  /** Meko refused a new datapack: the plan's datapack limit is reached. */
  const [atLimit, setAtLimit] = useState(false);
  const datapacksLoaded = useRef(false);
  /** No datapack, ticked by hand. */
  const [noDatapack, setNoDatapack] = useState(false);
  /**
   * A datapack picked from Project name's dropdown (`name`: editing the name drops it) or from
   * Search all datapacks (`search`: it stays, and never changes the name or the repo).
   */
  const [picked, setPicked] = useState<{ option: DpOption; from: 'name' | 'search' } | null>(null);
  /** Project name's dropdown is open. */
  const [suggesting, setSuggesting] = useState(false);
  const [highlight, setHighlight] = useState(-1);
  /** Search all datapacks' text, or null while the search is closed. */
  const [dpSearch, setDpSearch] = useState<string | null>(null);
  /** A datapack just chosen whose repo and name are still to be filled in (waits for the repo list). */
  const [autoFill, setAutoFill] = useState<DpOption | null>(null);
  const [autoHint, setAutoHint] = useState<{ ok: boolean; text: string } | null>(null);
  const [pendingInitial, setPendingInitial] = useState(initialInvite?.datapackId ?? null);

  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);

  const loadDatapacks = useCallback(() => {
    setDpLoading(true);
    setDpError(null);
    // One call: the server lists the datapacks once and syncs the shares with that same list.
    api
      .checkInvites()
      .then((r) => {
        setDatapacks(r.datapacks);
        setInvites(r.invites);
        setClosedProjects(r.closed);
      })
      .catch((e) => setDpError(errorMessage(e)))
      .finally(() => setDpLoading(false));
  }, []);

  useEffect(() => {
    api
      .repos()
      .then((r) => {
        setRepos(r.repos);
        setRoots(r.roots);
        setCloneInto(r.cloneInto);
      })
      .catch((e) => setReposError(errorMessage(e)));
  }, []);

  // Datapacks load once Meko can be used (it may come back while the dialog is open).
  useEffect(() => {
    if (mekoUsable && !datapacksLoaded.current) {
      datapacksLoaded.current = true;
      loadDatapacks();
    }
  }, [loadDatapacks, mekoUsable]);

  // The project name is typed, or filled in from a project picked in its dropdown. With nothing
  // picked, the datapack is the one named adlc/proj/<repo>/<project_name>, where <repo> is the
  // upstream repo's name for a fork (canonicalRepoName). A typed folder that is one of the scanned
  // repos is named the same way as picking it from the list.
  const folderRepo = source === 'folder' ? (repos?.find((r) => r.path === folder.trim().replace(/\/+$/, '')) ?? null) : null;
  const repoName =
    source === 'list'
      ? selected
        ? canonicalRepoName(selected)
        : ''
      : source === 'clone'
        ? remoteRepoName(cloneUrl)
        : source === 'new'
          ? newName.trim()
          : folderRepo
          ? canonicalRepoName(folderRepo)
          : nameFromPath(folder);
  const options = useMemo(
    () => datapackOptions(datapacks ?? [], invites, projects, closedProjects),
    [datapacks, invites, projects, closedProjects],
  );
  const suggestions = useMemo(
    () => (mekoUsable && suggesting && !noDatapack && dpSearch === null ? suggestProjects(name, options) : []),
    [mekoUsable, suggesting, noDatapack, dpSearch, name, options],
  );
  const searched = useMemo(() => (dpSearch === null ? [] : searchDatapacks(dpSearch, options)), [dpSearch, options]);

  /** From the dropdown: the name, the datapack and the repo. From the search: only the datapack. */
  const choose = (o: DpOption, from: 'name' | 'search') => {
    if (readOnly(o)) return;
    setPicked({ option: o, from });
    setNoDatapack(false);
    setDpSearch(null);
    setSuggesting(false);
    setHighlight(-1);
    if (from === 'name') {
      setAutoHint(null);
      setAutoFill(o);
    }
  };

  // Open as project: choose the datapack we were opened for once the lists are in.
  useEffect(() => {
    if (!pendingInitial || !datapacks) return;
    const o = options.find((x) => x.id === pendingInitial);
    setPendingInitial(null);
    if (!o || readOnly(o)) return;
    setPicked({ option: o, from: 'name' });
    setAutoFill(o);
  }, [pendingInitial, datapacks, options]);

  // Fill in the repo and project name from the project picked in the dropdown (once the repo list is in).
  useEffect(() => {
    if (!autoFill || !repos) return;
    const o = autoFill;
    setAutoFill(null);
    // A closed project's datapack: its own name and repo.
    if (o.closed) {
      const c = o.closed;
      setName(c.name);
      const repo = repos.find((r) => r.path === c.repoPath);
      if (repo) {
        setSource('list');
        setSelected(repo);
        setFilter('');
      } else {
        setSource('folder');
        setFolder(c.repoPath);
      }
      setAutoHint({ ok: true, text: `Name and repo set to those of “${c.name}”.` });
      return;
    }
    const parsed = parseProjectDatapack(o.name);
    if (!parsed) return;
    setName(parsed.project);
    const m = matchRepo(repos, o.name, o.remote);
    const where = roots.join(', ') || 'your repos folder';
    if (m.repo) {
      setSource('list');
      setSelected(m.repo);
      setFilter('');
      const why =
        m.by === 'upstream'
          ? `because its upstream matches the datapack's description`
          : m.by === 'remote'
            ? `because its origin matches the datapack's description`
            : `from the datapack name`;
      const more = m.count > 1 ? ` ${m.count} repos match; this one was used most recently.` : '';
      setAutoHint({ ok: true, text: `Repo ${m.repo.name} picked ${why}. Name set to ${parsed.project}.${more}` });
    } else if (o.remote) {
      setSource('clone');
      setCloneUrl(o.remote);
      setAutoHint({ ok: false, text: `No repo for this datapack under ${where}, so its remote is filled in to clone.` });
    } else {
      setSelected(null);
      setAutoHint({ ok: false, text: `No repo named ${parsed.repo} under ${where}. Pick one or clone it.` });
    }
  }, [autoFill, repos, roots]);

  const shown = useMemo(() => {
    if (!repos) return [];
    const q = filter.trim().toLowerCase();
    const list = repos.filter(
      (r) =>
        !q ||
        r.name.toLowerCase().includes(q) ||
        (r.remote ?? '').toLowerCase().includes(q) ||
        (r.upstream ?? '').toLowerCase().includes(q),
    );
    return list.sort((a, b) => (sort === 'az' ? a.name.localeCompare(b.name) : b.lastTouchedAt - a.lastTouchedAt));
  }, [repos, filter, sort]);

  const detected = detectDatapack({
    mekoUsable, noDatapack, picked: picked?.option ?? null, name, repoName,
    // A list that failed to load doesn't hold Open project up: the default name is tried, and Meko refuses a taken one.
    options: datapacks || dpError ? options : null,
  });
  const dpUsed = detected.kind === 'picked' || detected.kind === 'found' ? detected.option : null;

  // The repo picked, when it isn't the one the closed project being opened again was on.
  const pickedRepo = source === 'list' ? (selected?.path ?? null) : source === 'folder' ? folder.trim().replace(/\/+$/, '') || null : null;
  const movesFrom = dpUsed?.closed && (source === 'clone' || source === 'new' || (pickedRepo && pickedRepo !== dpUsed.closed.repoPath))
    ? dpUsed.closed.repoPath
    : null;

  // Checked as typed, as the server checks it; an empty name only keeps Open project off.
  const newProblem = newRepoNameProblem(newName);
  const repoChosen =
    source === 'list'
      ? !!selected
      : source === 'clone'
        ? cloneUrl.trim().length > 0
        : source === 'new'
          ? !newProblem
          : folder.trim().length > 0;
  const canCreate = repoChosen && !!detected.choice && name.trim().length > 0 && !busy;
  const accepting = !!dpUsed?.inviteId;

  const create = async (datapack: DatapackChoice | null = detected.choice) => {
    if (!datapack) return;
    setError(null);
    setAtLimit(false);
    setBusy(true);
    try {
      const p = await api.openProject({
        repoPath: source === 'list' ? selected!.path : source === 'folder' ? folder.trim() : undefined,
        cloneUrl: source === 'clone' ? cloneUrl.trim() : undefined,
        newRepo: source === 'new' ? newName.trim() : undefined,
        name: name.trim(),
        datapack,
      });
      onCreated(p);
    } catch (e) {
      setError(errorMessage(e));
      if (errorCode(e) === 'DATAPACK_LIMIT') setAtLimit(true);
    } finally {
      setBusy(false);
    }
  };

  const editName = (v: string) => {
    setName(v);
    setSuggesting(true);
    setHighlight(-1);
    // A project picked in the dropdown goes with its name; a datapack picked in the search stays.
    if (picked?.from === 'name') {
      setPicked(null);
      setAutoHint(null);
    }
  };
  const nameKey = (e: KeyboardEvent<HTMLInputElement>) => {
    if (!suggestions.length) return;
    if (e.key === 'ArrowDown' || e.key === 'ArrowUp') {
      e.preventDefault();
      setHighlight((h) => (e.key === 'ArrowDown' ? Math.min(suggestions.length - 1, h + 1) : Math.max(-1, h - 1)));
    } else if (e.key === 'Enter' && highlight >= 0 && suggestions[highlight]) {
      e.preventDefault();
      choose(suggestions[highlight], 'name');
    }
  };
  let lastGroup: DpOption['group'] | null = null;

  return (
    <Dialog
      title="Open project"
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
            {source !== 'new' && (
              <button className="link-btn" onClick={() => setSource('new')}>
                New repo…
              </button>
            )}
          </div>
          <button className="btn primary" disabled={!canCreate} onClick={() => void create()}>
            {busy
              ? accepting
                ? <Busy label="Accepting" />
                : source === 'clone'
                  ? <Busy label="Cloning" />
                  : source === 'new'
                    ? <Busy label="Creating" />
                    : <Busy label="Opening" />
              : accepting
                ? 'Accept and open project'
                : 'Open project'}
          </button>
        </>
      }
    >

      <div className="np-section">
        <label className="label" htmlFor="np-name">
          Project name
        </label>
        <div className="np-combo">
          <input
            id="np-name"
            className="input"
            autoComplete="off"
            role="combobox"
            aria-autocomplete="list"
            aria-controls="np-suggest"
            aria-expanded={suggestions.length > 0}
            aria-activedescendant={highlight >= 0 ? `np-suggest-${highlight}` : undefined}
            value={name}
            onChange={(e) => editName(e.target.value)}
            onFocus={() => setSuggesting(true)}
            onBlur={() => {
              setSuggesting(false);
              setHighlight(-1);
            }}
            onKeyDown={nameKey}
          />
          {suggestions.length > 0 && (
            // Clicks here keep the name field's focus, so its blur doesn't close the dropdown before they land.
            <div id="np-suggest" className="dp-list np-suggest" role="listbox" aria-label="Your projects" onMouseDown={(e) => e.preventDefault()}>
              {suggestions.map((o, i) => (
                <button
                  key={o.id}
                  id={`np-suggest-${i}`}
                  role="option"
                  tabIndex={-1}
                  aria-selected={dpUsed?.id === o.id}
                  className={cx('dp-row', (i === highlight || dpUsed?.id === o.id) && 'sel')}
                  disabled={readOnly(o)}
                  onClick={() => choose(o, 'name')}
                >
                  <span className="np-suggest-name">{projectNameOf(o)}</span>
                  <span className="muted">·</span>
                  <span className="mono">{o.name}</span>
                  {readOnly(o) && <span className="tag shared">read only</span>}
                </button>
              ))}
            </div>
          )}
        </div>
        {autoHint && <p className={cx('hint', autoHint.ok && 'np-hint-ok')}>{autoHint.text}</p>}
      </div>

      <div className="np-section">
        <span className="label">Datapack</span>
        <div className={cx('np-dp-line', !dpUsed && detected.kind !== 'new' && 'off')} aria-live="polite">
          <span className="np-dp-what">
            <DatapackLine detected={detected} />
          </span>
          {picked && detected.kind === 'picked' && (
            <button className="icon-btn" aria-label="Drop the picked datapack" title="Drop the picked datapack" onClick={() => setPicked(null)}>
              ×
            </button>
          )}
        </div>
        {detected.kind === 'new' && detected.readOnly && (
          <p className="hint">You can only read {detected.readOnly}, so the new datapack gets the next free name.</p>
        )}
        {dpUsed && dpUsed.usedBy.length > 0 && <p className="hint">Already used by {dpUsed.usedBy.map((n) => `“${n}”`).join(', ')} here.</p>}
        {movesFrom && (
          <p className="hint">“{dpUsed!.closed!.name}” was on {movesFrom}. It moves to the repo picked below, and its old sessions stay on the old one.</p>
        )}
        {dpError && <p className="error-text">{dpError}</p>}
        {dpSearch !== null && (
          <div className="dp-picker">
            <input
              id="np-dp-search"
              className="input"
              autoComplete="off"
              aria-label="Search all datapacks"
              placeholder="Search all datapacks by any part of the name"
              value={dpSearch}
              onChange={(e) => setDpSearch(e.target.value)}
              autoFocus={!touch}
            />
            <div className="dp-list" role="listbox" aria-label="All datapacks">
              {!datapacks && !dpError && <p className="muted pad">
                <Busy label="Loading datapacks" />
              </p>}
              {datapacks && searched.length === 0 && <p className="muted pad">No datapacks match.</p>}
              {searched.slice(0, 200).map((o) => {
                const head = o.group !== lastGroup;
                lastGroup = o.group;
                const viewer = readOnly(o);
                const tag = tagFor(o);
                return (
                  <Fragment key={o.id}>
                    {head && (
                      <div className="dp-group-hd" role="presentation">
                        {GROUP_LABEL[o.group]}
                      </div>
                    )}
                    <button role="option" aria-selected={dpUsed?.id === o.id} className={cx('dp-row', dpUsed?.id === o.id && 'sel')} disabled={viewer} onClick={() => choose(o, 'search')}>
                      <span className="radio" />
                      <span className="mono">{o.name}</span>
                      {o.usedBy.length > 0 && <span className="tag shared">In “{o.usedBy[0]}”</span>}
                      {tag && <span className={cx('tag', (o.group === 'yours' || viewer) && 'shared')}>{tag}</span>}
                      {viewer && (
                        <span className="dp-row-note">
                          Sessions can’t write to it, so it can’t be a project’s own datapack. Attach it from a project’s home.
                        </span>
                      )}
                    </button>
                  </Fragment>
                );
              })}
            </div>
          </div>
        )}
        <div className="np-dp-links">
          <label className="check">
            <input
              id="np-no-dp"
              type="checkbox"
              checked={!mekoUsable || noDatapack}
              disabled={!mekoUsable}
              onChange={(e) => {
                setNoDatapack(e.target.checked);
                if (e.target.checked) {
                  setPicked(null);
                  setDpSearch(null);
                }
              }}
            />
            No datapack
          </label>
          {mekoUsable && (dpSearch === null ? (
            <button className="link-btn" onClick={() => {
              setNoDatapack(false);
              setDpSearch('');
            }}>
              Search all datapacks
            </button>
          ) : (
            <button className="link-btn" onClick={() => setDpSearch(null)}>
              Close the search
            </button>
          ))}
          {mekoUsable && (
            <button className="link-btn" disabled={dpLoading} onClick={loadDatapacks}>
              {dpLoading ? <Busy label="Refreshing" /> : 'Refresh'}
            </button>
          )}
        </div>
      </div>

      <div className="np-section">
        <span className="label">Repo</span>
        {source === 'list' && (
          <>
            <div className="filter-row">
              <input
                id="np-filter"
                className="input"
                placeholder="Filter repos…"
                value={filter}
                onChange={(e) => setFilter(e.target.value)}
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
              {!repos && !reposError && <p className="muted pad">
                  <Busy label={`Scanning ${roots.join(', ') || 'your repos folder'}`} />
                </p>}
              {repos && shown.length === 0 && <p className="muted pad">No repos match.</p>}
              {shown.map((r) => {
                const used = r.projects.length > 0;
                return (
                  <button
                    key={r.path}
                    role="option"
                    aria-selected={selected?.path === r.path}
                    className={cx('repo', selected?.path === r.path && 'sel', used && 'used')}
                    onClick={() => setSelected(r)}
                    title={used ? `${r.path}\nIn ${projectList(r)}` : r.path}
                  >
                    <span className="radio" />
                    <span className="nm">
                      {r.name}
                      <span className="pf">{parentFolder(r.path)}</span>
                    </span>
                    <span className="rm">{r.remote ?? 'no remote'}</span>
                    <span className="br">{r.branch ?? '—'}</span>
                    <span className="when">
                      {!used
                        ? relativeTime(r.lastTouchedAt)
                        : r.projects.length === 1
                          ? `In “${r.projects[0].name}”`
                          : `In ${r.projects.length} projects`}
                    </span>
                  </button>
                );
              })}
            </div>
            {selected && selected.projects.length > 0 && (
              <p className="hint repo-note">
                This repo already has {selected.projects.length === 1 ? 'project' : 'projects'} {projectList(selected)}. The
                project you open gets its own datapack, sessions and worktrees. Branches share the repo, so a clashing
                branch name gets a number added.
              </p>
            )}
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
            />
          </Field>
        )}
        {source === 'new' && (
          <Field
            label="Repo name"
            hint={
              newName.trim() && newProblem ? (
                <span className="error-text">{newProblem}</span>
              ) : (
                `Created in ${(cloneInto || '~/code').replace(/\/+$/, '')}/${newName.trim() || '…'} with git init and an empty first commit. It has no remote, so add one before opening a PR.`
              )
            }
          >
            <input
              id="np-new"
              className="input mono"
              placeholder="my-new-repo"
              value={newName}
              onChange={(e) => setNewName(e.target.value)}
            />
          </Field>
        )}
      </div>

      <ErrorText error={error} />
      {atLimit && (
        <div className="np-limit">
          <button className="btn" disabled={busy} onClick={() => {
            setNoDatapack(true);
            setPicked(null);
            void create({ mode: 'none' });
          }}>
            Open without a datapack
          </button>
        </div>
      )}
    </Dialog>
  );
}
