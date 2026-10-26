import { createElement } from 'react';
import { renderToStaticMarkup } from 'react-dom/server';
import { describe, expect, it } from 'vitest';
import type { Session, Stage } from '../../src/shared/types.js';
import { RunToMergeMark, StageChip } from '../../src/web/components/common.js';
import { runsToMerge, sessionTabTitle, stageWords } from '../../src/web/util.js';

const session = (over: Partial<Session> = {}) =>
  ({
    name: 'Fix the login', harness: 'claude', status: 'idle', statusDetail: null, stage: 'code', autoRun: false,
    usage: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0 }, ...over,
  }) as Session;

describe('Run to merge mark', () => {
  it.each<[Stage, boolean]>([
    ['design', true],
    ['code', true],
    ['pr', true],
    ['review', true],
    ['merge', true],
    // The server leaves autoRun set after the merge: a finished session isn't marked.
    ['done', false],
  ])('marks a session with it on in %s: %s', (stage, want) => {
    expect(runsToMerge(session({ stage, autoRun: true }))).toBe(want);
    expect(runsToMerge(session({ stage }))).toBe(false);
  });

  it('puts the icon and words for screen readers after the stage in the chip, only while it runs', () => {
    expect(renderToStaticMarkup(createElement(StageChip, { session: session() }))).toBe('<span class="stchip">Code</span>');
    const on = renderToStaticMarkup(createElement(StageChip, { session: session({ autoRun: true }) }));
    expect(on).toMatch(/^<span class="stchip">Code<svg class="rtm-icon" width="8" height="8"[^>]*aria-hidden="true">.*<\/svg><span class="sr-only">, Run to merge<\/span><\/span>$/);
    expect(renderToStaticMarkup(createElement(RunToMergeMark, { session: session({ stage: 'done', autoRun: true }) }))).toBe('');
    expect(renderToStaticMarkup(createElement(RunToMergeMark, { session: session({ autoRun: true }), size: 10 }))).toContain('width="10"');
  });

  it('says it in words where the mark can’t go', () => {
    expect(stageWords(session())).toBe('Code');
    expect(stageWords(session({ autoRun: true }))).toBe('Code · Run to merge');
    expect(sessionTabTitle(session({ autoRun: true }))).toContain(' · Code · Run to merge · ');
    expect(sessionTabTitle(session({ stage: 'done', autoRun: true }))).not.toContain('Run to merge');
  });
});
