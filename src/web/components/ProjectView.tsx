import { useEffect, useRef, useState, type ReactNode } from 'react';
import {
  KIND_STAGES,
  STAGE_LABELS,
  type CloseSessionRequest,
  type MekoStatus,
  type MoveTarget,
  type ProjectWithSessions,
  type Session,
  type SessionAction,
  type Stage,
} from '../../shared/types';
import { kindOf, moveBlockedIn, moveTargets, stepOf } from '../../shared/stages';
import { api, errorMessage } from '../api';
import { approveDesign } from '../design-answers';
import { useIsMobile } from '../hooks/useMedia';
import { navigate } from '../router';
import { disposeTerminal } from '../terminals';
import { LEARNING_TITLE, STATUS_TEXT, approveLabel, cx, dotState, designApprovalBlocker, formatTokens, heldForTurnEnd, mergeGate, openPrGate, queuedText, repoFolder, totalTokens, usageTitle } from '../util';
import { AskSheet } from './AskSheet';
import { Busy, Check, ConfirmButton, ContextRing, DatapackName, NotesIcon, StatusDot } from './common';
import { ChangesPanel } from './ChangesPanel';
import { DesignPanel } from './DesignPanel';
import type { MovePreset } from './MoveSessionDialog';
import { EndSessionButton, SessionSheet } from './SessionSheet';
import { ShareDialog } from './ShareDialog';
import { TerminalPane } from './TerminalPane';
import { TopBar } from './TopBar';

/** Where this browser keeps the Review a PR sessions whose PR's design it has opened by itself. */
const REVIEW_DESIGNS_KEY = 'ak-review-designs-shown';

/**
 * True the first time for each Review a PR session (its design then opens by itself), false after:
 * kept in this browser, the last 200 sessions.
 */
