import { describe, expect, it } from 'vitest';
import { parseRoute, routePath } from '../../src/web/router.js';

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
  });

  it("round-trips the Meko calls page, with and without a session", () => {
    for (const r of [{ view: 'meko', projectId: 'a b', sessionId: 's/1' }, { view: 'meko', projectId: 'a', sessionId: null }] as const) {
      expect(parseRoute(routePath(r))).toEqual(r);
    }
  });

  it('round-trips ids that need encoding', () => {
    const r = { view: 'project', projectId: 'my repo/x', sessionId: 'a%b' } as const;
    expect(parseRoute(routePath(r))).toEqual(r);
  });

  it('keeps a segment that is not valid percent-encoding instead of throwing', () => {
    expect(parseRoute('/p/foo%')).toEqual({ view: 'project', projectId: 'foo%', sessionId: null });
    expect(parseRoute('/p/a/s/%E0%A4%A')).toEqual({ view: 'project', projectId: 'a', sessionId: '%E0%A4%A' });
  });
});
