import { useEffect, useMemo, useState, type FormEvent } from 'react';
import type { DatapackRef, MekoDatapack, Project, Session } from '../../shared/types';
import { canonicalRepoName, defaultProjectDatapackName, freeDatapackName, isProjectDatapack } from '../../shared/datapacks';
import { api, errorCode, errorMessage } from '../api';
import { cx, runningSessions } from '../util';
import { Busy, Dialog, ErrorText, Field } from './common';

/**
 * Gives a project without one its dedicated datapack: a new one, or one of yours (for a project
 * opened without Meko, or at the plan's datapack limit). With `replacing`, switches the project's
 * dedicated datapack to another one instead: the old one stays attached as shared, or is detached.
 * New sessions use it.
 */
export function AddDatapackDialog({
  project,
  reason,
  replacing,
  sessions = [],
  onClose,
  onAdded,
}: {
  project: Project;
  /** Why it's asked for now (Share needs one), shown at the top. */
  reason?: string;
  /** Switch: the project's dedicated datapack being replaced. */
  replacing?: DatapackRef;
  /** The project's sessions: the running ones keep the old datapack, and a switch says so. */
  sessions?: Session[];
  onClose: () => void;
  onAdded?: (p: Project) => void;
}) {
  // A switch starts on the datapacks already attached, if there are any.
  const hasShared = project.datapacks.some((d) => d.role === 'shared');
  const [mode, setMode] = useState<'create' | 'existing'>(replacing && hasShared ? 'existing' : 'create');
  const [modeTouched, setModeTouched] = useState(false);
  // The repo part is the folder's until the repo list says better; typing takes over for good.
  const folder = project.repoPath.replace(/\/+$/, '').split('/').pop() ?? '';
  const [dpName, setDpName] = useState(() => defaultProjectDatapackName(folder, project.name));
  const [nameTouched, setNameTouched] = useState(false);
  const [suggested, setSuggested] = useState<string | null>(null);
  const [datapacks, setDatapacks] = useState<MekoDatapack[] | null>(null);
  const [filter, setFilter] = useState('');
  const [chosen, setChosen] = useState<MekoDatapack | null>(null);
  const [detachOld, setDetachOld] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [atLimit, setAtLimit] = useState(false);
  const [busy, setBusy] = useState(false);

  // The default name follows the repo, as in Open project: adlc/proj/<repo>/<project>.
  useEffect(() => {
    api
      .repos()
      .then((r) => {
        // The server marks each repo with its projects by real path, so a symlinked one still matches.
        const repo = r.repos.find((x) => x.projects.some((p) => p.id === project.id));
        if (repo) setSuggested(defaultProjectDatapackName(canonicalRepoName(repo), project.name));
      })
      .catch(() => undefined);
    api
      .datapacks()
      .then(setDatapacks)
      .catch((e) => setError(errorMessage(e)));
  }, [project.id, project.name]);

  // A switch's default name is usually the old datapack's own: add -2 (or the next free number).
  const taken = useMemo(
    () => new Set([...(replacing ? [replacing.name] : []), ...(datapacks ?? []).map((d) => d.name)]),
    [replacing, datapacks],
  );
  useEffect(() => {
    if (nameTouched) return;
    const base = suggested ?? defaultProjectDatapackName(folder, project.name);
    setDpName(replacing ? freeDatapackName(base, taken) : base);
  }, [suggested, nameTouched, replacing, taken, folder, project.name]);

  const attached = useMemo(() => new Set(project.datapacks.filter((d) => d.role === 'shared').map((d) => d.id)), [project.datapacks]);
  const writable = useMemo(
    () => (datapacks ?? []).filter((d) => d.grant !== 'viewer' && !project.datapacks.some((x) => x.id === d.id && x.role === 'dedicated')),
    [datapacks, project.datapacks],
  );

  // Once the list is in: a switch with no attached datapack it could use starts on Create instead.
  useEffect(() => {
    if (replacing && datapacks && !modeTouched && !writable.some((d) => attached.has(d.id))) setMode('create');
  }, [replacing, datapacks, modeTouched, writable, attached]);

  const shown = useMemo(() => {
    const q = filter.trim().toLowerCase();
    return writable
      .filter((d) => !q || d.name.toLowerCase().includes(q))
      .sort(
        (a, b) =>
          Number(attached.has(b.id)) - Number(attached.has(a.id)) ||
          Number(isProjectDatapack(b.name)) - Number(isProjectDatapack(a.name)) ||
          a.name.localeCompare(b.name),
      )
      .slice(0, 200);
  }, [writable, filter, attached]);

  const running = replacing ? runningSessions(sessions).length : 0;

  const add = async (e: FormEvent) => {
    e.preventDefault();
    setBusy(true);
    setError(null);
    setAtLimit(false);
    try {
      const choice = mode === 'create'
        ? { mode: 'create' as const, name: dpName.trim() }
        : { mode: 'existing' as const, id: chosen!.id, name: chosen!.name };
      const p = await api.setDedicatedDatapack(project.id, replacing ? { ...choice, replace: replacing.id, detachOld } : choice);
      onAdded?.(p);
      onClose();
    } catch (err) {
      setError(errorMessage(err));
      if (errorCode(err) === 'DATAPACK_LIMIT') setAtLimit(true);
    } finally {
      setBusy(false);
    }
  };

  const ready = mode === 'create' ? !!dpName.trim() : !!chosen;
  const pick = (m: 'create' | 'existing') => {
    setMode(m);
    setModeTouched(true);
  };

  return (
    <Dialog title={replacing ? 'Switch the dedicated datapack' : 'Add a dedicated datapack'} onClose={onClose}>
      <form className="stack" onSubmit={add}>
        {replacing ? (
          <p className="muted">
            New sessions will write to the datapack you pick.{' '}
            {detachOld ? (
              <>
                <span className="mono">{replacing.name}</span> stays in Meko, but this project won’t search it any more.
              </>
            ) : (
              <>
                <span className="mono">{replacing.name}</span> stays attached as a shared datapack, so its memories can still be
                searched.
              </>
            )}
          </p>
        ) : (
          <p className="muted">
            {reason ?? `${project.name} has no datapack yet.`} New sessions write their memories to it; running ones keep
            what they started with.
          </p>
        )}
        {replacing && running > 0 && (
          <p className="banner warn inline">
            <span>
              <b>{running === 1 ? '1 session is' : `${running} sessions are`} running.</b>{' '}
              {running === 1 ? 'It will' : 'They’ll'} keep writing to <span className="mono">{replacing.name}</span> until{' '}
              {running === 1 ? 'it’s' : 'they’re'} restarted.
            </span>
          </p>
        )}
        <div className="seg" role="group" aria-label="Datapack">
          <button type="button" className={cx(mode === 'create' && 'on')} aria-pressed={mode === 'create'} onClick={() => pick('create')}>
            Create a new one
          </button>
          <button type="button" className={cx(mode === 'existing' && 'on')} aria-pressed={mode === 'existing'} onClick={() => pick('existing')}>
            Use an existing one
          </button>
        </div>
        {mode === 'create' ? (
          <Field label="New datapack name" hint="Datapack names are unique across Meko.">
            <input
              id="add-dp-name"
              className="input mono"
              value={dpName}
              onChange={(e) => {
                setDpName(e.target.value);
                setNameTouched(true);
              }}
            />
          </Field>
        ) : (
          <div className="dp-picker">
            <input id="add-dp-filter" className="input" placeholder="Search your datapacks…" value={filter} onChange={(e) => setFilter(e.target.value)} />
            <div className="dp-list" role="listbox" aria-label="Datapacks">
              {!datapacks && !error && <p className="muted pad">
                <Busy label="Loading datapacks" />
              </p>}
              {datapacks && shown.length === 0 && <p className="muted pad">No datapacks you can write to.</p>}
              {shown.map((d) => (
                <button
                  type="button"
                  key={d.id}
                  role="option"
                  aria-selected={chosen?.id === d.id}
                  className={cx('dp-row', chosen?.id === d.id && 'sel')}
                  onClick={() => setChosen(d)}
                >
                  <span className="radio" />
                  <span className="mono">{d.name}</span>
                  {attached.has(d.id) && <span className="tag">attached</span>}
                  {d.grant && <span className={cx('tag', d.grant === 'owner' && 'shared')}>{d.grant}</span>}
                </button>
              ))}
            </div>
          </div>
        )}
        {replacing && (
          <label className="check">
            <input id="add-dp-detach" type="checkbox" checked={detachOld} onChange={(e) => setDetachOld(e.target.checked)} />
            <span>
              Detach <span className="mono">{replacing.name}</span> from this project
            </span>
          </label>
        )}
        <ErrorText error={error} />
        {atLimit && mode === 'create' && (
          <p className="hint">Your Meko plan has no room for another datapack: pick one you have under Use an existing one.</p>
        )}
        <div className="settings-actions">
          <button className="btn primary" disabled={busy || !ready}>
            {replacing ? (busy ? <Busy label="Switching" /> : 'Switch datapack') : busy ? <Busy label="Adding" /> : 'Add datapack'}
          </button>
        </div>
      </form>
    </Dialog>
  );
}
