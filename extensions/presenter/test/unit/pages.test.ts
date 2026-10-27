import { describe, expect, it } from 'vitest';
import { byteRange, pageCsp, stampPage } from '../../src/server/server.js';
import { basePath } from '../../src/web/base.js';
import { parseRoute, routePath, type Route } from '../../src/web/router.js';

describe('the page under /ext/presenter/', () => {
  it('names where Meko serves it, and the server\'s version', () => {
    const page = stampPage('<html><head><title>Presenter</title></head></html>', '1.2.3"x');
    expect(page).toContain('<head>\n    <base href="/ext/presenter/" />');
    expect(page).toContain('<meta name="presenter-version" content="1.2.3&#34;x" />');
  });

  it('loads only from this server and can\'t be framed', () => {
    const csp = pageCsp('host:7777');
    expect(csp).toContain("script-src 'self'");
    expect(csp).toContain("frame-ancestors 'none'");
    expect(csp).toContain('connect-src \'self\' wss://host:7777 ws://host:7777');
    expect(pageCsp('evil" host')).toContain("connect-src 'self';");
  });

  it.each<[string | undefined, ReturnType<typeof byteRange>]>([
    [undefined, null],
    ['bytes=0-4', { start: 0, end: 4 }],
    ['bytes=5-', { start: 5, end: 9 }],
    ['bytes=-3', { start: 7, end: 9 }],
    ['bytes=8-20', { start: 8, end: 9 }],
    ['bytes=10-', 'unsatisfiable'],
    ['bytes=-0', 'unsatisfiable'],
    ['bytes=0-1,4-5', null],
  ])('sends a 10-byte asset for Range %s as %j', (header, want) => {
    expect(byteRange(header, 10)).toEqual(want);
  });

  it.each([
    ['https://h:7777/ext/presenter/', '/ext/presenter/'],
    ['https://h:7777/ext/presenter/decks/x/map', '/ext/presenter/decks/x/'],
    [null, '/'],
  ])('takes its base from %s', (uri, want) => {
    expect(basePath(uri)).toBe(want);
  });

  it.each<Route>([
    { view: 'decks' }, { view: 'new' }, { view: 'map', deck: 'dp1' }, { view: 'slides', deck: 'dp1', slide: 's-1' },
    { view: 'slides', deck: 'dp1', slide: null }, { view: 'console', run: 'r1' }, { view: 'audience', run: 'r1' },
  ])('routes $view under the base and back', (r) => {
    expect(parseRoute(routePath(r, '/ext/presenter/'), '/ext/presenter/')).toEqual(r);
  });
});
