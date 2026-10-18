import { readFileSync } from 'node:fs';
import { runInNewContext } from 'node:vm';
import { describe, expect, it } from 'vitest';

const source = readFileSync(new URL('../../src/web/public/sw.js', import.meta.url), 'utf8');
const ORIGIN = 'https://ak.example.test';

interface FakeWindow {
  url: string;
  focused: boolean;
  visibilityState: 'visible' | 'hidden';
  focus: () => Promise<unknown>;
  postMessage: (m: unknown) => void;
}

/** Loads sw.js with these open windows (most recently focused first) and clicks a notification. */
async function click(data: Record<string, string>, open: { path: string; focused?: boolean; visible?: boolean; refuseFocus?: boolean }[]) {
  const log: string[] = [];
  const windows: FakeWindow[] = open.map((o) => ({
    url: ORIGIN + o.path,
    focused: !!o.focused,
    visibilityState: o.visible || o.focused ? 'visible' : 'hidden',
    focus: async () => {
      if (o.refuseFocus) throw new Error('InvalidAccessError');
      log.push(`focus ${o.path}`);
    },
    postMessage: (m) => log.push(`message ${o.path} ${JSON.stringify(m)}`),
  }));
  const listeners: Record<string, (ev: unknown) => void> = {};
  const self = {
    addEventListener: (type: string, fn: (ev: unknown) => void) => (listeners[type] = fn),
    clients: {
      matchAll: async () => windows,
      claim: async () => undefined,
      openWindow: async (url: string) => log.push(`open ${url}`),
    },
  };
  runInNewContext(source, { self, URL, decodeURIComponent, encodeURIComponent });
  let done: Promise<unknown> = Promise.resolve();
  listeners.notificationclick({
    notification: { data, close: () => log.push('close') },
    waitUntil: (p: Promise<unknown>) => (done = p),
  });
  await done;
  return log;
}

const open = (projectId: string, sessionId: string) => ({ type: 'ak-open', projectId, sessionId });

describe('service worker notification clicks', () => {
  const data = { projectId: 'p1', sessionId: 's1' };

  it('switches to the tab already showing the session, not the one in use', async () => {
    const log = await click(data, [{ path: '/p/p2/s/s9', focused: true }, { path: '/p/p1/s/s2' }, { path: '/p/p1/s/s1' }]);
    expect(log).toEqual(['close', 'focus /p/p1/s/s1', `message /p/p1/s/s1 ${JSON.stringify(open('p1', 's1'))}`]);
  });

  it('else switches to a tab on the session\'s project, which opens the session', async () => {
    const log = await click(data, [{ path: '/p/p2/s/s9', focused: true }, { path: '/p/p1/home' }, { path: '/p/p1/s/s2' }]);
    expect(log.slice(1)).toEqual(['focus /p/p1/home', `message /p/p1/home ${JSON.stringify(open('p1', 's1'))}`]);
  });

  it('else opens it in the tab in use', async () => {
    const log = await click(data, [{ path: '/p/p3/s/s7', visible: true }, { path: '/p/p2/s/s9', focused: true }, { path: '/usage' }]);
    expect(log.slice(1)).toEqual(['focus /p/p2/s/s9', `message /p/p2/s/s9 ${JSON.stringify(open('p1', 's1'))}`]);
  });

  it('leaves a design doc open on its own page alone', async () => {
    const log = await click(data, [{ path: '/p/p1/s/s1/design', focused: true }, { path: '/p/p2/s/s9' }]);
    expect(log.slice(1)).toEqual(['focus /p/p2/s/s9', `message /p/p2/s/s9 ${JSON.stringify(open('p1', 's1'))}`]);
  });

  it('matches ids that need escaping in the URL', async () => {
    const log = await click({ projectId: 'a b', sessionId: 's/1' }, [{ path: '/', focused: true }, { path: '/p/a%20b/s/s%2F1' }]);
    expect(log[1]).toBe('focus /p/a%20b/s/s%2F1');
  });

  it('opens a new window on the session when none is open', async () => {
    expect(await click(data, [])).toEqual(['close', 'open /p/p1/s/s1']);
  });

  it('opens a share in the tab in use', async () => {
    const log = await click({ datapackId: 'd1' }, [{ path: '/p/p1/s/s1' }, { path: '/p/p2/s/s2', focused: true }]);
    expect(log.slice(1)).toEqual(['focus /p/p2/s/s2', 'message /p/p2/s/s2 {"type":"ak-invite","datapackId":"d1"}']);
    expect(await click({ datapackId: 'd1' }, [])).toEqual(['close', 'open /']);
  });

  it('still opens the session when the browser refuses focus', async () => {
    const log = await click(data, [{ path: '/p/p1/s/s1', refuseFocus: true }]);
    expect(log).toEqual(['close', `message /p/p1/s/s1 ${JSON.stringify(open('p1', 's1'))}`]);
  });
});
