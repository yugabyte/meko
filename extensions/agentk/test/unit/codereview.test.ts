import { readFileSync } from 'node:fs';
import { join } from 'node:path';
import { createElement } from 'react';
import { renderToStaticMarkup } from 'react-dom/server';
import { describe, expect, it } from 'vitest';
import {
  findMeAkReviewLine, meAkReviewLine, noCounts, parseMeAkReviewLine, readOverrides, resolveReview, reviewSegmentLabel, reviewSegmentText, reviewSettled, strategyTag, withMeAkReviewLine,
  type CodeReviewState, type CodeReviewSummary, type SeverityCounts,
} from '../../src/shared/codereview.js';
import { ReviewFields, ReviewSegment } from '../../src/web/components/common.js';

const STYLES = readFileSync(join(import.meta.dirname, '../../src/web/styles.css'), 'utf8');

const counts = (blocker: number, major: number, minor: number): SeverityCounts => ({ blocker, major, minor });

describe('the me_ak_review line', () => {
  it.each([
    [counts(0, 0, 0), 'sagittarius-3.0', 'me_ak_review:LGTM;Sagittarius3.0'],
    [counts(4, 4, 0), 'sagittarius-3.0', 'me_ak_review:4 blockers,4 major addressed;Sagittarius3.0'],
    [counts(1, 2, 3), 'sirius-1.0', 'me_ak_review:1 blocker,2 major,3 minor addressed;Sirius1.0'],
    [counts(0, 0, 2), 'golum-9', 'me_ak_review:2 minor addressed;golum-9'],
  ])('writes %j under %s as %s and reads it back', (c, strategy, line) => {
    expect(meAkReviewLine(c, strategy)).toBe(line);
    expect(parseMeAkReviewLine(line)).toEqual({ counts: c, strategy: strategyTag(strategy) });
  });

  it.each([
    ['me_ak_review:LGTM', counts(0, 0, 0)],
    ['me_ak_review:4 blockers,4 major addressed', counts(4, 4, 0)],
  ])('reads %s, from before the line named its strategy', (line, c) => {
    expect(parseMeAkReviewLine(line)).toEqual({ counts: c, strategy: null });
  });

  it.each([
    'meko_design:x', 'me_ak_review:4 blokers addressed', 'me_ak_review:4 blockers', 'me_ak_review:0 major addressed',
    'me_ak_review:1 minor addressed;', 'me_ak_review:LGTM;Sagittarius 3.0', 'me_ak_review:;Sagittarius3.0',
  ])("doesn't read %s as one", (line) => expect(parseMeAkReviewLine(line)).toBeNull());

  it.each([
    ['Body.', 'Body.\n\nme_ak_review:LGTM;Sirius1.0'],
    ['', 'me_ak_review:LGTM;Sirius1.0'],
    ['Body.\nme_ak_review:1 minor addressed\nmeko_conversation_id:c', 'Body.\nme_ak_review:LGTM;Sirius1.0\nmeko_conversation_id:c'],
  ])('sets it in %j', (body, want) => {
    expect(withMeAkReviewLine(body, meAkReviewLine(noCounts(), 'sirius-1.0'))).toBe(want);
    expect(findMeAkReviewLine(want)).toBe('me_ak_review:LGTM;Sirius1.0');
  });
});

