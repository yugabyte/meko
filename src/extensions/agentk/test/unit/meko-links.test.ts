import { createElement } from 'react';
import { renderToStaticMarkup } from 'react-dom/server';
import { describe, expect, it } from 'vitest';
import { MEKO_INSTALL_COMMAND, shareLinkToken, shareLinkUrl } from '../../src/shared/mekourls.js';
import type { AppState } from '../../src/shared/types.js';
import { MekoInstallBanner } from '../../src/web/components/Banners.js';
import { MekoLinkDialog } from '../../src/web/components/MekoLinkDialog.js';
import { MekoHomeLink } from '../../src/web/components/Rail.js';
import { mekoGapsPath } from '../../src/shared/mekourls.js';

describe("a datapack's Context Gaps link", () => {
  it("is Meko's own tab on this origin, whichever side has the datapack", () => {
    expect(mekoGapsPath('dp/1')).toBe('/datapacks/dp%2F1/gaps');
  });
});

describe('Meko share links', () => {
  const web = 'https://cloud.mekodata.ai';
  it.each([
    ['https://cloud.mekodata.ai/share/abc', web, 'abc'],
    [' https://cloud.mekodata.ai/share/a%2Bb/?x=1#y ', web, 'a+b'],
    ['https://meko.example.com/eu/share/abc', 'https://meko.example.com/eu/', 'abc'],
    ['https://evil.example/share/abc', web, null],
    ['https://cloud.mekodata.ai.evil.example/share/abc', web, null],
    ['http://cloud.mekodata.ai/share/abc', web, null],
    ['https://cloud.mekodata.ai/datapacks/abc', web, null],
    ['https://cloud.mekodata.ai/share/', web, null],
    ['https://cloud.mekodata.ai/share/a/b', web, null],
    ['see https://cloud.mekodata.ai/share/abc', web, null],
    ['https://cloud.mekodata.ai/share/abc', null, null],
  ])('reads the token of %j on %s', (text, webUrl, token) => {
    expect(shareLinkToken(text, webUrl)).toBe(token);
  });

  it('builds the link for a token, which reads back as that token', () => {
    const url = shareLinkUrl('https://cloud.mekodata.ai/', 'a+b/c');
    expect(url).toBe('https://cloud.mekodata.ai/share/a%2Bb%2Fc');
    expect(shareLinkToken(url, web)).toBe('a+b/c');
  });
});

describe('Set up Meko, while there is none', () => {
  it("says what's wrong with the Meko on this computer and opens Meko in this tab, with no word of Meko Cloud", () => {
    const html = renderToStaticMarkup(createElement(MekoLinkDialog, { local: 'stopped', onClose: () => undefined }));
    expect(html).toContain('<h2>Set up Meko</h2>');
    expect(html).toContain('start it with meko start');
    expect(html).toMatch(/<a class="btn primary" id="meko-link-open" href="\/">/);
    expect(html).not.toMatch(/Meko Cloud|signup/);
  });
});

describe("the projects pane's Meko link", () => {
  it("opens Meko's main page, at the root of Kode's origin, in this tab", () => {
    const html = renderToStaticMarkup(createElement(MekoHomeLink));
    expect(html).toMatch(/^<a [^>]*href="\/"/);
    expect(html).not.toContain('target=');
    expect(html).toContain('aria-label="Open Meko"');
  });
});

describe('the banner shown while there is no Meko', () => {
  const banner = (local: string) =>
    renderToStaticMarkup(createElement(MekoInstallBanner, { state: { serverName: 'devbox', meko: { local } } as unknown as AppState }));

  it('gives the install command, with no GitHub sign-in', () => {
    const html = banner('none');
    expect(html).toContain(`<code class="mono">${MEKO_INSTALL_COMMAND}</code>`);
    expect(html).not.toMatch(/\bgh\b|signed in/);
  });

  it('shows nothing once Meko is found', () => {
    expect(banner('running')).toBe('');
  });
});
