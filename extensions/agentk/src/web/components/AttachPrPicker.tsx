import { useEffect, useState } from 'react';
import type { Project, TaskPr, TaskPrs } from '../../shared/types';
import { api, errorMessage } from '../api';
import { cx, relativeTime, repoFolder } from '../util';
import { Busy, ErrorText } from './common';

const key = (pr: TaskPr) => `${pr.repo}#${pr.number}`;

/**
 * The New session dialog's Pull request field for a task that keeps working on an open PR
 * (Advanced → Pull request): your open PRs in the session's repo, and another PR by number or
 * link. A PR a session can't start on (its fork isn't a remote here, or a running session has its
 * branch) is listed but can't be picked, with the reason.
 */
export function AttachPrPicker({
  project,
  repoPath,
  picked,
  onPick,
}: {
  project: Project;
  /** The session's repo: where the PRs are listed and a bare number is looked up. */
  repoPath: string;
  picked: TaskPr | null;
  onPick: (pr: TaskPr | null) => void;
}) {
  const [list, setList] = useState<TaskPrs | null>(null);
  const [listError, setListError] = useState<string | null>(null);
  const [reload, setReload] = useState(0);
  const [other, setOther] = useState('');
  const [typed, setTyped] = useState<TaskPr | null>(null);
  const [looking, setLooking] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const inProjectRepo = repoPath === project.repoPath;

  useEffect(() => {
    let live = true;
    setList(null);
    setListError(null);
    api
      .taskPrs(project.id, { repoPath: inProjectRepo ? undefined : repoPath })
      .then((r) => {
        if (live) setList(r);
      })
      .catch((e) => {
        if (live) setListError(errorMessage(e));
      });
    return () => {
      live = false;
    };
  }, [project.id, repoPath, inProjectRepo, reload]);

  const lookUp = async () => {
    const q = other.trim();
    if (!q || looking) return;
    setError(null);
    setLooking(true);
    try {
      const r = await api.taskPrs(project.id, { pr: q, repoPath: inProjectRepo ? undefined : repoPath });
      const pr = r.prs[0] ?? null;
      setTyped(pr);
      onPick(pr && !pr.blocked ? pr : null);
    } catch (e) {
      setError(errorMessage(e));
    } finally {
      setLooking(false);
    }
  };

  const prs = list?.prs ?? [];
  const shown = typed && !prs.some((p) => key(p) === key(typed)) ? [...prs, typed] : prs;

  return (
    <div className="field">
      <span className="label" id="ns-attach-pr-label">
        Pull request
      </span>
      <div className="dp-list pr-pick" role="radiogroup" aria-labelledby="ns-attach-pr-label">
        {!list && !listError && (
          <p className="muted pr-pick-empty">
            <Busy label="Looking for your open PRs" />
          </p>
        )}
        {list && !prs.length && (
          <p className="muted pr-pick-empty">{list.error ?? `You have no open PRs in ${repoFolder(repoPath)}. Type one below.`}</p>
        )}
        {shown.map((pr) => {
          const sel = !!picked && key(picked) === key(pr);
          return (
            <div key={key(pr)} className="pr-pick-item">
              <button
                type="button"
                role="radio"
                aria-checked={sel}
                disabled={!!pr.blocked}
                className={cx('dp-row pr-pick-row', sel && 'sel')}
                onClick={() => onPick(pr)}
              >
                <span className="radio" />
                <span className="pr-pick-main">
                  <span className="pr-pick-title">
                    <b>#{pr.number}</b> {pr.title}
                  </span>
                  <span className="small muted mono pr-pick-branch">
                    {pr.fork ? `${pr.headRepo.split('/')[0]}:` : ''}
                    {pr.headRefName} → {pr.baseRefName}
                  </span>
                  <span className="small muted">updated {relativeTime(Date.parse(pr.updatedAt)).toLowerCase()}</span>
                </span>
                {pr.fork && <span className="tag shared">fork</span>}
              </button>
              {pr.blocked && <div className="pr-pick-note">{pr.blocked}</div>}
            </div>
          );
        })}
        <div className="pr-pick-other">
          <span className="small">Another PR</span>
          <span className="pr-pick-other-row">
            <input
              id="ns-attach-pr-other"
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
              {looking ? <Busy label="Looking" /> : 'Find'}
            </button>
          </span>
        </div>
      </div>
      <span className="hint">
        AgentK works on the PR's branch, against its base, and picks the PR up when the session moves to PR &amp; Review.{' '}
        <button type="button" className="link-btn" onClick={() => setReload((n) => n + 1)}>
          Refresh
        </button>
      </span>
      <ErrorText error={listError ?? error} />
    </div>
  );
}