function firstReviewDesign(id: string): boolean {
  let ids: string[] = [];
  try {
    const v: unknown = JSON.parse(localStorage.getItem(REVIEW_DESIGNS_KEY) ?? '[]');
    if (Array.isArray(v)) ids = v.filter((x): x is string => typeof x === 'string');
  } catch {
    /* unreadable: start a new list */
  }
  if (ids.includes(id)) return false;
  try {
    localStorage.setItem(REVIEW_DESIGNS_KEY, JSON.stringify([...ids, id].slice(-200)));
  } catch {
    /* storage off: it opens again next time */
  }
  return true;
}

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
  meko,
  onNewSession,
  onMove,
  onOpenPr,
  onRename,
  onMenu,
  onError,
}: {
  project: ProjectWithSessions;
  sessions: Session[];
  session: Session | null;
  mekoWebUrl: string | null;
  meko: MekoStatus;
  onNewSession: () => void;
  /** Opens the Move to another stage dialog for the session on screen. */
  onMove: (preset?: MovePreset) => void;
  /** Opens the Open PR with instructions dialog for the session on screen. */
  onOpenPr: () => void;
  /** Opens the Rename session dialog. */
  onRename: (sessionId: string) => void;
  /** Opens the projects drawer (phone layout). */
  onMenu: () => void;
  onError: (msg: string) => void;
}) {
  const mobile = useIsMobile();
  // One side panel at a time.
  const [panel, setPanel] = useState<'design' | 'changes' | null>(null);
  const toggle = (p: 'design' | 'changes') => setPanel((o) => (o === p ? null : p));
  const [sheet, setSheet] = useState(false);
  // Bumped to show the design's questions (Approve pressed with questions open).
  const [questionsAt, setQuestionsAt] = useState(0);
  const [sharing, setSharing] = useState(false);
  // The questions Claude is asking, opened from the bar above the terminal or the ⋯ sheet.
  const [asking, setAsking] = useState(false);
  const tabsRef = useRef<HTMLDivElement>(null);
  useEffect(() => {
    setPanel(null);
    setSheet(false);
    setSharing(false);
    setAsking(false);
    setQuestionsAt(0);
    tabsRef.current?.querySelector('.tab.sel')?.scrollIntoView({ block: 'nearest', inline: 'nearest' });
  }, [session?.id]);
  // Showing the questions is for the panel open now: one opened later starts on the design.
  useEffect(() => {
    if (panel !== 'design') setQuestionsAt(0);
  }, [panel]);
  // During Design, show the design as soon as it's written, and again whenever it's revised.
  const designAt = session?.stage === 'design' ? session.designUpdatedAt : null;
  useEffect(() => {
    if (designAt) setPanel('design');
  }, [session?.id, designAt]);
  // A review whose PR has a design opens it once, the first time the session shows with it.
  const reviewDesign = session?.stage === 'reviewing-pr' && session.hasDesign ? session.id : null;
  useEffect(() => {
    if (reviewDesign && firstReviewDesign(reviewDesign)) setPanel('design');
  }, [reviewDesign]);

  // One session action at a time, so a double tap can't send two merges or resumes.
  const [acting, setActing] = useState(false);
  const actingRef = useRef(false);
  const act = async (action: SessionAction, req?: CloseSessionRequest) => {
    if (!session || actingRef.current) return;
    // With open questions, approving starts with answering them; the approval goes from there.
    if (action === 'approve-design' && session.designQuestions.open > 0) {
      setPanel('design');
      setSheet(false);
      setQuestionsAt((n) => n + 1);
      return;
    }
    actingRef.current = true;
    setActing(true);
    try {
      if (action === 'approve-design') await approveDesign(session.id);
      // A Discard that waits for the learn step leaves the session (and its terminal) running until then.
      else if ((await api.sessionAction(session.id, action, req)).desired === 'archived') disposeTerminal(session.id);
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
  const openQuestions = session?.stage === 'design' ? session.designQuestions.open : 0;
  const rv = session?.reviewingPr ?? null;
  // On a phone the gate lives in the ⋯ sheet; a dot on ⋯ says when it's waiting on you (or Claude asks something).
  const gateReady = !!session && (!!session.asking || (!session.busy && (
    (session.stage === 'design' && session.hasDesign)
    || (session.stage === 'review' && session.canMerge)
    || (session.stage === 'reviewing-pr' && !!rv && (rv.prState !== 'OPEN' || rv.headSha !== rv.reviewSha || rv.designUnread)))));
  // Sharing the project with a PR's reviewers is sharing its dedicated datapack (through Meko's API).
  const canShare = !!dedicated && meko.configured;
  // AgentK saves what the session learned to the project's datapack: it needs Meko and Claude there.
  const canLearn = !!session && !!dedicated && meko.configured && session.status !== 'exited';
  const learnDisabled = acting || !!session?.busy || !!session?.learning;
  // Merge & Close and Discard save what the session learned first, unless asked not to. The merge's
  // step runs once the PR is merged, so it's offered even while Claude has exited (Resume may come first).
  const learnTo = canLearn && dedicated ? dedicated.name : null;
  const mergeLearnTo = dedicated && meko.configured ? dedicated.name : null;
  const goHome = () => navigate({ view: 'home', projectId: project.id });

  // A session in another repo than the project's names it first.
  const branchText = session && (
    <span title={session.repoPath !== project.repoPath ? session.repoPath : undefined}>
      {session.repoPath && session.repoPath !== project.repoPath ? `${repoFolder(session.repoPath)} · ` : ''}branch {session.branch}
    </span>
  );
  const tokensText = session && (
    <span title={usageTitle(session.usage)}>
      tokens {formatTokens(totalTokens(session.usage))} · {formatTokens(session.usage.output)} out
    </span>
  );
  // Measured by AgentK's mod after each turn; null without it. The phone's sheet has no tooltips, so it writes the percent too.
  const contextText = (showPercent: boolean) =>
    session && session.contextPercent !== null && <ContextRing percent={session.contextPercent} showPercent={showPercent} />;
  const mekoText = (
    <span title="The project's Meko datapack">
      Meko:{dedicated ? <DatapackName id={dedicated.id} name={dedicated.name} mekoWebUrl={mekoWebUrl} /> : '—'}
      {sharedCount > 0 ? ` · +${sharedCount} shared` : ''}
    </span>
  );
  const stageText = session && (
    <span>
      {session.stage === 'pr' ? 'opening PR' : STAGE_LABELS[session.stage].toLowerCase()}
      {rv && session.stage === 'reviewing-pr' && (
        <>
          {` · at ${rv.reviewSha.slice(0, 7)}`}
          {rv.prState !== 'OPEN' ? (
            ` · PR ${rv.prState === 'MERGED' ? 'merged' : 'closed'}`
          ) : rv.headSha !== rv.reviewSha ? (
            <span className="warn-text"> · {newCommitsText(rv.newCommits)}</span>
          ) : (
            ''
          )}
        </>
      )}
      {/* Only Review and Merge & Close follow the PR; after a move back to Code its state would be stale. */}
      {session.prNumber && (session.stage === 'review' || session.stage === 'merge' || session.stage === 'done')
        ? ` · ${REVIEW_TEXT[session.reviewState]} · ${CI_TEXT[session.ciState]}`
        : ''}
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
          // A double-click renames it (on a phone: Rename session in the ⋯ sheet).
          onDoubleClick={mobile ? undefined : () => onRename(s.id)}
          title={[
            s.name,
            (dotState(s) === 'learning' ? LEARNING_TITLE : STATUS_TEXT[dotState(s)]) +
              (s.statusDetail && s.status !== 'starting' ? `: ${s.statusDetail}` : ''),
            STAGE_LABELS[s.stage],
            `${formatTokens(totalTokens(s.usage))} tokens`,
          ].join(' · ') + (mobile ? '' : '\nDouble-click to rename')}
        >
          <StatusDot status={dotState(s)} phase={s.id} />
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
          learnTo={mergeLearnTo}
          onMove={onMove}
          onOpenPr={onOpenPr}
          onShare={canShare ? () => setSharing(true) : undefined}
          acting={acting}
          actions={
            <>
              {session?.prUrl && (
                <a className="btn small" href={session.prUrl} target="_blank" rel="noreferrer" title={session.prUrl}>
                  {rv ? `${rv.repo.split('/')[1] ?? rv.repo} #${session.prNumber}` : `PR #${session.prNumber}`}
                </a>
              )}
              <button className="btn small" onClick={goHome}>
                Project home
              </button>
              {canDesign && (
                <button className={cx('btn small', panel === 'design' && 'on')} onClick={() => toggle('design')}>
                  Design doc
                  {openQuestions > 0 && (
                    <span className="count-badge" title={`${openQuestions} open questions`} aria-label={`, ${openQuestions} open questions`}>
                      {openQuestions}
                    </span>
                  )}
                </button>
              )}
              {canLearn && (
                <button
                  className="btn small"
                  disabled={learnDisabled}
                  title={`Have AgentK save what this session learned to ${dedicated?.name ?? 'Meko'}`}
                  onClick={() => act('learn')}
                >
                  {session?.learning ? <Busy label="Saving learnings" /> : 'Save learnings'}
                </button>
              )}
              {session?.status === 'exited' && (
                <button className="btn small primary" disabled={acting} onClick={() => act('resume')}>
                  Resume
                </button>
              )}
              {session && <EndSessionButton key={session.id} session={session} onAct={act} learnTo={learnTo} disabled={acting} />}
            </>
          }
        />
      )}

      {!mobile && tabs}

      <div className="workarea">
        {/* Siblings need distinct keys: with the same key, React lost the terminal of a session left with
            its design or changes open, and kept it on screen beside the next one. */}
        {session ? (
          <TerminalPane key={`term-${session.id}`} session={session} onAsk={() => setAsking(true)} />
        ) : (
          <div className="empty-main">
            <h2>No sessions in {project.name}</h2>
            <p className="muted">
              Start a session for a task. It gets its own branch and worktree. Pick what it's for: exploring, planning
              then implementing, or coding straight away.
            </p>
            <button className="btn primary" onClick={onNewSession}>
              New session
            </button>
          </div>
        )}
        {session && panel === 'design' && (
          <DesignPanel
            key={`design-${session.id}`}
            session={session}
            showQuestions={questionsAt}
            onClose={() => setPanel(null)}
            onApprove={() => act('approve-design')}
          />
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
          {!mobile && contextText(false)}
          {!mobile && mekoText}
          {stageText}
        </footer>
      )}

      {session && sheet && (
        <SessionSheet
          session={session}
          stage={
            <StageBar
              session={session}
              project={project}
              onAct={act}
              learnTo={mergeLearnTo}
              onMove={(preset) => {
                setSheet(false);
                onMove(preset);
              }}
              onOpenPr={() => {
                setSheet(false);
                onOpenPr();
              }}
              onShare={
                canShare
                  ? () => {
                    setSheet(false);
                    setSharing(true);
                  }
                  : undefined
              }
              acting={acting}
              actions={null}
              reasons
            />
          }
          canDesign={canDesign}
          canLearn={canLearn}
          learnDisabled={learnDisabled}
          learnTo={learnTo}
          openQuestions={openQuestions}
          acting={acting}
          info={
            <>
              {branchText}
              {tokensText}
              {contextText(true)}
              {mekoText}
              {stageText}
            </>
          }
          onDesign={() => setPanel('design')}
          onAsk={() => setAsking(true)}
          onHome={goHome}
          onMekoCalls={project.settings.mekoLog ? () => navigate({ view: 'meko', projectId: project.id, sessionId: session.id }) : undefined}
          onMove={() => onMove()}
          onRename={() => onRename(session.id)}
          onAct={act}
          onClose={() => setSheet(false)}
        />
      )}
      {session && asking && <AskSheet session={session} onClose={() => setAsking(false)} />}
      {session && sharing && dedicated && (
        <ShareDialog
          projectId={project.id}
          projectName={project.name}
          datapack={dedicated}
          focusSessionId={session.id}
          onClose={() => setSharing(false)}
        />
      )}
    </div>
  );
}