describe('review settings', () => {
  const global = { strategy: 'sagittarius-3.0' as const, rounds: 0, depth: 1, check: true };

  it('takes the session over the project over the global values, each on its own', () => {
    expect(resolveReview(global, { strategy: 'sirius-1.0', depth: 3, check: false }, { depth: 2, rounds: null })).toEqual({ strategy: 'sirius-1.0', rounds: 0, depth: 2, check: false });
  });

  it.each([
    [{ strategy: 'off', rounds: 9, depth: 0, check: false }, { strategy: 'off', rounds: 5, depth: 1, check: false }],
    [{ strategy: 'goliath-1.0', rounds: 1.5, depth: '3', check: 'no' }, {}],
    [null, {}],
  ])('reads %j from untrusted input as %j', (input, want) => {
    expect(readOverrides(input)).toEqual(want);
  });

  it('lists the strategies from the lightest review to the most thorough', () => {
    const html = renderToStaticMarkup(createElement(ReviewFields, { id: 'r', value: {}, scoped: true, inherited: global, onChange: () => {} }));
    const strategy = html.slice(html.indexOf('id="r-strategy"'), html.indexOf('</select>'));
    expect([...strategy.matchAll(/<option value="([^"]*)"/g)].map((m) => m[1])).toEqual(['', 'off', 'sirius-1.0', 'sagittarius-3.0']);
  });
});

describe("the review's part of a task's tab", () => {
  const r = (over: Partial<CodeReviewSummary>): CodeReviewSummary => ({
    id: 'r', sessionId: 's', strategy: 'sagittarius-3.0', state: 'sweeping', round: 1, rounds: 1, sweep: 1, depth: 1,
    passesDone: 2, passes: 5, scoresDone: 3, candidates: 9, counts: noCounts(), addressed: false, mekoCall: false, waiting: false, detail: null, onDemand: false, stage: 'code', ...over,
  });

  // Steps: each pass, then scoring as one, so Sagittarius 3.0 has 6 and Sirius 1.0 (no scorers) 1.
  it.each([
    [r({}), '3/6'],
    [r({ passesDone: 5 }), '5/6'],
    [r({ state: 'scoring' }), '6/6'],
    [r({ strategy: 'sirius-1.0', passes: 1, passesDone: 0 }), '1/1'],
    [r({ state: 'fixing' }), 'R1'],
    [r({ state: 'triage', counts: counts(2, 1, 4) }), '2·1'],
    [r({ state: 'describing' }), '…'],
    [r({ state: 'done', counts: counts(2, 1, 4) }), '2·1'],
    [r({ state: 'done', counts: counts(2, 1, 4), addressed: true }), 'LGTM'],
    [r({ state: 'done' }), 'LGTM'],
    [r({ state: 'failed' }), '!'],
    [r({ state: 'cancelled' }), '!'],
  ])('shows %j as %s', (summary, text) => {
    expect(reviewSegmentText(summary)).toBe(text);
  });

  // The tab and the rail leave the magnifier out of a settled review: exactly a done one reading LGTM.
  it.each(['describing', 'preparing', 'sweeping', 'scoring', 'triage', 'fixing', 'done', 'failed', 'cancelled'] as CodeReviewState[])(
    'is settled in %s only when done and LGTM',
    (state) => {
      for (const over of [{}, { counts: counts(2, 1, 4) }, { counts: counts(2, 1, 4), addressed: true }, { counts: counts(0, 0, 1) }]) {
        const summary = r({ state, ...over });
        expect(reviewSettled(summary)).toBe(state === 'done' && reviewSegmentText(summary) === 'LGTM');
      }
    },
  );

  it.each([
    [r({}), 'Code review: Sagittarius 3.0, 2 of 5 passes reported'],
    [r({ state: 'scoring' }), 'Code review: Sagittarius 3.0, step 6 of 6, 3 of 9 scored'],
    [r({ state: 'describing' }), 'Code review: Sagittarius 3.0, waiting for the PR description'],
    [r({ state: 'triage' }), 'Code review: Sagittarius 3.0, its findings wait on you'],
    [r({ state: 'done', counts: counts(1, 0, 0), addressed: true }), 'Code review: Sagittarius 3.0, LGTM, 1 blocker addressed'],
    [r({ waiting: true }), 'Code review: Sagittarius 3.0, 2 of 5 passes reported, waiting on you in its terminal'],
    // One the user ran holds no Open PR: Retry is all it offers.
    [r({ state: 'failed', detail: 'no review pass reported', onDemand: true }), 'Code review: Sagittarius 3.0, failed: no review pass reported. Retry it'],
    [r({ state: 'cancelled', detail: 'you cancelled it', onDemand: true }), 'Code review: Sagittarius 3.0, cancelled. Retry it'],
  ])('spells %j out as %s', (summary, label) => {
    expect(reviewSegmentLabel(summary)).toBe(label);
  });

  const segment = (state: CodeReviewState, className = 'tab-review', selected = false) =>
    renderToStaticMarkup(createElement(ReviewSegment, { review: r({ state, mekoCall: state === 'sweeping', waiting: state === 'scoring' }), selected, className, onOpen: () => {} }));

  it.each<[Partial<CodeReviewSummary>, string]>([
    [{ waiting: true }, 'review-glass running waiting'],
    [{ waiting: true, mekoCall: true }, 'review-glass running waiting'],
    [{ mekoCall: true }, 'review-glass running meko'],
    [{ state: 'triage' }, 'review-glass waiting'],
    [{ state: 'failed', waiting: true }, 'review-glass stopped'],
  ])('draws the magnifier of %j as %s', (over, cls) => {
    const html = renderToStaticMarkup(createElement(ReviewSegment, { review: r(over), selected: false, onOpen: () => {} }));
    expect(html).toContain(`class="${cls}"`);
  });

  it.each<[CodeReviewState, boolean]>([
    ['failed', true],
    ['cancelled', true],
    ['sweeping', false],
    ['done', false],
  ])('marks the part and its magnifier stopped (red) when %s: %s', (state, stopped) => {
    const html = segment(state);
    expect(html.includes('class="review-seg tab-review stopped"')).toBe(stopped);
    expect(html.includes('class="review-glass stopped"')).toBe(stopped);
  });

  // Each selector's last compound (what it styles) as its tag and classes, outside comments and keyframes.
  const subjects = STYLES.replace(/\/\*[\s\S]*?\*\//g, '')
    .replace(/@keyframes[^{]*\{(?:[^{}]*\{[^}]*\})*[^}]*\}/g, '')
    .match(/[^{};]+(?=\{)/g)!
    .filter((sel) => !sel.trim().startsWith('@'))
    .flatMap((sel) => sel.split(','))
    .map((sel) => {
      const last = sel.trim().split(/\s*[\s>+~]\s*/).pop()!.replace(/::?[\w-]+(\([^)]*\))?|\[[^\]]*\]/g, '');
      return { selector: sel.trim(), tag: last.match(/^[a-z]\w*/)?.[0], classes: [...last.matchAll(/\.([\w-]+)/g)].map((m) => m[1]!) };
    })
    .filter((sub) => sub.classes.length > 0);
  const own = ['review-seg', 'review-glass', 'review-seg-text', 'tab-review', 'rail-review', 'lens', 'rim', 'handle', 'glass-m'];
  const states: CodeReviewState[] = ['failed', 'cancelled', 'sweeping', 'scoring', 'triage', 'fixing', 'done'];

  // A stopped review's `held` once matched the held-image tile's `.held` (44 px square, position:
  // relative), which put the magnifier at the top of a tall box and pushed the `!` out of the tab.
  it.each(states.flatMap((state) => ['tab-review', 'rail-review'].flatMap((className) => [true, false].map((selected) => [state, className, selected] as const))))(
    'takes no rule meant for something else when %s, in a %s, selected: %s',
    (state, className, selected) => {
      const elements = [...segment(state, className, selected).matchAll(/<(\w+)[^>]*\sclass="([^"]*)"/g)].map(([, tag, cls]) => ({ tag, classes: new Set(cls!.split(' ')) }));
      for (const el of elements) {
        const foreign = subjects.filter((sub) => (!sub.tag || sub.tag === el.tag) && sub.classes.every((c) => el.classes.has(c)) && !sub.classes.some((c) => own.includes(c)));
        expect(foreign.map((sub) => sub.selector), [...el.classes].join('.')).toEqual([]);
      }
    },
  );
});
