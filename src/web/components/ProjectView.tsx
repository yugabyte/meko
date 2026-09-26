import { useEffect, useRef, useState, type ReactNode } from 'react';
import {
  STAGES,
  STAGE_LABELS,
  type ProjectWithSessions,
  type Session,
  type SessionAction,
} from '../../shared/types';
import { api, errorMessage } from '../api';
import { useIsMobile } from '../hooks/useMedia';
import { navigate } from '../router';
import { disposeTerminal } from '../terminals';
import { STATUS_TEXT, cx, designApprovalBlocker, formatTokens, totalTokens, usageTitle } from '../util';
import { Check, ConfirmButton, DatapackName, StatusDot } from './common';
import { ChangesPanel } from './ChangesPanel';
import { DesignPanel } from './DesignPanel';
import { EndSessionButton, SessionSheet } from './SessionSheet';
import { TerminalPane } from './TerminalPane';
import { TopBar } from './TopBar';

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
  onMenu,
  onError,
}: {
  project: ProjectWithSessions;
  sessions: Session[];
  session: Session | null;
  mekoWebUrl: string | null;
  onNewSession: () => void;
  /** Opens the projects drawer (phone layout). */
  onMenu: () => void;
  onError: (msg: string) => void;
}) {
  const mobile = useIsMobile();
  // One side panel at a time.
  const [panel, setPanel] = useState<'design' | 'changes' | null>(null);
  const toggle = (p: 'design' | 'changes') => setPanel((o) => (o === p ? null : p));
  const [sheet, setSheet] = useState(false);
  const tabsRef = useRef<HTMLDivElement>(null);
  useEffect(() => {
    setPanel(null);
    setSheet(false);
    tabsRef.current?.querySelector('.tab.sel')?.scrollIntoView({ block: 'nearest', inline: 'nearest' });
  }, [session?.id]);
  // During Design, show the design as soon as it's written, and again whenever it's revised.
  const designAt = session?.stage === 'design' ? session.designUpdatedAt : null;
  useEffect(() => {
    if (designAt) setPanel('design');
  }, [session?.id, designAt]);

  // One session action at a time, so a double tap can't send two merges or resumes.
  const [acting, setActing] = useState(false);
  const actingRef = useRef(false);
  const act = async (action: SessionAction) => {
    if (!session || actingRef.current) return;
    actingRef.current = true;
    setActing(true);
    try {
      await api.sessionAction(session.id, action);
      if (action === 'archive' || action === 'discard') disposeTerminal(session.id);
    } catch (e) {
      onError(errorMessage(e));
    } finally {
      actingRef.current = false;
      setActing(false);
    }
  };

  const pending = session ? session.comments.drafts + session.comments.queued : 0;
  const dedicated = project.datapacks.find((d) => d.role === 'dedicated');
  const sharedCount = project.datapacks.filter((d) => d.role === 'shared').length;
  const canDesign = !!session && (session.hasDesign || session.stage === 'design');
  // On a phone the gate lives in the ⋯ sheet; a dot on ⋯ says when it's waiting on you.
  const gateReady = !!session && !session.busy && ((session.stage === 'design' && session.hasDesign) || (session.stage === 'review' && session.canMerge));
  const goHome = () => navigate({ view: 'home', projectId: project.id });

  const branchText = session && <span>branch {session.branch}</span>;
  const tokensText = session && (
    <span title={usageTitle(session.usage)}>
      tokens {formatTokens(totalTokens(session.usage))} · {formatTokens(session.usage.output)} out
    </span>
  );
  const mekoText = (
    <span title="The project's Meko datapack">
      Meko:{dedicated ? <DatapackName id={dedicated.id} name={dedicated.name} mekoWebUrl={mekoWebUrl} /> : '—'}
      {sharedCount > 0 ? ` · +${sharedCount} shared` : ''}
    </span>
  );
  const stageText = session && (
    <span>
      {STAGE_LABELS[session.stage].toLowerCase()}
      {session.prNumber ? ` · ${REVIEW_TEXT[session.reviewState]} · ${CI_TEXT[session.ciState]}` : ''}
    </span>
  );

  // On a phone the tabs sit in the app bar, between the drawer button and the session's actions.
  const tabs = (
    <div className="tabs" role="tablist" ref={tabsRef}>
      {sessions.map((s) => (
        <button
          key={s.id}
          role="tab"
          aria-selected={s.id === session?.id}
          className={cx('tab', s.id === session?.id && 'sel')}
          onClick={() => navigate({ view: 'project', projectId: project.id, sessionId: s.id })}
          title={[
            s.name,
            STATUS_TEXT[s.status] + (s.statusDetail && s.status !== 'starting' ? `: ${s.statusDetail}` : ''),
            STAGE_LABELS[s.stage],
            `${formatTokens(totalTokens(s.usage))} tokens`,
          ].join(' · ')}
        >
          <StatusDot status={s.status} />
          <span className="tab-name">{s.name}</span>
          <span className="stchip">{STAGE_LABELS[s.stage]}</span>
        </button>
      ))}
      <button className="tab new" onClick={onNewSession} title="New session" aria-label="New session">
        +
      </button>
    </div>
  );

  return (
    <div className="project-view">
      {mobile ? (
        <TopBar onMenu={onMenu}>
          {tabs}
          {session?.status === 'exited' && (
            <button className="btn small primary" disabled={acting} onClick={() => act('resume')}>
              Resume
            </button>
          )}
          {session ? (
            <button
              className="icon-btn topbar-more"
              onClick={() => setSheet(true)}
              aria-label={gateReady ? 'Session actions, one is ready for you' : 'Session actions'}
              aria-haspopup="dialog"
            >
              <svg width="20" height="20" viewBox="0 0 24 24" fill="currentColor" aria-hidden="true">
                <circle cx="5" cy="12" r="1.8" />
                <circle cx="12" cy="12" r="1.8" />
                <circle cx="19" cy="12" r="1.8" />
              </svg>
              {gateReady && <span className="topbar-dot" aria-hidden="true" />}
            </button>
          ) : (
            <button className="btn small" onClick={goHome}>
              Project home
            </button>
          )}
        </TopBar>
      ) : null}

      {/* Desktop: one row with the stages and their gate, then the session's actions. On a phone the
          stages, their gate and the actions are in the ⋯ sheet, to leave the room to the terminal. */}
      {!mobile && (
        <StageBar
          session={session}
          project={project}
          onAct={act}
          acting={acting}
          actions={
            <>
              {session?.prUrl && (
                <a className="btn small" href={session.prUrl} target="_blank" rel="noreferrer">
                  PR #{session.prNumber}
                </a>
              )}
              <button className="btn small" onClick={goHome}>
                Project home
              </button>
              {canDesign && (
                <button className={cx('btn small', panel === 'design' && 'on')} onClick={() => toggle('design')}>
                  Design doc
                </button>
              )}
              {session?.status === 'exited' && (
                <button className="btn small primary" disabled={acting} onClick={() => act('resume')}>
                  Resume
                </button>
              )}
              {session && <EndSessionButton key={session.id} session={session} onAct={act} disabled={acting} />}
            </>
          }
        />
      )}

      {!mobile && tabs}

      <div className="workarea">
        {/* Siblings need distinct keys: with the same key, React lost the terminal of a session left with
            its design or changes open, and kept it on screen beside the next one. */}
        {session ? (
          <TerminalPane key={`term-${session.id}`} session={session} />
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
          <DesignPanel key={`design-${session.id}`} session={session} onClose={() => setPanel(null)} onApprove={() => act('approve-design')} />
        )}
        {session && panel === 'changes' && <ChangesPanel key={`changes-${session.id}`} session={session} onClose={() => setPanel(null)} />}
      </div>

      {session && (
        <footer className="foot">
          {!mobile && branchText}
          {session.diffStat && (
            <button
              className={cx('foot-btn', panel === 'changes' && 'on')}
              onClick={() => toggle('changes')}
              aria-expanded={panel === 'changes'}
              title={`Show every file changed on this branch since ${session.baseBranch}`}
            >
              <span className="add">+{session.diffStat.added}</span> <span className="del">−{session.diffStat.removed}</span>
              {pending > 0 && ` · ${pending} ${pending === 1 ? 'comment' : 'comments'}`}
            </button>
          )}
          {!mobile && tokensText}
          {!mobile && mekoText}
          {stageText}
        </footer>
      )}

      {session && sheet && (
        <SessionSheet
          session={session}
          stage={<StageBar session={session} project={project} onAct={act} acting={acting} actions={null} reasons />}
          canDesign={canDesign}
          pendingComments={pending}
          acting={acting}
          info={
            <>
              {branchText}
              {tokensText}
              {mekoText}
              {stageText}
            </>
          }
          onDesign={() => setPanel('design')}
          onChanges={() => setPanel('changes')}
          onHome={goHome}
          onAct={act}
          onClose={() => setSheet(false)}
        />
      )}
    </div>
  );
}

