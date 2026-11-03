import { createElement } from 'react';
import { renderToStaticMarkup } from 'react-dom/server';
import { describe, expect, it } from 'vitest';
import type { CodeReviewSummary } from '../../src/shared/codereview.js';
import type { Session } from '../../src/shared/types.js';
import { actionsOn, sessionActions, type SessionActionContext, type Surface } from '../../src/web/sessionactions.js';
import { SessionMenuItems, nextEnabled } from '../../src/web/components/SessionMenu.js';

const session = (over: Partial<Session> = {}) =>
  ({
    id: 's1', name: 'Fix login', kind: 'task', harness: 'claude', stage: 'code', status: 'idle', busy: false, midTurn: false,
    promptsHeld: 0, queued: null, asking: null, codeReview: null, prNumber: null, prUrl: null, hasDesign: false,
    designQuestions: { open: 0 }, learning: false, discarding: false, workItemId: null, canMerge: false,
    mergeQueueable: false, mergeBlockers: [], ...over,
  }) as unknown as Session;

const review = (over: Partial<CodeReviewSummary> = {}) =>
  ({
    id: 'r1', sessionId: 'c1', strategy: 'sagittarius-3', state: 'sweeping', round: 1, rounds: 1, sweep: 1, depth: 1,
    passesDone: 1, passes: 4, scoresDone: 0, candidates: 0, counts: {}, addressed: false, mekoCall: false, waiting: false,
    detail: null, onDemand: false, stage: 'code', ...over,
  }) as unknown as CodeReviewSummary;

const ctx = (over: Partial<SessionActionContext> = {}): SessionActionContext => ({
  acting: false, canLearn: true, mergeLearns: true, canShare: true, mekoLog: false, atTop: false, mac: true, ...over,
});

const ids = (s: Session, on: Surface, c: Partial<SessionActionContext> = {}) => actionsOn(sessionActions(s, ctx(c)), on).map((a) => a.id);
const item = (s: Session, id: string, c: Partial<SessionActionContext> = {}) => sessionActions(s, ctx(c)).find((a) => a.id === id);

