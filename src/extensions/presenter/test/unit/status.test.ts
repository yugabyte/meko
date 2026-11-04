import { describe, expect, it } from 'vitest';
import { runs, statusLines, upFor, type StatusFacts } from '../../src/server/status.js';

const NOW = Date.parse('2026-11-03T12:00:00.000Z');
const facts = (o: Partial<StatusFacts> = {}): StatusFacts => ({
  version: '0.20261103.4', pid: 4242, startedAt: NOW - 243_000, answers: true, claude: true, home: '/home/alice/.meko/extensions/presenter', now: NOW, ...o,
});

describe('me_presenter status, shaped like meko status', () => {
  it('opens with a marked headline, then its checks, then the links', () => {
    expect(statusLines(facts(), false)).toEqual([
      '✓ Presenter 0.20261103.4 is running (pid 4242, up 4m3s)',
      '  ✓ Server          healthy',
      '  ✓ Claude Code     installed',
      '',
      '  Pages    meko ui /ext/presenter/',
      '  Home     /home/alice/.meko/extensions/presenter',
    ]);
    expect(runs(facts())).toBe(true);
  });

  it('says when it is stopped or not answering, and how to start it', () => {
    const stopped = statusLines(facts({ pid: null, answers: false, claude: false }), false);
    expect(stopped[0]).toBe("✗ Presenter 0.20261103.4 isn't running");
    expect(stopped).toContain('  ! Claude Code     not found: install it to build decks');
    expect(stopped.at(-1)).toBe('  Start it: me_presenter start');
    const silent = statusLines(facts({ answers: false }), false);
    expect(silent.slice(0, 2)).toEqual(["✗ Presenter 0.20261103.4 isn't answering (pid 4242)", '  ✗ Server          not answering on its socket']);
    expect(runs(facts({ answers: false }))).toBe(false);
    // start returns at once while that pid runs, so the hint is to stop it first.
    expect(silent.at(-1)).toBe('  Restart it: me_presenter stop, then me_presenter start (its log: /home/alice/.meko/extensions/presenter/logs/server.log)');
  });

  it("colors the rows' marks, never the headline's, which Meko reads", () => {
    const lines = statusLines(facts(), true);
    expect(lines[0].startsWith('✓ Presenter')).toBe(true);
    expect(lines[1]).toBe('  \x1b[32m✓\x1b[0m Server          healthy');
  });

  it('writes the uptime as Go writes a duration, as meko status does', () => {
    expect(upFor(33_000)).toBe('33s');
    expect(upFor((2 * 3600 + 4 * 60) * 1000)).toBe('2h4m0s');
  });
});
