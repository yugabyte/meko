import { createElement } from 'react';
import { renderToStaticMarkup } from 'react-dom/server';
import { describe, expect, it } from 'vitest';
import { WhatsNewDialog, whatsNewBack, type WhatsNewOpener } from '../../src/web/components/WhatsNewDialog.js';

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
