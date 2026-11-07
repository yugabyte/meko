import { describe, expect, it } from 'vitest';
import { headline, runs, statusLines, upFor, type StatusFacts } from '../../src/server/status.js';

const NOW = Date.parse('2026-11-03T12:00:00.000Z');
const facts = (o: Partial<StatusFacts> = {}): StatusFacts => ({
  version: '0.20261103.4', pid: 4242, startedAt: NOW - 243_000, answers: true, claude: true, home: '/home/alice/.meko/extensions/present', now: NOW, ...o,
});

describe('meko present status, laid out as meko status', () => {
  it('prints its checks, the headline, the links as plain rows, then what to run next', () => {
    expect(statusLines(facts(), false)).toEqual([
      '  ✓ Server          healthy',
      '  ✓ Claude Code     installed',
      '',
      '  ● Presenter 0.20261103.4 is running · pid 4242, up 4m3s',
      '',
      '  Pages  meko ui, then Presenter',
      '  Home   /home/alice/.meko/extensions/present',
      '',
      'Next:',
      '  meko ui    open the web UI, then Presenter',
    ]);
    expect(runs(facts())).toBe(true);
  });

  it('says when it is stopped or not answering, and what to run', () => {
    const stopped = statusLines(facts({ pid: null, answers: false, claude: false }), false);
    expect(stopped.slice(0, 3)).toEqual(['  ! Claude Code     not found: install it to build decks', '', "  ✗ Presenter 0.20261103.4 isn't running"]);
    expect(stopped.slice(-2)).toEqual(['Next:', '  meko present start    start Presenter']);
    const silent = statusLines(facts({ answers: false }), false);
    expect(silent[0]).toBe('  ✗ Server          not answering on its socket');
    expect(silent).toContain("  ✗ Presenter 0.20261103.4 isn't answering · pid 4242");
    expect(silent).toContain('  Logs   /home/alice/.meko/extensions/present/logs/server.log');
    expect(runs(facts({ answers: false }))).toBe(false);
    // start returns at once while that pid runs, so the next step is to stop it first.
    expect(silent.slice(-3)).toEqual([
      'Next:',
      "  meko present stop     stop the server that isn't answering",
      '  meko present start    then start it again',
    ]);
  });

  it("colors by Meko's roles: marks, a bold headline with a dim note, dim labels, and the call to action", () => {
    const lines = statusLines(facts(), true);
    expect(lines[0]).toBe('  \x1b[32m✓\x1b[0m Server          healthy');
    expect(lines[3]).toBe('  \x1b[32m●\x1b[0m \x1b[1mPresenter 0.20261103.4 is running\x1b[0m\x1b[2m · pid 4242, up 4m3s\x1b[0m');
    expect(lines[5]).toBe('  \x1b[2mPages\x1b[0m  meko ui, then Presenter');
    expect(lines.at(-1)).toBe(' \x1b[1;97;44m meko ui \x1b[0m   open the web UI, then Presenter');
  });
});

describe('the headline meko status shows in its Presenter row (MEKO_SUMMARY=1)', () => {
  it('is one marked line, never colored, as before', () => {
    expect(headline(facts())).toBe('✓ Presenter 0.20261103.4 is running (pid 4242, up 4m3s)');
    expect(headline(facts({ answers: false }))).toBe("✗ Presenter 0.20261103.4 isn't answering (pid 4242)");
    expect(headline(facts({ pid: null, answers: false }))).toBe("✗ Presenter 0.20261103.4 isn't running");
  });
});

describe('upFor', () => {
  it('writes the uptime as Go writes a duration, as meko status does', () => {
    expect(upFor(33_000)).toBe('33s');
    expect(upFor((2 * 3600 + 4 * 60) * 1000)).toBe('2h4m0s');
  });
});
