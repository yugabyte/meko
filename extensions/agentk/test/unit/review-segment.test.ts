import { readFileSync } from 'node:fs';
import { join } from 'node:path';
import { createElement } from 'react';
import { renderToStaticMarkup } from 'react-dom/server';
import { describe, expect, it } from 'vitest';
import { noCounts, type CodeReviewState, type CodeReviewSummary } from '../../src/shared/codereview.js';
import { ReviewGlass, ReviewSegment } from '../../src/web/components/common.js';

const STYLES = readFileSync(join(import.meta.dirname, '../../src/web/styles.css'), 'utf8');

const review = (state: CodeReviewState, over: Partial<CodeReviewSummary> = {}) =>
  ({
    id: 'r1', sessionId: 's2', strategy: 'sirius-1.0', state, round: 1, rounds: 1, sweep: 1, depth: 1,
    passesDone: 0, passes: 2, scoresDone: 0, candidates: 0, counts: noCounts(),
    mekoCall: false, detail: null, ...over,
  }) as CodeReviewSummary;

/** Each rendered element's tag and classes. */
const elements = (html: string) =>
  [...html.matchAll(/<(\w+)[^>]*\sclass="([^"]*)"/g)].map(([, tag, cls]) => ({ tag: tag!, classes: new Set(cls!.split(' ')) }));

/** Every selector's last compound (what it styles), as its tag and classes, outside comments and keyframes. */
const subjects = STYLES.replace(/\/\*[\s\S]*?\*\//g, '')
  .replace(/@keyframes[^{]*\{(?:[^{}]*\{[^}]*\})*[^}]*\}/g, '')
  .match(/[^{};]+(?=\{)/g)!
  .filter((s) => !s.trim().startsWith('@'))
  .flatMap((s) => s.split(','))
  .map((s) => {
    const last = s.trim().split(/\s*[\s>+~]\s*/).pop()!.replace(/::?[\w-]+(\([^)]*\))?|\[[^\]]*\]/g, '');
    return { selector: s.trim(), tag: last.match(/^[a-z]\w*/)?.[0], classes: [...last.matchAll(/\.([\w-]+)/g)].map((m) => m[1]!) };
  })
  .filter((s) => s.classes.length > 0);

const OWN = ['review-seg', 'review-glass', 'review-seg-text', 'tab-review', 'rail-review', 'lens', 'rim', 'handle', 'glass-m'];

describe('ReviewSegment', () => {
  it('marks a failed or cancelled review stopped, and only then', () => {
    for (const state of ['failed', 'cancelled'] as const) {
      const html = renderToStaticMarkup(createElement(ReviewSegment, { review: review(state), selected: true, onOpen: () => {} }));
      expect(html).toMatch(/^<button type="button" class="review-seg sel stopped"/);
      expect(html).toContain('<span class="review-glass stopped" aria-hidden="true">');
      expect(html).toContain('<span class="review-seg-text">!</span>');
    }
    expect(renderToStaticMarkup(createElement(ReviewGlass, { review: review('done') }))).not.toContain('stopped');
  });

  // A failed review's `held` once matched the held-image tile's `.held` (44 px square, position:
  // relative), which put the magnifier at the top of a tall box and pushed the `!` out of the tab.
  it('takes no rule written for something else, in any state', () => {
    const states: [CodeReviewState, Partial<CodeReviewSummary>?][] = [['failed'], ['cancelled'], ['sweeping', { mekoCall: true }], ['scoring'], ['fixing'], ['done']];
    for (const [state, over] of states) {
      for (const className of ['tab-review', 'rail-review']) {
        for (const selected of [true, false]) {
          const html = renderToStaticMarkup(createElement(ReviewSegment, { review: review(state, over), selected, className, onOpen: () => {} }));
          for (const el of elements(html)) {
            const foreign = subjects.filter(
              (s) => (!s.tag || s.tag === el.tag) && s.classes.every((c) => el.classes.has(c)) && !s.classes.some((c) => OWN.includes(c)),
            );
            expect(foreign.map((s) => s.selector), `${state} ${className} ${[...el.classes].join('.')}`).toEqual([]);
          }
        }
      }
    }
  });
});