/** "3 new commits", or what a force-push means for the review. */
function newCommitsText(n: number | null): string {
  if (n === null) return 'PR force-pushed';
  return n === 1 ? '1 new commit' : `${n} new commits`;
}

/**
 * The + notes button beside Open PR: opens the Open PR with instructions dialog, or, with an Open
 * PR queued, the instructions it goes with.
 */
function InstructionsButton({ queued, disabled, title, onClick }: { queued: boolean; disabled: boolean; title: string; onClick: () => void }) {
  return (
    <button
      className="btn small gate-note"
      disabled={disabled}
      title={title}
      aria-label={queued ? "The queued Open PR's instructions" : 'Open PR with instructions'}
      onClick={onClick}
    >
      +<NotesIcon />
    </button>
  );
}

/** A gate pressed early (Open PR or Merge & Close), shown as a pill with Cancel until it goes. */
function queuedGate(
  session: Session,
  blocked: boolean,
  onAct: (a: SessionAction, req?: CloseSessionRequest) => Promise<void>,
  title: string,
  onInstructions?: () => void,
): ReactNode {
  return (
    <span className="gate-group">
      {onInstructions && (
        <InstructionsButton
          queued
          disabled={blocked}
          title={session.queuedInstructions ? 'Change the instructions the queued Open PR goes with' : 'Give the queued Open PR instructions for AgentK'}
          onClick={onInstructions}
        />
      )}
      <span className="gate-pill" title={title}>
        {queuedText(session)}
      </span>
      <button className="btn small" disabled={blocked} onClick={() => onAct('cancel-queued')}>
        Cancel
      </button>
    </span>
  );
}

