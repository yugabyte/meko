import { useCallback, useEffect, useRef, useState } from 'react';
import { MAX_PASTE_BYTES, MAX_START_IMAGES, START_EXAMPLES, type CreateSessionRequest, type Project, type RepoInfo, type ReviewablePr, type ReviewablePrs, type Session, type TaskPr } from '../../shared/types';
import { expandBranchPrefix } from '../../shared/branches';
import { api, errorMessage } from '../api';
import { pastedImages } from '../clipboard';
import { HeldImages as HeldImageList } from '../heldImages';
import { cx, repoFolder, slugify } from '../util';
import { AttachPrPicker } from './AttachPrPicker';
import { Busy, Dialog, ErrorText, Field, StartPicker, type StartChoice } from './common';
import { HeldImages } from './HeldImages';
import { ReviewPrPicker } from './ReviewPrPicker';

/** A review's default name, from its PR. */
const reviewName = (pr: ReviewablePr) => `Review #${pr.number} ${pr.title}`.slice(0, 100).trim();

/** What a task works on (Advanced → Work on): a new branch from a base, an existing branch, or an open PR's branch. */
type WorkOn = 'new' | 'branch' | 'pr';
const WORK_ON: { value: WorkOn; label: string }[] = [
  { value: 'new', label: 'New branch' },
  { value: 'branch', label: 'Existing branch' },
  { value: 'pr', label: 'Pull request' },
];

