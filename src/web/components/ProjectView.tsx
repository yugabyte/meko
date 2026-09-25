import { useEffect, useState, type ReactNode } from 'react';
import {
  STAGES,
  STARTUP_STEPS,
  STAGE_LABELS,
  type DiscardCheck,
  type ProjectWithSessions,
  type Session,
  type SessionAction,
} from '../../shared/types';
import { api, errorMessage } from '../api';
import { navigate } from '../router';
import { disposeTerminal } from '../terminals';
import { STATUS_TEXT, cx, duration, formatTokens, totalTokens, usageTitle } from '../util';
import { ConfirmButton, DatapackName, Dots, StatusDot } from './common';
import { ChangesPanel } from './ChangesPanel';
import { DesignPanel } from './DesignPanel';
import { TerminalPane } from './TerminalPane';

const REVIEW_TEXT: Record<Session['reviewState'], string> = {
  none: 'no PR',
  waiting: 'waiting for review',
  in_review: 'AgentK reviewing',
  refused: 'AgentK declined',
  addressing: 'addressing review',
  addressed: 'review addressed',
  approved: 'approved',
};

const CI_TEXT: Record<Session['ciState'], string> = {
  none: 'no CI',
  pending: 'CI running',
  success: 'CI green',
  failure: 'CI failing',
};

export function ProjectView({
  project,
  sessions,
  session,
  mekoWebUrl,
  onNewSession,
  onError,
}: {
  project: ProjectWithSessions;
  sessions: Session[];
  session: Session | null;
  mekoWebUrl: string | null;
  onNewSession: () => void;
  onError: (msg: string) => void;
}) {
  // One side panel at a time.
  const [panel, setPanel] = useState<'design' | 'changes' | null>(null);
  const toggle = (p: 'design' | 'changes') => setPanel((o) => (o === p ? null : p));
  const [now, setNow] = useState(Date.now());
  useEffect(() => {
    const t = setInterval(() => setNow(Date.now()), 15000);
    return () => clearInterval(t);
  }, []);
  useEffect(() => setPanel(null), [session?.id]);
  // During Design, show the design as soon as it's written, and again whenever it's revised.
  const designAt = session?.stage === 'design' ? session.designUpdatedAt : null;
  useEffect(() => {
    if (designAt) setPanel('design');
  }, [session?.id, designAt]);

  const act = async (action: SessionAction) => {
    if (!session) return;
    try {
      await api.sessionAction(session.id, action);
      if (action === 'archive' || action === 'discard') disposeTerminal(session.id);
    } catch (e) {
      onError(errorMessage(e));
    }
  };

  const dedicated = project.datapacks.find((d) => d.role === 'dedicated');
  const sharedCount = project.datapacks.filter((d) => d.role === 'shared').length;

  return (
    <div className="project-view">
      <header className="crumb">
        <span className="muted">{project.name}</span>
        {session && (
          <>
            <span className="muted">/</span>
            <b className="crumb-name">{session.name}</b>
            <StatusDot status={session.status} />
            <span className="muted small status-text">
              {STATUS_TEXT[session.status]}
              {session.status === 'starting' && <Dots />}
              {session.status === 'starting' && session.startup
                ? ` · ${STARTUP_STEPS.find((x) => x.step === session.startup!.step)?.label ?? ''}`
                : ''}
              {session.status === 'working' && session.lastEventAt ? ` · ${duration(session.lastEventAt, now)}` : ''}
              {session.statusDetail && session.status !== 'starting' ? ` · ${session.statusDetail}` : ''}
            </span>
          </>
        )}
        <span className="sp" />
        {session?.prUrl && (
          <a className="btn small" href={session.prUrl} target="_blank" rel="noreferrer">
            PR #{session.prNumber}
          </a>
        )}
        <button className="btn small" onClick={() => navigate({ view: 'home', projectId: project.id })}>
          Project home
        </button>
        {session && (session.hasDesign || session.stage === 'design') && (
          <button className={cx('btn small', panel === 'design' && 'on')} onClick={() => toggle('design')}>
            Design doc
          </button>
        )}
        {session?.status === 'exited' && (
          <button className="btn small primary" onClick={() => act('resume')}>
            Resume
          </button>
        )}
        {session && <EndSessionButton key={session.id} session={session} onAct={act} />}
      </header>

      {session && <StageBar session={session} project={project} onAct={act} />}

      <div className="tabs" role="tablist">
        {sessions.map((s) => (
          <button
            key={s.id}
            role="tab"
            aria-selected={s.id === session?.id}
            className={cx('tab', s.id === session?.id && 'sel')}
            onClick={() => navigate({ view: 'project', projectId: project.id, sessionId: s.id })}
            title={`${s.name} · ${STATUS_TEXT[s.status]} · ${STAGE_LABELS[s.stage]} · ${formatTokens(totalTokens(s.usage))} tokens`}
          >
            <StatusDot status={s.status} />
            <span className="tab-name">{s.name}</span>
            <span className="stchip">{STAGE_LABELS[s.stage]}</span>
          </button>
        ))}
        <button className="tab new" onClick={onNewSession}>
          + New session
        </button>
      </div>

      <div className="workarea">
        {session ? (
          <TerminalPane key={session.id} session={session} />
        ) : (
          <div className="empty-main">
            <h2>No sessions in {project.name}</h2>
            <p className="muted">
              Start a session for a task. It gets its own branch and worktree, and Claude starts by writing a design
              unless you skip that step.
            </p>
            <button className="btn primary" onClick={onNewSession}>
              New session
            </button>
          </div>
        )}
        {session && panel === 'design' && (
          <DesignPanel session={session} onClose={() => setPanel(null)} onApprove={() => act('approve-design')} />
        )}
        {session && panel === 'changes' && <ChangesPanel session={session} onClose={() => setPanel(null)} />}
      </div>

      {session && (
        <footer className="foot">
          <span>branch {session.branch}</span>
          {session.diffStat && (
            <button
              className={cx('foot-btn', panel === 'changes' && 'on')}
              onClick={() => toggle('changes')}
              aria-expanded={panel === 'changes'}
              title="Show every file changed on this branch"
            >
              <span className="add">+{session.diffStat.added}</span> <span className="del">−{session.diffStat.removed}</span>{' '}
              vs {session.baseBranch}
            </button>
          )}
          <span title={usageTitle(session.usage)}>
            tokens {formatTokens(totalTokens(session.usage))} · {formatTokens(session.usage.output)} out
          </span>
          <span title="The project's Meko datapack">
            Meko:{dedicated ? <DatapackName id={dedicated.id} name={dedicated.name} mekoWebUrl={mekoWebUrl} /> : '—'}
            {sharedCount > 0 ? ` · +${sharedCount} shared` : ''}
          </span>
          <span>
            {STAGE_LABELS[session.stage].toLowerCase()}
            {session.prNumber ? ` · ${REVIEW_TEXT[session.reviewState]} · ${CI_TEXT[session.ciState]}` : ''}
          </span>
        </footer>
      )}
    </div>
  );
}

