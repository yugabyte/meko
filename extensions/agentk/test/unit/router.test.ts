import { describe, expect, it } from 'vitest';
import { parseRoute, routePath, type Route } from '../../src/web/router.js';

describe('router', () => {
  it('parses every view', () => {
    expect(parseRoute('/')).toEqual({ view: 'root' });
    expect(parseRoute('/usage')).toEqual({ view: 'usage' });
    expect(parseRoute('/p/a')).toEqual({ view: 'project', projectId: 'a', sessionId: null });
    expect(parseRoute('/p/a/home')).toEqual({ view: 'home', projectId: 'a' });
    expect(parseRoute('/p/a/s/b')).toEqual({ view: 'project', projectId: 'a', sessionId: 'b' });
    expect(parseRoute('/p/a/s/b/design')).toEqual({ view: 'design', projectId: 'a', sessionId: 'b' });
    expect(parseRoute('/p/a/meko')).toEqual({ view: 'meko', projectId: 'a', sessionId: null });
    expect(parseRoute('/p/a/meko/b')).toEqual({ view: 'meko', projectId: 'a', sessionId: 'b' });
    expect(parseRoute('/p/a/meko-gaps')).toEqual({ view: 'meko-gaps', projectId: 'a' });
  });

  it('round-trips every view, ids that need encoding included', () => {
    const routes: Route[] = [
      { view: 'root' }, { view: 'usage' }, { view: 'home', projectId: 'a b' }, { view: 'design', projectId: 'a', sessionId: 's/1' },
      { view: 'project', projectId: 'my repo/x', sessionId: 'a%b' }, { view: 'project', projectId: 'a', sessionId: null },
      { view: 'meko', projectId: 'a b', sessionId: 's/1' }, { view: 'meko', projectId: 'a', sessionId: null }, { view: 'meko-gaps', projectId: 'a b' },
    ];
    for (const r of routes) expect(parseRoute(routePath(r))).toEqual(r);
  });

  it('keeps a segment that is not valid percent-encoding instead of throwing', () => {
    expect(parseRoute('/p/foo%')).toEqual({ view: 'project', projectId: 'foo%', sessionId: null });
    expect(parseRoute('/p/a/s/%E0%A4%A')).toEqual({ view: 'project', projectId: 'a', sessionId: '%E0%A4%A' });
  });
});
