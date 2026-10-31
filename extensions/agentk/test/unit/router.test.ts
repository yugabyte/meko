import { describe, expect, it } from 'vitest';
import { basePath } from '../../src/web/base.js';
import { parseRoute, routePath, type Route } from '../../src/web/router.js';

describe('router', () => {
  // `/ext/agentk/` behind Meko, `/` under `npm run dev`.
  describe.each(['/', '/ext/agentk/'])('served under %s', (base) => {
    const at = (rest: string) => parseRoute(base + rest, base);

    it('parses every view', () => {
      expect(at('')).toEqual({ view: 'root' });
      expect(at('usage')).toEqual({ view: 'usage' });
      expect(at('p/a')).toEqual({ view: 'project', projectId: 'a', sessionId: null });
      expect(at('p/a/home')).toEqual({ view: 'home', projectId: 'a' });
      expect(at('p/a/s/b')).toEqual({ view: 'project', projectId: 'a', sessionId: 'b' });
      expect(at('p/a/s/b/design')).toEqual({ view: 'design', projectId: 'a', sessionId: 'b' });
      expect(at('p/a/meko')).toEqual({ view: 'meko', projectId: 'a', sessionId: null });
      expect(at('p/a/meko/b')).toEqual({ view: 'meko', projectId: 'a', sessionId: 'b' });
    });

    it('round-trips every view under the base, ids that need encoding included', () => {
      const routes: Route[] = [
        { view: 'root' }, { view: 'usage' }, { view: 'home', projectId: 'a b' }, { view: 'design', projectId: 'a', sessionId: 's/1' },
        { view: 'project', projectId: 'my repo/x', sessionId: 'a%b' }, { view: 'project', projectId: 'a', sessionId: null },
        { view: 'meko', projectId: 'a b', sessionId: 's/1' }, { view: 'meko', projectId: 'a', sessionId: null },
      ];
      for (const r of routes) {
        const path = routePath(r, base);
        expect(path.startsWith(base)).toBe(true);
        expect(parseRoute(path, base)).toEqual(r);
      }
      expect(routePath({ view: 'root' }, base)).toBe(base);
    });

    it('keeps a segment that is not valid percent-encoding instead of throwing', () => {
      expect(at('p/foo%')).toEqual({ view: 'project', projectId: 'foo%', sessionId: null });
      expect(at('p/a/s/%E0%A4%A')).toEqual({ view: 'project', projectId: 'a', sessionId: '%E0%A4%A' });
    });
  });

  it.each([
    ['https://host:7777/ext/agentk/', '/ext/agentk/'],
    ['http://127.0.0.1:7777/ext/agentk/index.html', '/ext/agentk/'],
    // No <base>: the page's own URL is a route, not where AgentK is served.
    [null, '/'],
  ])('takes the base path from the page’s <base> (%s)', (baseURI, base) => {
    expect(basePath(baseURI)).toBe(base);
  });
});
