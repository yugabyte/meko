// The session actions the desktop ⋯ menu, the phone's ⋯ sheet and the command palette offer, in one
// list: which exist for a session, when, with what label and why one waits. Pure, so it's unit-tested.
import { reviewHolds, reviewRunning, reviewSegmentLabel, reviewWaitsOnYou } from '../shared/codereview';
import { moveBlockedIn } from '../shared/stages';
import type { Session } from '../shared/types';
import type { LabelPart } from './palette';
import { agentName, canRunReview, isReviewChild, mergeGate, onDemandControls, openPrGate, reviewGatesPr } from './util';

/**
 * Where an action shows: the desktop ⋯ menu (`SessionMenu.tsx`), the phone's ⋯ sheet
 * (`SessionSheet.tsx`) or the ⌘K palette (`CommandPalette.tsx`). What the desktop bar keeps in
 * view (the gate, held reviews, Resume, Design doc in Design) isn't on `menu`.
 */
export type Surface = 'menu' | 'sheet' | 'palette';

/** The palette's ids are kept from before the list, so its items keep their names. */
export type SessionActionId =
  | 'answer' | 'resume' | 'review-open' | 'review-findings' | 'review-back' | 'review-send' | 'review-cancel'
  | 'review-retry' | 'review-open-anyway' | 'review-run' | 'review-settings' | 'design' | 'pr-link'
  | 'open-pr' | 'merge-skip-learn' | 'share' | 'learn' | 'move-session' | 'find-terminal'
  | 'rename-session' | 'session-to-top' | 'work-link' | 'work-show' | 'work-unlink' | 'attach-files'
  | 'home' | 'end';

/** Groups follow one another in the list; the menu draws a line between two. */
export type ActionGroup = 'you' | 'review' | 'stage' | 'session' | 'go' | 'end';

export interface SessionActionItem {
  id: SessionActionId;
  group: ActionGroup;
  /** The menu's and the sheet's label. */
  label: string;
  /** The palette's label, naming the session. */
  palette?: LabelPart[];
  /** Why it waits (it's disabled meanwhile), or another short note. The palette shows it too. */
  hint?: string;
  /** Its keyboard shortcut, shown on the menu and the palette (not on a phone). */
  keys?: string;
  /** A count shown in a badge beside the label (the design's open questions). */
  count?: string;
  /** Running now: the label shows as a Busy label with this text. */
  busy?: string;
  /** A link, opened in a new tab (↗). */
  href?: string;
  /** It opens a dialog, a sheet or another view (›). */
  opens?: boolean;
  /** The sheet's primary style. */
  primary?: boolean;
  /** The palette names it a View, not an Action. */
  view?: boolean;
  /** Words the palette's filter also matches. */
  keywords?: string;
  disabled: boolean;
  on: Surface[];
}

export interface SessionActionContext {
  /** A session action is in flight. */
  acting: boolean;
  /** Save learnings to Meko is offered (`offersLearn`). */
  canLearn: boolean;
  /** Merge & Close saves learnings first (the project has a datapack and Meko is there). */
  mergeLearns: boolean;
  /** The project can be shared with the PR's reviewers from here. */
  canShare: boolean;
  /** The session is first in its project's list. */
  atTop: boolean;
  /** The task, on its code-review session's screen. */
  parent?: Pick<Session, 'name'>;
  /** The linked work item's title, when the work queue has it. */
  workItemTitle?: string | null;
  /** On a Mac: the shortcuts' names. */
  mac: boolean;
  /** The project opens no PR (a workshop's demo, `Project.workshop`): no Open PR with instructions. */
  noPr?: boolean;
}

const ALL: Surface[] = ['menu', 'sheet', 'palette'];

