import { useEffect, useMemo, useState, type FormEvent } from 'react';
import type { MekoDatapack, Project } from '../../shared/types';
import { canonicalRepoName, defaultProjectDatapackName, isProjectDatapack } from '../../shared/datapacks';
import { api, errorCode, errorMessage } from '../api';
import { cx } from '../util';
import { Dialog, ErrorText, Field } from './common';

/**
 * Gives a project without one its dedicated datapack: a new one, or one of yours (for a project
 * created without Meko, or at the plan's datapack limit). New sessions use it.
 */
export function AddDatapackDialog({
  project,
  reason,
  onClose,
  onAdded,
}: {
  project: Project;
  /** Why it's asked for now (Share needs one), shown at the top. */
  reason?: string;
  onClose: () => void;
  onAdded?: (p: Project) => void;
}) {
  const [mode, setMode] = useState<'create' | 'existing'>('create');
  // The repo part is the folder's until the repo list says better; typing takes over for good.
  const folder = project.repoPath.replace(/\/+$/, '').split('/').pop() ?? '';
  const [dpName, setDpName] = useState(() => defaultProjectDatapackName(folder, project.name));
  const [nameTouched, setNameTouched] = useState(false);
  const [suggested, setSuggested] = useState<string | null>(null);
  const [datapacks, setDatapacks] = useState<MekoDatapack[] | null>(null);
  const [filter, setFilter] = useState('');
  const [chosen, setChosen] = useState<MekoDatapack | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [atLimit, setAtLimit] = useState(false);
  const [busy, setBusy] = useState(false);

  // The default name follows the repo, as in New project: adlc/proj/<repo>/<project>.
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

  useEffect(() => {
    if (suggested && !nameTouched) setDpName(suggested);
  }, [suggested, nameTouched]);

  const shown = useMemo(() => {
    const q = filter.trim().toLowerCase();
    return (datapacks ?? [])
      .filter((d) => d.grant !== 'viewer' && !project.datapacks.some((x) => x.id === d.id && x.role === 'dedicated'))
      .filter((d) => !q || d.name.toLowerCase().includes(q))
      .sort((a, b) => Number(isProjectDatapack(b.name)) - Number(isProjectDatapack(a.name)) || a.name.localeCompare(b.name))
      .slice(0, 200);
  }, [datapacks, filter, project.datapacks]);

  const add = async (e: FormEvent) => {
    e.preventDefault();
    setBusy(true);
    setError(null);
    setAtLimit(false);
    try {
      const p = await api.setDedicatedDatapack(
        project.id,
        mode === 'create' ? { mode: 'create', name: dpName.trim() } : { mode: 'existing', id: chosen!.id, name: chosen!.name },
      );
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

  return (
    <Dialog title="Add a dedicated datapack" onClose={onClose}>
      <form className="stack" onSubmit={add}>
        <p className="muted">
          {reason ?? `${project.name} has no datapack yet.`} New sessions write their memories to it; running ones keep
          what they started with.
        </p>
        <div className="seg" role="group" aria-label="Datapack">
          <button type="button" className={cx(mode === 'create' && 'on')} aria-pressed={mode === 'create'} onClick={() => setMode('create')}>
            Create a new one
          </button>
          <button type="button" className={cx(mode === 'existing' && 'on')} aria-pressed={mode === 'existing'} onClick={() => setMode('existing')}>
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
              {!datapacks && !error && <p className="muted pad">Loading datapacks…</p>}
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
                  {d.grant && <span className={cx('tag', d.grant === 'owner' && 'shared')}>{d.grant}</span>}
                </button>
              ))}
            </div>
          </div>
        )}
        <ErrorText error={error} />
        {atLimit && mode === 'create' && (
          <p className="hint">Your Meko plan has no room for another datapack: pick one you have under Use an existing one.</p>
        )}
        <div className="settings-actions">
          <button className="btn primary" disabled={busy || !ready}>
            {busy ? 'Adding…' : 'Add datapack'}
          </button>
        </div>
      </form>
    </Dialog>
  );
}
