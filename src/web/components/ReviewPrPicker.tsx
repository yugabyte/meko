import { useState } from 'react';
import type { ProjectWithSessions, ReviewablePr, ReviewablePrs } from '../../shared/types';
import { api, errorMessage } from '../api';
import { cx, relativeTime, repoFolder } from '../util';
import { ErrorText } from './common';

const key = (pr: ReviewablePr) => `${pr.repo}#${pr.number}`;

/**
 * The New session dialog's Pull request field for a Review a PR: the open PRs whose description
 * names this project's datapack (any repo), and another PR by number or link. A PR whose repo
 * isn't cloned here offers Clone; a typed PR naming a datapack the project lacks offers Attach it.
 */
export function ReviewPrPicker({
  project,
  list,
  listError,
  onReload,
  picked,
  onPick,
  lookupRepo,
}: {
  project: ProjectWithSessions;
  /** null while loading. */
  list: ReviewablePrs | null;
  listError: string | null;
  onReload: () => void;
  picked: ReviewablePr | null;
  onPick: (pr: ReviewablePr | null) => void;
  /** Where a bare PR number is looked up: the repo picked under Advanced. */
  lookupRepo: string;
}) {
  const [other, setOther] = useState('');
  const [typed, setTyped] = useState<ReviewablePr | null>(null);
  const [looking, setLooking] = useState(false);
  const [cloning, setCloning] = useState(false);
  const [attach, setAttach] = useState<'idle' | 'busy' | 'done'>('idle');
  const [error, setError] = useState<string | null>(null);
  const prs = list?.prs ?? [];
  const pickedIsTyped = !!picked && !!typed && key(picked) === key(typed) && !prs.some((p) => key(p) === key(picked));

  const lookUp = async () => {
    const q = other.trim();
    if (!q || looking) return;
    setError(null);
    setLooking(true);
    try {
      const r = await api.reviewPrs(project.id, { pr: q, repoPath: lookupRepo === project.repoPath ? undefined : lookupRepo });
      const pr = r.prs[0] ?? null;
      setTyped(pr);
      setAttach('idle');
      onPick(pr);
    } catch (e) {
      setError(errorMessage(e));
    } finally {
      setLooking(false);
    }
  };

  const clone = async (pr: ReviewablePr) => {
    setError(null);
    setCloning(true);
    try {
      const repo = await api.cloneRepo({ url: pr.cloneUrl });
      const next = { ...pr, localRepo: repo.path };
      if (typed && key(typed) === key(pr)) setTyped(next);
      onPick(next);
      onReload();
    } catch (e) {
      setError(errorMessage(e));
    } finally {
      setCloning(false);
    }
  };

  const attachDatapack = async (id: string) => {
    setError(null);
    setAttach('busy');
    try {
      const dp = (await api.datapacks()).find((d) => d.id === id);
      if (!dp) throw new Error("You don't have access to this PR's datapack in Meko. Ask its author to share the project with you.");
      await api.attachDatapack(project.id, { id: dp.id, name: dp.name });
      setAttach('done');
    } catch (e) {
      setError(errorMessage(e));
      setAttach('idle');
    }
  };

  const row = (pr: ReviewablePr, tag: string | null) => {
    const sel = !!picked && key(picked) === key(pr);
    return (
      <div key={key(pr)} className="pr-pick-item">
        <button type="button" role="radio" aria-checked={sel} className={cx('dp-row pr-pick-row', sel && 'sel')} onClick={() => onPick(pr)}>
          <span className="radio" />
          <span className="pr-pick-main">
            <span className="pr-pick-title">
              <span className="mono small muted">{pr.repo.split('/')[1]}</span> <b>#{pr.number}</b> {pr.title}
            </span>
            <span className="small muted">
              @{pr.author} · opened {relativeTime(Date.parse(pr.createdAt)).toLowerCase()}
              {pr.localRepo ? ` · ${repoFolder(pr.localRepo)}` : ''}
            </span>
          </span>
          {tag && <span className="tag shared">{tag}</span>}
        </button>
        {!pr.localRepo && (
          <div className="pr-pick-note">
            <span>{pr.repo} isn't cloned here.</span>
            <button type="button" className="btn small" disabled={cloning} onClick={() => void clone(pr)}>
              {cloning ? 'Cloning…' : `Clone ${pr.repo}`}
            </button>
          </div>
        )}
      </div>
    );
  };

  return (
    <div className="field">
      <span className="label" id="ns-pr-label">
        Pull request
      </span>
      <div className="dp-list pr-pick" role="radiogroup" aria-labelledby="ns-pr-label">
        {!list && !listError && <p className="muted pr-pick-empty">Looking for this project's PRs…</p>}
        {list && !prs.length && (
          <p className="muted pr-pick-empty">{list.error ?? 'No open PR names this project\'s datapack yet. Type one below.'}</p>
        )}
        {prs.map((pr) => row(pr, 'names this project'))}
        {typed && !prs.some((p) => key(p) === key(typed)) && row(typed, null)}
        <div className="pr-pick-other">
          <span className="small">Another PR</span>
          <span className="pr-pick-other-row">
            <input
              id="ns-pr-other"
              className="input"
              placeholder="Number, or a github.com PR link"
              value={other}
              autoCapitalize="off"
              autoCorrect="off"
              spellCheck={false}
              onChange={(e) => setOther(e.target.value)}
              onKeyDown={(e) => {
                if (e.key === 'Enter') {
                  e.preventDefault();
                  void lookUp();
                }
              }}
            />
            <button type="button" className="btn small" disabled={!other.trim() || looking} onClick={() => void lookUp()}>
              {looking ? 'Looking…' : 'Find'}
            </button>
          </span>
        </div>
      </div>
      {pickedIsTyped && picked?.otherDatapackId && attach !== 'done' && (
        <div className="pr-pick-note">
          <span>This PR's project datapack isn't in this project, so AgentK can't read its design.</span>
          <button type="button" className="btn small" disabled={attach === 'busy'} onClick={() => void attachDatapack(picked.otherDatapackId!)}>
            {attach === 'busy' ? 'Attaching…' : 'Attach it'}
          </button>
        </div>
      )}
      {attach === 'done' && <span className="ok-text">Attached. AgentK searches it in this review.</span>}
      <span className="hint">
        Open PRs in any repo whose description names this project's datapack. For any other PR, type its number or link.{' '}
        <button type="button" className="link-btn" onClick={onReload}>
          Refresh
        </button>
      </span>
      <ErrorText error={listError ?? error} />
    </div>
  );
}
