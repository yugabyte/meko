import { useEffect, useRef, useState, type ReactNode } from 'react';
import {
  KIND_STAGES,
  STAGE_LABELS,
  type CloseSessionRequest,
  type MekoStatus,
  type MoveTarget,
  type ProjectWithSessions,
  type ReviewingPr,
  type Session,
  type SessionAction,
  type Stage,
} from '../../shared/types';
import { kindOf, moveBlockedIn, moveTargets, stepOf } from '../../shared/stages';
import { HARNESS_CAPS, agentName, harnessOf } from '../../shared/harnesses';
import { api, errorMessage } from '../api';
import { copyText } from '../clipboard';
import { approveDesign, changingDecisions } from '../design-answers';
import { useIsMobile } from '../hooks/useMedia';
import { navigate } from '../router';
import { disposeTerminal } from '../terminals';
import { closingText, cx, dotState, designGate, isReviewChild, joinedPartSelected, reviewGatesPr, reviewOpens, reviewParent, shownStep, formatCredits, formatTokens, mergeGate, offersLearn, openPrGate, queuedText, repoFolder, reviewTarget, runToMergeShown, runToMergeTitle, sessionTabTitle, sidewaysWheel, totalTokens, updateShowing, usageTitle } from '../util';
import { AskSheet } from './AskSheet';
import { HeldReviewsSheet } from './HeldReviewsSheet';
import { Busy, Check, ContextRing, DatapackName, Elapsed, HarnessLogo, NotesIcon, ReviewSegment, RunToMergeCheck, RunToMergeMark, StageChip, StatusDot } from './common';
import { ReviewFindings } from './ReviewFindings';
import { DESCRIBE_LOST, countsText, reviewDoneText, reviewHolds, reviewRunning, reviewSegmentText, type CodeReviewSummary } from '../../shared/codereview';
import { ChangesPanel } from './ChangesPanel';
import { DesignPanel } from './DesignPanel';
import type { MovePreset } from './MoveSessionDialog';
import { EndSessionButton, LocalReviewRows, SessionSheet } from './SessionSheet';
import { newCommitsText, ReviewPrGate } from './ReviewPrGate';
import { MekoLinkDialog } from './MekoLinkDialog';
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
  meko,
  onNewSession,
  onMove,
  onOpenPr,
  onRename,
  onLinkWorkItem,
  onToTop,
  onMenu,
  showTabs,
  harnessLogos,
  findingsRequest,
  onFindingsTaken,
  onFindingsShown,
  onOpenFindings,
  onError,
}: {
  project: ProjectWithSessions;
  sessions: Session[];
  session: Session | null;
  meko: MekoStatus;
  onNewSession: () => void;
  /** Opens the Move to another stage dialog for the session on screen. */
  onMove: (preset?: MovePreset) => void;
  /** Opens the Open PR with instructions dialog for the session on screen. */
  onOpenPr: () => void;
  /** Opens the Rename session dialog. */
  onRename: (sessionId: string) => void;
  /** Opens the dialog that links a session to a work item. */
  onLinkWorkItem: (sessionId: string) => void;
  /** Moves a session to the top of the project's list (the ⋯ sheet; the rail has its own button). */
  onToTop: (sessionId: string) => void;
  /** Opens the projects drawer (phone layout). */
  onMenu: () => void;
  /** Show the session tabs, above the terminal or in a phone's app bar: the rail doesn't list the sessions, or (the wide layout only) is hidden. */
  showTabs: boolean;
  /** Show each session's harness logo in its tab: open sessions run on more than one harness and the setting is on. */
  harnessLogos: boolean;
  /** A task's findings asked open (its magnifier in the rail) or closed (the rail's task part), until taken. */
  findingsRequest: { id: string; open: boolean } | null;
  /** The request was acted on. */
  onFindingsTaken: () => void;
  /** The task whose findings are open beside its terminal, or null: for the rail's row. */
  onFindingsShown: (taskId: string | null) => void;
  /** Shows a task's local review findings, beside its terminal: from its magnifier, the gate or the ⋯ sheet. */
  onOpenFindings: (task: Session) => void;
  onError: (msg: string) => void;
}) {
  const mobile = useIsMobile();
  // One side panel at a time.
  const [panel, setPanel] = useState<'design' | 'changes' | 'findings' | null>(null);
  const toggle = (p: 'design' | 'changes' | 'findings') => setPanel((o) => (o === p ? null : p));
  // A task's code-review session (its part of the task's tab) and the task it reviews.
  const child = session && isReviewChild(session) ? session : null;
  const parent = session ? reviewParent(project.sessions, session) : undefined;
  // The review's screen (its terminal, its findings beside it) while its steps run (or it failed); its
  // findings beside the task's terminal while the task fixes a round's findings and once it's done.
  const openReviewTerminal = (task: Session) => {
    if (task.codeReview?.sessionId) navigate({ view: 'project', projectId: project.id, sessionId: task.codeReview.sessionId });
  };
  const openFindings = (task: Session) => {
    if (task.id === session?.id) setPanel('findings');
    else onOpenFindings(task);
  };
  const openReview = (task: Session) => (reviewOpens(task) === 'session' ? openReviewTerminal(task) : openFindings(task));
  const [sheet, setSheet] = useState(false);
  // Bumped to show the design's questions (Design Review, or Approve pressed with questions open).
  const [questionsAt, setQuestionsAt] = useState(0);
  // Bumped to bring the open design panel's approve button into view (Design Review).
  const [approveAt, setApproveAt] = useState(0);
  const [sharing, setSharing] = useState(false);
  // The questions Claude is asking, opened from the bar above the terminal or the ⋯ sheet.
  const [asking, setAsking] = useState(false);
  // The reviews held for the user, opened from the stage bar's Review gate.
  const [heldOpen, setHeldOpen] = useState(false);
  // The session whose worktree path was just copied from the footer, for 2 s from the last copy
  // (each copy is a new object, so another click restarts the timer).
  const [copiedPath, setCopiedPath] = useState<{ id: string } | null>(null);
  useEffect(() => {
    if (!copiedPath) return;
    const t = setTimeout(() => setCopiedPath(null), 2000);
    return () => clearTimeout(t);
  }, [copiedPath]);
  const tabsRef = useRef<HTMLDivElement>(null);
  useEffect(() => {
    setPanel(null);
    setSheet(false);
    setSharing(false);
    setAsking(false);
    setHeldOpen(false);
    setQuestionsAt(0);
    setApproveAt(0);
    // A review's findings open beside its terminal, on a desktop; a phone opens them from the ⋯ sheet.
    if (child && !mobile) setPanel('findings');
    // eslint-disable-next-line react-hooks/exhaustive-deps -- only when another session comes on screen
  }, [session?.id]);
  // After the reset above: a request for this task's findings, taken once (it stays in App, which
  // outlives this view, so a request left untaken would open them again on the way back).
  useEffect(() => {
    if (!findingsRequest || findingsRequest.id !== session?.id) return;
    setPanel((p) => (findingsRequest.open ? 'findings' : p === 'findings' ? null : p));
    onFindingsTaken();
  }, [findingsRequest, session?.id, onFindingsTaken]);
  // Which task's findings are open, for the rail's row; none once this view goes.
  const findingsTask = session && !child && session.codeReview && panel === 'findings' ? session.id : null;
  useEffect(() => {
    onFindingsShown(findingsTask);
  }, [findingsTask, onFindingsShown]);
  useEffect(() => () => onFindingsShown(null), [onFindingsShown]);
  // Also when the desktop tabs mount, as the rail stops listing the sessions or is hidden.
  useEffect(() => {
    tabsRef.current?.querySelector('.tab.sel, .review-seg.sel')?.scrollIntoView({ block: 'nearest', inline: 'nearest' });
  }, [session?.id, showTabs]);
  // Showing the questions is for the panel open now: one opened later starts on the design.
  useEffect(() => {
    if (panel !== 'design') {
      setQuestionsAt(0);
      setApproveAt(0);
    }
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
  // The view isn't remounted per session, so the action in flight names its session.
  const [actingOn, setActingOn] = useState<{ id: string; action: SessionAction } | null>(null);
  const acting = actingOn !== null;
  const actingRef = useRef(false);
  // An Update to latest that went through, with the PR as it stood: still Updating until the state shows it.
  const [updated, setUpdated] = useState<{ id: string; pr: ReviewingPr } | null>(null);
  useEffect(() => {
    if (!updated) return;
    // In case the state never shows it (a dead events socket), the button comes back.
    const t = setTimeout(() => setUpdated(null), 5000);
    return () => clearTimeout(t);
  }, [updated]);
  const act = async (action: SessionAction, req?: CloseSessionRequest) => {
    if (!session || actingRef.current) return;
    // With open questions (or a change to one of AgentK's decisions), approving starts with the
    // questions; the approval goes from there.
    if (action === 'approve-design' && (session.designQuestions.open > 0 || changingDecisions(session.id))) {
      setPanel('design');
      setSheet(false);
      setQuestionsAt((n) => n + 1);
      return;
    }
    actingRef.current = true;
    setActingOn({ id: session.id, action });
    const pr = session.reviewingPr;
    // The local review's actions are its task's, also from the review's own screen.
    const target = action.startsWith('review-') && session.parentId ? session.parentId : session.id;
    try {
      if (action === 'approve-design') await approveDesign(session.id);
      else {
        // A Discard that waits for the learn step leaves the session (and its terminal) running until then.
        if ((await api.sessionAction(target, action, req)).desired === 'archived') disposeTerminal(target);
        if (action === 'update-pr' && pr) setUpdated({ id: session.id, pr });
      }
    } catch (e) {
      onError(errorMessage(e));
    } finally {
      actingRef.current = false;
      setActingOn(null);
    }
  };
  // Design Review: the design, on its questions when there are any to answer; with the panel open
  // already, its approve button. The approval itself goes from the panel, with the answers.
  const openDesignReview = () => {
    if (!session) return;
    setSheet(false);
    const target = reviewTarget(session.designQuestions.open, changingDecisions(session.id), panel === 'design');
    if (target === 'approve') {
      setApproveAt((n) => n + 1);
      return;
    }
    setPanel('design');
    if (target === 'questions') setQuestionsAt((n) => n + 1);
  };
  // Run to merge's checkbox beside the gates (StageBar) and the design panel's Approve.
  const setAutoRun = async (on: boolean) => {
    if (!session) return;
    try {
      await api.updateSession(session.id, { autoRun: on });
    } catch (e) {
      onError(errorMessage(e));
    }
  };
  const updating = !!session && (
    (actingOn?.id === session.id && actingOn.action === 'update-pr')
    || (updated?.id === session.id && !updateShowing(updated.pr, session.reviewingPr)));

  const pending = session ? session.comments.drafts + session.comments.queued : 0;
  const changedFiles = session?.diffStat?.files ?? 0;
  const filesText = `${changedFiles.toLocaleString()} ${changedFiles === 1 ? 'file' : 'files'}`;
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
  // While AgentK can't share (the Meko on this computer), it points to Meko Cloud instead.
  const canShare = !!dedicated && meko.configured;
  const canLearn = !!session && offersLearn(session, !!dedicated, meko.configured);
  const learnDisabled = acting || !!session?.busy || !!session?.learning;
  // Merge & Close and Discard save what the session learned first, unless asked not to. The merge's
  // step runs once the PR is merged, so it's offered even while Claude has exited (Resume may come first).
  const learnTo = session && session.status !== 'exited' && dedicated && meko.configured ? dedicated.name : null;
  const mergeLearnTo = dedicated && meko.configured ? dedicated.name : null;
  // Files held in the terminal's strip can go to the dedicated datapack's knowledge base (Upload to Meko).
  const mekoTo = dedicated && meko.configured ? dedicated.name : null;
  const goHome = () => navigate({ view: 'home', projectId: project.id });

  // A session in another repo than the project's names it first.
  const branchLabel = session && (
    <>
      {session.repoPath && session.repoPath !== project.repoPath ? `${repoFolder(session.repoPath)} · ` : ''}branch {session.branch}
    </>
  );
  const branchText = session && (
    <span title={session.repoPath !== project.repoPath ? session.repoPath : undefined}>{branchLabel}</span>
  );
  // On a desktop, clicking the branch copies the worktree's path; the label says so for a moment.
  // Not while a start is still making the worktree: the path doesn't exist yet.
  const copyPath = async () => {
    if (!session) return;
    if (await copyText(session.worktreePath)) setCopiedPath({ id: session.id });
    else onError(`Couldn't copy the path. It is ${session.worktreePath}`);
  };
  const branchButton = session && (session.startup?.step === 'worktree' ? branchText : (
    <button className="foot-btn" onClick={() => void copyPath()} title={`${session.worktreePath}\nClick to copy this path`}>
      {copiedPath?.id === session.id ? 'path copied' : branchLabel}
    </button>
  ));
  // Kiro bills credits; its token counts are often zero, so a Kiro session shows its credits and its agent.
  const tokensText = session && (HARNESS_CAPS[harnessOf(session.harness)].usage === 'credits' ? (
    <span title={`${HARNESS_CAPS.kiro.label} · ${session.usage.messages} turns`}>
      {HARNESS_CAPS.kiro.short} · {formatCredits(session.usage.credits ?? 0)}
    </span>
  ) : (
    <span title={usageTitle(session.usage)}>
      tokens {formatTokens(totalTokens(session.usage))} · {formatTokens(session.usage.output)} out
    </span>
  ));
  // Measured by AgentK's mod after each turn; null without it. The phone's sheet has no tooltips, so it writes the percent too.
  const contextText = (showPercent: boolean) =>
    session && session.contextPercent !== null && <ContextRing percent={session.contextPercent} showPercent={showPercent} agent={agentName(session)} />;
  const mekoText = (
    <span title="The project's Meko datapack">
      Meko:{dedicated ? <DatapackName id={dedicated.id} name={dedicated.name} meko={meko} /> : '—'}
      {sharedCount > 0 ? ` · +${sharedCount} shared` : ''}
    </span>
  );
  const stageText = session && (
    <span>
      {session.stage === 'pr' ? 'opening PR' : STAGE_LABELS[session.stage].toLowerCase()}
      <RunToMergeMark session={session} size={10} />
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

  // On a phone the tabs sit in the app bar, between the drawer button and the session's actions. On a
  // desktop they sit above the terminal. Neither shows them while the rail lists the sessions (a setting)
  // and is shown; the phone's bar then names the session, with its dot, and marks the drawer button while
  // another session needs you.
  // A task with a local review is one joined tab: the task's part, then the review's (the magnifier),
  // inside one border. The task's part opens its session, the review's its findings beside it.
  const tabs = (
    <div
      className="tabs"
      role="tablist"
      ref={tabsRef}
      onWheel={(e) => {
        e.currentTarget.scrollLeft += sidewaysWheel(e, e.currentTarget.clientWidth);
      }}
    >
      {sessions.map((s) => {
        const part = joinedPartSelected(s, session?.id, panel === 'findings');
        const tab = (
          <button
            key={s.id}
            role="tab"
            aria-selected={part === 'task'}
            className={cx('tab', part === 'task' && 'sel')}
            onClick={() => {
              // The task's part, while its findings are open, goes back to its terminal alone.
              if (s.id !== session?.id) navigate({ view: 'project', projectId: project.id, sessionId: s.id });
              else if (part === 'review') setPanel(null);
            }}
            onDoubleClick={mobile ? undefined : () => onRename(s.id)}
            title={sessionTabTitle(s) + (mobile ? '' : '\nDouble-click to rename')}
          >
            <StatusDot status={dotState(s)} phase={s.id} />
            {harnessLogos && <HarnessLogo harness={s.harness} />}
            <span className="tab-name">{s.name}</span>
            <StageChip session={s} />
          </button>
        );
        if (!s.codeReview) return tab;
        return (
          <span key={s.id} className={cx('tab-joined', part && 'has-sel')}>
            {tab}
            <ReviewSegment review={s.codeReview} selected={part === 'review'} className="tab-review" tab onOpen={() => openReview(s)} />
          </span>
        );
      })}
      <button className="tab new" onClick={onNewSession} title="New session" aria-label="New session">
        +
      </button>
    </div>
  );

  return (
    <div className="project-view">
      {mobile ? (
        <TopBar
          onMenu={onMenu}
          menuDot={!showTabs && sessions.some((s) => s.id !== session?.id && dotState(s) === 'needs_input')}
          sub={showTabs || !session ? undefined : project.name}
          title={showTabs ? undefined : child ? `Local review of ${parent?.name ?? 'a task'}` : (session?.name ?? project.name)}
          icon={!showTabs && session ? <StatusDot status={dotState(session)} phase={session.id} /> : undefined}
        >
          {showTabs && tabs}
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
          onAct={act}
          onReview={openDesignReview}
          onOpenLocalReview={session && !child ? () => openFindings(session) : undefined}
          onOpenReviewTerminal={session && !child ? () => openReviewTerminal(session) : undefined}
          designOpen={panel === 'design'}
          onAutoRun={setAutoRun}
          learnTo={mergeLearnTo}
          onMove={onMove}
          onOpenPr={onOpenPr}
          onShare={canShare ? () => setSharing(true) : undefined}
          onHeld={() => setHeldOpen(true)}
          acting={acting}
          updating={updating}
          actions={child ? (
            <>
              {parent && (
                <button className="btn small" onClick={() => navigate({ view: 'project', projectId: project.id, sessionId: parent.id })} title={`Back to ${parent.name}`}>
                  ← {parent.name}
                </button>
              )}
              <button className={cx('btn small', panel === 'findings' && 'on')} onClick={() => toggle('findings')}>
                Findings
              </button>
              {child.status === 'exited' && (
                <button className="btn small primary" disabled={acting} onClick={() => act('resume')}>
                  Resume
                </button>
              )}
            </>
          ) : (
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
          )}
        />
      )}

      {!mobile && showTabs && tabs}

      <div className="workarea">
        {/* Siblings need distinct keys: with the same key, React lost the terminal of a session left with
            its design or changes open, and kept it on screen beside the next one. */}
        {session ? (
          <TerminalPane key={`term-${session.id}`} session={session} onAsk={() => setAsking(true)} mekoTo={mekoTo} />
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
            datapacks={project.datapacks}
            showQuestions={questionsAt}
            approveAt={approveAt}
            onClose={() => setPanel(null)}
            onApprove={() => act('approve-design')}
            onAutoRun={setAutoRun}
          />
        )}
        {session && panel === 'changes' && <ChangesPanel key={`changes-${session.id}`} session={session} onClose={() => setPanel(null)} />}
        {session && panel === 'findings' && (child || session.codeReview) && (
          <ReviewFindings
            key={`findings-${session.id}`}
            session={session}
            onClose={() => setPanel(null)}
            onTerminal={!child && session.codeReview?.sessionId ? () => openReviewTerminal(session) : undefined}
            onAct={!child && !acting ? (a) => void act(a) : undefined}
          />
        )}
      </div>

      {session && (
        <footer className="foot">
          {!mobile && branchButton}
          {session.diffStat && (
            <button
              className={cx('foot-btn', panel === 'changes' && 'on')}
              onClick={() => toggle('changes')}
              aria-expanded={panel === 'changes'}
              title={`Show the files changed on this branch since ${session.baseBranch} (${filesText})`}
            >
              {!mobile && `${filesText} · `}
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
              onAct={act}
              onReview={openDesignReview}
              onOpenLocalReview={!child ? () => {
                setSheet(false);
                openFindings(session);
              } : undefined}
              onOpenReviewTerminal={!child ? () => {
                setSheet(false);
                openReviewTerminal(session);
              } : undefined}
              designOpen={panel === 'design'}
              onAutoRun={setAutoRun}
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
              onHeld={() => {
                setSheet(false);
                setHeldOpen(true);
              }}
              acting={acting}
              updating={updating}
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
          onWorkItem={() => onLinkWorkItem(session.id)}
          onToTop={sessions[0]?.id !== session.id ? () => onToTop(session.id) : undefined}
          onAct={act}
          onClose={() => setSheet(false)}
          child={!!child}
          reviewRows={
            <LocalReviewRows
              session={session}
              parent={parent}
              projectReview={project.settings.codeReview ?? {}}
              acting={acting}
              onOpen={() => openReview(session)}
              onFindings={() => setPanel('findings')}
              onBack={() => parent && navigate({ view: 'project', projectId: project.id, sessionId: parent.id })}
              onAct={act}
              onClose={() => setSheet(false)}
              onError={onError}
            />
          }
        />
      )}
      {session && asking && <AskSheet session={session} onClose={() => setAsking(false)} />}
      {session && heldOpen && <HeldReviewsSheet session={session} onClose={() => setHeldOpen(false)} />}
      {session && sharing && dedicated && !meko.sharing && (
        <MekoLinkDialog sharing={meko.source} local={meko.local} onClose={() => setSharing(false)} />
      )}
      {session && sharing && dedicated && meko.sharing && (
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

/**
 * A gate pressed early (Open PR or Merge & Close), or kept queued by Run to merge, shown as a pill
 * with Cancel until it goes, beside Run to merge's box. Cancel on Run to merge's gate turns it off.
 */
function queuedGate(
  session: Session,
  blocked: boolean,
  onAct: (a: SessionAction, req?: CloseSessionRequest) => Promise<void>,
  onAutoRun: (on: boolean) => Promise<void>,
  title: string,
  onInstructions?: () => void,
): ReactNode {
  return (
    <span className="gate-group">
      <RunToMergeCheck session={session} disabled={blocked} onChange={onAutoRun} />
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

/**
 * A task's local review in its stage bar's gate (in Code, while it holds Open PR) or in the review
 * session's own: where it is with Cancel while it runs, the task fixing a round's findings, or why
 * it stopped with Retry and Open anyway (or only that it stopped, for one cancelled because the task
 * left Code, which holds nothing). While the review waits on you in its terminal, Open terminal goes
 * there. The actions are the task's (`act` sends them there).
 */
function reviewGate(
  r: CodeReviewSummary,
  blocked: boolean,
  onAct: (a: SessionAction) => Promise<void>,
  onOpen?: () => void,
  onTerminal?: () => void,
  midTurn = false,
): ReactNode {
  // Past an interrupted turn of the task's (its PR description, or a round's fixes), Open PR goes on.
  const reviewNow = (title: string) => (
    <button className="gate" disabled={blocked || midTurn} onClick={() => onAct('open-pr')} title={title}>
      Review now
    </button>
  );
  const open = onOpen && (
    <button className="btn small" onClick={onOpen} title="Show the local review's findings">
      Show findings
    </button>
  );
  if (reviewHolds(r)) {
    return (
      <span className="gate-group">
        <span className="gate-pill warn" title={r.detail ?? undefined}>
          {r.state === 'failed' ? 'Local review failed' : 'Local review cancelled'}
        </span>
        {open}
        <button className="btn small" disabled={blocked} onClick={() => onAct('review-open-anyway')} title="Send the PR prompt without a me_ak_review line">
          Open anyway
        </button>
        <button className="gate" disabled={blocked} onClick={() => onAct('review-retry')} title="Review again, reusing the steps already reported">
          Retry
        </button>
      </span>
    );
  }
  if (r.state === 'cancelled') {
    return (
      <span className="gate-group">
        <span className="gate-pill warn" title={r.detail ?? undefined}>
          Local review cancelled
        </span>
        {open}
      </span>
    );
  }
  if (r.state === 'describing') {
    return (
      <span className="gate-group">
        {/* Past an interrupted description turn or a lost ask (`detail`), the review waits for Review now. */}
        <span className="gate-pill" title={r.detail ?? "AgentK writes the PR's title and description first, for the local review to check the change against"}>
          {r.detail ? 'Local review waits to start' : <Busy label="Writing the PR description" />}
        </span>
        <button className="btn small" disabled={blocked} onClick={() => onAct('review-cancel')} title="Stop the review; then Retry or Open anyway">
          Cancel
        </button>
        {reviewNow(r.detail === DESCRIBE_LOST ? 'Ask for the PR description again' : 'Start the review with the PR description as it is now')}
      </span>
    );
  }
  if (r.state === 'fixing') {
    return (
      <span className="gate-group">
        <span className="gate-pill" title={`AgentK is fixing what round ${r.round} found (${countsText(r.counts)}); round ${r.round + 1} follows`}>
          <Busy label={`Fixing round ${r.round}'s findings`} />
        </span>
        {open}
        {reviewNow(`Start round ${r.round + 1} with the fixes as they are now`)}
      </span>
    );
  }
  if (r.state === 'done') {
    return (
      <span className="gate-group">
        <span className="gate-pill done">{reviewDoneText(r)}</span>
        {open}
      </span>
    );
  }
  if (r.waiting) {
    return (
      <span className="gate-group">
        <span className="gate-pill warn" title="A permission prompt or a question in the review's terminal">
          Local review waits on you
        </span>
        {onTerminal && (
          <button className="gate" onClick={onTerminal} title="Show the review's terminal">
            Open terminal
          </button>
        )}
        {open}
        <button className="btn small" disabled={blocked} onClick={() => onAct('review-cancel')} title="Stop the review; then Retry or Open anyway">
          Cancel
        </button>
      </span>
    );
  }
  return (
    <span className="gate-group">
      <span className="gate-pill" title="The PR prompt goes once the local review is done">
        <Busy label="Reviewing" /> <span className="mono">{reviewSegmentText(r)}</span>
      </span>
      {open}
      {reviewRunning(r) && (
        <button className="btn small" disabled={blocked} onClick={() => onAct('review-cancel')} title="Stop the review; then Retry or Open anyway">
          Cancel
        </button>
      )}
    </span>
  );
}

function StageBar({
  session,
  onAct,
  onReview,
  onOpenLocalReview,
  onOpenReviewTerminal,
  designOpen = false,
  onAutoRun,
  onMove,
  onOpenPr,
  onShare,
  onHeld,
  learnTo = null,
  acting = false,
  updating = false,
  actions,
  reasons = false,
}: {
  session: Session | null;
  onAct: (a: SessionAction, req?: CloseSessionRequest) => Promise<void>;
  /** Design Review: opens the design panel, or brings its approve button into view (`reviewTarget`). */
  onReview: () => void;
  /** Shows the task's local review findings; left out on the review's own screen. */
  onOpenLocalReview?: () => void;
  /** Shows the task's review session's terminal, while it waits on you; left out on the review's own screen. */
  onOpenReviewTerminal?: () => void;
  /** The design panel is open. */
  designOpen?: boolean;
  /** Turns Run to merge on or off (its checkbox beside Design Review, Open PR and Merge & Close). */
  onAutoRun: (on: boolean) => Promise<void>;
  /** The datapack Merge & Close saves what the session learned to first; null when it has none (or Meko is off). */
  learnTo?: string | null;
  onMove: (preset?: MovePreset) => void;
  /** Opens the Open PR with instructions dialog. */
  onOpenPr: () => void;
  /** Opens the Share dialog at the PR's reviewers; absent when the project can't be shared from here. */
  onShare?: () => void;
  /** Opens the reviews held for the user. */
  onHeld: () => void;
  /** A session action is in flight. */
  acting?: boolean;
  /** This session's Update to latest is in flight, or its result hasn't reached the state yet. */
  updating?: boolean;
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
  // A task whose local review holds its Open PR shows in Review & PR, while it stays in Code.
  const step = shownStep(session);
  const idx = session.stage === 'done' ? stages.length : stages.indexOf(step);
  // A task's steps are numbered from 0 (Explore), so Design is 1 and Merge & Close 4.
  const first = kind === 'task' ? 0 : 1;
  // Other stages the session can be moved to open the move dialog: not its own stage (a move there
  // changes nothing), which differs from the step it shows while a local review holds its Open PR.
  const targets: Stage[] = moveBlockedIn(session.stage) ? [] : moveTargets(kind).filter((t) => t !== stepOf(session.stage));
  const midTurn = session.status === 'working' || session.status === 'starting';
  const blocked = session.busy || acting;
  // Why the Design or Code gate is closed, if it is.
  let reason: string | null = null;

  let gate: ReactNode = null;
  switch (session.stage) {
    case 'explore':
      reason = midTurn ? 'Wait for AgentK to finish its turn.' : null;
      // A learn session closes by itself once it's done (it can still be moved from the stage bar).
      gate = session.purpose === 'learn' ? (
        <span className="gate-pill" title="AgentK closes this session once it has saved what it learned">
          Learning the project
        </span>
      ) : (
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
      const runToMerge = <RunToMergeCheck session={session} disabled={blocked} onChange={onAutoRun} />;
      if (session.approvalQueued) {
        gate = (
          <span className="gate-group">
            {runToMerge}
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
      // Design Review only opens the design: the approval goes from the panel, with the answers.
      const review = designGate(session);
      reason = review.reason;
      gate = (
        <span className="gate-group">
          {session.hasDesign && runToMerge}
          <button className="gate" disabled={blocked || !!reason} title={review.title} aria-expanded={designOpen} onClick={onReview}>
            {review.label}
          </button>
        </span>
      );
      break;
    }
    case 'code': {
      if (reviewGatesPr(session) && session.codeReview) {
        gate = reviewGate(session.codeReview, blocked, onAct, onOpenLocalReview, onOpenReviewTerminal, midTurn);
        reason = reviewHolds(session.codeReview) ? session.codeReview.detail : null;
        break;
      }
      if (session.queued === 'open-pr') {
        gate = queuedGate(session, blocked, onAct, onAutoRun, 'The PR prompt goes to AgentK when its turn ends', onOpenPr);
        break;
      }
      const pr = openPrGate(session);
      reason = pr.reason;
      gate = (
        <span className="gate-group">
          <RunToMergeCheck session={session} disabled={blocked} onChange={onAutoRun} />
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
      const held = session.heldReviews;
      gate = (
        <span className="gate-group">
          {held.count > 0 && (
            <button className="btn small held-btn" onClick={onHeld} title={`Held for you: ${held.authors.map((a) => `@${a}`).join(', ')}. Send to AgentK or dismiss.`}>
              {held.count === 1 ? '1 review for you' : `${held.count} reviews for you`}
            </button>
          )}
          {onShare && session.prNumber && (
            <button className="btn small" onClick={onShare} title="Share this project with the PR's reviewers, so they can review it with AgentK">
              Share with reviewers
            </button>
          )}
          {session.queued === 'merge' ? (
            queuedGate(session, blocked, onAct, onAutoRun, 'The pull request is merged once nothing blocks it')
          ) : (
            <>
              <RunToMergeCheck session={session} disabled={blocked} onChange={onAutoRun} />
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
      gate = (
        <span className="gate-pill">
          <Busy label={closingText(session)} />
          {session.closeStep === 'learn' && session.learnProgress && <Elapsed since={session.learnProgress.startedAt} />}
        </span>
      );
      break;
    case 'done':
      gate = <span className="gate-pill done">Done</span>;
      break;
    case 'reviewing-code':
      gate = session.codeReview ? reviewGate(session.codeReview, blocked, onAct) : null;
      reason = session.codeReview && reviewHolds(session.codeReview) ? session.codeReview.detail : null;
      break;
    case 'reviewing-pr': {
      reason = midTurn
        ? 'Wait for AgentK to finish its turn.'
        : session.status === 'needs_input'
          ? 'AgentK is waiting on you in the terminal.'
          : session.status === 'exited'
            ? 'Resume the session first.'
            : null;
      gate = <ReviewPrGate session={session} reason={reason} blocked={blocked} midTurn={midTurn} updating={updating} onAct={onAct} />;
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
                <span className="stg-label">
                  {STAGE_LABELS[st]}
                  {i === idx && <RunToMergeMark session={session} size={10} />}
                </span>
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
            <b>
              {STAGE_LABELS[step]}
              <RunToMergeMark session={session} size={10} />
            </b>
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
      {/* A phone has no tooltips: what the box beside the gate does. */}
      {reasons && runToMergeShown(session) && <div className="blockers">Run to merge: {runToMergeTitle(session)}.</div>}
    </div>
  );
}