describe('sessionActions', () => {
  it('offers Run code review on the menu, the sheet and the palette, waiting with the reason', () => {
    for (const on of ['menu', 'sheet', 'palette'] as Surface[]) expect(ids(session(), on)).toContain('review-run');
    expect(item(session(), 'review-run')).toMatchObject({ disabled: false, hint: undefined });
    expect(item(session({ status: 'working' }), 'review-run')).toMatchObject({ disabled: true, hint: "After AgentK's turn" });
    expect(item(session({ stage: 'design' }), 'review-run')).toBeUndefined();
  });

  it('keeps what the desktop bar shows off the menu: Resume, the gate, held reviews', () => {
    const exited = session({ status: 'exited' });
    expect(ids(exited, 'sheet')[0]).toBe('resume');
    expect(ids(exited, 'menu')).not.toContain('resume');
    // Open PR with instructions is the menu's; the gate keeps only Open PR.
    expect(ids(session(), 'menu')).toContain('open-pr');
  });

  it("leaves a code review's Cancel, Retry and Open anyway to the gate while it holds Open PR", () => {
    const failed = session({ codeReview: review({ state: 'failed', detail: 'It broke' }) });
    for (const id of ['review-retry', 'review-open-anyway']) {
      expect(ids(failed, 'menu')).not.toContain(id);
      expect(ids(failed, 'sheet')).toContain(id);
      expect(ids(failed, 'palette')).toContain(id);
    }
    // Its gate has no instructions button meanwhile.
    expect(ids(failed, 'menu')).not.toContain('open-pr');
    expect(ids(failed, 'palette')).toContain('open-pr');
    expect(ids(session({ codeReview: review() }), 'menu')).not.toContain('review-cancel');
  });

  it('puts a review you ran on the menu, since the gate never shows it', () => {
    const running = session({ codeReview: review({ onDemand: true }) });
    expect(ids(running, 'menu')).toContain('review-cancel');
    const stopped = session({ codeReview: review({ onDemand: true, state: 'failed' }) });
    expect(ids(stopped, 'menu')).toContain('review-retry');
    expect(item(stopped, 'review-retry')?.primary).toBe(false);
  });

  it('keeps Design doc in the bar while the design has open questions', () => {
    const asking = session({ stage: 'design', hasDesign: true, designQuestions: { open: 2 } as Session['designQuestions'] });
    expect(ids(asking, 'menu')).not.toContain('design');
    expect(item(asking, 'design')).toMatchObject({ count: '2 open questions', on: ['sheet'] });
    const answered = session({ stage: 'design', hasDesign: true });
    expect(ids(answered, 'menu')).toContain('design');
    expect(ids(session({ stage: 'code', hasDesign: true, designQuestions: { open: 2 } as Session['designQuestions'] }), 'menu')).toContain('design');
  });

  it('offers Merge & Close without saving learnings in place of the box, with the gate\'s reason', () => {
    const ready = session({ stage: 'review', prNumber: 7, prUrl: 'https://github.com/acme/demo-repo/pull/7', canMerge: true });
    expect(item(ready, 'merge-skip-learn')).toMatchObject({ label: 'Merge & Close without saving learnings', disabled: false, on: ['menu', 'sheet'] });
    expect(item(session({ stage: 'review', mergeBlockers: ['CI is failing.'] }), 'merge-skip-learn')).toMatchObject({ disabled: true, hint: 'CI is failing.' });
    expect(item(ready, 'merge-skip-learn', { mergeLearns: false })).toBeUndefined();
    expect(item({ ...ready, queued: 'merge' } as Session, 'merge-skip-learn')).toBeUndefined();
    expect(ids(ready, 'menu')).toEqual(expect.arrayContaining(['share', 'pr-link']));
    expect(ids(ready, 'menu', { canShare: false })).not.toContain('share');
  });

  it("gives a code review's own screen only its rows, and no Archive", () => {
    const child = session({ kind: 'code-review', stage: 'reviewing-code', codeReview: review() });
    expect(ids(child, 'sheet', { parent: { name: 'Fix login' } })).toEqual(['review-findings', 'review-back', 'review-cancel', 'find-terminal', 'home']);
    expect(ids(child, 'menu')).toEqual(['find-terminal', 'home']);
  });

  it('keeps the palette\'s ids and names the session in its labels', () => {
    expect(ids(session(), 'palette')).toEqual(['review-run', 'open-pr', 'learn', 'move-session', 'find-terminal', 'rename-session', 'session-to-top', 'work-link', 'attach-files']);
    expect(item(session(), 'rename-session')?.palette).toEqual(['Rename ', { session: 'Fix login' }]);
    expect(item(session(), 'find-terminal', { mac: false })?.keys).toBe('Ctrl+Shift+F');
    const linked = session({ workItemId: 'w1' });
    expect(ids(linked, 'palette', { workItemTitle: 'Login bug' })).toEqual(expect.arrayContaining(['work-show', 'work-unlink']));
    expect(ids(linked, 'palette')).not.toContain('work-link');
    expect(item(linked, 'work-link')?.label).toBe('Change its work item');
  });

  it('drops Move to top at the top, and Archive while a discard runs', () => {
    expect(ids(session(), 'menu', { atTop: true })).not.toContain('session-to-top');
    expect(ids(session(), 'menu').at(-1)).toBe('end');
    expect(ids(session({ discarding: true }), 'menu')).not.toContain('end');
  });

  it('waits Save learnings while another action or a learn step runs', () => {
    expect(item(session(), 'learn', { acting: true })?.disabled).toBe(true);
    expect(item(session({ learning: true }), 'learn')).toMatchObject({ disabled: true, busy: 'Saving learnings to Meko' });
    expect(item(session(), 'learn', { canLearn: false })).toBeUndefined();
  });
});

describe('SessionMenuItems', () => {
  it('renders menu items in groups, with the reason a disabled one waits', () => {
    const items = actionsOn(sessionActions(session({ status: 'working', prUrl: 'https://github.com/acme/demo-repo/pull/7', prNumber: 7 }), ctx()), 'menu');
    const html = renderToStaticMarkup(createElement(SessionMenuItems, { items, cur: 1, onPick: () => {} }));
    expect(html).toContain('<button role="menuitem" tabindex="-1" class="session-menu-item" type="button" aria-disabled="true"><span class="session-menu-label">Run code review</span><span class="session-menu-hint">After AgentK&#x27;s turn</span>');
    expect(html).toContain('<button role="menuitem" tabindex="0" class="session-menu-item cur" type="button">');
    expect(html).toContain('href="https://github.com/acme/demo-repo/pull/7" target="_blank"');
    expect(html).toContain('<div role="separator" class="session-menu-sep"></div>');
    expect(html).toContain('class="session-menu-item danger"');
  });

  it('moves past disabled items, wrapping', () => {
    const items = [{ disabled: false }, { disabled: true }, { disabled: false }];
    expect(nextEnabled(items, 0, 1)).toBe(2);
    expect(nextEnabled(items, 2, 1)).toBe(0);
    expect(nextEnabled(items, 0, -1)).toBe(2);
    expect(nextEnabled(items, -1, 1)).toBe(0);
    expect(nextEnabled(items, items.length, -1)).toBe(2);
  });
});
