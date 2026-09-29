import { useEffect, useRef, useState, type KeyboardEvent } from 'react';
import { KIND_HINTS, KIND_LABELS, KIND_STAGES, SESSION_KINDS, STAGE_LABELS } from '../../shared/types';
import type { ProjectWithSessions, RepoInfo, Session, SessionKind } from '../../shared/types';
import { api, errorMessage } from '../api';
import { cx, repoFolder, slugify } from '../util';
import { Dialog, ErrorText, Field } from './common';

const NAME_PLACEHOLDER: Record<SessionKind, string> = {
  explore: 'why is CI slow on macOS',
  plan: 'fix retry backoff',
  quick: 'fix typo in README',
};

/** First and last stage, e.g. "Design → Merge & Close", or the only one. */
function stageSpan(kind: SessionKind): string {
  const st = KIND_STAGES[kind];
  return st.length === 1 ? STAGE_LABELS[st[0]] : `${STAGE_LABELS[st[0]]} → ${STAGE_LABELS[st[st.length - 1]]}`;
}

/** The session types as boxes side by side, a radio group (arrow keys move the choice). */
function KindPicker({ value, onChange }: { value: SessionKind; onChange: (k: SessionKind) => void }) {
  const refs = useRef<Partial<Record<SessionKind, HTMLButtonElement | null>>>({});
  const onKey = (e: KeyboardEvent<HTMLDivElement>) => {
    const step = e.key === 'ArrowRight' || e.key === 'ArrowDown' ? 1 : e.key === 'ArrowLeft' || e.key === 'ArrowUp' ? -1 : 0;
    if (!step) return;
    e.preventDefault();
    const next = SESSION_KINDS[(SESSION_KINDS.indexOf(value) + step + SESSION_KINDS.length) % SESSION_KINDS.length];
    onChange(next);
    refs.current[next]?.focus();
  };
  return (
    <div className="field">
      <span className="label" id="ns-kind-label">
        Session type
      </span>
      <div className="kinds" role="radiogroup" aria-labelledby="ns-kind-label" aria-describedby="ns-kind-hint" onKeyDown={onKey}>
        {SESSION_KINDS.map((k) => (
          <button
            key={k}
            id={`ns-kind-${k}`}
            ref={(el) => {
              refs.current[k] = el;
            }}
            type="button"
            role="radio"
            aria-checked={k === value}
            tabIndex={k === value ? 0 : -1}
            className={cx('kind', k === value && 'on')}
            onClick={() => onChange(k)}
          >
            <b>{KIND_LABELS[k]}</b>
            <span className="kind-stages">{stageSpan(k)}</span>
          </button>
        ))}
      </div>
      <span className="hint" id="ns-kind-hint">
        {KIND_HINTS[value]}
      </span>
    </div>
  );
}