const plural = (n: number, word: string) => `${n} ${word}${n === 1 ? '' : 's'}`;

/** Archive keeps the branch; Discard deletes the worktree and branch after showing what would be lost. */
function EndSessionButton({ session, onAct }: { session: Session; onAct: (a: SessionAction) => Promise<void> }) {
  const [step, setStep] = useState<'closed' | 'choose' | 'checking' | 'discard'>('closed');
  const [check, setCheck] = useState<DiscardCheck | null>(null);
  const [busy, setBusy] = useState(false);

  const run = async (action: SessionAction) => {
    setBusy(true);
    try {
      await onAct(action);
    } finally {
      setBusy(false);
      setStep('closed');
    }
  };

  const askDiscard = async () => {
    setStep('checking');
    try {
      setCheck(await api.discardCheck(session.id));
    } catch {
      setCheck(null);
    }
    setStep('discard');
  };

  if (step === 'closed') {
    return (
      <button className="btn small" onClick={() => setStep('choose')}>
        Archive
      </button>
    );
  }
  const cancel = (
    <button className="btn ghost" disabled={busy} onClick={() => setStep('closed')}>
      Cancel
    </button>
  );
  if (step !== 'discard') {
    return (
      <span className="confirm-inline">
        <span className="confirm-prompt">End this session?</span>
        <button className="btn primary" disabled={busy || step === 'checking'} onClick={() => run('archive')}>
          Archive (keep branch)
        </button>
        <button className="btn danger" disabled={busy || step === 'checking'} onClick={askDiscard}>
          {step === 'checking' ? 'Checking…' : 'Discard…'}
        </button>
        {cancel}
      </span>
    );
  }
  const warnings: string[] = [];
  if (!check) warnings.push('Could not check for unsaved work.');
  else {
    if (check.uncommittedFiles) warnings.push(`${plural(check.uncommittedFiles, 'uncommitted file')} will be lost.`);
    if (check.unpushedCommits) warnings.push(`${plural(check.unpushedCommits, 'commit')} never pushed will be lost.`);
  }
  if (session.prNumber) warnings.push(`PR #${session.prNumber} and the remote branch stay on GitHub.`);
  return (
    <span className="confirm-inline">
      <span className="confirm-prompt">Delete the worktree and branch {session.branch}?</span>
      {warnings.map((w) => (
        <span key={w} className="confirm-warn">
          {w}
        </span>
      ))}
      <button className="btn danger" disabled={busy} onClick={() => run('discard')}>
        Discard
      </button>
      {cancel}
    </span>
  );
}

