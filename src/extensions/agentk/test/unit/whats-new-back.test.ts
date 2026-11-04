import { createElement } from 'react';
import { renderToStaticMarkup } from 'react-dom/server';
import { describe, expect, it } from 'vitest';
import { changeCount, WhatsNewDialog, whatsNewBack, type WhatsNewOpener } from '../../src/web/components/WhatsNewDialog.js';

const render = (onBack?: () => void) =>
  renderToStaticMarkup(createElement(WhatsNewDialog, { from: '0.4.0', to: '0.4.1', onClose: () => {}, onBack }));

describe("What's new's Back", () => {
  it.each<[WhatsNewOpener, string | undefined]>([
    ['settings', 'updates'],
    ['banner', undefined],
  ])('opened by %s, goes back to Settings tab %s', (opener, tab) => {
    expect(whatsNewBack(opener)).toBe(tab);
  });

  it.each([
    ['given onBack, shows a Back button before the title', () => {}, true],
    ['without onBack, shows no Back button', undefined, false],
  ])('%s', (_, onBack, shown) => {
    const html = render(onBack);
    expect(/<div class="dialog-hd"><button class="icon-btn" aria-label="Back" title="Back">←<\/button><h2>What’s new in v0\.4\.1<\/h2>/.test(html)).toBe(shown);
    expect(html.includes('aria-label="Back"')).toBe(shown);
    expect(html).toContain('aria-label="Close"');
  });
});

describe("What's new's change count", () => {
  it('counts each commit once, though a BREAKING CHANGE: footer gives it a second line', () => {
    const item = (sha: string, summary: string) => ({ component: null, summary, pr: null, sha, url: `https://github.com/o/r/commit/${sha}` });
    const notes = {
      from: '0.4.0',
      to: '0.4.1',
      sections: [
        { title: 'Breaking changes', items: [item('1111111', 'drop the old layout'), item('2222222', 'the config moves')] },
        { title: 'Fixes', items: [item('2222222', 'stop a crash'), item('3333333', 'stop another crash')] },
      ],
      commits: 4,
      totalCommits: 4,
      compareUrl: 'https://github.com/o/r/compare/v0.4.0...v0.4.1',
    };
    expect(changeCount(notes)).toBe(3);
  });
});