export function NewSessionDialog({
  project,
  onClose,
  onCreated,
}: {
  project: ProjectWithSessions;
  onClose: () => void;
  onCreated: (s: Session) => void;
}) {
  const [name, setName] = useState('');
  const [prompt, setPrompt] = useState('');
  const [kind, setKind] = useState<SessionKind>('plan');
  const [advanced, setAdvanced] = useState(false);
  const [repos, setRepos] = useState<RepoInfo[] | null>(null);
  const [reposError, setReposError] = useState<string | null>(null);
  const [repoPath, setRepoPath] = useState(project.repoPath);
  const [branches, setBranches] = useState<string[] | null>(null);
  const [baseBranch, setBaseBranch] = useState(project.defaultBranch);
  const [branch, setBranch] = useState('');
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  const inProjectRepo = repoPath === project.repoPath;

  // The chosen repo's branches; picking another repo starts from its default branch.
  useEffect(() => {
    let live = true;
    setBranches(null);
    const load = inProjectRepo
      ? api.branches(project.id).then((b) => ({ defaultBranch: project.defaultBranch, branches: b }))
      : api.repoBranches(repoPath);
    load
      .then((r) => {
        if (!live) return;
        setBranches(r.branches.includes(r.defaultBranch) ? r.branches : [r.defaultBranch, ...r.branches]);
        if (!inProjectRepo) setBaseBranch(r.defaultBranch);
      })
      .catch(() => {
        if (!live) return;
        // Left empty, the server starts from the repo's default branch.
        setBranches(inProjectRepo ? [project.defaultBranch] : []);
        if (!inProjectRepo) setBaseBranch('');
      });
    return () => {
      live = false;
    };
  }, [project.id, project.defaultBranch, repoPath, inProjectRepo]);

  // The scanned repos load the first time Advanced opens.
  useEffect(() => {
    if (!advanced || repos || reposError) return;
    api
      .repos()
      .then((r) => setRepos(r.repos))
      .catch((e) => setReposError(errorMessage(e)));
  }, [advanced, repos, reposError]);

  const autoBranch = `${project.settings.branchPrefix}${slugify(name) || 'task-name'}`;
  const repoChoices = [
    { path: project.repoPath, label: `${repoFolder(project.repoPath)} (project repo)` },
    ...(repos ?? [])
      .filter((r) => r.path !== project.repoPath && !r.projects.some((x) => x.id === project.id))
      .map((r) => ({ path: r.path, label: r.name })),
  ];
  const chooseRepo = (path: string) => {
    setRepoPath(path);
    if (path === project.repoPath) setBaseBranch(project.defaultBranch);
  };

  const create = async () => {
    setError(null);
    setBusy(true);
    try {
      const s = await api.createSession(project.id, {
        name: name.trim(),
        prompt: prompt.trim() || undefined,
        baseBranch,
        kind,
        repoPath: inProjectRepo ? undefined : repoPath,
        branch: branch.trim() || undefined,
      });
      onCreated(s);
    } catch (e) {
      setError(errorMessage(e));
    } finally {
      setBusy(false);
    }
  };

  return (
    <Dialog
      title={`New session in ${project.name}`}
      onClose={onClose}
      footer={
        <>
          <button className="btn primary" disabled={!name.trim() || busy || !branches} onClick={create}>
            {busy ? 'Starting…' : 'Start session'}
          </button>
        </>
      }
    >
      <form
        className="stack"
        onSubmit={(e) => {
          e.preventDefault();
          if (name.trim() && !busy && branches) create();
        }}
      >
        <Field label="Name">
          <input
            id="ns-name"
            className="input"
            placeholder={NAME_PLACEHOLDER[kind]}
            value={name}
            onChange={(e) => setName(e.target.value)}
            autoFocus
          />
        </Field>
        <KindPicker value={kind} onChange={setKind} />
        <Field
          label="First prompt (optional)"
          hint={
            kind === 'explore'
              ? 'What should AgentK do? You can also type it in the terminal.'
              : 'What should AgentK do? Left empty, AgentK starts on the name as the task.'
          }
        >
          <textarea
            id="ns-prompt"
            className="input"
            rows={5}
            value={prompt}
            onChange={(e) => setPrompt(e.target.value)}
            onKeyDown={(e) => {
              if (e.key === 'Enter' && (e.metaKey || e.ctrlKey) && name.trim() && !busy && branches) {
                e.preventDefault();
                create();
              }
            }}
          />
        </Field>
        <div className="advanced">
          <button
            type="button"
            id="ns-advanced"
            className="advanced-toggle"
            aria-expanded={advanced}
            aria-controls="ns-advanced-body"
            onClick={() => setAdvanced((v) => !v)}
          >
            <span className="advanced-caret" aria-hidden="true">
              {advanced ? '▾' : '▸'}
            </span>
            <b>Advanced</b>
            {!advanced && (
              <span className="advanced-summary mono">
                {repoFolder(repoPath)} · from {baseBranch || 'its default branch'} · {branch.trim() || autoBranch}
              </span>
            )}
          </button>
          {advanced && (
            <div className="advanced-body stack" id="ns-advanced-body">
              <Field
                label="Repo"
                hint={
                  reposError
                    ? `Couldn't list the other repos: ${reposError}`
                    : inProjectRepo
                      ? undefined
                      : `The project's repo is ${repoFolder(project.repoPath)}. This session keeps the project's datapack and settings.`
                }
              >
                <select id="ns-repo" className="input" value={repoPath} onChange={(e) => chooseRepo(e.target.value)}>
                  {repoChoices.map((r) => (
                    <option key={r.path} value={r.path}>
                      {r.label}
                    </option>
                  ))}
                </select>
              </Field>
              <Field label="Base branch">
                <select
                  id="ns-base"
                  className="input"
                  value={baseBranch}
                  disabled={!branches}
                  onChange={(e) => setBaseBranch(e.target.value)}
                >
                  {(branches ?? [baseBranch]).map((b) => (
                    <option key={b} value={b}>
                      {b}
                    </option>
                  ))}
                  {branches?.length === 0 && <option value="">The repo's default branch</option>}
                </select>
              </Field>
              <Field
                label="Branch"
                hint="Left empty, AgentK names it from the session name. An existing branch is picked up as it is, unless a running session uses it."
              >
                <input
                  id="ns-branch"
                  className="input mono"
                  placeholder={autoBranch}
                  value={branch}
                  autoCapitalize="off"
                  autoCorrect="off"
                  spellCheck={false}
                  onChange={(e) => setBranch(e.target.value)}
                />
              </Field>
            </div>
          )}
        </div>
        <ErrorText error={error} />
      </form>
    </Dialog>
  );
}