/** Every action for the session, in the order each surface lists them. */
export function sessionActions(s: Session, ctx: SessionActionContext): SessionActionItem[] {
  const out: SessionActionItem[] = [];
  const add = (item: Omit<SessionActionItem, 'disabled'> & { disabled?: boolean }) => out.push({ ...item, disabled: !!item.disabled });
  const name = { session: s.name };
  const child = isReviewChild(s);
  const blocked = ctx.acting || s.busy;
  // Why an action waits while another one, or a gate or stage change, is under way.
  const busyWait = blocked ? 'Another action is under way' : undefined;
  const actingWait = ctx.acting ? 'Another action is under way' : undefined;
  const r = s.codeReview;

  if (s.asking) add({ id: 'answer', group: 'you', label: `Answer AgentK: ${s.asking.questions[0]?.question ?? ''}`, opens: true, primary: true, on: ['sheet'] });
  if (s.status === 'exited') add({ id: 'resume', group: 'you', label: `Resume ${agentName(s)}`, primary: true, hint: actingWait, disabled: ctx.acting, on: ['sheet'] });

  if (child) {
    add({ id: 'review-findings', group: 'review', label: 'Findings', opens: true, on: ['sheet'] });
    if (ctx.parent) add({ id: 'review-back', group: 'review', label: `Back to ${ctx.parent.name}`, opens: true, on: ['sheet'] });
    // The review's own gate shows these on a desktop.
    if (r && reviewRunning(r)) add({ id: 'review-cancel', group: 'review', label: 'Cancel the code review', hint: actingWait, disabled: ctx.acting, on: ['sheet'] });
    if (r && reviewHolds(r)) {
      add({ id: 'review-retry', group: 'review', label: 'Retry the code review', primary: true, hint: actingWait, disabled: ctx.acting, on: ['sheet'] });
      add({ id: 'review-open-anyway', group: 'review', label: 'Open the PR without it', hint: actingWait, disabled: ctx.acting, on: ['sheet'] });
    }
  } else if (s.kind !== 'review-pr') {
    const demand = onDemandControls(s);
    // While the review holds Open PR, the Code gate shows its Cancel, Retry and Open anyway.
    const review: Surface[] = reviewGatesPr(s) ? ['sheet', 'palette'] : ALL;
    if (r) add({ id: 'review-open', group: 'review', label: reviewSegmentLabel(r), opens: true, on: ['menu', 'sheet'] });
    if (r && reviewWaitsOnYou(r)) {
      add({
        id: 'review-send', group: 'review', label: 'Send the findings to AgentK', palette: ['Send the code review findings of ', name, ' to AgentK'],
        keywords: 'code review findings send triage notes', primary: true, hint: actingWait, disabled: ctx.acting, on: review,
      });
    }
    if (r && (reviewRunning(r) || demand.cancel)) {
      add({
        id: 'review-cancel', group: 'review', label: 'Cancel the code review', palette: ['Cancel the code review of ', name],
        keywords: 'code review stop cancel', hint: actingWait, disabled: ctx.acting, on: review,
      });
    }
    if (r && (reviewHolds(r) || demand.retry)) {
      add({
        id: 'review-retry', group: 'review', label: 'Retry the code review', palette: ['Retry the code review of ', name],
        keywords: 'code review retry again', primary: reviewHolds(r), hint: actingWait, disabled: ctx.acting, on: review,
      });
    }
    if (r && reviewHolds(r)) {
      add({
        id: 'review-open-anyway', group: 'review', label: 'Open the PR without it', palette: ['Open the PR of ', name, ' without its code review'],
        keywords: 'code review skip open pr anyway', hint: actingWait, disabled: ctx.acting, on: review,
      });
    }
    const run = canRunReview(s);
    if (run.offered) {
      // While AgentK can't take it, the hint says why (and the server's refusal says so again).
      add({
        id: 'review-run', group: 'review', label: 'Run code review', palette: ['Run the code review of ', name],
        hint: run.wait ?? actingWait, keywords: 'code review run again findings', disabled: ctx.acting || !run.ready, on: ALL,
      });
    }
    add({ id: 'review-settings', group: 'review', label: 'Code review settings', opens: true, on: ['menu', 'sheet'] });
  }

  if (!child && (s.hasDesign || s.stage === 'design')) {
    const open = s.stage === 'design' ? s.designQuestions.open : 0;
    // In Design, the desktop bar keeps its Design doc button.
    add({
      id: 'design', group: 'stage', label: 'Design doc', opens: true,
      count: open > 0 ? (open === 1 ? '1 open question' : `${open} open questions`) : undefined,
      on: s.stage === 'design' ? ['sheet'] : ['menu', 'sheet'],
    });
  }
  if (s.prUrl) {
    // A reviewed PR is someone else's, often in another repo than the project's: its repo names it.
    const repo = s.reviewingPr ? `${s.reviewingPr.repo.split('/')[1] ?? s.reviewingPr.repo} ` : '';
    add({ id: 'pr-link', group: 'stage', label: `${repo}PR #${s.prNumber} on GitHub`, href: s.prUrl, on: ['menu', 'sheet'] });
  }
  if (s.stage === 'code' && !ctx.noPr) {
    const queued = s.queued === 'open-pr';
    const reason = queued ? null : openPrGate(s).reason;
    // Review Changes when the press starts AgentK's code review first, as the gate says.
    const gate = s.openPrReview === 'new' ? 'Review Changes' : 'Open PR';
    // While the code review holds Open PR, its gate has no instructions button; the palette keeps it.
    add({
      id: 'open-pr', group: 'stage',
      label: queued ? 'Instructions for the queued Open PR' : `${gate} with instructions`,
      palette: queued ? ['Instructions for the queued Open PR of ', name] : [`${gate} for `, name, ' with instructions'],
      hint: reason ?? busyWait, keywords: 'open pr pull request review changes code review instructions notes draft', opens: true,
      disabled: blocked || !!reason, on: reviewGatesPr(s) ? ['palette'] : ALL,
    });
  }
  if (s.stage === 'review' && ctx.mergeLearns && s.queued !== 'merge') {
    const merge = mergeGate(s);
    add({
      id: 'merge-skip-learn', group: 'stage', label: `${merge.label} without saving learnings`,
      hint: merge.reason ?? busyWait, disabled: blocked || !!merge.reason, on: ['menu', 'sheet'],
    });
  }
  if (s.stage === 'review' && ctx.canShare && s.prNumber) {
    add({ id: 'share', group: 'stage', label: 'Share with reviewers', opens: true, on: ['menu', 'sheet'] });
  }

  if (ctx.canLearn && !child) {
    add({
      id: 'learn', group: 'session', label: 'Save learnings to Meko', palette: ['Save what ', name, ' learned to Meko'],
      busy: s.learning ? 'Saving learnings to Meko' : undefined, hint: s.learning ? undefined : busyWait, keywords: 'save learnings learn meko memory',
      disabled: ctx.acting || s.busy || !!s.learning, on: ALL,
    });
  }
  if (!child && !moveBlockedIn(s.stage)) {
    add({
      id: 'move-session', group: 'session', label: 'Move to another stage', palette: ['Move ', name, ' to another stage'],
      keywords: 'change type stage move exploration explore plan quick code design pr review', opens: true, on: ALL,
    });
  }
  add({
    id: 'find-terminal', group: 'session', label: 'Find in terminal', palette: ['Find in ', name, '’s terminal'],
    keys: ctx.mac ? '⌘F' : 'Ctrl+Shift+F', keywords: 'find search terminal scrollback text output', on: ALL,
  });
  if (!child) {
    add({ id: 'rename-session', group: 'session', label: 'Rename session', palette: ['Rename ', name], keywords: 'rename name title session', opens: true, on: ALL });
    if (!ctx.atTop) {
      add({
        id: 'session-to-top', group: 'session', label: 'Move to top', palette: ['Move ', name, ' to top'],
        keywords: 'move top first pin priority order reorder session', on: ALL,
      });
    }
    // The palette names the item and unlinks it; the menu and the sheet open the dialog that does both.
    add({
      id: 'work-link', group: 'session', label: s.workItemId ? 'Change its work item' : 'Link to a work item',
      palette: ['Link ', name, ' to a work item'], keywords: 'work queue item link attach', opens: true,
      on: s.workItemId ? ['menu', 'sheet'] : ALL,
    });
    if (s.workItemId && ctx.workItemTitle) {
      add({
        id: 'work-show', group: 'session', label: 'Work item', palette: ['Work item of ', name, `: ${ctx.workItemTitle}`],
        keywords: 'work queue item show', view: true, on: ['palette'],
      });
    }
    if (s.workItemId) {
      add({
        id: 'work-unlink', group: 'session', label: 'Unlink its work item', palette: ['Unlink ', name, ' from its work item'],
        keywords: 'work queue item unlink remove', on: ['palette'],
      });
    }
  }
  add({
    id: 'attach-files', group: 'session', label: 'Attach files', palette: ['Attach files to ', name],
    keywords: 'attach file upload image pdf markdown document knowledge base kb meko', on: ['palette'],
  });

  add({ id: 'home', group: 'go', label: 'Project home', opens: true, on: ['menu', 'sheet'] });
  // The sheet ends with its own Archive or discard… block; a discard under way shows in the bar.
  if (!child && !s.discarding) add({ id: 'end', group: 'end', label: 'Archive or discard…', on: ['menu'] });
  return out;
}

/** The actions one surface shows, in order. */
export function actionsOn(items: SessionActionItem[], on: Surface): SessionActionItem[] {
  return items.filter((i) => i.on.includes(on));
}

/** The shortcut that opens the session's ⋯, for the ⋯ button's tooltip: ⌘. on a Mac, Ctrl+. elsewhere. */
export function sessionMenuKeys(mac: boolean): string {
  return mac ? '⌘.' : 'Ctrl+.';
}