function StageBar({
  session,
  project,
  onAct,
  acting = false,
  actions,
  reasons = false,
}: {
  session: Session | null;
  project: ProjectWithSessions;
  onAct: (a: SessionAction) => Promise<void>;
  /** A session action is in flight. */
  acting?: boolean;
  actions: ReactNode;
  /** Say under the stages why a disabled gate can't be passed (the phone's sheet, where there are no tooltips). */
  reasons?: boolean;
}) {
  if (!session) {
    return (
      <div className="stages-wrap">
        <div className="stages-row">
          <span className="sp" />
          <div className="stage-actions">{actions}</div>
        </div>
      </div>
    );
  }
  const idx = session.stage === 'done' ? STAGES.length : STAGES.indexOf(session.stage);
  const midTurn = session.status === 'working' || session.status === 'starting';
  const blocked = session.busy || acting;
  // Why the Design or Code gate is closed, if it is.
  let reason: string | null = null;

  let gate: ReactNode = null;
  switch (session.stage) {
    case 'design': {
      reason = designApprovalBlocker(session);
      gate = (
        <button
          className="gate"
          disabled={blocked || !!reason}
          title={reason ?? 'Approve the design and move to Code'}
          onClick={() => onAct('approve-design')}
        >
          Approve design
        </button>
      );
      break;
    }
    case 'code':
      reason = midTurn ? 'Wait for Claude to finish its turn.' : null;
      gate = (
        <button
          className="gate"
          disabled={blocked || !!reason}
          title={reason ?? 'Commit, push and open a pull request'}
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
            title={session.canMerge ? 'Merge the pull request, then remove the worktree and branch' : session.mergeBlockers.join('\n')}
            onClick={() => onAct('merge')}
          >
            Merge &amp; Close
          </button>
        </span>
      );
      break;
    case 'merge':
      gate = <span className="gate-pill">Merging and closing…</span>;
      break;
    case 'done':
      gate = <span className="gate-pill done">Done</span>;
      break;
  }

  return (
    <div className="stages-wrap">
      <div className="stages-row">
        <div className="stages" aria-label="Stages">
          {STAGES.map((st, i) => (
            <span key={st} className="stg-item">
              {i > 0 && <span className={cx('stg-sep', i <= idx && 'done')} />}
              <span className={cx('stg', i < idx && 'done', i === idx && 'cur')} aria-current={i === idx ? 'step' : undefined}>
                <span className="n">{i < idx ? <Check size={10} /> : i + 1}</span>
                <span className="stg-label">{STAGE_LABELS[st]}</span>
              </span>
            </span>
          ))}
          {/* The phone layout shows only the current stage's name, here. */}
          <span className="stg-now">
            <b>{STAGE_LABELS[session.stage]}</b>
            {session.stage !== 'done' && <span className="muted"> {idx + 1} of {STAGES.length}</span>}
          </span>
          <span className="stages-gate">{gate}</span>
        </div>
        {actions && <div className="stage-actions">{actions}</div>}
      </div>
      {session.stage === 'review' && !session.canMerge && session.mergeBlockers.length > 0 && (
        <div className="blockers">Merge is waiting on: {session.mergeBlockers.join(' · ')}</div>
      )}
      {reasons && reason && <div className="blockers">{reason}</div>}
    </div>
  );
}
