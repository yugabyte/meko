import { useCallback, useEffect, useState } from 'react';
import type { Project, RepoInfo, ReviewablePr, ReviewablePrs, Session } from '../../shared/types';
import { api, errorMessage } from '../api';
import { repoFolder, slugify } from '../util';
import { Busy, Dialog, ErrorText, Field, StartPicker, type StartChoice } from './common';
import { ReviewPrPicker } from './ReviewPrPicker';

const NAME_PLACEHOLDER: Record<StartChoice, string> = {
  explore: 'why is CI slow on macOS',
  design: 'fix retry backoff',
  code: 'fix typo in README',
  'review-pr': 'review #21',
};

/** A review's default name, from its PR. */
const reviewName = (pr: ReviewablePr) => `Review #${pr.number} ${pr.title}`.slice(0, 100).trim();

export function NewSessionDialog({
  project,
  fresh = false,
  onClose,
  onCreated,
}: {
  project: Project;
  /** The project was just opened: when it has PRs to review (a project shared with you, say), Review a PR comes first. */
  fresh?: boolean;
  onClose: () => void;
  onCreated: (s: Session) => void;
}) {
  const [name, setName] = useState('');
  const [nameEdited, setNameEdited] = useState(false);
  const [prompt, setPrompt] = useState('');
  const [start, setStart] = useState<StartChoice>('design');
  const review = start === 'review-pr';
  const [prList, setPrList] = useState<ReviewablePrs | null>(null);
  const [prListError, setPrListError] = useState<string | null>(null);
  const [picked, setPicked] = useState<ReviewablePr | null>(null);
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

  // The project's PRs to review: looked for when Review a PR is picked, or right away for a project
  // just opened, where they decide where it starts.
  const loadPrs = useCallback(() => {
    setPrListError(null);
    api
      .reviewPrs(project.id)
      .then((r) => {
        setPrList(r);
        return r;
      })
      .catch((e) => {
        setPrListError(errorMessage(e));
        return null;
      })
      .then((r) => {
        if (fresh && r?.prs.length) setStart((c) => (c === 'design' ? 'review-pr' : c));
      });
  }, [project.id, fresh]);
  const wantPrs = review || fresh;
  useEffect(() => {
    if (wantPrs && !prList && !prListError) loadPrs();
  }, [wantPrs, prList, prListError, loadPrs]);

  const pick = (pr: ReviewablePr | null) => {
    setPicked(pr);
    if (pr?.localRepo) setRepoPath(pr.localRepo);
    if (pr && !nameEdited) setName(reviewName(pr));
  };
  // A list that comes in after the type was switched to Review a PR starts on its newest PR.
  useEffect(() => {
    if (review && !picked && prList?.prs.length) pick(prList.prs[0]);
    // eslint-disable-next-line react-hooks/exhaustive-deps -- pick only when the list or type changes
  }, [review, prList]);

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
  // A review's repo is the PR's clone, which may not be listed yet (Advanced never opened, or just cloned).
  if (!repoChoices.some((r) => r.path === repoPath)) repoChoices.push({ path: repoPath, label: repoFolder(repoPath) });
  const reviewBranch = picked ? `${project.settings.branchPrefix}review-pr-${picked.number}` : '';
  const canStart = review ? !!picked?.localRepo && !busy : !!name.trim() && !busy && !!branches;
  const chooseRepo = (path: string) => {
    setRepoPath(path);
    if (path === project.repoPath) setBaseBranch(project.defaultBranch);
  };

  const create = async () => {
    setError(null);
    setBusy(true);
    try {
      const s = await api.createSession(
        project.id,
        review && picked
          ? {
            name: name.trim(),
            prompt: prompt.trim() || undefined,
            kind: 'review-pr',
            repoPath: inProjectRepo ? undefined : repoPath,
            pr: { repo: picked.repo, number: picked.number },
          }
          : {
            name: name.trim(),
            prompt: prompt.trim() || undefined,
            baseBranch,
            kind: 'task',
            start: start === 'review-pr' ? undefined : start,
            repoPath: inProjectRepo ? undefined : repoPath,
            branch: branch.trim() || undefined,
          },
      );
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
          <button className="btn primary" disabled={!canStart} onClick={create}>
            {busy ? <Busy label="Starting" /> : review ? 'Start review' : 'Start session'}
          </button>
        </>
      }
    >
      <form
        className="stack"
        onSubmit={(e) => {
          e.preventDefault();
          if (canStart) create();
        }}
      >
        <Field label="Name">
          <input
            id="ns-name"
            className="input"
            placeholder={NAME_PLACEHOLDER[start]}
            value={name}
            onChange={(e) => {
              setName(e.target.value);
              setNameEdited(true);
            }}
            autoFocus
          />
        </Field>
        <StartPicker id="ns-start" value={start} onChange={setStart} />
        {review && (
          <ReviewPrPicker
            project={project}
            list={prList}
            listError={prListError}
            onReload={() => {
              // The effect above looks again once the list is cleared.
              setPrList(null);
              setPrListError(null);
            }}
            picked={picked}
            onPick={pick}
            lookupRepo={repoPath}
          />
        )}
        <Field
          label={review ? 'Notes for AgentK (optional)' : 'First prompt (optional)'}
          hint={
            review
              ? 'Anything to focus on. AgentK sums up the PR and then waits for your questions.'
              : start === 'explore'
                ? 'What should AgentK do? You can also type it in the terminal.'
                : 'What should AgentK do? Left empty, AgentK starts on the name as the task.'
          }
        >
          <textarea
            id="ns-prompt"
            className="input"
            rows={review ? 2 : 5}
            placeholder={review ? 'e.g. the migration and the polling' : undefined}
            value={prompt}
            onChange={(e) => setPrompt(e.target.value)}
            onKeyDown={(e) => {
              if (e.key === 'Enter' && (e.metaKey || e.ctrlKey) && canStart) {
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
                {review
                  ? `${repoFolder(repoPath)}${picked ? ` · PR #${picked.number} · ${reviewBranch}` : ''}`
                  : `${repoFolder(repoPath)} · from ${baseBranch || 'its default branch'} · ${branch.trim() || autoBranch}`}
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
              {review && (
                <p className="hint">
                  The base and the branch come from the PR: AgentK checks out its latest commit on {reviewBranch || 'a review branch'}, where a
                  plain git push is refused.
                </p>
              )}
              {!review && (
                <>
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
                </>
              )}
            </div>
          )}
        </div>
        <ErrorText error={error} />
      </form>
    </Dialog>
  );
}
