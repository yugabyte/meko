import { createElement } from 'react';
import { renderToStaticMarkup } from 'react-dom/server';
import { describe, expect, it } from 'vitest';
import { shareLinkToken, shareLinkUrl } from '../../src/shared/mekourls.js';
import { MekoLinkDialog } from '../../src/web/components/MekoLinkDialog.js';
import { MekoHomeLink } from '../../src/web/components/Rail.js';
import { httpHost } from '../../src/web/util.js';

describe("a URL's host", () => {
  it('is the host of an http(s) URL only', () => {
    expect(httpHost('ftp://meko.example.com')).toBeNull();
    expect(httpHost('https://Meko.Example.com:8443/x')).toBe('meko.example.com:8443');
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

describe("Share while AgentK can't share", () => {
  const signup = '<a class="btn primary" id="meko-signup" href="https://cloud.mekodata.ai/signup?return_to=%2Fsettings" target="_blank" rel="noopener noreferrer">';
  it.each([
    ['the Meko on this computer', 'local' as const, 'Share through Meko Cloud', true],
    ['another Meko without its web app URL', 'config' as const, 'Set Meko’s web app URL', false],
    ['no Meko at all', undefined, 'Set up Meko', false],
  ])("on %s, links to Meko's Meko Cloud page in this tab, and to Meko Cloud's sign-up only on the local Meko", (_, sharing, title, offersSignup) => {
    const html = renderToStaticMarkup(createElement(MekoLinkDialog, { sharing, local: 'running', onClose: () => undefined }));
    expect(html).toContain(`<h2>${title}</h2>`);
    expect(html).toMatch(/<a class="btn( primary)?" id="meko-link-open" href="\/settings\/meko-cloud">/);
    expect(html.includes(signup)).toBe(offersSignup);
    expect(html.includes('signup')).toBe(offersSignup);
  });
});

describe("the projects pane's Meko link", () => {
  it("opens Meko's main page, at the root of AgentK's origin, in this tab", () => {
    const html = renderToStaticMarkup(createElement(MekoHomeLink));
    expect(html).toMatch(/^<a [^>]*href="\/"/);
    expect(html).not.toContain('target=');
    expect(html).toContain('aria-label="Open Meko"');
  });
});
