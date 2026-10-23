import { describe, expect, it } from 'vitest';
import { browserCommand, openBrowser } from '../../src/server/browser.js';

describe('agentk ui', () => {
  it.each([
    ['darwin', {}, 'open'],
    ['linux', { DISPLAY: ':0' }, 'xdg-open'],
    ['linux', { WAYLAND_DISPLAY: 'wayland-0' }, 'xdg-open'],
    ['linux', {}, null],
  ] as const)('opens the browser on %s with %j through %s', (platform, env, cmd) => {
    expect(browserCommand(platform, env)).toBe(cmd);
  });

  it.each([
    ['an opener that exits 0', 'true', 2000, true],
    ['one still running when the wait ends', 'sleep', 50, true],
    ['one that fails', 'false', 2000, false],
    ['a missing one', 'agentk-no-such-opener', 2000, false],
    ['no display', null, 2000, false],
  ] as const)('reports %s', async (_what, cmd, waitMs, opened) => {
    // The "URL" is `sleep`'s time; the other openers ignore it.
    expect(await openBrowser('1', cmd, waitMs)).toBe(opened);
  });
});