export function NewSessionDialog({
  project,
  ghLogin,
  fresh = false,
  initial,
  onClose,
  onCreated,
}: {
  project: Project;
  /** The login gh is signed in as, for `{login}` in the branch prefix. */
  ghLogin: string | null;
  /** The project was just opened: when it has PRs to review (a project shared with you, say), Review a PR comes first. */
  fresh?: boolean;
  /** What a failed start asked for, to try it again. */
  initial?: CreateSessionRequest;
  onClose: () => void;
  onCreated: (s: Session) => void;
}) {
  const [name, setName] = useState(initial?.name ?? '');
  const [nameEdited, setNameEdited] = useState(!!initial?.name);
  const [prompt, setPrompt] = useState(initial?.prompt ?? '');
  const [start, setStart] = useState<StartChoice>(initial?.kind === 'review-pr' ? 'review-pr' : (initial?.start ?? 'design'));
  // A retry starts where the failed start asked to.
  const startPicked = useRef(!!initial?.start);
  const review = start === 'review-pr';
  const [workOn, setWorkOn] = useState<WorkOn>(initial?.kind !== 'review-pr' && initial?.pr ? 'pr' : initial?.existingBranch ? 'branch' : 'new');
  const [existing, setExisting] = useState(initial?.existingBranch ? (initial.branch ?? '') : '');
  const [taskPr, setTaskPr] = useState<TaskPr | null>(null);
  const [prList, setPrList] = useState<ReviewablePrs | null>(null);
  const [prListError, setPrListError] = useState<string | null>(null);
  const [picked, setPicked] = useState<ReviewablePr | null>(null);
  // A retry opens Advanced when it asked for more than the defaults there.
  const [advanced, setAdvanced] = useState(
    !!initial && (!!initial.repoPath || !!initial.branch || (initial.kind !== 'review-pr' && !!initial.pr)
      || (!!initial.baseBranch && initial.baseBranch !== project.defaultBranch)),
  );
  const [repos, setRepos] = useState<RepoInfo[] | null>(null);
  const [reposError, setReposError] = useState<string | null>(null);
  const [repoPath, setRepoPath] = useState(initial?.repoPath ?? project.repoPath);
  const [branches, setBranches] = useState<string[] | null>(null);
  const [baseBranch, setBaseBranch] = useState(initial?.baseBranch ?? project.defaultBranch);
  const [branch, setBranch] = useState(initial?.existingBranch ? '' : (initial?.branch ?? ''));
  // A retry's base branch outlasts the first load of another repo's branches.
  const keepBase = useRef(initial?.baseBranch);
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  const inProjectRepo = repoPath === project.repoPath;

  // Images for the first prompt (a review's notes): each uploads as it's added, kept under the
  // project until the session is created with it.
  const [, setHeldChanged] = useState(0);
  const [held] = useState(
    () =>
      new HeldImageList<Blob>({
        upload: async (image) => (await api.stageImage(project.id, image)).id,
        thumb: (image) => URL.createObjectURL(image),
        revoke: (url) => URL.revokeObjectURL(url),
        onChange: () => setHeldChanged((n) => n + 1),
        onError: (e) => setError(`Couldn't add the image: ${errorMessage(e)}`),
      }),
  );
  // The thumbnails go with the dialog.
  useEffect(() => () => held.clear(), [held]);
  const imagePicker = useRef<HTMLInputElement>(null);
  const hold = (images: Blob[]) => {
    if (!images.length) return;
    if (images.some((i) => i.size > MAX_PASTE_BYTES)) {
      setError(`Couldn't add ${images.length === 1 ? 'the image' : 'the images'}: images over ${MAX_PASTE_BYTES / 1024 / 1024} MB can't be pasted.`);
      return;
    }
    if (held.size + images.length > MAX_START_IMAGES) {
      setError(`A session can start with at most ${MAX_START_IMAGES} images.`);
      return;
    }
    setError(null);
    held.add(images);
  };
  // A failed start's images are removed with it, so Try again can't send them.
  const lostImages = held.size ? 0 : (initial?.images?.length ?? 0);

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
        const kept = keepBase.current;
        keepBase.current = undefined;
        // A retry's base that isn't there (the reason it failed, say) goes back to the default.
        if (!inProjectRepo || (kept && !r.branches.includes(kept))) setBaseBranch(kept && r.branches.includes(kept) ? kept : r.defaultBranch);
      })
      .catch(() => {
        if (!live) return;
        keepBase.current = undefined;
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

  // The PR a retry was for, until something is picked.
  const retryPr = useRef(initial?.pr);
  const pick = (pr: ReviewablePr | null) => {
    retryPr.current = undefined;
    setPicked(pr);
    if (pr?.localRepo) setRepoPath(pr.localRepo);
    if (pr && !nameEdited) setName(reviewName(pr));
  };
  // A list that comes in after the type was switched to Review a PR starts on its newest PR. A
  // retry starts on the PR it was for: from the list, or looked up as one typed by number or link
  // (never another PR in its place).
  useEffect(() => {
    if (!review || picked || !prList) return;
    const want = retryPr.current;
    if (!want) {
      if (prList.prs.length) pick(prList.prs[0]);
      return;
    }
    const listed = prList.prs.find((p) => p.repo === want.repo && p.number === want.number);
    if (listed) {
      pick(listed);
      return;
    }
    let live = true;
    api
      .reviewPrs(project.id, { pr: `${want.repo}#${want.number}`, repoPath: initial?.repoPath })
      .then((r) => {
        if (live && r.prs[0]) pick(r.prs[0]);
      })
      .catch(() => {
        // Nothing picked: the user picks the PR again.
        if (live) retryPr.current = undefined;
      });
    return () => {
      live = false;
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps -- pick only when the list or type changes
  }, [review, prList]);

  // A retry on a PR looks it up again, so it's picked as before (unless it can't be any more).
  const retryTaskPr = useRef(initial?.kind !== 'review-pr' ? initial?.pr : undefined);
  useEffect(() => {
    const want = retryTaskPr.current;
    if (!want) return;
    retryTaskPr.current = undefined;
    let live = true;
    api
      .taskPrs(project.id, { pr: `${want.repo}#${want.number}`, repoPath: initial?.repoPath })
      .then((r) => {
        const pr = r.prs[0];
        if (live && pr && !pr.blocked) setTaskPr(pr);
      })
      .catch(() => {
        // Nothing picked: the user picks the PR again.
      });
    return () => {
      live = false;
    };
  }, [project.id, initial?.repoPath]);

  // Picking a PR names an unnamed session after it, and starts it at Code unless a start was picked.
  const pickTaskPr = (pr: TaskPr | null) => {
    setTaskPr(pr);
    if (!pr) return;
    if (!nameEdited) setName(pr.title.slice(0, 100).trim());
    if (!startPicked.current) setStart('code');
  };
  const chooseStart = (c: StartChoice) => {
    startPicked.current = true;
    setStart(c);
  };

  // The scanned repos load the first time Advanced opens.
  useEffect(() => {
    if (!advanced || repos || reposError) return;
    api
      .repos()
      .then((r) => setRepos(r.repos))
      .catch((e) => setReposError(errorMessage(e)));
  }, [advanced, repos, reposError]);

  const prefix = expandBranchPrefix(project.settings.branchPrefix, ghLogin);
  const autoBranch = `${prefix}${slugify(name) || 'task-name'}`;
  const repoChoices = [
    { path: project.repoPath, label: `${repoFolder(project.repoPath)} (project repo)` },
    ...(repos ?? [])
      .filter((r) => r.path !== project.repoPath && !r.projects.some((x) => x.id === project.id))
      .map((r) => ({ path: r.path, label: r.name })),
  ];
  // A review's repo is the PR's clone, which may not be listed yet (Advanced never opened, or just cloned).
  if (!repoChoices.some((r) => r.path === repoPath)) repoChoices.push({ path: repoPath, label: repoFolder(repoPath) });
  const reviewBranch = picked ? `${prefix}review-pr-${picked.number}` : '';
  const typedExists = workOn === 'new' && !!branch.trim() && !!branches?.includes(branch.trim());
  const ready = workOn === 'branch' ? !!existing.trim() : workOn === 'pr' ? !!taskPr : true;
  const canStart = review ? !!picked?.localRepo && !busy : !!name.trim() && !busy && !!branches && ready;
  const chooseRepo = (path: string) => {
    setRepoPath(path);
    // Another repo has other PRs.
    setTaskPr(null);
    if (path === project.repoPath) setBaseBranch(project.defaultBranch);
  };
  const workSummary = workOn === 'pr'
    ? (taskPr ? `PR #${taskPr.number} · ${taskPr.headRefName}` : 'a pull request')
    : workOn === 'branch'
      ? `on ${existing.trim() || 'an existing branch'}`
      : `from ${baseBranch || 'its default branch'} · ${branch.trim() || autoBranch}`;

  const create = async () => {
    setError(null);
    setBusy(true);
    try {
      // Images still uploading are waited for. One that failed is taken out and named (onError), and nothing starts.
      const ready = await held.ready();
      if (!ready) return;
      const images = ready.length ? ready.flatMap((i) => (i.path ? [i.path] : [])) : undefined;
      const s = await api.createSession(
        project.id,
        review && picked
          ? {
            name: name.trim(),
            prompt: prompt.trim() || undefined,
            kind: 'review-pr',
            repoPath: inProjectRepo ? undefined : repoPath,
            pr: { repo: picked.repo, number: picked.number },
            images,
          }
          : {
            name: name.trim(),
            prompt: prompt.trim() || undefined,
            baseBranch: workOn === 'pr' ? undefined : baseBranch,
            kind: 'task',
            start: start === 'review-pr' ? undefined : start,
            repoPath: inProjectRepo ? undefined : repoPath,
            images,
            ...(workOn === 'pr' && taskPr
              ? { pr: { repo: taskPr.repo, number: taskPr.number } }
              : workOn === 'branch'
                ? { branch: existing.trim(), existingBranch: true }
                : { branch: branch.trim() || undefined }),
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
            {busy ? <Busy label={held.uploading ? 'Uploading' : 'Starting'} /> : review ? 'Start review' : 'Start session'}
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
            placeholder={START_EXAMPLES[start].name}
            value={name}
            onChange={(e) => {
              setName(e.target.value);
              setNameEdited(true);
            }}
            autoFocus
          />
        </Field>
        <StartPicker id="ns-start" value={start} onChange={chooseStart} />
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
            (review
              ? 'Anything to focus on. AgentK sums up the PR and then waits for your questions.'
              : 'What should AgentK do? Left empty, AgentK starts on the name as the task.') + ' Images can be pasted or dropped in.'
          }
        >
          <textarea
            id="ns-prompt"
            className="input"
            rows={review ? 2 : 5}
            placeholder={START_EXAMPLES[start].prompt}
            value={prompt}
            onChange={(e) => setPrompt(e.target.value)}
            onPaste={(e) => {
              // A paste with images holds them; text (a spreadsheet's cells come with a picture) pastes as usual.
              const images = pastedImages(e.clipboardData);
              if (!images.length) return;
              e.preventDefault();
              hold(images);
            }}
            onDragOver={(e) => {
              if (!e.dataTransfer.types.includes('Files')) return;
              e.preventDefault();
              e.dataTransfer.dropEffect = 'copy';
            }}
            onDrop={(e) => {
              if (!e.dataTransfer.types.includes('Files')) return;
              e.preventDefault();
              const images = pastedImages(e.dataTransfer);
              if (images.length) hold(images);
              else setError('Only images can be dropped here: PNG, JPEG, GIF or WebP.');
            }}
          />
          <div className="ns-images">
            <button
              type="button"
              id="ns-add-image"
              className="btn small"
              aria-label="Add image"
              title="Add image"
              onClick={() => imagePicker.current?.click()}
            >
              <svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.8" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">
                <rect x="3" y="4" width="18" height="16" rx="2" />
                <circle cx="9" cy="10" r="1.6" />
                <path d="M21 16l-5-5-9 9" />
              </svg>
            </button>
            <HeldImages held={held} hint={review ? 'sent with your notes' : 'sent with the first prompt'} />
            {lostImages > 0 && (
              <span className="hint">
                {lostImages === 1 ? "The image from the start that failed wasn't kept." : `The ${lostImages} images from the start that failed weren't kept.`} Add
                them again.
              </span>
            )}
          </div>
        </Field>
        {/* Outside the field's label, so a tap on the label doesn't open the picker. */}
        <input
          ref={imagePicker}
          type="file"
          accept="image/png,image/jpeg,image/gif,image/webp,image/*"
          multiple
          hidden
          onChange={(e) => {
            const files = Array.from(e.target.files ?? []);
            // Cleared, so picking the same image again still fires a change.
            e.target.value = '';
            hold(files);
          }}
        />
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
                  : `${repoFolder(repoPath)} · ${workSummary}`}
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
                  <div className="field">
                    <span className="label" id="ns-work-on-label">
                      Work on
                    </span>
                    <div className="seg work-on" role="radiogroup" aria-labelledby="ns-work-on-label">
                      {WORK_ON.map((w) => (
                        <button
                          key={w.value}
                          id={`ns-work-on-${w.value}`}
                          type="button"
                          role="radio"
                          aria-checked={workOn === w.value}
                          className={cx(workOn === w.value && 'on')}
                          onClick={() => setWorkOn(w.value)}
                        >
                          {w.label}
                        </button>
                      ))}
                    </div>
                  </div>
                  {workOn !== 'pr' && (
                    <Field
                      label={workOn === 'branch' ? 'Compare with' : 'Base branch'}
                      hint={workOn === 'branch' ? "Used when the branch has no open PR. With one, the PR's base is used." : undefined}
                    >
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
                  )}
                  {workOn === 'new' && (
                    <Field
                      label="Branch"
                      hint={
                        typedExists
                          ? `${branch.trim()} already exists: AgentK works on it as it is, without starting a new branch.`
                          : 'Left empty, AgentK names it from the session name.'
                      }
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
                  )}
                  {workOn === 'branch' && (
                    <Field
                      label="Branch"
                      hint="One of your local branches, or type one that's only on origin. AgentK checks it out as it is (fast-forwarded to origin when it's behind) and picks up its open PR."
                    >
                      <input
                        id="ns-existing-branch"
                        className="input mono"
                        list="ns-existing-branches"
                        placeholder="Branch name"
                        value={existing}
                        autoCapitalize="off"
                        autoCorrect="off"
                        spellCheck={false}
                        onChange={(e) => setExisting(e.target.value)}
                      />
                      <datalist id="ns-existing-branches">
                        {(branches ?? []).map((b) => (
                          <option key={b} value={b} />
                        ))}
                      </datalist>
                    </Field>
                  )}
                  {workOn === 'pr' && <AttachPrPicker project={project} repoPath={repoPath} picked={taskPr} onPick={pickTaskPr} />}
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
