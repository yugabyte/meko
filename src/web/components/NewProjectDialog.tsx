import { Fragment, useCallback, useEffect, useMemo, useRef, useState } from 'react';
import type { ClosedProject, DatapackChoice, DatapackInvite, MekoDatapack, Project, RepoInfo } from '../../shared/types';
import { newRepoNameProblem } from '../../shared/types';
import {
  canonicalRepoName, defaultProjectDatapackName, isProjectDatapack, matchRepo, parseProjectDatapack, remoteFromDescription, remoteRepoName,
} from '../../shared/datapacks';
import { api, errorCode, errorMessage } from '../api';
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

/** One choice in the datapack list: a datapack we can see, or an invite waiting in Meko. */
interface DpOption {
  id: string;
  name: string;
  grant: string | null;
  owner: string | null;
  inviteId: string | null;
  remote: string | null;
  /** Projects here that already use it. */
  usedBy: string[];
  /** The closed project it was the dedicated datapack of: picking it opens that project again. */
  closed: ClosedProject | null;
  group: 'invites' | 'shared' | 'yours';
}

const GROUP_LABEL: Record<DpOption['group'], string> = {
  invites: 'Invites for you',
  shared: 'Shared with you',
  yours: 'Yours',
};

function tagFor(o: DpOption): string | null {
  if (o.inviteId) return `invite${o.grant ? ` · ${o.grant}` : ''}${o.owner ? ` · ${o.owner}` : ''}`;
  if (o.grant === 'viewer') return 'viewer · read only';
  if (o.grant && o.grant !== 'owner') return `${o.grant}${o.owner ? ` · ${o.owner}` : ''}`;
  return o.grant;
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
  onCreated: (p: Project) => void;
}) {
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
  // With Meko off a datapack can't be had: the project starts without one.
  const mekoUsable = mekoConfigured;
  // A new datapack by default; opened for a share (its banner), that datapack.
  const [dpMode, setDpMode] = useState<'create' | 'existing' | 'none'>(initialInvite ? 'existing' : mekoUsable ? 'create' : 'none');
  /** Meko refused a new datapack: the plan's datapack limit is reached. */
  const [atLimit, setAtLimit] = useState(false);
  /** The mode was picked by hand, so it no longer follows Meko coming and going. */
  const [dpModeTouched, setDpModeTouched] = useState(false);
  const datapacksLoaded = useRef(false);
  const pickMode = (m: 'create' | 'existing' | 'none') => {
    setDpModeTouched(true);
    setDpMode(m);
  };
  const [dpName, setDpName] = useState('');
  const [dpNameTouched, setDpNameTouched] = useState(false);
  const [dpChosen, setDpChosen] = useState<DpOption | null>(null);
  const [dpFilter, setDpFilter] = useState('');
  const [showAll, setShowAll] = useState(false);
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

  // Until a mode is picked, it follows Meko: No datapack while it can't be used, else a new one.
  useEffect(() => {
    if (!dpModeTouched) setDpMode(initialInvite ? 'existing' : mekoUsable ? 'create' : 'none');
  }, [mekoUsable, dpModeTouched, initialInvite]);

  // The project name is typed, or filled in from a chosen project datapack. The default name for a
  // new datapack follows the chosen repo and the name until edited: adlc/proj/<repo>/<project_name>,
  // where <repo> is the upstream repo's name for a fork (canonicalRepoName). A typed folder that is
  // one of the scanned repos is named the same way as picking it from the list.
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
  const defaultDp = defaultProjectDatapackName(repoName, name);
  useEffect(() => {
    if (!dpNameTouched) setDpName(defaultDp);
  }, [defaultDp, dpNameTouched]);

  const options = useMemo<DpOption[]>(() => {
    const usedBy = new Map<string, string[]>();
    for (const p of projects) for (const d of p.datapacks) usedBy.set(d.id, [...(usedBy.get(d.id) ?? []), p.name]);
    const closed = new Map(closedProjects.map((c) => [c.datapackId, c]));
    const out: DpOption[] = [];
    const seen = new Set<string>();
    for (const i of invites) {
      if (!i.inviteId) continue;
      seen.add(i.datapackId);
      out.push({
        id: i.datapackId, name: i.name, grant: i.grant, owner: i.ownerName ?? i.ownerEmail, inviteId: i.inviteId,
        remote: i.repoRemote, usedBy: usedBy.get(i.datapackId) ?? [], closed: closed.get(i.datapackId) ?? null, group: 'invites',
      });
    }
    for (const d of datapacks ?? []) {
      if (seen.has(d.id)) continue;
      const mine = !d.grant || d.grant === 'owner';
      out.push({
        id: d.id, name: d.name, grant: d.grant, owner: mine ? null : (d.ownerName ?? d.ownerEmail), inviteId: null,
        remote: remoteFromDescription(d.description), usedBy: usedBy.get(d.id) ?? [], closed: closed.get(d.id) ?? null, group: mine ? 'yours' : 'shared',
      });
    }
    return out;
  }, [datapacks, invites, projects, closedProjects]);

  const dpShown = useMemo(() => {
    const q = dpFilter.trim().toLowerCase();
    const order: DpOption['group'][] = ['invites', 'shared', 'yours'];
    return options
      .filter((o) => (showAll || isProjectDatapack(o.name)) && (!q || o.name.toLowerCase().includes(q)))
      .sort(
        (a, b) =>
          order.indexOf(a.group) - order.indexOf(b.group) ||
          Number(a.usedBy.length > 0) - Number(b.usedBy.length > 0) ||
          a.name.localeCompare(b.name),
      )
      .slice(0, 200);
  }, [options, dpFilter, showAll]);
  const hiddenCount = options.filter((o) => !isProjectDatapack(o.name)).length;

  const choose = (o: DpOption) => {
    setDpChosen(o);
    setAutoHint(null);
    setAutoFill(o);
  };

  // Open as project: choose the datapack we were opened for once the lists are in.
  useEffect(() => {
    if (!pendingInitial || !datapacks) return;
    const o = options.find((x) => x.id === pendingInitial);
    setPendingInitial(null);
    if (!o || o.grant === 'viewer') return;
    setDpChosen(o);
    setAutoFill(o);
  }, [pendingInitial, datapacks, options]);

  // Fill in the repo and project name from the chosen datapack (once the repo list is in).
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

  // The repo picked, when it isn't the one the closed project being opened again was on.
  const pickedRepo = source === 'list' ? (selected?.path ?? null) : source === 'folder' ? folder.trim().replace(/\/+$/, '') || null : null;
  const movesFrom = dpMode === 'existing' && dpChosen?.closed && (source === 'clone' || source === 'new' || (pickedRepo && pickedRepo !== dpChosen.closed.repoPath))
    ? dpChosen.closed.repoPath
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
  const dpReady = dpMode === 'none' ? true : dpMode === 'create' ? dpName.trim().length > 0 : !!dpChosen;
  const canCreate = repoChosen && dpReady && name.trim().length > 0 && (dpMode === 'none' || mekoUsable) && !busy;
  const accepting = dpMode === 'existing' && !!dpChosen?.inviteId;

  const create = async (mode = dpMode) => {
    setError(null);
    setAtLimit(false);
    setBusy(true);
    const datapack: DatapackChoice =
      mode === 'none'
        ? { mode: 'none' }
        : mode === 'create'
          ? { mode: 'create', name: dpName.trim() }
          : { mode: 'existing', id: dpChosen!.id, name: dpChosen!.name, ...(dpChosen!.inviteId ? { inviteId: dpChosen!.inviteId } : {}) };
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
        <div className="np-dp-hd">
          <span className="label">Dedicated datapack</span>
          <div className="seg" role="group" aria-label="Dedicated datapack">
            <button
              className={cx(dpMode === 'create' && 'on')}
              aria-pressed={dpMode === 'create'}
              disabled={!mekoUsable}
              onClick={() => pickMode('create')}
            >
              Create a new one
            </button>
            <button
              className={cx(dpMode === 'existing' && 'on')}
              aria-pressed={dpMode === 'existing'}
              disabled={!mekoUsable}
              onClick={() => pickMode('existing')}
            >
              Use an existing one
            </button>
            <button className={cx(dpMode === 'none' && 'on')} aria-pressed={dpMode === 'none'} onClick={() => pickMode('none')}>
              No datapack
            </button>
          </div>
        </div>
        {dpMode === 'none' ? (
          <p className="hint">
            {mekoConfigured
              ? 'The project starts without a datapack. You can add one later from its home.'
              : 'Meko is off, so the project starts without a datapack. Add a Meko key on the Meko tab in Settings to give projects one.'}
          </p>
        ) : dpMode === 'create' ? (
          <p className="hint">Its name is at the bottom, under the repo.</p>
        ) : dpChosen ? (
          <>
            <div className="dp-list">
              <div className="dp-row sel np-dp-chosen">
                <span className="radio" />
                <span className="mono">{dpChosen.name}</span>
                {tagFor(dpChosen) && <span className={cx('tag', dpChosen.group === 'yours' && 'shared')}>{tagFor(dpChosen)}</span>}
                <button id="np-dp-change" className="btn small" onClick={() => setDpChosen(null)}>
                  Change
                </button>
              </div>
            </div>
            {autoHint && <p className={cx('hint', autoHint.ok && 'np-hint-ok')}>{autoHint.text}</p>}
            {dpChosen.usedBy.length > 0 && (
              <p className="hint">Already used by {dpChosen.usedBy.map((n) => `“${n}”`).join(', ')} here.</p>
            )}
            {dpChosen.closed && (
              <p className="hint">
                Opens the closed project “{dpChosen.closed.name}” again, with its sessions, settings and usage.
                {movesFrom && ` It was on ${movesFrom}; it moves to the repo picked below, and its old sessions stay on the old one.`}
              </p>
            )}
          </>
        ) : (
          <div className="dp-picker">
            <input
              id="np-dp-filter"
              className="input"
              placeholder="Search datapacks…"
              value={dpFilter}
              onChange={(e) => setDpFilter(e.target.value)}
              autoFocus
            />
            <div className="dp-list" role="listbox" aria-label="Datapacks">
              {dpError && <p className="error-text pad">{dpError}</p>}
              {!datapacks && !dpError && <p className="muted pad">
                <Busy label="Loading datapacks" />
              </p>}
              {datapacks && dpShown.length === 0 && (
                <p className="muted pad">{showAll ? 'No datapacks match.' : 'No adlc/proj/ datapacks match.'}</p>
              )}
              {dpShown.map((o) => {
                const head = o.group !== lastGroup;
                lastGroup = o.group;
                const viewer = o.grant === 'viewer';
                const tag = tagFor(o);
                return (
                  <Fragment key={o.id}>
                    {head && (
                      <div className="dp-group-hd" role="presentation">
                        {GROUP_LABEL[o.group]}
                      </div>
                    )}
                    <button
                      role="option"
                      aria-selected={false}
                      className="dp-row"
                      disabled={viewer}
                      onClick={() => choose(o)}
                    >
                      <span className="radio" />
                      <span className="mono">{o.name}</span>
                      {o.usedBy.length > 0 && <span className="tag shared">In “{o.usedBy[0]}”</span>}
                      {o.closed && <span className="tag shared">Closed “{o.closed.name}”</span>}
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
            <div className="np-dp-links">
              <button className="link-btn" disabled={dpLoading} onClick={loadDatapacks}>
                {dpLoading ? <Busy label="Refreshing" /> : 'Refresh'}
              </button>
              {(showAll || hiddenCount > 0) && (
                <button className="link-btn" onClick={() => setShowAll((v) => !v)}>
                  {showAll ? 'Only adlc/proj/ datapacks' : `Show all datapacks (${hiddenCount} more)`}
                </button>
              )}
            </div>
          </div>
        )}
      </div>

      <Field label="Project name">
        <input id="np-name" className="input" value={name} onChange={(e) => setName(e.target.value)} />
      </Field>

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

      {dpMode === 'create' && (
        <Field
          label="New datapack name"
          hint={
            isProjectDatapack(dpName.trim())
              ? undefined
              : 'Without the adlc/proj/ prefix, people you share it with aren’t offered it as a project.'
          }
        >
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
      )}
      <ErrorText error={error} />
      {atLimit && (
        <div className="np-limit">
          <button className="btn" disabled={busy} onClick={() => {
            pickMode('none');
            void create('none');
          }}>
            Open without a datapack
          </button>
        </div>
      )}
    </Dialog>
  );
}