function StageBar({
  session,
  project,
  onAct,
  onMove,
  onOpenPr,
  onShare,
  learnTo = null,
  acting = false,
  actions,
  reasons = false,
}: {
  session: Session | null;
  project: ProjectWithSessions;
  onAct: (a: SessionAction, req?: CloseSessionRequest) => Promise<void>;
  /** The datapack Merge & Close saves what the session learned to first; null when it has none (or Meko is off). */
  learnTo?: string | null;
  onMove: (preset?: MovePreset) => void;
  /** Opens the Open PR with instructions dialog. */
  onOpenPr: () => void;
  /** Opens the Share dialog at the PR's reviewers; absent when the project can't be shared from here. */
  onShare?: () => void;
  /** A session action is in flight. */
  acting?: boolean;
  actions: ReactNode;
  /** Say under the stages why a disabled gate can't be passed (the phone's sheet, where there are no tooltips). */
  reasons?: boolean;
}) {
  // Don't save learnings at Merge & Close, for the session it was checked on (unchecked by default).
  const [skipFor, setSkipFor] = useState<string | null>(null);
  const skipLearn = !!session && skipFor === session.id;
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
  const kind = kindOf(session.kind);
  const stages: Stage[] = KIND_STAGES[kind];
  const idx = session.stage === 'done' ? stages.length : stages.indexOf(stepOf(session.stage));
  // A task's steps are numbered from 0 (Explore), so Design is 1 and Merge & Close 4.
  const first = kind === 'task' ? 0 : 1;
  // Other stages the session can be moved to open the move dialog.
  const targets: Stage[] = moveBlockedIn(session.stage) ? [] : moveTargets(kind);
  const midTurn = session.status === 'working' || session.status === 'starting';
  const blocked = session.busy || acting;
  // Why the Design or Code gate is closed, if it is.
  let reason: string | null = null;

  let gate: ReactNode = null;
  switch (session.stage) {
    case 'explore':
      reason = midTurn ? 'Wait for AgentK to finish its turn.' : null;
      gate = (
        <button
          className="gate"
          disabled={blocked || !!reason}
          title={reason ?? 'Move this session on to Design, Code or a PR'}
          onClick={() => onMove({ stage: 'design' })}
        >
          Move on…
        </button>
      );
      break;
    case 'design': {
      if (session.approvalQueued) {
        gate = (
          <span className="gate-group">
            <span className="gate-pill" title="The approval goes to AgentK when its turn ends, unless the turn changes the design">
              ✓ Approved: goes when the turn ends
            </span>
            <button className="btn small" disabled={blocked} onClick={() => onAct('cancel-approval')}>
              Cancel approval
            </button>
          </span>
        );
        break;
      }
      // With open questions the gate only shows them, so it isn't held up.
      reason = session.designQuestions.open ? null : designApprovalBlocker(session);
      const waits = heldForTurnEnd(session);
      gate = (
        <button
          className="gate"
          disabled={blocked || !!reason}
          title={reason ?? (session.designQuestions.open
            ? 'Answer the open questions, then approve'
            : waits ? 'AgentK is mid-turn: the approval goes when the turn ends, if the design stays the same' : 'Approve the design and move to Code')}
          onClick={() => onAct('approve-design')}
        >
          {approveLabel(session.designQuestions.open)}
        </button>
      );
      break;
    }
    case 'code': {
      if (session.queued === 'open-pr') {
        gate = queuedGate(session, blocked, onAct, 'The PR prompt goes to AgentK when its turn ends', onOpenPr);
        break;
      }
      const pr = openPrGate(session);
      reason = pr.reason;
      gate = (
        <span className="gate-group">
          <InstructionsButton queued={false} disabled={blocked || !!reason} title={reason ?? 'Open PR with instructions for AgentK'} onClick={onOpenPr} />
          <button className="gate" disabled={blocked || !!reason} title={pr.title} onClick={() => onAct('open-pr')}>
            {pr.label}
          </button>
        </span>
      );
      break;
    }
    case 'pr':
      gate = <span className="gate-pill">
          <Busy label="Opening PR" />
        </span>;
      break;
    case 'review': {
      const merge = mergeGate(session);
      gate = (
        <span className="gate-group">
          {onShare && session.prNumber && (
            <button className="btn small" onClick={onShare} title="Share this project with the PR's reviewers, so they can review it with AgentK">
              Share with reviewers
            </button>
          )}
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
          {project.settings.reviewer !== 'people' && session.reviewState !== 'refused' && (
            <button
              className="btn small"
              disabled={blocked || midTurn || session.reviewState === 'in_review' || session.reviewState === 'addressing'}
              onClick={() => onAct('request-review')}
              title="Ask AgentK to review the latest push"
            >
              {session.agentkSeen ? 'Request another round' : 'Request AgentK review'}
            </button>
          )}
          {session.queued === 'merge' ? (
            queuedGate(session, blocked, onAct, 'The pull request is merged once nothing blocks it')
          ) : (
            <>
              {learnTo && (
                <label className="check skip-learn" title={`Unchecked, AgentK saves what this session learned to ${learnTo} before closing it`}>
                  <input type="checkbox" checked={skipLearn} disabled={blocked} onChange={(e) => setSkipFor(e.target.checked ? session.id : null)} />
                  <span>Don't save learnings</span>
                </label>
              )}
              <button
                className="gate"
                disabled={blocked || !!merge.reason}
                title={merge.title}
                onClick={() => onAct('merge', learnTo && skipLearn ? { skipLearn: true } : undefined)}
              >
                {merge.label}
              </button>
            </>
          )}
        </span>
      );
      break;
    }
    case 'merge':
      gate = <span className="gate-pill"><Busy label="Merging and closing" /></span>;
      break;
    case 'done':
      gate = <span className="gate-pill done">Done</span>;
      break;
    case 'reviewing-pr': {
      const rv = session.reviewingPr;
      const gone = !!rv && rv.prState !== 'OPEN';
      const moved = !!rv && !gone && rv.headSha !== rv.reviewSha;
      // A version of the PR's design AgentK hasn't read: Update to latest has it read, commits or not.
      const designUnread = !!rv && !gone && rv.designUnread;
      reason = midTurn
        ? 'Wait for AgentK to finish its turn.'
        : session.status === 'needs_input'
          ? 'AgentK is waiting on you in the terminal.'
          : session.status === 'exited'
            ? 'Resume the session first.'
            : null;
      gate = (
        <span className="gate-group">
          <span className={cx('pr-pill', (moved || designUnread) && 'moved', gone && 'gone')}>
            {gone ? (rv!.prState === 'MERGED' ? '✓ merged' : 'closed') : moved ? newCommitsText(rv!.newCommits) : designUnread ? 'design updated' : 'up to date'}
          </span>
          {(moved || designUnread) && (
            <button
              className="gate"
              disabled={blocked || !!reason}
              title={reason ?? (moved ? "Check out the PR's latest commits; AgentK sums up what changed" : "AgentK reads the PR's new design and sums it up")}
              onClick={() => onAct('update-pr')}
            >
              Update to latest
            </button>
          )}
          <ConfirmButton
            className={moved || designUnread ? 'btn small' : 'gate'}
            label="Close review"
            confirmLabel="Close review"
            prompt="The session is archived and its review branch deleted."
            onConfirm={() => onAct('close-review')}
            disabled={blocked || midTurn}
          />
        </span>
      );
      break;
    }
  }

  return (
    <div className="stages-wrap">
      <div className="stages-row">
        <div className="stages" aria-label="Stages">
          {stages.map((st, i) => {
            const skipped = session.skippedStages.includes(st);
            const cls = cx('stg', i < idx && !skipped && 'done', skipped && 'skipped', i === idx && 'cur');
            const inner = (
              <>
                <span className="n">{skipped ? '–' : i < idx ? <Check size={10} /> : i + first}</span>
                <span className="stg-label">{STAGE_LABELS[st]}</span>
              </>
            );
            return (
              <span key={st} className="stg-item">
                {i > 0 && <span className={cx('stg-sep', i <= idx && 'done')} />}
                {i !== idx && targets.includes(st) ? (
                  <button
                    type="button"
                    className={cx(cls, 'stg-btn')}
                    disabled={blocked}
                    title={`${skipped ? 'Skipped. ' : ''}${i < idx ? 'Move back' : 'Move'} to ${STAGE_LABELS[st]}`}
                    aria-label={`${i < idx ? 'Move back' : 'Move'} to ${STAGE_LABELS[st]}`}
                    onClick={() => onMove({ stage: st as MoveTarget })}
                  >
                    {inner}
                  </button>
                ) : (
                  <span className={cls} aria-current={i === idx ? 'step' : undefined} title={skipped ? 'Skipped' : undefined}>
                    {inner}
                  </span>
                )}
              </span>
            );
          })}
          {/* The phone layout shows only the current stage's name, here. */}
          <span className="stg-now">
            <b>{STAGE_LABELS[session.stage]}</b>
            {session.stage !== 'done' && stages.length > 1 && idx + first > 0 && (
              <span className="muted">
                {' '}
                {idx + first} of {stages.length - 1 + first}
              </span>
            )}
          </span>
          <span className="stages-gate">{gate}</span>
        </div>
        {actions && <div className="stage-actions">{actions}</div>}
      </div>
      {session.stage === 'review' && !session.canMerge && session.mergeBlockers.length > 0 && (
        <div className="blockers">
          {session.queued === 'merge' ? 'Merges when these clear' : 'Merge is waiting on'}: {session.mergeBlockers.join(' · ')}
        </div>
      )}
      {reasons && reason && <div className="blockers">{reason}</div>}
    </div>
  );
}
