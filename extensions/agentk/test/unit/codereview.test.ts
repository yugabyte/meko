import { readFileSync } from 'node:fs';
import { join } from 'node:path';
import { createElement } from 'react';
import { renderToStaticMarkup } from 'react-dom/server';
import { describe, expect, it } from 'vitest';
import {
  findMeAkReviewLine, meAkReviewLine, noCounts, parseMeAkReviewLine, readOverrides, resolveReview, reviewSegmentLabel, reviewSegmentText, withMeAkReviewLine,
  type CodeReviewState, type CodeReviewSummary, type SeverityCounts,
} from '../../src/shared/codereview.js';
import { ReviewFields, ReviewSegment } from '../../src/web/components/common.js';

const STYLES = readFileSync(join(import.meta.dirname, '../../src/web/styles.css'), 'utf8');

const counts = (blocker: number, major: number, minor: number): SeverityCounts => ({ blocker, major, minor });

describe('the me_ak_review line', () => {
  it.each([
    [counts(0, 0, 0), 'me_ak_review:LGTM'],
    [counts(4, 4, 0), 'me_ak_review:4 blockers,4 major addressed'],
    [counts(1, 2, 3), 'me_ak_review:1 blocker,2 major,3 minor addressed'],
    [counts(0, 0, 2), 'me_ak_review:2 minor addressed'],
  ])('writes %j as %s and reads it back', (c, line) => {
    expect(meAkReviewLine(c)).toBe(line);
    expect(parseMeAkReviewLine(line)).toEqual(c);
  });

  it.each(['meko_design:x', 'me_ak_review:4 blokers addressed', 'me_ak_review:4 blockers', 'me_ak_review:0 major addressed'])(
    "doesn't read %s as one",
    (line) => expect(parseMeAkReviewLine(line)).toBeNull(),
  );

  it.each([
    ['Body.', 'Body.\n\nme_ak_review:LGTM'],
    ['', 'me_ak_review:LGTM'],
    ['Body.\nme_ak_review:1 minor addressed\nmeko_conversation_id:c', 'Body.\nme_ak_review:LGTM\nmeko_conversation_id:c'],
  ])('sets it in %j', (body, want) => {
    expect(withMeAkReviewLine(body, meAkReviewLine(noCounts()))).toBe(want);
    expect(findMeAkReviewLine(want)).toBe('me_ak_review:LGTM');
  });
});

describe('review settings', () => {
  const global = { strategy: 'sagittarius-3.0' as const, rounds: 0, depth: 1 };

  it('takes the session over the project over the global values, each on its own', () => {
    expect(resolveReview(global, { strategy: 'sirius-1.0', depth: 3 }, { depth: 2, rounds: null })).toEqual({ strategy: 'sirius-1.0', rounds: 0, depth: 2 });
  });

  it.each([
    [{ strategy: 'off', rounds: 9, depth: 0 }, { strategy: 'off', rounds: 5, depth: 1 }],
    [{ strategy: 'goliath-1.0', rounds: 1.5, depth: '3' }, {}],
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
    passesDone: 2, passes: 5, scoresDone: 3, candidates: 9, counts: noCounts(), addressed: false, mekoCall: false, waiting: false, detail: null, ...over,
  });

  // Steps: each pass, then scoring as one, so Sagittarius 3.0 has 6 and Sirius 1.0 (no scorers) 1.
  it.each([
    [r({}), '3/6'],
    [r({ passesDone: 5 }), '5/6'],
    [r({ state: 'scoring' }), '6/6'],
    [r({ strategy: 'sirius-1.0', passes: 1, passesDone: 0 }), '1/1'],
    [r({ state: 'fixing' }), 'R1'],
    [r({ state: 'done', counts: counts(2, 1, 4) }), '2·1'],
    [r({ state: 'done', counts: counts(2, 1, 4), addressed: true }), 'LGTM'],
    [r({ state: 'done' }), 'LGTM'],
    [r({ state: 'failed' }), '!'],
    [r({ state: 'cancelled' }), '!'],
  ])('shows %j as %s', (summary, text) => {
    expect(reviewSegmentText(summary)).toBe(text);
  });

  it.each([
    [r({}), 'Local review: Sagittarius 3.0, 2 of 5 passes reported'],
    [r({ state: 'scoring' }), 'Local review: Sagittarius 3.0, step 6 of 6, scoring 4 of 9'],
    [r({ state: 'done', counts: counts(1, 0, 0), addressed: true }), 'Local review: Sagittarius 3.0, LGTM, 1 blocker addressed'],
    [r({ waiting: true }), 'Local review: Sagittarius 3.0, 2 of 5 passes reported, waiting on you in its terminal'],
  ])('spells %j out as %s', (summary, label) => {
    expect(reviewSegmentLabel(summary)).toBe(label);
  });

  const segment = (state: CodeReviewState, className = 'tab-review', selected = false) =>
    renderToStaticMarkup(createElement(ReviewSegment, { review: r({ state, mekoCall: state === 'sweeping', waiting: state === 'scoring' }), selected, className, onOpen: () => {} }));

  it.each<[Partial<CodeReviewSummary>, string]>([
    [{ waiting: true }, 'review-glass running waiting'],
    [{ waiting: true, mekoCall: true }, 'review-glass running waiting'],
    [{ mekoCall: true }, 'review-glass running meko'],
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
  const states: CodeReviewState[] = ['failed', 'cancelled', 'sweeping', 'scoring', 'fixing', 'done'];

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
