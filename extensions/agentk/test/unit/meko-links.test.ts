import { createElement } from 'react';
import { renderToStaticMarkup } from 'react-dom/server';
import { describe, expect, it } from 'vitest';
import { MEKO_HOME_PATH, shareLinkToken, shareLinkUrl } from '../../src/shared/mekourls.js';
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

describe("the projects pane's Meko link", () => {
  it("opens Meko's main page, at the root of AgentK's origin, in this tab", () => {
    expect(MEKO_HOME_PATH).toBe('/');
    const html = renderToStaticMarkup(createElement(MekoHomeLink));
    expect(html).toMatch(/^<a [^>]*href="\/"/);
    expect(html).not.toContain('target=');
    expect(html).toContain('aria-label="Open Meko"');
  });

  it("draws the m of Meko's logo, hidden from screen readers", () => {
    const html = renderToStaticMarkup(createElement(MekoHomeLink));
    expect(html).toContain('<svg width="20" height="13" viewBox="0 79.93 340.94 217.49" fill="currentColor" aria-hidden="true">');
    expect(html).toContain('<path d="M340.77 171.81');
  });
});
