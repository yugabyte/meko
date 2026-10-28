import { createElement } from 'react';
import { renderToStaticMarkup } from 'react-dom/server';
import { describe, expect, it } from 'vitest';
import { WhatsNewDialog } from '../../src/web/components/WhatsNewDialog.js';

const render = (onBack?: () => void) =>
  renderToStaticMarkup(createElement(WhatsNewDialog, { from: '0.4.0', to: '0.4.1', onClose: () => {}, onBack }));

describe('WhatsNewDialog', () => {
  it('opened in place of Settings, has a back button before its title', () => {
    expect(render(() => {})).toMatch(
      /<div class="dialog-hd"><button class="icon-btn" aria-label="Back" title="Back">←<\/button><h2>What’s new in v0\.4\.1<\/h2>/,
    );
  });

  it('opened from a banner, has only the close button', () => {
    const html = render();
    expect(html).not.toContain('aria-label="Back"');
    expect(html).toContain('aria-label="Close"');
  });
});
