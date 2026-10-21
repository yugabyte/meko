import { describe, expect, it } from 'vitest';
import { shareLinkToken, shareLinkUrl } from '../../src/shared/mekourls.js';
import { httpHost, mekoKeyLinkBase, mekoKeyLinks } from '../../src/web/util.js';

/** No MCP or web app URL from the environment or Claude Code's settings. */
const plain = { fallbackUrl: 'https://mcp.mekodata.ai/mcp', envWebUrl: '' };
const typed = (url: string, webUrl = '') => ({ url, webUrl });

describe('Meko sign-up links', () => {
  it('links the sign-up, sign-in and settings pages, landing on Settings', () => {
    expect(mekoKeyLinks('https://cloud.mekodata.ai')).toEqual({
      signup: 'https://cloud.mekodata.ai/signup?return_to=%2Fsettings',
      signin: 'https://cloud.mekodata.ai/signin?return_to=%2Fsettings',
      settings: 'https://cloud.mekodata.ai/settings',
    });
    expect(mekoKeyLinks(' https://meko.example.com/ ')?.settings).toBe('https://meko.example.com/settings');
  });

  it('has no links without an http(s) web app', () => {
    expect(mekoKeyLinks(null)).toBeNull();
    expect(mekoKeyLinks('')).toBeNull();
    expect(mekoKeyLinks('meko.example.com')).toBeNull();
    expect(mekoKeyLinks('javascript:alert(1)')).toBeNull();
    expect(httpHost('ftp://meko.example.com')).toBeNull();
    expect(httpHost('https://Meko.Example.com:8443/x')).toBe('meko.example.com:8443');
  });

  it('uses a typed web app URL first', () => {
    expect(mekoKeyLinkBase(typed('https://meko.example.com/mcp', 'https://meko.example.com/'), plain)).toBe('https://meko.example.com');
    expect(mekoKeyLinkBase(typed('', 'https://meko.example.com'), plain)).toBe('https://meko.example.com');
    expect(mekoKeyLinkBase(typed('', 'https://meko.example.com'), { ...plain, envWebUrl: 'https://env-web.example' })).toBe('https://meko.example.com');
    // Not http(s): no links.
    expect(mekoKeyLinks(mekoKeyLinkBase(typed('', 'not a url'), plain))).toBeNull();
    expect(mekoKeyLinks(mekoKeyLinkBase(typed('', 'javascript:alert(1)'), plain))).toBeNull();
  });

  it("works out Meko Cloud's web app from its MCP URL, port or not", () => {
    expect(mekoKeyLinkBase(typed(''), plain)).toBe('https://cloud.mekodata.ai');
    expect(mekoKeyLinkBase(typed('https://mcp.mekodata.ai/mcp'), plain)).toBe('https://cloud.mekodata.ai');
    expect(mekoKeyLinkBase(typed('https://MCP.mekodata.ai:8443/mcp'), plain)).toBe('https://cloud.mekodata.ai');
  });

  it("has no links for your own Meko whose web app isn't known", () => {
    expect(mekoKeyLinkBase(typed('https://meko.example.com/mcp'), plain)).toBeNull();
    expect(mekoKeyLinkBase(typed('not a url'), plain)).toBeNull();
  });

  it('falls back to the environment for an empty field, as the server will', () => {
    const env = { fallbackUrl: 'https://meko.example.com/mcp', envWebUrl: 'https://meko.example.com' };
    // A cleared web app URL: the saved one is gone, the environment's applies.
    expect(mekoKeyLinkBase(typed('https://meko.example.com/mcp', ''), env)).toBe('https://meko.example.com');
    // MEKO_WEB_URL wins over Meko Cloud's web app, whatever the MCP URL.
    expect(mekoKeyLinkBase(typed('https://mcp.mekodata.ai/mcp'), { ...plain, envWebUrl: 'https://env-web.example' })).toBe('https://env-web.example');
    // An empty MCP URL is the environment's server, whose web app isn't known without one.
    expect(mekoKeyLinkBase(typed(''), { ...env, envWebUrl: '' })).toBeNull();
    // Meko Cloud's URL is saved as empty, so it's the environment's server too.
    expect(mekoKeyLinkBase(typed('https://mcp.mekodata.ai/mcp/'), { ...env, envWebUrl: '' })).toBeNull();
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