function StageBar({
  session,
  project,
  onAct,
}: {
  session: Session;
  project: ProjectWithSessions;
  onAct: (a: SessionAction) => Promise<void>;
}) {
  const idx = session.stage === 'done' ? STAGES.length : STAGES.indexOf(session.stage);
  const midTurn = session.status === 'working' || session.status === 'starting';
  const blocked = session.busy;

  let gate: ReactNode = null;
  switch (session.stage) {
    case 'design':
      gate = (
        <button
          className="gate"
          disabled={blocked || !session.hasDesign}
          title={session.hasDesign ? 'Approve the design and move to Code' : 'Claude hasn’t proposed a design yet'}
          onClick={() => onAct('approve-design')}
        >
          Approve design
        </button>
      );
      break;
    case 'code':
      gate = (
        <button
          className="gate"
          disabled={blocked || midTurn}
          title={midTurn ? 'Wait for Claude to finish its turn' : 'Commit, push and open a pull request'}
          onClick={() => onAct('open-pr')}
        >
          Open PR
        </button>
      );
      break;
    case 'pr':
      gate = <span className="gate-pill">Opening PR…</span>;
      break;
    case 'review':
      gate = (
        <span className="gate-group">
          {session.reviewState === 'refused' && (
            <ConfirmButton
              className="btn small"
              label="Force review"
              confirmLabel="Force AgentK review"
              prompt="AgentK declined this PR."
              onConfirm={() => onAct('force-review')}
              disabled={blocked}
            />
          )}
          {project.settings.reviewer === 'agentk' && session.reviewState !== 'refused' && (
            <button
              className="btn small"
              disabled={blocked || midTurn || session.reviewState === 'in_review' || session.reviewState === 'addressing'}
              onClick={() => onAct('request-review')}
              title="Ask AgentK to review the latest push"
            >
              Request another round
            </button>
          )}
          <button
            className="gate"
            disabled={blocked || !session.canMerge}
            title={session.canMerge ? 'Merge the pull request' : session.mergeBlockers.join('\n')}
            onClick={() => onAct('merge')}
          >
            Merge
          </button>
        </span>
      );
      break;
    case 'merge':
      gate = <span className="gate-pill">Merging…</span>;
      break;
    case 'cleanup':
      gate = <span className="gate-pill">Cleaning up…</span>;
      break;
    case 'done':
      gate = <span className="gate-pill done">Done</span>;
      break;
  }

  return (
    <div className="stages-wrap">
      <div className="stages" aria-label="Stages">
        {STAGES.map((st, i) => (
          <span key={st} className="stg-item">
            {i > 0 && <span className={cx('stg-sep', i <= idx && 'done')} />}
            <span className={cx('stg', i < idx && 'done', i === idx && 'cur')} aria-current={i === idx ? 'step' : undefined}>
              <span className="n">{i < idx ? '✓' : i + 1}</span>
              {STAGE_LABELS[st]}
            </span>
          </span>
        ))}
        <span className="stages-gate">{gate}</span>
      </div>
      {session.stage === 'review' && !session.canMerge && session.mergeBlockers.length > 0 && (
        <div className="blockers">Merge is waiting on: {session.mergeBlockers.join(' · ')}</div>
      )}
    </div>
  );
}
